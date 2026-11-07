//go:build windows

package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWindowsListIncludesSchedule(t *testing.T) {
	var output bytes.Buffer
	b := &windowsBackend{root: localTempDir(t), stdout: &output, stderr: io.Discard}
	config := windowsServiceConfig{
		Version: windowsStateVersion,
		Name:    "backup", Command: []string{"backup.exe"}, Mode: "schedule", Enabled: true,
		Schedule: "0 4 * * *", Timezone: "Asia/Shanghai",
	}
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := b.list(listOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "0 4 * * * Asia/Shanghai") {
		t.Fatalf("list output does not include schedule:\n%s", got)
	}
}

func TestWindowsRunAsScopesAndAliases(t *testing.T) {
	tests := []struct {
		value  string
		system bool
		want   string
		err    string
	}{
		{system: false, want: ""},
		{value: "bob", system: false, err: "requires global --system"},
		{system: true, want: "SYSTEM"},
		{value: "LocalSystem", system: true, want: "SYSTEM"},
		{value: "LocalService", system: true, want: `NT AUTHORITY\LOCALSERVICE`},
		{value: `NT AUTHORITY\NETWORK SERVICE`, system: true, want: `NT AUTHORITY\NETWORKSERVICE`},
		{value: `DOMAIN\svc-spm`, system: true, want: `DOMAIN\svc-spm`},
		{value: `DOMAIN\gmsa$`, system: true, want: `DOMAIN\gmsa$`},
		{value: `NT SERVICE\spm-api`, system: true, want: `NT SERVICE\spm-api`},
	}
	for _, test := range tests {
		got, err := windowsRunAs(test.value, test.system)
		if test.err != "" {
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Errorf("windowsRunAs(%q, %v) error = %v, want %q", test.value, test.system, err, test.err)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("windowsRunAs(%q, %v) = %q, %v; want %q", test.value, test.system, got, err, test.want)
		}
	}
}

func TestRenderWindowsScheduleTaskUsesPortableRunner(t *testing.T) {
	config := windowsServiceConfig{Name: "backup", Description: "Nightly & safe", Enabled: true, Schedule: "*/5 * * * *"}
	xml := renderWindowsScheduleTask(config, `C:\data\schedule-timer.vbs`, "S-1-5-18", "", "timer", true)
	for _, want := range []string{"<Interval>PT1M</Interval>", "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<RunLevel>HighestAvailable</RunLevel>", "wscript.exe", "//B //Nologo", "schedule-timer.vbs", "Nightly &amp; safe", "S-1-5-18"} {
		if !strings.Contains(xml, want) {
			t.Errorf("task XML does not contain %q:\n%s", want, xml)
		}
	}
	if strings.Contains(xml, "<LogonType>") {
		t.Errorf("system task XML should omit LogonType for Task Scheduler compatibility:\n%s", xml)
	}
}

func TestRenderWindowsDaemonExecutorHasNoPollingTrigger(t *testing.T) {
	config := windowsServiceConfig{Name: "backup", Enabled: true, Schedule: "30 0 4 * * *"}
	xml := renderWindowsScheduleTask(config, `C:\data\schedule-daemon.vbs`, "S-1-5-18", "", "daemon", true)
	for _, want := range []string{"<Triggers>", "schedule-daemon.vbs", "wscript.exe"} {
		if !strings.Contains(xml, want) {
			t.Errorf("daemon executor XML does not contain %q:\n%s", want, xml)
		}
	}
	if strings.Contains(xml, "<CalendarTrigger>") || strings.Contains(xml, "<Interval>PT1M</Interval>") {
		t.Fatalf("daemon executor unexpectedly polls:\n%s", xml)
	}
}

func TestRenderWindowsScheduleDaemonStartsAtScopeActivation(t *testing.T) {
	userXML := renderWindowsScheduleDaemonTask(`C:\data\scheduler.vbs`, "S-1-5-21-1", "InteractiveToken", false)
	if strings.Contains(userXML, "<LogonTrigger>") || !strings.Contains(userXML, "<Triggers></Triggers>") || !strings.Contains(userXML, "<RestartOnFailure>") {
		t.Fatalf("unexpected user scheduler XML:\n%s", userXML)
	}
	systemXML := renderWindowsScheduleDaemonTask(`C:\data\scheduler.vbs`, "S-1-5-18", "", true)
	if !strings.Contains(systemXML, "<BootTrigger>") || !strings.Contains(systemXML, "<RunLevel>HighestAvailable</RunLevel>") {
		t.Fatalf("unexpected system scheduler XML:\n%s", systemXML)
	}
}

func TestWriteWindowsScheduleLauncherHidesRunner(t *testing.T) {
	path := filepath.Join(localTempDir(t), "schedule-timer.vbs")
	if err := writeWindowsScheduleLauncher(path, `C:\Program Files\spm\spm.exe`, `C:\data\schedule.json`, `C:\data\state`, `C:\data\task.log`, "timer"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{`shell.Run(`, `, 0, True)`, `__scheduled-run`, `"C:\Program Files\spm\spm.exe"`, `--activation`, `timer`} {
		if !strings.Contains(script, want) {
			t.Errorf("launcher does not contain %q:\n%s", want, script)
		}
	}
}

func TestWindowsScheduledSystemPrincipalsOmitLogonType(t *testing.T) {
	b := &windowsBackend{system: true}
	for _, test := range []struct {
		runAs string
		want  string
	}{
		{"SYSTEM", "S-1-5-18"},
		{`NT AUTHORITY\LOCALSERVICE`, "S-1-5-19"},
		{`NT AUTHORITY\NETWORKSERVICE`, "S-1-5-20"},
	} {
		principal, logonType, err := b.scheduledTaskPrincipal(test.runAs)
		if err != nil || principal != test.want || logonType != "" {
			t.Errorf("scheduledTaskPrincipal(%q) = %q, %q, %v; want %q, empty logon type", test.runAs, principal, logonType, err, test.want)
		}
	}
}

func TestWindowsScheduledDeclarationIsPortable(t *testing.T) {
	b := &windowsBackend{root: t.TempDir(), stdout: io.Discard, stderr: io.Discard}
	service := serviceDeclaration{Name: "job", Command: []string{"cmd.exe", "/c", "exit", "0"}, Schedule: "*/5 9-17 1 * 1-5", Timezone: "Asia/Singapore", Persistent: true}
	config, err := b.prepareDeclaration(service)
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != "schedule" || config.Schedule != service.Schedule || config.Timezone != service.Timezone || !config.Persistent {
		t.Fatalf("unexpected schedule config: %#v", config)
	}
}

func TestWindowsEnvironmentOverridesCaseInsensitively(t *testing.T) {
	got := setWindowsEnv([]string{"Path=one", "KEEP=yes"}, "PATH", "two")
	want := []string{"PATH=two", "KEEP=yes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("setWindowsEnv() = %v, want %v", got, want)
	}
}

func TestWindowsRejectsPlatformSpecificStartOptions(t *testing.T) {
	options := defaultStartOptions()
	options.instances = 2
	if err := rejectWindowsStartOptions(options); err == nil || !strings.Contains(err.Error(), "--instances") {
		t.Fatalf("rejectWindowsStartOptions() error = %v", err)
	}
}

func TestWindowsSCMServiceName(t *testing.T) {
	backend := &windowsBackend{system: true}
	if got := backend.scmName("api"); got != "spm-api" {
		t.Fatalf("SCM service name = %q, want spm-api", got)
	}
}

func TestWindowsVirtualServiceAccount(t *testing.T) {
	if !windowsVirtualServiceAccount(`NT SERVICE\spm-api`) || windowsVirtualServiceAccount(`DOMAIN\svc-api`) {
		t.Fatal("virtual service account detection is incorrect")
	}
}

func TestWindowsSystemRootACLGrantsAllowServiceAccountTraversal(t *testing.T) {
	grants := strings.Join(windowsSystemRootACLGrants(), "\n")
	for _, expected := range []string{"*S-1-5-19:(RX)", "*S-1-5-20:(RX)", "*S-1-5-11:(RX)"} {
		if !strings.Contains(grants, expected) {
			t.Fatalf("root ACL grants %q do not include %q", grants, expected)
		}
	}
}

func TestWindowsSystemDataACLKeepsConfigAccessible(t *testing.T) {
	if !windowsIsElevated() {
		t.Skip("requires elevated token to test SYSTEM/Administrators ACLs")
	}
	b := &windowsBackend{root: t.TempDir(), system: true, stdout: io.Discard, stderr: io.Discard}
	config := windowsServiceConfig{Version: windowsStateVersion, Name: "acl-test", Command: []string{"cmd.exe"}, RunAs: `NT AUTHORITY\LOCALSERVICE`}
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := b.secureSystemData(config.Name, config.RunAs); err != nil {
		t.Fatal(err)
	}
	if _, err := b.readConfig(config.Name); err != nil {
		t.Fatalf("config inaccessible after securing ACLs: %v", err)
	}
	if err := b.writeConfig(config); err != nil {
		t.Fatalf("cannot replace secured config: %v", err)
	}
	if _, err := b.readConfig(config.Name); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPreparesPowerShellScriptAutomatically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.ps1")
	if err := os.WriteFile(path, []byte("Write-Output ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options, err := parseStart([]string{"--name", "worker", path})
	if err != nil {
		t.Fatal(err)
	}
	if err := options.prepare(); err != nil {
		t.Fatal(err)
	}
	command := strings.Join(options.command, " ")
	for _, want := range []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path} {
		if !strings.Contains(strings.ToLower(command), strings.ToLower(want)) {
			t.Fatalf("PowerShell command %q does not contain %q", command, want)
		}
	}
}

func TestWindowsConfigStateAndDiscovery(t *testing.T) {
	b := &windowsBackend{root: localTempDir(t), stdout: io.Discard, stderr: io.Discard}
	for _, name := range []string{"zeta", "alpha"} {
		config := windowsServiceConfig{Version: windowsStateVersion, Name: name, Command: []string{"cmd.exe", "/c", "exit", "0"}}
		if err := b.writeConfig(config); err != nil {
			t.Fatal(err)
		}
		got, err := b.readConfig(name)
		if err != nil || got.Name != name {
			t.Fatalf("readConfig(%q) = %#v, %v", name, got, err)
		}
	}
	names, err := b.names()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"alpha", "zeta"}) {
		t.Fatalf("names = %v", names)
	}

	state := windowsServiceState{Version: windowsStateVersion, Name: "alpha", Status: "stopped", Restarts: 3, StartedAt: time.Now()}
	if err := b.writeState(state); err != nil {
		t.Fatal(err)
	}
	if got := b.readState("alpha"); got.Status != "stopped" || got.Restarts != 3 {
		t.Fatalf("state = %#v", got)
	}
	if got := b.readState("missing"); got.Name != "missing" || got.Status != "stopped" || got.Version != windowsStateVersion {
		t.Fatalf("default state = %#v", got)
	}

	if err := b.writeControl("alpha", "restart"); err != nil {
		t.Fatal(err)
	}
	control, ok := b.takeControl("alpha")
	if !ok || control.Action != "restart" || control.RequestedAt == 0 {
		t.Fatalf("control = %#v, %v", control, ok)
	}
	if _, ok := b.takeControl("alpha"); ok {
		t.Fatal("control request was not consumed")
	}
}

func TestWindowsRejectsInvalidPersistedConfig(t *testing.T) {
	b := &windowsBackend{root: localTempDir(t), stdout: io.Discard, stderr: io.Discard}
	for _, config := range []windowsServiceConfig{
		{Name: "api", Command: []string{"cmd.exe"}},
		{Version: windowsStateVersion, Name: "api"},
	} {
		if err := b.writeConfig(config); err != nil {
			t.Fatal(err)
		}
		if _, err := b.readConfig("api"); err == nil {
			t.Fatalf("invalid config unexpectedly accepted: %#v", config)
		}
	}
}

func TestWindowsRestartPolicies(t *testing.T) {
	failure := errors.New("process failed")
	tests := []struct {
		policy   string
		waitErr  error
		exitCode int
		want     bool
	}{
		{"always", nil, 0, true},
		{"on-success", nil, 0, true},
		{"on-success", failure, 1, false},
		{"on-failure", failure, 0, true},
		{"on-abnormal", nil, 1, true},
		{"no", failure, 1, false},
	}
	for _, test := range tests {
		if got := windowsShouldRestart(test.policy, test.waitErr, test.exitCode); got != test.want {
			t.Errorf("windowsShouldRestart(%q, %v, %d) = %v, want %v", test.policy, test.waitErr, test.exitCode, got, test.want)
		}
	}
}

func TestWindowsEnvironmentLoadsFileAndOverridesValues(t *testing.T) {
	directory := localTempDir(t)
	envFile := filepath.Join(directory, "service.env")
	if err := os.WriteFile(envFile, []byte("# comment\nFROM_FILE='yes'\nOVERRIDE=file\nINVALID\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := windowsEnvironment(windowsServiceConfig{EnvFile: envFile, Env: []string{"OVERRIDE=config", "DIRECT=yes"}})
	values := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[strings.ToUpper(key)] = value
		}
	}
	for key, want := range map[string]string{"FROM_FILE": "yes", "OVERRIDE": "config", "DIRECT": "yes"} {
		if values[key] != want {
			t.Errorf("%s = %q, want %q", key, values[key], want)
		}
	}
}

func TestWindowsSelectionStatusAndLogsValidation(t *testing.T) {
	var output bytes.Buffer
	b := &windowsBackend{root: localTempDir(t), stdout: &output, stderr: io.Discard}
	config := windowsServiceConfig{Version: windowsStateVersion, Name: "backup", Command: []string{"backup.exe"}, Mode: "schedule", Enabled: true, Schedule: "0 4 * * *", Timezone: "Asia/Singapore"}
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	if names, err := b.selectNames([]string{"backup"}, false); err != nil || !reflect.DeepEqual(names, []string{"backup"}) {
		t.Fatalf("selectNames = %v, %v", names, err)
	}
	for _, selectors := range [][]string{nil, {"backup", "all"}, {"missing"}} {
		if _, err := b.selectNames(selectors, false); err == nil {
			t.Errorf("selectors %v unexpectedly succeeded", selectors)
		}
	}
	if err := b.status("backup"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"status", "waiting", "Asia/Singapore", "backup.exe"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("status output missing %q:\n%s", want, output.String())
		}
	}
	if err := b.logs([]string{"backup", "--health"}); err == nil || !strings.Contains(err.Error(), "windows logs") {
		t.Fatalf("unsupported logs error = %v", err)
	}
	if err := b.status("missing"); err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("missing status error = %v", err)
	}
}

func TestWindowsEscapingAndAccountHelpers(t *testing.T) {
	if got := windowsQuoteCommand(`C:\Program Files\"spm".exe`); got != `"C:\Program Files\\"spm\".exe"` {
		t.Fatalf("windowsQuoteCommand() = %q", got)
	}
	if got := windowsXMLEscape(`<tag a="b">Tom & Jerry's</tag>`); got != `&lt;tag a=&quot;b&quot;&gt;Tom &amp; Jerry&apos;s&lt;/tag&gt;` {
		t.Fatalf("windowsXMLEscape() = %q", got)
	}
	if got := vbscriptString(`say "hello"`); got != `"say ""hello"""` {
		t.Fatalf("vbscriptString() = %q", got)
	}
	for _, account := range []string{"SYSTEM", `NT AUTHORITY\LOCALSERVICE`, `NT AUTHORITY\NETWORKSERVICE`} {
		if !windowsBuiltInAccount(account) || !windowsScheduledServiceAccount(account) {
			t.Errorf("built-in account %q was not recognized", account)
		}
	}
	if windowsBuiltInAccount(`DOMAIN\svc`) || windowsScheduledServiceAccount(`DOMAIN\svc`) {
		t.Fatal("domain account incorrectly recognized as built-in")
	}
}

func TestWindowsCommandDispatchAndHelp(t *testing.T) {
	root := localTempDir(t)
	t.Setenv("SPM_WINDOWS_DATA_DIR", root)
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	var output bytes.Buffer
	b := &windowsBackend{root: root, stdout: &output, stderr: io.Discard}

	if err := b.run(nil); err != nil || !strings.Contains(output.String(), "cross-platform manager") {
		t.Fatalf("empty command = %v, output %q", err, output.String())
	}
	output.Reset()
	if err := b.run([]string{"list", "--json"}); err != nil || strings.TrimSpace(output.String()) != "[]" {
		t.Fatalf("empty JSON list = %v, %q", err, output.String())
	}
	output.Reset()
	for _, command := range []string{"start", "schedule", "list", "logs", "run", "restart", "delete", "doctor", "completion", "version"} {
		if !printPlatformCommandHelp(&output, command) {
			t.Errorf("help for %q was not handled", command)
		}
	}
	if printPlatformCommandHelp(io.Discard, "missing") {
		t.Fatal("unknown command help unexpectedly handled")
	}
	if !printPlatformHelp(io.Discard) {
		t.Fatal("platform help was not handled")
	}

	for _, args := range [][]string{
		{"doctor", "extra"},
		{"startup", "extra"},
		{"native"},
		{"status"},
		{"cat", "missing"},
		{"signal"},
		{"unknown"},
	} {
		if err := b.run(args); err == nil {
			t.Errorf("command %v unexpectedly succeeded", args)
		}
	}
	if err := b.run([]string{"startup"}); err != nil {
		t.Fatal(err)
	}
	if err := b.run([]string{"doctor"}); err != nil {
		t.Fatal(err)
	}

	backend, err := newWindowsBackend(io.Discard, io.Discard, false)
	absoluteRoot, absoluteErr := filepath.Abs(root)
	if err != nil || absoluteErr != nil || backend.root != absoluteRoot {
		t.Fatalf("newWindowsBackend() = %#v, %v", backend, err)
	}
	if app, ok := platformCompletionApplication(io.Discard, false); !ok || app == nil {
		t.Fatal("platform completion application unavailable")
	}
	for _, args := range [][]string{
		{"__windows-schedule-daemon"},
		{"__windows-service", "--root", "relative", "api"},
		{"__windows-supervise", "--root", "relative", "api"},
		{"__windows-launch", "--root", "relative", "api"},
	} {
		if err := runWindows(args, io.Discard, io.Discard, false); err == nil {
			t.Errorf("internal invocation %v unexpectedly succeeded", args)
		}
	}
}

func TestWindowsDeclarationInspectionAndDryRun(t *testing.T) {
	root := localTempDir(t)
	path := filepath.Join(root, "spm.toml")
	data := []byte("[[services]]\nname = \"api\"\ncommand = [\"go\", \"version\"]\nrestart_sec = \"2s\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	b := &windowsBackend{root: root, stdout: &output, stderr: io.Discard}
	if err := b.apply([]string{"--dry-run", path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "create\tapi") {
		t.Fatalf("dry-run output = %q", output.String())
	}
	output.Reset()
	if err := b.inspectDeclaration("validate", []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "valid for Windows") {
		t.Fatalf("validation output = %q", output.String())
	}
	services, err := parseServiceDeclarationsForPlatform(path, "windows")
	if err != nil {
		t.Fatal(err)
	}
	desired, err := b.prepareDeclaration(services[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := b.writeConfig(desired); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := b.inspectDeclaration("diff", []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "unchanged\tapi") {
		t.Fatalf("unchanged diff output = %q", output.String())
	}
	desired.Description = "old description"
	if err := b.writeConfig(desired); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := b.inspectDeclaration("diff", []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "update\tapi") || !strings.Contains(output.String(), "-  config") || !strings.Contains(output.String(), "+  config") {
		t.Fatalf("updated diff output = %q", output.String())
	}
	for _, args := range [][]string{nil, {"--bad", path}, {path, path}} {
		if err := b.apply(args); err == nil {
			t.Errorf("apply(%v) unexpectedly succeeded", args)
		}
	}
	if err := b.inspectDeclaration("validate", nil); err == nil {
		t.Fatal("validate without path unexpectedly succeeded")
	}
}

func TestWindowsSupervisorAndWaitFailurePaths(t *testing.T) {
	root, err := filepath.Abs(localTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	b := &windowsBackend{root: root, stdout: io.Discard, stderr: io.Discard}
	config := windowsServiceConfig{Version: windowsStateVersion, Name: "once", Cwd: root, Command: []string{"cmd.exe", "/d", "/c", "exit", "0"}, Restart: "no", RestartSec: "0s"}
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := b.supervise("once"); err != nil {
		t.Fatal(err)
	}
	state := b.readState("once")
	if state.Status != "stopped" || state.ExitCode == nil || *state.ExitCode != 0 || state.SupervisorPID != 0 {
		t.Fatalf("completed supervisor state = %#v", state)
	}
	log, err := os.ReadFile(b.logPath("once"))
	if err != nil || !strings.Contains(string(log), "process exited with code 0") {
		t.Fatalf("supervisor log = %q, %v", log, err)
	}

	failed := windowsServiceState{Version: windowsStateVersion, Name: "failed", Status: "failed", Error: "boom"}
	if err := os.MkdirAll(b.serviceDir("failed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := b.writeState(failed); err != nil {
		t.Fatal(err)
	}
	if err := b.waitRunning("failed", time.Millisecond); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("waitRunning error = %v", err)
	}
	if err := b.waitSupervisor("failed", time.Millisecond); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("waitSupervisor error = %v", err)
	}
	if err := b.supervise("missing"); err == nil || !strings.Contains(err.Error(), "supervisor config") {
		t.Fatalf("missing supervisor error = %v", err)
	}
}

func TestWindowsManagedServiceAndLogPaths(t *testing.T) {
	root := localTempDir(t)
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	var output bytes.Buffer
	b := &windowsBackend{root: root, stdout: &output, stderr: io.Discard}
	config := windowsServiceConfig{Version: windowsStateVersion, Name: "api", Command: []string{"cmd.exe"}, Restart: "no"}
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := b.startManagedStack("api", map[string]bool{"api": true}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
	config.Requires = []string{"database"}
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := b.startManaged("api"); err == nil || !strings.Contains(err.Error(), "required service") {
		t.Fatalf("missing dependency error = %v", err)
	}
	config.Requires = nil
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := b.sendControl("api", "stop"); err != nil {
		t.Fatal(err)
	}
	if err := b.start([]string{"--name", "api", "--", "cmd.exe"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing start error = %v", err)
	}

	if err := os.WriteFile(b.logPath("api"), []byte("first\nmatch line\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.logs([]string{"api", "--lines", "2", "--grep", "match"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "match line") || strings.Contains(output.String(), "first") {
		t.Fatalf("filtered logs = %q", output.String())
	}
	output.Reset()
	if err := b.native([]string{"windows", "inspect", "api"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Configuration:") {
		t.Fatalf("native output = %q", output.String())
	}

	startup := b.startupPath("api")
	if err := os.MkdirAll(filepath.Dir(startup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(startup, []byte("shortcut"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.unregister("api"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(startup); !os.IsNotExist(err) {
		t.Fatalf("startup shortcut still exists: %v", err)
	}

	removable := filepath.Join(root, "remove-me")
	if err := os.MkdirAll(removable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeWindowsServiceData(removable); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsSchedulePathAndLauncherHelpers(t *testing.T) {
	root := localTempDir(t)
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	b := &windowsBackend{root: root, stdout: io.Discard, stderr: io.Discard}
	if !strings.HasSuffix(b.scheduleRecordPath("job"), filepath.Join("job", "schedule.json")) ||
		!strings.HasSuffix(b.scheduleStateDir("job"), filepath.Join("job", "schedule-state")) ||
		!strings.HasSuffix(b.scheduleLauncherPath("job", "timer"), filepath.Join("job", "schedule-timer.vbs")) {
		t.Fatal("unexpected schedule paths")
	}
	if b.scheduleTaskName("job", "timer") != "spm-job-schedule" || b.scheduleTaskName("job", "boot") != "spm-job-schedule-boot" {
		t.Fatal("unexpected scheduled task names")
	}
	if b.scheduleDaemonTaskName() == "" || !strings.HasSuffix(b.scheduleDaemonLauncherPath(), "scheduler.vbs") || !strings.HasSuffix(b.scheduleDaemonStartupPath(), "spm-schedule-daemon.lnk") {
		t.Fatal("unexpected scheduler daemon paths")
	}
	launcher := filepath.Join(root, "hidden.vbs")
	if err := writeWindowsHiddenLauncher(launcher, `C:\Program Files\spm.exe`, []string{"arg one", `a"b`}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(launcher)
	if err != nil || !strings.Contains(string(data), "shell.Run") || !strings.Contains(string(data), `arg one`) {
		t.Fatalf("hidden launcher = %q, %v", data, err)
	}
	principal, logonType, err := b.scheduledTaskPrincipal("")
	if err != nil || principal == "" || logonType != "InteractiveToken" {
		t.Fatalf("user principal = %q, %q, %v", principal, logonType, err)
	}
	system := &windowsBackend{system: true}
	if _, _, err := system.scheduledTaskPrincipal(`DOMAIN\svc`); err == nil {
		t.Fatal("unsupported system schedule account unexpectedly accepted")
	}
}

func TestWindowsSCMMissingServicePaths(t *testing.T) {
	b := &windowsBackend{root: localTempDir(t), stdout: io.Discard, stderr: io.Discard, system: true}
	name := "codex-definitely-missing-service"
	if b.scmInstalled(name) {
		t.Fatalf("unexpected SCM service %q exists", name)
	}
	if err := b.configureSCMStartType(name, true); err == nil {
		t.Fatalf("configure missing SCM service error = %v", err)
	}
	if err := b.startSCMService(name); err == nil {
		t.Fatalf("start missing SCM service error = %v", err)
	}
	_ = b.stopSCMService(name)
	_ = b.deleteSCMService(name)
	if err := b.stopLegacySystemTask(name); err != nil {
		t.Fatalf("stopping a missing legacy task should be idempotent: %v", err)
	}
	if err := b.inspectSCMService(name, io.Discard, io.Discard); err == nil {
		t.Fatal("inspecting a missing SCM service unexpectedly succeeded")
	}
	_ = b.checkSCM()
}

func TestWindowsProcessAndEmptySchedulerPaths(t *testing.T) {
	if windowsProcessAlive(0) || portableProcessAlive(-1) {
		t.Fatal("non-positive process ID reported alive")
	}
	if !windowsProcessAlive(os.Getpid()) || !portableProcessAlive(os.Getpid()) {
		t.Fatal("current process reported dead")
	}
	if windowsProcessAlive(1 << 30) {
		t.Fatal("impossible process ID reported alive")
	}
	windowsCloseHandle(0)
	windowsKillTree(0)
	if handle, err := windowsCreateKillJob(1 << 30); err == nil || handle != 0 {
		if handle != 0 {
			windowsCloseHandle(handle)
		}
		t.Fatalf("job for impossible process = %v, %v", handle, err)
	}

	root := localTempDir(t)
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	b := &windowsBackend{root: root, stdout: io.Discard, stderr: io.Discard}
	if err := b.refreshScheduleDaemon(); err != nil {
		t.Fatalf("refresh empty scheduler: %v", err)
	}
	if err := b.unregisterScheduledTask("missing"); err != nil {
		t.Fatalf("unregister missing scheduled task: %v", err)
	}
	if err := triggerWindowsScheduledTask("missing"); err == nil {
		t.Fatal("triggering missing scheduled task unexpectedly succeeded")
	}
}

func TestWindowsRegularServiceListStatusAndDeadState(t *testing.T) {
	root := localTempDir(t)
	var output bytes.Buffer
	b := &windowsBackend{root: root, stdout: &output, stderr: io.Discard}
	config := windowsServiceConfig{Version: windowsStateVersion, Name: "worker", Command: []string{"worker.exe", "serve"}, Cwd: root, Enabled: true}
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	dead := windowsServiceState{Version: windowsStateVersion, Name: "worker", Status: "running", PID: 1 << 30, SupervisorPID: 1 << 30, Restarts: 2, StartedAt: time.Now().Add(-time.Minute)}
	if err := b.writeState(dead); err != nil {
		t.Fatal(err)
	}
	state := b.readState("worker")
	if state.Status != "stopped" || state.PID != 0 || state.SupervisorPID != 0 {
		t.Fatalf("dead process state was not normalized: %#v", state)
	}
	if err := b.list(listOptions{json: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name": "worker"`, `"status": "stopped"`, `"restarts": 2`} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("JSON list missing %q: %s", want, output.String())
		}
	}
	output.Reset()
	if err := b.status("worker"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"status", "stopped", "worker.exe serve", "enabled"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("status missing %q: %s", want, output.String())
		}
	}

	for _, value := range []string{"bad\naccount", strings.Repeat("x", 257)} {
		if _, err := windowsRunAs(value, true); err == nil {
			t.Errorf("invalid account %q unexpectedly accepted", value)
		}
	}
}

func TestWindowsScheduledCommandBoundaries(t *testing.T) {
	original := windowsExecCommand
	t.Cleanup(func() { windowsExecCommand = original })
	var calls []string
	windowsExecCommand = func(name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return exec.Command("cmd.exe", "/d", "/c", "exit 0")
	}

	root, err := filepath.Abs(localTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	b := &windowsBackend{root: root, stdout: io.Discard, stderr: io.Discard}
	if err := os.MkdirAll(b.serviceDir("job"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := windowsServiceConfig{
		Version: windowsStateVersion, Name: "job", Command: []string{"worker.exe"}, Enabled: true,
		Schedule: "* * * * *", TimerOnBoot: "1min", RunAs: "SYSTEM",
	}
	if err := b.registerScheduledTask(config); err != nil {
		t.Fatal(err)
	}
	if err := triggerWindowsScheduledTask("job"); err != nil {
		t.Fatal(err)
	}
	if err := b.deleteScheduledTask(b.scheduleTaskName("job", "timer")); err != nil {
		t.Fatal(err)
	}
	if err := b.removeLegacySystemTask("job"); err != nil {
		t.Fatal(err)
	}
	if err := b.inspectSCMService("job", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	if err := b.schedule([]string{"--name", "nightly", "--interval", "1min", "--on-boot", "1min", "--run-now", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	saved, err := b.readConfig("nightly")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.setScheduledEnabled(saved, false); err != nil {
		t.Fatal(err)
	}
	if err := b.unregisterScheduledTask("nightly"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"schtasks.exe /Create", "schtasks.exe /Run", "schtasks.exe /Delete", "sc.exe queryex", "sc.exe qc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("command calls do not contain %q:\n%s", want, joined)
		}
	}

	windowsExecCommand = func(string, ...string) *exec.Cmd {
		return exec.Command("cmd.exe", "/d", "/c", "echo simulated failure & exit /b 1")
	}
	if err := triggerWindowsScheduledTask("job"); err == nil {
		t.Fatal("failed task trigger unexpectedly succeeded")
	}
	if err := b.createScheduledTask("job", "<Task/>"); err == nil {
		t.Fatal("failed task creation unexpectedly succeeded")
	}
	if err := b.inspectSCMService("job", io.Discard, io.Discard); err == nil {
		t.Fatal("failed SCM inspection unexpectedly succeeded")
	}
}
