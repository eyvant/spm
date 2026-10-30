package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProbeValidation(t *testing.T) {
	probe := probeConfig{Kind: "http", Target: "http://127.0.0.1:3000/health", Timeout: "1s", Retries: 1}
	if err := probe.validate(); err != nil {
		t.Fatal(err)
	}
	if err := (probeConfig{Kind: "invalid", Target: "target"}).validate(); err == nil {
		t.Fatal("invalid probe kind unexpectedly succeeded")
	}
}

func TestProbeExecutionOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("probe execution uses the Linux runtime")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	for _, probe := range []probeConfig{
		{Kind: "http", Target: server.URL, Timeout: "1s", Retries: 1},
		{Kind: "command", Target: "exit 0", Timeout: "1s", Retries: 1},
	} {
		if err := runProbe(probe); err != nil {
			t.Fatalf("%s probe: %v", probe.Kind, err)
		}
	}
}

func TestStartInstallsHealthUnits(t *testing.T) {
	directory := localTempDir(t)
	addFakeExecutableToPath(t, "systemctl")
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	if err := a.start([]string{"--name", "api", "--health-http", "http://127.0.0.1:3000/health", "--health-interval", "30s", "--health-action", "restart", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range a.healthPaths("api") {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("health unit %s was not written: %v", path, err)
		}
	}
	service, err := os.ReadFile(a.healthPath("api", ".health.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OnFailure=spm-api.health-recover.service", "health-probe", "http://127.0.0.1:3000/health"} {
		if !strings.Contains(string(service), want) {
			t.Errorf("health service does not contain %q:\n%s", want, service)
		}
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user enable --now spm-api.health.timer") {
		t.Fatalf("health timer was not enabled:\n%s", got)
	}
	timer, err := os.ReadFile(a.healthPath("api", ".health.timer"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(timer), "StartLimitIntervalSec=0") {
		t.Fatalf("health timer keeps systemd start limiting enabled:\n%s", timer)
	}
}

func TestLogsHealthSelectsHealthUnits(t *testing.T) {
	directory := localTempDir(t)
	for _, filename := range []string{unitName("api"), healthTimerName("api")} {
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.logs([]string{"api", "--health"}); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	for _, want := range []string{"--unit spm-api.health.service", "--unit spm-api.health-recover.service"} {
		if !strings.Contains(got, want) {
			t.Errorf("health logs missing %q: %s", want, got)
		}
	}
}
