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
	}
}
