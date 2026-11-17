//go:build !windows && !darwin

package supervisor

import (
	"errors"
	"fmt"
	"io"

	"github.com/eyvant/spm/internal/runtime/common"
)

// Main reports the version; supervision exists only on Windows and macOS.
func Main(args []string, stdout, _ io.Writer) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, common.VersionLine("spm-supervisor"))
		return nil
	}
	return errors.New("spm-supervisor runs only on Windows and macOS; systemd supervises services on this platform")
}
