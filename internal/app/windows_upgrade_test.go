//go:build windows

package app

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// startFakeSpmProcess runs a long-lived copy of ping.exe named spm.exe, which
// looks like a scheduler daemon by image name.
func startFakeSpmProcess(t *testing.T, name string) *exec.Cmd {
	t.Helper()
	ping, err := exec.LookPath("ping.exe")
	if err != nil {
		t.Skipf("ping.exe unavailable: %v", err)
	}
	data, err := os.ReadFile(ping)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(localTempDir(t), name)
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// processExited reports whether the process has terminated. OpenProcess alone
// is not enough: exec.Cmd keeps a handle, so an exited process can still be
// opened.
func processExited(t *testing.T, cmd *exec.Cmd) bool {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == windows.WAIT_OBJECT_0
}

func writeSchedulerLock(t *testing.T, b *windowsBackend, pid int, written time.Time) {
	t.Helper()
	path := b.scheduleDaemonPIDPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsStopScheduleDaemonProcessVerifiesTheLockOwner(t *testing.T) {
	b := &windowsBackend{root: localTempDir(t), stdout: io.Discard, stderr: io.Discard}

	// No lock, a malformed lock, and this process's own PID are all no-ops.
	if err := b.stopScheduleDaemonProcess(); err != nil {
		t.Fatalf("missing lock: %v", err)
	}
	writeSchedulerLock(t, b, os.Getpid(), time.Now())
	if err := b.stopScheduleDaemonProcess(); err != nil {
		t.Fatalf("own pid: %v", err)
	}

	// A process that is not spm keeps running even when it holds the PID.
	other := startFakeSpmProcess(t, "other.exe")
	writeSchedulerLock(t, b, other.Process.Pid, time.Now().Add(time.Hour))
	if err := b.stopScheduleDaemonProcess(); err != nil {
		t.Fatal(err)
	}
	if processExited(t, other) {
		t.Fatal("terminated a process that is not spm")
	}

	// An spm process started after the lock was written reuses a stale PID.
	daemon := startFakeSpmProcess(t, "spm.exe")
	writeSchedulerLock(t, b, daemon.Process.Pid, time.Now().Add(-time.Hour))
	if err := b.stopScheduleDaemonProcess(); err != nil {
		t.Fatal(err)
	}
	if processExited(t, daemon) {
		t.Fatal("terminated an spm process that started after the lock was written")
	}

	// The real owner is terminated and has exited when the call returns.
	writeSchedulerLock(t, b, daemon.Process.Pid, time.Now().Add(time.Hour))
	if err := b.stopScheduleDaemonProcess(); err != nil {
		t.Fatal(err)
	}
	if !processExited(t, daemon) {
		t.Fatal("scheduler daemon survived")
	}
}

func TestWindowsUpgradeRuntimeWithNothingRunning(t *testing.T) {
	root := localTempDir(t)
	var output bytes.Buffer
	b := &windowsBackend{root: root, stdout: &output, stderr: io.Discard}
	stopped := windowsServiceConfig{Version: windowsStateVersion, Name: "idle", Command: []string{"idle.exe"}, Cwd: root, Enabled: true}
	if err := b.writeConfig(stopped); err != nil {
		t.Fatal(err)
	}
	if err := b.upgradeRuntime(upgradeComponents{supervisor: true, scheduler: true}); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("stopped services were reported as restarted: %q", output.String())
	}
	if restarted, err := b.upgradeUserService("idle"); restarted || err != nil {
		t.Fatalf("upgradeUserService(idle) = %v, %v", restarted, err)
	}
}

func TestUpgradeRuntimeReportFile(t *testing.T) {
	root := localTempDir(t)
	t.Setenv("SPM_WINDOWS_DATA_DIR", root)
	report := filepath.Join(root, "report.log")
	if err := runUpgradeRuntime([]string{"--report", report}, io.Discard, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(report); err != nil {
		t.Fatalf("report file was not written: %v", err)
	}
	if err := runUpgradeRuntime([]string{"--report"}, io.Discard, io.Discard, false); err == nil {
		t.Fatal("missing report path was accepted")
	}
	if err := runUpgradeRuntime([]string{"extra"}, io.Discard, io.Discard, false); err == nil {
		t.Fatal("unexpected argument was accepted")
	}
}
