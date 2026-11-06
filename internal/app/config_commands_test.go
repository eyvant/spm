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

func TestConfigCommandCloneAndHistory(t *testing.T) {
	directory := localTempDir(t)
	var output strings.Builder
	a := &application{runner: &fakeRunner{}, stdout: &output, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	config := persistedStartConfig{Version: 1, Cwd: directory, Restart: "always", RestartSec: "3s", Enable: true, Command: []string{"go", "version"}, ExplicitCmd: true}
	if err := a.installConfig("source", config, false); err != nil {
		t.Fatal(err)
	}
	if err := a.configCommand("clone", []string{"source", "copy"}); err != nil {
		t.Fatal(err)
	}
	cloned, err := a.readServiceConfig("copy")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cloned.Command, " ") != strings.Join(config.Command, " ") {
		t.Fatalf("cloned command = %v, want %v", cloned.Command, config.Command)
	}
	if err := a.configCommand("clone", []string{"source", "copy"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate clone error = %v", err)
	}
	if err := a.configCommand("history", []string{"source"}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigCommandAndMutationErrors(t *testing.T) {
	directory := localTempDir(t)
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	config := persistedStartConfig{Version: 1, Cwd: directory, Restart: "always", RestartSec: "3s", Enable: true, Command: []string{"go", "version"}, ExplicitCmd: true}
	if err := a.installConfig("api", config, false); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		command string
		args    []string
		want    string
	}{
		{"unsupported command", "wat", nil, "unsupported config command"},
		{"clone arity", "clone", []string{"api"}, "requires a source"},
		{"missing option", "set", []string{"api"}, "at least one option"},
		{"missing value", "set", []string{"api", "--cwd"}, "requires a value"},
		{"unknown option", "set", []string{"api", "--unknown", "value"}, "unknown set option"},
		{"command delimiter", "set", []string{"api", "--command", "go"}, "requires --"},
		{"persistent without schedule", "set", []string{"api", "--persistent"}, "requires a schedule"},
		{"invalid retries", "set", []string{"api", "--health-retries", "zero"}, "invalid health retries"},
		{"unset instances", "unset", []string{"api", "--instances", "2"}, "use `spm set"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := a.configCommand(test.command, test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestConfigMutationHelpers(t *testing.T) {
	values := []string{"a", "b"}
	if got := mutateString(append([]string(nil), values...), "b", false); strings.Join(got, ",") != "a,b" {
		t.Fatalf("duplicate mutateString = %v", got)
	}
	if got := mutateString(append([]string(nil), values...), "a", true); strings.Join(got, ",") != "b" {
		t.Fatalf("remove mutateString = %v", got)
	}
	if got := mutateString(append([]string(nil), values...), "c", false); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("append mutateString = %v", got)
	}

	keyValues := []string{"A=1", "B=2"}
	if got := mutateKeyValue(append([]string(nil), keyValues...), "A=3", false); strings.Join(got, ",") != "B=2,A=3" {
		t.Fatalf("replace mutateKeyValue = %v", got)
	}
	if got := mutateKeyValue(append([]string(nil), keyValues...), "B", true); strings.Join(got, ",") != "A=1" {
		t.Fatalf("remove mutateKeyValue = %v", got)
	}
}

func TestRollbackValidation(t *testing.T) {
	directory := localTempDir(t)
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	for _, test := range []struct {
		args []string
		want string
	}{
		{nil, "rollback requires"},
		{[]string{"missing"}, "not managed"},
		{[]string{"missing", "--revision", "zero"}, "not managed"},
	} {
		err := a.rollback(test.args)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("rollback(%v) error = %v, want %q", test.args, err, test.want)
		}
	}
}

func TestExportConfigSingleAllAndErrors(t *testing.T) {
	directory := localTempDir(t)
	var output strings.Builder
	a := &application{runner: &fakeRunner{}, stdout: &output, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	for _, name := range []string{"api", "worker"} {
		config := persistedStartConfig{Version: 1, Cwd: directory, Restart: "always", RestartSec: "3s", Enable: true, Command: []string{"go", "version"}, ExplicitCmd: true}
		if err := a.installConfig(name, config, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.exportConfig([]string{"api"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `name = "api"`) {
		t.Fatalf("single export = %q", output.String())
	}
	output.Reset()
	if err := a.exportConfig([]string{"all"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`name = "api"`, `name = "worker"`} {
		if !strings.Contains(output.String(), name) {
			t.Errorf("all export missing %q: %s", name, output.String())
		}
	}
	for _, args := range [][]string{nil, {"api", "worker"}, {"bad/name"}, {"missing"}} {
		if err := a.exportConfig(args); err == nil {
			t.Errorf("exportConfig(%v) unexpectedly succeeded", args)
		}
	}
}

func TestSetAndUnsetAllSupportedConfigFamilies(t *testing.T) {
	directory := localTempDir(t)
	addFakeExecutableToPath(t, "systemctl")
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	config := persistedStartConfig{Version: 1, Cwd: directory, Restart: "always", RestartSec: "3s", Enable: true, Command: []string{"go", "version"}, ExplicitCmd: true}
	if err := a.installConfig("api", config, false); err != nil {
		t.Fatal(err)
	}
	setArgs := []string{
		"api",
		"--cwd", directory,
		"--description", "API worker",
		"--restart", "on-failure",
		"--restart-sec", "4s",
		"--max-memory", "128M",
		"--env-file", "",
		"--exec-start-pre", "go version",
		"--exec-start-post", "go env",
		"--exec-stop", "go version",
		"--exec-reload", "go env",
		"--env", "MODE=test",
		"--property", "Nice=5",
		"--after", "network.target",
		"--wants", "network-online.target",
		"--requires", "database.service",
		"--on-failure", "notify.service",
		"--start-limit-interval", "30s",
		"--start-limit-burst", "5",
		"--type", "exec",
		"--no-enable",
	}
	if err := a.mutateConfig("set", setArgs); err != nil {
		t.Fatal(err)
	}
	updated, err := a.readServiceConfig("api")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != "API worker" || updated.Restart != "on-failure" || updated.MaxMemory != "128M" || updated.Enable || len(updated.Properties) != 1 || len(updated.ExecStartPre) != 1 {
		t.Fatalf("updated config = %#v", updated)
	}
	if err := a.mutateConfig("set", []string{"api", "--enable", "--cron-restart", "0 * * * *"}); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("unset", []string{"api", "--cron-restart", "--description", "--max-memory", "--env-file", "--exec-stop", "--exec-reload", "--start-limit-interval", "--start-limit-burst", "--type"}); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("unset", []string{"api", "--env", "MODE", "--property", "Nice", "--after", "network.target", "--wants", "network-online.target", "--requires", "database.service", "--on-failure", "notify.service", "--exec-start-pre", "go version", "--exec-start-post", "go env"}); err != nil {
		t.Fatal(err)
	}
	updated, err = a.readServiceConfig("api")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != "" || updated.CronRestart != "" || len(updated.Env)+len(updated.Properties)+len(updated.After)+len(updated.Wants)+len(updated.Requires)+len(updated.OnFailure)+len(updated.ExecStartPre)+len(updated.ExecStartPost) != 0 {
		t.Fatalf("unset config = %#v", updated)
	}
}

func TestSetScheduleHealthAndInstanceOptions(t *testing.T) {
	directory := localTempDir(t)
	addFakeExecutableToPath(t, "systemctl")
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	config := persistedStartConfig{Version: 1, Cwd: directory, Restart: "always", RestartSec: "3s", Enable: true, Command: []string{"go", "version"}, ExplicitCmd: true}
	if err := a.installConfig("job", config, false); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("set", []string{"job", "--schedule", "0 2 * * *", "--timezone", "Asia/Singapore", "--persistent", "--timer-randomized-delay", "30s"}); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("unset", []string{"job", "--schedule", "--timezone", "--timer-randomized-delay"}); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("set", []string{"job", "--health-http", "http://127.0.0.1/health", "--health-interval", "20s", "--health-timeout", "2s", "--health-retries", "2", "--health-retry-delay", "100ms", "--health-start-period", "5s", "--health-action", "restart"}); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("set", []string{"job", "--no-health"}); err != nil {
		t.Fatal(err)
	}
	updated, err := a.readServiceConfig("job")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Health.Probe.Kind != "" || updated.Mode != "" {
		t.Fatalf("final config = %#v", updated)
	}
	if err := a.installConfig("scaled", config, false); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateConfig("set", []string{"scaled", "--instances", "2"}); err != nil {
		t.Fatal(err)
	}
}
