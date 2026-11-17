//go:build windows

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/eyvant/spm/internal/runtime/common"
)

// Main is the spm-supervisor entry point:
//
//	spm-supervisor version
//	spm-supervisor supervise --root ROOT NAME   (user service)
//	spm-supervisor service --root ROOT NAME     (system service under the SCM)
//	spm-supervisor hold-output --root ROOT NAME --handle HANDLE --pid PID
//
// hold-output is started only by a detaching supervisor (see
// spawnOutputHolder).
func Main(args []string, stdout, _ io.Writer) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, common.VersionLine("spm-supervisor"))
		return nil
	}
	if len(args) == 8 && args[0] == "hold-output" && args[1] == "--root" && filepath.IsAbs(args[2]) && args[4] == "--handle" && args[6] == "--pid" {
		handle, handleErr := strconv.ParseUint(args[5], 10, 64)
		pid, pidErr := strconv.Atoi(args[7])
		if handleErr != nil || pidErr != nil || pid <= 0 {
			return errors.New("invalid hold-output arguments")
		}
		return HoldOutput(Layout{Root: filepath.Clean(args[2])}, args[3], handle, pid)
	}
	if len(args) != 4 || args[1] != "--root" || !filepath.IsAbs(args[2]) {
		return errors.New("usage: spm-supervisor version | supervise --root ROOT NAME | service --root ROOT NAME")
	}
	layout := Layout{Root: filepath.Clean(args[2]), Global: args[0] == "service"}
	switch args[0] {
	case "supervise":
		return Supervise(layout, args[3])
	case "service":
		return RunService(layout, args[3])
	default:
		return fmt.Errorf("unknown spm-supervisor command %q", args[0])
	}
}
