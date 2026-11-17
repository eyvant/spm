//go:build darwin

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eyvant/spm/internal/runtime/common"
	"golang.org/x/sys/unix"
)

// Main is the spm-supervisor entry point on macOS:
//
//	spm-supervisor version
//	spm-supervisor launchd --root ROOT NAME [--adopt PID:FD:STARTED]
//
// launchd runs one supervisor per service. --adopt is passed only by a
// supervisor that re-executes itself onto a new binary (see RunLaunchd).
func Main(args []string, stdout, _ io.Writer) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, common.VersionLine("spm-supervisor"))
		return nil
	}
	usage := errors.New("usage: spm-supervisor version | launchd --root ROOT NAME")
	if len(args) != 4 && len(args) != 6 || args[0] != "launchd" || args[1] != "--root" || !filepath.IsAbs(args[2]) {
		return usage
	}
	adopt := ""
	if len(args) == 6 {
		if args[4] != "--adopt" {
			return usage
		}
		adopt = args[5]
	}
	return RunLaunchd(Layout{Root: filepath.Clean(args[2])}, args[3], adopt)
}

// Signals the supervisor passes on to the service. launchd stops a job by
// sending SIGTERM to its main process, which is the supervisor.
var forwardedSignals = []os.Signal{unix.SIGTERM, unix.SIGINT, unix.SIGHUP, unix.SIGQUIT, unix.SIGUSR1, unix.SIGUSR2}

// RunLaunchd runs one service under launchd. It starts the service command
// in the working directory and environment launchd gave it, copies the
// service's output into service.log with rotation, records starts and exits
// in spm.log, forwards stop signals, and finally exits the way the service
// did, so launchd's KeepAlive policy sees the service's own exit status.
//
// Upgrading the supervisor does not restart the service: on a "reexec"
// request the supervisor execs the new binary in place. The process ID, which
// launchd tracks, stays the same, the service stays its child, and the output
// pipe is inherited by the new image.
func RunLaunchd(l Layout, name, adoptSpec string) error {
	events := common.OpenEventLog(l.EventLogPath(name), os.Stderr)
	defer events.Close()
	// launchd discards the supervisor's own stdout and stderr; a crash
	// report still lands next to the service's events.
	if crash, err := os.OpenFile(l.EventLogPath(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		_ = debug.SetCrashOutput(crash, debug.CrashOptions{})
		_ = crash.Close()
	}
	config, err := l.ReadConfig(name)
	if err != nil {
		events.Printf("supervisor %d cannot read the service configuration: %v", os.Getpid(), err)
		return err
	}
	output, err := common.OpenRotatingLog(l.LogPath(name), common.DefaultLogMaxBytes, common.DefaultLogBackups)
	if err != nil {
		events.Printf("supervisor %d cannot open %s: %v", os.Getpid(), l.LogPath(name), err)
		return err
	}
	defer output.Close()

	signals := make(chan os.Signal, 16)
	signal.Notify(signals, append([]os.Signal{unix.SIGCHLD}, forwardedSignals...)...)
	build := common.BuildID()
	state := State{Version: StateVersion, Name: name, Status: "running", SupervisorPID: os.Getpid(), Build: build}
	var pid, read int
	if adoptSpec != "" {
		var started time.Time
		pid, read, started, err = parseAdopt(adoptSpec)
		if err != nil {
			events.Printf("supervisor %d cannot take over: %v", os.Getpid(), err)
			return err
		}
		state.StartedAt = started
		events.Printf("supervisor %d (build %s) took over running process %d", os.Getpid(), build, pid)
	} else {
		var write *os.File
		read, write, err = newOutputPipe()
		if err != nil {
			events.Printf("start failed: %v", err)
			return err
		}
		cmd := exec.Command(config.Command[0], config.Command[1:]...)
		cmd.Stdout, cmd.Stderr = write, write
		events.Printf("starting %s", strings.Join(config.Command, " "))
		startErr := cmd.Start()
		// The child has its own copy now; closing this one lets the pipe
		// end once the child and everything it started have exited.
		_ = write.Close()
		if startErr != nil {
			_ = unix.Close(read)
			events.Printf("start failed: %v", startErr)
			state.Status, state.Error, state.SupervisorPID, state.StoppedAt = "failed", startErr.Error(), 0, time.Now()
			_ = l.WriteState(state)
			return startErr
		}
		pid, state.StartedAt = cmd.Process.Pid, time.Now()
		// The supervisor reaps the child itself (see reap), also after a
		// re-exec, where no os.Process exists.
		_ = cmd.Process.Release()
	}
	state.PID = pid
	_ = l.WriteState(state)

	pump := newOutputPump(read, output)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	stopping := false
	for {
		if code, sig, exited := reap(pid); exited {
			return finishLaunchd(l, events, output, pump, state, code, sig, stopping)
		}
		select {
		case received := <-signals:
			sig := received.(syscall.Signal)
			if sig == unix.SIGCHLD {
				continue
			}
			if sig == unix.SIGTERM || sig == unix.SIGINT || sig == unix.SIGHUP || sig == unix.SIGQUIT {
				stopping = true
			}
			events.Printf("passing %s on to process %d", unix.SignalName(sig), pid)
			_ = unix.Kill(pid, sig)
		case <-ticker.C:
			control, ok := l.TakeControl(name)
			if ok && control.Action == "reexec" {
				// On success this does not return.
				reexecLaunchd(l, name, events, pump, read, pid, state.StartedAt, control.Executable)
			}
		}
	}
}

// reap reports whether the service has exited, with its exit code or the
// signal that ended it.
func reap(pid int) (int, syscall.Signal, bool) {
	var status unix.WaitStatus
	for {
		got, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			// ECHILD: the process is gone and its status with it.
			return 1, 0, true
		}
		if got != pid {
			return 0, 0, false
		}
		if status.Signaled() {
			return 128 + int(status.Signal()), status.Signal(), true
		}
		return status.ExitStatus(), 0, true
	}
}

func finishLaunchd(l Layout, events *common.EventLog, output *common.RotatingLog, pump *outputPump, state State, code int, sig syscall.Signal, stopping bool) error {
	pump.finish()
	pid := state.PID
	state.PID, state.SupervisorPID, state.StoppedAt, state.ExitCode = 0, 0, time.Now(), &code
	state.Status = "stopped"
	if sig != 0 {
		events.Printf("process %d was ended by %s", pid, unix.SignalName(sig))
		if !stopping {
			state.Status, state.Error = "failed", "process was ended by "+unix.SignalName(sig)
		}
	} else {
		events.Printf("process %d exited with code %d", pid, code)
		if code != 0 {
			state.Status, state.Error = "failed", fmt.Sprintf("process exited with code %d", code)
		}
	}
	_ = l.WriteState(state)
	_ = output.Close()
	_ = events.Close()
	switch sig {
	case unix.SIGTERM, unix.SIGINT, unix.SIGHUP, unix.SIGKILL:
		// End the same way, so launchd sees what happened to the service.
		signal.Reset(sig)
		_ = unix.Kill(os.Getpid(), sig)
		time.Sleep(time.Second)
	}
	if code == 0 {
		return nil
	}
	return ExitStatus{Code: code}
}

// reexecLaunchd replaces this supervisor with executable (by default the
// binary it was started from, which an upgrade has replaced). It returns only
// if that fails, after resuming the output copy.
func reexecLaunchd(l Layout, name string, events *common.EventLog, pump *outputPump, read, pid int, started time.Time, executable string) {
	if executable == "" {
		var err error
		if executable, err = os.Executable(); err != nil {
			events.Printf("cannot switch supervisor: %v", err)
			return
		}
	}
	pump.stop()
	if _, err := unix.FcntlInt(uintptr(read), unix.F_SETFD, 0); err != nil {
		events.Printf("cannot switch supervisor: keep the output pipe open: %v", err)
		pump.start()
		return
	}
	events.Printf("supervisor %d switching to %s", os.Getpid(), executable)
	args := []string{executable, "launchd", "--root", l.Root, name, "--adopt", fmt.Sprintf("%d:%d:%d", pid, read, started.UnixNano())}
	err := unix.Exec(executable, args, os.Environ())
	unix.CloseOnExec(read)
	events.Printf("cannot switch supervisor to %s: %v", executable, err)
	pump.start()
}

func parseAdopt(spec string) (int, int, time.Time, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return 0, 0, time.Time{}, fmt.Errorf("invalid --adopt value %q", spec)
	}
	pid, pidErr := strconv.Atoi(parts[0])
	fd, fdErr := strconv.Atoi(parts[1])
	started, startedErr := strconv.ParseInt(parts[2], 10, 64)
	if pidErr != nil || fdErr != nil || startedErr != nil || pid <= 0 || fd < 0 {
		return 0, 0, time.Time{}, fmt.Errorf("invalid --adopt value %q", spec)
	}
	unix.CloseOnExec(fd)
	return pid, fd, time.Unix(0, started), nil
}

// outputPump copies the output pipe into the service log in the background.
type outputPump struct {
	read   int
	sink   io.Writer
	halt   chan struct{}
	drain  chan struct{}
	pumped chan bool
}

func newOutputPump(read int, sink io.Writer) *outputPump {
	pump := &outputPump{read: read, sink: sink}
	pump.start()
	return pump
}

func (p *outputPump) start() {
	if p.pumped != nil {
		return
	}
	halt, drain, pumped := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	p.halt, p.drain, p.pumped = halt, drain, pumped
	go func() { pumped <- pumpOutput(p.read, p.sink, halt, drain) }()
}

// stop returns once no read is in flight.
func (p *outputPump) stop() {
	if p.pumped == nil {
		return
	}
	close(p.halt)
	<-p.pumped
	p.pumped = nil
}

// finish copies what the exited service left in the pipe. Output that
// processes it started keep writing is dropped after a short grace period.
func (p *outputPump) finish() {
	if p.pumped != nil {
		close(p.drain)
		select {
		case <-p.pumped:
		case <-time.After(2 * time.Second):
			close(p.halt)
			<-p.pumped
		}
		p.pumped = nil
	}
	_ = unix.Close(p.read)
}
