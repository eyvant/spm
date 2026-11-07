package app

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCompletionCandidatesUseCommandContextAndManagedNames(t *testing.T) {
	directory := localTempDir(t)
	for _, name := range []string{"api", "worker"} {
		if err := os.WriteFile(filepath.Join(directory, unitName(name)), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := &application{unitDir: directory}
	tests := []struct {
		args []string
		want []string
	}{
		{args: []string{"st"}, want: []string{"start", "startup", "status", "status-all", "stop"}},
		{args: []string{"l"}, want: []string{"list", "logs", "ls"}},
		{args: []string{"restart", ""}, want: []string{"all", "api", "worker"}},
		{args: []string{"start", "--restart", "on-"}, want: []string{"on-abnormal", "on-abort", "on-failure", "on-success", "on-watchdog"}},
		{args: []string{"logs", "api", "--f"}, want: []string{"--follow"}},
		{args: []string{"logs", "api", "-f"}, want: []string{"-f"}},
		{args: []string{"completion", ""}, want: []string{"bash", "fish", "powershell", "zsh"}},
	}
	for _, test := range tests {
		got := a.completionCandidates(test.args)
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("completionCandidates(%v) = %v, want %v", test.args, got, test.want)
		}
	}
	shortOptions := a.completionCandidates([]string{"start", "-"})
	for _, want := range []string{"-n", "-e", "-f", "-h"} {
		if !containsCompletion(shortOptions, want) {
			t.Errorf("start short options %v do not contain %q", shortOptions, want)
		}
	}
}

func containsCompletion(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCompletionScriptsUseDynamicQuery(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var output bytes.Buffer
		if err := printCompletion(&output, []string{shell}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "spm __complete") {
			t.Errorf("%s completion does not use dynamic query:\n%s", shell, output.String())
		}
	}
}

func TestCompletionGenerationWorksBeforeLinuxRuntimeCheck(t *testing.T) {
	var output bytes.Buffer
	if err := Run([]string{"completion", "bash"}, &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "complete") {
		t.Fatalf("unexpected completion output: %q", output.String())
	}
}

func TestCompletionCandidatesCoverEveryCommandFamily(t *testing.T) {
	directory := localTempDir(t)
	for _, name := range []string{"api", "worker"} {
		if err := os.WriteFile(filepath.Join(directory, unitName(name)), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, templateUnitName("scaled")), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &application{unitDir: directory}
	queries := [][]string{
		{"--system"}, {"help", "st"}, {"start", ""}, {"start", "script", ""}, {"schedule", ""},
		{"run", ""}, {"status", ""}, {"show", ""}, {"describe", ""}, {"cat", ""}, {"edit", ""},
		{"security", ""}, {"health", ""}, {"history", ""}, {"logs", ""}, {"signal", "si"}, {"signal", "SIGTERM", ""},
		{"kill", ""}, {"scale", ""}, {"scale", "scaled", ""}, {"restart", ""}, {"stop", ""}, {"reload", ""},
		{"enable", ""}, {"disable", ""}, {"delete", ""}, {"remove", ""}, {"rm", ""}, {"set", ""},
		{"set", "api", ""}, {"unset", ""}, {"rollback", "api", ""}, {"clone", ""}, {"clone", "api", ""},
		{"export", ""}, {"completion", "p"}, {"apply", "--"}, {"import", "--"},
		{"start", "--health-action", "r"}, {"start", "--sandbox", "s"}, {"start", "--kill-mode", "m"},
		{"start", "--type", "n"}, {"start", "--timezone", "A"}, {"logs", "--output", "j"},
	}
	for _, query := range queries {
		_ = a.completionCandidates(query)
	}
	for _, command := range completionCommands {
		if len(completionOptions(command)) == 0 {
			t.Errorf("completionOptions(%q) returned no fallback", command)
		}
	}
	var output bytes.Buffer
	if err := runCompletionQuery([]string{"list", ""}, &output); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"bash", "extra"}, {"unknown"}} {
		if err := printCompletion(&output, args); err == nil {
			t.Errorf("printCompletion(%v) unexpectedly succeeded", args)
		}
	}
}
