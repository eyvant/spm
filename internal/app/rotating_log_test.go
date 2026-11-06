package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingLogRotatesDuringWritesAndRetainsBackups(t *testing.T) {
	path := filepath.Join(localTempDir(t), "service.log")
	log, err := openRotatingLog(path, 10, 2)
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
	path := filepath.Join(localTempDir(t), "scheduler.log")
	log, err := openRotatingLog(path, 5, 2)
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
	log, err := openRotatingLog(path, 5, 1)
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
	if err := printRotatedLogTail(&output, path, 2, ""); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "two\nthree\n" {
		t.Fatalf("tail = %q", got)
	}
}

func TestReadRotatingLogUpdatesFollowsReplacementFile(t *testing.T) {
	path := filepath.Join(localTempDir(t), "service.log")
	log, err := openRotatingLog(path, 4, 2)
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
	current, offset, err := readRotatingLogUpdates(&output, path, previous, previous.Size(), "")
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
