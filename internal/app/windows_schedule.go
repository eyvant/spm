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

func (b *windowsBackend) scheduleLauncherPath(name, activation string) string {
	return filepath.Join(b.serviceDir(name), "schedule-"+activation+".vbs")
}

func (b *windowsBackend) scheduleTaskName(name, kind string) string {
	suffix := ""
	if kind == "boot" {
		suffix = "-boot"
	}
	return "spm-" + name + "-schedule" + suffix
}

func triggerWindowsScheduledTask(name string) error {
	command := windowsExecCommand("schtasks.exe", "/Run", "/TN", "spm-"+name+"-schedule")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
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
	if err := b.refreshScheduleDaemon(); err != nil {
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
		launcher := b.scheduleLauncherPath(config.Name, "daemon")
		if err := writeWindowsScheduleLauncher(launcher, executable, b.scheduleRecordPath(config.Name), b.scheduleStateDir(config.Name), b.logPath(config.Name), "daemon"); err != nil {
			return err
		}
		_ = os.Remove(b.scheduleLauncherPath(config.Name, "timer"))
		xml := renderWindowsScheduleTask(config, launcher, principal, logonType, "daemon", b.system)
		if err := b.createScheduledTask(b.scheduleTaskName(config.Name, "timer"), xml); err != nil {
			return err
		}
	} else {
		_ = b.deleteScheduledTask(b.scheduleTaskName(config.Name, "timer"))
		_ = os.Remove(b.scheduleLauncherPath(config.Name, "daemon"))
	}
	if config.TimerOnBoot != "" {
		launcher := b.scheduleLauncherPath(config.Name, "boot")
		if err := writeWindowsScheduleLauncher(launcher, executable, b.scheduleRecordPath(config.Name), b.scheduleStateDir(config.Name), b.logPath(config.Name), "boot"); err != nil {
			return err
		}
		xml := renderWindowsScheduleTask(config, launcher, principal, logonType, "boot", b.system)
		if err := b.createScheduledTask(b.scheduleTaskName(config.Name, "boot"), xml); err != nil {
			return err
		}
	} else {
		_ = b.deleteScheduledTask(b.scheduleTaskName(config.Name, "boot"))
		_ = os.Remove(b.scheduleLauncherPath(config.Name, "boot"))
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

func writeWindowsScheduleLauncher(path, executable, recordPath, stateDir, logPath, activation string) error {
	arguments := []string{"__scheduled-run", "--config", recordPath, "--state-dir", stateDir, "--log", logPath, "--activation", activation}
	command := windowsQuoteCommand(executable)
	for _, value := range arguments {
		command += " " + windowsQuoteCommand(value)
	}
	script := "Set shell = CreateObject(\"WScript.Shell\")\r\n" +
		"exitCode = shell.Run(" + vbscriptString(command) + ", 0, True)\r\n" +
		"WScript.Quit exitCode\r\n"
	return writeFileAtomic(path, []byte(script), 0o600)
}

func vbscriptString(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func renderWindowsScheduleTask(config windowsServiceConfig, launcherPath, principal, logonType, activation string, system bool) string {
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
	} else if activation == "daemon" {
		trigger = ""
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
		"  <Actions Context=\"Author\"><Exec><Command>wscript.exe</Command><Arguments>//B //Nologo " + windowsXMLEscape(windowsQuoteCommand(launcherPath)) + "</Arguments></Exec></Actions>\n" +
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
	for _, value := range xml {
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
	command := windowsExecCommand("schtasks.exe", "/Create", "/TN", name, "/XML", path, "/F")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("register scheduled task %s: %s: %w", name, strings.TrimSpace(string(output)), err)
	}
	return nil
}

func (b *windowsBackend) deleteScheduledTask(name string) error {
	query := windowsExecCommand("schtasks.exe", "/Query", "/TN", name)
	query.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := query.Run(); err != nil {
		return nil
	}
	command := windowsExecCommand("schtasks.exe", "/Delete", "/TN", name, "/F")
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
	if err := b.registerScheduledTask(config); err != nil {
		return err
	}
	return b.refreshScheduleDaemon()
}

func (b *windowsBackend) scheduleDaemonTaskName() string {
	if b.system {
		return "spm-system-scheduler"
	}
	current, err := user.Current()
	if err != nil {
		return "spm-user-scheduler"
	}
	return "spm-user-scheduler-" + strings.ReplaceAll(current.Uid, "-", "")
}

func (b *windowsBackend) scheduleDaemonLauncherPath() string {
	return filepath.Join(b.root, "scheduler.vbs")
}

func (b *windowsBackend) scheduleDaemonStartupPath() string {
	directory, _ := windowsStartupDirectory()
	return filepath.Join(directory, "spm-schedule-daemon.lnk")
}

func (b *windowsBackend) registerUserScheduleDaemonStartup(taskName string) error {
	startup, err := windowsStartupDirectory()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(startup, 0o700); err != nil {
		return fmt.Errorf("create Windows Startup directory: %w", err)
	}
	target, err := exec.LookPath("schtasks.exe")
	if err != nil {
		return fmt.Errorf("find schtasks.exe: %w", err)
	}
	arguments := "/Run /TN " + windowsQuoteCommand(taskName)
	cmd := windowsExecCommand("powershell.exe", "-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", windowsCreateShortcutScript)
	cmd.Env = append(os.Environ(), "SPM_SHORTCUT_PATH="+b.scheduleDaemonStartupPath(), "SPM_SHORTCUT_TARGET="+target, "SPM_SHORTCUT_ARGS="+arguments, "SPM_SHORTCUT_DESCRIPTION=Start the spm user scheduler at logon")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create scheduler logon shortcut: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func (b *windowsBackend) refreshScheduleDaemon() error {
	tasks, err := discoverPortableSchedulerTasks(b.root)
	if err != nil {
		return err
	}
	name := b.scheduleDaemonTaskName()
	if len(tasks) == 0 {
		_ = b.endScheduledTask(name)
		_ = b.deleteScheduledTask(name)
		_ = os.Remove(b.scheduleDaemonLauncherPath())
		if !b.system {
			_ = os.Remove(b.scheduleDaemonStartupPath())
		}
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := writeWindowsHiddenLauncher(b.scheduleDaemonLauncherPath(), executable, []string{"__windows-schedule-daemon", "--root", b.root}); err != nil {
		return err
	}
	principal, logonType, err := b.scheduledTaskPrincipal("")
	if err != nil {
		return err
	}
	xml := renderWindowsScheduleDaemonTask(b.scheduleDaemonLauncherPath(), principal, logonType, b.system)
	_ = b.endScheduledTask(name)
	if err := b.createScheduledTask(name, xml); err != nil {
		return err
	}
	if !b.system {
		if err := b.registerUserScheduleDaemonStartup(name); err != nil {
			_ = b.deleteScheduledTask(name)
			return err
		}
	}
	command := exec.Command("schtasks.exe", "/Run", "/TN", name)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("start scheduler: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func writeWindowsHiddenLauncher(path, executable string, arguments []string) error {
	command := windowsQuoteCommand(executable)
	for _, value := range arguments {
		command += " " + windowsQuoteCommand(value)
	}
	script := "Set shell = CreateObject(\"WScript.Shell\")\r\n" +
		"exitCode = shell.Run(" + vbscriptString(command) + ", 0, True)\r\n" +
		"WScript.Quit exitCode\r\n"
	return writeFileAtomic(path, []byte(script), 0o600)
}

func renderWindowsScheduleDaemonTask(launcherPath, principal, logonType string, system bool) string {
	trigger, runLevel := "", "LeastPrivilege"
	if system {
		trigger, runLevel = "<BootTrigger><Enabled>true</Enabled></BootTrigger>", "HighestAvailable"
	}
	logonTypeXML := ""
	if logonType != "" {
		logonTypeXML = "<LogonType>" + windowsXMLEscape(logonType) + "</LogonType>"
	}
	return `<?xml version="1.0" encoding="UTF-16"?>` + "\n" +
		`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\n" +
		"  <RegistrationInfo><Description>SPM portable schedule daemon</Description></RegistrationInfo>\n" +
		"  <Triggers>" + trigger + "</Triggers>\n" +
		"  <Principals><Principal id=\"Author\"><UserId>" + windowsXMLEscape(principal) + "</UserId>" + logonTypeXML + "<RunLevel>" + runLevel + "</RunLevel></Principal></Principals>\n" +
		"  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><Enabled>true</Enabled><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure></Settings>\n" +
		"  <Actions Context=\"Author\"><Exec><Command>wscript.exe</Command><Arguments>//B //Nologo " + windowsXMLEscape(windowsQuoteCommand(launcherPath)) + "</Arguments></Exec></Actions>\n" +
		"</Task>\n"
}

func (b *windowsBackend) endScheduledTask(name string) error {
	command := exec.Command("schtasks.exe", "/End", "/TN", name)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = command.Run()
	return nil
}
