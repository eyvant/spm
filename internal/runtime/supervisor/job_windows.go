//go:build windows

package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Each service process lives in a job object that terminates it when the last
// handle to the job closes. The job is named so that a supervisor upgrade can
// hand it over: the CLI opens the job by name, the old supervisor detaches,
// and the new supervisor opens the same job and adopts the process. Holding a
// handle across the handoff keeps the process alive; if everything holding
// one exits, the process is terminated as before.

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procOpenJobObject  = kernel32.NewProc("OpenJobObjectW")
	procIsProcessInJob = kernel32.NewProc("IsProcessInJob")
)

const (
	jobAllAccess  = 0x1F001F // JOB_OBJECT_ALL_ACCESS
	jobQuery      = 0x0004   // JOB_OBJECT_QUERY
	processAccess = windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_TERMINATE | windows.PROCESS_SET_QUOTA
	// System services run as SYSTEM or a service account; an elevated CLI
	// must still be able to open their job for a handoff.
	systemJobSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)"
)

// JobName is the job object name of a service below this data root.
func (l Layout) JobName(name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(l.Root))))
	namespace := `Local\`
	if l.Global {
		namespace = `Global\`
	}
	return namespace + "spm-job-" + hex.EncodeToString(sum[:8]) + "-" + name
}

// HoldJob opens the job of a running service only to keep it alive: any open
// handle keeps a kill-on-close job, and its processes, from being closed. It
// asks for query access alone because a system service's job is created at
// System integrity, and an elevated (High integrity) CLI is denied any
// write-type access to it. It fails for services started by supervisor
// version 1, whose jobs had no name.
func HoldJob(l Layout, name string) (windows.Handle, error) {
	return openJob(l, name, jobQuery)
}

// openJob opens the job of a running service with full access, as its
// supervisor needs.
func openJob(l Layout, name string, access uintptr) (windows.Handle, error) {
	jobName, err := windows.UTF16PtrFromString(l.JobName(name))
	if err != nil {
		return 0, err
	}
	handle, _, callErr := procOpenJobObject.Call(access, 0, uintptr(unsafe.Pointer(jobName)))
	if handle == 0 {
		return 0, fmt.Errorf("open job %s: %w", l.JobName(name), callErr)
	}
	return windows.Handle(handle), nil
}

func createJob(l Layout, name string) (windows.Handle, error) {
	jobName, err := windows.UTF16PtrFromString(l.JobName(name))
	if err != nil {
		return 0, err
	}
	var attributes *windows.SecurityAttributes
	if l.Global {
		descriptor, err := windows.SecurityDescriptorFromString(systemJobSDDL)
		if err != nil {
			return 0, err
		}
		attributes = &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	}
	job, err := windows.CreateJobObject(attributes, jobName)
	if err != nil {
		return 0, err
	}
	if err := setKillOnClose(job); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func setKillOnClose(job windows.Handle) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}

// ProcessInJob reports whether process runs inside job.
func ProcessInJob(process, job windows.Handle) bool {
	var result int32
	ok, _, _ := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&result)))
	return ok != 0 && result != 0
}

// child is a supervised process, started here or adopted from a previous
// supervisor, with the read end of its output pipe.
type child struct {
	pid     int
	process windows.Handle
	job     windows.Handle
	done    chan uint32

	output windows.Handle
	sink   io.Writer
	halt   chan struct{}
	drain  chan struct{}
	pumped chan bool
}

func newChild(pid int, process, job windows.Handle) *child {
	c := &child{pid: pid, process: process, job: job, done: make(chan uint32, 1)}
	go func() {
		var code uint32 = 1
		if event, err := windows.WaitForSingleObject(process, windows.INFINITE); err == nil && event == windows.WAIT_OBJECT_0 {
			_ = windows.GetExitCodeProcess(process, &code)
		}
		c.done <- code
	}()
	return c
}

func startChild(l Layout, name string, config Config) (*child, error, error) {
	read, write, err := newOutputPipe()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(config.Command[0], config.Command[1:]...)
	cmd.Dir = config.Cwd
	cmd.Env = Environment(config)
	cmd.Stdout, cmd.Stderr = write, write
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	startErr := cmd.Start()
	// The child has its own copy now; closing this one lets the pipe break
	// once the child and everything it started have exited.
	_ = write.Close()
	if startErr != nil {
		_ = windows.CloseHandle(read)
		return nil, nil, startErr
	}
	pid := cmd.Process.Pid
	process, err := windows.OpenProcess(processAccess, false, uint32(pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = windows.CloseHandle(read)
		return nil, nil, fmt.Errorf("open started process: %w", err)
	}
	// The handle opened above is used from here on.
	_ = cmd.Process.Release()
	job, jobErr := createJob(l, name)
	if jobErr == nil {
		if err := windows.AssignProcessToJobObject(job, process); err != nil {
			_ = windows.CloseHandle(job)
			job, jobErr = 0, err
		}
	}
	started := newChild(pid, process, job)
	started.output = read
	return started, jobErr, nil
}

// errNothingToAdopt means there is no detached process to take over: no
// process was recorded, no job holds one, or it has already exited. A fresh
// start is then the right thing to do.
var errNothingToAdopt = errors.New("nothing to take over")

// adopt takes over the process a detached supervisor left running. It
// accepts the process only while it runs inside this service's job, so a
// recycled PID is never adopted. An error other than errNothingToAdopt means
// a live process is waiting in the job but cannot be taken over.
func adopt(l Layout, name string, pid int) (*child, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("%w: no process recorded", errNothingToAdopt)
	}
	job, err := openJob(l, name, jobAllAccess)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			// Nobody holds the job, so its processes are gone.
			return nil, fmt.Errorf("%w: %v", errNothingToAdopt, err)
		}
		return nil, err
	}
	process, err := windows.OpenProcess(processAccess, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil, fmt.Errorf("%w: process %d has exited", errNothingToAdopt, pid)
		}
		return nil, fmt.Errorf("open process %d: %w", pid, err)
	}
	fail := func(reason error) (*child, error) {
		_ = windows.CloseHandle(process)
		_ = windows.CloseHandle(job)
		return nil, reason
	}
	if event, _ := windows.WaitForSingleObject(process, 0); event == windows.WAIT_OBJECT_0 {
		return fail(fmt.Errorf("%w: process %d has exited", errNothingToAdopt, pid))
	}
	if !ProcessInJob(process, job) {
		return fail(fmt.Errorf("process %d is not in job %s", pid, l.JobName(name)))
	}
	if err := setKillOnClose(job); err != nil {
		return fail(fmt.Errorf("configure job: %w", err))
	}
	return newChild(pid, process, job), nil
}

// kill terminates the process, and with it everything it started.
func (c *child) kill() {
	if c.job != 0 && windows.TerminateJobObject(c.job, 1) == nil {
		return
	}
	_ = windows.TerminateProcess(c.process, 1)
}

// startPump starts copying the output pipe into dst, or into the previous
// destination when dst is nil.
func (c *child) startPump(dst io.Writer) {
	if dst != nil {
		c.sink = dst
	}
	if c.output == 0 || c.sink == nil || c.pumped != nil {
		return
	}
	halt, drain, pumped := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	c.halt, c.drain, c.pumped = halt, drain, pumped
	output, sink := c.output, c.sink
	go func() { pumped <- pumpOutput(output, sink, halt, drain) }()
}

// stopPump stops copying and returns once no read is in flight.
func (c *child) stopPump() {
	if c.pumped == nil {
		return
	}
	close(c.halt)
	<-c.pumped
	c.pumped = nil
}

// finishOutput copies what the exited process left in the pipe and closes
// it. Output that processes it started keep writing is dropped after a
// short grace period.
func (c *child) finishOutput() {
	if c.pumped != nil {
		close(c.drain)
		select {
		case <-c.pumped:
		case <-time.After(2 * time.Second):
			close(c.halt)
			<-c.pumped
		}
		c.pumped = nil
	}
	if c.output != 0 {
		_ = windows.CloseHandle(c.output)
		c.output = 0
	}
}

// handOutput stops copying output and leaves the pipe with an output holder
// for the next supervisor, recording where it is in state. A process without
// an output pipe has nothing to pass.
func (c *child) handOutput(l Layout, name string, state *State) error {
	if c.output == 0 {
		return nil
	}
	c.stopPump()
	holder, handle, err := spawnOutputHolder(l, name, c.output, c.pid)
	if err != nil {
		return err
	}
	_ = windows.CloseHandle(c.output)
	c.output = 0
	state.OutputHandle, state.OutputHolder = handle, holder
	return nil
}

// close releases the handles of a process that has exited.
func (c *child) close() {
	c.finishOutput()
	if c.job != 0 {
		_ = windows.CloseHandle(c.job)
		c.job = 0
	}
	_ = windows.CloseHandle(c.process)
}

// release gives up this supervisor's job handle without touching the
// process. The waiting goroutine keeps the process handle until exit.
func (c *child) release() {
	if c.job != 0 {
		_ = windows.CloseHandle(c.job)
		c.job = 0
	}
}

var errNotRunning = errors.New("service is not running")

// Detach asks the running supervisor of a service to exit and leave its
// process running, for a supervisor upgrade. The supervisor leaves the
// service's output pipe with an output holder (see spawnOutputHolder). The
// caller must hold the job (HoldJob) until the next supervisor has adopted
// the process; otherwise the process is terminated when the old supervisor
// exits.
func Detach(l Layout, name string) error {
	state := l.ReadState(name)
	if state.SupervisorPID <= 0 || state.PID <= 0 {
		return errNotRunning
	}
	return l.WriteControl(name, "detach")
}
