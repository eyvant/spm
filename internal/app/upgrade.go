package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

// runUpgradeRuntime moves every long-running process that executes the spm
// binary itself onto the binary that is installed now. Installers call it
// right after they replace the executable, so an upgrade never leaves the old
// version running in the background.
//
// Linux has nothing to restart: systemd runs service commands directly and
// spm only appears in short-lived health-check units, which pick up the new
// binary on their next run.
func runUpgradeRuntime(args []string, stdout, stderr io.Writer, system bool) (err error) {
	report := ""
	switch {
	case len(args) == 0:
	case len(args) == 2 && args[0] == "--report" && args[1] != "":
		report = args[1]
	default:
		return errors.New("usage: spm [--system] __upgrade-runtime [--report FILE]")
	}
	if report != "" {
		// An elevated child has no console the installer can read, so it
		// writes everything, including the final error, to the report file.
		file, openErr := os.OpenFile(report, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			return openErr
		}
		defer func() {
			if err != nil {
				fmt.Fprintf(file, "error: %v\n", err)
			}
			_ = file.Close()
		}()
		stdout, stderr = file, file
	}
	switch runtime.GOOS {
	case "windows":
		return runWindows([]string{"__upgrade-runtime"}, stdout, stderr, system)
	case "darwin":
		return runDarwin([]string{"__upgrade-runtime"}, stdout, stderr, system)
	default:
		return nil
	}
}
