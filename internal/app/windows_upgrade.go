//go:build windows

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/eyvant/spm/internal/runtime/scheduler"
	"github.com/eyvant/spm/internal/runtime/supervisor"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// upgradeRuntime moves this scope's background processes onto the installed
// runtimes.
//
// scheduler: every job launcher is rewritten to run spm-scheduler (older
// versions ran `spm.exe __scheduled-run`) and the scheduler daemon is
// replaced. A job that is executing right now is left to finish; its next
// run uses the new binary. Services are not touched.
//
// supervisor: every running service is handed over to the new supervisor
// without stopping it (see handOver). Services whose supervisor cannot hand
// over (version 1 had no named job) are restarted instead. System services
// also get a fresh spm-supervisor.exe copy.
func (b *windowsBackend) upgradeRuntime(components upgradeComponents) error {
	var errs []error
	names, err := b.names()
	if err != nil {
		return err
	}
	if components.scheduler {
		errs = append(errs, b.upgradeScheduler(names)...)
	}
	if !components.supervisor {
		return errors.Join(errs...)
	}
	for _, name := range names {
		config, err := b.readConfig(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if config.Mode == "schedule" {
			continue
		}
		var outcome string
		if b.system {
			outcome, err = b.upgradeSystemService(name, components.allowRestart)
		} else {
			outcome, err = b.upgradeUserService(name, components.allowRestart)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else if outcome != "" {
			fmt.Fprintf(b.stdout, "%s %s\n", name, outcome)
		}
	}
	return errors.Join(errs...)
}

func (b *windowsBackend) upgradeScheduler(names []string) []error {
	var errs []error
	executable, err := runtimeExecutable(schedulerRuntime)
	if err != nil {
		return []error{err}
	}
	for _, name := range names {
		config, err := b.readConfig(name)
		if err != nil || config.Mode != "schedule" {
			continue
		}
		// Only the launcher scripts change; the Task Scheduler tasks keep
		// running them, so a run in progress is not interrupted.
		for _, activation := range []string{"daemon", "boot"} {
			launcher := b.scheduleLauncherPath(name, activation)
			if _, err := os.Stat(launcher); err != nil {
				continue
			}
			if err := writeWindowsScheduleLauncher(launcher, executable, b.scheduleRecordPath(name), b.scheduleStateDir(name), b.logPath(name), b.eventLogPath(name), activation); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}
	}
	tasks, err := scheduler.DiscoverTasks(b.root)
	if err != nil {
		return append(errs, fmt.Errorf("find scheduled jobs: %w", err))
	}
	if len(tasks) > 0 {
		if err := b.refreshScheduleDaemon(); err != nil {
			errs = append(errs, fmt.Errorf("restart spm scheduler: %w", err))
		} else {
			fmt.Fprintln(b.stdout, "spm scheduler restarted")
		}
	}
	return errs
}

const outcomeHandedOver = "handed over to the new supervisor (kept running)"

func (b *windowsBackend) upgradeUserService(name string, allowRestart bool) (string, error) {
	state := b.readState(name)
	if state.SupervisorPID <= 0 || !windowsProcessAlive(state.SupervisorPID) {
		return "", nil
	}
	return b.handOver(name, state, allowRestart)
}

func (b *windowsBackend) upgradeSystemService(name string, allowRestart bool) (string, error) {
	if !b.scmInstalled(name) {
		return "", nil
	}
	running, err := b.scmRunning(name)
	if err != nil {
		return "", err
	}
	if !running {
		// installSCMService rewrites the service's spm-supervisor.exe copy
		// and keeps its SCM registration.
		if err := b.installSCMService(name); err != nil {
			return "", err
		}
		return "updated (not running)", nil
	}
	return b.handOver(name, b.readState(name), allowRestart)
}

// handOver moves a running service onto the installed spm-supervisor without
// stopping it, and never falls back to restarting it:
//
//  1. Check that the handoff can work: the service's job opens by name (it
//     does not for supervisor version 1, whose job had no name) and the
//     process runs inside it. If not, nothing is changed and the error says
//     how to move the service by hand.
//  2. Hold the job and ask the old supervisor to detach. It exits without
//     terminating the process; the handle held here keeps the kill-on-close
//     job, and so the process, alive. A system service's SCM service stops
//     at this point while its process keeps running.
//  3. Start the new supervisor, which adopts the process. For a system
//     service, its spm-supervisor.exe copy is replaced first and the previous
//     copy kept aside.
//  4. If that fails, start the previous supervisor again, which takes the
//     process back; the service is never restarted.
//
// If this process dies between steps 2 and 4, its handle closes and the
// service process is terminated.
func (b *windowsBackend) handOver(name string, state windowsServiceState, allowRestart bool) (string, error) {
	layout := b.layout()
	job, err := supervisor.HoldJob(layout, name)
	if err != nil {
		reason := err.Error()
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			reason = "the running supervisor is from spm 0.3.6 or earlier and cannot hand over"
		}
		return b.unavailable(name, reason, allowRestart)
	}
	defer windows.CloseHandle(job)
	process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(state.PID))
	if err != nil {
		return b.unavailable(name, fmt.Sprintf("open service process %d: %v", state.PID, err), allowRestart)
	}
	defer windows.CloseHandle(process)
	if !supervisor.ProcessInJob(process, job) {
		return b.unavailable(name, fmt.Sprintf("service process %d is not in the service's job", state.PID), allowRestart)
	}

	var waitOld, startNew, rollback func() error
	if b.system {
		current := filepath.Join(b.serviceDir(name), "spm-supervisor.exe")
		previous := current + ".previous"
		waitOld = func() error { return b.waitSCMStopped(name) }
		startNew = func() error {
			_ = os.Remove(previous)
			if err := os.Rename(current, previous); err != nil {
				return fmt.Errorf("keep the previous supervisor: %w", err)
			}
			if err := b.installSCMService(name); err != nil {
				return err
			}
			return b.startSCMService(name)
		}
		rollback = func() error {
			if _, err := os.Stat(previous); err != nil {
				return fmt.Errorf("previous supervisor copy is missing: %w", err)
			}
			_ = os.Remove(current)
			if err := os.Rename(previous, current); err != nil {
				return err
			}
			return b.startSCMService(name)
		}
		defer os.Remove(previous)
	} else {
		previousImage, err := windowsProcessImage(state.SupervisorPID)
		if err != nil {
			return b.unavailable(name, fmt.Sprintf("locate the running supervisor: %v", err), allowRestart)
		}
		waitOld = func() error { return b.waitProcessExit(state.SupervisorPID) }
		startNew = func() error { return b.startManaged(name) }
		rollback = func() error { return b.spawnSupervisor(previousImage, name) }
	}

	if err := supervisor.Detach(layout, name); err != nil {
		return b.unavailable(name, err.Error(), allowRestart)
	}
	if err := waitOld(); err != nil {
		// The old supervisor did not act on the request and still runs the
		// service; withdraw the request so it does not detach later.
		_ = os.Remove(b.controlPath(name))
		return b.unavailable(name, fmt.Sprintf("the running supervisor did not detach: %v", err), allowRestart)
	}
	startErr := startNew()
	after := b.readState(name)
	if startErr == nil && after.PID == state.PID {
		return outcomeHandedOver, nil
	}
	if startErr == nil {
		// The process exited during the handoff, and the new supervisor
		// applied the service's restart policy.
		return "handed over to the new supervisor; its process exited during the handoff and the restart policy applied", nil
	}
	if event, _ := windows.WaitForSingleObject(process, 0); event == windows.WAIT_OBJECT_0 {
		return "", fmt.Errorf("the new supervisor failed to start (%v) and the service process exited meanwhile; start it with '%s'", startErr, b.spmCommand("run", name))
	}
	if err := rollback(); err != nil {
		return "", fmt.Errorf("the new supervisor failed to start (%v), and the previous supervisor could not take the service back (%v); the service stops when this command exits. Start it again with '%s'", startErr, err, b.spmCommand("run", name))
	}
	return "", fmt.Errorf("the new supervisor failed to start (%v); the previous supervisor took the service back and it keeps running. Check '%s' and the service log, then %s", startErr, b.spmCommand("doctor", ""), b.moveByHand(name))
}

// unavailable handles a handoff that was not attempted, so nothing has
// changed yet: by default it reports the problem and leaves the service on
// its current supervisor; with allowRestart (--allow-restart, installer
// -RestartServices) it restarts the service onto the new supervisor.
func (b *windowsBackend) unavailable(name, reason string, allowRestart bool) (string, error) {
	if !allowRestart {
		return "", fmt.Errorf("could not hand over to the new supervisor: %s. It keeps running on its current supervisor; %s", reason, b.moveByHand(name))
	}
	if b.system {
		if err := b.stopSCMService(name); err != nil {
			return "", fmt.Errorf("could not hand over to the new supervisor (%s), and stopping it for a restart failed: %w", reason, err)
		}
		// startSystemManaged starts a stopped service on a fresh
		// spm-supervisor.exe copy.
		if err := b.startSystemManaged(name); err != nil {
			return "", fmt.Errorf("could not hand over to the new supervisor (%s); it was stopped for a restart but did not start again: %w. Start it with '%s'", reason, err, b.spmCommand("run", name))
		}
	} else {
		if err := b.sendControl(name, "stop"); err != nil {
			return "", fmt.Errorf("could not hand over to the new supervisor (%s), and stopping it for a restart failed: %w", reason, err)
		}
		if err := b.startManaged(name); err != nil {
			return "", fmt.Errorf("could not hand over to the new supervisor (%s); it was stopped for a restart but did not start again: %w. Start it with '%s'", reason, err, b.spmCommand("run", name))
		}
	}
	return fmt.Sprintf("restarted on the new supervisor (could not hand over: %s)", reason), nil
}

// moveByHand says how to move a service onto the installed supervisor at a
// time that suits a short interruption.
func (b *windowsBackend) moveByHand(name string) string {
	where := ""
	if b.system {
		where = " in an Administrator terminal"
	}
	return fmt.Sprintf("to move it to the new supervisor, which restarts it once, run '%s' and then '%s'%s, or upgrade with -RestartServices to restart every service that cannot be handed over", b.spmCommand("stop", name), b.spmCommand("run", name), where)
}

func (b *windowsBackend) spmCommand(command, name string) string {
	line := "spm"
	if b.system {
		line += " --system"
	}
	line += " " + command
	if name != "" {
		line += " " + name
	}
	return line
}

// windowsProcessImage returns the executable path of a running process.
func windowsProcessImage(pid int) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(process)
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buffer[:size]), nil
}

func (b *windowsBackend) waitProcessExit(pid int) error {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(process)
	if event, err := windows.WaitForSingleObject(process, 15_000); err != nil || event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process %d is still running", pid)
	}
	return nil
}

func (b *windowsBackend) waitSCMStopped(name string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		running, err := b.scmRunning(name)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("windows service %s did not stop", b.scmName(name))
}

func (b *windowsBackend) scmRunning(name string) (bool, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(b.scmName(name))
	if err != nil {
		return false, fmt.Errorf("open Windows service %s: %w", b.scmName(name), err)
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return false, fmt.Errorf("query Windows service: %w", err)
	}
	return status.State != svc.Stopped, nil
}

func (b *windowsBackend) scheduleDaemonPIDPath() string {
	return filepath.Join(b.root, "scheduler-state", "run.lock", "pid")
}

// stopScheduleDaemonProcess terminates the scheduler daemon that holds this
// root's lock. Ending the scheduler task only ends its wscript.exe launcher;
// the spm.exe daemon below it keeps running and keeps the lock. The PID is
// trusted only when that process is spm and was created before the lock was
// written, so a recycled PID never terminates an unrelated process.
func (b *windowsBackend) stopScheduleDaemonProcess() error {
	path := b.scheduleDaemonPIDPath()
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return nil
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if windowsProcessAlive(pid) {
			return fmt.Errorf("open scheduler process %d: %w", pid, err)
		}
		return nil
	}
	defer windows.CloseHandle(process)
	if !windowsScheduleDaemonProcess(process, info.ModTime()) {
		return nil
	}
	if err := windows.TerminateProcess(process, 1); err != nil {
		return fmt.Errorf("stop scheduler process %d: %w", pid, err)
	}
	if event, err := windows.WaitForSingleObject(process, 10_000); err != nil || event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("scheduler process %d did not exit", pid)
	}
	return nil
}

func windowsScheduleDaemonProcess(process windows.Handle, lockWritten time.Time) bool {
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(process, 0, &buffer[0], &size) != nil {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(filepath.Base(windows.UTF16ToString(buffer[:size]))), "spm") {
		return false
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(process, &created, &exited, &kernel, &user) != nil {
		return false
	}
	return !time.Unix(0, created.Nanoseconds()).After(lockWritten.Add(2 * time.Second))
}
