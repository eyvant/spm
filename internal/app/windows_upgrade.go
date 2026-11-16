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
			outcome, err = b.upgradeSystemService(name)
		} else {
			outcome, err = b.upgradeUserService(name)
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
			if err := writeWindowsScheduleLauncher(launcher, executable, b.scheduleRecordPath(name), b.scheduleStateDir(name), b.logPath(name), activation); err != nil {
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

const (
	outcomeHandedOver = "handed over to the new supervisor (kept running)"
	outcomeRestarted  = "restarted"
)

func (b *windowsBackend) upgradeUserService(name string) (string, error) {
	state := b.readState(name)
	if state.SupervisorPID <= 0 || !windowsProcessAlive(state.SupervisorPID) {
		return "", nil
	}
	if outcome, err := b.handOver(name, state, func() error { return b.waitProcessExit(state.SupervisorPID) }, func() error { return b.startManaged(name) }); err == nil {
		return outcome, nil
	} else if !errors.Is(err, errHandoverUnavailable) {
		return "", err
	}
	if err := b.sendControl(name, "stop"); err != nil {
		return "", fmt.Errorf("stop: %w", err)
	}
	if err := b.startManaged(name); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}
	return outcomeRestarted, nil
}

func (b *windowsBackend) upgradeSystemService(name string) (string, error) {
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
	// The service's spm-supervisor.exe copy cannot be replaced while it runs,
	// so the old supervisor detaches, which stops the SCM service while its
	// process keeps running; the copy is replaced and the service started
	// again, and the new supervisor adopts the process.
	outcome, err := b.handOver(name, b.readState(name), func() error { return b.waitSCMStopped(name) }, func() error {
		if err := b.installSCMService(name); err != nil {
			return err
		}
		return b.startSCMService(name)
	})
	if err == nil {
		return outcome, nil
	}
	if !errors.Is(err, errHandoverUnavailable) {
		return "", err
	}
	if err := b.stopSCMService(name); err != nil {
		return "", err
	}
	if err := b.installSCMService(name); err != nil {
		return "", err
	}
	if err := b.startSCMService(name); err != nil {
		return "", err
	}
	return outcomeRestarted, nil
}

// errHandoverUnavailable means nothing has changed yet and the service can
// still be restarted the old way.
var errHandoverUnavailable = errors.New("supervisor handoff unavailable")

// handOver replaces a service's supervisor without stopping the service:
//
//  1. Open the service's job by name. Version 1 supervisors used an unnamed
//     job, so this fails for them and the caller restarts the service.
//  2. Ask the old supervisor to detach and wait for it to go (waitOld).
//     It exits without terminating the process; the job handle held here
//     keeps the kill-on-close job, and so the process, alive.
//  3. Start the new supervisor (startNew), which adopts the process.
//  4. Close the job handle; the new supervisor now holds the job.
//
// If this process dies midway, its handle closes and the service process is
// terminated, the same result as a restart; spm restarts the service when its
// supervisor next starts.
func (b *windowsBackend) handOver(name string, state windowsServiceState, waitOld, startNew func() error) (string, error) {
	if state.PID <= 0 {
		return "", errHandoverUnavailable
	}
	job, err := supervisor.OpenJob(b.layout(), name)
	if err != nil {
		return "", errHandoverUnavailable
	}
	defer windows.CloseHandle(job)
	if err := supervisor.Detach(b.layout(), name); err != nil {
		return "", errHandoverUnavailable
	}
	if err := waitOld(); err != nil {
		// The old supervisor did not act on the request; withdraw it so it
		// does not detach later, after this handle is gone.
		_ = os.Remove(b.controlPath(name))
		return "", fmt.Errorf("previous supervisor did not detach: %w", err)
	}
	if err := startNew(); err != nil {
		return "", fmt.Errorf("start new supervisor: %w", err)
	}
	if b.readState(name).PID == state.PID {
		return outcomeHandedOver, nil
	}
	// The process exited during the handoff and the new supervisor applied
	// the restart policy.
	return outcomeRestarted, nil
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
