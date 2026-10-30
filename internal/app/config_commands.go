package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func (a *application) configCommand(command string, args []string) error {
	switch command {
	case "clone":
		if len(args) != 2 {
			return errors.New("clone requires a source and destination name")
		}
		if err := validateName(args[0]); err != nil {
			return err
		}
		if err := validateName(args[1]); err != nil {
			return err
		}
		if a.exists(args[1]) {
			return fmt.Errorf("service %q already exists", args[1])
		}
		config, err := a.readServiceConfig(args[0])
		if err != nil {
			return err
		}
		return a.installConfig(args[1], config, false)
	case "history":
		name, err := oneName(args)
		if err != nil {
			return err
		}
		files, err := a.historyFiles(name)
		if err != nil {
			return err
		}
		for i, file := range files {
			info, _ := os.Stat(file)
			fmt.Fprintf(a.stdout, "%d\t%s\n", i+1, info.ModTime().Format("2006-01-02 15:04:05"))
		}
		return nil
	case "rollback":
		return a.rollback(args)
	case "set", "unset":
		return a.mutateConfig(command, args)
	default:
		return fmt.Errorf("unsupported config command %q", command)
	}
}

func (a *application) mutateConfig(command string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("%s requires a service name and at least one option", command)
	}
	name := args[0]
	if err := validateName(name); err != nil {
		return err
	}
	config, err := a.readServiceConfig(name)
	if err != nil {
		return err
	}
	for i := 1; i < len(args); i++ {
		if args[i] != "--command" {
			continue
		}
		if command != "set" || i+2 >= len(args) || args[i+1] != "--" {
			return errors.New("set --command requires -- followed by a command")
		}
		options := optionsFromPersisted(name, config, true)
		options.command = append([]string(nil), args[i+2:]...)
		options.explicitCmd = true
		if err := a.prepareRunAsUser(&options); err != nil {
			return err
		}
		if err := options.prepare(); err != nil {
			return err
		}
		updated := persistedConfigFrom(options)
		updated.Mode, updated.Schedule, updated.Persistent = config.Mode, config.Schedule, config.Persistent
		updated.TimerOnBoot, updated.TimerInterval, updated.TimerRandomDelay = config.TimerOnBoot, config.TimerInterval, config.TimerRandomDelay
		return a.installConfig(name, updated, true)
	}
	for i := 1; i < len(args); i++ {
		key, inline, hasInline := strings.Cut(args[i], "=")
		value := inline
		boolean := key == "--enable" || key == "--no-enable" || key == "--no-health" || key == "--mise" || key == "--no-mise" || key == "--persistent" || key == "--no-persistent" || command == "unset" && (key == "--cron-restart" || key == "--schedule" || key == "--timer-on-boot" || key == "--timer-interval" || key == "--timer-randomized-delay" || key == "--max-memory" || key == "--env-file" || key == "--user" || key == "--group" || key == "--description" || key == "--exec-stop" || key == "--exec-reload" || key == "--start-limit-interval" || key == "--start-limit-burst" || key == "--type" || key == "--pid-file" || key == "--timezone")
		if !hasInline && !boolean {
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", key)
			}
			i++
			value = args[i]
		}
		switch key {
		case "--cwd":
			config.Cwd = value
		case "--description":
			if command == "unset" {
				config.Description = ""
			} else {
				config.Description = value
			}
		case "--restart":
			config.Restart = value
		case "--restart-sec":
			config.RestartSec = value
		case "--cron-restart":
			config.CronRestart = value
		case "--schedule":
			if command == "unset" {
				config.Schedule, config.Persistent = "", false
			} else {
				config.Mode, config.Schedule, config.Restart = "schedule", value, "no"
				config.CronRestart = ""
			}
		case "--timer-on-boot", "--timer-interval", "--timer-randomized-delay":
			target := &config.TimerOnBoot
			if key == "--timer-interval" {
				target = &config.TimerInterval
			} else if key == "--timer-randomized-delay" {
				target = &config.TimerRandomDelay
			}
			if command == "unset" {
				*target = ""
			} else {
				*target = value
				config.Mode, config.Restart, config.CronRestart = "schedule", "no", ""
			}
		case "--max-memory":
			config.MaxMemory = value
		case "--env-file":
			config.EnvFile = value
		case "--user":
			config.User = value
		case "--group":
			config.Group = value
		case "--exec-start-pre":
			config.ExecStartPre = mutateString(config.ExecStartPre, value, command == "unset")
		case "--exec-start-post":
			config.ExecStartPost = mutateString(config.ExecStartPost, value, command == "unset")
		case "--exec-stop":
			config.ExecStop = value
		case "--exec-reload":
			config.ExecReload = value
		case "--enable":
			config.Enable = true
		case "--no-enable":
			config.Enable = false
		case "--persistent":
			if config.Mode != "schedule" {
				return errors.New("--persistent requires a schedule")
			}
			config.Persistent = true
		case "--no-persistent":
			config.Persistent = false
		case "--mise":
			if !config.Mise {
				mode, schedule, persistent := config.Mode, config.Schedule, config.Persistent
				onBoot, interval, randomDelay := config.TimerOnBoot, config.TimerInterval, config.TimerRandomDelay
				options := optionsFromPersisted(name, config, true)
				options.mise = true
				if err := a.prepareRunAsUser(&options); err != nil {
					return err
				}
				if err := options.prepare(); err != nil {
					return err
				}
				config = persistedConfigFrom(options)
				config.Mode, config.Schedule, config.Persistent = mode, schedule, persistent
				config.TimerOnBoot, config.TimerInterval, config.TimerRandomDelay = onBoot, interval, randomDelay
			}
		case "--no-mise":
			if config.Mise {
				if len(config.Command) < 4 || executableBase(config.Command[0]) != "mise" || config.Command[1] != "exec" || config.Command[2] != "--" {
					return errors.New("saved mise command cannot be safely unwrapped")
				}
				config.Command = append([]string(nil), config.Command[3:]...)
				config.Mise = false
			}
		case "--env":
			config.Env = mutateKeyValue(config.Env, value, command == "unset")
		case "--property":
			config.Properties = mutateKeyValue(config.Properties, value, command == "unset")
		case "--after":
			config.After = mutateString(config.After, value, command == "unset")
		case "--wants":
			config.Wants = mutateString(config.Wants, value, command == "unset")
		case "--requires":
			config.Requires = mutateString(config.Requires, value, command == "unset")
		case "--on-failure":
			config.OnFailure = mutateString(config.OnFailure, value, command == "unset")
		case "--health-http", "--health-tcp", "--health-command":
			config.Health.Probe = probeConfig{}
			if err := config.Health.Probe.setTarget(strings.TrimPrefix(key, "--health-"), value); err != nil {
				return err
			}
		case "--health-interval":
			config.Health.Interval = value
		case "--health-timeout":
			config.Health.Probe.Timeout = value
		case "--health-retries":
			retries, err := strconv.Atoi(value)
			if err != nil || retries < 1 {
				return fmt.Errorf("invalid health retries %q", value)
			}
			config.Health.Probe.Retries = retries
		case "--health-retry-delay":
			config.Health.Probe.RetryDelay = value
		case "--health-start-period":
			config.Health.StartPeriod = value
		case "--health-action":
			config.Health.Action = value
		case "--no-health":
			config.Health = healthConfig{}
		case "--start-limit-interval":
			config.StartLimitInterval = value
		case "--start-limit-burst":
			config.StartLimitBurst = value
		case "--type":
			config.Type = value
		case "--pid-file":
			config.PIDFile = value
		case "--timezone":
			config.Timezone = value
		case "--instances":
			if command == "unset" {
				return errors.New("use `spm set NAME --instances N`, or `spm scale NAME N` to keep running instances")
			}
			count, err := parseInstanceCount(value)
			if err != nil {
				return err
			}
			config.Instances = count
		default:
			return fmt.Errorf("unknown %s option %q", command, key)
		}
	}
	if config.Mode == "schedule" && config.Schedule == "" && config.TimerOnBoot == "" && config.TimerInterval == "" {
		config.Mode, config.Persistent, config.TimerRandomDelay = "", false, ""
		if config.Restart == "no" {
			config.Restart = "always"
		}
	}
	return a.installConfig(name, config, true)
}

func mutateString(values []string, value string, remove bool) []string {
	result := values[:0]
	found := false
	for _, existing := range values {
		if existing == value {
			found = true
			if remove {
				continue
			}
		}
		result = append(result, existing)
	}
	if !remove && !found {
		result = append(result, value)
	}
	return result
}

func mutateKeyValue(values []string, value string, remove bool) []string {
	key, _, hasValue := strings.Cut(value, "=")
	if !hasValue {
		key = value
	}
	result := values[:0]
	for _, existing := range values {
		existingKey, _, _ := strings.Cut(existing, "=")
		if existingKey != key {
			result = append(result, existing)
		}
	}
	if !remove {
		result = append(result, value)
	}
	return result
}

func (a *application) installConfig(name string, config persistedStartConfig, force bool) error {
	options := optionsFromPersisted(name, config, force)
	if err := options.validatePrepared(); err != nil {
		return err
	}
	if config.Mode == "schedule" {
		calendar, err := scheduleCalendar(config)
		if err != nil {
			return err
		}
		return a.installScheduled(options, config, calendar, false)
	}
	return a.installPrepared(options, config)
}

func (a *application) historyFiles(name string) ([]string, error) {
	directory := a.historyDirectory(name)
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, filepath.Join(directory, entry.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files, nil
}

func (a *application) historyDirectory(name string) string {
	return filepath.Join(filepath.Dir(a.serviceConfigPath(name)), "history", name)
}

func (a *application) rollback(args []string) error {
	if len(args) != 1 && len(args) != 3 {
		return errors.New("rollback requires NAME [--revision N]")
	}
	revision := 1
	if err := validateName(args[0]); err != nil {
		return err
	}
	if !a.exists(args[0]) {
		return fmt.Errorf("service %q is not managed by spm", args[0])
	}
	if len(args) == 3 {
		if args[1] != "--revision" {
			return fmt.Errorf("unknown rollback option %q", args[1])
		}
		value, err := strconv.Atoi(args[2])
		if err != nil || value < 1 {
			return fmt.Errorf("invalid revision %q", args[2])
		}
		revision = value
	}
	files, err := a.historyFiles(args[0])
	if err != nil {
		return err
	}
	if revision > len(files) {
		return fmt.Errorf("revision %d does not exist", revision)
	}
	data, err := os.ReadFile(files[revision-1])
	if err != nil {
		return err
	}
	var config persistedStartConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	return a.installConfig(args[0], config, true)
}
