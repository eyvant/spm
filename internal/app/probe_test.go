package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type probeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f probeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestProbeValidation(t *testing.T) {
	probe := probeConfig{Kind: "http", Target: "http://127.0.0.1:3000/health", Timeout: "1s", Retries: 1}
	if err := probe.validate(); err != nil {
		t.Fatal(err)
	}
	if err := (probeConfig{Kind: "invalid", Target: "target"}).validate(); err == nil {
		t.Fatal("invalid probe kind unexpectedly succeeded")
	}
}

func TestWaitForProbeSuccessAndTimeout(t *testing.T) {
	if err := waitForProbe(probeConfig{Kind: "command", Target: "go version", Timeout: "1s"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForProbe(probeConfig{Kind: "command", Target: "go version"}, 0); err == nil {
		t.Fatal("zero-timeout probe unexpectedly succeeded")
	}
}

func TestProbeValidationRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		probe probeConfig
	}{
		{"zero timeout", probeConfig{Kind: "http", Target: "http://localhost", Timeout: "0s"}},
		{"negative retries", probeConfig{Kind: "tcp", Target: "localhost:1", Retries: -1}},
		{"negative delay", probeConfig{Kind: "command", Target: "true", RetryDelay: "-1s"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.probe.validate(); err == nil {
				t.Fatalf("%#v unexpectedly validated", test.probe)
			}
		})
	}

	var probe probeConfig
	if err := probe.setTarget("http", ""); err == nil {
		t.Fatal("empty target unexpectedly accepted")
	}
	if err := probe.setTarget("http", "http://localhost\ninvalid"); err == nil {
		t.Fatal("newline in target unexpectedly accepted")
	}
	if err := probe.setTarget("http", "http://localhost"); err != nil {
		t.Fatal(err)
	}
	if err := probe.setTarget("tcp", "localhost:80"); err == nil {
		t.Fatal("multiple probe types unexpectedly accepted")
	}
}

func TestHealthValidationAndDefaults(t *testing.T) {
	if err := (healthConfig{Interval: "1s"}).validate(); err == nil {
		t.Fatal("health options without a probe unexpectedly validated")
	}
	for _, health := range []healthConfig{
		{Probe: probeConfig{Kind: "http", Target: "http://localhost"}, Interval: "bad"},
		{Probe: probeConfig{Kind: "tcp", Target: "localhost:80"}, StartPeriod: "0s"},
		{Probe: probeConfig{Kind: "command", Target: "true"}, Action: "reboot"},
	} {
		if err := health.validate(); err == nil {
			t.Fatalf("%#v unexpectedly validated", health)
		}
	}
	normalized := (probeConfig{}).normalized()
	if normalized.Timeout != "5s" || normalized.Retries != 3 || normalized.RetryDelay != "1s" {
		t.Fatalf("unexpected defaults: %#v", normalized)
	}
}

func TestRunProbeOnceHTTPAndTCP(t *testing.T) {
	originalClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: probeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := http.StatusNoContent
		if request.URL.Path == "/fail" {
			status = http.StatusServiceUnavailable
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = originalClient })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runProbeOnce(ctx, probeConfig{Kind: "http", Target: "http://health.local/ok"}); err != nil {
		t.Fatal(err)
	}
	if err := runProbeOnce(ctx, probeConfig{Kind: "http", Target: "http://health.local/fail"}); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("HTTP failure error = %v", err)
	}
	if err := runProbeOnce(ctx, probeConfig{Kind: "tcp", Target: "127.0.0.1:0"}); err == nil {
		t.Fatal("TCP connection to port zero unexpectedly succeeded")
	}
	if err := runProbeOnce(ctx, probeConfig{Kind: "unknown", Target: "target"}); err == nil {
		t.Fatal("unsupported probe unexpectedly succeeded")
	}
}

func TestHealthProbeCLIRejectsMalformedArguments(t *testing.T) {
	a := &application{}
	for _, args := range [][]string{
		nil,
		{"http", "http://localhost", "--timeout"},
		{"http", "http://localhost", "--unknown", "value"},
		{"http", "http://localhost", "--retries", "invalid"},
	} {
		if err := a.healthProbeCLI(args); err == nil {
			t.Errorf("arguments %v unexpectedly succeeded", args)
		}
	}
}

func TestProbeExecution(t *testing.T) {
	originalClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: probeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := http.StatusNoContent
		if request.URL.Path == "/fail" {
			status = http.StatusServiceUnavailable
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = originalClient })
	for _, probe := range []probeConfig{
		{Kind: "http", Target: "http://health.local/ok", Timeout: "1s", Retries: 1},
		{Kind: "command", Target: "exit 0", Timeout: "1s", Retries: 1},
	} {
		if err := runProbe(probe); err != nil {
			t.Fatalf("%s probe: %v", probe.Kind, err)
		}
	}
	if err := runProbe(probeConfig{Kind: "http", Target: "http://health.local/fail", Timeout: "1s", Retries: 2, RetryDelay: "0s"}); err == nil {
		t.Fatal("failing probe unexpectedly succeeded")
	}
}

func TestHealthStatusAndStateClassification(t *testing.T) {
	directory := localTempDir(t)
	for _, name := range []string{"checking", "healthy", "unhealthy", "disabled"} {
		if err := os.WriteFile(filepath.Join(directory, unitName(name)), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
		if name != "disabled" {
			if err := os.WriteFile(filepath.Join(directory, healthTimerName(name)), []byte("timer"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	runner := &fakeRunner{runFunc: func(_ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--user" {
			args = args[1:]
		}
		if len(args) > 0 && args[0] == "show" {
			return []byte(strings.Join([]string{
				"Id=spm-checking.health.service\nActiveState=active\nResult=success\nExecMainStatus=0",
				"Id=spm-healthy.health.service\nActiveState=inactive\nResult=success\nExecMainStatus=0",
				"Id=spm-unhealthy.health.service\nActiveState=failed\nResult=exit-code\nExecMainStatus=1",
				"Id=other.service\nActiveState=active",
			}, "\n\n")), nil
		}
		return nil, nil
	}}
	var output strings.Builder
	a := &application{runner: runner, stdout: &output, stderr: io.Discard, unitDir: directory}
	states := a.readHealthStates([]string{"checking", "healthy", "unhealthy", "disabled"})
	want := map[string]string{"checking": "checking", "healthy": "healthy", "unhealthy": "unhealthy", "disabled": "disabled"}
	for name, state := range want {
		if states[name] != state {
			t.Errorf("state[%s] = %q, want %q", name, states[name], state)
		}
	}
	if err := a.healthStatus("disabled"); err != nil || !strings.Contains(output.String(), "disabled") {
		t.Fatalf("disabled health status = %v, %q", err, output.String())
	}
	if err := a.healthStatus("missing"); err == nil {
		t.Fatal("missing health service unexpectedly succeeded")
	}
	if err := a.removeHealthFiles("checking"); err != nil || a.hasHealth("checking") {
		t.Fatalf("removeHealthFiles() = %v", err)
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
