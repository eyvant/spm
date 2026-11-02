//go:build windows

package app

import (
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
