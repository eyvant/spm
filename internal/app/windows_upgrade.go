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
// supervisor: user services whose supervisor is running are stopped and
// started again (the supervisor holds its service in a kill-on-close job, so
// swapping it always restarts the service); system services get a fresh
// spm-supervisor.exe copy, and the ones that were running are started again.
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
		var restarted bool
		if b.system {
			restarted, err = b.upgradeSystemService(name)
		} else {
			restarted, err = b.upgradeUserService(name)
		}
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		case restarted:
			fmt.Fprintf(b.stdout, "%s restarted\n", name)
		case b.system:
			fmt.Fprintf(b.stdout, "%s updated (not running)\n", name)
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

func (b *windowsBackend) upgradeUserService(name string) (bool, error) {
	state := b.readState(name)
	if state.SupervisorPID <= 0 || !windowsProcessAlive(state.SupervisorPID) {
		return false, nil
	}
	if err := b.sendControl(name, "stop"); err != nil {
		return false, fmt.Errorf("stop: %w", err)
	}
	if err := b.startManaged(name); err != nil {
		return false, fmt.Errorf("start: %w", err)
	}
	return true, nil
}

func (b *windowsBackend) upgradeSystemService(name string) (bool, error) {
	if !b.scmInstalled(name) {
		return false, nil
	}
	running, err := b.scmRunning(name)
	if err != nil {
		return false, err
	}
	if running {
		// spm-supervisor.exe cannot be replaced while the service runs it.
		if err := b.stopSCMService(name); err != nil {
			return false, err
		}
	}
	// installSCMService rewrites spm-supervisor.exe from the running
	// executable and keeps the existing SCM registration.
	if err := b.installSCMService(name); err != nil {
		return false, err
	}
	if !running {
		return false, nil
	}
	if err := b.startSCMService(name); err != nil {
		return false, err
	}
	return true, nil
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
