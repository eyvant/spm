//go:build windows

package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func handoffLayout(t *testing.T, name string) Layout {
	t.Helper()
	root, err := os.MkdirTemp(".", ".spm-test-*")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.Abs(root)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	ping, err := exec.LookPath("ping.exe")
	if err != nil {
		t.Skipf("ping.exe unavailable: %v", err)
	}
	l := Layout{Root: root}
	if err := os.MkdirAll(l.ServiceDir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{Version: StateVersion, Name: name, Cwd: root, Command: []string{ping, "-n", "120", "127.0.0.1"}, Restart: "no", RestartSec: "1s", Enabled: true}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(l.ConfigPath(name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

func superviseAsync(l Layout, name string) chan error {
	done := make(chan error, 1)
	go func() { done <- Supervise(l, name) }()
	return done
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitReturn(t *testing.T, done chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func alive(pid int) bool {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(process)
	event, _ := windows.WaitForSingleObject(process, 0)
	return event == uint32(windows.WAIT_TIMEOUT)
}

func TestSupervisorHandoffKeepsProcessRunning(t *testing.T) {
	const name = "handoff"
	l := handoffLayout(t, name)
	first := superviseAsync(l, name)
	waitFor(t, "first supervisor to start the process", func() bool { s := l.ReadState(name); return s.Status == "running" && s.PID > 0 })
	pid := l.ReadState(name).PID
	started := l.ReadState(name).StartedAt

	// The CLI's part: hold the job (query access only), then ask the supervisor to detach.
	job, err := HoldJob(l, name)
	if err != nil {
		t.Fatalf("open job of a version 2 supervisor: %v", err)
	}
	if err := Detach(l, name); err != nil {
		t.Fatal(err)
	}
	waitReturn(t, first, "first supervisor to detach")
	if !alive(pid) {
		t.Fatal("detaching terminated the service process")
	}
	if s := l.ReadState(name); s.SupervisorPID != 0 || s.PID != pid {
		t.Fatalf("state after detach = %+v", s)
	}

	second := superviseAsync(l, name)
	waitFor(t, "second supervisor to adopt", func() bool { return l.ReadState(name).SupervisorPID != 0 })
	_ = windows.CloseHandle(job)
	time.Sleep(300 * time.Millisecond)
	state := l.ReadState(name)
	if state.PID != pid || !alive(pid) {
		t.Fatalf("second supervisor did not adopt process %d: %+v", pid, state)
	}
	if !state.StartedAt.Equal(started) {
		t.Fatalf("adoption reset the start time: %v, want %v", state.StartedAt, started)
	}

	// The adopted process is fully supervised: stop terminates it.
	if err := l.WriteControl(name, "stop"); err != nil {
		t.Fatal(err)
	}
	waitReturn(t, second, "second supervisor to stop")
	waitFor(t, "process to exit", func() bool { return !alive(pid) })
	if s := l.ReadState(name); s.Status != "stopped" || s.PID != 0 {
		t.Fatalf("state after stop = %+v", s)
	}
}

func TestSupervisorDetachWithoutHolderTerminatesProcess(t *testing.T) {
	// Nobody holds the job: when the detached supervisor's handle closes the
	// kill-on-close job terminates the process, as a restart would.
	const name = "orphan"
	l := handoffLayout(t, name)
	first := superviseAsync(l, name)
	waitFor(t, "process start", func() bool { s := l.ReadState(name); return s.Status == "running" && s.PID > 0 })
	pid := l.ReadState(name).PID
	if err := Detach(l, name); err != nil {
		t.Fatal(err)
	}
	waitReturn(t, first, "supervisor to detach")
	waitFor(t, "unheld process to exit", func() bool { return !alive(pid) })

	// The next supervisor finds nothing to adopt and starts a new process.
	second := superviseAsync(l, name)
	waitFor(t, "fresh start", func() bool { s := l.ReadState(name); return s.Status == "running" && s.PID > 0 && s.PID != pid })
	_ = l.WriteControl(name, "stop")
	waitReturn(t, second, "supervisor to stop")
}

func TestAdoptRejectsProcessOutsideTheJob(t *testing.T) {
	const name = "stranger"
	l := handoffLayout(t, name)
	ping, _ := exec.LookPath("ping.exe")
	stranger := exec.Command(ping, "-n", "30", "127.0.0.1")
	if err := stranger.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stranger.Process.Kill(); _, _ = stranger.Process.Wait() })
	// A job exists under the service's name, but the PID is not in it.
	job, err := createJob(l, name)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)
	if adopted, _ := adopt(l, name, stranger.Process.Pid); adopted != nil {
		t.Fatal("adopted a process outside the service's job")
	}
	if adopted, _ := adopt(l, name, 0); adopted != nil {
		t.Fatal("adopted PID 0")
	}
}

func TestJobNameScopesByRootAndNamespace(t *testing.T) {
	user := Layout{Root: `C:\Data\spm`}
	if got := user.JobName("api"); got != (Layout{Root: `c:\data\spm`}).JobName("api") || got[:6] != `Local\` {
		t.Fatalf("user job name %q", got)
	}
	if got := (Layout{Root: `C:\Data\spm`, Global: true}).JobName("api"); got[:7] != `Global\` {
		t.Fatalf("system job name %q", got)
	}
	if user.JobName("api") == (Layout{Root: `C:\Other`}).JobName("api") {
		t.Fatal("different roots share a job name")
	}
}
