package common

import (
	"os"
	"testing"
)

func localTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(".", ".spm-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}
