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
		{args: []string{"completion", ""}, want: []string{"bash", "fish", "zsh"}},
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
	for _, shell := range []string{"bash", "zsh", "fish"} {
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
