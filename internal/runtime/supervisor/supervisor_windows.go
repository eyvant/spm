//go:build windows

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/eyvant/spm/internal/runtime/common"
)

// Supervise runs the service command, restarts it according to its policy,
// and answers stop/restart/detach requests until the service stops.
//
// The service writes its output into a pipe that the supervisor copies to
// service.log, rotating it; what the supervisor itself does goes to spm.log.
//
// When the previous supervisor detached (see Detach), Supervise adopts the
// service process that is still running, and its output pipe, instead of
// starting a new one, so a supervisor upgrade does not interrupt the service.
func Supervise(l Layout, name string) error {
	if err := l.EnsureDirectories(); err != nil {
		return err
	}
	events := openEvents(l, name)
	defer events.Close()
	config, err := l.ReadConfig(name)
	if err != nil {
		events.Printf("supervisor %d cannot read the service configuration: %v", os.Getpid(), err)
		return fmt.Errorf("read supervisor config: %w", err)
	}
	previous := l.ReadState(name)
	if previous.SupervisorPID > 0 && previous.SupervisorPID != os.Getpid() {
		return fmt.Errorf("service %s is already supervised by process %d", name, previous.SupervisorPID)
	}
	output, err := common.OpenRotatingLog(l.LogPath(name), common.DefaultLogMaxBytes, common.DefaultLogBackups)
	if err != nil {
		events.Printf("supervisor %d cannot open %s: %v", os.Getpid(), l.LogPath(name), err)
		return err
	}
	defer output.Close()
	build := common.BuildID()
	state := State{Version: StateVersion, Name: name, Status: "starting", SupervisorPID: os.Getpid(), Build: build}
	adopted, adoptErr := adopt(l, name, previous.PID)
	if adopted != nil {
		state.Restarts, state.StartedAt = previous.Restarts, previous.StartedAt
		if previous.OutputHolder > 0 {
			read, takeErr := takeOutput(previous.OutputHolder, previous.OutputHandle)
			if takeErr != nil {
				events.Printf("warning: supervisor %d took over process %d without its output: %v", os.Getpid(), adopted.pid, takeErr)
			} else {
				adopted.output = read
				releaseOutputHolder(l, name, previous.OutputHolder)
			}
		}
		events.Printf("supervisor %d (build %s) took over running process %d", os.Getpid(), build, adopted.pid)
	} else if !errors.Is(adoptErr, errNothingToAdopt) {
		// A live process is waiting to be taken over. Starting a second
		// one, or terminating it, would turn a failed handoff into a restart;
		// leave it to whoever is holding its job, which restores the
		// previous supervisor.
		events.Printf("supervisor %d could not take over running process %d: %v", os.Getpid(), previous.PID, adoptErr)
		return fmt.Errorf("cannot take over running process %d: %w", previous.PID, adoptErr)
	}
	_ = l.WriteState(state)
	delay, err := time.ParseDuration(config.RestartSec)
	if err != nil || delay < 0 {
		delay = 3 * time.Second
	}
	for {
		current := adopted
		adopted = nil
		if current == nil {
			events.Printf("starting %s", strings.Join(config.Command, " "))
			started := time.Now()
			var jobErr error
			current, jobErr, err = startChild(l, name, config)
			if err != nil {
				state.Status, state.Error, state.PID, state.SupervisorPID = "failed", err.Error(), 0, 0
				state.StoppedAt = time.Now()
				events.Printf("start failed: %v", err)
				_ = l.WriteState(state)
				return nil
			}
			if jobErr != nil {
				events.Printf("warning: cannot create process job: %v", jobErr)
			}
			state.StartedAt = started
		}
		current.startPump(output)
		state.Status, state.Error, state.PID, state.StoppedAt, state.ExitCode = "running", "", current.pid, time.Time{}, nil
		state.OutputHandle, state.OutputHolder = 0, 0
		_ = l.WriteState(state)
		action, exitCode := monitor(l, name, current, events, &state)
		if action == "detach" {
			// Whoever asked for the detach holds the job, and now the
			// output pipe, so the service keeps running until the next
			// supervisor adopts it.
			current.release()
			state.SupervisorPID = 0
			events.Printf("supervisor %d detached; process %d keeps running", os.Getpid(), current.pid)
			_ = l.WriteState(state)
			return nil
		}
		current.close()
		state.PID, state.ExitCode, state.StoppedAt = 0, &exitCode, time.Now()
		events.Printf("process exited with code %d", exitCode)
		if action == "stop" {
			state.Status, state.SupervisorPID = "stopped", 0
			_ = l.WriteState(state)
			return nil
		}
		var waitErr error
		if exitCode != 0 {
			waitErr = fmt.Errorf("process exited with code %d", exitCode)
		}
		if action == "restart" || ShouldRestart(config.Restart, waitErr, exitCode) {
			state.Status = "restarting"
			state.Restarts++
			_ = l.WriteState(state)
			if action != "restart" && delay > 0 {
				events.Printf("restarting in %s", delay)
				time.Sleep(delay)
			}
			continue
		}
		state.SupervisorPID = 0
		if exitCode == 0 {
			state.Status = "stopped"
		} else {
			state.Status = "failed"
			state.Error = fmt.Sprintf("process exited with code %d", exitCode)
		}
		_ = l.WriteState(state)
		return nil
	}
}

// monitor waits for the process to exit or for a control request. stop and
// restart terminate the process first; detach leaves it running, and its
// output pipe with an output holder, recorded in state.
func monitor(l Layout, name string, current *child, events *common.EventLog, state *State) (string, int) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case code := <-current.done:
			return "", int(code)
		case <-ticker.C:
			control, ok := l.TakeControl(name)
			if !ok {
				continue
			}
			switch control.Action {
			case "stop", "restart":
				events.Printf("%s requested", control.Action)
				current.kill()
				return control.Action, int(<-current.done)
			case "detach":
				if err := current.handOutput(l, name, state); err != nil {
					// Keep supervising: the requester sees that this
					// supervisor did not exit and reports the handoff as
					// failed.
					events.Printf("cannot detach: %v", err)
					current.startPump(nil)
					continue
				}
				return "detach", 0
			}
		}
	}
}

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// CreateKillJob puts pid into a job that terminates it when the returned
// handle closes, including when the supervisor itself exits.
func CreateKillJob(pid int) (syscall.Handle, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createJob := kernel32.NewProc("CreateJobObjectW")
	setJob := kernel32.NewProc("SetInformationJobObject")
	openProcess := kernel32.NewProc("OpenProcess")
	assignProcess := kernel32.NewProc("AssignProcessToJobObject")
	handle, _, createErr := createJob.Call(0, 0)
	if handle == 0 {
		return 0, createErr
	}
	job := syscall.Handle(handle)
	info := jobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	result, _, setErr := setJob.Call(handle, 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if result == 0 {
		CloseHandle(job)
		return 0, setErr
	}
	process, _, openErr := openProcess.Call(0x0100|0x0001, 0, uintptr(pid)) // SET_QUOTA | TERMINATE
	if process == 0 {
		CloseHandle(job)
		return 0, openErr
	}
	defer CloseHandle(syscall.Handle(process))
	result, _, assignErr := assignProcess.Call(handle, process)
	if result == 0 {
		CloseHandle(job)
		return 0, assignErr
	}
	return job, nil
}

func CloseHandle(handle syscall.Handle) {
	if handle == 0 {
		return
	}
	closeHandle := syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle")
	_, _, _ = closeHandle.Call(uintptr(handle))
}

func KillTree(pid int) {
	if pid <= 0 {
		return
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")
	terminateProcess := kernel32.NewProc("TerminateProcess")
	process, _, _ := openProcess.Call(0x0001, 0, uintptr(pid)) // PROCESS_TERMINATE
	if process == 0 {
		return
	}
	defer CloseHandle(syscall.Handle(process))
	_, _, _ = terminateProcess.Call(process, 1)
}
