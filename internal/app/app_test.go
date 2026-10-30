package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testExitError int

func (e testExitError) Error() string { return "exit status" }
func (e testExitError) ExitCode() int { return int(e) }

func TestParseLogsAcceptsOptionsBeforeAndAfterName(t *testing.T) {
	tests := []struct {
		args   []string
		name   string
		follow bool
		lines  string
	}{
		{args: []string{"api", "-f"}, name: "api", follow: true, lines: "100"},
		{args: []string{"-f", "api"}, name: "api", follow: true, lines: "100"},
		{args: []string{"-n", "100", "-f", "api"}, name: "api", follow: true, lines: "100"},
		{args: []string{"api", "--lines=50", "--follow"}, name: "api", follow: true, lines: "50"},
		{args: []string{"api", "--since", "1h", "--until=now", "--grep", "ERROR", "--boot", "-o", "json"}, name: "api", lines: "100"},
	}
	for _, test := range tests {
		options, err := parseLogs(test.args)
		if err != nil {
			t.Fatalf("parseLogs(%v): %v", test.args, err)
		}
		if strings.Join(options.names, ",") != test.name || options.follow != test.follow || options.lines != test.lines {
			t.Errorf("parseLogs(%v) = %#v", test.args, options)
		}
	}
}

func TestLogsPassesJournalFilters(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.logs([]string{"api", "--since", "1h", "--until", "now", "--grep", "ERROR", "--boot", "--output", "json"}); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	for _, want := range []string{"--since 1h", "--until now", "--grep ERROR", "--boot", "--output json"} {
		if !strings.Contains(got, want) {
			t.Errorf("logs call does not contain %q: %s", want, got)
		}
	}
}

func TestDeleteAllRequiresYes(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.removeMany([]string{"all"}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("removeMany(all) error = %v", err)
	}
}

func TestDeleteRemovesSavedHistory(t *testing.T) {
	directory := localTempDir(t)
	configDir := filepath.Join(directory, "config")
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: configDir}
	if err := os.WriteFile(a.unitPath("api"), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.healthPath("api", ".health.timer"), []byte("timer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.historyDirectory("api"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.historyDirectory("api"), "revision.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.remove("api"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.historyDirectory("api")); !os.IsNotExist(err) {
		t.Fatalf("history directory still exists after delete: %v", err)
	}
	resetCall := "reset-failed " + strings.Join([]string{unitName("api"), healthServiceName("api"), healthTimerName("api"), healthRecoveryName("api")}, " ")
	if calls := callsString(a.runner.(*fakeRunner).calls); !strings.Contains(calls, resetCall) {
		t.Fatalf("delete did not reset failed companion units:\n%s", calls)
	}
}

func TestParseLogsRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"all", "api"},
		{"--unknown", "api"},
		{"--lines", "api"},
		{"--lines=-1", "api"},
		{"--follow=true", "api"},
	} {
		if _, err := parseLogs(args); err == nil {
			t.Errorf("parseLogs(%v) unexpectedly succeeded", args)
		}
	}
}

func TestListRejectsExtraArguments(t *testing.T) {
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: localTempDir(t)}
	if err := a.run([]string{"list", "unexpected"}); err == nil {
		t.Fatal("list unexpectedly accepted an argument")
	}
}

func TestStatusAcceptsSystemctlInactiveExitCode(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		code    int
		wantErr bool
	}{
		{code: 3, wantErr: false},
		{code: 1, wantErr: true},
	} {
		runner := &fakeRunner{interactiveErr: testExitError(test.code)}
		a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
		err := a.status("api", nil)
		if (err != nil) != test.wantErr {
			t.Errorf("status exit code %d error = %v, wantErr %t", test.code, err, test.wantErr)
		}
	}
}

func TestSubcommandHelp(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "--help"},
		{"start", "-h"},
		{"help", "restart"},
		{"completion", "--help"},
		{"--system", "help", "start"},
	} {
		var output bytes.Buffer
		if err := Run(args, &output, io.Discard); err != nil {
			t.Fatalf("Run(%v): %v", args, err)
		}
		if !strings.Contains(output.String(), "Usage:") {
			t.Errorf("Run(%v) did not print usage: %q", args, output.String())
		}
	}
}

func TestGlobalHelpShowsSystemAsGlobalOptionOnly(t *testing.T) {
	var output bytes.Buffer
	printHelp(&output)
	if count := strings.Count(output.String(), "[--system]"); count != 1 {
		t.Fatalf("global help contains [--system] %d times:\n%s", count, output.String())
	}
}

func TestInteractiveCommandPreservesStandardStreams(t *testing.T) {
	stdin := strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd := interactiveCommand(stdin, &stdout, &stderr, "systemctl", "edit", "spm-api.service")
	if cmd.Stdin != stdin || cmd.Stdout != &stdout || cmd.Stderr != &stderr {
		t.Fatal("interactive command wrapped a standard stream and may hide its TTY")
	}
}

func TestStartCommandHelpAfterDelimiterBelongsToApplication(t *testing.T) {
	if hasCommandHelp([]string{"--", "server", "--help"}) {
		t.Fatal("application --help was interpreted as spm help")
	}
}

func TestResolveServiceNamesSupportsAllAndMultipleNames(t *testing.T) {
	directory := localTempDir(t)
	for _, name := range []string{"worker", "api"} {
		path := filepath.Join(directory, unitName(name))
		if err := os.WriteFile(path, []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, filename := range []string{cronRestartServiceName("api"), cronRestartTimerName("api")} {
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory}

	names, err := a.resolveServiceNames([]string{"all"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api", "worker"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("all resolved to %v, want %v", names, want)
	}
	names, err = a.resolveServiceNames([]string{"worker", "api", "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"worker", "api"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("multiple names resolved to %v, want %v", names, want)
	}
	if _, err := a.resolveServiceNames([]string{"all", "api"}); err == nil {
		t.Fatal("all combined with a service name unexpectedly succeeded")
	}
}

func TestControlManyRunsEachManagedService(t *testing.T) {
	directory := localTempDir(t)
	for _, name := range []string{"api", "worker"} {
		if err := os.WriteFile(filepath.Join(directory, unitName(name)), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.controlMany("restart", []string{"api", "worker"}); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	for _, call := range []string{
		"systemctl --user restart spm-api.service",
		"systemctl --user restart spm-worker.service",
	} {
		if !strings.Contains(got, call) {
			t.Errorf("missing call %q in:\n%s", call, got)
		}
	}
}

func TestSystemModeUsesSystemManager(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory, system: true}
	if err := a.control("restart", "api"); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	if !strings.Contains(got, "systemctl restart spm-api.service") {
		t.Fatalf("system manager was not used:\n%s", got)
	}
	if strings.Contains(got, "--user") {
		t.Fatalf("system mode passed --user:\n%s", got)
	}
}

func TestSystemModeLogsUseSystemJournal(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory, system: true}
	if err := a.logs([]string{"api"}); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	if !strings.Contains(got, "journalctl --unit spm-api.service --no-pager --lines 100") {
		t.Fatalf("system journal was not used:\n%s", got)
	}
	if strings.Contains(got, "--user") {
		t.Fatalf("system logs passed a user selector:\n%s", got)
	}
}

func TestSystemStartupDoesNotEnableLinger(t *testing.T) {
	runner := &fakeRunner{}
	var output strings.Builder
	a := &application{runner: runner, stdout: &output, stderr: io.Discard, system: true}
	if err := a.startup(nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 || !strings.Contains(output.String(), "do not need user lingering") {
		t.Fatalf("unexpected system startup behavior: calls=%v output=%q", runner.calls, output.String())
	}
}

func TestControlManyContinuesAfterFailure(t *testing.T) {
	directory := localTempDir(t)
	for _, name := range []string{"api", "worker"} {
		if err := os.WriteFile(filepath.Join(directory, unitName(name)), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "spm-api.service") {
			return nil, testExitError(1)
		}
		return nil, nil
	}}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.controlMany("restart", []string{"api", "worker"}); err == nil {
		t.Fatal("controlMany unexpectedly succeeded")
	}
	if got := callsString(runner.calls); !strings.Contains(got, "spm-worker.service") {
		t.Fatalf("worker was skipped after api failure:\n%s", got)
	}
}

func TestRestartStopsCompanionTimersBeforeService(t *testing.T) {
	directory := localTempDir(t)
	for _, name := range []string{unitName("api"), healthTimerName("api"), cronRestartTimerName("api")} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.control("restart", "api"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"stop " + healthTimerName("api"),
		"stop " + cronRestartTimerName("api"),
		"restart " + unitName("api"),
		"start " + cronRestartTimerName("api"),
		"start " + healthTimerName("api"),
	}
	calls := callsString(runner.calls)
	position := 0
	for _, fragment := range want {
		index := strings.Index(calls[position:], fragment)
		if index < 0 {
			t.Fatalf("systemctl calls do not contain %q in order:\n%s", fragment, calls)
		}
		position += index + len(fragment)
	}
}

func TestListQueriesAllUnitsAtOnce(t *testing.T) {
	directory := localTempDir(t)
	for _, name := range []string{"api", "worker"} {
		if err := os.WriteFile(filepath.Join(directory, unitName(name)), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
		if name != "systemctl" {
			return nil, nil
		}
		return []byte("Id=spm-api.service\nActiveState=active\nSubState=running\nMainPID=10\nMemoryCurrent=1048576\nNRestarts=2\nUnitFileState=enabled\nActiveEnterTimestampMonotonic=1000000\n\nId=spm-worker.service\nActiveState=inactive\nSubState=dead\nMainPID=0\nMemoryCurrent=[not set]\nNRestarts=0\nUnitFileState=disabled\nActiveEnterTimestampMonotonic=0\n"), nil
	}}
	var output strings.Builder
	a := &application{runner: runner, stdout: &output, stderr: io.Discard, unitDir: directory, sleep: func(time.Duration) {}}
	if err := a.list(); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || callsString(runner.calls[:1]) != callsString(runner.calls[1:]) {
		t.Fatalf("systemctl calls = %d, want one batched query per CPU sample:\n%s", len(runner.calls), callsString(runner.calls))
	}
	for _, want := range []string{"RESTARTS", "ENABLED", "CPU", "api", "2", "enabled", "worker", "disabled"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("list output does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestListUsesScheduleTimerStatusAndEnabledState(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("backup")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, scheduleTimerName("backup")), []byte("timer"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		if strings.Contains(call, scheduleTimerName("backup")) {
			return []byte("Id=spm-backup.timer\nActiveState=active\nSubState=waiting\nUnitFileState=enabled\nActiveEnterTimestampMonotonic=2000000\n"), nil
		}
		return []byte("Id=spm-backup.service\nActiveState=inactive\nSubState=dead\nMainPID=0\nMemoryCurrent=[not set]\nNRestarts=0\nUnitFileState=static\nActiveEnterTimestampMonotonic=0\n"), nil
	}}
	var output strings.Builder
	a := &application{runner: runner, stdout: &output, stderr: io.Discard, unitDir: directory}
	if err := a.list(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"backup", "active/waiting", "enabled"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("list output does not contain %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "inactive/dead") || strings.Contains(output.String(), "static") {
		t.Fatalf("list displayed service state instead of schedule timer state:\n%s", output.String())
	}
}

func TestLogsDefaultsToLastHundredLinesAndStreamsUserJournal(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{runFunc: func(string, ...string) ([]byte, error) {
		return nil, testExitError(1)
	}}
	a := &application{runner: runner, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.logs([]string{"api"}); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	for _, want := range []string{"--user --unit spm-api.service", "--lines 100"} {
		if !strings.Contains(got, want) {
			t.Errorf("logs call does not contain %q: %s", want, got)
		}
	}
}

func TestLogsFallsBackToUserUnitSelector(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{interactiveFunc: func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "--user" {
			return testExitError(1)
		}
		return nil
	}}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.logs([]string{"api"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !strings.Contains(callsString(runner.calls), "--user-unit spm-api.service") {
		t.Fatalf("journal fallback was not used:\n%s", callsString(runner.calls))
	}
}

func TestShowUsesSummaryWhileStatusUsesSystemctlStatus(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	var output strings.Builder
	a := &application{runner: runner, stdin: strings.NewReader(""), stdout: &output, stderr: io.Discard, unitDir: directory}
	if err := a.run([]string{"show", "api"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "unit file") {
		t.Fatalf("show did not render summary:\n%s", output.String())
	}
	if got := callsString(runner.calls); strings.Contains(got, "systemctl --user status") {
		t.Fatalf("show unexpectedly called systemctl status:\n%s", got)
	}
	runner.calls = nil
	if err := a.run([]string{"status", "api"}); err != nil {
		t.Fatal(err)
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user status") {
		t.Fatalf("status did not call systemctl status:\n%s", got)
	}
}
