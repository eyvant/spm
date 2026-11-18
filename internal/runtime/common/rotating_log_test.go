package common

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestRotatingLogRotatesDuringWritesAndRetainsBackups(t *testing.T) {
	path := filepath.Join(localTempDir(t), "service.log")
	log, err := OpenRotatingLog(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0000000000", "1111111111", "2222222222", "3333333333"} {
		if _, err := log.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		path:        "3333333333",
		path + ".1": "2222222222",
		path + ".2": "1111111111",
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", file, data, want)
		}
	}
}

func TestRotatingLogSplitsSingleLargeWrite(t *testing.T) {
	path := filepath.Join(localTempDir(t), "log")
	log, err := OpenRotatingLog(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := log.Write([]byte("abcdefghijkl")); err != nil || count != 12 {
		t.Fatalf("Write = %d, %v", count, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		path:        "kl",
		path + ".1": "fghij",
		path + ".2": "abcde",
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", file, data, want)
		}
	}
}

func TestRotatingLogRotatesOversizedFileWhenOpened(t *testing.T) {
	path := filepath.Join(localTempDir(t), "service.log")
	if err := os.WriteFile(path, []byte("oversized"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := OpenRotatingLog(path, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path + ".1"); err != nil || string(data) != "oversized" {
		t.Fatalf("rotated file = %q, %v", data, err)
	}
}

func TestPrintRotatedLogTailReadsBackupsInOrder(t *testing.T) {
	path := filepath.Join(localTempDir(t), "service.log")
	for file, content := range map[string]string{
		path + ".2": "one\n",
		path + ".1": "two\n",
		path:        "three\n",
	} {
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := PrintRotatedLogTail(&output, path, 2, nil); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "two\nthree\n" {
		t.Fatalf("tail = %q", got)
	}
}

func TestReadRotatingLogUpdatesFollowsReplacementFile(t *testing.T) {
	path := filepath.Join(localTempDir(t), "service.log")
	log, err := OpenRotatingLog(path, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("old\n")); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	current, offset, err := ReadRotatingLogUpdates(&output, path, previous, previous.Size(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "new\n" {
		t.Fatalf("updates = %q", got)
	}
	if current == nil || offset != current.Size() {
		t.Fatalf("current = %#v, offset = %d", current, offset)
	}
}

func TestRotatingLogValidationAndEdgePaths(t *testing.T) {
	path := filepath.Join(localTempDir(t), "edge.log")
	for _, limits := range [][2]int64{{0, 1}, {1, -1}} {
		if _, err := OpenRotatingLog(path, limits[0], int(limits[1])); err == nil {
			t.Errorf("OpenRotatingLog limits %v unexpectedly succeeded", limits)
		}
	}
	if err := os.WriteFile(path, []byte("discard"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateLogFiles(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("zero-backup rotation left file: %v", err)
	}

	var output bytes.Buffer
	previous, offset, err := ReadRotatingLogUpdates(&output, path, nil, 7, nil)
	if err != nil || previous != nil || offset != 7 {
		t.Fatalf("missing update = %#v, %d, %v", previous, offset, err)
	}
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	current, next, err := ReadRotatingLogUpdates(&output, path, info, info.Size()+10, regexp.MustCompile("two"))
	if err != nil || current == nil || next != info.Size() || output.String() != "two\n" {
		t.Fatalf("truncated update = %#v, %d, %q, %v", current, next, output.String(), err)
	}
	directory := localTempDir(t)
	if err := PrintRotatedLogTail(&output, directory, 1, nil); err == nil {
		t.Fatal("tailing a directory unexpectedly succeeded")
	}
	if _, err := printLogFromOffset(&output, directory, 0, nil); err == nil {
		t.Fatal("reading a directory unexpectedly succeeded")
	}
	log := &RotatingLog{}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompileLogGrepMatchesLikeJournalctl(t *testing.T) {
	lines := []string{"[ok] signals=21 errors=0", "WARN disk low", "Error: timeout", "plain line"}
	for _, test := range []struct {
		pattern string
		want    []string
	}{
		{"", lines},
		{"error|warn", []string{"[ok] signals=21 errors=0", "WARN disk low", "Error: timeout"}},
		{"ERRORS", nil},
		{"Error", []string{"Error: timeout"}},
		{`errors=\d`, []string{"[ok] signals=21 errors=0"}},
	} {
		grep, err := CompileLogGrep(test.pattern)
		if err != nil {
			t.Fatalf("CompileLogGrep(%q): %v", test.pattern, err)
		}
		var got []string
		for _, line := range lines {
			if grep == nil || grep.MatchString(line) {
				got = append(got, line)
			}
		}
		if len(got) != len(test.want) {
			t.Fatalf("pattern %q matched %q, want %q", test.pattern, got, test.want)
		}
		for index := range got {
			if got[index] != test.want[index] {
				t.Fatalf("pattern %q matched %q, want %q", test.pattern, got, test.want)
			}
		}
	}
	if _, err := CompileLogGrep("error("); err == nil {
		t.Fatal("invalid pattern compiled without an error")
	}
}

func TestPrintRotatedLogTailFiltersWithRegex(t *testing.T) {
	path := filepath.Join(localTempDir(t), "service.log")
	if err := os.WriteFile(path, []byte("start\nERROR one\nok\nwarn two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	grep, err := CompileLogGrep("error|warn")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := PrintRotatedLogTail(&output, path, 10, grep); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "ERROR one\nwarn two\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
