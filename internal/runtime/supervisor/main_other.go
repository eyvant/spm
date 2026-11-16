//go:build !windows

package supervisor

import (
	"errors"
	"fmt"
	"io"
)

// Main reports the version; supervision itself exists only on Windows.
func Main(args []string, stdout, _ io.Writer) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "spm-supervisor %d\n", Version)
		return nil
	}
	return errors.New("spm-supervisor runs only on Windows; systemd and launchd supervise services on this platform")
}
