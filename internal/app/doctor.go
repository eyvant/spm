package app

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	colorBoldGreen = "\x1b[1;32m"
	colorBoldRed   = "\x1b[1;31m"
)

type doctorCheck struct {
	level  string
	name   string
	detail string
}

// userNamespacesRestricted reports whether unprivileged user namespaces are
// disabled, which breaks namespace based sandboxing in user services.
func userNamespacesRestricted() (bool, string) {
	if value, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil && strings.TrimSpace(string(value)) == "1" {
		return true, "kernel.apparmor_restrict_unprivileged_userns=1"
	}
	if value, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil && strings.TrimSpace(string(value)) == "0" {
		return true, "kernel.unprivileged_userns_clone=0"
	}
	return false, ""
}

func parseSystemdVersion(output string) int {
	fields := strings.Fields(output)
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0
	}
	version, _ := strconv.Atoi(fields[1])
	return version
}

func (a *application) doctor() error {
	var checks []doctorCheck
	add := func(level, name, format string, args ...any) {
		checks = append(checks, doctorCheck{level: level, name: name, detail: fmt.Sprintf(format, args...)})
	}
	manager := "user manager"
	if a.system {
		manager = "system manager"
	}

	output, err := a.runSystemctl("is-system-running")
	state := strings.TrimSpace(string(output))
	switch {
	case state == "running":
		add("ok", manager, "running")
	case state == "degraded":
		add("warn", manager, "degraded; run `systemctl %s` to see failed units", strings.Join(a.systemctlArgs("--failed"), " "))
	case state != "" && state != "offline" && state != "unknown":
		add("warn", manager, "%s", state)
	default:
		add("fail", manager, "not reachable: %v", err)
	}

	if output, err := a.runner.Run("systemctl", "--version"); err == nil {
		version := parseSystemdVersion(string(output))
		switch {
		case version == 0:
			add("warn", "systemd version", "cannot parse %q", strings.SplitN(string(output), "\n", 2)[0])
		case version < 245:
			add("warn", "systemd version", "%d; --timezone needs 245+, --restart-max-delay needs 254+", version)
		case version < 254:
			add("warn", "systemd version", "%d; --restart-max-delay and --restart-steps need 254+", version)
		default:
			add("ok", "systemd version", "%d", version)
		}
	} else {
		add("fail", "systemd version", "%v", err)
	}

	if !a.system {
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			add("warn", "XDG_RUNTIME_DIR", "not set; systemctl --user may not find the user bus, try XDG_RUNTIME_DIR=/run/user/%d", os.Getuid())
		} else {
			add("ok", "XDG_RUNTIME_DIR", "%s", os.Getenv("XDG_RUNTIME_DIR"))
		}
		if currentUser, err := user.Current(); err != nil {
			add("warn", "lingering", "cannot resolve current user: %v", err)
		} else if enabled, err := a.lingerEnabled(currentUser.Username); err != nil {
			add("warn", "lingering", "cannot read linger state: %v", err)
		} else if enabled {
			add("ok", "lingering", "enabled for %s", currentUser.Username)
		} else {
			add("warn", "lingering", "disabled; services stop after logout and do not start at boot, run `spm startup`")
		}
	}

	if info, err := os.Stat("/var/log/journal"); err == nil && info.IsDir() {
		add("ok", "journal storage", "persistent (/var/log/journal)")
	} else {
		add("warn", "journal storage", "volatile; logs are lost on reboot and user journals may be stored in the system journal, create /var/log/journal to persist them")
	}
	journalArgs := []string{"--lines", "0", "--no-pager", "--quiet"}
	if !a.system {
		journalArgs = append([]string{"--user"}, journalArgs...)
	}
	if _, err := a.runner.Run("journalctl", journalArgs...); err != nil {
		add("warn", "journal access", "%v", err)
	} else {
		add("ok", "journal access", "readable")
	}

	if err := os.MkdirAll(a.unitDir, 0o755); err != nil {
		add("fail", "unit directory", "%s: %v", a.unitDir, err)
	} else if probe, err := os.CreateTemp(a.unitDir, ".spm-doctor-*"); err != nil {
		add("fail", "unit directory", "%s is not writable: %v", a.unitDir, err)
	} else {
		probe.Close()
		_ = os.Remove(probe.Name())
		add("ok", "unit directory", "%s", a.unitDir)
	}

	mise, err := resolveExecutable("mise")
	switch {
	case err != nil:
		add("info", "mise", "not found in PATH; only needed for --mise")
	case isSnapMise(mise):
		if executable, snapErr := resolveSnapMise(mise); snapErr != nil {
			add("fail", "mise", "Snap-installed mise cannot run outside snap run: %v", snapErr)
		} else {
			add("ok", "mise", "Snap-installed, units use %s", executable)
		}
	default:
		add("ok", "mise", "%s", mise)
	}

	if !a.system {
		if restricted, reason := userNamespacesRestricted(); restricted {
			add("warn", "user namespaces", "%s; --sandbox in user mode may fail, use spm --system for sandboxed services", reason)
		} else {
			add("ok", "user namespaces", "available for --sandbox")
		}
	}

	if executable, err := os.Executable(); err != nil {
		add("warn", "spm executable", "%v", err)
	} else if temp := os.TempDir(); strings.HasPrefix(executable, temp+string(filepath.Separator)) || strings.Contains(executable, "go-build") {
		add("warn", "spm executable", "%s is temporary; health check units call this path, install spm to a stable location", executable)
	} else {
		add("ok", "spm executable", "%s", executable)
	}

	failed := 0
	for _, check := range checks {
		label := fmt.Sprintf("%-4s", strings.ToUpper(check.level))
		color := ""
		switch check.level {
		case "ok":
			color = colorBoldGreen
		case "warn":
			color = colorBoldYellow
		case "fail":
			color = colorBoldRed
			failed++
		case "info":
			color = colorBoldCyan
		}
		if supportsColor(a.stdout) {
			label = color + label + colorReset
		}
		fmt.Fprintf(a.stdout, "%s  %s: %s\n", label, check.name, check.detail)
	}
	if failed > 0 {
		return errors.New("spm doctor found problems that block normal operation")
	}
	return nil
}
