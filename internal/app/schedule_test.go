package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScheduleInstallsOneshotServiceAndTimer(t *testing.T) {
	directory := localTempDir(t)
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: &strings.Builder{}, stderr: &strings.Builder{}, unitDir: directory, configDir: filepath.Join(directory, "config")}
	if err := a.schedule([]string{"--name", "backup", "--cron", "*/30 * * * *", "--persistent", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(a.unitPath("backup"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "Type=oneshot") || strings.Contains(string(unit), "Restart=") {
		t.Fatalf("unexpected scheduled service:\n%s", unit)
	}
	timer, err := os.ReadFile(a.scheduleTimerPath("backup"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# CronExpression=*/30 * * * *", "StartLimitIntervalSec=0", "Persistent=true", "Unit=spm-backup.service"} {
		if !strings.Contains(string(timer), want) {
			t.Errorf("timer does not contain %q:\n%s", want, timer)
		}
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user enable --now spm-backup.timer") {
		t.Fatalf("schedule timer was not enabled:\n%s", got)
	}
	config, err := a.readServiceConfig("backup")
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != "schedule" || config.Schedule != "*/30 * * * *" || !config.Persistent {
		t.Fatalf("unexpected saved config: %#v", config)
	}
}

func TestScheduledTaskControlTargetsTimerForEnable(t *testing.T) {
	directory := localTempDir(t)
	for _, filename := range []string{unitName("backup"), scheduleTimerName("backup")} {
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{}
	a := &application{runner: runner, stdout: &strings.Builder{}, stderr: &strings.Builder{}, unitDir: directory}
	if err := a.control("enable", "backup"); err != nil {
		t.Fatal(err)
	}
	if got := callsString(runner.calls); !strings.Contains(got, "systemctl --user enable --now spm-backup.timer") {
		t.Fatalf("enable did not target schedule timer:\n%s", got)
	}
}

func TestParseScheduleRequiresCron(t *testing.T) {
	if _, err := parseSchedule([]string{"--", "go", "version"}); err == nil {
		t.Fatal("schedule without --cron unexpectedly succeeded")
	}
}

func TestScheduleSupportsBootAndIntervalTimers(t *testing.T) {
	directory := localTempDir(t)
	a := &application{runner: &fakeRunner{}, stdout: &strings.Builder{}, stderr: &strings.Builder{}, unitDir: directory, configDir: filepath.Join(directory, "config")}
	if err := a.schedule([]string{"--name", "sync", "--on-boot", "5min", "--interval", "30min", "--randomized-delay", "2min", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	timer, err := os.ReadFile(a.scheduleTimerPath("sync"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OnBootSec=5min", "OnUnitActiveSec=30min", "RandomizedDelaySec=2min"} {
		if !strings.Contains(string(timer), want) {
			t.Errorf("timer does not contain %q:\n%s", want, timer)
		}
	}
	if strings.Contains(string(timer), "OnCalendar=") || strings.Contains(string(timer), "Persistent=true") {
		t.Fatalf("unexpected calendar settings:\n%s", timer)
	}
}

func TestScheduleIntervalOnlySetsInitialAndRepeatingTrigger(t *testing.T) {
	options, err := parseSchedule([]string{"--interval", "15min", "--", "go", "version"})
	if err != nil {
		t.Fatal(err)
	}
	config := persistedConfigFrom(options.start)
	config.Mode, config.TimerInterval = "schedule", options.interval
	timer := renderScheduleTimer("sync", config, "")
	if !strings.Contains(timer, "OnActiveSec=15min") || !strings.Contains(timer, "OnUnitActiveSec=15min") {
		t.Fatalf("interval-only timer does not start and repeat:\n%s", timer)
	}
}

func TestReadScheduleDescribesNonCronTimer(t *testing.T) {
	directory := localTempDir(t)
	a := &application{unitDir: directory}
	config := persistedStartConfig{Mode: "schedule", TimerOnBoot: "5min", TimerInterval: "30min", TimerRandomDelay: "2min"}
	if err := os.WriteFile(a.scheduleTimerPath("sync"), []byte(renderScheduleTimer("sync", config, "")), 0o644); err != nil {
		t.Fatal(err)
	}
	got := a.readSchedule("sync")
	for _, want := range []string{"boot+5min", "every 30min", "jitter 2min"} {
		if !strings.Contains(got, want) {
			t.Errorf("schedule description %q does not contain %q", got, want)
		}
	}
}
