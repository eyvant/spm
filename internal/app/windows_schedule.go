//go:build windows

package app

import (
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
	"unicode/utf16"
)

func (b *windowsBackend) scheduleRecordPath(name string) string {
	return filepath.Join(b.serviceDir(name), "schedule.json")
}

func (b *windowsBackend) scheduleStateDir(name string) string {
	return filepath.Join(b.serviceDir(name), "schedule-state")
}

func (b *windowsBackend) scheduleTaskName(name, kind string) string {
	suffix := ""
	if kind == "boot" {
		suffix = "-boot"
	}
	return "spm-" + name + "-schedule" + suffix
}

func (b *windowsBackend) schedule(args []string) error {
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
	runAs, err := windowsRunAs(options.start.runAsUser, b.system)
	if err != nil {
		return err
	}
	if options.start.runAsGroup != "" {
		return fmt.Errorf("--group has no Windows scheduling equivalent")
	}
	if b.system && !windowsScheduledServiceAccount(runAs) {
		return fmt.Errorf("scheduled system tasks currently require SYSTEM, LocalService, or NetworkService; got %q", runAs)
	}
	options.start.runAsUser, options.start.runAsGroup = "", ""
	timezone := options.start.timezone
	options.start.timezone = ""
	if err := rejectWindowsStartOptions(options.start); err != nil {
		return err
	}
	if err := options.start.prepare(); err != nil {
		return err
	}
	name := options.start.name
	if b.exists(name) {
		_ = b.stop(name)
		_ = b.unregister(name)
	}
	config := windowsServiceConfig{
		Version: windowsStateVersion, Name: name, Description: options.start.description,
		Cwd: options.start.cwd, Command: append([]string(nil), options.start.command...), Env: append([]string(nil), options.start.env...),
		EnvFile: options.start.envFile, Restart: "no", RestartSec: options.start.restartSec, Enabled: options.start.enable,
		RunAs: runAs, After: append([]string(nil), options.start.after...), Wants: append([]string(nil), options.start.wants...), Requires: append([]string(nil), options.start.requires...),
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
	if b.system {
		if err := b.secureSystemData(name, runAs); err != nil {
			_ = os.RemoveAll(b.serviceDir(name))
			return err
		}
	}
	if err := b.registerScheduledTask(config); err != nil {
		_ = b.unregisterScheduledTask(name)
		_ = os.RemoveAll(b.serviceDir(name))
		return err
	}
	if options.runNow {
		if err := b.runScheduledTask(name, true); err != nil {
			return err
		}
	}
	fmt.Fprintf(b.stdout, "%s scheduled with Windows Task Scheduler\n", name)
	return nil
}

func windowsScheduledServiceAccount(account string) bool {
	upper := strings.ToUpper(account)
	return upper == "SYSTEM" || upper == `NT AUTHORITY\LOCALSERVICE` || upper == `NT AUTHORITY\NETWORKSERVICE`
}

func (b *windowsBackend) runScheduledTask(name string, force bool) error {
	args := []string{"--config", b.scheduleRecordPath(name), "--state-dir", b.scheduleStateDir(name), "--log", b.logPath(name), "--activation", "timer"}
	if force {
		args = append(args, "--force")
	}
	return runPortableSchedule(args, b.stdout, b.stderr)
}

func (b *windowsBackend) registerScheduledTask(config windowsServiceConfig) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	principal, logonType, err := b.scheduledTaskPrincipal(config.RunAs)
	if err != nil {
		return err
	}
	if config.Schedule != "" || config.TimerInterval != "" {
		xml := renderWindowsScheduleTask(config, executable, b.scheduleRecordPath(config.Name), b.scheduleStateDir(config.Name), b.logPath(config.Name), principal, logonType, "timer", b.system)
		if err := b.createScheduledTask(b.scheduleTaskName(config.Name, "timer"), xml); err != nil {
			return err
		}
	} else {
		_ = b.deleteScheduledTask(b.scheduleTaskName(config.Name, "timer"))
	}
	if config.TimerOnBoot != "" {
		xml := renderWindowsScheduleTask(config, executable, b.scheduleRecordPath(config.Name), b.scheduleStateDir(config.Name), b.logPath(config.Name), principal, logonType, "boot", b.system)
		if err := b.createScheduledTask(b.scheduleTaskName(config.Name, "boot"), xml); err != nil {
			return err
		}
	} else {
		_ = b.deleteScheduledTask(b.scheduleTaskName(config.Name, "boot"))
	}
	return nil
}

func (b *windowsBackend) scheduledTaskPrincipal(runAs string) (string, string, error) {
	if !b.system {
		current, err := user.Current()
		if err != nil {
			return "", "", err
		}
		// Task Scheduler accepts a SID reliably on local, domain-joined, and
		// ephemeral CI machines. A DOMAIN\user name can be rejected as a
		// network address when the domain controller is unavailable.
		return current.Uid, "InteractiveToken", nil
	}
	switch strings.ToUpper(runAs) {
	case "", "SYSTEM":
		return "S-1-5-18", "", nil
	case `NT AUTHORITY\LOCALSERVICE`:
		return "S-1-5-19", "", nil
	case `NT AUTHORITY\NETWORKSERVICE`:
		return "S-1-5-20", "", nil
	default:
		return "", "", fmt.Errorf("unsupported scheduled task account %q", runAs)
	}
}

func renderWindowsScheduleTask(config windowsServiceConfig, executable, recordPath, stateDir, logPath, principal, logonType, activation string, system bool) string {
	arguments := []string{"__scheduled-run", "--config", recordPath, "--state-dir", stateDir, "--log", logPath, "--activation", activation}
	quoted := make([]string, len(arguments))
	for index, value := range arguments {
		quoted[index] = windowsQuoteCommand(value)
	}
	trigger := ""
	runLevel := "LeastPrivilege"
	if system {
		runLevel = "HighestAvailable"
	}
	if activation == "boot" {
		kind := "BootTrigger"
		if !system {
			kind = "LogonTrigger"
		}
		trigger = fmt.Sprintf("    <%s>\n      <Enabled>%t</Enabled>\n    </%s>\n", kind, config.Enabled, kind)
	} else {
		start := time.Now().Add(time.Minute).Truncate(time.Minute).Format("2006-01-02T15:04:05")
		trigger = fmt.Sprintf("    <CalendarTrigger>\n      <Repetition><Interval>PT1M</Interval><Duration>P1D</Duration><StopAtDurationEnd>false</StopAtDurationEnd></Repetition>\n      <StartBoundary>%s</StartBoundary>\n      <Enabled>%t</Enabled>\n      <ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>\n    </CalendarTrigger>\n", start, config.Enabled)
	}
	logonTypeXML := ""
	if logonType != "" {
		logonTypeXML = "<LogonType>" + windowsXMLEscape(logonType) + "</LogonType>"
	}
	return `<?xml version="1.0" encoding="UTF-16"?>` + "\n" +
		`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\n" +
		"  <RegistrationInfo><Description>" + windowsXMLEscape(valueOr(config.Description, "SPM scheduled task "+config.Name)) + "</Description></RegistrationInfo>\n" +
		"  <Triggers>\n" + trigger + "  </Triggers>\n" +
		"  <Principals><Principal id=\"Author\"><UserId>" + windowsXMLEscape(principal) + "</UserId>" + logonTypeXML + "<RunLevel>" + runLevel + "</RunLevel></Principal></Principals>\n" +
		"  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><Enabled>" + strconv.FormatBool(config.Enabled) + "</Enabled><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>\n" +
		"  <Actions Context=\"Author\"><Exec><Command>" + windowsXMLEscape(executable) + "</Command><Arguments>" + windowsXMLEscape(strings.Join(quoted, " ")) + "</Arguments></Exec></Actions>\n" +
		"</Task>\n"
}

func windowsXMLEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(value)
}

func (b *windowsBackend) createScheduledTask(name, xml string) error {
	file, err := os.CreateTemp(b.root, "schedule-*.xml")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write([]byte{'\xff', '\xfe'}); err != nil {
		_ = file.Close()
		return err
	}
	// schtasks expects UTF-16 when the XML declaration says UTF-16.
	for _, value := range []rune(xml) {
		encoded := utf16.Encode([]rune{value})
		for _, unit := range encoded {
			if _, err := file.Write([]byte{byte(unit), byte(unit >> 8)}); err != nil {
				_ = file.Close()
				return err
			}
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	command := exec.Command("schtasks.exe", "/Create", "/TN", name, "/XML", path, "/F")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("register scheduled task %s: %s: %w", name, strings.TrimSpace(string(output)), err)
	}
	return nil
}

func (b *windowsBackend) deleteScheduledTask(name string) error {
	query := exec.Command("schtasks.exe", "/Query", "/TN", name)
	query.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := query.Run(); err != nil {
		return nil
	}
	command := exec.Command("schtasks.exe", "/Delete", "/TN", name, "/F")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("delete scheduled task %s: %s: %w", name, strings.TrimSpace(string(output)), err)
	}
	return nil
}

func (b *windowsBackend) unregisterScheduledTask(name string) error {
	var errs []error
	for _, kind := range []string{"timer", "boot"} {
		if err := b.deleteScheduledTask(b.scheduleTaskName(name, kind)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *windowsBackend) setScheduledEnabled(config windowsServiceConfig, enabled bool) error {
	config.Enabled = enabled
	if err := b.writeConfig(config); err != nil {
		return err
	}
	return b.registerScheduledTask(config)
}
