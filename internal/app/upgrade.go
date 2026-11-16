package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

// upgradeComponents selects which runtimes an upgrade moved to a new version.
type upgradeComponents struct {
	supervisor bool // spm-supervisor: restart supervised services
	scheduler  bool // spm-scheduler: rewrite job launchers, restart the daemon
}

func (c upgradeComponents) flags() []string {
	var flags []string
	if c.supervisor {
		flags = append(flags, "--supervisor")
	}
	if c.scheduler {
		flags = append(flags, "--scheduler")
	}
	return flags
}

func parseUpgradeComponents(args []string) (upgradeComponents, error) {
	var components upgradeComponents
	for _, arg := range args {
		switch arg {
		case "--supervisor":
			components.supervisor = true
		case "--scheduler":
			components.scheduler = true
		default:
			return components, fmt.Errorf("unknown upgrade component %q", arg)
		}
	}
	if !components.supervisor && !components.scheduler {
		components = upgradeComponents{supervisor: true, scheduler: true}
	}
	return components, nil
}

// runUpgradeRuntime moves running processes onto newly installed runtimes.
// Installers call it after replacing spm-supervisor or spm-scheduler, naming
// the runtimes whose version changed:
//
//	spm [--system] __upgrade-runtime [--supervisor] [--scheduler] [--report FILE]
//
// With neither component named, both are refreshed. Upgrading only the spm
// CLI needs no call at all: no background process runs the CLI.
//
// Linux has nothing to restart: systemd runs service commands directly and
// schedules jobs with timers.
func runUpgradeRuntime(args []string, stdout, stderr io.Writer, system bool) (err error) {
	report := ""
	var rest []string
	for index := 0; index < len(args); index++ {
		if args[index] == "--report" {
			if index+1 >= len(args) || args[index+1] == "" {
				return errors.New("usage: spm [--system] __upgrade-runtime [--supervisor] [--scheduler] [--report FILE]")
			}
			report = args[index+1]
			index++
			continue
		}
		rest = append(rest, args[index])
	}
	components, err := parseUpgradeComponents(rest)
	if err != nil {
		return err
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
	command := append([]string{"__upgrade-runtime"}, components.flags()...)
	switch runtime.GOOS {
	case "windows":
		return runWindows(command, stdout, stderr, system)
	case "darwin":
		return runDarwin(command, stdout, stderr, system)
	default:
		return nil
	}
}
