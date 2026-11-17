package common

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestEventLogTimestampsEveryLine(t *testing.T) {
	var output bytes.Buffer
	log := NewEventLog(&output)
	log.Printf("starting %s", "api")
	for _, part := range []string{"first\nsec", "ond\n"} {
		if n, err := log.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	stamp := regexp.MustCompile(`^\[\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d[^\]]*\] `)
	want := []string{"starting api", "first", "second"}
	if len(lines) != len(want) {
		t.Fatalf("event log = %q", output.String())
	}
	for index, line := range lines {
		if !stamp.MatchString(line) || stamp.ReplaceAllString(line, "") != want[index] {
			t.Errorf("line %d = %q, want a timestamp and %q", index, line, want[index])
		}
	}
}

func TestOpenEventLogFallsBackWhenFileCannotOpen(t *testing.T) {
	directory := t.TempDir()
	blocker := filepath.Join(directory, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var fallback bytes.Buffer
	log := OpenEventLog(filepath.Join(blocker, "spm.log"), &fallback)
	log.Printf("lost?")
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fallback.String(), "lost?") {
		t.Fatalf("fallback = %q", fallback.String())
	}
	path := filepath.Join(directory, "service", EventLogName)
	log = OpenEventLog(path, nil)
	log.Printf("kept")
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "kept") {
		t.Fatalf("event log file = %q, %v", data, err)
	}
}
