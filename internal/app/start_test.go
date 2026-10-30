package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type recordedCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls           []recordedCall
	lingerOutput    []byte
	lingerErr       error
	interactiveErr  error
	interactiveFunc func(name string, args ...string) error
	runFunc         func(name string, args ...string) ([]byte, error)
}

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "spm-test-path-*")
	if err != nil {
		panic(err)
	}
	name := "go"
	content := []byte("#!/bin/sh\nexit 0\n")
	if runtime.GOOS == "windows" {
		name += ".bat"
		content = []byte("@echo off\r\n")
	}
	if err := os.WriteFile(filepath.Join(directory, name), content, 0o755); err != nil {
		panic(err)
	}
	oldPath := os.Getenv("PATH")
	_ = os.Setenv("PATH", directory+string(os.PathListSeparator)+oldPath)
	code := m.Run()
	_ = os.Setenv("PATH", oldPath)
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

func (r *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, recordedCall{name: name, args: append([]string(nil), args...)})
	if r.runFunc != nil {
		return r.runFunc(name, args...)
	}
	if name == "loginctl" && len(args) > 0 && args[0] == "show-user" {
		return r.lingerOutput, r.lingerErr
	}
	return nil, nil
}

func TestInferNameUsesScriptOrCommandInsteadOfLastArgument(t *testing.T) {
	tests := []struct {
		command  []string
		explicit bool
		want     string
	}{
		{command: []string{"index.js", "8080"}, want: "index"},
		{command: []string{"node", "server.js", "--port", "8080"}, explicit: true, want: "server"},
		{command: []string{"/opt/api/server", "--port", "8080"}, explicit: true, want: "server"},
		{command: []string{"mise", "exec", "--", "node", "apps/worker.js", "60"}, explicit: true, want: "worker"},
	}
	for _, test := range tests {
		if got := inferName(test.command, test.explicit); got != test.want {
			t.Errorf("inferName(%v, %t) = %q, want %q", test.command, test.explicit, got, test.want)
		}
	}
}

func TestPrepareResolvesScriptRelativeToCwd(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "index.js")
	if err := os.WriteFile(script, []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addFakeExecutableToPath(t, "node")
	options, err := parseStart([]string{"index.js", "8080", "--cwd", directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := options.prepare(); err != nil {
		t.Fatal(err)
	}
	if options.name != "index" || len(options.command) != 3 || options.command[1] != script {
		t.Fatalf("unexpected prepared options: %#v", options)
	}

	missing, err := parseStart([]string{"missing.js", "--cwd", directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.prepare(); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing script error = %v", err)
	}
}

func TestValidSystemdDuration(t *testing.T) {
	for _, value := range []string{"3", "3s", "250ms", "1min 30s", "1h30min"} {
		if !validSystemdDuration(value) {
			t.Errorf("validSystemdDuration(%q) = false", value)
		}
	}
	for _, value := range []string{"", "3xyz", ".5s", "3s!", " 3s"} {
		if validSystemdDuration(value) {
			t.Errorf("validSystemdDuration(%q) = true", value)
		}
	}
}

func (r *fakeRunner) Interactive(_ io.Reader, _, _ io.Writer, name string, args ...string) error {
	r.calls = append(r.calls, recordedCall{name: name, args: append([]string(nil), args...)})
	if r.interactiveFunc != nil {
		return r.interactiveFunc(name, args...)
	}
	return r.interactiveErr
}

func localTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(".", ".spm-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func addFakeExecutableToPath(t *testing.T, name string) string {
	t.Helper()
	directory := t.TempDir()
	filename := name
	if runtime.GOOS == "windows" {
		filename += ".bat"
	}
	path := filepath.Join(directory, filename)
	if err := os.WriteFile(path, []byte("@echo off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func addFakeHomeExecutable(t *testing.T, home, name string) string {
	t.Helper()
	directory := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	filename := name
	content := []byte("#!/bin/sh\nexit 0\n")
	if runtime.GOOS == "windows" {
		filename += ".bat"
		content = []byte("@echo off\r\n")
	}
	path := filepath.Join(directory, filename)
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseStartSupportsPM2StyleFlagsAfterScript(t *testing.T) {
	options, err := parseStart([]string{"index.js", "--name", "api", "--mise", "--env", "PORT=3000", "--restart=always", "--cron-restart", "*/30 * * * *", "--user", "www-data"})
	if err != nil {
		t.Fatal(err)
	}
	if options.name != "api" || !options.mise || options.restart != "always" || options.cronRestart != "*/30 * * * *" || options.runAsUser != "www-data" {
		t.Fatalf("unexpected options: %#v", options)
	}
	if len(options.command) != 1 || options.command[0] != "index.js" {
		t.Fatalf("unexpected command: %#v", options.command)
	}
}

func TestParseStartPreservesExplicitCommandArguments(t *testing.T) {
	options, err := parseStart([]string{"--name", "api", "--", "mise", "exec", "--", "node", "index.js", "--port", "3000"})
	if err != nil {
		t.Fatal(err)
	}
	want := "mise exec -- node index.js --port 3000"
	if got := strings.Join(options.command, " "); got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestPrepareWrapsExplicitCommandWithMise(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "apps", "aethir", "mon_aethir_update.js")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	miseName := "mise"
	if runtime.GOOS == "windows" {
		miseName += ".bat"
	}
	misePath := filepath.Join(directory, miseName)
	if err := os.WriteFile(misePath, []byte("@echo off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)

	options, err := parseStart([]string{
		"--name", "mon_aethir_update", "--cwd", directory, "--mise", "--",
		"node", "./apps/aethir/mon_aethir_update.js", "--interval", "60",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := options.prepare(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		misePath, "exec", "--", "node", script, "--interval", "60",
	}
	if strings.Join(options.command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command = %#v, want %#v", options.command, want)
	}
}

func writeSnapMise(t *testing.T, confinement string) (string, string) {
	t.Helper()
	root := t.TempDir()
	snapBin := filepath.Join(root, "snap", "bin")
	current := filepath.Join(root, "snap", "mise", "current")
	for _, directory := range []string{snapBin, filepath.Join(current, "meta"), filepath.Join(current, "bin")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	launcherName := "mise"
	if runtime.GOOS == "windows" {
		launcherName += ".bat"
	}
	if err := os.WriteFile(filepath.Join(snapBin, launcherName), []byte("@echo off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	snapYAML := "name: mise\n" +
		"description: |\n" +
		"  command: not-this\n" +
		"apps:\n" +
		"  other:\n" +
		"    command: bin/other\n" +
		"  mise:\n" +
		"    environment:\n" +
		"      command: not-this\n" +
		"    command: bin/mise\n" +
		"confinement: " + confinement + "\n"
	if err := os.WriteFile(filepath.Join(current, "meta", "snap.yaml"), []byte(snapYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(current, "bin", "mise")
	if err := os.WriteFile(executable, []byte("executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", snapBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return root, executable
}

func TestPrepareRunsSnapMiseBinaryDirectly(t *testing.T) {
	root, snapMise := writeSnapMise(t, "classic")
	explicitScript := filepath.Join(root, "apps", "api.js")
	if err := os.MkdirAll(filepath.Dir(explicitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicitScript, []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	options, err := parseStart([]string{
		"--name", "api", "--cwd", root, "--mise", "--",
		"node", "./apps/api.js", "--port", "3000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := options.prepare(); err != nil {
		t.Fatal(err)
	}
	if !options.snapMise {
		t.Fatal("Snap mise was not detected")
	}
	wantCommand := []string{snapMise, "exec", "--", "node", explicitScript, "--port", "3000"}
	if strings.Join(options.command, "\x00") != strings.Join(wantCommand, "\x00") {
		t.Fatalf("command = %#v, want %#v", options.command, wantCommand)
	}
	if len(options.env) != 0 {
		t.Fatalf("Snap mise should not add environment values: %#v", options.env)
	}

	scriptPath := filepath.Join(root, "worker.py")
	if err := os.WriteFile(scriptPath, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptOptions, err := parseStart([]string{scriptPath, "--name", "worker", "--cwd", root, "--mise"})
	if err != nil {
		t.Fatal(err)
	}
	if err := scriptOptions.prepare(); err != nil {
		t.Fatal(err)
	}
	wantScriptCommand := []string{snapMise, "exec", "--", "python3", scriptPath}
	if !scriptOptions.snapMise || strings.Join(scriptOptions.command, "\x00") != strings.Join(wantScriptCommand, "\x00") {
		t.Fatalf("script command = %#v, want %#v", scriptOptions.command, wantScriptCommand)
	}
}

func TestPrepareRejectsStrictSnapMise(t *testing.T) {
	root, _ := writeSnapMise(t, "strict")
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	options, err := parseStart([]string{"--name", "api", "--cwd", root, "--mise", "--", "node", "index.js"})
	if err != nil {
		t.Fatal(err)
	}
	if err := options.prepare(); err == nil || !strings.Contains(err.Error(), "confinement") {
		t.Fatalf("prepare() error = %v, want confinement error", err)
	}
}

func TestParseSnapAppReadsQuotedCommandWithCRLF(t *testing.T) {
	confinement, command := parseSnapApp([]byte("apps:\r\n  tool:\r\n    command: \"$SNAP/bin/tool\"\r\nconfinement: classic\r\n"), "tool")
	if confinement != "classic" || command != "$SNAP/bin/tool" {
		t.Fatalf("parseSnapApp() = %q, %q", confinement, command)
	}
}

func TestIsSnapMise(t *testing.T) {
	for _, path := range []string{"/snap/bin/mise", "/var/lib/snapd/snap/bin/mise", "/tmp/project/snap/bin/mise"} {
		if !isSnapMise(path) {
			t.Errorf("isSnapMise(%q) = false", path)
		}
	}
	if isSnapMise("/home/ubuntu/.local/bin/mise") {
		t.Fatal("regular mise installation was detected as Snap")
	}
}

func TestParseStartRejectsValuesForBooleanOptions(t *testing.T) {
	for _, args := range [][]string{
		{"index.js", "--mise=false"},
		{"index.js", "--force=true"},
		{"index.js", "--no-enable=false"},
		{"index.js", "-f=true"},
	} {
		if _, err := parseStart(args); err == nil {
			t.Errorf("parseStart(%v) unexpectedly succeeded", args)
		}
	}
}

func TestRenderUnitEscapesSystemdExpansion(t *testing.T) {
	options := defaultStartOptions()
	options.name = "api"
	options.description = "API service 100%"
	options.cwd = "/srv/app with spaces"
	options.command = []string{"/usr/bin/node", "/srv/app/index.js", "100%", "$PORT", `a"b`}
	options.env = []string{"TOKEN=a$b%"}
	options.envFile = "/srv/app with spaces/config%prod.env"

	unit, err := renderUnit(options, false)
	if err != nil {
		t.Fatal(err)
	}
	checks := []string{
		"Description=API service 100%%\n",
		"WorkingDirectory=/srv/app with spaces\n",
		"EnvironmentFile=/srv/app with spaces/config%%prod.env\n",
		`"100%%"`,
		`"$$PORT"`,
		`"a\"b"`,
		`Environment="TOKEN=a$b%%"`,
		`WantedBy=default.target`,
	}
	for _, check := range checks {
		if !strings.Contains(unit, check) {
			t.Errorf("unit does not contain %q:\n%s", check, unit)
		}
	}
}

func TestEscapeSystemdPath(t *testing.T) {
	tests := map[string]string{
		"/home/ubuntu/code/xss-js/go/http_proxy": "/home/ubuntu/code/xss-js/go/http_proxy",
		"/srv/app with spaces":                   `/srv/app with spaces`,
		"/srv/100%/a$b":                          `/srv/100%%/a$b`,
		"/srv/应用":                                `/srv/应用`,
	}
	for input, want := range tests {
		if got := escapeSystemdPath(input); got != want {
			t.Errorf("escapeSystemdPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateNameRejectsPathsAndInstances(t *testing.T) {
	for _, name := range []string{"", "../api", "api/service", "api@1", "-api", "api.cron-restart"} {
		if err := validateName(name); err == nil {
			t.Errorf("validateName(%q) unexpectedly succeeded", name)
		}
	}
	for _, name := range []string{"api", "api-1", "worker.eu_1"} {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q): %v", name, err)
		}
	}
}

func TestWriteFileAtomic(t *testing.T) {
	directory := localTempDir(t)
	path := filepath.Join(directory, "spm-api.service")
	if err := writeFileAtomic(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateEnvironment(t *testing.T) {
	for _, value := range []string{"PORT=3000", "NODE_ENV=production", "EMPTY="} {
		if err := validateEnvironment(value); err != nil {
			t.Errorf("validateEnvironment(%q): %v", value, err)
		}
	}
	for _, value := range []string{"PORT", "1PORT=3000", "BAD-NAME=value", "A=x\ny"} {
		if err := validateEnvironment(value); err == nil {
			t.Errorf("validateEnvironment(%q) unexpectedly succeeded", value)
		}
	}
}

func TestStartCreatesAndEnablesUnit(t *testing.T) {
	directory := localTempDir(t)
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}

	if err := a.start([]string{"--name", "api", "--cwd", directory, "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(directory, "spm-api.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "ExecStart=") || !strings.Contains(string(unit), `"version"`) {
		t.Fatalf("unexpected unit:\n%s", unit)
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user daemon-reload") || !strings.Contains(got, "systemctl --user enable --now spm-api.service") {
		t.Fatalf("unexpected calls: %s", got)
	}
}

func TestSystemStartCreatesAndEnablesSystemUnit(t *testing.T) {
	directory := localTempDir(t)
	home, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	targetMise := addFakeHomeExecutable(t, home, "mise")
	addFakeExecutableToPath(t, "mise")
	runner := &fakeRunner{}
	a := &application{
		runner: runner, stdout: io.Discard, stderr: io.Discard,
		unitDir: directory, configDir: filepath.Join(directory, "config"), system: true,
		userHome: func(string) (string, error) { return home, nil },
	}

	if err := a.start([]string{"--name", "api", "--cwd", directory, "--user", "www-data", "--mise", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(directory, "spm-api.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "WantedBy=multi-user.target") || !strings.Contains(string(unit), "User=www-data") || !strings.Contains(string(unit), quoteSystemdExec(targetMise)) {
		t.Fatalf("unexpected system unit:\n%s", unit)
	}
	if err := a.start([]string{"--force", "--name", "api", "--cwd", directory, "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	unit, err = os.ReadFile(filepath.Join(directory, "spm-api.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "User=www-data") {
		t.Fatalf("system user was not inherited:\n%s", unit)
	}
	if err := a.start([]string{"--force", "--name", "api", "--cwd", directory, "--user", "", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	unit, err = os.ReadFile(filepath.Join(directory, "spm-api.service"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "User=") {
		t.Fatalf("system user was not cleared:\n%s", unit)
	}
	got := callsString(runner.calls)
	for _, want := range []string{"systemctl daemon-reload", "systemctl enable --now spm-api.service"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing call %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--user") || strings.Contains(got, "loginctl") {
		t.Fatalf("system start used user manager operations:\n%s", got)
	}
}

func TestUserModeRejectsRunAsUser(t *testing.T) {
	directory := localTempDir(t)
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	err := a.start([]string{"--name", "api", "--cwd", directory, "--user", "www-data", "--", "go", "version"})
	if err == nil || !strings.Contains(err.Error(), "only supported with global --system") {
		t.Fatalf("start error = %v", err)
	}
}

func TestSystemStartRejectsUnknownRunAsUser(t *testing.T) {
	directory := localTempDir(t)
	a := &application{
		runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard,
		unitDir: directory, system: true,
		userHome: func(string) (string, error) { return "", errors.New("user not found") },
	}
	err := a.start([]string{"--name", "api", "--cwd", directory, "--user", "missing", "--", "go", "version"})
	if err == nil || !strings.Contains(err.Error(), `resolve system user "missing"`) {
		t.Fatalf("start error = %v", err)
	}
}

func TestStartOnlyPrintsStartupHintWhenLingerIsDisabled(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      string
		wantPrompt bool
	}{
		{name: "enabled", state: "yes\n", wantPrompt: false},
		{name: "disabled", state: "no\n", wantPrompt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := localTempDir(t)
			var output strings.Builder
			runner := &fakeRunner{lingerOutput: []byte(test.state)}
			a := &application{runner: runner, stdout: io.Discard, stderr: &output, unitDir: directory}

			if err := a.start([]string{"--name", "api", "--cwd", directory, "--", "go", "version"}); err != nil {
				t.Fatal(err)
			}
			prompted := strings.Contains(output.String(), "Hint: Run `spm startup`")
			if prompted != test.wantPrompt {
				t.Fatalf("output = %q, startup prompt = %t, want %t", output.String(), prompted, test.wantPrompt)
			}
		})
	}
}

func TestStartCreatesCronRestartTimer(t *testing.T) {
	addFakeExecutableToPath(t, "systemctl")
	directory := localTempDir(t)
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}

	if err := a.start([]string{
		"--name", "api", "--cwd", directory, "--cron-restart", "*/30 * * * *", "--",
		"go", "version",
	}); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{unitName("api"), cronRestartServiceName("api"), cronRestartTimerName("api")} {
		if _, err := os.Stat(filepath.Join(directory, filename)); err != nil {
			t.Errorf("expected unit %s: %v", filename, err)
		}
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user enable --now "+cronRestartTimerName("api")) {
		t.Fatalf("cron timer was not enabled: %s", got)
	}
}

func TestForceUpdateRemovesCronRestartTimer(t *testing.T) {
	addFakeExecutableToPath(t, "systemctl")
	directory := localTempDir(t)
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	baseArgs := []string{"--name", "api", "--cwd", directory, "--", "go", "version"}
	cronArgs := []string{"--name", "api", "--cwd", directory, "--cron-restart", "*/30 * * * *", "--", "go", "version"}
	if err := a.start(cronArgs); err != nil {
		t.Fatal(err)
	}
	if err := a.start(append([]string{"--force", "--no-cron-restart"}, baseArgs...)); err != nil {
		t.Fatal(err)
	}
	if a.hasCronRestart("api") {
		t.Fatal("cron restart timer still exists after replacement without --cron-restart")
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user disable --now "+cronRestartTimerName("api")) {
		t.Fatalf("cron timer was not disabled: %s", got)
	}
}

func TestForceUpdateRestartsExistingUnit(t *testing.T) {
	directory := localTempDir(t)
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	args := []string{"--name", "api", "--cwd", directory, "--", "go", "version"}
	if err := a.start(args); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if err := a.start(append(args, "--force")); err == nil {
		t.Fatal("explicit command arguments after -- must not be interpreted as spm options")
	}
	forceArgs := []string{"--force", "--name", "api", "--cwd", directory, "--", "go", "version"}
	if err := a.start(forceArgs); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	if !strings.Contains(got, "systemctl --user enable spm-api.service") || !strings.Contains(got, "systemctl --user restart spm-api.service") {
		t.Fatalf("updated service was not enabled and restarted: %s", got)
	}
}

func TestForceNoEnableDisablesExistingServiceAndTimer(t *testing.T) {
	addFakeExecutableToPath(t, "systemctl")
	directory := localTempDir(t)
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	args := []string{"--name", "api", "--cwd", directory, "--cron-restart", "*/30 * * * *", "--", "go", "version"}
	if err := a.start(args); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if err := a.start(append([]string{"--force", "--no-enable"}, args...)); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	for _, want := range []string{"systemctl --user disable spm-api.service", "systemctl --user disable spm-api.cron-restart.timer"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in calls:\n%s", want, got)
		}
	}
}

func TestForceFailureRestoresPreviousUnit(t *testing.T) {
	directory := localTempDir(t)
	unitPath := filepath.Join(directory, unitName("api"))
	oldUnit := []byte("old unit\n")
	if err := os.WriteFile(unitPath, oldUnit, 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 1 && args[1] == "restart" {
			return nil, errors.New("restart failed")
		}
		return nil, nil
	}}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	err := a.start([]string{"--force", "--name", "api", "--cwd", directory, "--", "go", "version"})
	if err == nil {
		t.Fatal("start unexpectedly succeeded")
	}
	got, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(oldUnit) {
		t.Fatalf("unit was not restored:\n%s", got)
	}
}

func TestForceInheritsSavedOptions(t *testing.T) {
	addFakeExecutableToPath(t, "systemctl")
	directory := localTempDir(t)
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	initial := []string{
		"--name", "api", "--cwd", directory,
		"--restart-sec", "9s", "--cron-restart", "*/30 * * * *",
		"--env", "PORT=3000", "--max-memory", "512M",
		"--property", "Nice=5", "--", "go", "version",
	}
	if err := a.start(initial); err != nil {
		t.Fatal(err)
	}
	configInfo, err := os.Stat(a.serviceConfigPath("api"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && configInfo.Mode().Perm() != 0o600 && configInfo.Mode().Perm() != 0o777 {
		t.Fatalf("config mode = %o, want 600", configInfo.Mode().Perm())
	}
	if err := a.start([]string{"--force", "--name", "api", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(a.unitPath("api"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"RestartSec=9s", "Environment=\"PORT=3000\"", "MemoryMax=512M", "Nice=5"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("updated unit did not inherit %q:\n%s", want, unit)
		}
	}
	if !a.hasCronRestart("api") {
		t.Fatal("cron restart was not inherited")
	}
}

func TestRenderUnitSupportsLimitsAndSafeProperties(t *testing.T) {
	options := defaultStartOptions()
	options.name = "api"
	options.description = "api"
	options.cwd = "/srv/api"
	options.command = []string{"/srv/api/server"}
	options.startLimitInterval = "30s"
	options.startLimitBurst = "20"
	options.maxMemory = "512M"
	options.properties = []string{"Nice=5"}
	unit, err := renderUnit(options, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"StartLimitIntervalSec=30s", "StartLimitBurst=20", "MemoryMax=512M", "Nice=5"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit does not contain %q:\n%s", want, unit)
		}
	}
	for _, property := range []string{"ExecStart=/tmp/other", "User=root", "Bad-Name=value", "Nice=x\ny"} {
		if err := validateServiceProperty(property); err == nil {
			t.Errorf("validateServiceProperty(%q) unexpectedly succeeded", property)
		}
	}
}

func TestRenderSystemUnitUsesMultiUserTarget(t *testing.T) {
	options := defaultStartOptions()
	options.name = "api"
	options.description = "api"
	options.cwd = "/srv/api"
	options.command = []string{"/srv/api/server"}
	options.runAsUser = "www-data"
	unit, err := renderUnit(options, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "WantedBy=multi-user.target") || !strings.Contains(unit, "User=www-data") || strings.Contains(unit, "WantedBy=default.target") {
		t.Fatalf("unexpected system install target:\n%s", unit)
	}
}

func TestValidateSystemUser(t *testing.T) {
	for _, value := range []string{"", "root", "www-data", "service.user", "_worker", "1000"} {
		if err := validateSystemUser(value); err != nil {
			t.Errorf("validateSystemUser(%q): %v", value, err)
		}
	}
	for _, value := range []string{"-worker", "user name", "user/name", "user\nUser=root", "4294967296"} {
		if err := validateSystemUser(value); err == nil {
			t.Errorf("validateSystemUser(%q) unexpectedly succeeded", value)
		}
	}
}

func TestRestartUpdateRegeneratesUnitFromSavedConfig(t *testing.T) {
	directory := localTempDir(t)
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	args := []string{"--name", "api", "--cwd", directory, "--restart-sec", "7s", "--", "go", "version"}
	if err := a.start(args); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.unitPath("api"), []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.controlMany("restart", []string{"--update", "api"}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(a.unitPath("api"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "RestartSec=7s") || strings.Contains(string(unit), "broken") {
		t.Fatalf("unit was not regenerated:\n%s", unit)
	}
}

func TestInterpreterProgramIndexSkipsOptionsWithValues(t *testing.T) {
	command := []string{"node", "--require", "./register.js", "./server.js", "--port", "3000"}
	index, isFile := interpreterProgramIndex(command, 0)
	if index != 3 || !isFile {
		t.Fatalf("interpreterProgramIndex() = %d, %t, want 3, true", index, isFile)
	}
	if got := inferName(command, true); got != "server" {
		t.Fatalf("inferName() = %q, want server", got)
	}
}

func TestResolveExplicitScriptAcceptsShellInlineCommand(t *testing.T) {
	command := []string{"/bin/sh", "-c", "echo ready; exec /bin/sleep 300"}
	if err := resolveExplicitScript(command, t.TempDir()); err != nil {
		t.Fatalf("resolveExplicitScript() rejected an inline shell command: %v", err)
	}
	if got := inferName(command, true); got != "sh" {
		t.Fatalf("inferName() = %q, want sh", got)
	}
}

func TestMiseUsesScriptShebang(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "worker.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\necho ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addFakeExecutableToPath(t, "mise")
	options, err := parseStart([]string{"worker.sh", "--cwd", directory, "--mise"})
	if err != nil {
		t.Fatal(err)
	}
	if err := options.prepare(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(options.command, " "); !strings.Contains(got, "exec -- bash "+script) {
		t.Fatalf("mise command did not honor shebang: %s", got)
	}
}

func TestCheckStartedReportsSystemdResult(t *testing.T) {
	runner := &fakeRunner{runFunc: func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 1 && args[1] == "show" {
			return []byte("Id=spm-api.service\nActiveState=failed\nSubState=failed\nResult=exit-code\nExecMainCode=1\nExecMainStatus=2\n"), nil
		}
		return nil, nil
	}}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard}
	err := a.checkStarted("api", "always")
	if err == nil || !strings.Contains(err.Error(), "result=exit-code") || !strings.Contains(err.Error(), "1/2") {
		t.Fatalf("checkStarted() error = %v", err)
	}
}

func TestStartConveniencePropertiesAndDependencies(t *testing.T) {
	options, err := parseStart([]string{
		"--max-cpu", "200%", "--max-tasks", "50", "--stop-timeout", "45s", "--sandbox", "basic",
		"--after", "network.target", "--requires", "redis.service", "--", "go", "version",
	})
	if err != nil {
		t.Fatal(err)
	}
	options.name = "api"
	options.cwd = t.TempDir()
	if err := options.prepare(); err != nil {
		t.Fatal(err)
	}
	unit, err := renderUnit(options, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CPUQuota=200%", "TasksMax=50", "TimeoutStopSec=45s", "NoNewPrivileges=true", "ProtectHome=read-only", "ReadWritePaths=", "After=network.target", "Requires=redis.service"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit does not contain %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "TimeoutStopSec=30s") {
		t.Errorf("default stop timeout should be omitted when --stop-timeout is set:\n%s", unit)
	}
}

func TestStartStructuredLifecycleOptions(t *testing.T) {
	options, err := parseStart([]string{
		"--group", "www-data", "--exec-start-pre", "/usr/bin/test -f config.json",
		"--exec-start-post", "/usr/bin/logger started", "--exec-stop", "/usr/bin/logger stopping",
		"--exec-reload", "/bin/kill -HUP $MAINPID", "--on-failure", "notify.service",
		"--", "/usr/bin/server",
	})
	if err != nil {
		t.Fatal(err)
	}
	options.name, options.cwd = "api", t.TempDir()
	unit, err := renderUnit(options, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Group=www-data", "ExecStartPre=/usr/bin/test -f config.json", "ExecStartPost=/usr/bin/logger started", "ExecStop=/usr/bin/logger stopping", "ExecReload=/bin/kill -HUP $MAINPID", "OnFailure=notify.service"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit does not contain %q:\n%s", want, unit)
		}
	}
}

func callsString(calls []recordedCall) string {
	var values []string
	for _, call := range calls {
		values = append(values, strings.Join(append([]string{call.name}, call.args...), " "))
	}
	return strings.Join(values, "\n")
}
