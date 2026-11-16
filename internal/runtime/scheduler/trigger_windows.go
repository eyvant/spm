//go:build windows

package scheduler

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// platformTrigger starts a job's on-demand Task Scheduler task.
func platformTrigger(string) (func(string) error, error) {
	return func(name string) error {
		command := exec.Command("schtasks.exe", "/Run", "/TN", "spm-"+name+"-schedule")
		command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
		}
		return nil
	}, nil
}
