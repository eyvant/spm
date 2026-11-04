//go:build windows

package app

import (
	"io"
	"os"
	"path/filepath"
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
		{value: `NT SERVICE\spm-api`, system: true, want: `NT SERVICE\spm-api`},
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

func TestRenderWindowsScheduleTaskUsesPortableRunner(t *testing.T) {
	config := windowsServiceConfig{Name: "backup", Description: "Nightly & safe", Enabled: true, Schedule: "*/5 * * * *"}
	xml := renderWindowsScheduleTask(config, `C:\Program Files\spm\spm.exe`, `C:\data\schedule.json`, `C:\data\state`, `C:\data\task.log`, "S-1-5-18", "", "timer", true)
	for _, want := range []string{"<Interval>PT1M</Interval>", "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<RunLevel>HighestAvailable</RunLevel>", "__scheduled-run", "Nightly &amp; safe", "S-1-5-18"} {
		if !strings.Contains(xml, want) {
			t.Errorf("task XML does not contain %q:\n%s", want, xml)
		}
	}
	if strings.Contains(xml, "<LogonType>") {
		t.Errorf("system task XML should omit LogonType for Task Scheduler compatibility:\n%s", xml)
	}
}

func TestWindowsScheduledSystemPrincipalsOmitLogonType(t *testing.T) {
	b := &windowsBackend{system: true}
	for _, test := range []struct {
		runAs string
		want  string
	}{
		{"SYSTEM", "S-1-5-18"},
		{`NT AUTHORITY\LOCALSERVICE`, "S-1-5-19"},
		{`NT AUTHORITY\NETWORKSERVICE`, "S-1-5-20"},
	} {
		principal, logonType, err := b.scheduledTaskPrincipal(test.runAs)
		if err != nil || principal != test.want || logonType != "" {
			t.Errorf("scheduledTaskPrincipal(%q) = %q, %q, %v; want %q, empty logon type", test.runAs, principal, logonType, err, test.want)
		}
	}
}

func TestWindowsScheduledDeclarationIsPortable(t *testing.T) {
	b := &windowsBackend{root: t.TempDir(), stdout: io.Discard, stderr: io.Discard}
	service := serviceDeclaration{Name: "job", Command: []string{"cmd.exe", "/c", "exit", "0"}, Schedule: "*/5 9-17 1 * 1-5", Timezone: "Asia/Singapore", Persistent: true}
	config, err := b.prepareDeclaration(service)
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != "schedule" || config.Schedule != service.Schedule || config.Timezone != service.Timezone || !config.Persistent {
		t.Fatalf("unexpected schedule config: %#v", config)
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

func TestWindowsSCMServiceName(t *testing.T) {
	backend := &windowsBackend{system: true}
	if got := backend.scmName("api"); got != "spm-api" {
		t.Fatalf("SCM service name = %q, want spm-api", got)
	}
}

func TestWindowsVirtualServiceAccount(t *testing.T) {
	if !windowsVirtualServiceAccount(`NT SERVICE\spm-api`) || windowsVirtualServiceAccount(`DOMAIN\svc-api`) {
		t.Fatal("virtual service account detection is incorrect")
	}
}

func TestWindowsSystemRootACLGrantsAllowServiceAccountTraversal(t *testing.T) {
	grants := strings.Join(windowsSystemRootACLGrants(), "\n")
	for _, expected := range []string{"*S-1-5-19:(RX)", "*S-1-5-20:(RX)", "*S-1-5-11:(RX)"} {
		if !strings.Contains(grants, expected) {
			t.Fatalf("root ACL grants %q do not include %q", grants, expected)
		}
	}
}

func TestWindowsSystemDataACLKeepsConfigAccessible(t *testing.T) {
	if !windowsIsElevated() {
		t.Skip("requires elevated token to test SYSTEM/Administrators ACLs")
	}
	b := &windowsBackend{root: t.TempDir(), system: true, stdout: io.Discard, stderr: io.Discard}
	config := windowsServiceConfig{Version: windowsStateVersion, Name: "acl-test", Command: []string{"cmd.exe"}, RunAs: `NT AUTHORITY\LOCALSERVICE`}
	if err := b.writeConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := b.secureSystemData(config.Name, config.RunAs); err != nil {
		t.Fatal(err)
	}
	if _, err := b.readConfig(config.Name); err != nil {
		t.Fatalf("config inaccessible after securing ACLs: %v", err)
	}
	if err := b.writeConfig(config); err != nil {
		t.Fatalf("cannot replace secured config: %v", err)
	}
	if _, err := b.readConfig(config.Name); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPreparesPowerShellScriptAutomatically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.ps1")
	if err := os.WriteFile(path, []byte("Write-Output ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options, err := parseStart([]string{"--name", "worker", path})
	if err != nil {
		t.Fatal(err)
	}
	if err := options.prepare(); err != nil {
		t.Fatal(err)
	}
	command := strings.Join(options.command, " ")
	for _, want := range []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path} {
		if !strings.Contains(strings.ToLower(command), strings.ToLower(want)) {
			t.Fatalf("PowerShell command %q does not contain %q", command, want)
		}
	}
}
