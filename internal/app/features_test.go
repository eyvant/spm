package app

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestApplication(t *testing.T, runner *fakeRunner) (*application, *strings.Builder) {
	t.Helper()
	directory := localTempDir(t)
	var output strings.Builder
	return &application{runner: runner, stdout: &output, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config"), sleep: func(time.Duration) {}}, &output
}

func writeUnit(t *testing.T, a *application, filename string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.unitDir, filename), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func startInstances(t *testing.T, a *application, count string) {
	t.Helper()
	if err := a.start([]string{"--name", "api", "--cwd", a.unitDir, "--instances", count, "--env", "PORT=300{{instance}}", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
}

func TestParseLogsAcceptsMultipleNamesAndNone(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: nil, want: ""},
		{args: []string{"all", "-f"}, want: "all"},
		{args: []string{"api", "worker", "-n", "20"}, want: "api,worker"},
	} {
		options, err := parseLogs(test.args)
		if err != nil {
			t.Fatalf("parseLogs(%v): %v", test.args, err)
		}
		if got := strings.Join(options.names, ","); got != test.want {
			t.Errorf("parseLogs(%v) names = %q, want %q", test.args, got, test.want)
		}
	}
}

func TestLogsWithoutNameMergesAllServices(t *testing.T) {
	runner := &fakeRunner{}
	a, _ := newTestApplication(t, runner)
	writeUnit(t, a, unitName("worker"))
	startInstances(t, a, "2")
	runner.calls = nil
	if err := a.logs(nil); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	for _, want := range []string{"--unit spm-api@0.service", "--unit spm-api@1.service", "--unit spm-worker.service"} {
		if !strings.Contains(got, want) {
			t.Errorf("journalctl call does not contain %q: %s", want, got)
		}
	}
}

func TestNormalizeSignal(t *testing.T) {
	for input, want := range map[string]string{"usr2": "SIGUSR2", "SIGHUP": "SIGHUP", "Term": "SIGTERM", "9": "9", "RTMIN+1": "SIGRTMIN+1"} {
		got, err := normalizeSignal(input)
		if err != nil || got != want {
			t.Errorf("normalizeSignal(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "SIG", "0", "65", "USR 2", "usr2;rm"} {
		if _, err := normalizeSignal(input); err == nil {
			t.Errorf("normalizeSignal(%q) unexpectedly succeeded", input)
		}
	}
}

func TestSignalTargetsMainProcessOfEveryInstance(t *testing.T) {
	runner := &fakeRunner{}
	a, output := newTestApplication(t, runner)
	writeUnit(t, a, unitName("worker"))
	startInstances(t, a, "2")
	runner.calls = nil
	if err := a.signal([]string{"usr2", "all"}); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	for _, want := range []string{
		"systemctl --user kill --signal=SIGUSR2 --kill-whom=main spm-api@0.service spm-api@1.service",
		"systemctl --user kill --signal=SIGUSR2 --kill-whom=main spm-worker.service",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("calls do not contain %q:\n%s", want, got)
		}
	}
	if !strings.Contains(output.String(), "worker sent SIGUSR2") {
		t.Errorf("unexpected output %q", output.String())
	}
	runner.calls = nil
	if err := a.signal([]string{"HUP", "worker", "--all-processes"}); err != nil {
		t.Fatal(err)
	}
	if got := callsString(runner.calls); !strings.Contains(got, "--kill-whom=all spm-worker.service") {
		t.Errorf("--all-processes was not passed: %s", got)
	}
}

func TestStartInstancesInstallsTemplate(t *testing.T) {
	runner := &fakeRunner{}
	a, output := newTestApplication(t, runner)
	startInstances(t, a, "3")
	unit, err := os.ReadFile(a.templatePath("api"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`Environment="SPM_INSTANCE=%i" "NODE_APP_INSTANCE=%i"`, `Environment="PORT=300%i"`, "SyslogIdentifier=spm-api@%i", "Description=spm service: api instance %i"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("template does not contain %q:\n%s", want, unit)
		}
	}
	if _, err := os.Stat(a.unitPath("api")); !os.IsNotExist(err) {
		t.Fatalf("plain unit should not exist: %v", err)
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user enable --now spm-api@0.service spm-api@1.service spm-api@2.service") {
		t.Fatalf("instances were not enabled:\n%s", got)
	}
	config, err := a.readServiceConfig("api")
	if err != nil || config.Instances != 3 {
		t.Fatalf("saved instances = %d, %v", config.Instances, err)
	}
	names, _ := a.managedNames()
	if strings.Join(names, ",") != "api" {
		t.Fatalf("managedNames() = %v", names)
	}
	if !strings.Contains(output.String(), "with 3 instances") {
		t.Errorf("unexpected output %q", output.String())
	}
}

func TestInstancesRejectUnsupportedCombinations(t *testing.T) {
	a, _ := newTestApplication(t, &fakeRunner{})
	for _, args := range [][]string{
		{"--name", "api", "--cwd", a.unitDir, "--instances", "2", "--cron-restart", "0 * * * *", "--", "go", "version"},
		{"--name", "api", "--cwd", a.unitDir, "--instances", "2", "--health-tcp", "127.0.0.1:1", "--", "go", "version"},
		{"--name", "api", "--cwd", a.unitDir, "--instances", "2", "--type", "forking", "--pid-file", "/run/api.pid", "--", "go", "version"},
	} {
		if err := a.start(args); err == nil {
			t.Errorf("start(%v) unexpectedly succeeded", args)
		}
	}
}

func TestRollingRestartRestartsInstancesInOrder(t *testing.T) {
	runner := &fakeRunner{}
	a, _ := newTestApplication(t, runner)
	startInstances(t, a, "3")
	runner.calls = nil
	if err := a.control("restart", "api"); err != nil {
		t.Fatal(err)
	}
	var restarts []string
	for _, call := range runner.calls {
		if call.name == "systemctl" && len(call.args) == 3 && call.args[1] == "restart" {
			restarts = append(restarts, call.args[2])
		}
	}
	if strings.Join(restarts, ",") != "spm-api@0.service,spm-api@1.service,spm-api@2.service" {
		t.Fatalf("restart order = %v\n%s", restarts, callsString(runner.calls))
	}
}

func TestScaleStartsAndStopsOnlyChangedInstances(t *testing.T) {
	runner := &fakeRunner{}
	a, _ := newTestApplication(t, runner)
	startInstances(t, a, "2")
	runner.calls = nil
	if err := a.scale([]string{"api", "4"}); err != nil {
		t.Fatal(err)
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user enable --now spm-api@2.service spm-api@3.service") || strings.Contains(got, "spm-api@0.service") {
		t.Fatalf("scale up calls:\n%s", got)
	}
	runner.calls = nil
	if err := a.scale([]string{"api", "1"}); err != nil {
		t.Fatal(err)
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user disable --now spm-api@1.service spm-api@2.service spm-api@3.service") {
		t.Fatalf("scale down calls:\n%s", got)
	}
	if count := a.instanceCount("api"); count != 1 {
		t.Fatalf("instanceCount() = %d, want 1", count)
	}
	writeUnit(t, a, unitName("plain"))
	if err := a.scale([]string{"plain", "2"}); err == nil {
		t.Fatal("scaling a plain service unexpectedly succeeded")
	}
}

func TestForceReplacesTemplateWithPlainService(t *testing.T) {
	runner := &fakeRunner{}
	a, _ := newTestApplication(t, runner)
	startInstances(t, a, "2")
	runner.calls = nil
	if err := a.start([]string{"--name", "api", "--cwd", a.unitDir, "--force", "--instances", "0", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	if a.isTemplate("api") {
		t.Fatal("template unit still exists")
	}
	if _, err := os.Stat(a.unitPath("api")); err != nil {
		t.Fatalf("plain unit was not written: %v", err)
	}
	if got := callsString(runner.calls); !strings.Contains(got, "disable --now spm-api@0.service spm-api@1.service") {
		t.Fatalf("old instances were not stopped:\n%s", got)
	}
}

func TestListJSONIncludesInstancesAndCPU(t *testing.T) {
	samples := 0
	runner := &fakeRunner{}
	a, output := newTestApplication(t, runner)
	startInstances(t, a, "2")
	a.sleep = func(time.Duration) { time.Sleep(2 * time.Millisecond) }
	runner.runFunc = func(name string, args ...string) ([]byte, error) {
		if name != "systemctl" || len(args) < 2 || args[1] != "show" {
			return nil, nil
		}
		samples++
		cpu := "1000000"
		if samples > 1 {
			cpu = "251000000"
		}
		return []byte("Id=spm-api@0.service\nActiveState=active\nSubState=running\nMainPID=42\nMemoryCurrent=2048\nCPUUsageNSec=" + cpu + "\nNRestarts=1\nUnitFileState=enabled\n\nId=spm-api@1.service\nActiveState=failed\nSubState=failed\nMainPID=0\nMemoryCurrent=[not set]\nCPUUsageNSec=[not set]\nNRestarts=0\nUnitFileState=enabled\n"), nil
	}
	options, err := parseList([]string{"--json"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.listWith(options); err != nil {
		t.Fatal(err)
	}
	var rows []listRow
	if err := json.Unmarshal([]byte(output.String()[strings.Index(output.String(), "["):]), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, output.String())
	}
	if len(rows) != 2 || rows[0].Instance == nil || *rows[0].Instance != 0 || rows[0].PID != 42 || rows[0].MemoryBytes == nil || *rows[0].MemoryBytes != 2048 {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if rows[0].CPUPercent == nil || *rows[0].CPUPercent <= 0 {
		t.Fatalf("CPU percent was not computed: %+v", rows[0])
	}
	if rows[1].CPUPercent != nil || rows[1].MemoryBytes != nil || rows[1].Status != "failed" {
		t.Fatalf("failed instance row: %+v", rows[1])
	}
}

func TestListWatchRefreshesWithoutExtraSamples(t *testing.T) {
	runner := &fakeRunner{}
	a, output := newTestApplication(t, runner)
	writeUnit(t, a, unitName("api"))
	a.watchLimit = 2
	options, err := parseList([]string{"--watch=1s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.listWith(options); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(output.String(), "spm list, every 1s"); count != 2 {
		t.Fatalf("watch refreshed %d times:\n%s", count, output.String())
	}
	for _, args := range [][]string{{"--watch=10ms"}, {"--json", "--watch"}, {"--unknown"}} {
		if _, err := parseList(args); err == nil {
			t.Errorf("parseList(%v) unexpectedly succeeded", args)
		}
	}
}

func TestCPUPercent(t *testing.T) {
	start := time.Now()
	previous := &cpuSample{at: start, usage: map[string]uint64{"a": 0}}
	current := &cpuSample{at: start.Add(time.Second), usage: map[string]uint64{"a": uint64(1500 * time.Millisecond)}}
	got := cpuPercent(previous, current, "a", serviceState{active: "active"})
	if got == nil || *got != 150 {
		t.Fatalf("cpuPercent() = %v, want 150", got)
	}
	if cpuPercent(previous, current, "a", serviceState{active: "inactive"}) != nil {
		t.Fatal("inactive unit should not report CPU")
	}
}

func TestRenderUnitServiceTypes(t *testing.T) {
	options := defaultStartOptions()
	options.name, options.cwd, options.command = "api", "/srv/api", []string{"/usr/sbin/daemon"}
	options.serviceType, options.pidFile = "forking", "/run/api.pid"
	unit, err := renderUnit(options, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "Type=forking\nPIDFile=/run/api.pid\n") {
		t.Fatalf("unexpected unit:\n%s", unit)
	}
	options.instances, options.pidFile = 2, "/run/api-{{instance}}.pid"
	unit, err = renderUnit(options, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "PIDFile=/run/api-%i.pid\n") {
		t.Fatalf("instance pid file was not expanded:\n%s", unit)
	}
}

func TestValidateRuntimeOptions(t *testing.T) {
	for _, mutate := range []func(*startOptions){
		func(o *startOptions) { o.serviceType = "oneshot" },
		func(o *startOptions) { o.pidFile = "/run/a.pid" },
		func(o *startOptions) { o.serviceType, o.pidFile = "forking", "run/a.pid" },
		func(o *startOptions) { o.timezone = "../etc/passwd" },
		func(o *startOptions) { o.timezone = "Asia/Singapore;" },
		func(o *startOptions) { o.properties = []string{"RestartSteps=5"} },
	} {
		options := defaultStartOptions()
		mutate(&options)
		if err := options.validateRuntimeOptions(); err == nil {
			t.Errorf("validateRuntimeOptions(%+v) unexpectedly succeeded", options)
		}
	}
	options := defaultStartOptions()
	options.serviceType, options.timezone = "notify", "Asia/Singapore"
	options.properties = []string{"RestartMaxDelaySec=1min", "RestartSteps=5"}
	if err := options.validateRuntimeOptions(); err != nil {
		t.Fatal(err)
	}
}

func TestRestartBackoffAndTimezoneFlags(t *testing.T) {
	addFakeExecutableToPath(t, "systemctl")
	runner := &fakeRunner{}
	a, _ := newTestApplication(t, runner)
	if err := a.start([]string{"--name", "api", "--cwd", a.unitDir, "--restart-max-delay", "2min", "--restart-steps", "8", "--cron-restart", "0 4 * * *", "--timezone", "Asia/Singapore", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(a.unitPath("api"))
	if !strings.Contains(string(unit), "RestartMaxDelaySec=2min\nRestartSteps=8\n") {
		t.Errorf("backoff properties missing:\n%s", unit)
	}
	timer, _ := os.ReadFile(a.cronRestartTimerPath("api"))
	if !strings.Contains(string(timer), "OnCalendar=*-*-* 04:00:00 Asia/Singapore\n") || !strings.Contains(string(timer), "# Timezone=Asia/Singapore\n") {
		t.Errorf("cron restart timer does not use the time zone:\n%s", timer)
	}
	if got := a.readSchedule("api"); got != "restart: 0 4 * * * Asia/Singapore" {
		t.Errorf("readSchedule() = %q", got)
	}
	if err := a.start([]string{"--name", "other", "--cwd", a.unitDir, "--timezone", "UTC", "--", "go", "version"}); err == nil {
		t.Error("--timezone without --cron-restart unexpectedly succeeded")
	}
	options, err := parseStart([]string{"--restart-max-delay", "1min", "--", "go", "version"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasProperty(options.properties, "RestartSteps") {
		t.Errorf("a lone --restart-max-delay should get default steps: %v", options.properties)
	}
}

func TestScheduleTimezone(t *testing.T) {
	options, err := parseSchedule([]string{"--cron", "30 9 * * 1-5", "--timezone", "Asia/Singapore", "--", "go", "version"})
	if err != nil {
		t.Fatal(err)
	}
	if options.calendar != "Mon..Fri *-*-* 09:30:00 Asia/Singapore" {
		t.Fatalf("calendar = %q", options.calendar)
	}
	timer := renderScheduleTimer("job", persistedStartConfig{Schedule: "30 9 * * 1-5", Timezone: "Asia/Singapore"}, options.calendar)
	if !strings.Contains(timer, "# CronExpression=30 9 * * 1-5\n# Timezone=Asia/Singapore\n") {
		t.Fatalf("timer comments:\n%s", timer)
	}
	for _, args := range [][]string{
		{"--interval", "5min", "--timezone", "UTC", "--", "go", "version"},
		{"--cron", "0 * * * *", "--instances", "2", "--", "go", "version"},
		{"--cron", "0 * * * *", "--type", "notify", "--", "go", "version"},
	} {
		if _, err := parseSchedule(args); err == nil {
			t.Errorf("parseSchedule(%v) unexpectedly succeeded", args)
		}
	}
}

func TestDeclarationsSupportNewKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spm.toml")
	content := "[[services]]\nname = \"api\"\ncommand = [\"go\", \"version\"]\ninstances = 3\ntype = \"notify\"\n\n[[services]]\nname = \"job\"\ncommand = [\"go\", \"version\"]\nschedule = \"0 9 * * *\"\ntimezone = \"Asia/Singapore\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	services, err := parseServiceDeclarations(path)
	if err != nil {
		t.Fatal(err)
	}
	if services[0].Instances != 3 || services[0].Type != "notify" || services[1].Timezone != "Asia/Singapore" {
		t.Fatalf("unexpected declarations: %+v", services)
	}
	args := strings.Join(services[0].startArgs(true, false), " ")
	for _, want := range []string{"--type  ", "--instances 0", "--type notify", "--instances 3"} {
		if !strings.Contains(args, want) {
			t.Errorf("start args %q do not contain %q", args, want)
		}
	}
	var output strings.Builder
	writeDeclaration(&output, "api", persistedStartConfig{Command: []string{"/usr/bin/node"}, Type: "notify", Timezone: "UTC", Instances: 3})
	for _, want := range []string{"type = \"notify\"", "timezone = \"UTC\"", "instances = 3"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("export does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestDoctorReportsChecks(t *testing.T) {
	runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "systemctl" && len(args) > 0 && args[len(args)-1] == "is-system-running":
			return []byte("degraded\n"), nil
		case name == "systemctl" && len(args) == 1 && args[0] == "--version":
			return []byte("systemd 249 (249.11)\n+PAM\n"), nil
		case name == "loginctl":
			return []byte("no\n"), nil
		}
		return nil, nil
	}}
	a, output := newTestApplication(t, runner)
	if err := a.doctor(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WARN  user manager: degraded", "WARN  systemd version: 249", "unit directory", "journal access"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("doctor output does not contain %q:\n%s", want, output.String())
		}
	}
	if parseSystemdVersion("systemd 255 (255.4-1ubuntu8)") != 255 {
		t.Fatal("parseSystemdVersion failed")
	}
}

func TestCompletionIncludesNewCommands(t *testing.T) {
	a, _ := newTestApplication(t, &fakeRunner{})
	startInstances(t, a, "2")
	writeUnit(t, a, unitName("plain"))
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"sc"}, want: "scale"},
		{args: []string{"doc"}, want: "doctor"},
		{args: []string{"signal", "SIGU"}, want: "SIGUSR2"},
		{args: []string{"start", "--type", "no"}, want: "notify"},
		{args: []string{"list", "--j"}, want: "--json"},
	} {
		got := strings.Join(a.completionCandidates(test.args), " ")
		if !strings.Contains(got, test.want) {
			t.Errorf("completionCandidates(%v) = %q, want %q", test.args, got, test.want)
		}
	}
	if got := a.completionCandidates([]string{"scale", ""}); strings.Join(got, ",") != "api" {
		t.Errorf("scale completion = %v, want only template services", got)
	}
}
