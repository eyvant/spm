package scheduler

import (
	"os"
	"testing"
)

// localTempDir creates a directory below the working directory, which keeps
// executables started by tests on the same volume as the package.
func localTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(".", ".spm-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}
