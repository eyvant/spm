package app

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseServiceDeclarations(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "spm.toml")
	data := []byte(`[[services]]
name = "api"
command = ["node", "apps/api.js", "--port", "3000"]
user = "www-data"
mise = true
restart_sec = "5s"
env = ["NODE_ENV=production"]
properties = ["Nice=5"]
group = "www-data"
exec_start_pre = ["/usr/bin/test -f config.json"]
exec_start_post = ["/usr/bin/logger api-started"]
exec_stop = "/usr/bin/logger api-stopping"
exec_reload = "/bin/kill -HUP $MAINPID"
on_failure = ["notify-admin.service"]

[[services]]
name = "worker"
command = ["/opt/worker"]
enable = false
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	services, err := parseServiceDeclarations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 {
		t.Fatalf("services = %d, want 2", len(services))
	}
	if services[0].Name != "api" || !services[0].Mise || services[0].Cwd != directory || services[0].User != "www-data" || services[0].Group != "www-data" {
		t.Fatalf("unexpected api declaration: %#v", services[0])
	}
	if !reflect.DeepEqual(services[0].Command, []string{"node", "apps/api.js", "--port", "3000"}) {
		t.Fatalf("unexpected command: %#v", services[0].Command)
	}
	if !services[1].EnableSet || services[1].Enable {
		t.Fatalf("unexpected worker enable setting: %#v", services[1])
	}
}

func TestParseServiceDeclarationSupportsIntervalTimer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spm.toml")
	data := []byte("[[services]]\nname = \"backup\"\ncommand = [\"go\", \"version\"]\ntimer_on_boot = \"5min\"\ntimer_interval = \"30min\"\ntimer_randomized_delay = \"2min\"\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	services, err := parseServiceDeclarations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || !services[0].hasSchedule() || services[0].TimerInterval != "30min" {
		t.Fatalf("unexpected timer declaration: %#v", services)
	}
}

func TestParseServiceDeclarationsRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spm.toml")
	if err := os.WriteFile(path, []byte("[[services]]\nname = \"api\"\ncommand = [\"go\", \"version\"]\nunknown = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseServiceDeclarations(path); err == nil {
		t.Fatal("unknown key unexpectedly succeeded")
	}
}

func TestParseServiceDeclarationsAppliesPlatformOverlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spm.toml")
	data := []byte(`schema = 1
[[services]]
name = "api"
command = ["common", "serve"]
restart = "always"

[services.linux]
command = ["linux", "serve"]
user = "www-data"

[services.windows]
command = ["windows.exe", "serve"]
user = "LocalService"

[services.macos]
command = ["macos", "serve"]
user = "_www"
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	for platform, want := range map[string]struct {
		command string
		user    string
	}{
		"linux":   {"linux", "www-data"},
		"windows": {"windows.exe", "LocalService"},
		"macos":   {"macos", "_www"},
	} {
		services, err := parseServiceDeclarationsForPlatform(path, platform)
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		if got := services[0]; got.Command[0] != want.command || got.User != want.user || got.Restart != "always" {
			t.Fatalf("%s overlay = %#v", platform, got)
		}
	}
}

func TestParseServiceDeclarationsValidatesInactiveOverlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spm.toml")
	data := []byte("[[services]]\nname = \"api\"\ncommand = [\"go\", \"version\"]\n[services.windows]\nenable = maybe\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseServiceDeclarationsForPlatform(path, "linux"); err == nil {
		t.Fatal("invalid inactive overlay unexpectedly succeeded")
	}
}

func TestParseServiceDeclarationsOrdersDependencies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spm.toml")
	data := []byte(`[[services]]
name = "api"
command = ["api"]
requires = ["database"]
[[services]]
name = "database"
command = ["database"]
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	services, err := parseServiceDeclarationsForPlatform(path, "windows")
	if err != nil {
		t.Fatal(err)
	}
	if services[0].Name != "database" || services[1].Name != "api" {
		t.Fatalf("dependency order = %s, %s", services[0].Name, services[1].Name)
	}
}

func TestApplyCreatesDeclaredServices(t *testing.T) {
	directory := localTempDir(t)
	home, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	targetGo := addFakeHomeExecutable(t, home, "go")
	path := filepath.Join(directory, "spm.toml")
	data := []byte("[[services]]\nname = \"api\"\ncommand = [\"go\", \"version\"]\nrestart_sec = \"4s\"\nmax_memory = \"128M\"\nuser = \"www-data\"\ngroup = \"www-data\"\nexec_start_pre = [\"/usr/bin/test -f config.json\"]\nexec_start_post = [\"/usr/bin/logger started\"]\nexec_stop = \"/usr/bin/logger stopping\"\nexec_reload = \"/bin/kill -HUP $MAINPID\"\non_failure = [\"notify.service\"]\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	a := &application{
		runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard,
		unitDir: directory, configDir: filepath.Join(directory, "config"), system: true,
		userHome: func(string) (string, error) { return home, nil },
	}
	if err := a.apply([]string{path}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(a.unitPath("api"))
	if err != nil {
		t.Fatal(err)
	}
	if string(unit) == "" || !containsAll(string(unit), "RestartSec=4s", "MemoryMax=128M", "User=www-data", "Group=www-data", "ExecStartPre=/usr/bin/test -f config.json", "ExecStartPost=/usr/bin/logger started", "ExecStop=/usr/bin/logger stopping", "ExecReload=/bin/kill -HUP $MAINPID", "OnFailure=notify.service", quoteSystemdExec(targetGo)) {
		t.Fatalf("unexpected unit:\n%s", unit)
	}
}

func TestApplyDryRunDoesNotWriteUnits(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "spm.toml")
	if err := os.WriteFile(path, []byte("[[services]]\nname = \"api\"\ncommand = [\"go\", \"version\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	a := &application{runner: &fakeRunner{}, stdout: &output, stderr: io.Discard, unitDir: filepath.Join(directory, "units"), configDir: filepath.Join(directory, "config")}
	if err := a.apply([]string{"--dry-run", path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "create\tapi") {
		t.Fatalf("unexpected dry-run output: %q", output.String())
	}
	if a.exists("api") {
		t.Fatal("dry-run wrote a unit")
	}
}

func TestInspectDeclarationValidateDiffAndUpdate(t *testing.T) {
	directory := localTempDir(t)
	path := filepath.Join(directory, "spm.toml")
	write := func(description string) {
		data := "[[services]]\nname = \"api\"\ncommand = [\"go\", \"version\"]\nrestart_sec = \"4s\"\n"
		if description != "" {
			data += "description = \"" + description + "\"\n"
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	var output strings.Builder
	a := &application{runner: &fakeRunner{}, stdout: &output, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	if err := a.inspectDeclaration("validate", []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "is valid") {
		t.Fatalf("validate output = %q", output.String())
	}
	output.Reset()
	if err := a.inspectDeclaration("diff", []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "create\tapi") {
		t.Fatalf("create diff = %q", output.String())
	}
	if err := a.apply([]string{path}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := a.inspectDeclaration("diff", []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "unchanged\tapi") {
		t.Fatalf("unchanged diff = %q", output.String())
	}
	write("updated")
	output.Reset()
	if err := a.inspectDeclaration("diff", []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "update\tapi") || !strings.Contains(output.String(), "+  description = \"updated\"") {
		t.Fatalf("update diff = %q", output.String())
	}
	for _, args := range [][]string{nil, {path, path}} {
		if err := a.inspectDeclaration("validate", args); err == nil {
			t.Errorf("inspectDeclaration(%v) unexpectedly succeeded", args)
		}
	}
}

func TestApplyArgumentErrors(t *testing.T) {
	a := &application{stdout: io.Discard, stderr: io.Discard}
	for _, args := range [][]string{nil, {"--unknown"}, {"one.toml", "two.toml"}} {
		if err := a.apply(args); err == nil {
			t.Errorf("apply(%v) unexpectedly succeeded", args)
		}
	}
}

func TestSetDeclarationValueCoversAllKeys(t *testing.T) {
	service := &serviceDeclaration{}
	stringsByKey := map[string]string{
		"name": "api", "cwd": "/srv/api", "description": "API", "restart": "always", "restart_sec": "3s",
		"start_limit_interval": "30s", "start_limit_burst": "5", "cron_restart": "*/5 * * * *", "env_file": ".env",
		"interpreter": "node", "max_memory": "128M", "user": "svc", "group": "svc", "type": "forking",
		"pid_file": "/tmp/api.pid", "timezone": "UTC", "script": "server.js", "exec_stop": "stop", "exec_reload": "reload",
		"schedule": "0 * * * *", "timer_on_boot": "1min", "timer_interval": "5min", "timer_randomized_delay": "30s",
		"health_http": "http://localhost/health", "health_tcp": "localhost:80", "health_command": "echo ok",
		"health_interval": "30s", "health_timeout": "5s", "health_retry_delay": "1s", "health_start_period": "10s", "health_action": "restart",
	}
	for key, value := range stringsByKey {
		if err := setDeclarationValue(service, key, strconv.Quote(value)); err != nil {
			t.Errorf("setDeclarationValue(%s): %v", key, err)
		}
	}
	arraysByKey := map[string][]string{
		"command": {"node", "server.js"}, "args": {"--port", "3000"}, "env": {"PORT=3000"}, "properties": {"Nice=5"},
		"exec_start_pre": {"echo pre"}, "exec_start_post": {"echo post"}, "after": {"network.target"}, "wants": {"cache.service"},
		"requires": {"db.service"}, "on_failure": {"notify.service"},
	}
	for key, value := range arraysByKey {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := setDeclarationValue(service, key, string(raw)); err != nil {
			t.Errorf("setDeclarationValue(%s): %v", key, err)
		}
	}
	for key, raw := range map[string]string{"instances": "2", "health_retries": "3", "persistent": "true", "mise": "true", "enable": "false"} {
		if err := setDeclarationValue(service, key, raw); err != nil {
			t.Errorf("setDeclarationValue(%s): %v", key, err)
		}
	}
	for key, raw := range map[string]string{
		"name": "unquoted", "command": "bad", "instances": "-1", "health_retries": "0", "persistent": "maybe", "mise": "maybe", "enable": "maybe", "missing": `"value"`,
	} {
		if err := setDeclarationValue(&serviceDeclaration{}, key, raw); err == nil {
			t.Errorf("invalid %s=%s unexpectedly succeeded", key, raw)
		}
	}
	for _, line := range []string{`value = "# retained" # removed`, `value = "escaped \" # retained" # removed`, "value = 1 # removed", "value = 1"} {
		if got := stripTOMLComment(line); strings.Contains(got, "removed") {
			t.Errorf("stripTOMLComment(%q) = %q", line, got)
		}
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
