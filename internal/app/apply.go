package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type serviceDeclaration struct {
	Name               string
	Command            []string
	Script             string
	Args               []string
	Cwd                string
	Description        string
	Restart            string
	RestartSec         string
	StartLimitInterval string
	StartLimitBurst    string
	CronRestart        string
	Env                []string
	EnvFile            string
	Interpreter        string
	Mise               bool
	Enable             bool
	EnableSet          bool
	MaxMemory          string
	User               string
	Group              string
	Properties         []string
	ExecStartPre       []string
	ExecStartPost      []string
	ExecStop           string
	ExecReload         string
	After              []string
	Wants              []string
	Requires           []string
	OnFailure          []string
	Schedule           string
	TimerOnBoot        string
	TimerInterval      string
	TimerRandomDelay   string
	Persistent         bool
	HealthHTTP         string
	HealthTCP          string
	HealthCommand      string
	HealthInterval     string
	HealthTimeout      string
	HealthRetries      int
	HealthRetryDelay   string
	HealthStartPeriod  string
	HealthAction       string
	Type               string
	PIDFile            string
	Timezone           string
	Instances          int
}

func (service serviceDeclaration) hasSchedule() bool {
	return service.Schedule != "" || service.TimerOnBoot != "" || service.TimerInterval != ""
}

func validateTimerDeclaration(service serviceDeclaration) error {
	if service.Schedule != "" {
		if _, err := cronToOnCalendar(service.Schedule); err != nil {
			return fmt.Errorf("invalid schedule: %w", err)
		}
	}
	for field, value := range map[string]string{"timer_on_boot": service.TimerOnBoot, "timer_interval": service.TimerInterval, "timer_randomized_delay": service.TimerRandomDelay} {
		if value != "" && !validSystemdDuration(value) {
			return fmt.Errorf("invalid %s duration %q", field, value)
		}
	}
	if service.Persistent && service.Schedule == "" {
		return errors.New("persistent requires schedule")
	}
	if service.TimerRandomDelay != "" && !service.hasSchedule() {
		return errors.New("timer_randomized_delay requires schedule, timer_on_boot, or timer_interval")
	}
	return nil
}

func (a *application) apply(args []string) error {
	dryRun := false
	var path string
	for _, arg := range args {
		if arg == "--dry-run" {
			dryRun = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown apply option %q", arg)
		}
		if path != "" {
			return errors.New("apply requires exactly one spm.toml path")
		}
		path = arg
	}
	if path == "" {
		return errors.New("apply requires exactly one spm.toml path")
	}
	services, err := parseServiceDeclarations(path)
	if err != nil {
		return err
	}
	if dryRun {
		return a.printDeclarationPlan(services, false)
	}
	var errs []error
	for _, service := range services {
		startArgs := service.startArgs(a.exists(service.Name), a.system)
		var applyErr error
		if service.hasSchedule() {
			var scheduleArgs []string
			for _, option := range [][2]string{{"--cron", service.Schedule}, {"--on-boot", service.TimerOnBoot}, {"--interval", service.TimerInterval}, {"--randomized-delay", service.TimerRandomDelay}} {
				if option[1] != "" {
					scheduleArgs = append(scheduleArgs, option[0], option[1])
				}
			}
			startArgs = append(scheduleArgs, startArgs...)
			if service.Persistent {
				startArgs = append([]string{"--persistent"}, startArgs...)
			}
			applyErr = a.schedule(startArgs)
		} else {
			applyErr = a.start(startArgs)
		}
		if applyErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", service.Name, applyErr))
		}
	}
	return errors.Join(errs...)
}

func (a *application) inspectDeclaration(action string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%s requires exactly one spm.toml path", action)
	}
	services, err := parseServiceDeclarations(args[0])
	if err != nil {
		return err
	}
	if action == "validate" {
		for _, service := range services {
			if _, _, err := a.prepareDeclaration(service); err != nil {
				return fmt.Errorf("%s: %w", service.Name, err)
			}
		}
		fmt.Fprintf(a.stdout, "%s is valid (%d services)\n", args[0], len(services))
		return nil
	}
	return a.printDeclarationPlan(services, action == "diff")
}

func (a *application) printDeclarationPlan(services []serviceDeclaration, detailed bool) error {
	for _, service := range services {
		options, config, err := a.prepareDeclaration(service)
		if err != nil {
			return fmt.Errorf("%s: %w", service.Name, err)
		}
		action, err := a.declarationAction(options, config)
		if err != nil {
			return fmt.Errorf("%s: %w", service.Name, err)
		}
		fmt.Fprintf(a.stdout, "%s\t%s\n", action, service.Name)
		if detailed && action == "update" {
			if err := a.printConfigDiff(service.Name, config); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *application) printConfigDiff(name string, desired persistedStartConfig) error {
	current, err := a.readServiceConfig(name)
	if err != nil {
		return err
	}
	var currentMap, desiredMap map[string]any
	currentData, _ := json.Marshal(current)
	desiredData, _ := json.Marshal(desired)
	_ = json.Unmarshal(currentData, &currentMap)
	_ = json.Unmarshal(desiredData, &desiredMap)
	keys := make([]string, 0, len(currentMap)+len(desiredMap))
	seen := map[string]bool{}
	for key := range currentMap {
		seen[key] = true
		keys = append(keys, key)
	}
	for key := range desiredMap {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		oldValue, newValue := currentMap[key], desiredMap[key]
		if reflect.DeepEqual(oldValue, newValue) {
			continue
		}
		oldJSON, _ := json.Marshal(oldValue)
		newJSON, _ := json.Marshal(newValue)
		fmt.Fprintf(a.stdout, "-  %s = %s\n+  %s = %s\n", key, oldJSON, key, newJSON)
	}
	return nil
}

func (a *application) prepareDeclaration(service serviceDeclaration) (startOptions, persistedStartConfig, error) {
	options, err := parseStart(service.startArgs(a.exists(service.Name), a.system))
	if err != nil {
		return options, persistedStartConfig{}, err
	}
	if err := a.prepareRunAsUser(&options); err != nil {
		return options, persistedStartConfig{}, err
	}
	if err := options.prepare(); err != nil {
		return options, persistedStartConfig{}, err
	}
	if service.hasSchedule() {
		options.restart = "no"
		if options.health.Probe.Kind != "" {
			return options, persistedStartConfig{}, errors.New("scheduled oneshot tasks cannot use periodic health checks")
		}
	}
	config := persistedConfigFrom(options)
	if service.hasSchedule() {
		if err := validateTimerDeclaration(service); err != nil {
			return options, config, err
		}
		config.Mode = "schedule"
		config.Schedule = service.Schedule
		config.Persistent = service.Persistent
		config.TimerOnBoot = service.TimerOnBoot
		config.TimerInterval = service.TimerInterval
		config.TimerRandomDelay = service.TimerRandomDelay
	}
	return options, config, nil
}

func (a *application) declarationAction(options startOptions, config persistedStartConfig) (string, error) {
	if !a.exists(options.name) {
		return "create", nil
	}
	unit, err := renderUnit(options, a.system)
	if err != nil {
		return "", err
	}
	if config.Mode == "schedule" {
		unit = strings.Replace(unit, "Type=exec\n", "Type=oneshot\n", 1)
		unit = strings.Replace(unit, "Restart=no\nRestartSec="+options.restartSec+"\n", "", 1)
	}
	unitPath := a.unitPath(options.name)
	if options.instances > 0 {
		unitPath = a.templatePath(options.name)
	}
	currentUnit, err := os.ReadFile(unitPath)
	if os.IsNotExist(err) {
		return "update", nil
	}
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	currentConfig, err := os.ReadFile(a.serviceConfigPath(options.name))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if bytes.Equal(currentUnit, []byte(unit)) && bytes.Equal(currentConfig, data) {
		return "unchanged", nil
	}
	return "update", nil
}

func (service serviceDeclaration) startArgs(force, system bool) []string {
	args := []string{"--name", service.Name}
	if force {
		args = append(args,
			"--force", "--enable", "--no-cron-restart", "--clear-env", "--no-mise",
			"--no-start-limit", "--no-max-memory", "--clear-properties", "--clear-exec-hooks", "--clear-on-failure",
			"--restart", "always", "--restart-sec", "3s", "--env-file", "", "--interpreter", "", "--description", "",
			"--type", "", "--pid-file", "", "--timezone", "", "--instances", "0",
		)
		if system {
			args = append(args, "--user", "", "--group", "")
		}
	}
	appendValue := func(flag, value string) {
		if value != "" {
			args = append(args, flag, value)
		}
	}
	appendValue("--cwd", service.Cwd)
	appendValue("--description", service.Description)
	appendValue("--restart", service.Restart)
	appendValue("--restart-sec", service.RestartSec)
	appendValue("--start-limit-interval", service.StartLimitInterval)
	appendValue("--start-limit-burst", service.StartLimitBurst)
	appendValue("--cron-restart", service.CronRestart)
	appendValue("--env-file", service.EnvFile)
	appendValue("--interpreter", service.Interpreter)
	appendValue("--max-memory", service.MaxMemory)
	appendValue("--user", service.User)
	appendValue("--group", service.Group)
	appendValue("--type", service.Type)
	appendValue("--pid-file", service.PIDFile)
	appendValue("--timezone", service.Timezone)
	if service.Instances > 0 {
		args = append(args, "--instances", strconv.Itoa(service.Instances))
	}
	for _, value := range service.ExecStartPre {
		args = append(args, "--exec-start-pre", value)
	}
	for _, value := range service.ExecStartPost {
		args = append(args, "--exec-start-post", value)
	}
	appendValue("--exec-stop", service.ExecStop)
	appendValue("--exec-reload", service.ExecReload)
	for _, value := range service.Env {
		args = append(args, "--env", value)
	}
	for _, value := range service.Properties {
		args = append(args, "--property", value)
	}
	for _, value := range service.After {
		args = append(args, "--after", value)
	}
	for _, value := range service.Wants {
		args = append(args, "--wants", value)
	}
	for _, value := range service.Requires {
		args = append(args, "--requires", value)
	}
	for _, value := range service.OnFailure {
		args = append(args, "--on-failure", value)
	}
	if service.Mise {
		args = append(args, "--mise")
	}
	if service.EnableSet && !service.Enable {
		args = append(args, "--no-enable")
	}
	if service.EnableSet && service.Enable {
		args = append(args, "--enable")
	}
	for flag, value := range map[string]string{"--health-http": service.HealthHTTP, "--health-tcp": service.HealthTCP, "--health-command": service.HealthCommand, "--health-interval": service.HealthInterval, "--health-timeout": service.HealthTimeout, "--health-retry-delay": service.HealthRetryDelay, "--health-start-period": service.HealthStartPeriod, "--health-action": service.HealthAction} {
		if value != "" {
			args = append(args, flag, value)
		}
	}
	if service.HealthRetries > 0 {
		args = append(args, "--health-retries", strconv.Itoa(service.HealthRetries))
	}
	if service.Script != "" {
		args = append(args, service.Script)
		return append(args, service.Args...)
	}
	args = append(args, "--")
	return append(args, service.Command...)
}

func parseServiceDeclarations(path string) ([]serviceDeclaration, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve service declaration path: %w", err)
	}
	path = absolutePath
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open service declaration: %w", err)
	}
	defer file.Close()

	var services []serviceDeclaration
	var current *serviceDeclaration
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if line == "" {
			continue
		}
		if line == "[[services]]" || line == "[[service]]" {
			services = append(services, serviceDeclaration{})
			current = &services[len(services)-1]
			continue
		}
		if current == nil {
			return nil, fmt.Errorf("%s:%d: values must be inside [[services]]", path, lineNumber)
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, lineNumber)
		}
		if err := setDeclarationValue(current, strings.TrimSpace(key), strings.TrimSpace(raw)); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read service declaration: %w", err)
	}
	if len(services) == 0 {
		return nil, errors.New("service declaration contains no [[services]] entries")
	}
	seen := make(map[string]bool, len(services))
	for i, service := range services {
		if err := validateName(service.Name); err != nil {
			return nil, fmt.Errorf("service %d: %w", i+1, err)
		}
		if len(service.Command) == 0 && service.Script == "" {
			return nil, fmt.Errorf("service %q has no command or script", service.Name)
		}
		if len(service.Command) > 0 && service.Script != "" {
			return nil, fmt.Errorf("service %q cannot set both command and script", service.Name)
		}
		if len(service.Command) > 0 && service.Interpreter != "" {
			return nil, fmt.Errorf("service %q cannot use interpreter with an explicit command", service.Name)
		}
		if service.hasSchedule() && service.CronRestart != "" {
			return nil, fmt.Errorf("service %q cannot set both schedule and cron_restart", service.Name)
		}
		if service.hasSchedule() && (service.Instances > 0 || service.Type != "") {
			return nil, fmt.Errorf("service %q cannot set instances or type on a scheduled task", service.Name)
		}
		if err := validateTimerDeclaration(service); err != nil {
			return nil, fmt.Errorf("service %q: %w", service.Name, err)
		}
		if services[i].Cwd == "" {
			services[i].Cwd = filepath.Dir(path)
		}
		if seen[service.Name] {
			return nil, fmt.Errorf("service %q is declared more than once", service.Name)
		}
		seen[service.Name] = true
	}
	return services, nil
}

func setDeclarationValue(service *serviceDeclaration, key, raw string) error {
	stringValue := func() (string, error) {
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", fmt.Errorf("%s must be a double-quoted string", key)
		}
		return value, nil
	}
	arrayValue := func() ([]string, error) {
		var values []string
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return nil, fmt.Errorf("%s must be an array of double-quoted strings", key)
		}
		return values, nil
	}
	setString := func(target *string) error {
		value, err := stringValue()
		if err == nil {
			*target = value
		}
		return err
	}
	switch key {
	case "name":
		return setString(&service.Name)
	case "cwd":
		return setString(&service.Cwd)
	case "description":
		return setString(&service.Description)
	case "restart":
		return setString(&service.Restart)
	case "restart_sec":
		return setString(&service.RestartSec)
	case "start_limit_interval":
		return setString(&service.StartLimitInterval)
	case "start_limit_burst":
		return setString(&service.StartLimitBurst)
	case "cron_restart":
		return setString(&service.CronRestart)
	case "env_file":
		return setString(&service.EnvFile)
	case "interpreter":
		return setString(&service.Interpreter)
	case "max_memory":
		return setString(&service.MaxMemory)
	case "user":
		return setString(&service.User)
	case "group":
		return setString(&service.Group)
	case "type":
		return setString(&service.Type)
	case "pid_file":
		return setString(&service.PIDFile)
	case "timezone":
		return setString(&service.Timezone)
	case "instances":
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > maxInstances {
			return fmt.Errorf("instances must be an integer between 0 and %d", maxInstances)
		}
		service.Instances = value
		return nil
	case "command":
		values, err := arrayValue()
		if err == nil {
			service.Command = values
		}
		return err
	case "script":
		return setString(&service.Script)
	case "args":
		values, err := arrayValue()
		if err == nil {
			service.Args = values
		}
		return err
	case "env":
		values, err := arrayValue()
		if err == nil {
			service.Env = values
		}
		return err
	case "properties":
		values, err := arrayValue()
		if err == nil {
			service.Properties = values
		}
		return err
	case "exec_start_pre":
		values, err := arrayValue()
		if err == nil {
			service.ExecStartPre = values
		}
		return err
	case "exec_start_post":
		values, err := arrayValue()
		if err == nil {
			service.ExecStartPost = values
		}
		return err
	case "exec_stop":
		return setString(&service.ExecStop)
	case "exec_reload":
		return setString(&service.ExecReload)
	case "after":
		values, err := arrayValue()
		if err == nil {
			service.After = values
		}
		return err
	case "wants":
		values, err := arrayValue()
		if err == nil {
			service.Wants = values
		}
		return err
	case "requires":
		values, err := arrayValue()
		if err == nil {
			service.Requires = values
		}
		return err
	case "on_failure":
		values, err := arrayValue()
		if err == nil {
			service.OnFailure = values
		}
		return err
	case "schedule":
		return setString(&service.Schedule)
	case "timer_on_boot":
		return setString(&service.TimerOnBoot)
	case "timer_interval":
		return setString(&service.TimerInterval)
	case "timer_randomized_delay":
		return setString(&service.TimerRandomDelay)
	case "persistent":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("persistent must be true or false")
		}
		service.Persistent = value
		return nil
	case "health_http":
		return setString(&service.HealthHTTP)
	case "health_tcp":
		return setString(&service.HealthTCP)
	case "health_command":
		return setString(&service.HealthCommand)
	case "health_interval":
		return setString(&service.HealthInterval)
	case "health_timeout":
		return setString(&service.HealthTimeout)
	case "health_retry_delay":
		return setString(&service.HealthRetryDelay)
	case "health_start_period":
		return setString(&service.HealthStartPeriod)
	case "health_action":
		return setString(&service.HealthAction)
	case "health_retries":
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return errors.New("health_retries must be a positive integer")
		}
		service.HealthRetries = value
		return nil
	case "mise":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("mise must be true or false")
		}
		service.Mise = value
		return nil
	case "enable":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("enable must be true or false")
		}
		service.Enable, service.EnableSet = value, true
		return nil
	default:
		return fmt.Errorf("unknown service key %q", key)
	}
}

func stripTOMLComment(line string) string {
	quoted, escaped := false, false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '#' && !quoted {
			return line[:i]
		}
	}
	return line
}
