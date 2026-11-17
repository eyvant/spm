package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseLogsSources(t *testing.T) {
	options, err := parseLogs([]string{"--spm", "api"})
	if err != nil || !options.spm || options.scheduler || strings.Join(options.names, ",") != "api" {
		t.Fatalf("parseLogs(--spm api) = %#v, %v", options, err)
	}
	options, err = parseLogs([]string{"--scheduler", "-n", "5"})
	if err != nil || !options.scheduler || options.lines != "5" {
		t.Fatalf("parseLogs(--scheduler) = %#v, %v", options, err)
	}
	for _, args := range [][]string{
		{"--spm", "--scheduler"},
		{"--spm", "--health", "api"},
		{"--scheduler", "api"},
		{"--spm=yes", "api"},
	} {
		if _, err := parseLogs(args); err == nil {
			t.Errorf("parseLogs(%v) unexpectedly succeeded", args)
		}
	}
}

func TestLinuxSpmLogsMatchManagerMessages(t *testing.T) {
	directory := localTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, unitName("api")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory, system: true}
	if err := a.logs([]string{"--spm", "api"}); err != nil {
		t.Fatal(err)
	}
	got := callsString(runner.calls)
	if !strings.Contains(got, "journalctl UNIT=spm-api.service --no-pager --lines 100") || strings.Contains(got, "--unit") {
		t.Fatalf("system --spm logs = %s", got)
	}

	runner = &fakeRunner{interactiveFunc: func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "--user" {
			return testExitError(1)
		}
		return nil
	}}
	a = &application{runner: runner, stdout: io.Discard, stderr: io.Discard, unitDir: directory}
	if err := a.logs([]string{"--spm", "api"}); err != nil {
		t.Fatal(err)
	}
	got = callsString(runner.calls)
	for _, want := range []string{"journalctl --user USER_UNIT=spm-api.service --no-pager", "journalctl USER_UNIT=spm-api.service _UID=" + strconv.Itoa(os.Getuid()) + " --no-pager"} {
		if !strings.Contains(got, want) {
			t.Errorf("user --spm logs do not contain %q: %s", want, got)
		}
	}
	if err := a.logs([]string{"--scheduler"}); err == nil || !strings.Contains(err.Error(), "no spm scheduler on Linux") {
		t.Fatalf("--scheduler on Linux = %v", err)
	}
}

type fakeFileLogs struct{ root string }

func (f fakeFileLogs) names() ([]string, error) { return []string{"api", "worker"}, nil }
func (f fakeFileLogs) exists(name string) bool  { return name == "api" || name == "worker" }
func (f fakeFileLogs) logPath(name string) string {
	return filepath.Join(f.root, name, "service.log")
}
func (f fakeFileLogs) eventLogPath(name string) string {
	return filepath.Join(f.root, name, "spm.log")
}
func (f fakeFileLogs) schedulerLogPath() string { return filepath.Join(f.root, "scheduler.log") }

func TestFileLogsSelectSourceAndPrefixSeveralServices(t *testing.T) {
	root := localTempDir(t)
	backend := fakeFileLogs{root: root}
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(backend.logPath("api"), "api output\n")
	write(backend.eventLogPath("api"), "[t] starting api\n")
	write(backend.logPath("worker"), "worker output\n")
	write(backend.eventLogPath("worker"), "[t] starting worker\n")
	write(backend.schedulerLogPath(), "[t] scheduler started\n")

	read := func(args ...string) string {
		t.Helper()
		options, err := parseLogs(args)
		if err != nil {
			t.Fatal(err)
		}
		files, err := selectLogFiles(backend, options, "test")
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := printLogFiles(&output, files, options); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}
	if got := read("api"); got != "api output\n" {
		t.Errorf("service log = %q", got)
	}
	if got := read("--spm", "api"); got != "[t] starting api\n" {
		t.Errorf("spm log = %q", got)
	}
	if got := read("--spm"); got != "api | [t] starting api\nworker | [t] starting worker\n" {
		t.Errorf("spm logs of all services = %q", got)
	}
	if got := read("--scheduler"); got != "[t] scheduler started\n" {
		t.Errorf("scheduler log = %q", got)
	}
	options, _ := parseLogs([]string{"--since", "1h", "api"})
	if _, err := selectLogFiles(backend, options, "test"); err == nil || !strings.Contains(err.Error(), "test logs support") {
		t.Errorf("--since was accepted: %v", err)
	}
	options, _ = parseLogs([]string{"missing"})
	if _, err := selectLogFiles(backend, options, "test"); err == nil {
		t.Error("an unknown service was accepted")
	}
}

func TestLinePrefixWriterHandlesSplitLines(t *testing.T) {
	var output bytes.Buffer
	w := &linePrefixWriter{out: &output, prefix: "api | "}
	for _, part := range []string{"one\ntw", "o\n", "three"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if got := output.String(); got != "api | one\napi | two\napi | three" {
		t.Fatalf("prefixed output = %q", got)
	}
}
