//go:build darwin

package supervisor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLaunchdConfig(t *testing.T, l Layout, name string, command ...string) {
	t.Helper()
	if err := os.MkdirAll(l.ServiceDir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(Config{Version: StateVersion, Name: name, Command: command, Restart: "on-failure", RestartSec: "1s"})
	if err := os.WriteFile(l.ConfigPath(name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The supervisor copies the service's output to service.log, records the run
// in spm.log and exits with the service's exit code for launchd.
func TestRunLaunchdSeparatesOutputAndMirrorsExitCode(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	writeLaunchdConfig(t, l, "job", "/bin/sh", "-c", "echo out; echo err >&2; exit 3")
	err := RunLaunchd(l, "job", "")
	var status ExitStatus
	if !errors.As(err, &status) || status.Code != 3 {
		t.Fatalf("RunLaunchd = %v, want exit status 3", err)
	}
	output, _ := os.ReadFile(l.LogPath("job"))
	if string(output) != "out\nerr\n" {
		t.Fatalf("service.log = %q", output)
	}
	events, _ := os.ReadFile(l.EventLogPath("job"))
	for _, want := range []string{"starting /bin/sh -c", "exited with code 3"} {
		if !strings.Contains(string(events), want) {
			t.Errorf("spm.log does not contain %q:\n%s", want, events)
		}
	}
	state := l.ReadState("job")
	if state.Status != "failed" || state.ExitCode == nil || *state.ExitCode != 3 || state.SupervisorPID != 0 || state.Build == "" {
		t.Fatalf("state = %+v", state)
	}
}

func TestRunLaunchdReportsMissingCommand(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	writeLaunchdConfig(t, l, "broken", filepath.Join(l.Root, "missing"))
	if err := RunLaunchd(l, "broken", ""); err == nil {
		t.Fatal("a missing command was started")
	}
	events, _ := os.ReadFile(l.EventLogPath("broken"))
	if !strings.Contains(string(events), "start failed") {
		t.Fatalf("spm.log = %q", events)
	}
}

func TestParseAdopt(t *testing.T) {
	pid, fd, started, err := parseAdopt("42:7:1700000000000000000")
	if err != nil || pid != 42 || fd != 7 || started.UnixNano() != 1700000000000000000 {
		t.Fatalf("parseAdopt = %d %d %v %v", pid, fd, started, err)
	}
	for _, spec := range []string{"", "1:2", "a:1:1", "0:1:1", "1:-1:1"} {
		if _, _, _, err := parseAdopt(spec); err == nil {
			t.Errorf("parseAdopt(%q) succeeded", spec)
		}
	}
}
