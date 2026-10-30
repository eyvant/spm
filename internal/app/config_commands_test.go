package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetUpdatesSavedEnvironmentAndCreatesHistory(t *testing.T) {
	directory := localTempDir(t)
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	config := persistedStartConfig{Version: 1, Cwd: directory, Restart: "always", RestartSec: "3s", Enable: true, Command: []string{"go", "version"}, ExplicitCmd: true, Env: []string{"OLD=1", "KEEP=1"}}
	if err := a.installConfig("api", config, false); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("set", []string{"api", "--env", "OLD=2"}); err != nil {
		t.Fatal(err)
	}
	updated, err := a.readServiceConfig("api")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Env) != 2 || updated.Env[0] != "KEEP=1" || updated.Env[1] != "OLD=2" {
		t.Fatalf("unexpected environment: %v", updated.Env)
	}
	files, err := a.historyFiles("api")
	if err != nil || len(files) == 0 {
		t.Fatalf("history files = %v, error = %v", files, err)
	}
}

func TestSetCommandRepreparesAndReplacesCommand(t *testing.T) {
	directory := localTempDir(t)
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	config := persistedStartConfig{Version: 1, Cwd: directory, Restart: "always", RestartSec: "3s", Enable: true, Command: []string{"go", "version"}, ExplicitCmd: true}
	if err := a.installConfig("api", config, false); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("set", []string{"api", "--command", "--", "go", "env"}); err != nil {
		t.Fatal(err)
	}
	updated, err := a.readServiceConfig("api")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Command) != 2 || executableBase(updated.Command[0]) != "go" || updated.Command[1] != "env" {
		t.Fatalf("command = %v", updated.Command)
	}
}

func TestSetAddsLifecycleAndConvertsServiceToIntervalSchedule(t *testing.T) {
	directory := localTempDir(t)
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	config := persistedStartConfig{Version: 1, Cwd: directory, Restart: "always", RestartSec: "3s", Enable: true, Command: []string{"go", "version"}, ExplicitCmd: true}
	if err := a.installConfig("job", config, false); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("set", []string{"job", "--timer-interval", "30min", "--timer-randomized-delay", "2min", "--exec-start-pre", "/bin/true", "--on-failure", "notify.service"}); err != nil {
		t.Fatal(err)
	}
	updated, err := a.readServiceConfig("job")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Mode != "schedule" || updated.TimerInterval != "30min" || updated.TimerRandomDelay != "2min" || updated.Restart != "no" || len(updated.ExecStartPre) != 1 || len(updated.OnFailure) != 1 {
		t.Fatalf("unexpected updated config: %#v", updated)
	}
	if _, err := os.Stat(a.scheduleTimerPath("job")); err != nil {
		t.Fatalf("schedule timer was not created: %v", err)
	}
}

func TestExportUnwrapsSavedMiseCommand(t *testing.T) {
	var output strings.Builder
	writeDeclaration(&output, "api", persistedStartConfig{Command: []string{"/home/u/.local/bin/mise", "exec", "--", "node", "server.js"}, Mise: true, Enable: true})
	if strings.Contains(output.String(), "mise\", \"exec") || !strings.Contains(output.String(), `command = ["node", "server.js"]`) {
		t.Fatalf("unexpected export:\n%s", output.String())
	}
}

func TestExportWritesStructuredUnitAndTimerFields(t *testing.T) {
	var output strings.Builder
	writeDeclaration(&output, "job", persistedStartConfig{Command: []string{"/opt/job"}, Mode: "schedule", TimerOnBoot: "5min", TimerInterval: "30min", TimerRandomDelay: "2min", Group: "batch", ExecStartPre: []string{"/usr/bin/test -f /opt/job/config"}, ExecReload: "/bin/kill -HUP $MAINPID", OnFailure: []string{"notify.service"}})
	for _, want := range []string{`timer_on_boot = "5min"`, `timer_interval = "30min"`, `timer_randomized_delay = "2min"`, `group = "batch"`, `exec_start_pre = ["/usr/bin/test -f /opt/job/config"]`, `exec_reload = "/bin/kill -HUP $MAINPID"`, `on_failure = ["notify.service"]`} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("export does not contain %q:\n%s", want, output.String())
		}
	}
}
