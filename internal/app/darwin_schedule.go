//go:build darwin

package app

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	record := portableScheduleRecord{
		Version: portableScheduleVersion, Name: name, Command: append([]string(nil), config.Command...), Cwd: config.Cwd,
		Env: append([]string(nil), config.Env...), EnvFile: config.EnvFile, Schedule: config.Schedule, Timezone: config.Timezone,
		TimerOnBoot: config.TimerOnBoot, TimerInterval: config.TimerInterval, TimerRandomDelay: config.TimerRandomDelay,
		Persistent: config.Persistent, CreatedAt: time.Now().UTC(),
	}
	if err := preparePortableScheduleUpdate(b.scheduleRecordPath(name), b.scheduleStateDir(name), &record); err != nil {
		return err
	}
	if err := writePortableSchedule(b.scheduleRecordPath(name), record); err != nil {
		_ = os.RemoveAll(b.serviceDir(name))
		return err
	}
	if err := b.writeSchedulePlists(config); err != nil {
		return err
	}
	if b.system && runAs != "" {
		if err := b.secureScheduleData(name, runAs, group); err != nil {
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

func (b *darwinBackend) secureScheduleData(name, account, group string) error {
	owner, err := user.Lookup(account)
	if err != nil {
		return fmt.Errorf("look up scheduled task user %q: %w", account, err)
	}
	uid, err := strconv.Atoi(owner.Uid)
	if err != nil {
		return err
	}
	gidText := owner.Gid
	if group != "" {
		groupEntry, lookupErr := user.LookupGroup(group)
		if lookupErr != nil {
			return fmt.Errorf("look up scheduled task group %q: %w", group, lookupErr)
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
	args := []string{"--config", b.scheduleRecordPath(name), "--state-dir", b.scheduleStateDir(name), "--log", b.logPath(name), "--activation", "timer"}
	if force {
		args = append(args, "--force")
	}
	return runPortableSchedule(args, b.stdout, b.stderr)
}

func (b *darwinBackend) writeSchedulePlists(config darwinServiceConfig) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if config.Schedule != "" || config.TimerInterval != "" {
		data := renderDarwinSchedulePlist(config, b.label(config.Name), executable, b.scheduleRecordPath(config.Name), b.scheduleStateDir(config.Name), b.logPath(config.Name), "daemon", b.system)
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
		data := renderDarwinSchedulePlist(config, b.bootLabel(config.Name), executable, b.scheduleRecordPath(config.Name), b.scheduleStateDir(config.Name), b.logPath(config.Name), "boot", b.system)
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

func renderDarwinSchedulePlist(config darwinServiceConfig, label, executable, recordPath, stateDir, logPath, activation string, system bool) string {
	var builder strings.Builder
	builder.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n")
	darwinPlistKeyString(&builder, "Label", label)
	builder.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, value := range []string{executable, "__scheduled-run", "--config", recordPath, "--state-dir", stateDir, "--log", logPath, "--activation", activation} {
		fmt.Fprintf(&builder, "    <string>%s</string>\n", darwinXMLEscape(value))
	}
	builder.WriteString("  </array>\n")
	darwinPlistKeyString(&builder, "StandardOutPath", logPath)
	darwinPlistKeyString(&builder, "StandardErrorPath", logPath)
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
		if err := b.secureScheduleData(config.Name, config.RunAs, config.Group); err != nil {
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
	tasks, err := discoverPortableSchedulerTasks(b.root)
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
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	data := renderDarwinScheduleDaemonPlist(b.scheduleDaemonLabel(), executable, b.root, b.domain(), filepath.Join(b.root, "scheduler.log"))
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

func (b *darwinBackend) loadedScheduleTarget(target string) bool {
	_, err := b.launchctl("print", target)
	return err == nil
}

func renderDarwinScheduleDaemonPlist(label, executable, root, domain, logPath string) string {
	var builder strings.Builder
	builder.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n")
	darwinPlistKeyString(&builder, "Label", label)
	builder.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, value := range []string{executable, "__darwin-schedule-daemon", "--root", root, "--domain", domain} {
		fmt.Fprintf(&builder, "    <string>%s</string>\n", darwinXMLEscape(value))
	}
	builder.WriteString("  </array>\n")
	darwinPlistKeyString(&builder, "StandardOutPath", logPath)
	darwinPlistKeyString(&builder, "StandardErrorPath", logPath)
	builder.WriteString("  <key>RunAtLoad</key>\n  <true/>\n  <key>KeepAlive</key>\n  <true/>\n  <key>ProcessType</key>\n  <string>Background</string>\n")
	builder.WriteString("</dict>\n</plist>\n")
	return builder.String()
}
