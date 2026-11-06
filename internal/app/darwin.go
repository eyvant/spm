//go:build darwin

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
	"text/tabwriter"
	"time"
)

const darwinConfigVersion = 1

type darwinServiceConfig struct {
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
	Group            string   `json:"group,omitempty"`
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

type darwinBackend struct {
	stdout   io.Writer
	stderr   io.Writer
	root     string
	plistDir string
	system   bool
	uid      int
}

func runDarwin(args []string, stdout, stderr io.Writer, system bool) error {
	if len(args) > 0 && args[0] == "__darwin-schedule-daemon" {
		if len(args) != 5 || args[1] != "--root" || !filepath.IsAbs(args[2]) || args[3] != "--domain" {
			return errors.New("invalid macOS scheduler invocation")
		}
		domain := args[4]
		root := filepath.Clean(args[2])
		log, closeLog := openPortableSchedulerLog(root, stderr)
		defer closeLog()
		return runPortableScheduler(context.Background(), root, func(name string) error {
			command := exec.Command("launchctl", "kickstart", domain+"/com.eyvant.spm."+name)
			if output, err := command.CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
			}
			return nil
		}, log)
	}
	if system && os.Geteuid() != 0 {
		return errors.New("system mode requires root; run: sudo spm --system <command>")
	}
	b, err := newDarwinBackend(stdout, stderr, system)
	if err != nil {
		return err
	}
	return b.run(args)
}

func newDarwinBackend(stdout, stderr io.Writer, system bool) (*darwinBackend, error) {
	uid := os.Getuid()
	if system {
		return &darwinBackend{stdout: stdout, stderr: stderr, root: "/Library/Application Support/spm", plistDir: "/Library/LaunchDaemons", system: true, uid: uid}, nil
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve macOS Application Support directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve macOS home directory: %w", err)
	}
	return &darwinBackend{stdout: stdout, stderr: stderr, root: filepath.Join(config, "spm"), plistDir: filepath.Join(home, "Library", "LaunchAgents"), uid: uid}, nil
}

func platformCompletionApplication(stdout io.Writer, system bool) (*application, bool) {
	b, err := newDarwinBackend(io.Discard, io.Discard, system)
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
  spm start [options] <script|-- command>
  spm start <name>
  spm schedule [options] <script|-- command>
  spm list [--json] [--watch[=DURATION]]
  spm status|show|describe <name>
  spm logs [--lines N] [--follow] [--grep PATTERN] [name...|all]
  spm restart|stop|enable|disable <name...|all>
  spm delete <name...|all> [--yes]
  spm apply|validate|diff spm.toml
  spm native launchd print <name>
  spm doctor

Global options:
  --system                    Install a LaunchDaemon; requires root.

User services are LaunchAgents in ~/Library/LaunchAgents. System services are
LaunchDaemons in /Library/LaunchDaemons. Both use the same spm lifecycle UI.
`)
	return true
}

func printPlatformCommandHelp(w io.Writer, command string) bool {
	switch command {
	case "start":
		fmt.Fprintln(w, "Usage:\n  spm start [options] <script> [arguments]\n  spm start [options] -- <command> [arguments]\n  spm start <name>")
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
	case "doctor", "version":
		fmt.Fprintf(w, "Usage:\n  spm %s\n", command)
	case "apply", "validate", "diff":
		fmt.Fprintf(w, "Usage:\n  spm %s spm.toml\n", command)
	default:
		return false
	}
	return true
}

func (b *darwinBackend) run(args []string) error {
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
		data, err := os.ReadFile(b.servicePlistPath(name))
		if err == nil {
			_, err = b.stdout.Write(data)
		}
		return err
	case "apply":
		return b.apply(args[1:])
	case "validate", "diff":
		return b.inspectDeclaration(args[0], args[1:])
	case "native":
		return b.native(args[1:])
	case "doctor":
		if len(args) != 1 {
			return errors.New("doctor does not accept arguments")
		}
		return b.doctor()
	case "startup":
		fmt.Fprintln(b.stdout, "Enabled macOS services already start through launchd; no additional startup setup is required.")
		return nil
	case "signal", "kill", "scale", "reload", "set", "unset", "clone", "history", "rollback", "security", "export", "health", "import", "edit":
		return fmt.Errorf("%s is not supported by the macOS backend yet", args[0])
	default:
		return fmt.Errorf("unknown command %q; run spm help", args[0])
	}
}

func (b *darwinBackend) servicesDir() string { return filepath.Join(b.root, "services") }
func (b *darwinBackend) serviceDir(name string) string {
	return filepath.Join(b.servicesDir(), name)
}
func (b *darwinBackend) configPath(name string) string {
	return filepath.Join(b.serviceDir(name), "config.json")
}
func (b *darwinBackend) logPath(name string) string {
	return filepath.Join(b.serviceDir(name), "service.log")
}
func (b *darwinBackend) label(name string) string { return "com.eyvant.spm." + name }
func (b *darwinBackend) plistPath(name string) string {
	return filepath.Join(b.plistDir, b.label(name)+".plist")
}
func (b *darwinBackend) servicePlistPath(name string) string {
	return filepath.Join(b.serviceDir(name), "service.plist")
}
func (b *darwinBackend) domain() string {
	if b.system {
		return "system"
	}
	return "gui/" + strconv.Itoa(b.uid)
}
func (b *darwinBackend) target(name string) string { return b.domain() + "/" + b.label(name) }

func (b *darwinBackend) ensureDirectories(name string) error {
	for _, directory := range []string{b.root, b.servicesDir(), b.serviceDir(name), b.plistDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
	}
	return nil
}

func (b *darwinBackend) exists(name string) bool {
	info, err := os.Stat(b.configPath(name))
	return err == nil && info.Mode().IsRegular()
}

func (b *darwinBackend) names() ([]string, error) {
	entries, err := os.ReadDir(b.servicesDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && validateName(entry.Name()) == nil && b.exists(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (b *darwinBackend) readConfig(name string) (darwinServiceConfig, error) {
	var config darwinServiceConfig
	data, err := os.ReadFile(b.configPath(name))
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, err
	}
	if config.Version != darwinConfigVersion || config.Name != name || len(config.Command) == 0 {
		return config, fmt.Errorf("invalid macOS service configuration for %q", name)
	}
	return config, nil
}

func (b *darwinBackend) writeConfig(config darwinServiceConfig) error {
	if err := b.ensureDirectories(config.Name); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	return writeFileAtomic(b.configPath(config.Name), append(data, '\n'), 0o600)
}

func (b *darwinBackend) start(args []string) error {
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
	if !b.system && (options.runAsUser != "" || options.runAsGroup != "") {
		return errors.New("--user and --group require global --system mode on macOS")
	}
	runAs, group := options.runAsUser, options.runAsGroup
	options.runAsUser, options.runAsGroup = "", ""
	if err := rejectDarwinStartOptions(options); err != nil {
		return err
	}
	if err := options.prepare(); err != nil {
		return err
	}
	if b.exists(options.name) {
		_ = b.bootout(options.name)
		_ = os.Remove(b.bootPlistPath(options.name))
		_ = os.Remove(b.serviceBootPlistPath(options.name))
	}
	config := darwinServiceConfig{Version: darwinConfigVersion, Name: options.name, Description: options.description, Cwd: options.cwd, Command: append([]string(nil), options.command...), Env: append([]string(nil), options.env...), EnvFile: options.envFile, Restart: options.restart, RestartSec: options.restartSec, Enabled: options.enable, RunAs: runAs, Group: group, After: append([]string(nil), options.after...), Wants: append([]string(nil), options.wants...), Requires: append([]string(nil), options.requires...)}
	if err := b.writeConfig(config); err != nil {
		return err
	}
	if err := b.writePlist(config); err != nil {
		return err
	}
	if err := b.startManaged(config.Name); err != nil {
		return err
	}
	if options.wait {
		if err := b.waitRunning(config.Name, options.waitTimeout); err != nil {
			return err
		}
		if options.waitProbe.Kind != "" {
			options.waitProbe.RuntimeCwd, options.waitProbe.RuntimeEnv = options.cwd, append([]string(nil), options.env...)
			if err := waitForProbe(options.waitProbe, options.waitTimeout); err != nil {
				return fmt.Errorf("service %s did not become ready: %w", options.name, err)
			}
		}
	}
	fmt.Fprintf(b.stdout, "%s started as %s\n", config.Name, b.label(config.Name))
	return nil
}

func rejectDarwinStartOptions(o startOptions) error {
	var unsupported []string
	checks := []struct {
		set  bool
		name string
	}{
		{o.startLimitInterval != "" || o.startLimitBurst != "", "start limits"}, {o.cronRestart != "", "--cron-restart"},
		{o.maxMemory != "" || len(o.properties) > 0, "resource/property options"},
		{len(o.execStartPre)+len(o.execStartPost) > 0 || o.execStop != "" || o.execReload != "", "lifecycle hooks"},
		{len(o.onFailure) > 0, "--on-failure handlers"},
		{o.health.Probe.Kind != "", "health checks"}, {o.serviceType != "", "--type"}, {o.pidFile != "", "--pid-file"},
		{o.timezone != "", "--timezone"}, {o.instances != 0, "--instances"}, {o.sandbox != "", "--sandbox"},
	}
	for _, check := range checks {
		if check.set {
			unsupported = append(unsupported, check.name)
		}
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("macOS backend does not support %s", strings.Join(unsupported, ", "))
	}
	return nil
}

func (b *darwinBackend) writePlist(config darwinServiceConfig) error {
	if config.Mode == "schedule" {
		return b.writeSchedulePlists(config)
	}
	if err := b.ensureDirectories(config.Name); err != nil {
		return err
	}
	env := darwinEnvironment(config)
	var builder strings.Builder
	builder.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n")
	darwinPlistKeyString(&builder, "Label", b.label(config.Name))
	builder.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, value := range config.Command {
		fmt.Fprintf(&builder, "    <string>%s</string>\n", darwinXMLEscape(value))
	}
	builder.WriteString("  </array>\n")
	darwinPlistKeyString(&builder, "WorkingDirectory", config.Cwd)
	darwinPlistKeyString(&builder, "StandardOutPath", b.logPath(config.Name))
	darwinPlistKeyString(&builder, "StandardErrorPath", b.logPath(config.Name))
	if config.Description != "" {
		darwinPlistKeyString(&builder, "ServiceDescription", config.Description)
	}
	if b.system && config.RunAs != "" {
		darwinPlistKeyString(&builder, "UserName", config.RunAs)
	}
	if b.system && config.Group != "" {
		darwinPlistKeyString(&builder, "GroupName", config.Group)
	}
	builder.WriteString("  <key>RunAtLoad</key>\n")
	fmt.Fprintf(&builder, "  <%t/>\n", config.Enabled)
	switch config.Restart {
	case "always":
		builder.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	case "on-failure", "on-abnormal", "on-abort", "on-watchdog":
		builder.WriteString("  <key>KeepAlive</key>\n  <dict>\n    <key>SuccessfulExit</key>\n    <false/>\n  </dict>\n")
	case "on-success":
		builder.WriteString("  <key>KeepAlive</key>\n  <dict>\n    <key>SuccessfulExit</key>\n    <true/>\n  </dict>\n")
	}
	if delay, err := time.ParseDuration(config.RestartSec); err == nil && delay >= time.Second {
		builder.WriteString("  <key>ThrottleInterval</key>\n")
		fmt.Fprintf(&builder, "  <integer>%d</integer>\n", int(delay/time.Second))
	}
	if len(env) > 0 {
		builder.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			darwinPlistKeyString(&builder, key, env[key])
		}
		builder.WriteString("  </dict>\n")
	}
	builder.WriteString("</dict>\n</plist>\n")
	data := []byte(builder.String())
	if err := writeFileAtomic(b.servicePlistPath(config.Name), data, 0o644); err != nil {
		return err
	}
	if config.Enabled {
		return writeFileAtomic(b.plistPath(config.Name), data, 0o644)
	}
	if err := os.Remove(b.plistPath(config.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func darwinPlistKeyString(builder *strings.Builder, key, value string) {
	fmt.Fprintf(builder, "  <key>%s</key>\n  <string>%s</string>\n", darwinXMLEscape(key), darwinXMLEscape(value))
}

func darwinXMLEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func darwinEnvironment(config darwinServiceConfig) map[string]string {
	result := map[string]string{}
	if config.EnvFile != "" {
		if file, err := os.Open(config.EnvFile); err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if key, value, ok := strings.Cut(line, "="); ok {
					result[key] = strings.Trim(value, "\"'")
				}
			}
			_ = file.Close()
		}
	}
	for _, item := range config.Env {
		if key, value, ok := strings.Cut(item, "="); ok {
			result[key] = value
		}
	}
	return result
}

func (b *darwinBackend) launchctl(args ...string) ([]byte, error) {
	cmd := exec.Command("launchctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return output, nil
}

func (b *darwinBackend) loaded(name string) bool {
	_, err := b.launchctl("print", b.target(name))
	return err == nil
}

func (b *darwinBackend) startManaged(name string) error {
	return b.startManagedStack(name, map[string]bool{})
}

func (b *darwinBackend) startManagedStack(name string, stack map[string]bool) error {
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
	}
	for _, dependency := range config.Wants {
		if b.exists(dependency) {
			if err := b.startManagedStack(dependency, stack); err != nil {
				fmt.Fprintf(b.stderr, "warning: wanted service %s did not start: %v\n", dependency, err)
			}
		}
	}
	for _, dependency := range config.After {
		if b.loaded(dependency) {
			if err := b.waitRunning(dependency, 30*time.Second); err != nil {
				return fmt.Errorf("wait for service %q: %w", dependency, err)
			}
		}
	}
	if !b.loaded(name) {
		plist := b.servicePlistPath(name)
		if config.Enabled {
			plist = b.plistPath(name)
		}
		if _, err := b.launchctl("bootstrap", b.domain(), plist); err != nil {
			return fmt.Errorf("bootstrap %s: %w", name, err)
		}
	}
	if _, err := b.launchctl("kickstart", b.target(name)); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return b.waitRunning(name, 5*time.Second)
}

func (b *darwinBackend) bootout(name string) error {
	config, _ := b.readConfig(name)
	labels := []string{b.target(name)}
	if config.Mode == "schedule" && config.TimerOnBoot != "" {
		labels = append(labels, b.domain()+"/"+b.bootLabel(name))
	}
	for _, label := range labels {
		if _, err := b.launchctl("print", label); err != nil {
			continue
		}
		if _, err := b.launchctl("bootout", label); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	return nil
}

func (b *darwinBackend) waitRunning(name string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state := b.readState(name)
		if state.active == "active" && state.pid != "" {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s to run", name)
}

func (b *darwinBackend) readState(name string) serviceState {
	output, err := b.launchctl("print", b.target(name))
	if err != nil {
		return serviceState{active: "inactive", sub: "unloaded"}
	}
	state := serviceState{active: "active", sub: "loaded"}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			state.sub = value
			if value != "running" {
				state.active = "inactive"
			}
		case "pid":
			state.pid = value
		case "runs":
			state.restarts = value
		case "last exit code":
			state.execStatus = value
		}
	}
	return state
}

func (b *darwinBackend) controlMany(action string, selectors []string) error {
	names, err := b.selectNames(selectors)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		config, _ := b.readConfig(name)
		if config.Mode == "schedule" {
			switch action {
			case "stop":
				err = b.bootout(name)
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
			err = b.bootout(name)
		case "restart":
			if b.loaded(name) {
				_, err = b.launchctl("kickstart", "-k", b.target(name))
			} else {
				err = b.startManaged(name)
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

func (b *darwinBackend) setEnabled(name string, enabled bool) error {
	config, err := b.readConfig(name)
	if err != nil {
		return err
	}
	if config.Mode == "schedule" {
		return b.setScheduledEnabled(config, enabled)
	}
	_ = b.bootout(name)
	config.Enabled = enabled
	if err := b.writeConfig(config); err != nil {
		return err
	}
	if err := b.writePlist(config); err != nil {
		return err
	}
	if enabled {
		return b.startManaged(name)
	}
	return nil
}

func (b *darwinBackend) list(options listOptions) error {
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
		config, err := b.readConfig(name)
		if err != nil {
			continue
		}
		if config.Mode == "schedule" {
			status := "disabled"
			active := "inactive"
			if config.Enabled {
				status, active = "waiting", "active"
			}
			unit := b.label(name)
			if config.Schedule == "" && config.TimerInterval == "" {
				unit = b.bootLabel(name)
			}
			rows = append(rows, listRow{Name: name, Unit: unit, Status: status, ActiveState: active, SubState: status, Health: "-", Enabled: map[bool]string{true: "enabled", false: "disabled"}[config.Enabled], Schedule: portableScheduleSummary(config.Schedule, config.Timezone, config.TimerOnBoot, config.TimerInterval, config.TimerRandomDelay)})
			continue
		}
		state := b.readState(name)
		pid, _ := strconv.Atoi(state.pid)
		restarts, _ := strconv.Atoi(state.restarts)
		rows = append(rows, listRow{Name: name, Unit: b.label(name), Status: state.sub, ActiveState: state.active, SubState: state.sub, Health: "-", PID: pid, Restarts: restarts, Enabled: map[bool]string{true: "enabled", false: "disabled"}[config.Enabled]})
	}
	return (&application{stdout: b.stdout}).printListRows(rows, options.json)
}

func (b *darwinBackend) status(name string) error {
	config, err := b.readConfig(name)
	if err != nil {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	if config.Mode == "schedule" {
		state := readPortableScheduleState(filepath.Join(b.scheduleStateDir(name), "schedule-state.json"))
		status := "disabled"
		if config.Enabled {
			status = "waiting"
		}
		w := tabwriter.NewWriter(b.stdout, 0, 4, 2, ' ', 0)
		for _, row := range [][2]string{{"status", status}, {"name", name}, {"scope", map[bool]string{true: "system", false: "user"}[b.system]}, {"enabled", strconv.FormatBool(config.Enabled)}, {"schedule", valueOr(config.Schedule, "-")}, {"interval", valueOr(config.TimerInterval, "-")}, {"on boot", valueOr(config.TimerOnBoot, "-")}, {"timezone", valueOr(config.Timezone, "local")}, {"last scheduled", formatScheduleTime(state.LastScheduledAt)}, {"last finished", formatScheduleTime(state.LastFinishedAt)}, {"last exit code", scheduleExitCode(state.LastExitCode)}, {"command", strings.Join(config.Command, " ")}, {"cwd", config.Cwd}, {"log", b.logPath(name)}} {
			fmt.Fprintf(w, "%s\t%s\n", row[0], row[1])
		}
		return w.Flush()
	}
	state := b.readState(name)
	scope := "user"
	if b.system {
		scope = "system"
	}
	w := tabwriter.NewWriter(b.stdout, 0, 4, 2, ' ', 0)
	plist := b.servicePlistPath(name)
	if config.Enabled {
		plist = b.plistPath(name)
	}
	for _, row := range [][2]string{{"status", state.sub}, {"name", name}, {"scope", scope}, {"backend", "launchd"}, {"label", b.label(name)}, {"enabled", strconv.FormatBool(config.Enabled)}, {"pid", valueOr(state.pid, "-")}, {"runs", valueOr(state.restarts, "0")}, {"command", strings.Join(config.Command, " ")}, {"cwd", config.Cwd}, {"plist", plist}, {"log", b.logPath(name)}} {
		fmt.Fprintf(w, "%s\t%s\n", row[0], row[1])
	}
	return w.Flush()
}

func (b *darwinBackend) logs(args []string) error {
	options, err := parseLogs(args)
	if err != nil {
		return err
	}
	if options.since != "" || options.until != "" || options.output != "" || options.boot || options.health {
		return errors.New("macOS logs currently support service names, --lines, --follow, and --grep")
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
		if err := printFileTail(b.stdout, b.logPath(name), lineCount, options.grep); err != nil {
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

func printFileTail(w io.Writer, path string, count int, grep string) error {
	return printRotatedLogTail(w, path, count, grep)
}

func (b *darwinBackend) selectNames(selectors []string) ([]string, error) {
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
	return names, nil
}

func (b *darwinBackend) removeMany(args []string) error {
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
	names, err := b.selectNames(selectors)
	if err != nil {
		return err
	}
	var errs []error
	scheduleChanged := false
	for _, name := range names {
		if config, configErr := b.readConfig(name); configErr == nil && config.Mode == "schedule" {
			scheduleChanged = true
		}
		_ = b.bootout(name)
		if err := os.Remove(b.plistPath(name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.Remove(b.bootPlistPath(name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(b.serviceDir(name)); err != nil {
			errs = append(errs, err)
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

func (b *darwinBackend) native(args []string) error {
	if len(args) == 3 && args[0] == "launchd" && args[1] == "print" {
		name, err := oneName(args[2:])
		if err != nil {
			return err
		}
		cmd := exec.Command("launchctl", "print", b.target(name))
		cmd.Stdout, cmd.Stderr = b.stdout, b.stderr
		return cmd.Run()
	}
	return errors.New("usage: spm native launchd print <name>")
}

func (b *darwinBackend) apply(args []string) error {
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

func (b *darwinBackend) inspectDeclaration(action string, args []string) error {
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
		fmt.Fprintf(b.stdout, "%s is valid for macOS (%d services)\n", args[0], len(services))
		return nil
	}
	return b.printDeclarationPlan(services, true)
}

func (b *darwinBackend) prepareDeclaration(service serviceDeclaration) (darwinServiceConfig, error) {
	options, err := parseStart(service.startArgs(b.exists(service.Name), b.system))
	if err != nil {
		return darwinServiceConfig{}, err
	}
	if !b.system && (options.runAsUser != "" || options.runAsGroup != "") {
		return darwinServiceConfig{}, errors.New("user/group require system mode")
	}
	runAs, group := options.runAsUser, options.runAsGroup
	options.runAsUser, options.runAsGroup = "", ""
	timezone := options.timezone
	if service.hasSchedule() {
		options.timezone = ""
	}
	if err := rejectDarwinStartOptions(options); err != nil {
		return darwinServiceConfig{}, err
	}
	if err := options.prepare(); err != nil {
		return darwinServiceConfig{}, err
	}
	config := darwinServiceConfig{Version: darwinConfigVersion, Name: options.name, Description: options.description, Cwd: options.cwd, Command: append([]string(nil), options.command...), Env: append([]string(nil), options.env...), EnvFile: options.envFile, Restart: options.restart, RestartSec: options.restartSec, Enabled: options.enable, RunAs: runAs, Group: group, After: append([]string(nil), options.after...), Wants: append([]string(nil), options.wants...), Requires: append([]string(nil), options.requires...)}
	if service.hasSchedule() {
		config.Mode, config.Restart = "schedule", "no"
		config.Schedule, config.Timezone, config.TimerOnBoot = service.Schedule, timezone, service.TimerOnBoot
		config.TimerInterval, config.TimerRandomDelay, config.Persistent = service.TimerInterval, service.TimerRandomDelay, service.Persistent
		record := portableScheduleRecord{Version: 1, Name: config.Name, Command: config.Command, Cwd: config.Cwd, Schedule: config.Schedule, Timezone: config.Timezone, TimerOnBoot: config.TimerOnBoot, TimerInterval: config.TimerInterval, TimerRandomDelay: config.TimerRandomDelay, Persistent: config.Persistent, CreatedAt: time.Now()}
		if err := validatePortableSchedule(record); err != nil {
			return darwinServiceConfig{}, err
		}
	}
	return config, nil
}

func (b *darwinBackend) printDeclarationPlan(services []serviceDeclaration, detailed bool) error {
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

func (b *darwinBackend) doctor() error {
	failed := false
	check := func(label, status, detail string) {
		fmt.Fprintf(b.stdout, "%-4s  %-20s %s\n", status, label, detail)
		failed = failed || status == "FAIL"
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		check("launchd", "FAIL", "launchctl is unavailable")
	} else {
		check("launchd", "OK", b.domain())
	}
	if err := os.MkdirAll(b.servicesDir(), 0o755); err != nil {
		check("data directory", "FAIL", err.Error())
	} else {
		check("data directory", "OK", b.root)
	}
	check("plist directory", "OK", b.plistDir)
	if failed {
		return errors.New("macOS doctor found failures")
	}
	return nil
}
