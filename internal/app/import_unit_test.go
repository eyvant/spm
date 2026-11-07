package app

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSystemdUnitPreservesSupportedDirectives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.service")
	unit := "[Unit]\nDescription=API\nAfter=network.target\nOnFailure=notify.service\n\n[Service]\nType=exec\nUser=www-data\nGroup=www-data\nWorkingDirectory=/opt/api\nExecStartPre=/usr/bin/test -f config.json\nExecStart=\"/usr/bin/node\" \"server.js\" \"--port\" \"3000\"\nExecStartPost=/usr/bin/logger started\nExecStop=/usr/bin/logger stopping\nExecReload=/bin/kill -HUP $MAINPID\nEnvironment=\"NODE_ENV=production\"\nRestart=on-failure\nRestartSec=5s\nNice=5\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := parseSystemdUnit(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Command, []string{"/usr/bin/node", "server.js", "--port", "3000"}) || config.Cwd != "/opt/api" || config.Restart != "on-failure" {
		t.Fatalf("unexpected imported config: %#v", config)
	}
	if len(config.Properties) != 1 || config.Properties[0] != "Nice=5" {
		t.Fatalf("properties = %v", config.Properties)
	}
	if config.Group != "www-data" || len(config.ExecStartPre) != 1 || config.ExecStop == "" || len(config.OnFailure) != 1 {
		t.Fatalf("structured directives were not preserved: %#v", config)
	}
}

func TestParseSystemdUnitRejectsUnsupportedType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oneshot.service")
	if err := os.WriteFile(path, []byte("[Service]\nType=oneshot\nExecStart=/usr/bin/server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSystemdUnit(path); err == nil {
		t.Fatal("Type=oneshot unexpectedly imported")
	}
}

func TestParseSystemdUnitKeepsNotifyAndForkingTypes(t *testing.T) {
	directory := t.TempDir()
	for _, test := range []struct{ unit, wantType, wantPID string }{
		{"[Service]\nType=notify\nExecStart=/usr/bin/server\n", "notify", ""},
		{"[Service]\nType=forking\nPIDFile=/run/server.pid\nExecStart=/usr/bin/server\n", "forking", "/run/server.pid"},
		{"[Service]\nType=exec\nExecStart=/usr/bin/server\n", "", ""},
	} {
		path := filepath.Join(directory, test.wantType+"x.service")
		if err := os.WriteFile(path, []byte(test.unit), 0o644); err != nil {
			t.Fatal(err)
		}
		config, err := parseSystemdUnit(path)
		if err != nil {
			t.Fatal(err)
		}
		if config.Type != test.wantType || config.PIDFile != test.wantPID {
			t.Errorf("parseSystemdUnit(%q) type=%q pid=%q", test.unit, config.Type, config.PIDFile)
		}
	}
}

func TestImportUnitInstallsAndValidatesArguments(t *testing.T) {
	directory := localTempDir(t)
	sourceDirectory := filepath.Join(directory, "source")
	if err := os.MkdirAll(sourceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sourceDirectory, "spm-imported.service")
	data := []byte("[Unit]\nDescription=Imported service\n[Service]\nType=exec\nExecStart=go version\nWorkingDirectory=" + directory + "\nRestart=always\n[Install]\nWantedBy=default.target\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &application{runner: &fakeRunner{}, stdout: io.Discard, stderr: io.Discard, unitDir: directory, configDir: filepath.Join(directory, "config")}
	if err := a.importUnit([]string{path}); err != nil {
		t.Fatal(err)
	}
	config, err := a.readServiceConfig("imported")
	if err != nil || config.Description != "Imported service" || len(config.Command) != 2 {
		t.Fatalf("imported config = %#v, %v", config, err)
	}
	if err := a.importUnit([]string{path}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate import error = %v", err)
	}
	for _, args := range [][]string{nil, {path, "--bad", "name"}, {path, "--name", "bad/name"}} {
		if err := a.importUnit(args); err == nil {
			t.Errorf("importUnit(%v) unexpectedly succeeded", args)
		}
	}
}
