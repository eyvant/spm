//go:build darwin

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinWritePlist(t *testing.T) {
	directory := t.TempDir()
	b := &darwinBackend{root: directory, plistDir: directory, uid: os.Getuid()}
	config := darwinServiceConfig{
		Version: darwinConfigVersion, Name: "api", Description: "API & worker",
		Cwd: directory, Command: []string{"/bin/echo", "<ready>"}, Env: []string{"MODE=a&b"},
		Restart: "on-failure", RestartSec: "4s", Enabled: true,
	}
	if err := b.writePlist(config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "services", "api", "service.plist"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{
		"<string>com.eyvant.spm.api</string>", "<string>&lt;ready&gt;</string>",
		"<string>API &amp; worker</string>", "<key>SuccessfulExit</key>", "<false/>",
		"<integer>4</integer>", "<key>MODE</key>", "<string>a&amp;b</string>",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("plist does not contain %q:\n%s", expected, text)
		}
	}
}

func TestDarwinXMLAndRestartPolicies(t *testing.T) {
	if got := darwinXMLEscape(`a<&>"'`); got != "a&lt;&amp;&gt;&quot;&apos;" {
		t.Fatalf("escape = %q", got)
	}
	directory := t.TempDir()
	b := &darwinBackend{root: directory, plistDir: directory, uid: os.Getuid()}
	for policy, expected := range map[string]string{"always": "<true/>", "on-success": "<key>SuccessfulExit</key>", "no": ""} {
		config := darwinServiceConfig{Version: 1, Name: "job-" + strings.ReplaceAll(policy, "-", ""), Cwd: directory, Command: []string{"/usr/bin/true"}, Restart: policy, RestartSec: "500ms"}
		if err := b.writePlist(config); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(b.servicePlistPath(config.Name))
		if err != nil {
			t.Fatal(err)
		}
		if expected != "" && !strings.Contains(string(data), expected) {
			t.Fatalf("%s plist missing %q", policy, expected)
		}
		if policy == "no" && strings.Contains(string(data), "<key>KeepAlive</key>") {
			t.Fatalf("no policy unexpectedly has KeepAlive: %s", data)
		}
		if policy == "no" && !strings.Contains(string(data), "<key>ThrottleInterval</key>\n  <integer>1</integer>") {
			t.Fatalf("no policy should use a one-second launchd throttle: %s", data)
		}
	}
}

func TestDarwinThrottleRoundsRestartDelayUp(t *testing.T) {
	directory := t.TempDir()
	b := &darwinBackend{root: directory, plistDir: directory, uid: os.Getuid()}
	config := darwinServiceConfig{Version: 1, Name: "worker", Cwd: directory, Command: []string{"/usr/bin/true"}, Restart: "always", RestartSec: "1500ms"}
	if err := b.writePlist(config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(b.servicePlistPath(config.Name))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<key>ThrottleInterval</key>\n  <integer>2</integer>") {
		t.Fatalf("restart delay should round up to whole seconds: %s", data)
	}
}

func TestRenderDarwinSchedulePlistUsesPortableRunner(t *testing.T) {
	config := darwinServiceConfig{Name: "backup", Description: "Nightly & safe", Enabled: true, Schedule: "*/5 * * * *"}
	plist := renderDarwinSchedulePlist(config, "com.eyvant.spm.backup", "/usr/local/bin/spm", "/tmp/schedule.json", "/tmp/state", "/tmp/task.log", "timer", false)
	for _, want := range []string{"<key>StartInterval</key>", "<integer>60</integer>", "__scheduled-run", "Nightly &amp; safe", "<string>timer</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("schedule plist does not contain %q:\n%s", want, plist)
		}
	}
}

func TestRenderDarwinDaemonExecutorHasNoPollingInterval(t *testing.T) {
	config := darwinServiceConfig{Name: "backup", Enabled: true, Schedule: "30 0 4 * * *"}
	plist := renderDarwinSchedulePlist(config, "com.eyvant.spm.backup", "/usr/local/bin/spm", "/tmp/schedule.json", "/tmp/state", "/tmp/task.log", "daemon", false)
	if !strings.Contains(plist, "__scheduled-run") || !strings.Contains(plist, "<string>daemon</string>") {
		t.Fatalf("unexpected daemon executor plist:\n%s", plist)
	}
	if strings.Contains(plist, "<key>StartInterval</key>") || strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Fatalf("daemon executor unexpectedly has a native polling trigger:\n%s", plist)
	}
}

func TestRenderDarwinScheduleDaemon(t *testing.T) {
	plist := renderDarwinScheduleDaemonPlist("com.eyvant.spm.scheduler", "/usr/local/bin/spm", "/tmp/spm", "gui/501", "/tmp/scheduler.log")
	for _, want := range []string{"__darwin-schedule-daemon", "<key>RunAtLoad</key>", "<key>KeepAlive</key>", "<string>gui/501</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("scheduler plist does not contain %q:\n%s", want, plist)
		}
	}
}

func TestDarwinScheduledDeclarationIsPortable(t *testing.T) {
	b := &darwinBackend{root: t.TempDir(), plistDir: t.TempDir(), uid: os.Getuid()}
	service := serviceDeclaration{Name: "job", Command: []string{"/usr/bin/true"}, Schedule: "*/5 9-17 1 * 1-5", Timezone: "Asia/Singapore", Persistent: true}
	config, err := b.prepareDeclaration(service)
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != "schedule" || config.Schedule != service.Schedule || config.Timezone != service.Timezone || !config.Persistent {
		t.Fatalf("unexpected schedule config: %#v", config)
	}
}
