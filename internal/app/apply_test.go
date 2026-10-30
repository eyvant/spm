package app

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
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

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
