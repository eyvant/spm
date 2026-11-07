package app

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateTimerDeclarationBranches(t *testing.T) {
	tests := []struct {
		name    string
		service serviceDeclaration
		wantErr string
	}{
		{name: "empty", service: serviceDeclaration{}},
		{name: "valid schedule", service: serviceDeclaration{Schedule: "*/5 * * * *", Persistent: true, Timezone: "UTC", TimerRandomDelay: "30s"}},
		{name: "valid interval", service: serviceDeclaration{TimerInterval: "5min", TimerOnBoot: "1min"}},
		{name: "bad schedule", service: serviceDeclaration{Schedule: "not a cron"}, wantErr: "invalid schedule"},
		{name: "bad systemd duration", service: serviceDeclaration{TimerOnBoot: "soon"}, wantErr: "invalid timer_on_boot duration"},
		{name: "short interval", service: serviceDeclaration{TimerInterval: "30s"}, wantErr: "at least 1min"},
		{name: "persistent without schedule", service: serviceDeclaration{Persistent: true, TimerInterval: "5min"}, wantErr: "persistent requires schedule"},
		{name: "timezone without schedule", service: serviceDeclaration{Timezone: "UTC", TimerInterval: "5min"}, wantErr: "timezone requires schedule"},
		{name: "bad timezone", service: serviceDeclaration{Schedule: "0 * * * *", Timezone: "Nowhere/Invalid"}, wantErr: ""},
		{name: "random delay alone", service: serviceDeclaration{TimerRandomDelay: "30s"}, wantErr: "timer_randomized_delay requires"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTimerDeclaration(test.service)
			switch {
			case test.name == "bad timezone":
				if err == nil {
					t.Fatal("invalid timezone unexpectedly accepted")
				}
			case test.wantErr == "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			case err == nil || !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestInterpreterProgramIndex(t *testing.T) {
	tests := []struct {
		command []string
		index   int
		program bool
	}{
		{command: []string{"node", "server.js"}, index: 1, program: true},
		{command: []string{"node", "--", "server.js"}, index: 2, program: true},
		{command: []string{"node", "--"}, index: -1},
		{command: []string{"node", "--max-old-space-size=512", "app.js"}, index: 2, program: true},
		{command: []string{"node", "-r", "dotenv/config", "app.js"}, index: 3, program: true},
		{command: []string{"nodejs", "--import", "tsx", "app.ts"}, index: 3, program: true},
		{command: []string{"node", "-e", "console.log(1)"}, index: -1},
		{command: []string{"node", "--print", "1"}, index: -1},
		{command: []string{"python3", "-m", "http.server"}, index: 2},
		{command: []string{"python", "-m"}, index: -1},
		{command: []string{"python", "-c", "print(1)"}, index: -1},
		{command: []string{"python", "-W", "ignore", "-u", "main.py"}, index: 4, program: true},
		{command: []string{"python3", "-X", "dev", "-"}, index: 3, program: true},
		{command: []string{"ruby", "-I", "lib", "-r", "json", "app.rb"}, index: 5, program: true},
		{command: []string{"ruby", "-e", "puts 1"}, index: -1},
		{command: []string{"bash", "-c", "echo hi"}, index: -1},
		{command: []string{"sh", "-x", "script.sh"}, index: 2, program: true},
		{command: []string{"deno", "run", "--allow-net", "main.ts"}, index: 3, program: true},
		{command: []string{"bun", "task", "dev"}, index: 2, program: true},
		{command: []string{"node", "--inspect"}, index: -1},
	}
	for _, test := range tests {
		index, program := interpreterProgramIndex(test.command, 0)
		if index != test.index || program != test.program {
			t.Errorf("interpreterProgramIndex(%q) = %d, %t; want %d, %t", test.command, index, program, test.index, test.program)
		}
	}
	for _, test := range []struct {
		command []string
		want    string
	}{
		{command: []string{"node", "-e", "1"}, want: "node"},
		{command: []string{"python3", "-W", "ignore", "worker.py"}, want: "worker"},
	} {
		if got := inferName(test.command, true); got != test.want {
			t.Errorf("inferName(%q) = %q, want %q", test.command, got, test.want)
		}
	}
}

func TestWriteFileAtomicReplacesAndReportsErrors(t *testing.T) {
	directory := localTempDir(t)
	path := filepath.Join(directory, "file.txt")
	for _, content := range []string{"first", "second"} {
		if err := writeFileAtomic(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != content {
			t.Fatalf("content = %q, %v; want %q", data, err, content)
		}
	}
	if err := writeFileAtomic(filepath.Join(directory, "missing", "file.txt"), []byte("x"), 0o600); err == nil {
		t.Fatal("write into missing directory unexpectedly succeeded")
	}
	blocked := filepath.Join(directory, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(blocked, []byte("x"), 0o600); err == nil {
		t.Fatal("replacing a non-empty directory unexpectedly succeeded")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".spm-unit-") {
			t.Fatalf("temporary file %s was left behind", entry.Name())
		}
	}
}

func TestInspectDeclarationValidatesScheduledServices(t *testing.T) {
	directory := localTempDir(t)
	path := filepath.Join(directory, "spm.toml")
	var output strings.Builder
	a := &application{runner: &fakeRunner{}, stdout: &output, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	write := func(extra string) {
		t.Helper()
		data := "[[services]]\nname = \"job\"\ncommand = [\"go\", \"version\"]\n" + extra
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("schedule = \"*/10 * * * *\"\npersistent = true\n")
	if err := a.inspectDeclaration("validate", []string{path}); err != nil {
		t.Fatal(err)
	}
	options, config, err := a.prepareDeclaration(serviceDeclaration{Name: "job", Command: []string{"go", "version"}, Schedule: "*/10 * * * *", Persistent: true, TimerRandomDelay: "30s"})
	if err != nil {
		t.Fatal(err)
	}
	if options.restart != "no" || config.Mode != "schedule" || !config.Persistent || config.TimerRandomDelay != "30s" {
		t.Fatalf("scheduled declaration = restart %q, config %+v", options.restart, config)
	}
	if _, _, err := a.prepareDeclaration(serviceDeclaration{Name: "job", Command: []string{"go", "version"}, TimerInterval: "30s"}); err == nil {
		t.Fatal("short portable interval unexpectedly accepted")
	}
	if _, _, err := a.prepareDeclaration(serviceDeclaration{Name: "job", Command: []string{"go", "version"}, Schedule: "0 * * * *", HealthCommand: "go version"}); err == nil {
		t.Fatal("scheduled task with health check unexpectedly accepted")
	}
	if _, _, err := a.prepareDeclaration(serviceDeclaration{Name: "job"}); err == nil {
		t.Fatal("declaration without command unexpectedly accepted")
	}
}

func TestRemoveCompanionUnitsAndFailures(t *testing.T) {
	newApp := func(t *testing.T, fail string) (*application, *fakeRunner) {
		t.Helper()
		directory := localTempDir(t)
		runner := &fakeRunner{}
		if fail != "" {
			runner.runFunc = func(name string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), fail) {
					return nil, errors.New("boom")
				}
				return nil, nil
			}
		}
		a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
		return a, runner
	}
	writeFiles := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, path := range paths {
			if err := os.WriteFile(path, []byte("unit"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	a, runner := newApp(t, "")
	writeFiles(t, a.unitPath("api"), a.cronRestartTimerPath("api"), a.cronRestartServicePath("api"), a.scheduleTimerPath("api"))
	if err := a.removeMany([]string{"api", "--yes"}); err != nil {
		t.Fatal(err)
	}
	calls := callsString(runner.calls)
	for _, want := range []string{"disable --now " + cronRestartTimerName("api"), "disable --now " + scheduleTimerName("api"), cronRestartServiceName("api")} {
		if !strings.Contains(calls, want) {
			t.Errorf("remove calls missing %q:\n%s", want, calls)
		}
	}
	for _, path := range []string{a.unitPath("api"), a.cronRestartTimerPath("api"), a.scheduleTimerPath("api")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists: %v", path, err)
		}
	}
	if err := a.removeMany([]string{"all", "-y"}); err != nil {
		t.Fatalf("removeMany with no services: %v", err)
	}
	if err := a.remove("api"); err == nil {
		t.Fatal("removing an unmanaged service unexpectedly succeeded")
	}

	a, runner = newApp(t, "")
	writeFiles(t, a.templatePath("web"))
	if err := a.remove("web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.templatePath("web")); !os.IsNotExist(err) {
		t.Fatalf("template still exists: %v", err)
	}
	if !strings.Contains(callsString(runner.calls), "daemon-reload") {
		t.Fatalf("template remove did not reload: %s", callsString(runner.calls))
	}

	for _, fail := range []string{cronRestartTimerName("api"), scheduleTimerName("api"), healthTimerName("api"), "disable --now " + unitName("api"), "daemon-reload"} {
		a, _ := newApp(t, fail)
		writeFiles(t, a.unitPath("api"), a.cronRestartTimerPath("api"), a.scheduleTimerPath("api"), a.healthPath("api", ".health.timer"))
		if err := a.removeMany([]string{"api"}); err == nil {
			t.Errorf("remove with failing %q unexpectedly succeeded", fail)
		}
	}
	for _, fail := range []string{"disable --now", "daemon-reload"} {
		a, _ := newApp(t, fail)
		writeFiles(t, a.templatePath("web"))
		if err := a.remove("web"); err == nil {
			t.Errorf("template remove with failing %q unexpectedly succeeded", fail)
		}
	}
}

func TestUpdateFromSavedConfigAndDescribe(t *testing.T) {
	runner := &fakeRunner{}
	a, output := newTestApplication(t, runner)
	if err := a.updateFromSavedConfig("svc"); err == nil {
		t.Fatal("updating an unmanaged service unexpectedly succeeded")
	}
	if err := a.start([]string{"--name", "svc", "--cwd", a.unitDir, "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	if err := a.updateFromSavedConfig("svc"); err != nil {
		t.Fatalf("update from saved config: %v", err)
	}
	output.Reset()
	if err := a.describe("svc"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "unit file") || !strings.Contains(output.String(), "config file") {
		t.Fatalf("describe output = %q", output.String())
	}
	if err := a.describe("missing"); err == nil {
		t.Fatal("describing an unmanaged service unexpectedly succeeded")
	}

	if err := a.schedule([]string{"--name", "backup", "--cron", "*/30 * * * *", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	if err := a.updateFromSavedConfig("backup"); err != nil {
		t.Fatalf("update scheduled service: %v", err)
	}
	output.Reset()
	if err := a.describe("backup"); err != nil {
		t.Fatal(err)
	}

	startInstances(t, a, "2")
	output.Reset()
	if err := a.describe("api"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "instances") {
		t.Fatalf("describe instances output = %q", output.String())
	}

	// A saved config whose working directory vanished must be rejected.
	config, err := a.readServiceConfig("backup")
	if err != nil {
		t.Fatal(err)
	}
	config.Cwd = filepath.Join(a.unitDir, "gone")
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.serviceConfigPath("backup"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.updateFromSavedConfig("backup"); err == nil || !strings.Contains(err.Error(), "validate saved service config") {
		t.Fatalf("invalid saved config error = %v", err)
	}
	if err := os.WriteFile(a.serviceConfigPath("backup"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.updateFromSavedConfig("backup"); err == nil {
		t.Fatal("corrupt saved config unexpectedly accepted")
	}
	if err := os.Remove(a.serviceConfigPath("backup")); err != nil {
		t.Fatal(err)
	}
	if err := a.updateFromSavedConfig("backup"); err == nil || !strings.Contains(err.Error(), "saved spm configuration is missing") {
		t.Fatalf("missing saved config error = %v", err)
	}
}

func TestPortableScheduleRecordValidationAndPersistence(t *testing.T) {
	valid := portableScheduleRecord{Version: portableScheduleVersion, Name: "job", Command: []string{"go", "version"}, Schedule: "0 * * * *"}
	for name, record := range map[string]portableScheduleRecord{
		"missing name":            {Version: portableScheduleVersion, Command: []string{"go"}, Schedule: "0 * * * *"},
		"wrong version":           {Version: 99, Name: "job", Command: []string{"go"}, Schedule: "0 * * * *"},
		"no trigger":              {Version: portableScheduleVersion, Name: "job", Command: []string{"go"}},
		"bad cron":                {Version: portableScheduleVersion, Name: "job", Command: []string{"go"}, Schedule: "bad"},
		"bad timezone":            {Version: portableScheduleVersion, Name: "job", Command: []string{"go"}, Schedule: "0 * * * *", Timezone: "Nowhere/Invalid"},
		"timezone without cron":   {Version: portableScheduleVersion, Name: "job", Command: []string{"go"}, TimerInterval: "5m", Timezone: "UTC"},
		"bad duration":            {Version: portableScheduleVersion, Name: "job", Command: []string{"go"}, TimerOnBoot: "soon"},
		"sub-second interval":     {Version: portableScheduleVersion, Name: "job", Command: []string{"go"}, TimerInterval: "10ms"},
		"persistent without cron": {Version: portableScheduleVersion, Name: "job", Command: []string{"go"}, TimerInterval: "5m", Persistent: true},
	} {
		if err := validatePortableSchedule(record); err == nil {
			t.Errorf("%s: record unexpectedly valid", name)
		}
	}
	if err := validatePortableSchedule(valid); err != nil {
		t.Fatal(err)
	}

	directory := localTempDir(t)
	path := filepath.Join(directory, "nested", "job.json")
	stateDir := filepath.Join(directory, "state")
	if err := preparePortableScheduleUpdate(path, stateDir, &valid); err != nil {
		t.Fatalf("prepare without previous record: %v", err)
	}
	record := valid
	record.Version = 0
	if err := writePortableSchedule(path, record); err != nil {
		t.Fatal(err)
	}
	if err := writePortableSchedule(path, portableScheduleRecord{Name: "job"}); err == nil {
		t.Fatal("invalid record unexpectedly written")
	}
	saved, err := readPortableSchedule(path)
	if err != nil || saved.CreatedAt.IsZero() || saved.Version != portableScheduleVersion {
		t.Fatalf("saved record = %+v, %v", saved, err)
	}
	same := valid
	if err := preparePortableScheduleUpdate(path, stateDir, &same); err != nil || !same.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("unchanged schedule did not keep CreatedAt: %+v, %v", same, err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	changed := valid
	changed.Schedule = "30 * * * *"
	if err := preparePortableScheduleUpdate(path, stateDir, &changed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("changed schedule kept state directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparePortableScheduleUpdate(path, stateDir, &changed); err == nil {
		t.Fatal("corrupt previous record unexpectedly accepted")
	}

	statePath := filepath.Join(directory, "state2", "state.json")
	code := 3
	if err := writePortableScheduleState(statePath, portableScheduleState{Version: portableScheduleVersion, LastExitCode: &code}); err != nil {
		t.Fatal(err)
	}
	if state := readPortableScheduleState(statePath); state.LastExitCode == nil || *state.LastExitCode != 3 {
		t.Fatalf("state = %+v", state)
	}
	if err := os.WriteFile(statePath, []byte(`{"version": 42}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := readPortableScheduleState(statePath); state.LastExitCode != nil {
		t.Fatalf("mismatched state version was not reset: %+v", state)
	}
	if scheduleExitCode(nil) != "-" || scheduleExitCode(&code) != "3" {
		t.Fatal("scheduleExitCode formatting is incorrect")
	}
	if formatScheduleTime(time.Time{}) != "-" || formatScheduleTime(time.Unix(0, 0).UTC()) != "1970-01-01T00:00:00Z" {
		t.Fatal("formatScheduleTime formatting is incorrect")
	}

	due := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	valid.Timezone = "UTC"
	if key, err := portableOccurrenceKey(valid, due); err != nil || key != "cron:2026-01-02T03:04" {
		t.Fatalf("cron key = %q, %v", key, err)
	}
	seconds := valid
	seconds.Schedule = "*/5 * * * * *"
	if key, err := portableOccurrenceKey(seconds, due); err != nil || key != "cron:2026-01-02T03:04:05" {
		t.Fatalf("seconds cron key = %q, %v", key, err)
	}
	if key, err := portableOccurrenceKey(portableScheduleRecord{TimerInterval: "5m"}, due); err != nil || !strings.HasPrefix(key, "timer:") {
		t.Fatalf("timer key = %q, %v", key, err)
	}
	for _, record := range []portableScheduleRecord{{Schedule: "0 * * * *", Timezone: "Nowhere/Invalid"}, {Schedule: "bad"}} {
		if _, err := portableOccurrenceKey(record, due); err == nil {
			t.Errorf("portableOccurrenceKey(%+v) unexpectedly succeeded", record)
		}
	}
}

func TestSplitSystemdCommand(t *testing.T) {
	got, err := splitSystemdCommand(`/usr/bin/app "two words" a\ b \x41\tz`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/app", "two words", "a b", "Atz"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("split = %q, want %q", got, want)
	}
	for _, value := range []string{"", "   ", `"open`, `trail\`, `-/usr/bin/app`, `/usr/bin/app %i`, `app \xZZ`} {
		if _, err := splitSystemdCommand(value); err == nil {
			t.Errorf("splitSystemdCommand(%q) unexpectedly succeeded", value)
		}
	}
}

func TestHealthStatusBranches(t *testing.T) {
	runner := &fakeRunner{}
	a, output := newTestApplication(t, runner)
	if err := a.healthStatus("api"); err == nil {
		t.Fatal("health status for unmanaged service unexpectedly succeeded")
	}
	writeUnit(t, a, unitName("api"))
	if err := a.healthStatus("api"); err != nil || !strings.Contains(output.String(), "disabled") {
		t.Fatalf("disabled health = %q, %v", output.String(), err)
	}
	for _, suffix := range []string{".health.timer", ".health-recover.service"} {
		if err := os.WriteFile(a.healthPath("api", suffix), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.healthStatus("api"); err != nil {
		t.Fatal(err)
	}
	if calls := callsString(runner.calls); !strings.Contains(calls, healthRecoveryName("api")) {
		t.Fatalf("health status did not include recovery unit: %s", calls)
	}
	runner.interactiveErr = errors.New("boom")
	if err := a.healthStatus("api"); err == nil {
		t.Fatal("health status error was swallowed")
	}
}

func TestControlActionsAcrossServiceKinds(t *testing.T) {
	actions := []string{"start", "stop", "restart", "enable", "disable", "reload"}
	setup := func(t *testing.T, kind string, fail string) *application {
		t.Helper()
		directory := localTempDir(t)
		runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
			if fail != "" && strings.Contains(strings.Join(args, " "), fail) {
				return nil, errors.New("boom")
			}
			return nil, nil
		}}
		a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config"), sleep: func(time.Duration) {}}
		paths := []string{}
		switch kind {
		case "schedule":
			paths = append(paths, a.unitPath("svc"), a.scheduleTimerPath("svc"))
		case "companions":
			paths = append(paths, a.unitPath("svc"), a.cronRestartTimerPath("svc"), a.healthPath("svc", ".health.timer"))
		case "template":
			paths = append(paths, a.templatePath("svc"))
		}
		for _, path := range paths {
			if err := os.WriteFile(path, []byte("unit"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	for _, kind := range []string{"schedule", "companions", "template"} {
		for _, action := range actions {
			a := setup(t, kind, "")
			err := a.control(action, "svc")
			if kind == "schedule" && action == "reload" {
				if err == nil {
					t.Errorf("%s %s unexpectedly succeeded", kind, action)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %s: %v", kind, action, err)
			}
		}
	}
	failures := map[string][]string{
		"schedule":   {scheduleTimerName("svc"), unitName("svc")},
		"companions": {healthTimerName("svc"), cronRestartTimerName("svc"), unitName("svc")},
		"template":   {"svc"},
	}
	for kind, targets := range failures {
		for _, target := range targets {
			for _, action := range actions {
				if kind == "schedule" && action == "reload" {
					continue
				}
				_ = setup(t, kind, target).control(action, "svc")
			}
		}
	}
	for _, action := range []string{"stop", "start", "restart"} {
		if err := setup(t, "schedule", unitName("svc")).control(action, "svc"); err == nil {
			t.Errorf("schedule %s with failing unit unexpectedly succeeded", action)
		}
	}
	for _, action := range []string{"stop", "disable", "restart", "start", "enable"} {
		if err := setup(t, "companions", healthTimerName("svc")).control(action, "svc"); err == nil {
			t.Errorf("companions %s with failing health timer unexpectedly succeeded", action)
		}
		if err := setup(t, "companions", cronRestartTimerName("svc")).control(action, "svc"); err == nil {
			t.Errorf("companions %s with failing cron timer unexpectedly succeeded", action)
		}
	}
	if err := setup(t, "", "").control("start", "svc"); err == nil {
		t.Fatal("controlling an unmanaged service unexpectedly succeeded")
	}
}

func TestPrepareRejectsInvalidOptions(t *testing.T) {
	directory := localTempDir(t)
	envFile := filepath.Join(directory, "app.env")
	if err := os.WriteFile(envFile, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := func(t *testing.T) startOptions {
		t.Helper()
		options, err := parseStart([]string{"--name", "api", "--cwd", directory, "--", "go", "version"})
		if err != nil {
			t.Fatal(err)
		}
		return options
	}
	tests := map[string]func(*startOptions){
		"no command":           func(o *startOptions) { o.command = nil },
		"restart":              func(o *startOptions) { o.restart = "sometimes" },
		"restart sec":          func(o *startOptions) { o.restartSec = "soon" },
		"start limit interval": func(o *startOptions) { o.startLimitInterval = "soon" },
		"start limit burst":    func(o *startOptions) { o.startLimitBurst = "0" },
		"start limit text":     func(o *startOptions) { o.startLimitBurst = "many" },
		"memory":               func(o *startOptions) { o.maxMemory = "lots" },
		"property":             func(o *startOptions) { o.properties = []string{"not a property"} },
		"cron restart":         func(o *startOptions) { o.cronRestart = "bad cron" },
		"environment":          func(o *startOptions) { o.env = []string{"=missing"} },
		"missing cwd":          func(o *startOptions) { o.cwd = filepath.Join(directory, "missing") },
		"cwd is file":          func(o *startOptions) { o.cwd = envFile },
		"whitespace env file":  func(o *startOptions) { o.envFile = " app.env" },
		"missing env file":     func(o *startOptions) { o.envFile = "missing.env" },
		"interpreter explicit": func(o *startOptions) { o.interpreter = "node" },
		"missing executable":   func(o *startOptions) { o.command = []string{"spm-command-that-does-not-exist"} },
	}
	for name, mutate := range tests {
		options := base(t)
		mutate(&options)
		if err := options.prepare(); err == nil {
			t.Errorf("%s: prepare unexpectedly succeeded", name)
		}
	}
	options := base(t)
	options.envFile = "app.env"
	options.cronRestart = "0 3 * * *"
	options.timezone = "UTC"
	options.startLimitInterval = "10min"
	options.startLimitBurst = "5"
	options.maxMemory = "512M"
	if err := options.prepare(); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(options.envFile) || options.cronCalendar == "" {
		t.Fatalf("prepared options = envFile %q, calendar %q", options.envFile, options.cronCalendar)
	}
}

func TestInstallInstancesReplacementAndRollback(t *testing.T) {
	newApp := func(t *testing.T) (*application, *string) {
		t.Helper()
		fail := new(string)
		runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if *fail != "" && strings.Contains(joined, *fail) {
				return nil, errors.New("boom")
			}
			if len(args) > 0 && args[len(args)-1] == "--value" || strings.Contains(joined, "show") {
				return []byte("active\nrunning\n0\n"), nil
			}
			return nil, nil
		}}
		a, _ := newTestApplication(t, runner)
		return a, fail
	}
	instanceArgs := func(a *application, count string, extra ...string) []string {
		args := append([]string{"--name", "api", "--cwd", a.unitDir, "--instances", count}, extra...)
		return append(args, "--", "go", "version")
	}

	a, _ := newApp(t)
	if err := a.start(instanceArgs(a, "2")); err != nil {
		t.Fatal(err)
	}
	if err := a.start(instanceArgs(a, "3")); err == nil {
		t.Fatal("replacing without --force unexpectedly succeeded")
	}
	if err := a.start(instanceArgs(a, "2", "--cron-restart", "0 3 * * *", "--force")); err == nil {
		t.Fatal("instances with cron restart unexpectedly accepted")
	}
	if err := a.start(instanceArgs(a, "3", "--force")); err != nil {
		t.Fatalf("scale instances: %v", err)
	}
	if err := a.start(instanceArgs(a, "0", "--force")); err != nil {
		t.Fatalf("switch template to plain service: %v", err)
	}
	if a.isTemplate("api") || !a.exists("api") {
		t.Fatal("template was not replaced by a plain unit")
	}
	if err := os.WriteFile(a.scheduleTimerPath("api"), []byte("timer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.start(instanceArgs(a, "2", "--force")); err == nil {
		t.Fatal("switching a service with timers to instances unexpectedly succeeded")
	}

	for _, target := range []string{"daemon-reload", "disable --now", "enable"} {
		a, fail := newApp(t)
		if err := a.start(instanceArgs(a, "2", "--enable")); err != nil {
			t.Fatal(err)
		}
		*fail = target
		if err := a.start(instanceArgs(a, "3", "--enable", "--force")); err == nil {
			t.Errorf("replacement with failing %q unexpectedly succeeded", target)
		}
		*fail = ""
		if !a.isTemplate("api") {
			t.Errorf("failing %q did not keep the template unit", target)
		}
	}
}

func TestInstallPreparedCompanionFailuresRollBack(t *testing.T) {
	addFakeExecutableToPath(t, "systemctl")
	args := func(a *application, extra ...string) []string {
		base := []string{"--name", "api", "--cwd", a.unitDir, "--cron-restart", "0 3 * * *", "--health-command", "go version", "--enable"}
		return append(append(base, extra...), "--", "go", "version")
	}
	for _, target := range []string{"daemon-reload", cronRestartTimerName("api"), healthTimerName("api"), "enable --now " + unitName("api"), "restart " + unitName("api")} {
		fail := ""
		runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
			if fail != "" && strings.Contains(strings.Join(args, " "), fail) {
				return nil, errors.New("boom")
			}
			return nil, nil
		}}
		a, _ := newTestApplication(t, runner)
		if err := a.start(args(a)); err != nil {
			t.Fatal(err)
		}
		fail = target
		_ = a.start(args(a, "--force", "--description", "changed"))
		fail = ""
		if !a.exists("api") {
			t.Errorf("failing %q lost the existing unit", target)
		}
		fresh, _ := newTestApplication(t, runner)
		fail = target
		_ = fresh.start(args(fresh))
		fail = ""
	}
	a, _ := newTestApplication(t, &fakeRunner{})
	if err := a.start([]string{"--name", "api", "--cwd", a.unitDir, "--user", "nobody", "--", "go", "version"}); err == nil {
		t.Fatal("--user without --system unexpectedly accepted")
	}
}

func TestUptimeDurationAndCompletionQuery(t *testing.T) {
	if _, ok := uptimeDuration(0, 1); ok {
		t.Error("zero boot uptime unexpectedly produced an uptime")
	}
	if _, ok := uptimeDuration(time.Second, 0); ok {
		t.Error("zero activation time unexpectedly produced an uptime")
	}
	if _, ok := uptimeDuration(time.Second, uint64(2*time.Second/time.Microsecond)); ok {
		t.Error("activation after boot uptime unexpectedly produced an uptime")
	}
	if got, ok := uptimeDuration(10*time.Second, uint64(3500*time.Millisecond/time.Microsecond)); !ok || got != 6*time.Second {
		t.Errorf("uptimeDuration = %s, %t", got, ok)
	}

	home, err := filepath.Abs(localTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "HOME", "LOCALAPPDATA", "APPDATA", "USERPROFILE"} {
		t.Setenv(key, home)
	}
	var output strings.Builder
	if err := runCompletionQuery([]string{"res"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "restart") {
		t.Fatalf("completion output = %q", output.String())
	}
	output.Reset()
	if err := runCompletionQuery([]string{"--system", "restart", ""}, &output); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if err := runCompletionQuery([]string{""}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestApplyScheduledDeclarationsAndWaitTimeout(t *testing.T) {
	runner := &fakeRunner{}
	a, _ := newTestApplication(t, runner)
	path := filepath.Join(a.unitDir, "spm.toml")
	data := "[[services]]\nname = \"job\"\ncommand = [\"go\", \"version\"]\nschedule = \"*/15 * * * *\"\npersistent = true\ntimer_randomized_delay = \"30s\"\n" +
		"[[services]]\nname = \"tick\"\ncommand = [\"go\", \"version\"]\ntimer_on_boot = \"5min\"\ntimer_interval = \"10min\"\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.apply([]string{path}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"job", "tick"} {
		if !a.hasSchedule(name) {
			t.Errorf("%s schedule timer was not written", name)
		}
	}
	calls := callsString(runner.calls)
	if !strings.Contains(calls, scheduleTimerName("job")) {
		t.Fatalf("apply did not enable the schedule timer: %s", calls)
	}

	if err := a.waitForActive("job", 0); err == nil {
		t.Fatal("waitForActive with an inactive unit unexpectedly succeeded")
	}
	var warnings strings.Builder
	a.stderr = &warnings
	a.warnSandboxSupport(startOptions{sandbox: "basic"})
	a.warnSandboxSupport(startOptions{})
}
