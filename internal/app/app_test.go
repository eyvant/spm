package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
	if !strings.Contains(output.String(), "spm - a simple, cross-platform manager for native system services") {
		t.Fatalf("global help does not contain the product description:\n%s", output.String())
	}
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

func TestListQueriesAllUnitsOnce(t *testing.T) {
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
	a := &application{runner: runner, stdout: &output, stderr: io.Discard, unitDir: directory, sleep: func(duration time.Duration) {
		t.Fatalf("list unexpectedly slept for %s", duration)
	}}
	if err := a.list(); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("systemctl calls = %d, want one batched query:\n%s", len(runner.calls), callsString(runner.calls))
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

func TestPublicRunGlobalDispatch(t *testing.T) {
	root := localTempDir(t)
	t.Setenv("SPM_WINDOWS_DATA_DIR", root)
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	tests := []struct {
		name string
		args []string
		err  string
		out  string
	}{
		{"no args", nil, "", "Usage:"},
		{"help", []string{"help"}, "", "Usage:"},
		{"global help", []string{"--help"}, "", "Usage:"},
		{"command help", []string{"help", "start"}, "", "spm start"},
		{"inline command help", []string{"start", "--help"}, "", "spm start"},
		{"version", []string{"version"}, "", "spm "},
		{"short version", []string{"-v"}, "", "spm "},
		{"version help", []string{"version", "--help"}, "", "spm version"},
		{"completion help", []string{"completion", "--help"}, "", "completion"},
		{"powershell completion", []string{"completion", "powershell"}, "", "Register-ArgumentCompleter"},
		{"list", []string{"list", "--json"}, "", "[]"},
		{"mcp arguments", []string{"mcp", "extra"}, "does not accept", ""},
		{"help arguments", []string{"help", "start", "extra"}, "at most one", ""},
		{"global help arguments", []string{"--help", "extra"}, "does not accept", ""},
		{"version arguments", []string{"version", "extra"}, "does not accept", ""},
		{"health probe arguments", []string{"health-probe"}, "requires TYPE TARGET", ""},
		{"scheduled run arguments", []string{"__scheduled-run"}, "requires", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := Run(test.args, &output, io.Discard)
			if test.err == "" {
				if err != nil {
					t.Fatalf("Run(%v): %v", test.args, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Fatalf("Run(%v) error = %v, want %q", test.args, err, test.err)
			}
			if test.out != "" && !strings.Contains(output.String(), test.out) {
				t.Fatalf("Run(%v) output missing %q:\n%s", test.args, test.out, output.String())
			}
		})
	}
}

func TestExecRunnerAndCommandErrors(t *testing.T) {
	runner := execRunner{}
	shell := "sh"
	successArgs := []string{"-c", "echo hello"}
	failureArgs := []string{"-c", "echo failure; exit 7"}
	interactiveArgs := []string{"-c", "echo interactive"}
	if runtime.GOOS == "windows" {
		shell = "cmd.exe"
		successArgs = []string{"/d", "/c", "echo hello"}
		failureArgs = []string{"/d", "/c", "echo failure & exit /b 7"}
		interactiveArgs = []string{"/d", "/c", "echo interactive"}
	}
	output, err := runner.Run(shell, successArgs...)
	if err != nil || !strings.Contains(string(output), "hello") {
		t.Fatalf("exec runner output = %q, %v", output, err)
	}
	if _, err := runner.Run(shell, failureArgs...); err == nil || !strings.Contains(err.Error(), "failure") {
		t.Fatalf("exec runner failure = %v", err)
	}
	var interactive bytes.Buffer
	if err := runner.Interactive(strings.NewReader(""), &interactive, io.Discard, shell, interactiveArgs...); err != nil || !strings.Contains(interactive.String(), "interactive") {
		t.Fatalf("interactive runner output = %q, %v", interactive.String(), err)
	}
	base := errors.New("exit")
	for _, test := range []struct {
		message string
		want    string
	}{
		{"Failed to connect to bus", "systemd manager"},
		{"XDG_RUNTIME_DIR is missing", "systemd manager"},
		{"No journal files were found", "journal storage"},
		{"permission denied reading journal", "journal storage"},
		{"plain failure", "plain failure"},
	} {
		if got := commandError(test.message, base); !strings.Contains(got.Error(), test.want) || !errors.Is(got, base) {
			t.Errorf("commandError(%q) = %v", test.message, got)
		}
	}
	if got := commandError("", base); got != base {
		t.Fatalf("empty command error = %v", got)
	}
	var notices bytes.Buffer
	a := &application{stderr: &notices}
	a.warn("careful %d", 1)
	a.hint("try %s", "again")
	if !strings.Contains(notices.String(), "Warning: careful 1") || !strings.Contains(notices.String(), "Hint: try again") {
		t.Fatalf("notices = %q", notices.String())
	}
}

func TestGenericHelpAndApplicationDispatch(t *testing.T) {
	var output bytes.Buffer
	printGenericHelp(&output)
	if !strings.Contains(output.String(), "Common commands") {
		t.Fatalf("generic help output = %q", output.String())
	}
	commands := []string{
		"start", "schedule", "run", "list", "ls", "status-all", "signal", "kill", "scale", "doctor",
		"status", "show", "describe", "restart", "stop", "reload", "enable", "disable", "set", "unset",
		"clone", "history", "rollback", "delete", "remove", "rm", "logs", "cat", "edit", "startup",
		"apply", "validate", "diff", "native", "security", "export", "health", "import", "mcp", "version", "completion",
	}
	for _, command := range commands {
		output.Reset()
		if err := printGenericCommandHelp(&output, command); err != nil {
			t.Errorf("printGenericCommandHelp(%q): %v", command, err)
		}
		if output.Len() == 0 {
			t.Errorf("printGenericCommandHelp(%q) produced no output", command)
		}
	}
	if err := printGenericCommandHelp(io.Discard, "missing"); err == nil {
		t.Fatal("unknown generic help command unexpectedly succeeded")
	}

	directory := localTempDir(t)
	a := &application{runner: &fakeRunner{}, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	invocations := [][]string{
		{"start"}, {"schedule"}, {"run"}, {"list", "--bad"}, {"signal"}, {"kill"}, {"scale"}, {"doctor", "extra"},
		{"status"}, {"show"}, {"describe"}, {"restart"}, {"stop"}, {"reload"}, {"enable"}, {"disable"},
		{"set"}, {"unset"}, {"clone"}, {"history"}, {"rollback"}, {"delete"}, {"remove"}, {"rm"},
		{"logs", "--bad"}, {"cat"}, {"edit"}, {"startup", "extra"}, {"apply"}, {"validate"}, {"diff"},
		{"native"}, {"security"}, {"export"}, {"health"}, {"health-probe"}, {"import"}, {"unknown"},
	}
	for _, invocation := range invocations {
		_ = a.run(invocation)
	}

	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"status", "show", "cat"} {
		if err := a.native([]string{"systemd", action, "api"}); err != nil {
			t.Errorf("native %s: %v", action, err)
		}
	}
}

func TestDescribeTemplateAndConfigRows(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, templateUnitName("worker")), []byte("template"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{runFunc: func(_ string, _ ...string) ([]byte, error) {
		return []byte("Id=spm-worker@0.service\nActiveState=active\nSubState=running\nMainPID=10\nMemoryCurrent=1024\nNRestarts=1\nActiveEnterTimestampMonotonic=1000000\n\nId=spm-worker@1.service\nActiveState=inactive\nSubState=dead\nMainPID=0\nMemoryCurrent=0\nNRestarts=0\nActiveEnterTimestampMonotonic=0\n"), nil
	}}
	var output bytes.Buffer
	a := &application{runner: runner, stdout: &output, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	config := persistedStartConfig{Version: 1, Command: []string{"go", "run", "worker.go"}, Cwd: directory, Restart: "always", RestartSec: "3s", Env: []string{"PORT=3000", "DEBUG=1", "INVALID"}, Instances: 2}
	if err := a.writeServiceConfig("worker", config); err != nil {
		t.Fatal(err)
	}
	if err := a.describeInstances("worker"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"instances", "worker@0", "worker@1", "PORT,DEBUG", "current user"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("template description missing %q:\n%s", want, output.String())
		}
	}
	if got := environmentKeys(nil); got != "-" {
		t.Fatalf("environmentKeys(nil) = %q", got)
	}
	a.system = true
	config.User = ""
	if err := a.writeServiceConfig("worker", config); err != nil {
		t.Fatal(err)
	}
	rows := a.configRows("worker")
	if len(rows) == 0 {
		t.Fatal("system config rows are empty")
	}
	if rows := a.configRows("missing"); rows != nil {
		t.Fatalf("missing config rows = %#v", rows)
	}
	for _, duration := range []time.Duration{0, time.Second, time.Minute, time.Hour, 24 * time.Hour} {
		_ = formatUptime(duration, 10)
	}
	for _, bytes := range []string{"", "0", "1023", "1024", "1048576", "1073741824", "bad"} {
		_ = formatBytes(bytes)
	}
}

func TestInspectStartupAndSmallHelpers(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{lingerOutput: []byte("no\n")}
	var output bytes.Buffer
	a := &application{runner: runner, stdin: strings.NewReader(""), stdout: &output, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	for _, action := range []string{"cat", "edit"} {
		if err := a.inspectUnit(action, "api", nil); err != nil {
			t.Errorf("inspectUnit(%s): %v", action, err)
		}
	}
	if err := a.inspectUnit("cat", "", errors.New("parse")); err == nil {
		t.Fatal("inspectUnit parse error was ignored")
	}
	if err := a.inspectUnit("cat", "missing", nil); err == nil {
		t.Fatal("inspectUnit missing service unexpectedly succeeded")
	}
	if err := a.startup(nil); err != nil {
		t.Fatal(err)
	}
	if err := a.startup([]string{"extra"}); err == nil {
		t.Fatal("startup arguments unexpectedly succeeded")
	}
	for action, want := range map[string]string{"start": "started", "stop": "stopped", "restart": "restarted", "reload": "reloaded", "enable": "enabled", "disable": "disabled and stopped", "other": "other"} {
		if got := actionPastTense(action); got != want {
			t.Errorf("actionPastTense(%q) = %q, want %q", action, got, want)
		}
	}
	if got := a.savedRestartPolicy("missing"); got != "always" {
		t.Errorf("missing restart policy = %q", got)
	}
	config := persistedStartConfig{Version: 1, Restart: "on-failure"}
	if err := a.writeServiceConfig("api", config); err != nil {
		t.Fatal(err)
	}
	if got := a.savedRestartPolicy("api"); got != "on-failure" {
		t.Errorf("saved restart policy = %q", got)
	}
}
