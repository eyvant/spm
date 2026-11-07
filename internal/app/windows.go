//go:build windows

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unsafe"
)

// The Windows backend deliberately uses only operating-system facilities. A
// small detached spm supervisor owns each user process. The current user's
// Startup folder handles user logon, while machine-wide services use the
// Windows Service Control Manager and are visible in services.msc.

const windowsStateVersion = 1

type windowsServiceConfig struct {
	Version          int      `json:"version"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Cwd              string   `json:"cwd"`
	Command          []string `json:"command"`
	Env              []string `json:"env,omitempty"`
	EnvFile          string   `json:"env_file,omitempty"`
	Restart          string   `json:"restart"`
	RestartSec       string   `json:"restart_sec"`
	Enabled          bool     `json:"enabled"`
	RunAs            string   `json:"run_as,omitempty"`
	After            []string `json:"after,omitempty"`
	Wants            []string `json:"wants,omitempty"`
	Requires         []string `json:"requires,omitempty"`
	Mode             string   `json:"mode,omitempty"`
	Schedule         string   `json:"schedule,omitempty"`
	Timezone         string   `json:"timezone,omitempty"`
	TimerOnBoot      string   `json:"timer_on_boot,omitempty"`
	TimerInterval    string   `json:"timer_interval,omitempty"`
	TimerRandomDelay string   `json:"timer_randomized_delay,omitempty"`
	Persistent       bool     `json:"persistent,omitempty"`
}

type windowsServiceState struct {
	Version       int       `json:"version"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	SupervisorPID int       `json:"supervisor_pid,omitempty"`
	PID           int       `json:"pid,omitempty"`
	Restarts      int       `json:"restarts"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	StoppedAt     time.Time `json:"stopped_at,omitempty"`
	ExitCode      *int      `json:"exit_code,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type windowsControl struct {
	Action      string `json:"action"`
	RequestedAt int64  `json:"requested_at"`
}

type windowsBackend struct {
	stdout io.Writer
	stderr io.Writer
	root   string
	system bool
}

func runWindows(args []string, stdout, stderr io.Writer, system bool) error {
	if len(args) > 0 && args[0] == "__windows-schedule-daemon" {
		if len(args) != 3 || args[1] != "--root" || !filepath.IsAbs(args[2]) {
			return errors.New("invalid Windows scheduler invocation")
		}
		root := filepath.Clean(args[2])
		log, closeLog := openPortableSchedulerLog(root, stderr)
		defer closeLog()
		return runPortableScheduler(context.Background(), root, triggerWindowsScheduledTask, log)
	}
	if len(args) > 0 && args[0] == "__windows-service" {
		if len(args) != 4 || args[1] != "--root" || !filepath.IsAbs(args[2]) {
			return errors.New("invalid Windows service invocation")
		}
		b := &windowsBackend{stdout: stdout, stderr: stderr, root: filepath.Clean(args[2]), system: true}
		return runWindowsSCMService(b, args[3])
	}
	if len(args) > 0 && args[0] == "__windows-supervise" {
		if len(args) != 4 || args[1] != "--root" || !filepath.IsAbs(args[2]) {
			return errors.New("invalid Windows supervisor invocation")
		}
		b := &windowsBackend{stdout: stdout, stderr: stderr, root: filepath.Clean(args[2])}
		return b.supervise(args[3])
	}
	if len(args) > 0 && args[0] == "__windows-launch" {
		if len(args) != 4 || args[1] != "--root" || !filepath.IsAbs(args[2]) {
			return errors.New("invalid Windows launcher invocation")
		}
		b := &windowsBackend{stdout: io.Discard, stderr: io.Discard, root: filepath.Clean(args[2])}
		return b.startManaged(args[3])
	}
	if system && !windowsIsElevated() {
		return errors.New("system mode requires an Administrator PowerShell terminal")
	}
	b, err := newWindowsBackend(stdout, stderr, system)
	if err != nil {
		return err
	}
	return b.run(args)
}

func newWindowsBackend(stdout, stderr io.Writer, system bool) (*windowsBackend, error) {
	root := os.Getenv("SPM_WINDOWS_DATA_DIR")
	if root == "" {
		if system {
			root = os.Getenv("ProgramData")
			if root == "" {
				root = filepath.Join(filepath.VolumeName(os.Getenv("SystemRoot"))+`\`, "ProgramData")
			}
			root = filepath.Join(root, "spm")
		} else {
			config, err := os.UserConfigDir()
			if err != nil {
				return nil, fmt.Errorf("resolve Windows configuration directory: %w", err)
			}
			root = filepath.Join(config, "spm")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve Windows data directory: %w", err)
	}
	return &windowsBackend{stdout: stdout, stderr: stderr, root: root, system: system}, nil
}

func platformCompletionApplication(stdout io.Writer, system bool) (*application, bool) {
	b, err := newWindowsBackend(io.Discard, io.Discard, system)
	if err != nil {
		return nil, false
	}
	return &application{stdout: stdout, stderr: io.Discard, system: system, managedNamesOverride: b.names}, true
}

func printPlatformHelp(w io.Writer) bool {
	fmt.Fprint(w, `spm - a simple, cross-platform manager for native system services

Usage:
  spm [--system] <command> [options]

Core commands:
  spm start [options] <script> [arguments]
  spm start [options] -- <command> [arguments]
  spm schedule [options] <script|-- command>
  spm run <name>
  spm list [--json] [--watch[=DURATION]]
  spm status|show|describe <name>
  spm logs [--lines N] [--follow] [--grep PATTERN] [name...|all]
  spm restart|stop|enable|disable <name...|all>
  spm delete <name...|all> [--yes]
  spm cat <name>
  spm apply [--dry-run] spm.toml
  spm validate|diff spm.toml
  spm native windows inspect <name>
  spm doctor
  spm completion powershell

Global options:
  --system                    Use Windows services in %ProgramData%\spm.
                              Requires Administrator and appears in services.msc.

Start options supported on Windows:
  -n, --name NAME             Service name
  --cwd DIRECTORY             Working directory
  --interpreter COMMAND       Script interpreter
  -e, --env KEY=VALUE         Environment variable, repeatable
  --env-file FILE             Environment file
  --restart POLICY            no, on-success, on-failure, on-abnormal,
                              on-abort, on-watchdog, always (default)
  --restart-sec DURATION      Restart delay, defaults to 3s
  --description TEXT          Description
  --wait                      Wait until the process is running
  --wait-timeout DURATION     Readiness timeout
  --wait-http URL             Wait for an HTTP response
  --wait-tcp ADDRESS          Wait for a TCP connection
  --wait-command COMMAND      Wait for a successful command
  --user ACCOUNT              With --system: SYSTEM, LocalService,
                              NetworkService, DOMAIN\User, or a local account
  --no-enable                 Run now without starting automatically
  --force                     Replace an existing service

Scopes:
  User services run as the current account and start at that user's logon.
  System services use SCM, run at boot, default to LocalSystem, and may select
  another Windows account with --user.
`)
	return true
}

func printPlatformCommandHelp(w io.Writer, command string) bool {
	switch command {
	case "start":
		fmt.Fprint(w, `Usage:
  spm start [options] <script> [arguments]
  spm start [options] -- <command> [arguments]
  spm start <name>

Run spm with no arguments for the Windows option list.
`)
	case "schedule":
		fmt.Fprintln(w, "Usage:\n  spm schedule [start options] [--cron EXPR|--interval DURATION|--on-boot DURATION] <script|-- command>")
	case "list", "ls", "status-all":
		fmt.Fprintln(w, "Usage:\n  spm list [--json] [--watch[=DURATION]]")
	case "logs":
		fmt.Fprintln(w, "Usage:\n  spm logs [--follow] [--lines N] [--grep PATTERN] [name...|all]")
	case "run", "status", "show", "describe", "cat":
		fmt.Fprintf(w, "Usage:\n  spm %s <name>\n", command)
	case "restart", "stop", "enable", "disable":
		fmt.Fprintf(w, "Usage:\n  spm %s <name...|all>\n", command)
	case "delete", "remove", "rm":
		fmt.Fprintln(w, "Usage:\n  spm delete <name...|all> [--yes]")
	case "doctor":
		fmt.Fprintln(w, "Usage:\n  spm doctor\n\nCheck Windows startup integration, storage, and the spm executable.")
	case "completion":
		fmt.Fprintln(w, "Usage:\n  spm completion powershell")
	case "version":
		fmt.Fprintln(w, "Usage:\n  spm version")
	default:
		return false
	}
	return true
}

func (b *windowsBackend) run(args []string) error {
	if len(args) == 0 {
		printHelp(b.stdout)
		return nil
	}
	switch args[0] {
	case "start":
		return b.start(args[1:])
	case "schedule":
		return b.schedule(args[1:])
	case "run":
		name, err := oneName(args[1:])
		if err != nil {
			return err
		}
		return b.startManaged(name)
	case "list", "ls", "status-all":
		options, err := parseList(args[1:])
		if err != nil {
			return err
		}
		return b.list(options)
	case "status", "show", "describe":
		name, err := oneName(args[1:])
		if err != nil {
			return err
		}
		return b.status(name)
	case "restart", "stop", "enable", "disable":
		return b.controlMany(args[0], args[1:])
	case "delete", "remove", "rm":
		return b.removeMany(args[1:])
	case "logs":
		return b.logs(args[1:])
	case "cat":
		name, err := oneName(args[1:])
		if err != nil {
			return err
		}
		data, err := os.ReadFile(b.configPath(name))
		if err != nil {
			return fmt.Errorf("read service %q: %w", name, err)
		}
		_, err = b.stdout.Write(data)
		return err
	case "doctor":
		if len(args) != 1 {
			return errors.New("doctor does not accept arguments")
		}
		return b.doctor()
	case "apply":
		return b.apply(args[1:])
	case "validate", "diff":
		return b.inspectDeclaration(args[0], args[1:])
	case "native":
		return b.native(args[1:])
	case "startup":
		if len(args) != 1 {
			return errors.New("startup does not accept arguments")
		}
		if b.system {
			fmt.Fprintln(b.stdout, "Enabled Windows system services start automatically at boot through the Service Control Manager.")
		} else {
			fmt.Fprintln(b.stdout, "Enabled Windows user services start automatically at logon through the user's Startup folder.")
		}
		return nil
	case "signal", "kill", "scale", "reload", "set", "unset", "clone", "history", "rollback", "security", "export", "health", "import", "edit":
		return fmt.Errorf("%s is not supported by the Windows backend yet", args[0])
	default:
		return fmt.Errorf("unknown command %q; run spm help", args[0])
	}
}

func (b *windowsBackend) native(args []string) error {
	if len(args) != 3 || args[0] != "windows" || args[1] != "inspect" {
		return errors.New("usage: spm native windows inspect <name>")
	}
	name := args[2]
	if err := validateName(name); err != nil {
		return err
	}
	if !b.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	if !b.system {
		fmt.Fprintf(b.stdout, "Startup shortcut: %s\n", b.startupPath(name))
		fmt.Fprintf(b.stdout, "Configuration: %s\n", b.configPath(name))
		return nil
	}
	return b.inspectSCMService(name, b.stdout, b.stderr)
}

func (b *windowsBackend) ensureDirectories() error {
	for _, directory := range []string{b.root, b.servicesDir()} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
	}
	return nil
}

func (b *windowsBackend) servicesDir() string { return filepath.Join(b.root, "services") }
func (b *windowsBackend) serviceDir(name string) string {
	return filepath.Join(b.servicesDir(), name)
}
func (b *windowsBackend) configPath(name string) string {
	return filepath.Join(b.serviceDir(name), "config.json")
}
func (b *windowsBackend) statePath(name string) string {
	return filepath.Join(b.serviceDir(name), "state.json")
}
func (b *windowsBackend) controlPath(name string) string {
	return filepath.Join(b.serviceDir(name), "control.json")
}
func (b *windowsBackend) logPath(name string) string {
	return filepath.Join(b.serviceDir(name), "service.log")
}

func (b *windowsBackend) exists(name string) bool {
	info, err := os.Stat(b.configPath(name))
	return err == nil && info.Mode().IsRegular()
}

func (b *windowsBackend) names() ([]string, error) {
	entries, err := os.ReadDir(b.servicesDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Windows services: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			name := entry.Name()
			if validateName(name) == nil && b.exists(name) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

func (b *windowsBackend) readConfig(name string) (windowsServiceConfig, error) {
	var config windowsServiceConfig
	data, err := os.ReadFile(b.configPath(name))
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, err
	}
	if config.Version != windowsStateVersion || config.Name != name || len(config.Command) == 0 {
		return config, fmt.Errorf("invalid Windows service configuration for %q", name)
	}
	return config, nil
}

func (b *windowsBackend) writeConfig(config windowsServiceConfig) error {
	if err := b.ensureDirectories(); err != nil {
		return err
	}
	if err := os.MkdirAll(b.serviceDir(config.Name), 0o700); err != nil {
		return fmt.Errorf("create Windows service directory: %w", err)
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(b.configPath(config.Name), data, 0o600)
}

func (b *windowsBackend) readState(name string) windowsServiceState {
	var state windowsServiceState
	data, err := os.ReadFile(b.statePath(name))
	if err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.Name == "" {
		state.Name = name
		state.Version = windowsStateVersion
		state.Status = "stopped"
	}
	if state.PID > 0 && !windowsProcessAlive(state.PID) {
		state.PID = 0
		if state.Status == "running" || state.Status == "starting" {
			state.Status = "stopped"
		}
	}
	if state.SupervisorPID > 0 && !windowsProcessAlive(state.SupervisorPID) {
		state.SupervisorPID = 0
	}
	return state
}

func (b *windowsBackend) writeState(state windowsServiceState) error {
	if err := b.ensureDirectories(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(b.statePath(state.Name), append(data, '\n'), 0o600)
}

func (b *windowsBackend) start(args []string) error {
	options, err := parseStart(args)
	if err != nil {
		return err
	}
	if len(options.command) == 1 && options.name == "" && !options.explicitCmd && b.exists(options.command[0]) {
		return b.startManaged(options.command[0])
	}
	if options.name == "" {
		options.name = inferName(options.command, options.explicitCmd)
	}
	if b.exists(options.name) && !options.force {
		return fmt.Errorf("service %q already exists; use --force to replace it", options.name)
	}
	runAs, err := windowsRunAs(options.runAsUser, b.system)
	if err != nil {
		return err
	}
	if options.runAsGroup != "" {
		return errors.New("--group has no Windows equivalent; grant the service account access through Windows ACLs")
	}
	// The shared validator checks Unix account syntax. Windows account names may
	// contain a domain separator, spaces, and a trailing $ (gMSA), so validate
	// them above and keep them out of the Unix-specific path.
	options.runAsUser, options.runAsGroup = "", ""
	if err := rejectWindowsStartOptions(options); err != nil {
		return err
	}
	if err := options.prepare(); err != nil {
		return err
	}
	if b.exists(options.name) {
		if err := b.stop(options.name); err != nil {
			return err
		}
		_ = b.unregister(options.name)
	}
	config := windowsServiceConfig{
		Version: windowsStateVersion, Name: options.name, Description: options.description,
		Cwd: options.cwd, Command: append([]string(nil), options.command...), Env: append([]string(nil), options.env...),
		EnvFile: options.envFile, Restart: options.restart, RestartSec: options.restartSec, Enabled: options.enable,
		RunAs: runAs, After: append([]string(nil), options.after...), Wants: append([]string(nil), options.wants...), Requires: append([]string(nil), options.requires...),
	}
	if err := b.writeConfig(config); err != nil {
		return fmt.Errorf("write Windows service config: %w", err)
	}
	if b.system {
		if err := b.register(config.Name); err != nil {
			_ = os.RemoveAll(b.serviceDir(config.Name))
			return err
		}
		if err := b.secureSystemData(config.Name, config.RunAs); err != nil {
			_ = b.unregister(config.Name)
			_ = os.RemoveAll(b.serviceDir(config.Name))
			return err
		}
	} else if config.Enabled {
		if err := b.register(config.Name); err != nil {
			_ = os.RemoveAll(b.serviceDir(config.Name))
			return err
		}
	}
	if err := b.startManaged(config.Name); err != nil {
		return err
	}
	if options.wait {
		if err := b.waitRunning(config.Name, options.waitTimeout); err != nil {
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
	fmt.Fprintf(b.stdout, "%s started on Windows (pid %d)\n", config.Name, b.readState(config.Name).PID)
	return nil
}

func rejectWindowsStartOptions(o startOptions) error {
	var unsupported []string
	checks := []struct {
		set  bool
		name string
	}{
		{o.startLimitInterval != "" || o.startLimitBurst != "", "start limits"},
		{o.cronRestart != "", "--cron-restart"}, {o.maxMemory != "", "--max-memory"},
		{len(o.properties) > 0, "systemd resource/property options"},
		{len(o.execStartPre)+len(o.execStartPost) > 0 || o.execStop != "" || o.execReload != "", "systemd lifecycle hooks"},
		{len(o.onFailure) > 0, "--on-failure handlers"},
		{o.health.Probe.Kind != "", "health checks"}, {o.serviceType != "", "--type"}, {o.pidFile != "", "--pid-file"},
		{o.timezone != "", "--timezone"}, {o.instances != 0, "--instances"}, {o.sandbox != "", "--sandbox"}, {o.mise, "--mise"},
	}
	for _, check := range checks {
		if check.set {
			unsupported = append(unsupported, check.name)
		}
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("windows backend does not support %s", strings.Join(unsupported, ", "))
	}
	return nil
}

func (b *windowsBackend) apply(args []string) error {
	dryRun := false
	path := ""
	for _, arg := range args {
		if arg == "--dry-run" {
			dryRun = true
		} else if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown apply option %q", arg)
		} else if path != "" {
			return errors.New("apply requires exactly one spm.toml path")
		} else {
			path = arg
		}
	}
	if path == "" {
		return errors.New("apply requires exactly one spm.toml path")
	}
	services, err := parseServiceDeclarations(path)
	if err != nil {
		return err
	}
	if dryRun {
		return b.printDeclarationPlan(services, false)
	}
	var errs []error
	for _, service := range services {
		var applyErr error
		if service.hasSchedule() {
			var scheduleArgs []string
			for _, option := range [][2]string{{"--cron", service.Schedule}, {"--on-boot", service.TimerOnBoot}, {"--interval", service.TimerInterval}, {"--randomized-delay", service.TimerRandomDelay}} {
				if option[1] != "" {
					scheduleArgs = append(scheduleArgs, option[0], option[1])
				}
			}
			if service.Persistent {
				scheduleArgs = append(scheduleArgs, "--persistent")
			}
			applyErr = b.schedule(append(scheduleArgs, service.startArgs(b.exists(service.Name), b.system)...))
		} else {
			applyErr = b.start(service.startArgs(b.exists(service.Name), b.system))
		}
		if applyErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", service.Name, applyErr))
		}
	}
	return errors.Join(errs...)
}

func (b *windowsBackend) inspectDeclaration(action string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%s requires exactly one spm.toml path", action)
	}
	services, err := parseServiceDeclarations(args[0])
	if err != nil {
		return err
	}
	if action == "validate" {
		for _, service := range services {
			if _, err := b.prepareDeclaration(service); err != nil {
				return fmt.Errorf("%s: %w", service.Name, err)
			}
		}
		fmt.Fprintf(b.stdout, "%s is valid for Windows (%d services)\n", args[0], len(services))
		return nil
	}
	return b.printDeclarationPlan(services, true)
}

func (b *windowsBackend) prepareDeclaration(service serviceDeclaration) (windowsServiceConfig, error) {
	options, err := parseStart(service.startArgs(b.exists(service.Name), b.system))
	if err != nil {
		return windowsServiceConfig{}, err
	}
	runAs, err := windowsRunAs(options.runAsUser, b.system)
	if err != nil {
		return windowsServiceConfig{}, err
	}
	if options.runAsGroup != "" {
		return windowsServiceConfig{}, errors.New("group has no Windows equivalent")
	}
	options.runAsUser, options.runAsGroup = "", ""
	timezone := options.timezone
	if service.hasSchedule() {
		options.timezone = ""
		if b.system && !windowsScheduledServiceAccount(runAs) {
			return windowsServiceConfig{}, fmt.Errorf("scheduled system tasks currently require SYSTEM, LocalService, or NetworkService; got %q", runAs)
		}
	}
	if err := rejectWindowsStartOptions(options); err != nil {
		return windowsServiceConfig{}, err
	}
	if err := options.prepare(); err != nil {
		return windowsServiceConfig{}, err
	}
	config := windowsServiceConfig{Version: windowsStateVersion, Name: options.name, Description: options.description, Cwd: options.cwd, Command: append([]string(nil), options.command...), Env: append([]string(nil), options.env...), EnvFile: options.envFile, Restart: options.restart, RestartSec: options.restartSec, Enabled: options.enable, RunAs: runAs, After: append([]string(nil), options.after...), Wants: append([]string(nil), options.wants...), Requires: append([]string(nil), options.requires...)}
	if service.hasSchedule() {
		config.Mode, config.Restart = "schedule", "no"
		config.Schedule, config.Timezone, config.TimerOnBoot = service.Schedule, timezone, service.TimerOnBoot
		config.TimerInterval, config.TimerRandomDelay, config.Persistent = service.TimerInterval, service.TimerRandomDelay, service.Persistent
		record := portableScheduleRecord{Version: 1, Name: config.Name, Command: config.Command, Cwd: config.Cwd, Schedule: config.Schedule, Timezone: config.Timezone, TimerOnBoot: config.TimerOnBoot, TimerInterval: config.TimerInterval, TimerRandomDelay: config.TimerRandomDelay, Persistent: config.Persistent, CreatedAt: time.Now()}
		if err := validatePortableSchedule(record); err != nil {
			return windowsServiceConfig{}, err
		}
	}
	return config, nil
}

func (b *windowsBackend) printDeclarationPlan(services []serviceDeclaration, detailed bool) error {
	for _, service := range services {
		desired, err := b.prepareDeclaration(service)
		if err != nil {
			return fmt.Errorf("%s: %w", service.Name, err)
		}
		action := "create"
		if b.exists(service.Name) {
			current, readErr := b.readConfig(service.Name)
			if readErr != nil {
				return readErr
			}
			action = "update"
			currentData, _ := json.Marshal(current)
			desiredData, _ := json.Marshal(desired)
			if string(currentData) == string(desiredData) {
				action = "unchanged"
			}
			if detailed && action == "update" {
				fmt.Fprintf(b.stdout, "update\t%s\n-  config = %s\n+  config = %s\n", service.Name, currentData, desiredData)
				continue
			}
		}
		fmt.Fprintf(b.stdout, "%s\t%s\n", action, service.Name)
	}
	return nil
}

func (b *windowsBackend) startManaged(name string) error {
	return b.startManagedStack(name, map[string]bool{})
}

func (b *windowsBackend) startManagedStack(name string, stack map[string]bool) error {
	if !b.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	if stack[name] {
		return fmt.Errorf("dependency cycle includes %q", name)
	}
	stack[name] = true
	defer delete(stack, name)
	config, err := b.readConfig(name)
	if err != nil {
		return err
	}
	if config.Mode == "schedule" {
		return b.runScheduledTask(name, true)
	}
	for _, dependency := range config.Requires {
		if !b.exists(dependency) {
			return fmt.Errorf("required service %q is not managed by spm", dependency)
		}
		if err := b.startManagedStack(dependency, stack); err != nil {
			return fmt.Errorf("start required service %q: %w", dependency, err)
		}
		if err := b.waitRunning(dependency, 30*time.Second); err != nil {
			return fmt.Errorf("wait for required service %q: %w", dependency, err)
		}
	}
	for _, dependency := range config.Wants {
		if !b.exists(dependency) {
			continue
		}
		if err := b.startManagedStack(dependency, stack); err != nil {
			fmt.Fprintf(b.stderr, "warning: wanted service %s did not start: %v\n", dependency, err)
		}
	}
	for _, dependency := range config.After {
		state := b.readState(dependency)
		if state.SupervisorPID > 0 || state.PID > 0 {
			if err := b.waitRunning(dependency, 30*time.Second); err != nil {
				return fmt.Errorf("wait for service %q: %w", dependency, err)
			}
		}
	}
	state := b.readState(name)
	if state.SupervisorPID > 0 && windowsProcessAlive(state.SupervisorPID) {
		if b.system && !b.scmInstalled(name) {
			if err := b.stopLegacySystemTask(name); err != nil {
				return err
			}
		} else {
			if state.PID > 0 {
				return nil
			}
			return fmt.Errorf("service %q supervisor is already starting", name)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve spm executable: %w", err)
	}
	if err := b.ensureDirectories(); err != nil {
		return err
	}
	_ = os.Remove(b.controlPath(name))
	if b.system {
		return b.startSystemManaged(name)
	}
	cmd := exec.Command(executable, "__windows-supervise", "--root", b.root, name)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200, HideWindow: true}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Windows supervisor: %w", err)
	}
	_ = cmd.Process.Release()
	if err := b.waitSupervisor(name, 5*time.Second); err != nil {
		return err
	}
	return b.waitRunning(name, 5*time.Second)
}

func (b *windowsBackend) startSystemManaged(name string) error {
	if !b.scmInstalled(name) {
		if err := b.removeLegacySystemTask(name); err != nil {
			return err
		}
		if err := b.installSCMService(name); err != nil {
			return err
		}
	}
	return b.startSCMService(name)
}

func (b *windowsBackend) waitSupervisor(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state := b.readState(name)
		if state.SupervisorPID > 0 && windowsProcessAlive(state.SupervisorPID) {
			return nil
		}
		if state.Status == "failed" && state.Error != "" {
			return fmt.Errorf("start %s: %s", name, state.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for Windows supervisor for %s", name)
}

func (b *windowsBackend) waitRunning(name string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state := b.readState(name)
		if state.Status == "running" && state.PID > 0 {
			return nil
		}
		if state.Status == "failed" {
			return fmt.Errorf("service %s failed: %s", name, valueOr(state.Error, "process exited"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s to run", name)
}

func (b *windowsBackend) supervise(name string) error {
	config, err := b.readConfig(name)
	if err != nil {
		return fmt.Errorf("read supervisor config: %w", err)
	}
	if err := b.ensureDirectories(); err != nil {
		return err
	}
	log, err := os.OpenFile(b.logPath(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	state := windowsServiceState{Version: windowsStateVersion, Name: name, Status: "starting", SupervisorPID: os.Getpid()}
	_ = b.writeState(state)
	delay, err := time.ParseDuration(config.RestartSec)
	if err != nil || delay < 0 {
		delay = 3 * time.Second
	}
	for {
		fmt.Fprintf(log, "[%s] spm: starting %s\r\n", time.Now().Format(time.RFC3339), strings.Join(config.Command, " "))
		cmd := exec.Command(config.Command[0], config.Command[1:]...)
		cmd.Dir = config.Cwd
		cmd.Env = windowsEnvironment(config)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.Stdin = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		started := time.Now()
		if err := cmd.Start(); err != nil {
			state.Status, state.Error, state.PID, state.SupervisorPID = "failed", err.Error(), 0, 0
			state.StoppedAt = time.Now()
			fmt.Fprintf(log, "[%s] spm: start failed: %v\r\n", time.Now().Format(time.RFC3339), err)
			_ = log.Close()
			_ = b.writeState(state)
			return nil
		}
		job, jobErr := windowsCreateKillJob(cmd.Process.Pid)
		if jobErr != nil {
			fmt.Fprintf(log, "[%s] spm: warning: cannot create process job: %v\r\n", time.Now().Format(time.RFC3339), jobErr)
		}
		state.Status, state.Error, state.PID, state.StartedAt, state.StoppedAt, state.ExitCode = "running", "", cmd.Process.Pid, started, time.Time{}, nil
		_ = b.writeState(state)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		ticker := time.NewTicker(200 * time.Millisecond)
		action := ""
		var waitErr error
	waitLoop:
		for {
			select {
			case waitErr = <-done:
				break waitLoop
			case <-ticker.C:
				control, ok := b.takeControl(name)
				if ok && (control.Action == "stop" || control.Action == "restart") {
					action = control.Action
					if job != 0 {
						windowsCloseHandle(job)
						job = 0
					} else {
						_ = cmd.Process.Kill()
					}
					waitErr = <-done
					break waitLoop
				}
			}
		}
		ticker.Stop()
		if job != 0 {
			windowsCloseHandle(job)
		}
		exitCode := cmd.ProcessState.ExitCode()
		state.PID, state.ExitCode, state.StoppedAt = 0, &exitCode, time.Now()
		fmt.Fprintf(log, "[%s] spm: process exited with code %d\r\n", time.Now().Format(time.RFC3339), exitCode)
		if action == "stop" {
			state.Status, state.SupervisorPID = "stopped", 0
			_ = log.Close()
			_ = b.writeState(state)
			return nil
		}
		if action == "restart" || windowsShouldRestart(config.Restart, waitErr, exitCode) {
			state.Status = "restarting"
			state.Restarts++
			_ = b.writeState(state)
			if action != "restart" && delay > 0 {
				time.Sleep(delay)
			}
			continue
		}
		state.SupervisorPID = 0
		if exitCode == 0 {
			state.Status = "stopped"
		} else {
			state.Status = "failed"
			state.Error = fmt.Sprintf("process exited with code %d", exitCode)
		}
		_ = log.Close()
		_ = b.writeState(state)
		return nil
	}
}

func windowsShouldRestart(policy string, waitErr error, exitCode int) bool {
	switch policy {
	case "always":
		return true
	case "on-success":
		return exitCode == 0
	case "on-failure", "on-abnormal", "on-abort", "on-watchdog":
		return waitErr != nil || exitCode != 0
	default:
		return false
	}
}

func windowsEnvironment(config windowsServiceConfig) []string {
	env := append([]string(nil), os.Environ()...)
	if config.EnvFile != "" {
		if file, err := os.Open(config.EnvFile); err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if key, value, ok := strings.Cut(line, "="); ok && key != "" {
					value = strings.Trim(value, "\"'")
					env = setWindowsEnv(env, key, value)
				}
			}
			_ = file.Close()
		}
	}
	for _, item := range config.Env {
		if key, value, ok := strings.Cut(item, "="); ok {
			env = setWindowsEnv(env, key, value)
		}
	}
	return env
}

func setWindowsEnv(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	for i, item := range env {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

func (b *windowsBackend) takeControl(name string) (windowsControl, bool) {
	var control windowsControl
	data, err := os.ReadFile(b.controlPath(name))
	if err != nil || json.Unmarshal(data, &control) != nil {
		return control, false
	}
	if err := os.Remove(b.controlPath(name)); err != nil {
		return control, false
	}
	return control, true
}

func (b *windowsBackend) sendControl(name, action string) error {
	state := b.readState(name)
	if state.SupervisorPID == 0 || !windowsProcessAlive(state.SupervisorPID) {
		if action == "stop" {
			return nil
		}
		return b.startManaged(name)
	}
	if err := b.writeControl(name, action); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		next := b.readState(name)
		if action == "stop" && next.SupervisorPID == 0 {
			return nil
		} else if action == "restart" && next.Restarts > state.Restarts && next.PID > 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if action == "stop" {
		windowsKillTree(state.SupervisorPID)
		return nil
	}
	return fmt.Errorf("timed out waiting for %s to %s", name, action)
}

func (b *windowsBackend) writeControl(name, action string) error {
	control := windowsControl{Action: action, RequestedAt: time.Now().UnixNano()}
	data, _ := json.Marshal(control)
	return writeFileAtomic(b.controlPath(name), data, 0o600)
}

func (b *windowsBackend) stop(name string) error {
	if config, err := b.readConfig(name); err == nil && config.Mode == "schedule" {
		for _, kind := range []string{"timer", "boot"} {
			command := windowsExecCommand("schtasks.exe", "/End", "/TN", b.scheduleTaskName(name, kind))
			command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			_ = command.Run()
		}
		return nil
	}
	if b.system {
		return b.stopSCMService(name)
	}
	return b.sendControl(name, "stop")
}

func (b *windowsBackend) controlMany(action string, selectors []string) error {
	names, err := b.selectNames(selectors, false)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		config, _ := b.readConfig(name)
		if config.Mode == "schedule" {
			switch action {
			case "stop":
				err = b.stop(name)
			case "restart":
				err = b.runScheduledTask(name, true)
			case "enable":
				err = b.setScheduledEnabled(config, true)
			case "disable":
				err = b.setScheduledEnabled(config, false)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			} else {
				fmt.Fprintf(b.stdout, "%s %s\n", name, actionPastTense(action))
			}
			continue
		}
		switch action {
		case "stop":
			err = b.stop(name)
		case "restart":
			if b.system {
				err = b.stopSCMService(name)
				if err == nil {
					err = b.startSystemManaged(name)
				}
			} else {
				err = b.sendControl(name, "restart")
			}
		case "enable":
			err = b.setEnabled(name, true)
		case "disable":
			err = b.setEnabled(name, false)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else {
			fmt.Fprintf(b.stdout, "%s %s\n", name, actionPastTense(action))
		}
	}
	return errors.Join(errs...)
}

func (b *windowsBackend) setEnabled(name string, enabled bool) error {
	config, err := b.readConfig(name)
	if err != nil {
		return err
	}
	if config.Mode == "schedule" {
		return b.setScheduledEnabled(config, enabled)
	}
	if b.system {
		previous := config.Enabled
		config.Enabled = enabled
		if err := b.writeConfig(config); err != nil {
			return err
		}
		if !b.scmInstalled(name) {
			err = b.installSCMService(name)
		} else {
			err = b.configureSCMStartType(name, enabled)
		}
		if err == nil && enabled {
			err = b.startSystemManaged(name)
		} else if err == nil {
			err = b.stopSCMService(name)
		}
		if err != nil {
			config.Enabled = previous
			_ = b.writeConfig(config)
			return err
		}
		return nil
	}
	if enabled {
		config.Enabled = true
		if err := b.writeConfig(config); err != nil {
			return err
		}
		if err := b.register(name); err != nil {
			config.Enabled = false
			_ = b.writeConfig(config)
			return err
		}
		if err := b.startManaged(name); err != nil {
			return err
		}
	} else {
		if err := b.unregister(name); err != nil {
			return err
		}
		if err := b.stop(name); err != nil {
			return err
		}
	}
	config.Enabled = enabled
	return b.writeConfig(config)
}

func (b *windowsBackend) register(name string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if b.system {
		return b.installSCMService(name)
	} else {
		return b.registerUserStartup(name, executable)
	}
}

func (b *windowsBackend) registerUserStartup(name, executable string) error {
	startup, err := windowsStartupDirectory()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(startup, 0o700); err != nil {
		return fmt.Errorf("create Windows Startup directory: %w", err)
	}
	arguments := "__windows-launch --root " + windowsQuoteCommand(b.root) + " " + windowsQuoteCommand(name)
	cmd := windowsExecCommand("powershell.exe", "-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", windowsCreateShortcutScript)
	cmd.Env = append(os.Environ(), "SPM_SHORTCUT_PATH="+b.startupPath(name), "SPM_SHORTCUT_TARGET="+executable, "SPM_SHORTCUT_ARGS="+arguments, "SPM_SHORTCUT_DESCRIPTION=Start an spm user service at logon")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create user logon shortcut: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

const windowsCreateShortcutScript = `$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$shortcut = $shell.CreateShortcut($env:SPM_SHORTCUT_PATH)
$shortcut.TargetPath = $env:SPM_SHORTCUT_TARGET
$shortcut.Arguments = $env:SPM_SHORTCUT_ARGS
$shortcut.WorkingDirectory = Split-Path -Parent $env:SPM_SHORTCUT_TARGET
$shortcut.WindowStyle = 7
$shortcut.Description = $env:SPM_SHORTCUT_DESCRIPTION
$shortcut.Save()
`

func windowsRunAs(value string, system bool) (string, error) {
	if !system {
		if value != "" {
			return "", errors.New("--user requires global --system mode on Windows")
		}
		return "", nil
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "SYSTEM", nil
	}
	if strings.ContainsAny(value, "\r\n\"") || len(value) > 256 {
		return "", fmt.Errorf("invalid Windows service account %q", value)
	}
	switch strings.ToUpper(strings.ReplaceAll(value, " ", "")) {
	case "SYSTEM", "LOCALSYSTEM", `NTAUTHORITY\SYSTEM`:
		return "SYSTEM", nil
	case "LOCALSERVICE", `NTAUTHORITY\LOCALSERVICE`:
		return `NT AUTHORITY\LOCALSERVICE`, nil
	case "NETWORKSERVICE", `NTAUTHORITY\NETWORKSERVICE`:
		return `NT AUTHORITY\NETWORKSERVICE`, nil
	default:
		return value, nil
	}
}

func windowsBuiltInAccount(value string) bool {
	upper := strings.ToUpper(strings.ReplaceAll(value, " ", ""))
	return upper == "SYSTEM" || upper == `NTAUTHORITY\SYSTEM` || upper == `NTAUTHORITY\LOCALSERVICE` || upper == `NTAUTHORITY\NETWORKSERVICE`
}

func (b *windowsBackend) secureSystemData(name, runAs string) error {
	rootGrants := windowsSystemRootACLGrants()
	// Secure the existing services container before its parent. ACL changes do
	// not propagate to existing children, and every service account must be able
	// to traverse both containers before reaching its isolated directory.
	for _, directory := range []string{b.servicesDir(), b.root} {
		rootArgs := []string{directory, "/inheritance:r", "/grant:r"}
		rootArgs = append(rootArgs, rootGrants...)
		rootArgs = append(rootArgs, "/Q")
		rootCommand := windowsExecCommand("icacls.exe", rootArgs...)
		rootCommand.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if output, err := rootCommand.CombinedOutput(); err != nil {
			return fmt.Errorf("secure system service container %s: %s: %w", directory, strings.TrimSpace(string(output)), err)
		}
	}
	grants := []string{"*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F"}
	switch strings.ToUpper(strings.ReplaceAll(runAs, " ", "")) {
	case "SYSTEM", `NTAUTHORITY\SYSTEM`:
	case `NTAUTHORITY\LOCALSERVICE`:
		grants = append(grants, "*S-1-5-19:(OI)(CI)M")
	case `NTAUTHORITY\NETWORKSERVICE`:
		grants = append(grants, "*S-1-5-20:(OI)(CI)M")
	default:
		grants = append(grants, runAs+":(OI)(CI)M")
	}
	args := []string{b.serviceDir(name), "/inheritance:r", "/grant:r"}
	args = append(args, grants...)
	args = append(args, "/Q")
	cmd := windowsExecCommand("icacls.exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("secure system service data for %s: %s: %w", runAs, strings.TrimSpace(string(output)), err)
	}
	// Apply inheritable ACEs to the directory only. Applying /inheritance:r
	// recursively removes effective inherited permissions from ordinary files.
	// Reset children afterwards so they inherit the completed directory ACL.
	entries, err := os.ReadDir(b.serviceDir(name))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		resetCommand := windowsExecCommand("icacls.exe", filepath.Join(b.serviceDir(name), entry.Name()), "/reset", "/T", "/Q")
		resetCommand.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if output, err := resetCommand.CombinedOutput(); err != nil {
			return fmt.Errorf("inherit system service file ACLs: %s: %w", strings.TrimSpace(string(output)), err)
		}
	}
	return nil
}

func windowsSystemRootACLGrants() []string {
	return []string{
		"*S-1-5-18:(OI)(CI)F",     // SYSTEM
		"*S-1-5-32-544:(OI)(CI)F", // Administrators
		"*S-1-5-19:(RX)",          // LocalService: traverse to its isolated service directory
		"*S-1-5-20:(RX)",          // NetworkService: traverse to its isolated service directory
		"*S-1-5-11:(RX)",          // Authenticated custom, gMSA, and virtual service accounts
	}
}

func (b *windowsBackend) unregister(name string) error {
	if config, err := b.readConfig(name); err == nil && config.Mode == "schedule" {
		return b.unregisterScheduledTask(name)
	}
	if !b.system {
		if err := os.Remove(b.startupPath(name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove user logon shortcut: %w", err)
		}
		return nil
	}
	return b.deleteSCMService(name)
}

func windowsStartupDirectory() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		var err error
		appData, err = os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve Windows Startup directory: %w", err)
		}
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup"), nil
}

func (b *windowsBackend) startupPath(name string) string {
	directory, _ := windowsStartupDirectory()
	return filepath.Join(directory, "spm-"+name+".lnk")
}

func windowsQuoteCommand(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

func (b *windowsBackend) list(options listOptions) error {
	if options.watch {
		for {
			if supportsColor(b.stdout) {
				fmt.Fprint(b.stdout, "\x1b[H\x1b[2J")
			}
			if err := b.list(listOptions{json: options.json}); err != nil {
				return err
			}
			time.Sleep(options.watchInterval)
		}
	}
	names, err := b.names()
	if err != nil {
		return err
	}
	rows := make([]listRow, 0, len(names))
	for _, name := range names {
		config, configErr := b.readConfig(name)
		if configErr != nil {
			continue
		}
		if config.Mode == "schedule" {
			status := "disabled"
			active := "inactive"
			if config.Enabled {
				status, active = "waiting", "active"
			}
			kind := "timer"
			if config.Schedule == "" && config.TimerInterval == "" {
				kind = "boot"
			}
			rows = append(rows, listRow{Name: name, Unit: b.scheduleTaskName(name, kind), Status: status, ActiveState: active, SubState: status, Health: "-", Enabled: map[bool]string{true: "enabled", false: "disabled"}[config.Enabled], Schedule: portableScheduleSummary(config.Schedule, config.Timezone, config.TimerOnBoot, config.TimerInterval, config.TimerRandomDelay)})
			continue
		}
		state := b.readState(name)
		row := listRow{Name: name, Unit: "windows:" + name, Status: state.Status, ActiveState: state.Status, SubState: state.Status, Health: "-", PID: state.PID, Restarts: state.Restarts, Enabled: map[bool]string{true: "enabled", false: "disabled"}[config.Enabled]}
		if state.PID > 0 && !state.StartedAt.IsZero() {
			seconds := int64(time.Since(state.StartedAt) / time.Second)
			row.UptimeSeconds = &seconds
		}
		rows = append(rows, row)
	}
	a := &application{stdout: b.stdout}
	return a.printListRows(rows, options.json)
}

func (b *windowsBackend) status(name string) error {
	config, err := b.readConfig(name)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("service %q is not managed by spm", name)
		}
		return err
	}
	state := b.readState(name)
	uptime := "-"
	if state.PID > 0 && !state.StartedAt.IsZero() {
		uptime = time.Since(state.StartedAt).Truncate(time.Second).String()
	}
	w := tabwriter.NewWriter(b.stdout, 0, 4, 2, ' ', 0)
	scope := "user"
	account := valueOr(os.Getenv("USERNAME"), "current user")
	if b.system {
		scope, account = "system", valueOr(config.RunAs, "SYSTEM")
	}
	rows := [][2]string{{"status", state.Status}, {"name", name}, {"scope", scope}, {"account", account}}
	if config.Mode == "schedule" {
		scheduleState := readPortableScheduleState(filepath.Join(b.scheduleStateDir(name), "schedule-state.json"))
		status := "disabled"
		if config.Enabled {
			status = "waiting"
		}
		rows = [][2]string{{"status", status}, {"name", name}, {"scope", scope}, {"account", account}, {"enabled", strconv.FormatBool(config.Enabled)}, {"schedule", valueOr(config.Schedule, "-")}, {"interval", valueOr(config.TimerInterval, "-")}, {"on boot", valueOr(config.TimerOnBoot, "-")}, {"timezone", valueOr(config.Timezone, "local")}, {"last scheduled", formatScheduleTime(scheduleState.LastScheduledAt)}, {"last finished", formatScheduleTime(scheduleState.LastFinishedAt)}, {"last exit code", scheduleExitCode(scheduleState.LastExitCode)}, {"command", strings.Join(config.Command, " ")}, {"cwd", config.Cwd}, {"log", b.logPath(name)}}
		for _, row := range rows {
			fmt.Fprintf(w, "%s\t%s\n", row[0], row[1])
		}
		return w.Flush()
	}
	if b.system {
		rows = append(rows, [2]string{"SCM service", b.scmName(name)})
	}
	rows = append(rows, [][2]string{{"enabled", strconv.FormatBool(config.Enabled)}, {"pid", valueOr(strconv.Itoa(state.PID), "-")}, {"supervisor pid", strconv.Itoa(state.SupervisorPID)}, {"restarts", strconv.Itoa(state.Restarts)}, {"uptime", uptime}, {"command", strings.Join(config.Command, " ")}, {"cwd", config.Cwd}, {"log", b.logPath(name)}}...)
	for _, row := range rows {
		value := row[1]
		if value == "0" && (row[0] == "pid" || row[0] == "supervisor pid") {
			value = "-"
		}
		fmt.Fprintf(w, "%s\t%s\n", row[0], value)
	}
	return w.Flush()
}

func (b *windowsBackend) logs(args []string) error {
	options, err := parseLogs(args)
	if err != nil {
		return err
	}
	if options.since != "" || options.until != "" || options.output != "" || options.boot || options.health {
		return errors.New("windows logs currently support service names, --lines, --follow, and --grep")
	}
	names := options.names
	if len(names) == 0 || len(names) == 1 && names[0] == "all" {
		names, err = b.names()
		if err != nil {
			return err
		}
	}
	lineCount, _ := strconv.Atoi(options.lines)
	for _, name := range names {
		if !b.exists(name) {
			return fmt.Errorf("service %q is not managed by spm", name)
		}
		if err := b.printLogTail(name, lineCount, options.grep); err != nil {
			return err
		}
	}
	if !options.follow {
		return nil
	}
	offsets := map[string]int64{}
	identities := map[string]os.FileInfo{}
	for _, name := range names {
		if info, err := os.Stat(b.logPath(name)); err == nil {
			offsets[name] = info.Size()
			identities[name] = info
		}
	}
	for {
		time.Sleep(500 * time.Millisecond)
		for _, name := range names {
			path := b.logPath(name)
			info, offset, readErr := readRotatingLogUpdates(b.stdout, path, identities[name], offsets[name], options.grep)
			if readErr == nil {
				identities[name], offsets[name] = info, offset
			}
		}
	}
}

func (b *windowsBackend) printLogTail(name string, count int, grep string) error {
	return printRotatedLogTail(b.stdout, b.logPath(name), count, grep)
}

func (b *windowsBackend) selectNames(selectors []string, deleteAll bool) ([]string, error) {
	if len(selectors) == 0 {
		return nil, errors.New("at least one service name or all is required")
	}
	if len(selectors) == 1 && selectors[0] == "all" {
		return b.names()
	}
	var names []string
	for _, name := range selectors {
		if name == "all" {
			return nil, errors.New(`"all" cannot be combined with service names`)
		}
		if err := validateName(name); err != nil {
			return nil, err
		}
		if !b.exists(name) {
			return nil, fmt.Errorf("service %q is not managed by spm", name)
		}
		names = append(names, name)
	}
	_ = deleteAll
	return names, nil
}

func (b *windowsBackend) removeMany(args []string) error {
	yes := false
	var selectors []string
	for _, arg := range args {
		if arg == "--yes" || arg == "-y" {
			yes = true
		} else {
			selectors = append(selectors, arg)
		}
	}
	if len(selectors) == 1 && selectors[0] == "all" && !yes {
		return errors.New("deleting all managed services requires --yes")
	}
	names, err := b.selectNames(selectors, true)
	if err != nil {
		return err
	}
	var errs []error
	scheduleChanged := false
	for _, name := range names {
		config, _ := b.readConfig(name)
		if config.Mode == "schedule" {
			scheduleChanged = true
		}
		if err := b.stop(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if config.Mode == "schedule" || b.system || config.Enabled {
			_ = b.unregister(name)
		}
		if err := removeWindowsServiceData(b.serviceDir(name)); err != nil {
			errs = append(errs, fmt.Errorf("remove service data for %s: %w", name, err))
			continue
		}
		fmt.Fprintf(b.stdout, "%s deleted\n", name)
	}
	if scheduleChanged {
		if err := b.refreshScheduleDaemon(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeWindowsServiceData(path string) error {
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		err = os.RemoveAll(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func (b *windowsBackend) doctor() error {
	failed := false
	check := func(label, status, detail string) {
		fmt.Fprintf(b.stdout, "%-4s  %-20s %s\n", status, label, detail)
		failed = failed || status == "FAIL"
	}
	if b.system {
		if err := b.checkSCM(); err != nil {
			check("Service manager", "FAIL", err.Error())
		} else {
			check("Service manager", "OK", "SCM available; services are visible in services.msc")
		}
	} else {
		if startup, err := windowsStartupDirectory(); err != nil {
			check("Startup directory", "FAIL", err.Error())
		} else {
			check("Startup directory", "OK", startup)
		}
	}
	if b.system {
		check("service account", "OK", "configurable per service; administrator access is required to make changes")
	} else {
		check("service account", "OK", valueOr(os.Getenv("USERNAME"), "current user"))
	}
	if err := b.ensureDirectories(); err != nil {
		check("data directory", "FAIL", err.Error())
	} else {
		check("data directory", "OK", b.root)
	}
	if executable, err := os.Executable(); err != nil {
		check("spm executable", "FAIL", err.Error())
	} else {
		check("spm executable", "OK", executable)
	}
	if failed {
		return errors.New("windows doctor found failures")
	}
	return nil
}

func windowsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")
	closeHandle := kernel32.NewProc("CloseHandle")
	handle, _, _ := openProcess.Call(0x1000, 0, uintptr(pid))
	if handle == 0 {
		return false
	}
	_, _, _ = closeHandle.Call(handle)
	return true
}

type windowsJobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type windowsIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type windowsJobExtendedLimitInformation struct {
	BasicLimitInformation windowsJobBasicLimitInformation
	IoInfo                windowsIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func windowsCreateKillJob(pid int) (syscall.Handle, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createJob := kernel32.NewProc("CreateJobObjectW")
	setJob := kernel32.NewProc("SetInformationJobObject")
	openProcess := kernel32.NewProc("OpenProcess")
	assignProcess := kernel32.NewProc("AssignProcessToJobObject")
	handle, _, createErr := createJob.Call(0, 0)
	if handle == 0 {
		return 0, createErr
	}
	job := syscall.Handle(handle)
	info := windowsJobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	result, _, setErr := setJob.Call(handle, 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if result == 0 {
		windowsCloseHandle(job)
		return 0, setErr
	}
	process, _, openErr := openProcess.Call(0x0100|0x0001, 0, uintptr(pid)) // SET_QUOTA | TERMINATE
	if process == 0 {
		windowsCloseHandle(job)
		return 0, openErr
	}
	defer windowsCloseHandle(syscall.Handle(process))
	result, _, assignErr := assignProcess.Call(handle, process)
	if result == 0 {
		windowsCloseHandle(job)
		return 0, assignErr
	}
	return job, nil
}

func windowsCloseHandle(handle syscall.Handle) {
	if handle == 0 {
		return
	}
	closeHandle := syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle")
	_, _, _ = closeHandle.Call(uintptr(handle))
}

func windowsKillTree(pid int) {
	if pid <= 0 {
		return
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")
	terminateProcess := kernel32.NewProc("TerminateProcess")
	process, _, _ := openProcess.Call(0x0001, 0, uintptr(pid)) // PROCESS_TERMINATE
	if process == 0 {
		return
	}
	defer windowsCloseHandle(syscall.Handle(process))
	_, _, _ = terminateProcess.Call(process, 1)
}
