package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const startSettleDelay = time.Second

type startOptions struct {
	name               string
	cwd                string
	description        string
	restart            string
	restartSec         string
	startLimitInterval string
	startLimitBurst    string
	cronRestart        string
	cronCalendar       string
	snapMise           bool
	env                []string
	envFile            string
	interpreter        string
	mise               bool
	force              bool
	enable             bool
	command            []string
	explicitCmd        bool
	maxMemory          string
	runAsUser          string
	runAsGroup         string
	executablePath     string
	properties         []string
	execStartPre       []string
	execStartPost      []string
	execStop           string
	execReload         string
	after              []string
	wants              []string
	requires           []string
	onFailure          []string
	set                map[string]bool
	wait               bool
	waitTimeout        time.Duration
	waitProbe          probeConfig
	health             healthConfig
	serviceType        string
	pidFile            string
	timezone           string
	instances          int
	sandbox            string
}

func defaultStartOptions() startOptions {
	return startOptions{restart: "always", restartSec: "3s", enable: true, set: make(map[string]bool)}
}

func (a *application) prepareRunAsUser(options *startOptions) error {
	if !a.system && (options.runAsUser != "" || options.set["--user"] || options.runAsGroup != "" || options.set["--group"]) {
		return errors.New("--user and --group are only supported with global --system mode")
	}
	if options.runAsUser == "" {
		return nil
	}
	resolveHome := a.userHome
	if resolveHome == nil {
		resolveHome = systemUserHome
	}
	home, err := resolveHome(options.runAsUser)
	if err != nil {
		return fmt.Errorf("resolve system user %q: %w", options.runAsUser, err)
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("system user %q has non-absolute home directory %q", options.runAsUser, home)
	}
	options.executablePath = executableSearchPath(home)
	return nil
}

func systemUserHome(identity string) (string, error) {
	var account *user.User
	var err error
	if isNumericIdentity(identity) {
		account, err = user.LookupId(identity)
	} else {
		account, err = user.Lookup(identity)
	}
	if err != nil {
		return "", err
	}
	if account.HomeDir == "" {
		return "", errors.New("user database returned an empty home directory")
	}
	return account.HomeDir, nil
}

func executableSearchPath(home string) string {
	directories := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".cargo", "bin"),
		filepath.Join(home, ".local", "share", "mise", "bin"),
		filepath.Join(home, ".local", "share", "mise", "shims"),
	}
	if runtime.GOOS == "windows" {
		return strings.Join(append(directories, filepath.SplitList(os.Getenv("PATH"))...), string(os.PathListSeparator))
	}
	return strings.Join(append(directories, "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin", "/snap/bin", "/var/lib/snapd/snap/bin"), string(os.PathListSeparator))
}

func isNumericIdentity(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (a *application) start(args []string) error {
	options, err := parseStart(args)
	if err != nil {
		return err
	}
	if len(options.command) == 1 && options.name == "" && !options.explicitCmd && a.exists(options.command[0]) {
		return a.control("start", options.command[0])
	}
	if options.name == "" {
		options.name = inferName(options.command, options.explicitCmd)
	}
	if options.force && a.exists(options.name) {
		if err := a.inheritStartOptions(&options); err != nil {
			return err
		}
	}
	if options.set["--timezone"] && options.timezone != "" && options.cronRestart == "" {
		return errors.New("--timezone requires --cron-restart; use `spm schedule --cron ... --timezone ...` for scheduled tasks")
	}
	if err := a.prepareRunAsUser(&options); err != nil {
		return err
	}
	if err := options.prepare(); err != nil {
		return err
	}
	a.warnSandboxSupport(options)
	savedConfig := persistedConfigFrom(options)
	return a.installPrepared(options, savedConfig)
}

func (a *application) warnSandboxSupport(options startOptions) {
	if options.sandbox == "" || a.system {
		return
	}
	if restricted, reason := userNamespacesRestricted(); restricted {
		a.warn("--sandbox in user mode needs unprivileged user namespaces, but %s; the service may fail to start. Use `spm --system` or run `spm doctor`.", reason)
	}
}

// installInstances installs a template unit spm-NAME@.service and runs
// instances 0..N-1. It also converts between plain and template services.
func (a *application) installInstances(options startOptions, savedConfig persistedStartConfig) (returnErr error) {
	name := options.name
	replacing := a.exists(name)
	if replacing && !options.force {
		return fmt.Errorf("service %q already exists; use --force to replace it", name)
	}
	if options.instances > 0 && (options.cronRestart != "" || options.health.Probe.Kind != "" || options.waitProbe.Kind != "") {
		return errors.New("--instances cannot be combined with --cron-restart, health checks, or readiness probes")
	}
	var oldUnits []string
	if a.isTemplate(name) {
		oldUnits = a.serviceUnits(name)
	} else if replacing {
		if a.hasSchedule(name) || a.hasCronRestart(name) || a.hasHealth(name) {
			return fmt.Errorf("service %q has timers or health checks; delete it before switching to --instances", name)
		}
		oldUnits = []string{unitName(name)}
	}
	oldStates := make(map[string]activationState, len(oldUnits))
	for _, unit := range oldUnits {
		oldStates[unit] = a.readActivationState(unit)
	}
	var template string
	if options.instances > 0 {
		var err error
		template, err = renderUnit(options, a.system)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(a.unitDir, 0o755); err != nil {
		return fmt.Errorf("create systemd unit directory: %w", err)
	}
	backups, err := snapshotFiles([]string{a.unitPath(name), a.templatePath(name), a.serviceConfigPath(name)})
	if err != nil {
		return fmt.Errorf("back up existing unit files: %w", err)
	}
	newUnits := instanceUnits(name, options.instances)
	changed := false
	succeeded := false
	defer func() {
		if succeeded || !changed {
			return
		}
		if len(newUnits) > 0 {
			_, _ = a.runSystemctl(append([]string{"disable", "--now"}, newUnits...)...)
		}
		if err := restoreFiles(backups); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore previous unit files: %w", err))
		}
		if _, err := a.runSystemctl("daemon-reload"); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("reload restored systemd units: %w", err))
		}
		for _, unit := range oldUnits {
			if err := a.restoreActivationState(unit, oldStates[unit]); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore %s state: %w", unit, err))
			}
		}
	}()
	if len(oldUnits) > 0 {
		changed = true
		if _, err := a.runSystemctl(append([]string{"disable", "--now"}, oldUnits...)...); err != nil {
			return fmt.Errorf("stop previous units of %s: %w", name, err)
		}
	}
	if options.instances == 0 {
		if err := os.Remove(a.templatePath(name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove template unit: %w", err)
		}
		changed = true
		if _, err := a.runSystemctl("daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd manager: %w", err)
		}
		if err := a.installPrepared(options, savedConfig); err != nil {
			return err
		}
		succeeded = true
		return nil
	}
	if err := os.Remove(a.unitPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove previous unit file: %w", err)
	}
	if err := writeFileAtomic(a.templatePath(name), []byte(template), 0o644); err != nil {
		return fmt.Errorf("write template unit file: %w", err)
	}
	changed = true
	if _, err := a.runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd manager: %w", err)
	}
	action := []string{"start"}
	if options.enable {
		action = []string{"enable", "--now"}
	}
	if _, err := a.runSystemctl(append(action, newUnits...)...); err != nil {
		return fmt.Errorf("start instances of %s: %w", name, err)
	}
	for _, unit := range newUnits {
		if err := a.checkUnitStarted(unit, options.restart); err != nil {
			return err
		}
		if options.wait {
			if err := a.waitForUnitActive(unit, options.waitTimeout); err != nil {
				return err
			}
		}
	}
	if err := a.writeServiceConfig(name, savedConfig); err != nil {
		return err
	}
	succeeded = true
	fmt.Fprintf(a.stdout, "%s started as %s with %d instances\n", name, templateUnitName(name), options.instances)
	a.printStartNotices(options)
	return nil
}

func (a *application) printStartNotices(options startOptions) {
	if options.snapMise {
		a.warn("Snap-installed mise detected, running %s directly to keep the service in its own cgroup.", options.command[0])
	}
	if options.enable && !a.system {
		if currentUser, userErr := user.Current(); userErr == nil {
			if enabled, lingerErr := a.lingerEnabled(currentUser.Username); lingerErr == nil && !enabled {
				a.hint("Run `spm startup` once if this user must start services at boot without logging in.")
			}
		}
	}
}

func (a *application) installPrepared(options startOptions, savedConfig persistedStartConfig) (returnErr error) {
	if !a.system && (options.runAsUser != "" || options.set["--user"] || options.runAsGroup != "" || options.set["--group"]) {
		return errors.New("--user and --group are only supported with global --system mode")
	}
	if options.instances > 0 || a.isTemplate(options.name) {
		return a.installInstances(options, savedConfig)
	}
	replacing := a.exists(options.name)
	if replacing && !options.force {
		return fmt.Errorf("service %q already exists; use --force to replace it", options.name)
	}
	unit, err := renderUnit(options, a.system)
	if err != nil {
		return err
	}
	var cronService, cronTimer string
	if options.cronRestart != "" {
		if _, err := a.runner.Run("systemd-analyze", "calendar", options.cronCalendar); err != nil {
			return fmt.Errorf("validate cron restart calendar %q: %w", options.cronCalendar, err)
		}
		systemctl, resolveErr := resolveExecutable("systemctl")
		if resolveErr != nil {
			return fmt.Errorf("resolve systemctl for cron restart: %w", resolveErr)
		}
		cronService, cronTimer = renderCronRestartUnits(options.name, options.cronRestart, options.timezone, options.cronCalendar, systemctl, a.system)
	}
	healthService, healthTimer, healthRecovery, err := renderHealthUnits(options, a.system)
	if err != nil {
		return fmt.Errorf("render health units: %w", err)
	}
	if err := os.MkdirAll(a.unitDir, 0o755); err != nil {
		return fmt.Errorf("create systemd unit directory: %w", err)
	}
	paths := []string{a.unitPath(options.name), a.cronRestartServicePath(options.name), a.cronRestartTimerPath(options.name), a.scheduleTimerPath(options.name), a.serviceConfigPath(options.name)}
	paths = append(paths, a.healthPaths(options.name)...)
	backups, err := snapshotFiles(paths)
	if err != nil {
		return fmt.Errorf("back up existing unit files: %w", err)
	}
	hadCronRestart := a.hasCronRestart(options.name)
	hadHealth := a.hasHealth(options.name)
	oldUnitState := activationState{}
	oldTimerState := activationState{}
	oldHealthState := activationState{}
	if replacing {
		oldUnitState = a.readActivationState(unitName(options.name))
	}
	if hadCronRestart {
		oldTimerState = a.readActivationState(cronRestartTimerName(options.name))
	}
	if hadHealth {
		oldHealthState = a.readActivationState(healthTimerName(options.name))
	}
	changed := false
	succeeded := false
	defer func() {
		if succeeded || !changed {
			return
		}
		if !replacing {
			// A new service has no previous state to restore, so it is
			// disabled before its files disappear to avoid dangling wants
			// links and a failed not-found unit.
			newUnits := []string{unitName(options.name)}
			if options.cronRestart != "" {
				newUnits = append(newUnits, cronRestartTimerName(options.name))
			}
			if options.health.Probe.Kind != "" {
				newUnits = append(newUnits, healthTimerName(options.name))
			}
			_, _ = a.runSystemctl(append([]string{"disable", "--now"}, newUnits...)...)
		}
		if err := restoreFiles(backups); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore previous unit files: %w", err))
		}
		if _, err := a.runSystemctl("daemon-reload"); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("reload restored systemd units: %w", err))
		}
		if !replacing {
			_, _ = a.runSystemctl("reset-failed", unitName(options.name))
		}
		if err := a.restoreActivationState(unitName(options.name), oldUnitState); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore service state: %w", err))
		}
		if err := a.restoreActivationState(cronRestartTimerName(options.name), oldTimerState); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore cron timer state: %w", err))
		}
		if err := a.restoreActivationState(healthTimerName(options.name), oldHealthState); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore health timer state: %w", err))
		}
	}()
	if hadCronRestart && options.cronRestart == "" {
		changed = true
		if _, err := a.runSystemctl("disable", "--now", cronRestartTimerName(options.name)); err != nil {
			return fmt.Errorf("disable cron restart for %s: %w", options.name, err)
		}
		if err := a.removeCronRestartFiles(options.name); err != nil {
			return err
		}
	}
	if a.hasSchedule(options.name) {
		changed = true
		_, _ = a.runSystemctl("disable", "--now", scheduleTimerName(options.name))
		if err := os.Remove(a.scheduleTimerPath(options.name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove previous schedule timer: %w", err)
		}
	}
	if a.hasHealth(options.name) && options.health.Probe.Kind == "" {
		changed = true
		_, _ = a.runSystemctl("disable", "--now", healthTimerName(options.name))
		if err := a.removeHealthFiles(options.name); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(a.unitPath(options.name), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}
	changed = true
	if options.cronRestart != "" {
		if err := writeFileAtomic(a.cronRestartServicePath(options.name), []byte(cronService), 0o644); err != nil {
			return fmt.Errorf("write cron restart service: %w", err)
		}
		if err := writeFileAtomic(a.cronRestartTimerPath(options.name), []byte(cronTimer), 0o644); err != nil {
			return fmt.Errorf("write cron restart timer: %w", err)
		}
	}
	if options.health.Probe.Kind != "" {
		for _, file := range []struct {
			path, content string
		}{{a.healthPath(options.name, ".health.service"), healthService}, {a.healthPath(options.name, ".health.timer"), healthTimer}, {a.healthPath(options.name, ".health-recover.service"), healthRecovery}} {
			if file.content == "" {
				_ = os.Remove(file.path)
				continue
			}
			if err := writeFileAtomic(file.path, []byte(file.content), 0o644); err != nil {
				return fmt.Errorf("write health unit: %w", err)
			}
		}
	}
	if _, err := a.runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd manager: %w", err)
	}
	if options.enable && replacing {
		if _, err := a.runSystemctl("enable", unitName(options.name)); err != nil {
			return fmt.Errorf("enable %s: %w", options.name, err)
		}
		if _, err := a.runSystemctl("restart", unitName(options.name)); err != nil {
			return fmt.Errorf("restart updated service %s: %w", options.name, err)
		}
	} else if options.enable {
		if _, err := a.runSystemctl("enable", "--now", unitName(options.name)); err != nil {
			return fmt.Errorf("enable and start %s: %w", options.name, err)
		}
	} else {
		if replacing {
			if _, err := a.runSystemctl("disable", unitName(options.name)); err != nil {
				return fmt.Errorf("disable boot start for %s: %w", options.name, err)
			}
		}
		action := "start"
		if replacing {
			action = "restart"
		}
		if _, err := a.runSystemctl(action, unitName(options.name)); err != nil {
			return fmt.Errorf("%s %s: %w", action, options.name, err)
		}
	}
	if options.cronRestart != "" {
		if options.enable && hadCronRestart {
			if _, err := a.runSystemctl("enable", cronRestartTimerName(options.name)); err != nil {
				return fmt.Errorf("enable cron restart for %s: %w", options.name, err)
			}
			if _, err := a.runSystemctl("restart", cronRestartTimerName(options.name)); err != nil {
				return fmt.Errorf("restart cron timer for %s: %w", options.name, err)
			}
		} else if options.enable {
			if _, err := a.runSystemctl("enable", "--now", cronRestartTimerName(options.name)); err != nil {
				return fmt.Errorf("enable cron restart for %s: %w", options.name, err)
			}
		} else {
			if hadCronRestart {
				if _, err := a.runSystemctl("disable", cronRestartTimerName(options.name)); err != nil {
					return fmt.Errorf("disable cron timer boot start for %s: %w", options.name, err)
				}
			}
			action := "start"
			if hadCronRestart {
				action = "restart"
			}
			if _, err := a.runSystemctl(action, cronRestartTimerName(options.name)); err != nil {
				return fmt.Errorf("%s cron restart for %s: %w", action, options.name, err)
			}
		}
	}
	if options.health.Probe.Kind != "" {
		if options.enable && hadHealth {
			if _, err := a.runSystemctl("enable", healthTimerName(options.name)); err != nil {
				return fmt.Errorf("enable health timer for %s: %w", options.name, err)
			}
			if _, err := a.runSystemctl("restart", healthTimerName(options.name)); err != nil {
				return fmt.Errorf("restart health timer for %s: %w", options.name, err)
			}
		} else if options.enable {
			if _, err := a.runSystemctl("enable", "--now", healthTimerName(options.name)); err != nil {
				return fmt.Errorf("enable health timer for %s: %w", options.name, err)
			}
		} else {
			if hadHealth {
				if _, err := a.runSystemctl("disable", healthTimerName(options.name)); err != nil {
					return fmt.Errorf("disable health timer for %s: %w", options.name, err)
				}
			}
			action := "start"
			if hadHealth {
				action = "restart"
			}
			if _, err := a.runSystemctl(action, healthTimerName(options.name)); err != nil {
				return fmt.Errorf("%s health timer for %s: %w", action, options.name, err)
			}
		}
	}
	if err := a.checkStarted(options.name, options.restart); err != nil {
		return err
	}
	if options.wait {
		if err := a.waitForActive(options.name, options.waitTimeout); err != nil {
			return err
		}
		if options.waitProbe.Kind != "" {
			options.waitProbe.RuntimeCwd = options.cwd
			options.waitProbe.RuntimeEnv = append([]string(nil), options.env...)
			if err := waitForProbe(options.waitProbe, options.waitTimeout); err != nil {
				return fmt.Errorf("service %s did not become ready: %w", options.name, err)
			}
		}
	}
	fmt.Fprintf(a.stdout, "%s started as %s\n", options.name, unitName(options.name))
	a.printStartNotices(options)
	if err := a.writeServiceConfig(options.name, savedConfig); err != nil {
		return err
	}
	succeeded = true
	return nil
}

func parseStart(args []string) (startOptions, error) {
	options := defaultStartOptions()
	if len(args) == 0 {
		return options, errors.New("start requires a script or a command after --")
	}
	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if len(positionals) != 0 {
				return options, errors.New("do not put a script before --")
			}
			options.command = append([]string(nil), args[i+1:]...)
			options.explicitCmd = true
			if len(options.command) == 0 {
				return options, errors.New("-- must be followed by a command")
			}
			break
		}
		key, inlineValue, hasInlineValue := strings.Cut(arg, "=")
		switch key {
		case "-n", "--name", "--cwd", "--interpreter", "-e", "--env", "--env-file", "--restart", "--restart-sec", "--start-limit-interval", "--start-limit-burst", "--cron-restart", "--description", "--max-memory", "--user", "--group", "--property", "--exec-start-pre", "--exec-start-post", "--exec-stop", "--exec-reload", "--max-cpu", "--max-tasks", "--nice", "--open-files", "--stop-signal", "--stop-timeout", "--kill-mode", "--sandbox", "--after", "--wants", "--requires", "--on-failure", "--wait-http", "--wait-tcp", "--wait-command", "--health-http", "--health-tcp", "--health-command", "--health-interval", "--health-timeout", "--health-retries", "--health-retry-delay", "--health-start-period", "--health-action", "--type", "--pid-file", "--timezone", "-i", "--instances", "--restart-max-delay", "--restart-steps":
			value := inlineValue
			if !hasInlineValue {
				if i+1 >= len(args) {
					return options, fmt.Errorf("%s requires a value", key)
				}
				i++
				value = args[i]
			}
			switch key {
			case "-n", "--name":
				options.name = value
			case "--cwd":
				options.cwd = value
			case "--interpreter":
				options.interpreter = value
			case "-e", "--env":
				options.env = append(options.env, value)
			case "--env-file":
				options.envFile = value
			case "--restart":
				options.restart = value
			case "--restart-sec":
				options.restartSec = value
			case "--start-limit-interval":
				options.startLimitInterval = value
			case "--start-limit-burst":
				options.startLimitBurst = value
			case "--cron-restart":
				options.cronRestart = value
			case "--description":
				options.description = value
			case "--max-memory":
				options.maxMemory = value
			case "--user":
				options.runAsUser = value
			case "--group":
				options.runAsGroup = value
			case "--property":
				options.properties = append(options.properties, value)
			case "--exec-start-pre":
				options.execStartPre = append(options.execStartPre, value)
			case "--exec-start-post":
				options.execStartPost = append(options.execStartPost, value)
			case "--exec-stop":
				options.execStop = value
			case "--exec-reload":
				options.execReload = value
			case "--max-cpu":
				options.properties = append(options.properties, "CPUQuota="+value)
			case "--max-tasks":
				options.properties = append(options.properties, "TasksMax="+value)
			case "--nice":
				options.properties = append(options.properties, "Nice="+value)
			case "--open-files":
				options.properties = append(options.properties, "LimitNOFILE="+value)
			case "--stop-signal":
				options.properties = append(options.properties, "KillSignal="+value)
			case "--stop-timeout":
				options.properties = append(options.properties, "TimeoutStopSec="+value)
			case "--kill-mode":
				options.properties = append(options.properties, "KillMode="+value)
			case "--sandbox":
				if _, err := sandboxProperties(value); err != nil {
					return options, err
				}
				options.sandbox = value
			case "--type":
				options.serviceType = value
			case "--pid-file":
				options.pidFile = value
			case "--timezone":
				options.timezone = value
			case "-i", "--instances":
				count, err := parseInstanceCount(value)
				if err != nil {
					return options, err
				}
				options.instances = count
			case "--restart-max-delay":
				if !validSystemdDuration(value) {
					return options, fmt.Errorf("invalid restart max delay %q", value)
				}
				options.properties = append(options.properties, "RestartMaxDelaySec="+value)
			case "--restart-steps":
				steps, err := strconv.Atoi(value)
				if err != nil || steps < 1 || steps > 3000 {
					return options, fmt.Errorf("invalid restart steps %q", value)
				}
				options.properties = append(options.properties, "RestartSteps="+value)
			case "--after":
				options.after = append(options.after, value)
			case "--wants":
				options.wants = append(options.wants, value)
			case "--requires":
				options.requires = append(options.requires, value)
			case "--on-failure":
				options.onFailure = append(options.onFailure, value)
			case "--wait-http", "--wait-tcp", "--wait-command":
				if err := options.waitProbe.setTarget(strings.TrimPrefix(key, "--wait-"), value); err != nil {
					return options, err
				}
				options.wait = true
				if options.waitTimeout == 0 {
					options.waitTimeout = 30 * time.Second
				}
			case "--health-http", "--health-tcp", "--health-command":
				if err := options.health.Probe.setTarget(strings.TrimPrefix(key, "--health-"), value); err != nil {
					return options, err
				}
				options.set["--health"] = true
			case "--health-interval":
				options.health.Interval = value
				options.set["--health"] = true
			case "--health-timeout":
				options.health.Probe.Timeout = value
				options.set["--health"] = true
			case "--health-retries":
				retries, err := strconv.Atoi(value)
				if err != nil || retries < 1 {
					return options, fmt.Errorf("invalid health retries %q", value)
				}
				options.health.Probe.Retries = retries
				options.set["--health"] = true
			case "--health-retry-delay":
				options.health.Probe.RetryDelay = value
				options.set["--health"] = true
			case "--health-start-period":
				options.health.StartPeriod = value
				options.set["--health"] = true
			case "--health-action":
				options.health.Action = value
				options.set["--health"] = true
			}
			if strings.HasPrefix(key, "--exec-") {
				options.set["--exec-hooks"] = true
			} else if strings.HasPrefix(key, "--max-") && key != "--max-memory" || key == "--nice" || key == "--open-files" || key == "--stop-signal" || key == "--stop-timeout" || key == "--kill-mode" || key == "--sandbox" || key == "--restart-max-delay" || key == "--restart-steps" {
				options.set["--property"] = true
			} else if key == "-i" {
				options.set["--instances"] = true
			} else {
				options.set[key] = true
			}
		case "--mise":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.mise = true
			options.set["--mise"] = true
		case "--wait":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.wait = true
			if options.waitTimeout == 0 {
				options.waitTimeout = 30 * time.Second
			}
		case "--wait-timeout":
			value := inlineValue
			if !hasInlineValue {
				if i+1 >= len(args) {
					return options, errors.New("--wait-timeout requires a duration")
				}
				i++
				value = args[i]
			}
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 {
				return options, fmt.Errorf("invalid wait timeout %q", value)
			}
			options.wait = true
			options.waitTimeout = duration
		case "--no-mise":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.mise = false
			options.set["--mise"] = true
		case "--no-health":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.health = healthConfig{}
			options.set["--health"] = true
		case "--force", "-f":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.force = true
		case "--no-enable":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.enable = false
			options.set["--enable"] = true
		case "--enable":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.enable = true
			options.set["--enable"] = true
		case "--no-cron-restart":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.cronRestart = ""
			options.set["--cron-restart"] = true
		case "--clear-env":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.env = nil
			options.set["--env"] = true
		case "--no-start-limit":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.startLimitInterval = ""
			options.startLimitBurst = ""
			options.set["--start-limit-interval"] = true
			options.set["--start-limit-burst"] = true
		case "--no-max-memory":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.maxMemory = ""
			options.set["--max-memory"] = true
		case "--clear-properties":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.properties = nil
			options.set["--property"] = true
		case "--clear-exec-hooks":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.execStartPre = nil
			options.execStartPost = nil
			options.execStop = ""
			options.execReload = ""
			options.set["--exec-hooks"] = true
		case "--clear-on-failure":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.onFailure = nil
			options.set["--on-failure"] = true
		default:
			if strings.HasPrefix(arg, "-") && len(positionals) == 0 {
				return options, fmt.Errorf("unknown start option %q", arg)
			}
			positionals = append(positionals, arg)
		}
	}
	if !options.explicitCmd {
		options.command = positionals
	}
	// RestartSteps=0 disables the backoff, so a lone maximum delay gets a
	// default number of steps.
	if hasProperty(options.properties, "RestartMaxDelaySec") && !hasProperty(options.properties, "RestartSteps") {
		options.properties = append(options.properties, "RestartSteps="+defaultRestartSteps)
	}
	return options, nil
}

const defaultRestartSteps = "10"

type persistedStartConfig struct {
	Version            int          `json:"version"`
	Mode               string       `json:"mode,omitempty"`
	Schedule           string       `json:"schedule,omitempty"`
	Persistent         bool         `json:"persistent,omitempty"`
	TimerOnBoot        string       `json:"timer_on_boot,omitempty"`
	TimerInterval      string       `json:"timer_interval,omitempty"`
	TimerRandomDelay   string       `json:"timer_randomized_delay,omitempty"`
	Cwd                string       `json:"cwd"`
	Description        string       `json:"description,omitempty"`
	Restart            string       `json:"restart"`
	RestartSec         string       `json:"restart_sec"`
	StartLimitInterval string       `json:"start_limit_interval,omitempty"`
	StartLimitBurst    string       `json:"start_limit_burst,omitempty"`
	CronRestart        string       `json:"cron_restart,omitempty"`
	Env                []string     `json:"env,omitempty"`
	EnvFile            string       `json:"env_file,omitempty"`
	Interpreter        string       `json:"interpreter,omitempty"`
	Mise               bool         `json:"mise"`
	Enable             bool         `json:"enable"`
	Command            []string     `json:"command"`
	ExplicitCmd        bool         `json:"explicit_command"`
	MaxMemory          string       `json:"max_memory,omitempty"`
	User               string       `json:"user,omitempty"`
	Group              string       `json:"group,omitempty"`
	Properties         []string     `json:"properties,omitempty"`
	ExecStartPre       []string     `json:"exec_start_pre,omitempty"`
	ExecStartPost      []string     `json:"exec_start_post,omitempty"`
	ExecStop           string       `json:"exec_stop,omitempty"`
	ExecReload         string       `json:"exec_reload,omitempty"`
	After              []string     `json:"after,omitempty"`
	Wants              []string     `json:"wants,omitempty"`
	Requires           []string     `json:"requires,omitempty"`
	OnFailure          []string     `json:"on_failure,omitempty"`
	Health             healthConfig `json:"health,omitempty"`
	Type               string       `json:"type,omitempty"`
	PIDFile            string       `json:"pid_file,omitempty"`
	Timezone           string       `json:"timezone,omitempty"`
	Instances          int          `json:"instances,omitempty"`
}

func persistedConfigFrom(options startOptions) persistedStartConfig {
	return persistedStartConfig{Version: 1, Cwd: options.cwd, Description: options.description, Restart: options.restart, RestartSec: options.restartSec, StartLimitInterval: options.startLimitInterval, StartLimitBurst: options.startLimitBurst, CronRestart: options.cronRestart, Env: append([]string(nil), options.env...), EnvFile: options.envFile, Interpreter: options.interpreter, Mise: options.mise, Enable: options.enable, Command: append([]string(nil), options.command...), ExplicitCmd: options.explicitCmd, MaxMemory: options.maxMemory, User: options.runAsUser, Group: options.runAsGroup, Properties: append([]string(nil), options.properties...), ExecStartPre: append([]string(nil), options.execStartPre...), ExecStartPost: append([]string(nil), options.execStartPost...), ExecStop: options.execStop, ExecReload: options.execReload, After: append([]string(nil), options.after...), Wants: append([]string(nil), options.wants...), Requires: append([]string(nil), options.requires...), OnFailure: append([]string(nil), options.onFailure...), Health: options.health, Type: options.serviceType, PIDFile: options.pidFile, Timezone: options.timezone, Instances: options.instances}
}

func (a *application) inheritStartOptions(options *startOptions) error {
	saved, err := a.readServiceConfig(options.name)
	if os.IsNotExist(err) {
		if !options.set["--cron-restart"] {
			options.cronRestart = a.readCronRestart(options.name)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read saved service config: %w", err)
	}
	if saved.Version != 1 {
		return fmt.Errorf("unsupported saved service config version %d", saved.Version)
	}
	if !options.set["--cwd"] {
		options.cwd = saved.Cwd
	}
	if !options.set["--description"] {
		options.description = saved.Description
	}
	if !options.set["--restart"] {
		options.restart = saved.Restart
	}
	if !options.set["--restart-sec"] {
		options.restartSec = saved.RestartSec
	}
	if !options.set["--start-limit-interval"] {
		options.startLimitInterval = saved.StartLimitInterval
	}
	if !options.set["--start-limit-burst"] {
		options.startLimitBurst = saved.StartLimitBurst
	}
	if !options.set["--cron-restart"] {
		options.cronRestart = saved.CronRestart
	}
	if !options.set["--env"] && !options.set["-e"] {
		options.env = append([]string(nil), saved.Env...)
	}
	if !options.set["--env-file"] {
		options.envFile = saved.EnvFile
	}
	if !options.set["--interpreter"] {
		options.interpreter = saved.Interpreter
	}
	if !options.set["--mise"] {
		options.mise = saved.Mise
	}
	if !options.set["--enable"] {
		options.enable = saved.Enable
	}
	if !options.set["--max-memory"] {
		options.maxMemory = saved.MaxMemory
	}
	if !options.set["--user"] {
		options.runAsUser = saved.User
	}
	if !options.set["--group"] {
		options.runAsGroup = saved.Group
	}
	if !options.set["--property"] {
		options.properties = append([]string(nil), saved.Properties...)
	}
	if !options.set["--exec-hooks"] {
		options.execStartPre = append([]string(nil), saved.ExecStartPre...)
		options.execStartPost = append([]string(nil), saved.ExecStartPost...)
		options.execStop = saved.ExecStop
		options.execReload = saved.ExecReload
	}
	if !options.set["--after"] {
		options.after = append([]string(nil), saved.After...)
	}
	if !options.set["--wants"] {
		options.wants = append([]string(nil), saved.Wants...)
	}
	if !options.set["--requires"] {
		options.requires = append([]string(nil), saved.Requires...)
	}
	if !options.set["--on-failure"] {
		options.onFailure = append([]string(nil), saved.OnFailure...)
	}
	if !options.set["--health"] {
		options.health = saved.Health
	}
	if !options.set["--type"] {
		options.serviceType = saved.Type
	}
	if !options.set["--pid-file"] {
		options.pidFile = saved.PIDFile
	}
	if !options.set["--timezone"] {
		options.timezone = saved.Timezone
	}
	if !options.set["--instances"] {
		options.instances = saved.Instances
	}
	return nil
}

func (a *application) readServiceConfig(name string) (persistedStartConfig, error) {
	var config persistedStartConfig
	data, err := os.ReadFile(a.serviceConfigPath(name))
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("parse saved service config: %w", err)
	}
	return config, nil
}

func (a *application) writeServiceConfig(name string, config persistedStartConfig) error {
	path := a.serviceConfigPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create service config directory: %w", err)
	}
	if previous, err := os.ReadFile(path); err == nil {
		historyDir := filepath.Join(filepath.Dir(path), "history", name)
		if err := os.MkdirAll(historyDir, 0o700); err != nil {
			return fmt.Errorf("create service history directory: %w", err)
		}
		historyPath := filepath.Join(historyDir, fmt.Sprintf("%020d.json", time.Now().UnixNano()))
		if err := writeFileAtomic(historyPath, previous, 0o600); err != nil {
			return fmt.Errorf("save service config history: %w", err)
		}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode service config: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write saved service config: %w", err)
	}
	return nil
}

func (a *application) updateFromSavedConfig(name string) error {
	if !a.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	config, err := a.readServiceConfig(name)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("saved spm configuration is missing; replace the service once with `spm start ... --force`")
		}
		return err
	}
	options := optionsFromPersisted(name, config, true)
	if err := options.validatePrepared(); err != nil {
		return fmt.Errorf("validate saved service config: %w", err)
	}
	if config.Mode == "schedule" {
		calendar, err := scheduleCalendar(config)
		if err != nil {
			return fmt.Errorf("validate saved schedule: %w", err)
		}
		return a.installScheduled(options, config, calendar, false)
	}
	return a.installPrepared(options, config)
}

func optionsFromPersisted(name string, config persistedStartConfig, force bool) startOptions {
	return startOptions{
		name: name, cwd: config.Cwd, description: config.Description,
		restart: config.Restart, restartSec: config.RestartSec,
		startLimitInterval: config.StartLimitInterval, startLimitBurst: config.StartLimitBurst,
		cronRestart: config.CronRestart, env: append([]string(nil), config.Env...), envFile: config.EnvFile,
		interpreter: config.Interpreter, mise: config.Mise, enable: config.Enable,
		command: append([]string(nil), config.Command...), explicitCmd: config.ExplicitCmd,
		maxMemory: config.MaxMemory, runAsUser: config.User, runAsGroup: config.Group, properties: append([]string(nil), config.Properties...), force: force,
		execStartPre: append([]string(nil), config.ExecStartPre...), execStartPost: append([]string(nil), config.ExecStartPost...), execStop: config.ExecStop, execReload: config.ExecReload,
		after: append([]string(nil), config.After...), wants: append([]string(nil), config.Wants...), requires: append([]string(nil), config.Requires...), onFailure: append([]string(nil), config.OnFailure...),
		health:      config.Health,
		serviceType: config.Type, pidFile: config.PIDFile, timezone: config.Timezone, instances: config.Instances,
	}
}

func (o *startOptions) validatePrepared() error {
	if err := validateName(o.name); err != nil {
		return err
	}
	if len(o.command) == 0 {
		return errors.New("saved command is empty")
	}
	if info, err := os.Stat(o.cwd); err != nil || !info.IsDir() {
		return fmt.Errorf("working directory %q does not exist or is not a directory", o.cwd)
	}
	if err := validateRestart(o.restart); err != nil {
		return err
	}
	if !validSystemdDuration(o.restartSec) {
		return fmt.Errorf("invalid restart delay %q", o.restartSec)
	}
	if o.startLimitInterval != "" && !validSystemdDuration(o.startLimitInterval) {
		return fmt.Errorf("invalid start limit interval %q", o.startLimitInterval)
	}
	if o.startLimitBurst != "" {
		burst, err := strconv.ParseUint(o.startLimitBurst, 10, 32)
		if err != nil || burst == 0 {
			return fmt.Errorf("invalid start limit burst %q", o.startLimitBurst)
		}
	}
	if o.maxMemory != "" && !validSystemdSize(o.maxMemory) {
		return fmt.Errorf("invalid memory limit %q", o.maxMemory)
	}
	if err := validateSystemUser(o.runAsUser); err != nil {
		return err
	}
	if err := validateSystemUser(o.runAsGroup); err != nil {
		return fmt.Errorf("invalid system group: %w", err)
	}
	if err := o.validateExecHooks(); err != nil {
		return err
	}
	for _, value := range o.env {
		if err := validateEnvironment(value); err != nil {
			return err
		}
	}
	for _, property := range o.properties {
		if err := validateServiceProperty(property); err != nil {
			return err
		}
	}
	if err := o.validateDependencies(); err != nil {
		return err
	}
	if err := o.health.validate(); err != nil {
		return err
	}
	if err := o.waitProbe.validate(); err != nil {
		return err
	}
	if err := o.validateRuntimeOptions(); err != nil {
		return err
	}
	if o.cronRestart != "" {
		calendar, err := cronToOnCalendar(o.cronRestart)
		if err != nil {
			return err
		}
		o.cronCalendar = withTimezone(calendar, o.timezone)
	}
	return nil
}

var serviceTypes = []string{"exec", "simple", "notify", "notify-reload", "forking", "idle"}

func (o *startOptions) validateRuntimeOptions() error {
	if o.serviceType != "" {
		valid := false
		for _, serviceType := range serviceTypes {
			valid = valid || o.serviceType == serviceType
		}
		if !valid {
			return fmt.Errorf("invalid service type %q; use %s", o.serviceType, strings.Join(serviceTypes, ", "))
		}
	}
	if o.pidFile != "" {
		if o.serviceType != "forking" {
			return errors.New("--pid-file requires --type forking")
		}
		if !filepath.IsAbs(o.pidFile) || strings.ContainsAny(o.pidFile, "\r\n") {
			return fmt.Errorf("pid file %q must be an absolute path", o.pidFile)
		}
		if o.instances > 0 && !strings.Contains(o.pidFile, instancePlaceholder) {
			return fmt.Errorf("--pid-file must contain %s when running multiple instances", instancePlaceholder)
		}
	}
	if err := validateTimezone(o.timezone); err != nil {
		return err
	}
	if o.instances < 0 || o.instances > maxInstances {
		return fmt.Errorf("instance count must be between 0 and %d", maxInstances)
	}
	if hasProperty(o.properties, "RestartSteps") && !hasProperty(o.properties, "RestartMaxDelaySec") {
		return errors.New("--restart-steps requires --restart-max-delay")
	}
	return nil
}

func validateTimezone(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 64 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "..") {
		return fmt.Errorf("invalid time zone %q", value)
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '/' || r == '_' || r == '-' || r == '+') {
			return fmt.Errorf("invalid time zone %q", value)
		}
	}
	return nil
}

// withTimezone appends an IANA time zone to an OnCalendar expression, which
// systemd 245 and later evaluate in that zone instead of the system zone.
func withTimezone(calendar, timezone string) string {
	if calendar == "" || timezone == "" {
		return calendar
	}
	return calendar + " " + timezone
}

func hasProperty(properties []string, key string) bool {
	for _, property := range properties {
		if propertyKey, _, _ := strings.Cut(property, "="); propertyKey == key {
			return true
		}
	}
	return false
}

func (o *startOptions) prepare() error {
	if len(o.command) == 0 {
		return errors.New("no command specified")
	}
	if err := validateRestart(o.restart); err != nil {
		return err
	}
	if !validSystemdDuration(o.restartSec) {
		return fmt.Errorf("invalid restart delay %q", o.restartSec)
	}
	if o.startLimitInterval != "" && !validSystemdDuration(o.startLimitInterval) {
		return fmt.Errorf("invalid start limit interval %q", o.startLimitInterval)
	}
	if o.startLimitBurst != "" {
		burst, err := strconv.ParseUint(o.startLimitBurst, 10, 32)
		if err != nil || burst == 0 {
			return fmt.Errorf("invalid start limit burst %q", o.startLimitBurst)
		}
	}
	if o.maxMemory != "" && !validSystemdSize(o.maxMemory) {
		return fmt.Errorf("invalid memory limit %q", o.maxMemory)
	}
	if err := validateSystemUser(o.runAsUser); err != nil {
		return err
	}
	if err := validateSystemUser(o.runAsGroup); err != nil {
		return fmt.Errorf("invalid system group: %w", err)
	}
	if err := o.validateExecHooks(); err != nil {
		return err
	}
	for _, property := range o.properties {
		if err := validateServiceProperty(property); err != nil {
			return err
		}
	}
	if err := o.validateDependencies(); err != nil {
		return err
	}
	if err := o.health.validate(); err != nil {
		return err
	}
	if err := o.waitProbe.validate(); err != nil {
		return err
	}
	if err := o.validateRuntimeOptions(); err != nil {
		return err
	}
	if o.cronRestart != "" {
		calendar, cronErr := cronToOnCalendar(o.cronRestart)
		if cronErr != nil {
			return fmt.Errorf("invalid cron restart: %w", cronErr)
		}
		o.cronCalendar = withTimezone(calendar, o.timezone)
	}
	for _, value := range o.env {
		if err := validateEnvironment(value); err != nil {
			return err
		}
	}

	var err error
	if o.cwd == "" {
		o.cwd, err = os.Getwd()
	} else {
		o.cwd, err = filepath.Abs(o.cwd)
	}
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(o.cwd)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("working directory %q does not exist or is not a directory", o.cwd)
	}
	for _, path := range []string{o.cwd, o.envFile, o.pidFile} {
		if strings.TrimSpace(path) != path || strings.ContainsAny(path, "\r\n") {
			return fmt.Errorf("path %q cannot start or end with whitespace or contain a newline in a systemd unit", path)
		}
	}
	if o.envFile != "" {
		if !filepath.IsAbs(o.envFile) {
			o.envFile = filepath.Join(o.cwd, o.envFile)
		}
		o.envFile, err = filepath.Abs(o.envFile)
		if err != nil {
			return fmt.Errorf("resolve environment file: %w", err)
		}
		if info, statErr := os.Stat(o.envFile); statErr != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("environment file %q does not exist or is not a regular file", o.envFile)
		}
	}

	if o.name == "" {
		o.name = inferName(o.command, o.explicitCmd)
	}
	if o.explicitCmd {
		if o.interpreter != "" {
			return errors.New("--interpreter cannot be used with an explicit command after --")
		}
		if err := resolveExplicitScript(o.command, o.cwd); err != nil {
			return err
		}
		if o.mise {
			mise, resolveErr := o.resolveMise()
			if resolveErr != nil {
				return resolveErr
			}
			o.command = append([]string{mise, "exec", "--"}, o.command...)
		} else {
			executable, resolveErr := resolveExecutableFromPath(o.command[0], o.cwd, o.executablePath)
			if resolveErr != nil {
				return resolveErr
			}
			if o.executablePath != "" {
				if shebang, ok := readShebang(executable); ok {
					interpreter, interpreterErr := resolveExecutableFromPath(shebang[0], "", o.executablePath)
					if interpreterErr != nil {
						return fmt.Errorf("resolve shebang interpreter %s: %w", shebang[0], interpreterErr)
					}
					command := append([]string{interpreter}, shebang[1:]...)
					command = append(command, executable)
					o.command = append(command, o.command[1:]...)
				} else {
					o.command[0] = executable
				}
			} else {
				o.command[0] = executable
			}
		}
	} else {
		if err := o.prepareScriptCommand(); err != nil {
			return err
		}
	}
	if err := validateName(o.name); err != nil {
		return err
	}
	if o.description == "" {
		o.description = "spm service: " + o.name
	}
	if strings.ContainsAny(o.description, "\r\n") {
		return errors.New("description cannot contain a newline")
	}
	if o.sandbox != "" {
		preset, err := sandboxProperties(o.sandbox)
		if err != nil {
			return err
		}
		o.properties = append(o.properties, preset...)
		// ProtectHome and ProtectSystem make most paths read-only, so the
		// working directory stays writable for logs, caches, and pid files.
		o.properties = append(o.properties, "ReadWritePaths="+systemdPathList(o.cwd))
	}
	return nil
}

// systemdPathList quotes one path for space separated path settings such as
// ReadWritePaths.
func systemdPathList(path string) string {
	if strings.ContainsAny(path, " \t\"'\\") {
		return quoteSystemd(path)
	}
	return escapeSystemdSpecifiers(path)
}

func resolveExplicitScript(command []string, cwd string) error {
	if len(command) < 2 {
		return nil
	}
	commandIndex := 0
	if executableBase(command[0]) == "mise" {
		for commandIndex < len(command) && command[commandIndex] != "--" {
			commandIndex++
		}
		commandIndex++
	}
	if commandIndex >= len(command) || !isInterpreter(executableBase(command[commandIndex])) {
		return nil
	}
	scriptIndex, isFile := interpreterProgramIndex(command, commandIndex)
	if scriptIndex < 0 || !isFile {
		return nil
	}
	script := command[scriptIndex]
	if filepath.Ext(script) == "" && !strings.ContainsRune(script, filepath.Separator) {
		return nil
	}
	if !filepath.IsAbs(script) {
		script = filepath.Join(cwd, script)
	}
	script, err := filepath.Abs(script)
	if err != nil {
		return fmt.Errorf("resolve explicit command script: %w", err)
	}
	info, err := os.Stat(script)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("script %q does not exist or is not a regular file", script)
	}
	command[scriptIndex] = script
	return nil
}

func (o *startOptions) prepareScriptCommand() error {
	target := o.command[0]
	arguments := append([]string(nil), o.command[1:]...)
	resolvedTarget := target
	if !filepath.IsAbs(resolvedTarget) {
		resolvedTarget = filepath.Join(o.cwd, resolvedTarget)
	}
	resolvedTarget, err := filepath.Abs(resolvedTarget)
	if err != nil {
		return fmt.Errorf("resolve script path: %w", err)
	}
	info, statErr := os.Stat(resolvedTarget)
	if statErr == nil && info.Mode().IsRegular() {
		target = resolvedTarget
	} else if o.mise || o.interpreter != "" || filepath.Ext(target) != "" || strings.ContainsRune(target, filepath.Separator) {
		return fmt.Errorf("script %q does not exist or is not a regular file", resolvedTarget)
	}
	if o.mise {
		mise, err := o.resolveMise()
		if err != nil {
			return err
		}
		tool := o.interpreter
		var toolArgs []string
		if tool == "" {
			if shebang, ok := readShebang(target); ok {
				tool = shebang[0]
				toolArgs = shebang[1:]
			} else {
				tool, err = scriptRuntime(target)
				if err != nil {
					return err
				}
			}
		}
		command := append([]string{mise, "exec", "--", tool}, toolArgs...)
		command = append(command, target)
		o.command = append(command, arguments...)
		return nil
	}
	if o.interpreter != "" {
		interpreter, err := resolveExecutableFromPath(o.interpreter, "", o.executablePath)
		if err != nil {
			return fmt.Errorf("resolve interpreter: %w", err)
		}
		o.command = append([]string{interpreter, target}, arguments...)
		return nil
	}
	if statErr == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 && hasShebang(target) && o.executablePath == "" {
		o.command = append([]string{target}, arguments...)
		return nil
	}
	if statErr == nil && info.Mode().IsRegular() {
		if shebang, ok := readShebang(target); ok {
			interpreter, resolveErr := resolveExecutableFromPath(shebang[0], "", o.executablePath)
			if resolveErr != nil {
				return fmt.Errorf("resolve shebang interpreter %s: %w", shebang[0], resolveErr)
			}
			o.command = append([]string{interpreter}, append(shebang[1:], append([]string{target}, arguments...)...)...)
			return nil
		}
	}
	if runtimeName, err := scriptRuntime(target); err == nil {
		interpreter, resolveErr := resolveExecutableFromPath(runtimeName, "", o.executablePath)
		if resolveErr != nil {
			return fmt.Errorf("resolve %s for %s: %w", runtimeName, target, resolveErr)
		}
		o.command = append([]string{interpreter, target}, arguments...)
		return nil
	}
	executable, err := resolveExecutableFromPath(target, o.cwd, o.executablePath)
	if err != nil {
		return err
	}
	o.command = append([]string{executable}, arguments...)
	return nil
}

// resolveMise returns the mise executable written into the unit. A Snap
// launcher goes through "snap run", which moves the process and all of its
// children into a snap scope outside the service cgroup, so the binary inside
// the classic snap is used directly instead.
func (o *startOptions) resolveMise() (string, error) {
	mise, err := resolveExecutableFromPath("mise", "", o.executablePath)
	if err != nil {
		return "", fmt.Errorf("resolve mise: %w", err)
	}
	if !isSnapMise(mise) {
		return mise, nil
	}
	executable, err := resolveSnapMise(mise)
	if err != nil {
		return "", fmt.Errorf("resolve Snap-installed mise %s: %w; install mise without Snap to avoid the snap scope", mise, err)
	}
	o.snapMise = true
	return executable, nil
}

// resolveSnapMise maps a Snap launcher such as /snap/bin/mise to the app
// command inside /snap/<snap>/current. The "current" link is kept so the
// service follows snap refreshes.
func resolveSnapMise(launcher string) (string, error) {
	appName := filepath.Base(launcher)
	if runtime.GOOS == "windows" {
		appName = strings.TrimSuffix(appName, filepath.Ext(appName))
	}
	snapName, app, ok := strings.Cut(appName, ".")
	if !ok {
		app = snapName
	}
	roots := []string{"/snap", "/var/lib/snapd/snap"}
	if binDir := filepath.Dir(launcher); filepath.Base(binDir) == "bin" && filepath.Base(filepath.Dir(binDir)) == "snap" {
		roots = []string{filepath.Dir(binDir)}
	}
	var lastErr error
	for _, root := range roots {
		current := filepath.Join(root, snapName, "current")
		data, err := os.ReadFile(filepath.Join(current, "meta", "snap.yaml"))
		if err != nil {
			lastErr = err
			continue
		}
		confinement, command := parseSnapApp(data, app)
		if confinement != "classic" {
			return "", fmt.Errorf("snap %s uses %q confinement, only classic can run outside snap run", snapName, valueOr(confinement, "strict"))
		}
		command = strings.TrimPrefix(strings.TrimPrefix(command, "$SNAP/"), "${SNAP}/")
		if command == "" || strings.ContainsAny(command, " \t$") || filepath.IsAbs(command) {
			return "", fmt.Errorf("unsupported command %q for snap app %s", command, appName)
		}
		executable := filepath.Join(current, filepath.FromSlash(command))
		info, err := os.Stat(executable)
		if err != nil || info.IsDir() || runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("snap command %q does not exist or is not executable", executable)
		}
		return executable, nil
	}
	return "", fmt.Errorf("read snap metadata: %w", lastErr)
}

// parseSnapApp reads the confinement and the command of one app from
// meta/snap.yaml. It only understands the plain block mappings snapd writes.
func parseSnapApp(data []byte, app string) (confinement, command string) {
	section, currentApp := "", ""
	appIndent, fieldIndent := -1, -1
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, value, _ := strings.Cut(trimmed, ":")
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch {
		case indent == 0:
			section, currentApp, appIndent, fieldIndent = key, "", -1, -1
			if key == "confinement" {
				confinement = value
			}
		case section != "apps":
		case appIndent < 0 || indent <= appIndent:
			appIndent, currentApp, fieldIndent = indent, key, -1
		case currentApp == app:
			if fieldIndent < 0 {
				fieldIndent = indent
			}
			if indent == fieldIndent && key == "command" {
				command = value
			}
		}
	}
	return confinement, command
}

func isSnapMise(path string) bool {
	normalized := filepath.ToSlash(path)
	if strings.Contains(normalized, "/snap/bin/") || strings.Contains(normalized, "/var/lib/snapd/snap/bin/") {
		return true
	}
	target, err := filepath.EvalSymlinks(path)
	return err == nil && filepath.Base(target) == "snap"
}

func scriptRuntime(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs":
		return "node", nil
	case ".py":
		return "python3", nil
	case ".rb":
		return "ruby", nil
	case ".sh":
		return "sh", nil
	default:
		return "", fmt.Errorf("cannot infer runtime for %q; use --interpreter or an explicit command after --", path)
	}
}

func resolveExecutable(name string) (string, error) {
	return resolveExecutableFromPath(name, "", "")
}

func resolveExecutableFrom(name, cwd string) (string, error) {
	return resolveExecutableFromPath(name, cwd, "")
}

func resolveExecutableFromPath(name, cwd, searchPath string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		path := name
		if !filepath.IsAbs(path) && cwd != "" {
			path = filepath.Join(cwd, path)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve executable %q: %w", name, err)
		}
		info, err := os.Stat(absolute)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("executable %q does not exist or is not executable", absolute)
		}
		return absolute, nil
	}
	path, err := lookPath(name, searchPath)
	if err != nil {
		return "", fmt.Errorf("executable %q was not found in PATH", name)
	}
	return filepath.Abs(path)
}

func lookPath(name, searchPath string) (string, error) {
	if searchPath == "" {
		return exec.LookPath(name)
	}
	names := []string{name}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		extensions := filepath.SplitList(os.Getenv("PATHEXT"))
		if len(extensions) == 0 {
			extensions = []string{".COM", ".EXE", ".BAT", ".CMD"}
		}
		for _, extension := range extensions {
			names = append(names, name+strings.ToLower(extension), name+strings.ToUpper(extension))
		}
	}
	for _, directory := range filepath.SplitList(searchPath) {
		if directory == "" {
			continue
		}
		for _, candidateName := range names {
			candidate := filepath.Join(directory, candidateName)
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				return candidate, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

func inferName(command []string, explicit bool) string {
	if len(command) == 0 {
		return "app"
	}
	index := 0
	base := executableBase(command[index])
	if explicit && base == "mise" {
		for index < len(command) && command[index] != "--" {
			index++
		}
		if index+1 < len(command) {
			return inferName(command[index+1:], true)
		}
	}
	if explicit && isInterpreter(base) {
		programIndex, _ := interpreterProgramIndex(command, index)
		if programIndex < 0 {
			return sanitizeName(base)
		}
		index = programIndex
	}
	name := filepath.Base(command[index])
	if ext := filepath.Ext(name); ext != "" {
		name = strings.TrimSuffix(name, ext)
	}
	if name = sanitizeName(name); name != "" {
		return name
	}
	return "app"
}

func executableBase(value string) string {
	base := filepath.Base(value)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func isInterpreter(value string) bool {
	switch value {
	case "node", "nodejs", "python", "python3", "ruby", "bun", "deno", "sh", "bash", "zsh":
		return true
	default:
		return false
	}
}

func interpreterProgramIndex(command []string, interpreterIndex int) (int, bool) {
	base := executableBase(command[interpreterIndex])
	for i := interpreterIndex + 1; i < len(command); i++ {
		arg := command[i]
		if arg == "--" {
			if i+1 < len(command) {
				return i + 1, true
			}
			return -1, false
		}
		if (base == "deno" || base == "bun") && (arg == "run" || arg == "task") {
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return i, true
		}
		if strings.Contains(arg, "=") {
			continue
		}
		if interpreterInlineProgram(base, arg) {
			return -1, false
		}
		if interpreterModuleOption(base, arg) {
			if i+1 < len(command) {
				return i + 1, false
			}
			return -1, false
		}
		if interpreterOptionTakesValue(base, arg) {
			i++
		}
	}
	return -1, false
}

func interpreterInlineProgram(interpreter, option string) bool {
	switch interpreter {
	case "node", "nodejs":
		return option == "-e" || option == "--eval" || option == "-p" || option == "--print"
	case "python", "python3", "ruby":
		return option == "-c" || option == "-e"
	case "sh", "bash", "zsh":
		return option == "-c"
	default:
		return false
	}
}

func interpreterModuleOption(interpreter, option string) bool {
	return (interpreter == "python" || interpreter == "python3") && option == "-m"
}

func interpreterOptionTakesValue(interpreter, option string) bool {
	switch interpreter {
	case "node", "nodejs":
		switch option {
		case "-r", "--require", "--loader", "--import", "--conditions", "--inspect-port", "--title":
			return true
		}
	case "python", "python3":
		return option == "-W" || option == "-X" || option == "--check-hash-based-pycs"
	case "ruby":
		return option == "-I" || option == "-r" || option == "--require"
	}
	return false
}

func sanitizeName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), ".-")
}

func validateRestart(value string) error {
	valid := map[string]bool{
		"no": true, "on-success": true, "on-failure": true, "on-abnormal": true,
		"on-abort": true, "on-watchdog": true, "always": true,
	}
	if !valid[value] {
		return fmt.Errorf("invalid restart policy %q", value)
	}
	return nil
}

func validSystemdDuration(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	units := []string{"usec", "msec", "seconds", "second", "minutes", "minute", "months", "month", "hours", "hour", "weeks", "week", "years", "year", "us", "ms", "min", "s", "h", "d", "w", "M", "y"}
	for len(value) > 0 {
		numberEnd := 0
		dot := false
		for numberEnd < len(value) {
			c := value[numberEnd]
			if c >= '0' && c <= '9' {
				numberEnd++
				continue
			}
			if c == '.' && !dot && numberEnd > 0 {
				dot = true
				numberEnd++
				continue
			}
			break
		}
		if numberEnd == 0 || value[numberEnd-1] == '.' {
			return false
		}
		value = value[numberEnd:]
		if value == "" {
			return true
		}
		matched := false
		for _, unit := range units {
			if strings.HasPrefix(value, unit) {
				value = value[len(unit):]
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
		value = strings.TrimLeft(value, " ")
	}
	return true
}

func validSystemdSize(value string) bool {
	if value == "infinity" {
		return true
	}
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	index := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
	}
	if index == 0 {
		return false
	}
	unit := value[index:]
	switch unit {
	case "", "K", "M", "G", "T", "P", "E", "KB", "MB", "GB", "TB", "PB", "EB", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB":
		return true
	default:
		return false
	}
}

func validateSystemUser(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 256 {
		return fmt.Errorf("invalid system user %q", value)
	}
	numeric := true
	for _, r := range value {
		if r < '0' || r > '9' {
			numeric = false
			break
		}
	}
	if numeric {
		if _, err := strconv.ParseUint(value, 10, 32); err == nil {
			return nil
		}
		return fmt.Errorf("invalid numeric system user %q", value)
	}
	for index, r := range value {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		digit := r >= '0' && r <= '9'
		if !(letter || r == '_' || index > 0 && (digit || r == '-' || r == '.')) {
			return fmt.Errorf("invalid system user %q", value)
		}
	}
	return nil
}

func validateServiceProperty(value string) error {
	key, propertyValue, ok := strings.Cut(value, "=")
	if !ok || key == "" || propertyValue == "" || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("service property %q must use KEY=VALUE on one line", value)
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return fmt.Errorf("invalid service property name %q", key)
		}
	}
	reserved := map[string]bool{
		"Type": true, "ExecStart": true, "WorkingDirectory": true, "Environment": true,
		"EnvironmentFile": true, "Restart": true, "RestartSec": true, "User": true,
		"Group": true, "ExecStartPre": true, "ExecStartPost": true, "ExecStop": true,
		"ExecReload": true, "SyslogIdentifier": true,
	}
	if reserved[key] {
		return fmt.Errorf("service property %q is managed by spm and cannot be overridden", key)
	}
	return nil
}

func (o startOptions) validateDependencies() error {
	for _, values := range [][]string{o.after, o.wants, o.requires, o.onFailure} {
		for _, value := range values {
			if value == "" || strings.ContainsAny(value, " \t\r\n") {
				return fmt.Errorf("invalid systemd unit dependency %q", value)
			}
		}
	}
	return nil
}

func (o startOptions) validateExecHooks() error {
	commands := append([]string(nil), o.execStartPre...)
	commands = append(commands, o.execStartPost...)
	for _, command := range commands {
		if command == "" {
			return errors.New("systemd pre/post command cannot be empty")
		}
	}
	commands = append(commands, o.execStop, o.execReload)
	for _, command := range commands {
		if command != "" && (strings.TrimSpace(command) != command || strings.ContainsAny(command, "\r\n")) {
			return fmt.Errorf("invalid systemd exec command %q", command)
		}
	}
	return nil
}

func hasShebang(path string) bool {
	_, ok := readShebang(path)
	return ok
}

func readShebang(path string) ([]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), "#!") {
		return nil, false
	}
	line := strings.SplitN(string(data[2:]), "\n", 2)[0]
	fields := strings.Fields(strings.TrimSuffix(line, "\r"))
	if len(fields) == 0 {
		return nil, false
	}
	if fields[0] == "/usr/bin/env" && len(fields) > 1 {
		fields = fields[1:]
		if fields[0] == "-S" {
			fields = fields[1:]
		}
	}
	return fields, len(fields) > 0
}

func validateEnvironment(value string) error {
	key, _, ok := strings.Cut(value, "=")
	if !ok || key == "" {
		return fmt.Errorf("environment value %q must use KEY=VALUE", value)
	}
	for i, r := range key {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		digit := r >= '0' && r <= '9'
		if !(r == '_' || letter || i > 0 && digit) {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("environment value for %s contains an unsupported control character", key)
	}
	return nil
}

func renderUnit(options startOptions, system bool) (string, error) {
	if len(options.command) == 0 {
		return "", errors.New("cannot render a unit without a command")
	}
	if err := validateSystemUser(options.runAsUser); err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString("# Managed by spm. Manual changes may be overwritten.\n")
	builder.WriteString("[Unit]\n")
	description := escapeSystemdSpecifiers(options.description)
	if options.instances > 0 {
		description += " instance %i"
	}
	builder.WriteString("Description=" + description + "\n")
	for _, unit := range options.after {
		builder.WriteString("After=" + unit + "\n")
	}
	for _, unit := range options.wants {
		builder.WriteString("Wants=" + unit + "\n")
	}
	for _, unit := range options.requires {
		builder.WriteString("Requires=" + unit + "\n")
	}
	for _, unit := range options.onFailure {
		builder.WriteString("OnFailure=" + unit + "\n")
	}
	if options.startLimitInterval != "" {
		builder.WriteString("StartLimitIntervalSec=" + options.startLimitInterval + "\n")
	}
	if options.startLimitBurst != "" {
		builder.WriteString("StartLimitBurst=" + options.startLimitBurst + "\n")
	}
	builder.WriteByte('\n')
	builder.WriteString("[Service]\n")
	builder.WriteString("Type=" + valueOr(options.serviceType, "exec") + "\n")
	if options.pidFile != "" {
		builder.WriteString("PIDFile=" + escapeSystemdPath(options.pidFile) + "\n")
	}
	if options.runAsUser != "" {
		if !system {
			return "", errors.New("service User= requires system mode")
		}
		builder.WriteString("User=" + options.runAsUser + "\n")
	}
	if options.runAsGroup != "" {
		if !system {
			return "", errors.New("service Group= requires system mode")
		}
		builder.WriteString("Group=" + options.runAsGroup + "\n")
	}
	builder.WriteString("WorkingDirectory=" + escapeSystemdPath(options.cwd) + "\n")
	for _, command := range options.execStartPre {
		builder.WriteString("ExecStartPre=" + command + "\n")
	}
	builder.WriteString("ExecStart=")
	for i, arg := range options.command {
		if i > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(quoteSystemdExec(arg))
	}
	builder.WriteByte('\n')
	for _, command := range options.execStartPost {
		builder.WriteString("ExecStartPost=" + command + "\n")
	}
	if options.execStop != "" {
		builder.WriteString("ExecStop=" + options.execStop + "\n")
	}
	if options.execReload != "" {
		builder.WriteString("ExecReload=" + options.execReload + "\n")
	}
	if options.instances > 0 {
		builder.WriteString("Environment=\"SPM_INSTANCE=%i\" \"NODE_APP_INSTANCE=%i\"\n")
	}
	for _, value := range options.env {
		builder.WriteString("Environment=" + quoteSystemd(value) + "\n")
	}
	if options.envFile != "" {
		builder.WriteString("EnvironmentFile=" + escapeSystemdPath(options.envFile) + "\n")
	}
	builder.WriteString("Restart=" + options.restart + "\n")
	builder.WriteString("RestartSec=" + options.restartSec + "\n")
	if options.maxMemory != "" {
		builder.WriteString("MemoryMax=" + options.maxMemory + "\n")
	}
	if !hasProperty(options.properties, "KillSignal") {
		builder.WriteString("KillSignal=SIGTERM\n")
	}
	if !hasProperty(options.properties, "TimeoutStopSec") {
		builder.WriteString("TimeoutStopSec=30s\n")
	}
	for _, property := range options.properties {
		builder.WriteString(property + "\n")
	}
	identifier := "spm-" + options.name
	if options.instances > 0 {
		identifier += "@%i"
	}
	builder.WriteString("SyslogIdentifier=" + identifier + "\n\n")
	builder.WriteString("[Install]\n")
	if system {
		builder.WriteString("WantedBy=multi-user.target\n")
	} else {
		builder.WriteString("WantedBy=default.target\n")
	}
	unit := builder.String()
	if options.instances > 0 {
		unit = strings.ReplaceAll(unit, instancePlaceholder, "%i")
	}
	return unit, nil
}

// instancePlaceholder in commands, environment values, and paths expands to
// the instance number of a template service, for example PORT=300{{instance}}.
const instancePlaceholder = "{{instance}}"

func sandboxProperties(level string) ([]string, error) {
	basic := []string{"NoNewPrivileges=true", "PrivateTmp=true", "ProtectSystem=full", "ProtectHome=read-only"}
	switch level {
	case "basic":
		return basic, nil
	case "strict":
		return append(basic, "ProtectKernelTunables=true", "ProtectKernelModules=true", "ProtectControlGroups=true", "RestrictSUIDSGID=true", "LockPersonality=true"), nil
	default:
		return nil, fmt.Errorf("invalid sandbox preset %q; use basic or strict", level)
	}
}

type fileBackup struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

type activationState struct {
	active       bool
	activeKnown  bool
	enabled      bool
	enabledKnown bool
}

func (a *application) readActivationState(unit string) activationState {
	state := activationState{}
	if output, _ := a.runSystemctl("is-active", unit); len(output) > 0 {
		value := strings.TrimSpace(string(output))
		state.activeKnown = value == "active" || value == "inactive" || value == "failed"
		state.active = value == "active"
	}
	if output, _ := a.runSystemctl("is-enabled", unit); len(output) > 0 {
		value := strings.TrimSpace(string(output))
		state.enabledKnown = value == "enabled" || value == "disabled" || value == "static" || value == "indirect"
		state.enabled = value == "enabled"
	}
	return state
}

func (a *application) restoreActivationState(unit string, state activationState) error {
	var errs []error
	if state.enabledKnown {
		action := "disable"
		if state.enabled {
			action = "enable"
		}
		if _, err := a.runSystemctl(action, unit); err != nil {
			errs = append(errs, err)
		}
	}
	if state.activeKnown {
		action := "stop"
		if state.active {
			action = "restart"
		}
		if _, err := a.runSystemctl(action, unit); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func snapshotFiles(paths []string) ([]fileBackup, error) {
	backups := make([]fileBackup, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			backups = append(backups, fileBackup{path: path})
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		backups = append(backups, fileBackup{path: path, data: data, mode: info.Mode(), exists: true})
	}
	return backups, nil
}

func restoreFiles(backups []fileBackup) error {
	var errs []error
	for _, backup := range backups {
		if backup.exists {
			if err := writeFileAtomic(backup.path, backup.data, backup.mode); err != nil {
				errs = append(errs, err)
			}
		} else if err := os.Remove(backup.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *application) checkStarted(name, restart string) error {
	return a.checkUnitStarted(unitName(name), restart)
}

func (a *application) checkUnitStarted(unit, restart string) error {
	if a.sleep != nil {
		a.sleep(startSettleDelay)
	}
	state := a.readUnitStates([]string{unit})[unit]
	if state.active == "" || state.active == "active" || state.active == "reloading" || state.active == "activating" && state.sub != "auto-restart" {
		return nil
	}
	if restart == "no" && state.active == "inactive" && (state.result == "" || state.result == "success") && (state.execStatus == "" || state.execStatus == "0") {
		return nil
	}
	label := unitLabel(unit)
	name, _, _ := strings.Cut(label, "@")
	logsCommand := "spm logs"
	if a.system {
		logsCommand = "spm --system logs"
	}
	return fmt.Errorf("service %s entered %s/%s after start (result=%s, exec=%s); run `%s %s`", label, state.active, state.sub, valueOr(state.result, "unknown"), formatExecStatus(state), logsCommand, name)
}

func (a *application) waitForActive(name string, timeout time.Duration) error {
	return a.waitForUnitActive(unitName(name), timeout)
}

func (a *application) waitForUnitActive(unit string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	name := unitLabel(unit)
	for {
		state := a.readUnitStates([]string{unit})[unit]
		if state.active == "active" {
			return nil
		}
		if state.active == "failed" {
			return fmt.Errorf("service %s failed while waiting for readiness", name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for service %s to become active", timeout, name)
		}
		if a.sleep != nil {
			a.sleep(200 * time.Millisecond)
		} else {
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func waitForProbe(probe probeConfig, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	probe.Retries = 1
	probe.RetryDelay = "0s"
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = runProbe(probe)
		if lastErr == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
}

// Description= is not unquoted by systemd, so only specifiers are escaped.
func escapeSystemdSpecifiers(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

func quoteSystemd(value string) string {
	return quoteSystemdValue(value, false)
}

func quoteSystemdExec(value string) string {
	return quoteSystemdValue(value, true)
}

// escapeSystemdPath escapes specifiers in single path settings.
// WorkingDirectory=, EnvironmentFile= and PIDFile= take the rest of the line
// literally and do not decode C escapes, so a space written as \x20 would
// point at a different path.
func escapeSystemdPath(value string) string {
	return escapeSystemdSpecifiers(value)
}

func quoteSystemdValue(value string, escapeDollar bool) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			builder.WriteString("\\\\")
		case '"':
			builder.WriteString("\\\"")
		case '\n':
			builder.WriteString("\\n")
		case '\r':
			builder.WriteString("\\r")
		case '\t':
			builder.WriteString("\\t")
		case '%':
			builder.WriteString("%%")
		case '$':
			if escapeDollar {
				builder.WriteString("$$")
			} else {
				builder.WriteRune(r)
			}
		default:
			if r < 0x20 || r == 0x7f {
				builder.WriteString(fmt.Sprintf("\\x%02x", r))
			} else {
				builder.WriteRune(r)
			}
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".spm-unit-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		if runtime.GOOS != "windows" {
			return err
		}
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return err
		}
		return os.Rename(temporaryName, path)
	}
	return nil
}
