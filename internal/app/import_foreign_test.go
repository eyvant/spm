package app

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func linuxImport() foreignImportOptions { return foreignImportOptions{goos: "linux"} }

func findImported(t *testing.T, services []importedService, name string) importedService {
	t.Helper()
	for _, service := range services {
		if service.Name == name {
			return service
		}
	}
	t.Fatalf("service %q not imported; got %#v", name, services)
	return importedService{}
}

func hasNote(service importedService, fragment string) bool {
	for _, note := range service.notes {
		if strings.Contains(note, fragment) {
			return true
		}
	}
	return false
}

func TestPM2EcosystemJSONConversion(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ecosystem.json")
	data := `{"apps": [
	  {"name": "api", "script": "server.js", "args": "--port 3000 --host '0.0.0.0'", "cwd": "app",
	   "instances": 3, "exec_mode": "cluster", "max_memory_restart": "512MB", "cron_restart": "0 3 * * *",
	   "env": {"NODE_ENV": "development", "PORT": 3000}, "env_production": {"NODE_ENV": "production"},
	   "restart_delay": 1500, "max_restarts": 10, "watch": true, "out_file": "/var/log/api.log"},
	  {"name": "bin", "script": "/usr/local/bin/worker", "interpreter": "none", "args": ["-v"], "autorestart": false},
	  {"name": "inspect", "script": "index.js", "node_args": "--max-old-space-size=4096"},
	  {"name": "api", "script": "other.js", "user": "www"}
	]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	options := linuxImport()
	options.source, options.envName = path, "production"
	services, _, err := loadPM2Services(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 4 {
		t.Fatalf("got %d services", len(services))
	}
	api := findImported(t, services, "api")
	if api.Script != "server.js" || !reflect.DeepEqual(api.Args, []string{"--port", "3000", "--host", "0.0.0.0"}) {
		t.Errorf("api command = %q %q", api.Script, api.Args)
	}
	if api.Cwd != filepath.Join(directory, "app") || api.Instances != 3 || api.MaxMemory != "512M" || api.RestartSec != "1500ms" || api.StartLimitBurst != "10" || api.Restart != "always" {
		t.Errorf("api settings = %#v", api.serviceDeclaration)
	}
	if !reflect.DeepEqual(api.Env, []string{"NODE_ENV=production", "PORT=3000"}) {
		t.Errorf("api env = %v", api.Env)
	}
	if api.CronRestart != "" || !hasNote(api, "cron_restart") || !hasNote(api, "cluster") || !hasNote(api, "watch") || !hasNote(api, "out_file") {
		t.Errorf("api notes = %v", api.notes)
	}
	bin := findImported(t, services, "bin")
	if !reflect.DeepEqual(bin.Command, []string{"/usr/local/bin/worker", "-v"}) || bin.Restart != "no" {
		t.Errorf("bin = %#v", bin.serviceDeclaration)
	}
	inspect := findImported(t, services, "inspect")
	if !reflect.DeepEqual(inspect.Command, []string{"node", "--max-old-space-size=4096", "index.js"}) {
		t.Errorf("inspect command = %v", inspect.Command)
	}
	duplicate := findImported(t, services, "api-2")
	if duplicate.User != "" || !hasNote(duplicate, "--system") {
		t.Errorf("user should need system mode: %#v %v", duplicate.serviceDeclaration, duplicate.notes)
	}

	options.envName = "staging"
	if _, _, err := loadPM2Services(options); err == nil || !strings.Contains(err.Error(), "env_staging") {
		t.Errorf("missing env block error = %v", err)
	}
}

func TestPM2RuntimeStateFiltersInheritedEnvironment(t *testing.T) {
	t.Setenv("SPM_IMPORT_INHERITED", "same")
	jlist := `[
	  {"name": "api", "pm2_env": {"pm_exec_path": "/srv/api/server.js", "pm_cwd": "/srv/api", "exec_interpreter": "node",
	    "args": ["--port", "80"], "status": "online", "instances": 1,
	    "env": {"APP_KEY": "x", "SPM_IMPORT_INHERITED": "same", "PM2_HOME": "/root/.pm2", "pm_id": "0", "worker": "{}", "unique_id": "u"}}},
	  {"name": "worker", "pm2_env": {"pm_exec_path": "/usr/bin/redis-server", "pm_cwd": "/", "exec_interpreter": "node", "status": "stopped", "env": {}}}
	]`
	original := runPM2JList
	t.Cleanup(func() { runPM2JList = original })
	runPM2JList = func() ([]byte, error) { return []byte(jlist), nil }
	services, source, err := loadPM2Services(linuxImport())
	if err != nil {
		t.Fatal(err)
	}
	if source != "pm2 jlist" {
		t.Errorf("source = %q", source)
	}
	api := findImported(t, services, "api")
	if !reflect.DeepEqual(api.Env, []string{"APP_KEY=x"}) || api.Script != "/srv/api/server.js" || api.Cwd != "/srv/api" || !api.Enable {
		t.Errorf("api = %#v", api.serviceDeclaration)
	}
	worker := findImported(t, services, "worker")
	if !reflect.DeepEqual(worker.Command, []string{"/usr/bin/redis-server"}) || worker.Enable {
		t.Errorf("worker = %#v", worker.serviceDeclaration)
	}
}

func TestPM2FallsBackToDumpFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PM2_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "dump.pm2"), []byte(`[{"name":"api","pm_exec_path":"/srv/a.js","pm_cwd":"/srv","env":{}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	original := runPM2JList
	t.Cleanup(func() { runPM2JList = original })
	runPM2JList = func() ([]byte, error) { return nil, os.ErrNotExist }
	services, source, err := loadPM2Services(linuxImport())
	if err != nil || len(services) != 1 || !strings.HasSuffix(source, "dump.pm2") {
		t.Fatalf("services=%v source=%q err=%v", services, source, err)
	}
}

func TestImportAdaptsLinuxOnlySettings(t *testing.T) {
	service := importedService{serviceDeclaration: serviceDeclaration{Name: "api", StartLimitBurst: "5", CronRestart: "0 * * * *", MaxMemory: "1G", Properties: []string{"KillSignal=SIGQUIT"}}}
	adaptToPlatform(&service, "windows")
	if service.StartLimitBurst != "" || service.CronRestart != "" || service.MaxMemory != "" || service.Properties != nil || len(service.notes) != 4 {
		t.Fatalf("windows adaptation = %#v %v", service.serviceDeclaration, service.notes)
	}
	var notes []string
	instanced := importedService{}
	applyInstances(&instanced, 4, "darwin", func(format string, args ...any) { notes = append(notes, format) })
	if instanced.Instances != 0 || len(notes) != 1 {
		t.Fatalf("darwin instances = %d notes=%v", instanced.Instances, notes)
	}
}

func TestSupervisorConversionFollowsIncludes(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPM_IMPORT_QUEUE", "high")
	main := `[supervisord]
logfile=/tmp/supervisord.log

[program:queue]
command=/usr/bin/php artisan queue:work --queue=%(ENV_SPM_IMPORT_QUEUE)s --name=%(program_name)s ; comment
directory=/srv/app
autorestart=true
numprocs=2
process_name=%(program_name)s_%(process_num)02d
environment=APP_ENV="production",
    TOKEN='a b,c'
user=www-data
stopsignal=QUIT
stopwaitsecs=30
startretries=4
stdout_logfile=/var/log/queue.log

[group:all]
programs=queue

[include]
files = conf.d/*.conf
`
	if err := os.WriteFile(filepath.Join(directory, "supervisord.conf"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := "[program:once]\ncommand = bash -c \"echo hi && sleep 1\"\nautostart = false\nautorestart = false\n"
	if err := os.WriteFile(filepath.Join(directory, "conf.d", "once.conf"), []byte(extra), 0o600); err != nil {
		t.Fatal(err)
	}
	options := linuxImport()
	options.source, options.system = filepath.Join(directory, "supervisord.conf"), true
	services, err := loadSupervisorServices(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 {
		t.Fatalf("got %d services: %#v", len(services), services)
	}
	queue := findImported(t, services, "queue")
	if !reflect.DeepEqual(queue.Command, []string{"/usr/bin/php", "artisan", "queue:work", "--queue=high", "--name=queue"}) {
		t.Errorf("queue command = %q", queue.Command)
	}
	if queue.Cwd != "/srv/app" || queue.Restart != "always" || queue.Instances != 2 || queue.User != "www-data" || queue.StartLimitBurst != "5" || !queue.Enable {
		t.Errorf("queue = %#v", queue.serviceDeclaration)
	}
	if !reflect.DeepEqual(queue.Env, []string{"APP_ENV=production", "TOKEN=a b,c"}) {
		t.Errorf("queue env = %q", queue.Env)
	}
	if !reflect.DeepEqual(queue.Properties, []string{"KillSignal=SIGQUIT", "TimeoutStopSec=30"}) || !hasNote(queue, "log settings") {
		t.Errorf("queue properties=%v notes=%v", queue.Properties, queue.notes)
	}
	once := findImported(t, services, "once")
	if !reflect.DeepEqual(once.Command, []string{"bash", "-c", "echo hi && sleep 1"}) || once.Restart != "no" || once.Enable || !once.EnableSet {
		t.Errorf("once = %#v", once.serviceDeclaration)
	}
}

func TestSupervisorDefaultRestartIsOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(path, []byte("[program:app]\ncommand=/bin/app\nexitcodes=0,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := linuxImport()
	options.source = path
	services, err := loadSupervisorServices(options)
	if err != nil || len(services) != 1 || services[0].Restart != "on-failure" || !hasNote(services[0], "exitcodes") {
		t.Fatalf("services=%#v err=%v", services, err)
	}
}

func TestSplitShellWordsAndSupervisorEnvironment(t *testing.T) {
	words, err := splitShellWords(`a "b c" 'd "e"' f\ g "h\"i"`)
	if err != nil || !reflect.DeepEqual(words, []string{"a", "b c", `d "e"`, "f g", `h"i`}) {
		t.Fatalf("words = %q, %v", words, err)
	}
	if _, err := splitShellWords(`"open`); err == nil {
		t.Error("unterminated quote accepted")
	}
	env, err := parseSupervisorEnvironment(`A="1,2", B=plain,C='x y'`)
	if err != nil || !reflect.DeepEqual(env, []string{"A=1,2", "B=plain", "C=x y"}) {
		t.Fatalf("env = %q, %v", env, err)
	}
	if _, err := parseSupervisorEnvironment(`=bad`); err == nil {
		t.Error("malformed environment accepted")
	}
}

func TestImportPM2WritesValidDeclarations(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "server.js"), []byte("// server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, "ecosystem.config.js")
	if err := os.WriteFile(config, []byte("module.exports = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := loadJSConfig
	t.Cleanup(func() { loadJSConfig = original })
	loadJSConfig = func(path string) ([]byte, error) {
		if path != config {
			t.Errorf("node loaded %q", path)
		}
		return []byte(`{"apps":[{"name":"api","script":"server.js","env":{"A":"1"}}]}`), nil
	}
	output := filepath.Join(directory, "spm.toml")
	if err := Run([]string{"import", "pm2", config, "-o", output}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	services, err := parseServiceDeclarations(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].Name != "api" || services[0].Script != "server.js" || services[0].Cwd != directory || !reflect.DeepEqual(services[0].Env, []string{"A=1"}) {
		t.Fatalf("declarations = %#v", services)
	}
	if err := Run([]string{"import", "pm2", config, "-o", output}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("overwrite error = %v", err)
	}
	var stdout strings.Builder
	if err := Run([]string{"import", "pm2", config}, &stdout, io.Discard); err != nil || !strings.Contains(stdout.String(), `name = "api"`) {
		t.Fatalf("stdout import = %q, %v", stdout.String(), err)
	}
}

func TestForeignImportArgumentErrors(t *testing.T) {
	for _, args := range [][]string{
		{"import", "pm2", "a.json", "b.json"},
		{"import", "pm2", "a.json", "--bogus"},
		{"import", "pm2", "a.json", "-o"},
		{"import", "supervisor", "a.conf", "--env", "prod"},
		{"import", "pm2", "a.json", "-o", "a.json"},
	} {
		if err := Run(args, io.Discard, io.Discard); err == nil {
			t.Errorf("Run(%v) unexpectedly succeeded", args)
		}
	}
}

func TestImportCompletionOffersSources(t *testing.T) {
	a := &application{}
	if got := a.completionCandidates([]string{"import", "p"}); !reflect.DeepEqual(got, []string{"pm2"}) {
		t.Errorf("completion = %v", got)
	}
}

func stubSupervisorSearch(t *testing.T, executable, systemDir string) {
	t.Helper()
	originalLook, originalSystem := lookSupervisord, supervisorSystemDir
	t.Cleanup(func() { lookSupervisord, supervisorSystemDir = originalLook, originalSystem })
	lookSupervisord = func() (string, error) {
		if executable == "" {
			return "", os.ErrNotExist
		}
		return executable, nil
	}
	supervisorSystemDir = func() string { return systemDir }
}

func writeSupervisorProgram(t *testing.T, path, program string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[program:"+program+"]\ncommand=/bin/"+program+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindSupervisorConfigFollowsSupervisordOrder(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "prefix")
	work := filepath.Join(root, "work")
	system := filepath.Join(root, "etc")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	stubSupervisorSearch(t, filepath.Join(prefix, "bin", "supervisord"), system)

	files := []string{
		filepath.Join(prefix, "etc", "supervisord.conf"),
		filepath.Join(prefix, "supervisord.conf"),
		filepath.Join(work, "supervisord.conf"),
		filepath.Join(work, "etc", "supervisord.conf"),
		filepath.Join(system, "supervisord.conf"),
		filepath.Join(system, "supervisor", "supervisord.conf"),
	}
	for _, file := range files {
		writeSupervisorProgram(t, file, "app")
	}
	for _, want := range files {
		found, searched := findSupervisorConfig()
		if found != want {
			t.Fatalf("found %q, want %q (searched %v)", found, want, searched)
		}
		if err := os.Remove(want); err != nil {
			t.Fatal(err)
		}
	}
	found, searched := findSupervisorConfig()
	if found != "" || !reflect.DeepEqual(searched, files) {
		t.Fatalf("found %q, searched %v", found, searched)
	}
}

func TestImportSupervisorWithoutPathUsesDiscoveredConfig(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	stubSupervisorSearch(t, "", filepath.Join(root, "etc"))
	var stdout, stderr strings.Builder
	err := Run([]string{"import", "supervisor"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "no supervisord.conf found") || !strings.Contains(err.Error(), filepath.Join(root, "etc", "supervisor", "supervisord.conf")) {
		t.Fatalf("missing config error = %v", err)
	}

	config := filepath.Join(root, "etc", "supervisor", "supervisord.conf")
	writeSupervisorProgram(t, config, "worker")
	stdout.Reset()
	if err := Run([]string{"import", "supervisor"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Using supervisor configuration "+config) || !strings.Contains(stdout.String(), `name = "worker"`) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
