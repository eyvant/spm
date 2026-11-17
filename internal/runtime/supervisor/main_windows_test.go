//go:build windows

package supervisor

import (
	"os"
	"testing"
)

// A supervisor detaching inside a test starts its output holder from the
// test binary (os.Executable), so the test binary must act as one.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "hold-output" {
		if err := Main(os.Args[1:], os.Stdout, os.Stderr); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
