//go:build windows

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

// Main is the spm-supervisor entry point:
//
//	spm-supervisor version
//	spm-supervisor supervise --root ROOT NAME   (user service)
//	spm-supervisor service --root ROOT NAME     (system service under the SCM)
func Main(args []string, stdout, _ io.Writer) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "spm-supervisor %d\n", Version)
		return nil
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
