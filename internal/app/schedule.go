package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type scheduleOptions struct {
	start       startOptions
	expression  string
	calendar    string
	onBoot      string
	interval    string
	randomDelay string
	persistent  bool
	runNow      bool
}

func scheduleCalendar(config persistedStartConfig) (string, error) {
	if config.Schedule == "" {
		return "", nil
	}
	calendar, err := cronToOnCalendar(config.Schedule)
	if err != nil {
		return "", err
	}
	return withTimezone(calendar, config.Timezone), nil
}

func validatePersistedSchedule(config persistedStartConfig) error {
	if config.Mode != "schedule" {
		return nil
	}
	if config.Schedule == "" && config.TimerOnBoot == "" && config.TimerInterval == "" {
		return errors.New("schedule requires a cron, boot, or interval trigger")
	}
	if config.Persistent && config.Schedule == "" {
		return errors.New("persistent requires a cron schedule")
	}
	if config.Timezone != "" && config.Schedule == "" {
		return errors.New("timezone requires a cron schedule")
	}
	if config.Instances > 0 {
		return errors.New("scheduled tasks cannot run multiple instances")
	}
	if config.Type != "" {
		return errors.New("scheduled tasks always use Type=oneshot; remove --type")
	}
	for field, value := range map[string]string{"timer_on_boot": config.TimerOnBoot, "timer_interval": config.TimerInterval, "timer_randomized_delay": config.TimerRandomDelay} {
		if value != "" && !validSystemdDuration(value) {
			return fmt.Errorf("invalid %s duration %q", field, value)
		}
	}
	_, err := scheduleCalendar(config)
	return err
}

func (a *application) schedule(args []string) error {
	options, err := parseSchedule(args)
	if err != nil {
		return err
	}
	if options.start.name == "" {
		options.start.name = inferName(options.start.command, options.start.explicitCmd)
	}
	if err := a.prepareRunAsUser(&options.start); err != nil {
		return err
	}
	if err := options.start.prepare(); err != nil {
		return err
	}
	if options.start.health.Probe.Kind != "" {
		return errors.New("scheduled oneshot tasks cannot use periodic health checks")
	}
	a.warnSandboxSupport(options.start)
	if options.calendar != "" {
		if _, err := a.runner.Run("systemd-analyze", "calendar", options.calendar); err != nil {
			return fmt.Errorf("validate schedule calendar %q: %w", options.calendar, err)
		}
	}
	config := persistedConfigFrom(options.start)
	config.Mode = "schedule"
	config.Schedule = options.expression
	config.TimerOnBoot = options.onBoot
	config.TimerInterval = options.interval
	config.TimerRandomDelay = options.randomDelay
	config.Persistent = options.persistent
	return a.installScheduled(options.start, config, options.calendar, options.runNow)
}

func parseSchedule(args []string) (scheduleOptions, error) {
	var options scheduleOptions
	var startArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			startArgs = append(startArgs, args[i:]...)
			break
		}
		key, inline, hasInline := strings.Cut(arg, "=")
		switch key {
		case "--cron", "--on-boot", "--interval", "--randomized-delay":
			value := inline
			if !hasInline {
				if i+1 >= len(args) {
					return options, fmt.Errorf("%s requires a value", key)
				}
				i++
				value = args[i]
			}
			switch key {
			case "--cron":
				options.expression = value
			case "--on-boot":
				options.onBoot = value
			case "--interval":
				options.interval = value
			case "--randomized-delay":
				options.randomDelay = value
			}
		case "--persistent", "--run-now":
			if hasInline {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			if key == "--persistent" {
				options.persistent = true
			} else {
				options.runNow = true
			}
		default:
			startArgs = append(startArgs, arg)
		}
	}
	if options.expression == "" && options.onBoot == "" && options.interval == "" {
		return options, errors.New("schedule requires --cron, --on-boot, or --interval")
	}
	if options.persistent && options.expression == "" {
		return options, errors.New("--persistent requires --cron")
	}
	for flag, value := range map[string]string{"--on-boot": options.onBoot, "--interval": options.interval, "--randomized-delay": options.randomDelay} {
		if value != "" && !validSystemdDuration(value) {
			return options, fmt.Errorf("invalid %s duration %q", flag, value)
		}
	}
	if options.expression != "" {
		calendar, err := cronToOnCalendar(options.expression)
		if err != nil {
			return options, fmt.Errorf("invalid schedule: %w", err)
		}
		options.calendar = calendar
	}
	start, err := parseStart(startArgs)
	if err != nil {
		return options, err
	}
	start.restart = "no"
	start.cronRestart = ""
	if start.timezone != "" && options.expression == "" {
		return options, errors.New("--timezone requires --cron")
	}
	if start.instances > 0 {
		return options, errors.New("scheduled tasks cannot use --instances")
	}
	if start.serviceType != "" {
		return options, errors.New("scheduled tasks always use Type=oneshot; remove --type")
	}
	options.calendar = withTimezone(options.calendar, start.timezone)
	options.start = start
	return options, nil
}

func (a *application) installScheduled(options startOptions, config persistedStartConfig, calendar string, runNow bool) (returnErr error) {
	if err := validatePersistedSchedule(config); err != nil {
		return err
	}
	replacing := a.exists(options.name)
	if replacing && !options.force {
		return fmt.Errorf("service %q already exists; use --force to replace it", options.name)
	}
	if a.isTemplate(options.name) {
		return fmt.Errorf("service %q runs multiple instances; delete it before scheduling it as a task", options.name)
	}
	unit, err := renderUnit(options, a.system)
	if err != nil {
		return err
	}
	unit = strings.Replace(unit, "Type=exec\n", "Type=oneshot\n", 1)
	unit = strings.Replace(unit, "Restart=no\nRestartSec="+options.restartSec+"\n", "", 1)
	timer := renderScheduleTimer(options.name, config, calendar)
	if err := os.MkdirAll(a.unitDir, 0o755); err != nil {
		return fmt.Errorf("create systemd unit directory: %w", err)
	}
	paths := []string{a.unitPath(options.name), a.scheduleTimerPath(options.name), a.cronRestartServicePath(options.name), a.cronRestartTimerPath(options.name), a.serviceConfigPath(options.name)}
	paths = append(paths, a.healthPaths(options.name)...)
	backups, err := snapshotFiles(paths)
	if err != nil {
		return fmt.Errorf("back up existing files: %w", err)
	}
	oldHealthState := activationState{}
	if a.hasHealth(options.name) {
		oldHealthState = a.readActivationState(healthTimerName(options.name))
	}
	changed := false
	defer func() {
		if returnErr == nil || !changed {
			return
		}
		if restoreErr := restoreFiles(backups); restoreErr != nil {
			returnErr = errors.Join(returnErr, restoreErr)
		}
		_, _ = a.runSystemctl("daemon-reload")
		if restoreErr := a.restoreActivationState(healthTimerName(options.name), oldHealthState); restoreErr != nil {
			returnErr = errors.Join(returnErr, restoreErr)
		}
	}()
	if replacing {
		changed = true
		if a.hasHealth(options.name) {
			_, _ = a.runSystemctl("disable", "--now", healthTimerName(options.name))
			if err := a.removeHealthFiles(options.name); err != nil {
				return err
			}
		}
		if a.hasCronRestart(options.name) {
			_, _ = a.runSystemctl("disable", "--now", cronRestartTimerName(options.name))
			if err := a.removeCronRestartFiles(options.name); err != nil {
				return err
			}
		}
		if a.hasSchedule(options.name) {
			_, _ = a.runSystemctl("disable", "--now", scheduleTimerName(options.name))
		} else {
			_, _ = a.runSystemctl("disable", "--now", unitName(options.name))
		}
	}
	if err := writeFileAtomic(a.unitPath(options.name), []byte(unit), 0o644); err != nil {
		return err
	}
	changed = true
	if err := writeFileAtomic(a.scheduleTimerPath(options.name), []byte(timer), 0o644); err != nil {
		return err
	}
	if err := a.writeServiceConfig(options.name, config); err != nil {
		return err
	}
	if _, err := a.runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd manager: %w", err)
	}
	if options.enable {
		if _, err := a.runSystemctl("enable", "--now", scheduleTimerName(options.name)); err != nil {
			return fmt.Errorf("enable schedule %s: %w", options.name, err)
		}
	} else if replacing {
		if _, err := a.runSystemctl("disable", scheduleTimerName(options.name)); err != nil {
			return fmt.Errorf("disable schedule %s: %w", options.name, err)
		}
	}
	if runNow {
		if _, err := a.runSystemctl("start", unitName(options.name)); err != nil {
			return fmt.Errorf("run scheduled task %s: %w", options.name, err)
		}
	}
	fmt.Fprintf(a.stdout, "%s scheduled as %s\n", options.name, scheduleTimerName(options.name))
	return nil
}

func renderScheduleTimer(name string, config persistedStartConfig, calendar string) string {
	var builder strings.Builder
	if config.Schedule != "" {
		builder.WriteString("# CronExpression=" + config.Schedule + "\n")
		if config.Timezone != "" {
			builder.WriteString("# Timezone=" + config.Timezone + "\n")
		}
	}
	if config.TimerOnBoot != "" {
		builder.WriteString("# TimerOnBoot=" + config.TimerOnBoot + "\n")
	}
	if config.TimerInterval != "" {
		builder.WriteString("# TimerInterval=" + config.TimerInterval + "\n")
	}
	if config.TimerRandomDelay != "" {
		builder.WriteString("# TimerRandomizedDelay=" + config.TimerRandomDelay + "\n")
	}
	builder.WriteString("[Unit]\nDescription=" + escapeSystemdSpecifiers("Schedule for spm task: "+name) + "\nStartLimitIntervalSec=0\n\n")
	builder.WriteString("[Timer]\n")
	if calendar != "" {
		builder.WriteString("OnCalendar=" + calendar + "\n")
	}
	if config.TimerOnBoot != "" {
		builder.WriteString("OnBootSec=" + config.TimerOnBoot + "\n")
	}
	if config.TimerInterval != "" {
		if config.TimerOnBoot == "" && calendar == "" {
			builder.WriteString("OnActiveSec=" + config.TimerInterval + "\n")
		}
		builder.WriteString("OnUnitActiveSec=" + config.TimerInterval + "\n")
	}
	if config.TimerRandomDelay != "" {
		builder.WriteString("RandomizedDelaySec=" + config.TimerRandomDelay + "\n")
	}
	builder.WriteString("Unit=" + unitName(name) + "\n")
	if config.Persistent {
		builder.WriteString("Persistent=true\n")
	}
	builder.WriteString("\n[Install]\nWantedBy=timers.target\n")
	return builder.String()
}
