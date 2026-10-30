package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type probeConfig struct {
	Kind       string   `json:"kind,omitempty"`
	Target     string   `json:"target,omitempty"`
	Timeout    string   `json:"timeout,omitempty"`
	Retries    int      `json:"retries,omitempty"`
	RetryDelay string   `json:"retry_delay,omitempty"`
	RuntimeCwd string   `json:"-"`
	RuntimeEnv []string `json:"-"`
}

type healthConfig struct {
	Probe       probeConfig `json:"probe,omitempty"`
	Interval    string      `json:"interval,omitempty"`
	StartPeriod string      `json:"start_period,omitempty"`
	Action      string      `json:"action,omitempty"`
}

func (p *probeConfig) setTarget(kind, target string) error {
	if p.Kind != "" && (p.Kind != kind || p.Target != target) {
		return errors.New("only one HTTP, TCP, or command probe may be configured")
	}
	if target == "" || strings.ContainsAny(target, "\r\n") {
		return errors.New("probe target cannot be empty or contain a newline")
	}
	p.Kind, p.Target = kind, target
	return nil
}

func (p probeConfig) validate() error {
	if p.Kind == "" {
		return nil
	}
	if p.Kind != "http" && p.Kind != "tcp" && p.Kind != "command" {
		return fmt.Errorf("invalid probe type %q", p.Kind)
	}
	if p.Timeout != "" {
		if duration, err := time.ParseDuration(p.Timeout); err != nil || duration <= 0 {
			return fmt.Errorf("invalid probe timeout %q", p.Timeout)
		}
	}
	if p.Retries < 0 {
		return errors.New("probe retries cannot be negative")
	}
	if p.RetryDelay != "" {
		if duration, err := time.ParseDuration(p.RetryDelay); err != nil || duration < 0 {
			return fmt.Errorf("invalid probe retry delay %q", p.RetryDelay)
		}
	}
	return nil
}

func (h healthConfig) validate() error {
	if h.Probe.Kind == "" {
		if h.Interval != "" || h.StartPeriod != "" || h.Action != "" {
			return errors.New("health options require --health-http, --health-tcp, or --health-command")
		}
		return nil
	}
	if err := h.Probe.validate(); err != nil {
		return err
	}
	for label, value := range map[string]string{"interval": h.Interval, "start period": h.StartPeriod} {
		if value != "" {
			if duration, err := time.ParseDuration(value); err != nil || duration <= 0 {
				return fmt.Errorf("invalid health %s %q", label, value)
			}
		}
	}
	if h.Action != "" && h.Action != "none" && h.Action != "restart" && h.Action != "stop" {
		return fmt.Errorf("invalid health action %q", h.Action)
	}
	return nil
}

func (p probeConfig) normalized() probeConfig {
	if p.Timeout == "" {
		p.Timeout = "5s"
	}
	if p.Retries == 0 {
		p.Retries = 3
	}
	if p.RetryDelay == "" {
		p.RetryDelay = "1s"
	}
	return p
}

func runProbe(probe probeConfig) error {
	probe = probe.normalized()
	timeout, _ := time.ParseDuration(probe.Timeout)
	retryDelay, _ := time.ParseDuration(probe.RetryDelay)
	var lastErr error
	for attempt := 1; attempt <= probe.Retries; attempt++ {
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		lastErr = runProbeOnce(ctx, probe)
		cancel()
		if lastErr == nil {
			fmt.Fprintf(os.Stdout, "health check succeeded type=%s duration=%s attempt=%d\n", probe.Kind, time.Since(started).Round(time.Millisecond), attempt)
			return nil
		}
		fmt.Fprintf(os.Stderr, "health check failed type=%s error=%q attempt=%d/%d\n", probe.Kind, lastErr, attempt, probe.Retries)
		if attempt < probe.Retries {
			time.Sleep(retryDelay)
		}
	}
	return lastErr
}

func runProbeOnce(ctx context.Context, probe probeConfig) error {
	switch probe.Kind {
	case "http":
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.Target, nil)
		if err != nil {
			return err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return fmt.Errorf("HTTP status %d", response.StatusCode)
		}
		return nil
	case "tcp":
		dialer := net.Dialer{}
		connection, err := dialer.DialContext(ctx, "tcp", probe.Target)
		if err != nil {
			return err
		}
		return connection.Close()
	case "command":
		command := exec.CommandContext(ctx, "/bin/sh", "-c", probe.Target)
		command.Dir = probe.RuntimeCwd
		if len(probe.RuntimeEnv) > 0 {
			command.Env = append(os.Environ(), probe.RuntimeEnv...)
		}
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		return command.Run()
	default:
		return fmt.Errorf("unsupported probe type %q", probe.Kind)
	}
}

func (a *application) healthProbeCLI(args []string) error {
	if len(args) < 2 {
		return errors.New("health-probe requires TYPE TARGET")
	}
	probe := probeConfig{Kind: args[0], Target: args[1]}
	for i := 2; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return errors.New("invalid health-probe arguments")
		}
		switch args[i] {
		case "--timeout":
			probe.Timeout = args[i+1]
		case "--retries":
			probe.Retries, _ = strconv.Atoi(args[i+1])
		case "--retry-delay":
			probe.RetryDelay = args[i+1]
		default:
			return fmt.Errorf("unknown health-probe option %q", args[i])
		}
	}
	if err := probe.validate(); err != nil {
		return err
	}
	return runProbe(probe)
}

func healthServiceName(name string) string  { return "spm-" + name + ".health.service" }
func healthTimerName(name string) string    { return "spm-" + name + ".health.timer" }
func healthRecoveryName(name string) string { return "spm-" + name + ".health-recover.service" }

func (a *application) healthPath(name, suffix string) string {
	return filepath.Join(a.unitDir, "spm-"+name+suffix)
}

func (a *application) hasHealth(name string) bool {
	info, err := os.Stat(a.healthPath(name, ".health.timer"))
	return err == nil && info.Mode().IsRegular()
}

func (a *application) healthPaths(name string) []string {
	return []string{a.healthPath(name, ".health.service"), a.healthPath(name, ".health.timer"), a.healthPath(name, ".health-recover.service")}
}

func (a *application) removeHealthFiles(name string) error {
	for _, path := range a.healthPaths(name) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func renderHealthUnits(options startOptions, system bool) (string, string, string, error) {
	if options.health.Probe.Kind == "" {
		return "", "", "", nil
	}
	spm, err := os.Executable()
	if err != nil {
		return "", "", "", err
	}
	systemctl, err := resolveExecutable("systemctl")
	if err != nil {
		return "", "", "", err
	}
	probe := options.health.Probe.normalized()
	interval := options.health.Interval
	if interval == "" {
		interval = "30s"
	}
	startPeriod := options.health.StartPeriod
	if startPeriod == "" {
		startPeriod = interval
	}
	action := options.health.Action
	if action == "" {
		action = "none"
	}
	managerArg := ""
	if !system {
		managerArg = " \"--user\""
	}
	probeArgs := " \"health-probe\" " + quoteSystemdExec(probe.Kind) + " " + quoteSystemdExec(probe.Target) + " \"--timeout\" " + quoteSystemdExec(probe.Timeout) + " \"--retries\" \"" + strconv.Itoa(probe.Retries) + "\" \"--retry-delay\" " + quoteSystemdExec(probe.RetryDelay)
	onFailure := ""
	if action != "none" {
		onFailure = "OnFailure=" + healthRecoveryName(options.name) + "\n"
	}
	service := "[Unit]\nDescription=Health check for spm service: " + options.name + "\nAfter=" + unitName(options.name) + "\n" + onFailure + "\n[Service]\nType=oneshot\nWorkingDirectory=" + escapeSystemdPath(options.cwd) + "\n"
	if system && options.runAsUser != "" {
		service += "User=" + options.runAsUser + "\n"
	}
	for _, value := range options.env {
		service += "Environment=" + quoteSystemd(value) + "\n"
	}
	if options.envFile != "" {
		service += "EnvironmentFile=" + escapeSystemdPath(options.envFile) + "\n"
	}
	service += "ExecCondition=" + quoteSystemdExec(systemctl) + managerArg + " \"is-active\" \"--quiet\" " + quoteSystemdExec(unitName(options.name)) + "\nExecStart=" + quoteSystemdExec(spm) + probeArgs + "\n"
	timer := "[Unit]\nDescription=Health timer for spm service: " + options.name + "\nStartLimitIntervalSec=0\nPartOf=" + unitName(options.name) + "\nAfter=" + unitName(options.name) + "\n\n[Timer]\nOnActiveSec=" + startPeriod + "\nOnUnitInactiveSec=" + interval + "\nUnit=" + healthServiceName(options.name) + "\n\n[Install]\nWantedBy=timers.target\n"
	recovery := ""
	if action != "none" {
		recovery = "[Unit]\nDescription=Health recovery for spm service: " + options.name + "\n\n[Service]\nType=oneshot\nExecStart=" + quoteSystemdExec(systemctl) + managerArg + " \"" + action + "\" " + quoteSystemdExec(unitName(options.name)) + "\n"
	}
	return service, timer, recovery, nil
}

func (a *application) healthStatus(name string) error {
	if !a.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	if !a.hasHealth(name) {
		fmt.Fprintf(a.stdout, "%s health check is disabled\n", name)
		return nil
	}
	units := []string{"status", healthTimerName(name), healthServiceName(name)}
	if _, err := os.Stat(a.healthPath(name, ".health-recover.service")); err == nil {
		units = append(units, healthRecoveryName(name))
	}
	units = append(units, "--no-pager", "--full")
	err := a.interactiveSystemctl(units...)
	var exitError interface{ ExitCode() int }
	if err == nil || errors.As(err, &exitError) && exitError.ExitCode() == 3 {
		return nil
	}
	return err
}

func (a *application) readHealthStates(names []string) map[string]string {
	states := make(map[string]string, len(names))
	var units []string
	for _, name := range names {
		if !a.hasHealth(name) {
			states[name] = "disabled"
			continue
		}
		states[name] = "unknown"
		units = append(units, healthServiceName(name))
	}
	if len(units) == 0 {
		return states
	}
	args := append([]string{"show"}, units...)
	args = append(args, "--property=Id", "--property=ActiveState", "--property=Result", "--property=ExecMainStatus")
	output, err := a.runSystemctl(args...)
	if err != nil {
		return states
	}
	for _, block := range strings.Split(strings.TrimSpace(string(output)), "\n\n") {
		values := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				values[key] = value
			}
		}
		id := values["Id"]
		if !strings.HasPrefix(id, "spm-") || !strings.HasSuffix(id, ".health.service") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(id, "spm-"), ".health.service")
		switch {
		case values["ActiveState"] == "activating" || values["ActiveState"] == "active":
			states[name] = "checking"
		case values["Result"] == "success" && (values["ExecMainStatus"] == "0" || values["ExecMainStatus"] == ""):
			states[name] = "healthy"
		case values["Result"] != "" && values["Result"] != "success", values["ExecMainStatus"] != "" && values["ExecMainStatus"] != "0":
			states[name] = "unhealthy"
		}
	}
	return states
}
