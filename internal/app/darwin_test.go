//go:build darwin

package app

import (
	"bytes"
	"io"
	"os"
	"os/exec"
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

func TestDarwinLocalManagementAndReporting(t *testing.T) {
	root := t.TempDir()
	plists := filepath.Join(root, "plists")
	var output bytes.Buffer
	b := &darwinBackend{root: root, plistDir: plists, uid: os.Getuid(), stdout: &output, stderr: io.Discard}
	regular := darwinServiceConfig{Version: darwinConfigVersion, Name: "api", Cwd: root, Command: []string{"/usr/bin/true"}, Restart: "no", RestartSec: "1s", Enabled: true}
	scheduled := darwinServiceConfig{Version: darwinConfigVersion, Name: "backup", Cwd: root, Command: []string{"/usr/bin/true"}, Restart: "no", RestartSec: "1s", Enabled: true, Mode: "schedule", Schedule: "0 * * * *"}
	for _, config := range []darwinServiceConfig{scheduled, regular} {
		if err := b.writeConfig(config); err != nil {
			t.Fatal(err)
		}
	}
	if names, err := b.names(); err != nil || strings.Join(names, ",") != "api,backup" {
		t.Fatalf("names = %v, %v", names, err)
	}
	if err := b.list(listOptions{json: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name": "api"`, `"name": "backup"`, `"schedule": "0 * * * *"`} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("list output does not contain %q: %s", want, output.String())
		}
	}
	output.Reset()
	if err := b.status("backup"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "waiting") || !strings.Contains(output.String(), "0 * * * *") {
		t.Fatalf("schedule status: %s", output.String())
	}
	output.Reset()
	if err := b.status("api"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "launchd") || !strings.Contains(output.String(), "/usr/bin/true") {
		t.Fatalf("service status: %s", output.String())
	}
	if err := os.WriteFile(b.logPath("api"), []byte("first\nmatch this\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := b.logs([]string{"--lines", "2", "--grep", "match", "api"}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "match this") || strings.Contains(got, "last") {
		t.Fatalf("filtered logs: %q", got)
	}
	for _, args := range [][]string{{"list", "--json"}, {"status", "api"}, {"logs", "--lines", "1", "api"}, {"doctor"}} {
		if err := b.run(args); err != nil {
			t.Errorf("run(%v): %v", args, err)
		}
	}
	for _, args := range [][]string{{"status"}, {"native"}, {"unknown"}} {
		if err := b.run(args); err == nil {
			t.Errorf("run(%v) unexpectedly succeeded", args)
		}
	}
	for _, selectors := range [][]string{nil, {"all", "api"}, {"missing"}, {"bad/name"}} {
		if _, err := b.selectNames(selectors); err == nil {
			t.Errorf("selectNames(%v) unexpectedly succeeded", selectors)
		}
	}
	if err := b.removeMany([]string{"all"}); err == nil {
		t.Fatal("remove all without confirmation unexpectedly succeeded")
	}
	output.Reset()
	if err := b.removeMany([]string{"api"}); err != nil {
		t.Fatal(err)
	}
	if b.exists("api") || !strings.Contains(output.String(), "api deleted") {
		t.Fatalf("regular service was not deleted: %s", output.String())
	}
}

func TestDarwinEnvironmentAndRejectedOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.env")
	if err := os.WriteFile(path, []byte("# comment\nA=from-file\nQUOTED='hello world'\nINVALID\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := darwinEnvironment(darwinServiceConfig{EnvFile: path, Env: []string{"A=override", "B=value", "INVALID"}})
	if env["A"] != "override" || env["B"] != "value" || env["QUOTED"] != "hello world" {
		t.Fatalf("environment = %#v", env)
	}
	options := []startOptions{
		{startLimitBurst: "2"}, {cronRestart: "* * * * *"}, {maxMemory: "1G"},
		{execStartPre: []string{"true"}}, {onFailure: []string{"notify"}},
		{health: healthConfig{Probe: probeConfig{Kind: "http"}}}, {serviceType: "forking"},
		{pidFile: "/tmp/pid"}, {timezone: "UTC"}, {instances: 2}, {sandbox: "strict"},
	}
	for _, option := range options {
		if err := rejectDarwinStartOptions(option); err == nil {
			t.Errorf("options %#v unexpectedly accepted", option)
		}
	}
	if err := rejectDarwinStartOptions(startOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinDeclarationPlans(t *testing.T) {
	root := t.TempDir()
	var output bytes.Buffer
	b := &darwinBackend{root: root, plistDir: filepath.Join(root, "plists"), uid: os.Getuid(), stdout: &output, stderr: io.Discard}
	services := []serviceDeclaration{{Name: "api", Command: []string{"/usr/bin/true"}}}
	if err := b.printDeclarationPlan(services, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "create\tapi") {
		t.Fatalf("create plan: %s", output.String())
	}
	desired, err := b.prepareDeclaration(services[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := b.writeConfig(desired); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := b.printDeclarationPlan(services, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "unchanged\tapi") {
		t.Fatalf("unchanged plan: %s", output.String())
	}
	services[0].Description = "changed"
	output.Reset()
	if err := b.printDeclarationPlan(services, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"update\tapi", "-  config =", "+  config ="} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("detailed update plan missing %q: %s", want, output.String())
		}
	}
}

func TestDarwinNativeLifecycleWithCommandBoundary(t *testing.T) {
	original := darwinExecCommand
	darwinExecCommand = func(string, ...string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "printf 'state = running\\npid = 123\\nruns = 1\\n'")
	}
	t.Cleanup(func() { darwinExecCommand = original })
	root := t.TempDir()
	var output bytes.Buffer
	b := &darwinBackend{root: root, plistDir: filepath.Join(root, "plists"), uid: os.Getuid(), stdout: &output, stderr: io.Discard}
	if err := b.start([]string{"--name", "api", "--no-enable", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"restart", "stop", "enable", "disable"} {
		if err := b.controlMany(action, []string{"api"}); err != nil {
			t.Fatalf("%s service: %v", action, err)
		}
	}
	if err := b.native([]string{"launchd", "print", "api"}); err != nil {
		t.Fatal(err)
	}
	if err := b.schedule([]string{"--name", "job", "--interval", "5min", "--on-boot", "1min", "--", "go", "version"}); err != nil {
		t.Fatal(err)
	}
	config, err := b.readConfig("job")
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		if err := b.setScheduledEnabled(config, enabled); err != nil {
			t.Fatal(err)
		}
		config.Enabled = enabled
	}
	if err := b.removeMany([]string{"api", "job"}); err != nil {
		t.Fatal(err)
	}
	if b.exists("api") || b.exists("job") {
		t.Fatal("services survived removal")
	}
}
