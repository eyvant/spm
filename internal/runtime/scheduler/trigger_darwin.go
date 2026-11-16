//go:build darwin

package scheduler

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// platformTrigger kickstarts a job's launchd job in the given domain.
func platformTrigger(domain string) (func(string) error, error) {
	if domain == "" {
		return nil, errors.New("scheduler daemon requires --domain on macOS")
	}
	return func(name string) error {
		command := exec.Command("launchctl", "kickstart", domain+"/com.eyvant.spm."+name)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
		}
		return nil
	}, nil
}
