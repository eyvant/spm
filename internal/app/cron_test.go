package app

import (
	"strings"
	"testing"
)

func TestCronToOnCalendar(t *testing.T) {
	tests := map[string]string{
		"*/30 * * * *": "*-*-* *:00/30:00",
		"0 3 * * *":    "*-*-* 03:00:00",
		"15 4 1 * *":   "*-*-01 04:15:00",
		"0 9 * * 1-5":  "Mon..Fri *-*-* 09:00:00",
		"0 9 * * 0-1":  "Sun,Mon *-*-* 09:00:00",
		"0 9 * * 0-5":  "Sun,Mon..Fri *-*-* 09:00:00",
		"0 9 * * 0-6":  "Sun,Mon..Sat *-*-* 09:00:00",
		"0 9 * * 0-7":  "*-*-* 09:00:00",
	}
	for expression, want := range tests {
		got, err := cronToOnCalendar(expression)
		if err != nil {
			t.Fatalf("cronToOnCalendar(%q): %v", expression, err)
		}
		if got != want {
			t.Errorf("cronToOnCalendar(%q) = %q, want %q", expression, got, want)
		}
	}
}

func TestCronToOnCalendarRejectsUnsupportedExpressions(t *testing.T) {
	for _, expression := range []string{"* * *", "60 * * * *", "0 0 1 * 1", "0 0 * * */2", "* * * * *\n"} {
		if _, err := cronToOnCalendar(expression); err == nil {
			t.Errorf("cronToOnCalendar(%q) unexpectedly succeeded", expression)
		}
	}
}

func TestRenderCronRestartUnits(t *testing.T) {
	service, timer := renderCronRestartUnits("api", "*/30 * * * *", "", "*-*-* *:00/30:00", "/usr/bin/systemctl", false)
	if !strings.Contains(service, `ExecStart="/usr/bin/systemctl" "--user" "restart" "spm-api.service"`) {
		t.Fatalf("unexpected restart service:\n%s", service)
	}
	for _, value := range []string{"# CronExpression=*/30 * * * *", "StartLimitIntervalSec=0", "OnCalendar=*-*-* *:00/30:00", "Unit=spm-api.cron-restart.service"} {
		if !strings.Contains(timer, value) {
			t.Errorf("timer does not contain %q:\n%s", value, timer)
		}
	}
}

func TestRenderSystemCronRestartUnit(t *testing.T) {
	service, _ := renderCronRestartUnits("api", "*/30 * * * *", "", "*-*-* *:00/30:00", "/usr/bin/systemctl", true)
	if !strings.Contains(service, `ExecStart="/usr/bin/systemctl" "restart" "spm-api.service"`) {
		t.Fatalf("unexpected system restart service:\n%s", service)
	}
	if strings.Contains(service, `"--user"`) {
		t.Fatalf("system restart service contains --user:\n%s", service)
	}
}
