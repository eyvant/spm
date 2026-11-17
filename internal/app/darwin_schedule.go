//go:build darwin

package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eyvant/spm/internal/runtime/common"
	"github.com/eyvant/spm/internal/runtime/scheduler"
	"github.com/eyvant/spm/internal/runtime/supervisor"
)

func (b *darwinBackend) scheduleRecordPath(name string) string {
	return filepath.Join(b.serviceDir(name), "schedule.json")
}

func (b *darwinBackend) scheduleStateDir(name string) string {
	return filepath.Join(b.serviceDir(name), "schedule-state")
}

func (b *darwinBackend) bootLabel(name string) string {
	return b.label(name) + ".boot"
}

func (b *darwinBackend) scheduleDaemonLabel() string { return "com.eyvant.spm.scheduler" }

func (b *darwinBackend) scheduleDaemonPlistPath() string {
	return filepath.Join(b.plistDir, b.scheduleDaemonLabel()+".plist")
}

func (b *darwinBackend) serviceScheduleDaemonPlistPath() string {
	return filepath.Join(b.root, "scheduler.plist")
}

func (b *darwinBackend) bootPlistPath(name string) string {
	return filepath.Join(b.plistDir, b.bootLabel(name)+".plist")
}

func (b *darwinBackend) serviceBootPlistPath(name string) string {
	return filepath.Join(b.serviceDir(name), "boot.plist")
}

func (b *darwinBackend) schedule(args []string) error {
	options, err := parseSchedule(args)
	if err != nil {
		return err
	}
	if options.start.name == "" {
		options.start.name = inferName(options.start.command, options.start.explicitCmd)
	}
	if b.exists(options.start.name) && !options.start.force {
		return fmt.Errorf("service %q already exists; use --force to replace it", options.start.name)
	}
	if !b.system && (options.start.runAsUser != "" || options.start.runAsGroup != "") {
		return errors.New("--user and --group require global --system mode on macOS")
	}
	runAs, group := options.start.runAsUser, options.start.runAsGroup
	options.start.runAsUser, options.start.runAsGroup = "", ""
	timezone := options.start.timezone
	options.start.timezone = ""
	if err := rejectDarwinStartOptions(options.start); err != nil {
		return err
	}
	if err := options.start.prepare(); err != nil {
		return err
	}
	name := options.start.name
	if b.exists(name) {
		_ = b.bootout(name)
	}
	config := darwinServiceConfig{
		Version: darwinConfigVersion, Name: name, Description: options.start.description, Cwd: options.start.cwd,
		Command: append([]string(nil), options.start.command...), Env: append([]string(nil), options.start.env...), EnvFile: options.start.envFile,
		Restart: "no", RestartSec: options.start.restartSec, Enabled: options.start.enable, RunAs: runAs, Group: group,
		After: append([]string(nil), options.start.after...), Wants: append([]string(nil), options.start.wants...), Requires: append([]string(nil), options.start.requires...),
		Mode: "schedule", Schedule: options.expression, Timezone: timezone, TimerOnBoot: options.onBoot,
		TimerInterval: options.interval, TimerRandomDelay: options.randomDelay, Persistent: options.persistent,
	}
	if err := b.writeConfig(config); err != nil {
		return err
	}
	record := scheduler.Record{
		Version: scheduler.RecordVersion, Name: name, Command: append([]string(nil), config.Command...), Cwd: config.Cwd,
		Env: append([]string(nil), config.Env...), EnvFile: config.EnvFile, Schedule: config.Schedule, Timezone: config.Timezone,
		TimerOnBoot: config.TimerOnBoot, TimerInterval: config.TimerInterval, TimerRandomDelay: config.TimerRandomDelay,
		Persistent: config.Persistent, CreatedAt: time.Now().UTC(),
	}
	if err := scheduler.PrepareUpdate(b.scheduleRecordPath(name), b.scheduleStateDir(name), &record); err != nil {
		return err
	}
	if err := scheduler.WriteRecord(b.scheduleRecordPath(name), record); err != nil {
		_ = os.RemoveAll(b.serviceDir(name))
		return err
	}
	if err := b.writeSchedulePlists(config); err != nil {
		return err
	}
	if b.system && runAs != "" {
		if err := b.secureServiceData(name, runAs, group); err != nil {
			return err
		}
	}
	if config.Enabled {
		if err := b.bootstrapSchedule(config); err != nil {
			return err
		}
	}
	if err := b.refreshScheduleDaemon(); err != nil {
		return err
	}
	if options.runNow {
		if err := b.runScheduledTask(name, true); err != nil {
			return err
		}
	}
	fmt.Fprintf(b.stdout, "%s scheduled with launchd\n", name)
	return nil
}

func (b *darwinBackend) secureServiceData(name, account, group string) error {
	owner, err := user.Lookup(account)
	if err != nil {
		return fmt.Errorf("look up service user %q: %w", account, err)
	}
	uid, err := strconv.Atoi(owner.Uid)
	if err != nil {
		return err
	}
	gidText := owner.Gid
	if group != "" {
		groupEntry, lookupErr := user.LookupGroup(group)
		if lookupErr != nil {
			return fmt.Errorf("look up service group %q: %w", group, lookupErr)
		}
		gidText = groupEntry.Gid
	}
	gid, err := strconv.Atoi(gidText)
	if err != nil {
		return err
	}
	return filepath.Walk(b.serviceDir(name), func(path string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Chown(path, uid, gid)
	})
}

func (b *darwinBackend) runScheduledTask(name string, force bool) error {
	executable, err := runtimeExecutable(schedulerRuntime)
	if err != nil {
		return err
	}
	args := []string{"run", "--config", b.scheduleRecordPath(name), "--state-dir", b.scheduleStateDir(name), "--log", b.logPath(name), "--events", b.eventLogPath(name), "--activation", "timer"}
	if force {
		args = append(args, "--force")
	}
	command := exec.Command(executable, args...)
	command.Stdout, command.Stderr = b.stdout, b.stderr
	return command.Run()
}

func (b *darwinBackend) writeSchedulePlists(config darwinServiceConfig) error {
	executable, err := runtimeExecutable(schedulerRuntime)
	if err != nil {
		return err
	}
	if config.Schedule != "" || config.TimerInterval != "" {
		data := renderDarwinSchedulePlist(config, b.label(config.Name), executable, b.scheduleRecordPath(config.Name), b.scheduleStateDir(config.Name), b.logPath(config.Name), b.eventLogPath(config.Name), "daemon", b.system)
		if err := writeFileAtomic(b.servicePlistPath(config.Name), []byte(data), 0o644); err != nil {
			return err
		}
		if config.Enabled {
			if err := writeFileAtomic(b.plistPath(config.Name), []byte(data), 0o644); err != nil {
				return err
			}
		} else {
			_ = os.Remove(b.plistPath(config.Name))
		}
	} else {
		_ = os.Remove(b.servicePlistPath(config.Name))
		_ = os.Remove(b.plistPath(config.Name))
	}
	if config.TimerOnBoot != "" {
		data := renderDarwinSchedulePlist(config, b.bootLabel(config.Name), executable, b.scheduleRecordPath(config.Name), b.scheduleStateDir(config.Name), b.logPath(config.Name), b.eventLogPath(config.Name), "boot", b.system)
		if err := writeFileAtomic(b.serviceBootPlistPath(config.Name), []byte(data), 0o644); err != nil {
			return err
		}
		if config.Enabled {
			if err := writeFileAtomic(b.bootPlistPath(config.Name), []byte(data), 0o644); err != nil {
				return err
			}
		} else {
			_ = os.Remove(b.bootPlistPath(config.Name))
		}
	} else {
		_ = os.Remove(b.serviceBootPlistPath(config.Name))
		_ = os.Remove(b.bootPlistPath(config.Name))
	}
	return nil
}

// renderDarwinSchedulePlist describes one activation of a scheduled job.
// spm-scheduler writes the command's output to logPath and its own records to
// eventsPath; launchd's redirection is not used, so both files can rotate.
func renderDarwinSchedulePlist(config darwinServiceConfig, label, executable, recordPath, stateDir, logPath, eventsPath, activation string, system bool) string {
	var builder strings.Builder
	builder.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n")
	darwinPlistKeyString(&builder, "Label", label)
	builder.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, value := range []string{executable, "run", "--config", recordPath, "--state-dir", stateDir, "--log", logPath, "--events", eventsPath, "--activation", activation} {
		fmt.Fprintf(&builder, "    <string>%s</string>\n", darwinXMLEscape(value))
	}
	builder.WriteString("  </array>\n")
	darwinPlistKeyString(&builder, "StandardOutPath", "/dev/null")
	darwinPlistKeyString(&builder, "StandardErrorPath", "/dev/null")
	if config.Description != "" {
		darwinPlistKeyString(&builder, "ServiceDescription", config.Description)
	}
	if system && config.RunAs != "" {
		darwinPlistKeyString(&builder, "UserName", config.RunAs)
	}
	if system && config.Group != "" {
		darwinPlistKeyString(&builder, "GroupName", config.Group)
	}
	if activation == "boot" {
		builder.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	} else if activation == "daemon" {
		// The shared scheduler starts this loaded, dormant job with launchctl kickstart.
	} else {
		builder.WriteString("  <key>StartInterval</key>\n  <integer>60</integer>\n")
	}
	builder.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")
	builder.WriteString("</dict>\n</plist>\n")
	return builder.String()
}

func (b *darwinBackend) bootstrapSchedule(config darwinServiceConfig) error {
	paths := []string{}
	if config.Schedule != "" || config.TimerInterval != "" {
		paths = append(paths, b.plistPath(config.Name))
	}
	if config.TimerOnBoot != "" {
		paths = append(paths, b.bootPlistPath(config.Name))
	}
	for _, path := range paths {
		if _, err := b.launchctl("bootstrap", b.domain(), path); err != nil {
			return fmt.Errorf("bootstrap schedule %s: %w", config.Name, err)
		}
	}
	return nil
}

func (b *darwinBackend) setScheduledEnabled(config darwinServiceConfig, enabled bool) error {
	_ = b.bootout(config.Name)
	config.Enabled = enabled
	if err := b.writeConfig(config); err != nil {
		return err
	}
	if err := b.writeSchedulePlists(config); err != nil {
		return err
	}
	if b.system && config.RunAs != "" {
		if err := b.secureServiceData(config.Name, config.RunAs, config.Group); err != nil {
			return err
		}
	}
	if enabled {
		if err := b.bootstrapSchedule(config); err != nil {
			return err
		}
	}
	return b.refreshScheduleDaemon()
}

func (b *darwinBackend) refreshScheduleDaemon() error {
	tasks, err := scheduler.DiscoverTasks(b.root)
	if err != nil {
		return err
	}
	target := b.domain() + "/" + b.scheduleDaemonLabel()
	if b.loadedScheduleTarget(target) {
		_, _ = b.launchctl("bootout", target)
	}
	if len(tasks) == 0 {
		_ = os.Remove(b.scheduleDaemonPlistPath())
		_ = os.Remove(b.serviceScheduleDaemonPlistPath())
		return nil
	}
	if err := os.MkdirAll(b.plistDir, 0o755); err != nil {
		return err
	}
	executable, err := runtimeExecutable(schedulerRuntime)
	if err != nil {
		return err
	}
	data := renderDarwinScheduleDaemonPlist(b.scheduleDaemonLabel(), executable, b.root, b.domain())
	if err := writeFileAtomic(b.serviceScheduleDaemonPlistPath(), []byte(data), 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(b.scheduleDaemonPlistPath(), []byte(data), 0o644); err != nil {
		return err
	}
	if _, err := b.launchctl("bootstrap", b.domain(), b.scheduleDaemonPlistPath()); err != nil {
		return fmt.Errorf("bootstrap schedule daemon: %w", err)
	}
	return nil
}

// upgradeRuntime moves this scope's background processes onto the installed
// runtimes.
//
// scheduler: job plists that changed are rewritten and reloaded. A job that
// is running right now is not reloaded, because booting it out would kill the
// run; launchd picks up its rewritten plist at the next login. The scheduler
// daemon is always restarted onto the new binary.
//
// supervisor: every running service's spm-supervisor re-executes the
// installed binary in place (see upgradeSupervisor), so the service keeps
// running.
func (b *darwinBackend) upgradeRuntime(components upgradeComponents) error {
	names, err := b.names()
	if err != nil {
		return err
	}
	var errs []error
	if components.supervisor {
		errs = append(errs, b.upgradeSupervisors(names, components.allowRestart)...)
	}
	if !components.scheduler {
		return errors.Join(errs...)
	}
	for _, name := range names {
		config, err := b.readConfig(name)
		if err != nil || config.Mode != "schedule" {
			continue
		}
		before, _ := os.ReadFile(b.servicePlistPath(name))
		bootBefore, _ := os.ReadFile(b.serviceBootPlistPath(name))
		if err := b.writeSchedulePlists(config); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		after, _ := os.ReadFile(b.servicePlistPath(name))
		bootAfter, _ := os.ReadFile(b.serviceBootPlistPath(name))
		if !config.Enabled || bytes.Equal(before, after) && bytes.Equal(bootBefore, bootAfter) {
			continue
		}
		if b.readState(name).sub == "running" {
			fmt.Fprintf(b.stdout, "%s is running; it switches to spm-scheduler at the next login or reboot\n", name)
			continue
		}
		if err := b.bootout(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if err := b.bootstrapSchedule(config); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	tasks, err := scheduler.DiscoverTasks(b.root)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if len(tasks) > 0 {
		if err := b.refreshScheduleDaemon(); err != nil {
			errs = append(errs, fmt.Errorf("restart spm scheduler: %w", err))
		} else {
			fmt.Fprintln(b.stdout, "spm scheduler restarted")
		}
	}
	return errors.Join(errs...)
}

func (b *darwinBackend) loadedScheduleTarget(target string) bool {
	_, err := b.launchctl("print", target)
	return err == nil
}

// renderDarwinScheduleDaemonPlist describes the scheduler daemon, which
// writes scheduler.log in root itself.
func renderDarwinScheduleDaemonPlist(label, executable, root, domain string) string {
	var builder strings.Builder
	builder.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n")
	darwinPlistKeyString(&builder, "Label", label)
	builder.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, value := range []string{executable, "daemon", "--root", root, "--domain", domain} {
		fmt.Fprintf(&builder, "    <string>%s</string>\n", darwinXMLEscape(value))
	}
	builder.WriteString("  </array>\n")
	darwinPlistKeyString(&builder, "StandardOutPath", "/dev/null")
	darwinPlistKeyString(&builder, "StandardErrorPath", "/dev/null")
	builder.WriteString("  <key>RunAtLoad</key>\n  <true/>\n  <key>KeepAlive</key>\n  <true/>\n  <key>ProcessType</key>\n  <string>Background</string>\n")
	builder.WriteString("</dict>\n</plist>\n")
	return builder.String()
}

const outcomeSwitched = "switched to the new supervisor (kept running)"

func (b *darwinBackend) upgradeSupervisors(names []string, allowRestart bool) []error {
	executable, err := runtimeExecutable(supervisorRuntime)
	if err != nil {
		return []error{err}
	}
	build, err := common.FileBuildID(executable)
	if err != nil {
		return []error{fmt.Errorf("read %s: %w", executable, err)}
	}
	var errs []error
	for _, name := range names {
		config, err := b.readConfig(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if config.Mode == "schedule" {
			continue
		}
		// A plist that points at another supervisor path takes effect the
		// next time launchd loads the service.
		if err := b.writePlist(config); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		outcome, err := b.upgradeSupervisor(name, executable, build, allowRestart)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else if outcome != "" {
			fmt.Fprintf(b.stdout, "%s %s\n", name, outcome)
		}
	}
	return errs
}

// upgradeSupervisor asks a running service's supervisor to exec executable in
// place and waits until the new image reports build. The process ID that
// launchd tracks, the service process and its output pipe all stay, so the
// service is not interrupted. If the switch does not happen, the old
// supervisor keeps running the service; with allowRestart the service is
// restarted onto the new supervisor instead.
func (b *darwinBackend) upgradeSupervisor(name, executable, build string, allowRestart bool) (string, error) {
	launchdState := b.readState(name)
	if launchdState.sub != "running" {
		return "", nil
	}
	layout := b.layout()
	before := layout.ReadState(name)
	if before.SupervisorPID <= 0 || before.PID <= 0 {
		return "", nil
	}
	if before.Build == build {
		return "", nil
	}
	if err := layout.WriteControlRequest(name, supervisor.Control{Action: "reexec", Executable: executable}); err != nil {
		return "", err
	}
	// The supervisor of a system service may run as another account.
	if info, err := os.Stat(b.serviceDir(name)); err == nil {
		if owner, ok := info.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(layout.ControlPath(name), int(owner.Uid), int(owner.Gid))
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		after := layout.ReadState(name)
		if after.SupervisorPID == before.SupervisorPID && after.Build == build {
			return outcomeSwitched, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(layout.ControlPath(name))
	reason := fmt.Sprintf("the running supervisor did not switch to %s within 10s; see '%s'", executable, b.spmLogsCommand(name))
	if !allowRestart {
		return "", fmt.Errorf("%s. It keeps running on its current supervisor; restart it with 'spm restart %s' to move it", reason, name)
	}
	if _, err := b.launchctl("kickstart", "-k", b.target(name)); err != nil {
		return "", fmt.Errorf("%s, and restarting it failed: %w", reason, err)
	}
	return "restarted on the new supervisor", nil
}

func (b *darwinBackend) spmLogsCommand(name string) string {
	if b.system {
		return "sudo spm --system logs --spm " + name
	}
	return "spm logs --spm " + name
}
