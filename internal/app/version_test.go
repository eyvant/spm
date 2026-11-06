package app

import (
	"bytes"
	"testing"
)

func TestVersionOutputIncludesShortCommit(t *testing.T) {
	oldVersion, oldCommit := version, commit
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })
	version, commit = "1.2.3", "0123456789abcdef"

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"-v"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "spm 1.2.3 (0123456)\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestVersionOutputOmitsUnknownCommit(t *testing.T) {
	oldVersion, oldCommit := version, commit
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })
	version, commit = "dev", ""

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "spm dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}
