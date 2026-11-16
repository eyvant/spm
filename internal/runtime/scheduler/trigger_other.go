//go:build !windows && !darwin

package scheduler

import "errors"

// Linux schedules jobs with systemd timers and needs no scheduler daemon.
func platformTrigger(string) (func(string) error, error) {
	return nil, errors.New("the spm scheduler daemon runs only on Windows and macOS")
}
