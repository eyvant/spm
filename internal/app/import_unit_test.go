package app

import (
	"os"
	"path/filepath"
	"reflect"
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
