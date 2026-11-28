package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// importedService is one service converted from another process manager,
// together with the settings spm could not carry over.
type importedService struct {
	serviceDeclaration
	notes []string
}

type foreignImportOptions struct {
	source  string
	output  string
	envName string
	force   bool
	system  bool
	goos    string
}

// Hooks that reach outside the process, replaced in tests.
var (
	runPM2JList = func() ([]byte, error) {
		return exec.Command("pm2", "jlist").Output()
	}
	loadJSConfig = func(path string) ([]byte, error) {
		const script = `const p=require('path').resolve(process.argv[1]);` +
			`import(require('url').pathToFileURL(p).href).then(m=>{let c=m.default!==undefined?m.default:m;` +
			`if(c&&c.default!==undefined&&c.apps===undefined)c=c.default;process.stdout.write(JSON.stringify(c))})` +
			`.catch(e=>{console.error(e&&e.message||e);process.exit(1)})`
		command := exec.Command("node", "-e", script, path)
		command.Stderr = os.Stderr
		return command.Output()
	}
)

func isForeignImport(args []string) bool {
	return len(args) >= 2 && args[0] == "import" && (args[1] == "pm2" || args[1] == "supervisor" || args[1] == "supervisord")
}

// importForeign converts a pm2 or supervisor configuration into spm.toml
// declarations. It never installs anything: the result is meant to be
// reviewed and then loaded with `spm apply`.
func importForeign(args []string, stdout, stderr io.Writer, system bool) error {
	kind := args[0]
	options, err := parseForeignImportArgs(kind, args[1:])
	if err != nil {
		return err
	}
	options.system = system
	options.goos = runtime.GOOS
	var services []importedService
	var source string
	if kind == "pm2" {
		services, source, err = loadPM2Services(options)
	} else {
		if options.source == "" {
			found, searched := findSupervisorConfig()
			if found == "" {
				return fmt.Errorf("no supervisord.conf found; pass its path. Searched:\n  %s", strings.Join(searched, "\n  "))
			}
			fmt.Fprintf(stderr, "Using supervisor configuration %s\n", found)
			options.source = found
		}
		source = options.source
		services, err = loadSupervisorServices(options)
	}
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return fmt.Errorf("no %s programs found in %s", kind, source)
	}
	document := renderImportedServices(kind, source, services)
	if err := validateImportedDocument(document, options.goos); err != nil {
		return fmt.Errorf("converted declarations are invalid: %w", err)
	}
	if options.output == "" {
		_, err = io.WriteString(stdout, document)
	} else {
		if _, statErr := os.Stat(options.output); statErr == nil && !options.force {
			return fmt.Errorf("%s already exists; pass --force to overwrite it", options.output)
		}
		if err = os.WriteFile(options.output, []byte(document), 0o600); err == nil {
			fmt.Fprintf(stderr, "Wrote %d service(s) to %s\n", len(services), options.output)
		}
	}
	if err != nil {
		return err
	}
	noted := 0
	for _, service := range services {
		if len(service.notes) > 0 {
			noted++
		}
	}
	if noted > 0 {
		fmt.Fprintf(stderr, "%d service(s) have settings spm did not carry over; see the # comments.\n", noted)
	}
	target := options.output
	if target == "" {
		target = "spm.toml"
	}
	fmt.Fprintf(stderr, "Review the declarations, stop the %s processes, then run: spm apply %s\n", kind, target)
	return nil
}

func parseForeignImportArgs(kind string, args []string) (foreignImportOptions, error) {
	var options foreignImportOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case arg == "-o" || arg == "--output":
			options.output, err = value()
		case strings.HasPrefix(arg, "--output="):
			options.output = strings.TrimPrefix(arg, "--output=")
		case arg == "--env" && kind == "pm2":
			options.envName, err = value()
		case strings.HasPrefix(arg, "--env=") && kind == "pm2":
			options.envName = strings.TrimPrefix(arg, "--env=")
		case arg == "--force":
			options.force = true
		case strings.HasPrefix(arg, "-"):
			return options, fmt.Errorf("unknown import %s option %q", kind, arg)
		default:
			if options.source != "" {
				return options, fmt.Errorf("import %s accepts one configuration path", kind)
			}
			options.source = arg
		}
		if err != nil {
			return options, err
		}
	}
	if options.output != "" && options.output == options.source {
		return options, errors.New("--output must differ from the source configuration")
	}
	return options, nil
}

// ---------------------------------------------------------------------------
// pm2

func loadPM2Services(options foreignImportOptions) ([]importedService, string, error) {
	var data []byte
	var err error
	source := options.source
	switch {
	case source == "":
		data, source, err = livePM2Processes()
	case strings.HasSuffix(source, ".js") || strings.HasSuffix(source, ".cjs") || strings.HasSuffix(source, ".mjs"):
		data, err = loadJSConfig(source)
		if err != nil {
			err = fmt.Errorf("load %s with node (node must be on PATH; or convert it to JSON first): %w", source, err)
		}
	default:
		data, err = os.ReadFile(source)
	}
	if err != nil {
		return nil, source, err
	}
	apps, err := decodePM2Apps(data)
	if err != nil {
		return nil, source, fmt.Errorf("%s: %w", source, err)
	}
	baseDir := ""
	if options.source != "" {
		if absolute, absErr := filepath.Abs(filepath.Dir(options.source)); absErr == nil {
			baseDir = absolute
		}
	}
	appNames := map[string]bool{}
	for _, app := range apps {
		if name, ok := app["name"].(string); ok {
			appNames[name] = true
		}
	}
	var services []importedService
	used := map[string]bool{}
	envFound := false
	for _, app := range apps {
		if _, ok := app["env_"+options.envName]; ok || app["__runtime"] == true {
			envFound = true
		}
	}
	for i, app := range apps {
		service, err := convertPM2App(app, baseDir, appNames, options)
		if err != nil {
			return nil, source, fmt.Errorf("app %d: %w", i+1, err)
		}
		adaptToPlatform(&service, options.goos)
		service.Name = uniqueName(service.Name, used)
		services = append(services, service)
	}
	if options.envName != "" && !envFound {
		return nil, source, fmt.Errorf("no app defines an env_%s block", options.envName)
	}
	return services, source, nil
}

func livePM2Processes() ([]byte, string, error) {
	data, err := runPM2JList()
	if err == nil {
		return data, "pm2 jlist", nil
	}
	home := os.Getenv("PM2_HOME")
	if home == "" {
		userHome, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, "", fmt.Errorf("run pm2 jlist: %w", err)
		}
		home = filepath.Join(userHome, ".pm2")
	}
	dump := filepath.Join(home, "dump.pm2")
	dumpData, dumpErr := os.ReadFile(dump)
	if dumpErr != nil {
		return nil, "", fmt.Errorf("run pm2 jlist: %v; read %s: %v (pass an ecosystem file instead)", err, dump, dumpErr)
	}
	return dumpData, dump, nil
}

// decodePM2Apps accepts an ecosystem object ({"apps": [...]}), a single app
// object, or an array as produced by `pm2 jlist` and ~/.pm2/dump.pm2.
func decodePM2Apps(data []byte) ([]map[string]any, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse pm2 configuration JSON: %w", err)
	}
	var list []any
	switch value := raw.(type) {
	case []any:
		list = value
	case map[string]any:
		if apps, ok := value["apps"]; ok {
			switch apps := apps.(type) {
			case []any:
				list = apps
			case map[string]any:
				list = []any{apps}
			default:
				return nil, errors.New("apps must be an array")
			}
		} else {
			list = []any{value}
		}
	default:
		return nil, errors.New("expected an ecosystem object or an array of apps")
	}
	apps := make([]map[string]any, 0, len(list))
	for _, item := range list {
		app, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("every app must be an object")
		}
		// pm2 jlist nests the definition under pm2_env.
		if env, ok := app["pm2_env"].(map[string]any); ok {
			merged := map[string]any{}
			for key, value := range env {
				merged[key] = value
			}
			if name, ok := app["name"]; ok {
				merged["name"] = name
			}
			merged["__runtime"] = true
			app = merged
		} else if _, ok := app["pm_exec_path"]; ok {
			app["__runtime"] = true
		}
		apps = append(apps, app)
	}
	return apps, nil
}

func convertPM2App(app map[string]any, baseDir string, appNames map[string]bool, options foreignImportOptions) (importedService, error) {
	var service importedService
	note := func(format string, args ...any) { service.notes = append(service.notes, fmt.Sprintf(format, args...)) }
	runtimeState := app["__runtime"] == true

	script := firstString(app, "script", "pm_exec_path")
	if script == "" {
		return service, errors.New("app has no script")
	}
	name := sanitizeName(firstString(app, "name"))
	if name == "" {
		name = sanitizeName(strings.TrimSuffix(filepath.Base(script), filepath.Ext(script)))
	}
	if name == "" || name == "all" {
		name = "app"
	}
	service.Name = name
	service.Description = "Imported from pm2 app " + strconv.Quote(firstString(app, "name"))

	cwd := firstString(app, "cwd", "pm_cwd")
	if cwd != "" && !filepath.IsAbs(cwd) && baseDir != "" {
		cwd = filepath.Join(baseDir, cwd)
	}
	if cwd == "" {
		cwd = baseDir
	}
	service.Cwd = cwd

	args, err := stringList(app["args"])
	if err != nil {
		return service, fmt.Errorf("args: %w", err)
	}
	interpreterArgs, err := stringList(firstValue(app, "interpreter_args", "node_args"))
	if err != nil {
		return service, fmt.Errorf("interpreter_args: %w", err)
	}
	interpreter := firstString(app, "interpreter", "exec_interpreter")
	switch {
	case interpreter == "none":
		service.Command = append([]string{script}, args...)
	case len(interpreterArgs) > 0:
		if interpreter == "" {
			interpreter = pm2DefaultInterpreter(script)
		}
		service.Command = append(append(append([]string{interpreter}, interpreterArgs...), script), args...)
	case runtimeState && interpreter == "node" && !isJavaScript(script):
		// pm2 records "node" for binaries it started directly.
		service.Command = append([]string{script}, args...)
	default:
		service.Script = script
		service.Args = args
		if interpreter != "" && interpreter != pm2DefaultInterpreter(script) {
			service.Interpreter = interpreter
		}
	}

	if autorestart, ok := app["autorestart"].(bool); ok && !autorestart {
		service.Restart = "no"
	} else {
		service.Restart = "always"
	}
	if delay, ok := numberValue(app["restart_delay"]); ok && delay > 0 {
		service.RestartSec = strconv.FormatInt(int64(delay), 10) + "ms"
	}
	if _, ok := app["exp_backoff_restart_delay"]; ok {
		note("exp_backoff_restart_delay is not supported; restart_sec is a fixed delay")
	}
	if restarts, ok := numberValue(app["max_restarts"]); ok && restarts > 0 {
		service.StartLimitBurst = strconv.Itoa(int(restarts))
		note("max_restarts mapped to start_limit_burst; check start_limit_interval")
	}

	if memory := firstString(app, "max_memory_restart"); memory != "" {
		if size := normalizePM2Memory(memory); size != "" {
			service.MaxMemory = size
		} else {
			note("max_memory_restart %q is not a size spm understands", memory)
		}
	} else if memory, ok := numberValue(app["max_memory_restart"]); ok && memory > 0 {
		service.MaxMemory = strconv.FormatInt(int64(memory), 10)
	}

	instances := 1
	if value, ok := numberValue(app["instances"]); ok {
		instances = int(value)
	} else if value := firstString(app, "instances"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			instances = parsed
		} else {
			instances = runtime.NumCPU()
			note("instances %q was resolved to %d CPUs on this machine", value, instances)
		}
	}
	if instances <= 0 {
		instances = runtime.NumCPU() + instances
		if instances <= 0 {
			instances = 1
		}
		note("instances was resolved to %d on this machine", instances)
	}
	if mode := firstString(app, "exec_mode"); strings.Contains(mode, "cluster") {
		note("cluster mode has no spm equivalent; instances run as separate processes and must not share a port unless the app uses SO_REUSEPORT")
	}

	if cron := firstString(app, "cron_restart", "cron"); cron != "" {
		service.CronRestart = cron
	}
	applyInstances(&service, instances, options.goos, note)

	env, err := pm2Environment(app, options.envName, runtimeState, appNames)
	if err != nil {
		return service, err
	}
	service.Env = env

	if user := firstString(app, "user", "uid", "username"); user != "" && !runtimeState {
		if options.system {
			service.User = user
		} else {
			note("user %q requires a system service (spm --system apply)", user)
		}
	}
	if group := firstString(app, "gid"); group != "" && !runtimeState {
		if options.system {
			service.Group = group
		} else {
			note("group %q requires a system service (spm --system apply)", group)
		}
	}
	if watch, ok := app["watch"]; ok && watch != false && watch != nil {
		note("watch mode is not supported; restart the service after deploys instead")
	}
	for _, key := range []string{"out_file", "error_file", "log_file", "output", "error", "log"} {
		if value := firstString(app, key); value != "" && !runtimeState {
			note("%s is ignored; spm keeps logs itself (spm logs %s)", key, service.Name)
			break
		}
	}
	if status := firstString(app, "status"); runtimeState && status != "" && status != "online" && status != "launching" {
		service.Enable = false
		service.EnableSet = true
		note("pm2 status was %q, so the service is imported disabled", status)
	}
	service.Enable = service.Enable || !service.EnableSet
	return service, nil
}

// adaptToPlatform drops settings the current backend rejects, so the
// generated file applies cleanly, and records what was dropped.
func adaptToPlatform(service *importedService, goos string) {
	if goos == "linux" {
		return
	}
	note := func(format string, args ...any) { service.notes = append(service.notes, fmt.Sprintf(format, args...)) }
	if service.StartLimitBurst != "" {
		note("start_limit_burst %s dropped: start limits are supported only on Linux", service.StartLimitBurst)
		service.StartLimitBurst = ""
	}
	if service.CronRestart != "" {
		note("cron_restart %q dropped: supported only on Linux; use spm schedule to restart it instead", service.CronRestart)
		service.CronRestart = ""
	}
	if service.MaxMemory != "" {
		note("max_memory %s dropped: memory limits are supported only on Linux", service.MaxMemory)
		service.MaxMemory = ""
	}
	if len(service.Properties) > 0 {
		note("%s dropped: systemd properties are supported only on Linux", strings.Join(service.Properties, ", "))
		service.Properties = nil
	}
}

func applyInstances(service *importedService, instances int, goos string, note func(string, ...any)) {
	if instances <= 1 {
		return
	}
	if goos != "linux" {
		note("%d instances requested, but instances are supported only on Linux; importing one", instances)
		return
	}
	if instances > maxInstances {
		note("instances capped at %d", maxInstances)
		instances = maxInstances
	}
	if service.CronRestart != "" {
		note("cron_restart %q dropped because spm cannot combine it with instances", service.CronRestart)
		service.CronRestart = ""
	}
	service.Instances = instances
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// pm2Environment merges env with env_<name>. Runtime state from pm2 also
// records the whole shell environment pm2 inherited, so values that match
// the current environment and pm2's own bookkeeping are dropped.
func pm2Environment(app map[string]any, envName string, runtimeState bool, appNames map[string]bool) ([]string, error) {
	values := map[string]string{}
	merge := func(raw any, key string) error {
		if raw == nil {
			return nil
		}
		object, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", key)
		}
		for name, value := range object {
			switch value := value.(type) {
			case string:
				values[name] = value
			case float64:
				values[name] = strconv.FormatFloat(value, 'f', -1, 64)
			case bool:
				values[name] = strconv.FormatBool(value)
			case nil:
			default:
				encoded, _ := json.Marshal(value)
				values[name] = string(encoded)
			}
		}
		return nil
	}
	if err := merge(app["env"], "env"); err != nil {
		return nil, err
	}
	if envName != "" {
		key := "env_" + envName
		if err := merge(app[key], key); err != nil {
			return nil, err
		}
	}
	var result []string
	for name, value := range values {
		if !envNamePattern.MatchString(name) {
			continue
		}
		if runtimeState {
			if appNames[name] || strings.HasPrefix(name, "PM2_") || strings.HasPrefix(name, "pm_") || name == "NODE_APP_INSTANCE" || name == "unique_id" {
				continue
			}
			if current, ok := os.LookupEnv(name); ok && current == value {
				continue
			}
		}
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result, nil
}

func pm2DefaultInterpreter(script string) string {
	switch strings.ToLower(filepath.Ext(script)) {
	case ".js", ".cjs", ".mjs", ".ts":
		return "node"
	case ".py":
		return "python3"
	case ".rb":
		return "ruby"
	case ".sh":
		return "bash"
	case ".php":
		return "php"
	case ".pl":
		return "perl"
	}
	return ""
}

func isJavaScript(script string) bool { return pm2DefaultInterpreter(script) == "node" }

func normalizePM2Memory(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, "B")
	if value == "" {
		return ""
	}
	if validSystemdSize(value) {
		return value
	}
	return ""
}

// ---------------------------------------------------------------------------
// supervisor

var (
	lookSupervisord     = func() (string, error) { return exec.LookPath("supervisord") }
	supervisorSystemDir = func() string { return string(filepath.Separator) + "etc" }
)

// findSupervisorConfig searches the locations supervisord itself checks
// when started without -c, in the same order, and returns the first file
// found together with every path it looked at.
func findSupervisorConfig() (string, []string) {
	var candidates []string
	if executable, err := lookSupervisord(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		parent := filepath.Dir(filepath.Dir(executable))
		candidates = append(candidates, filepath.Join(parent, "etc", "supervisord.conf"), filepath.Join(parent, "supervisord.conf"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "supervisord.conf"), filepath.Join(cwd, "etc", "supervisord.conf"))
	}
	etc := supervisorSystemDir()
	candidates = append(candidates, filepath.Join(etc, "supervisord.conf"), filepath.Join(etc, "supervisor", "supervisord.conf"))
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, candidates
		}
	}
	return "", candidates
}

type iniSection struct {
	name   string
	values map[string]string
	order  []string
	file   string
}

func loadSupervisorServices(options foreignImportOptions) ([]importedService, error) {
	sections, err := readSupervisorConfig(options.source, map[string]bool{})
	if err != nil {
		return nil, err
	}
	var services []importedService
	used := map[string]bool{}
	for _, section := range sections {
		kind, program, ok := strings.Cut(section.name, ":")
		if !ok || kind != "program" {
			continue
		}
		service, err := convertSupervisorProgram(program, section, options)
		if err != nil {
			return nil, fmt.Errorf("%s [program:%s]: %w", section.file, program, err)
		}
		adaptToPlatform(&service, options.goos)
		service.Name = uniqueName(service.Name, used)
		services = append(services, service)
	}
	return services, nil
}

func readSupervisorConfig(path string, seen map[string]bool) ([]iniSection, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if seen[absolute] {
		return nil, nil
	}
	seen[absolute] = true
	data, err := os.ReadFile(absolute)
	if err != nil {
		return nil, err
	}
	sections, err := parseINI(string(data), absolute)
	if err != nil {
		return nil, err
	}
	var result []iniSection
	for _, section := range sections {
		if section.name != "include" {
			result = append(result, section)
			continue
		}
		here := filepath.Dir(absolute)
		for _, pattern := range strings.Fields(section.values["files"]) {
			pattern = strings.ReplaceAll(pattern, "%(here)s", here)
			if !filepath.IsAbs(pattern) {
				pattern = filepath.Join(here, pattern)
			}
			matches, err := filepath.Glob(pattern)
			if err != nil {
				return nil, fmt.Errorf("%s: include %q: %w", absolute, pattern, err)
			}
			sort.Strings(matches)
			for _, match := range matches {
				included, err := readSupervisorConfig(match, seen)
				if err != nil {
					return nil, err
				}
				result = append(result, included...)
			}
		}
	}
	return result, nil
}

// parseINI reads the Python configparser dialect supervisor uses: "key = value"
// or "key: value", indented continuation lines, and ";"/"#" comments.
func parseINI(text, file string) ([]iniSection, error) {
	var sections []iniSection
	var current *iniSection
	lastKey := ""
	for number, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			if trimmed == "" {
				lastKey = ""
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if current == nil || lastKey == "" {
				return nil, fmt.Errorf("%s:%d: continuation line without a key", file, number+1)
			}
			current.values[lastKey] += "\n" + stripINIComment(trimmed)
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			if !strings.HasSuffix(trimmed, "]") {
				return nil, fmt.Errorf("%s:%d: malformed section header", file, number+1)
			}
			sections = append(sections, iniSection{name: strings.TrimSpace(trimmed[1 : len(trimmed)-1]), values: map[string]string{}, file: file})
			current = &sections[len(sections)-1]
			lastKey = ""
			continue
		}
		if current == nil {
			return nil, fmt.Errorf("%s:%d: setting outside a section", file, number+1)
		}
		separator := strings.IndexAny(trimmed, "=:")
		if separator <= 0 {
			return nil, fmt.Errorf("%s:%d: expected key = value", file, number+1)
		}
		key := strings.ToLower(strings.TrimSpace(trimmed[:separator]))
		if _, exists := current.values[key]; !exists {
			current.order = append(current.order, key)
		}
		current.values[key] = stripINIComment(strings.TrimSpace(trimmed[separator+1:]))
		lastKey = key
	}
	return sections, nil
}

func stripINIComment(value string) string {
	for i := 1; i < len(value); i++ {
		if value[i] == ';' && (value[i-1] == ' ' || value[i-1] == '\t') {
			return strings.TrimSpace(value[:i])
		}
	}
	return value
}

var supervisorExpansion = regexp.MustCompile(`%\(([A-Za-z0-9_]+)\)[-#0 +]*[0-9]*[sd]`)

func convertSupervisorProgram(program string, section iniSection, options foreignImportOptions) (importedService, error) {
	var service importedService
	note := func(format string, args ...any) { service.notes = append(service.notes, fmt.Sprintf(format, args...)) }
	here := filepath.Dir(section.file)
	expand := func(value string) string {
		return supervisorExpansion.ReplaceAllStringFunc(value, func(match string) string {
			key := supervisorExpansion.FindStringSubmatch(match)[1]
			switch {
			case key == "program_name" || key == "group_name":
				return program
			case key == "process_num":
				return "0"
			case key == "numprocs":
				return section.values["numprocs"]
			case key == "here":
				return here
			case key == "host_node_name":
				host, _ := os.Hostname()
				return host
			case strings.HasPrefix(key, "ENV_"):
				return os.Getenv(strings.TrimPrefix(key, "ENV_"))
			}
			note("could not expand %s", match)
			return match
		})
	}
	value := func(key string) string { return expand(strings.TrimSpace(section.values[key])) }

	command := value("command")
	if command == "" {
		return service, errors.New("program has no command")
	}
	words, err := splitShellWords(strings.ReplaceAll(command, "\n", " "))
	if err != nil {
		return service, fmt.Errorf("command: %w", err)
	}
	service.Command = words
	service.Name = sanitizeName(program)
	if service.Name == "" || service.Name == "all" {
		service.Name = "program"
	}
	service.Description = "Imported from supervisor program " + strconv.Quote(program)
	if directory := value("directory"); directory != "" {
		service.Cwd = directory
	} else {
		service.Cwd = "/"
		if options.goos == "windows" {
			service.Cwd = here
		}
		note("no directory was set; supervisor runs programs from its own working directory")
	}

	switch strings.ToLower(value("autorestart")) {
	case "", "unexpected":
		service.Restart = "on-failure"
		if codes := value("exitcodes"); codes != "" && codes != "0" {
			note("exitcodes=%s is not carried over; only exit code 0 counts as expected", codes)
		}
	case "true":
		service.Restart = "always"
	case "false":
		service.Restart = "no"
	default:
		note("autorestart=%s is not recognised; using on-failure", value("autorestart"))
		service.Restart = "on-failure"
	}
	if retries := value("startretries"); retries != "" {
		if count, err := strconv.Atoi(retries); err == nil && count > 0 {
			service.StartLimitBurst = strconv.Itoa(count + 1)
		}
	}
	switch strings.ToLower(value("autostart")) {
	case "false", "no", "off", "0":
		service.Enable = false
		service.EnableSet = true
	default:
		service.Enable = true
	}

	env, err := parseSupervisorEnvironment(value("environment"))
	if err != nil {
		return service, fmt.Errorf("environment: %w", err)
	}
	service.Env = env

	if user := value("user"); user != "" {
		if options.system {
			service.User = user
		} else {
			note("user %q requires a system service (spm --system apply)", user)
		}
	}
	if processes := value("numprocs"); processes != "" {
		count, err := strconv.Atoi(processes)
		if err != nil || count < 1 {
			return service, fmt.Errorf("invalid numprocs %q", processes)
		}
		if count > 1 && strings.Contains(section.values["command"], "%(process_num)") {
			note("the command uses %%(process_num)s; spm instances expose the index as SPM_INSTANCE instead, so review the command")
		}
		applyInstances(&service, count, options.goos, note)
	}
	if signal := value("stopsignal"); signal != "" && !strings.EqualFold(signal, "TERM") {
		service.Properties = append(service.Properties, "KillSignal=SIG"+strings.TrimPrefix(strings.ToUpper(signal), "SIG"))
	}
	if wait := value("stopwaitsecs"); wait != "" {
		if _, err := strconv.Atoi(wait); err == nil {
			service.Properties = append(service.Properties, "TimeoutStopSec="+wait)
		}
	}
	for _, key := range []string{"stdout_logfile", "stderr_logfile", "redirect_stderr"} {
		if section.values[key] != "" {
			note("log settings are ignored; spm keeps logs itself (spm logs %s)", service.Name)
			break
		}
	}
	for _, key := range []string{"umask", "priority", "startsecs", "stopasgroup", "killasgroup", "serverurl"} {
		if section.values[key] != "" {
			note("%s is not carried over", key)
		}
	}
	return service, nil
}

// parseSupervisorEnvironment parses KEY="value",KEY2=value2.
func parseSupervisorEnvironment(value string) ([]string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if value == "" {
		return nil, nil
	}
	var result []string
	var current strings.Builder
	var quote byte
	flush := func() error {
		pair := strings.TrimSpace(current.String())
		current.Reset()
		if pair == "" {
			return nil
		}
		key, raw, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || !envNamePattern.MatchString(key) {
			return fmt.Errorf("malformed entry %q", pair)
		}
		words, err := splitShellWords(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		result = append(result, key+"="+strings.Join(words, " "))
		return nil
	}
	for i := 0; i < len(value); i++ {
		char := value[i]
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
			}
		case char == '"' || char == '\'':
			quote = char
		case char == ',':
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		current.WriteByte(char)
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// shared helpers

// splitShellWords splits like POSIX shlex: quotes group words, backslash
// escapes outside single quotes. No expansion is performed.
func splitShellWords(value string) ([]string, error) {
	var words []string
	var current strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(value); i++ {
		char := value[i]
		switch {
		case quote == '\'':
			if char == '\'' {
				quote = 0
			} else {
				current.WriteByte(char)
			}
		case quote == '"':
			if char == '"' {
				quote = 0
			} else if char == '\\' && i+1 < len(value) && strings.IndexByte("\"\\$`", value[i+1]) >= 0 {
				i++
				current.WriteByte(value[i])
			} else {
				current.WriteByte(char)
			}
		case char == '\'' || char == '"':
			quote = char
			inWord = true
		case char == '\\' && i+1 < len(value):
			i++
			current.WriteByte(value[i])
			inWord = true
		case char == ' ' || char == '\t' || char == '\n':
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteByte(char)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		words = append(words, current.String())
	}
	return words, nil
}

func stringList(value any) ([]string, error) {
	switch value := value.(type) {
	case nil:
		return nil, nil
	case string:
		return splitShellWords(value)
	case []any:
		result := make([]string, 0, len(value))
		for _, item := range value {
			switch item := item.(type) {
			case string:
				result = append(result, item)
			case float64:
				result = append(result, strconv.FormatFloat(item, 'f', -1, 64))
			case bool:
				result = append(result, strconv.FormatBool(item))
			default:
				return nil, errors.New("must contain only strings")
			}
		}
		return result, nil
	}
	return nil, errors.New("must be a string or an array of strings")
}

func firstValue(app map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := app[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func firstString(app map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := app[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func numberValue(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok
}

func uniqueName(name string, used map[string]bool) string {
	candidate := name
	for i := 2; used[candidate]; i++ {
		candidate = name + "-" + strconv.Itoa(i)
	}
	used[candidate] = true
	return candidate
}

func renderImportedServices(kind, source string, services []importedService) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Converted from %s (%s) by spm import %s.\n", kind, source, kind)
	builder.WriteString("# Review it, stop the original processes, then run: spm apply <this file>\n")
	builder.WriteString("schema = 1\n\n")
	for _, service := range services {
		for _, note := range service.notes {
			builder.WriteString("# NOTE: " + strings.ReplaceAll(note, "\n", " ") + "\n")
		}
		builder.WriteString("[[services]]\nname = " + strconv.Quote(service.Name) + "\n")
		writeTOMLString(&builder, "description", service.Description)
		writeTOMLString(&builder, "cwd", service.Cwd)
		if service.Script != "" {
			writeTOMLString(&builder, "script", service.Script)
			if len(service.Args) > 0 {
				builder.WriteString("args = " + stringArray(service.Args) + "\n")
			}
			writeTOMLString(&builder, "interpreter", service.Interpreter)
		} else {
			builder.WriteString("command = " + stringArray(service.Command) + "\n")
		}
		writeTOMLString(&builder, "restart", service.Restart)
		writeTOMLString(&builder, "restart_sec", service.RestartSec)
		writeTOMLString(&builder, "start_limit_burst", service.StartLimitBurst)
		writeTOMLString(&builder, "cron_restart", service.CronRestart)
		writeTOMLString(&builder, "max_memory", service.MaxMemory)
		writeTOMLString(&builder, "user", service.User)
		writeTOMLString(&builder, "group", service.Group)
		if service.Instances > 0 {
			fmt.Fprintf(&builder, "instances = %d\n", service.Instances)
		}
		if len(service.Env) > 0 {
			builder.WriteString("env = " + stringArray(service.Env) + "\n")
		}
		if len(service.Properties) > 0 {
			builder.WriteString("properties = " + stringArray(service.Properties) + "\n")
		}
		fmt.Fprintf(&builder, "enable = %t\n\n", service.Enable)
	}
	return builder.String()
}

func validateImportedDocument(document, goos string) error {
	file, err := os.CreateTemp("", "spm-import-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(document); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	_, err = parseServiceDeclarationsForPlatform(file.Name(), goos)
	return err
}
