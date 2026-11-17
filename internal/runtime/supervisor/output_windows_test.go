//go:build windows

package supervisor

import (
	"bytes"
	"strings"
	"testing"
)

// A process that prints and exits at once must not lose its output: the
// pipe can still look empty for a moment after the exit is reported.
func TestOutputOfShortLivedProcessIsKept(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	config := Config{Command: []string{"cmd.exe", "/d", "/c", "echo service-output& exit 0"}, Cwd: l.Root}
	for attempt := 0; attempt < 5; attempt++ {
		started, _, err := startChild(l, "short", config)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		started.startPump(&output)
		<-started.done
		started.close()
		if strings.TrimSpace(output.String()) != "service-output" {
			t.Fatalf("attempt %d: output = %q", attempt, output.String())
		}
	}
}

// Stopping the copy leaves nothing in flight, and copying resumes where it
// stopped, as a supervisor handoff requires.
func TestOutputPumpStopsAndResumes(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	config := Config{Command: []string{"cmd.exe", "/d", "/c", "for /l %i in (1,1,40) do @echo line %i& ping -n 1 -w 10 127.0.0.1 >nul"}, Cwd: l.Root}
	started, _, err := startChild(l, "lines", config)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	started.startPump(&output)
	started.stopPump()
	started.startPump(nil)
	<-started.done
	started.close()
	for _, want := range []string{"line 1\r\n", "line 20\r\n", "line 40\r\n"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output is missing %q:\n%s", want, output.String())
		}
	}
	if got := strings.Count(output.String(), "line "); got != 40 {
		t.Fatalf("got %d lines, want 40:\n%s", got, output.String())
	}
}
