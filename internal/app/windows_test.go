//go:build windows

package app

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestWindowsRunAsScopesAndAliases(t *testing.T) {
	tests := []struct {
		value  string
		system bool
		want   string
		err    string
	}{
		{system: false, want: ""},
		{value: "bob", system: false, err: "requires global --system"},
		{system: true, want: "SYSTEM"},
		{value: "LocalSystem", system: true, want: "SYSTEM"},
		{value: "LocalService", system: true, want: `NT AUTHORITY\LOCALSERVICE`},
		{value: `NT AUTHORITY\NETWORK SERVICE`, system: true, want: `NT AUTHORITY\NETWORKSERVICE`},
		{value: `DOMAIN\svc-spm`, system: true, want: `DOMAIN\svc-spm`},
		{value: `DOMAIN\gmsa$`, system: true, want: `DOMAIN\gmsa$`},
	}
	for _, test := range tests {
		got, err := windowsRunAs(test.value, test.system)
		if test.err != "" {
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Errorf("windowsRunAs(%q, %v) error = %v, want %q", test.value, test.system, err, test.err)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("windowsRunAs(%q, %v) = %q, %v; want %q", test.value, test.system, got, err, test.want)
		}
	}
}

func TestWindowsEnvironmentOverridesCaseInsensitively(t *testing.T) {
	got := setWindowsEnv([]string{"Path=one", "KEEP=yes"}, "PATH", "two")
	want := []string{"PATH=two", "KEEP=yes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("setWindowsEnv() = %v, want %v", got, want)
	}
}

func TestWindowsRejectsPlatformSpecificStartOptions(t *testing.T) {
	options := defaultStartOptions()
	options.instances = 2
	if err := rejectWindowsStartOptions(options); err == nil || !strings.Contains(err.Error(), "--instances") {
		t.Fatalf("rejectWindowsStartOptions() error = %v", err)
	}
}

func TestWindowsTaskNamesSeparateScopes(t *testing.T) {
	t.Setenv("USERNAME", "alice")
	user := (&windowsBackend{}).taskName("api")
	system := (&windowsBackend{system: true}).taskName("api")
	if user != "spm-user-alice-api" || system != "spm-system-api" {
		t.Fatalf("task names = %q, %q", user, system)
	}
}

func TestWindowsStartupTaskHasNoExecutionDeadline(t *testing.T) {
	if !strings.Contains(windowsRegisterTaskScript, "[TimeSpan]::Zero") || !strings.Contains(windowsRegisterTaskScript, "MultipleInstances IgnoreNew") {
		t.Fatalf("startup task settings are not suitable for a persistent supervisor")
	}
}

func TestWindowsStartupTaskPowerShellParses(t *testing.T) {
	command := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-Command", `[ScriptBlock]::Create([Console]::In.ReadToEnd()) | Out-Null`)
	command.Stdin = strings.NewReader(windowsRegisterTaskScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("PowerShell startup task script does not parse: %v\n%s", err, output)
	}
}
