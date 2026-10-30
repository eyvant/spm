package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

type runner interface {
	Run(name string, args ...string) ([]byte, error)
	Interactive(stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return output, commandError(message, err)
		}
	}
	return output, err
}

func (execRunner) Interactive(stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	return interactiveCommand(stdin, stdout, stderr, name, args...).Run()
}

func interactiveCommand(stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd
}

func commandError(message string, err error) error {
	if strings.Contains(message, "Failed to connect to bus") || strings.Contains(message, "XDG_RUNTIME_DIR") {
		return fmt.Errorf("%s: %w; ensure the selected systemd manager is running; in user mode, run spm as the target login user and, if appropriate, set XDG_RUNTIME_DIR=/run/user/$(id -u)", message, err)
	}
	if strings.Contains(message, "No journal files were found") || strings.Contains(strings.ToLower(message), "permission denied") && strings.Contains(strings.ToLower(message), "journal") {
		return fmt.Errorf("%s: %w; check journal storage and whether this user can read its journal", message, err)
	}
	if message != "" {
		return fmt.Errorf("%s: %w", message, err)
	}
	return err
}

type application struct {
	runner    runner
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	unitDir   string
	configDir string
	system    bool
	sleep     func(time.Duration)
	userHome  func(string) (string, error)
	// watchLimit bounds `list --watch` refreshes in tests; 0 means unlimited.
	watchLimit int
}

const (
	colorBoldYellow = "\x1b[1;33m"
	colorBoldCyan   = "\x1b[1;36m"
	colorReset      = "\x1b[0m"
)

func (a *application) warn(format string, args ...any) {
	a.notice("Warning", colorBoldYellow, format, args...)
}

func (a *application) hint(format string, args ...any) {
	a.notice("Hint", colorBoldCyan, format, args...)
}

func (a *application) notice(label, color, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if supportsColor(a.stderr) {
		fmt.Fprintf(a.stderr, "%s%s:%s %s\n", color, label, colorReset, message)
		return
	}
	fmt.Fprintf(a.stderr, "%s: %s\n", label, message)
}

func supportsColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func Run(args []string, stdout, stderr io.Writer) error {
	var system bool
	if len(args) > 0 && args[0] == "--system" {
		system = true
		args = args[1:]
	}
	if len(args) == 0 {
		printHelp(stdout)
		return nil
	}
	if args[0] == "__complete" {
		return runCompletionQuery(args[1:], stdout)
	}
	if args[0] == "completion" {
		if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			return printCommandHelp(stdout, "completion")
		}
		return printCompletion(stdout, args[1:])
	}
	if args[0] == "help" {
		if len(args) == 1 {
			printHelp(stdout)
			return nil
		}
		if len(args) == 2 {
			return printCommandHelp(stdout, args[1])
		}
		return errors.New("help accepts at most one command name")
	}
	if args[0] == "--help" || args[0] == "-h" {
		if len(args) != 1 {
			return errors.New("global help does not accept additional arguments")
		}
		printHelp(stdout)
		return nil
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			return printCommandHelp(stdout, "version")
		}
		if len(args) != 1 {
			return errors.New("version does not accept arguments")
		}
		fmt.Fprintf(stdout, "spm %s\n", version)
		return nil
	}
	if args[0] == "health-probe" {
		return (&application{stdout: stdout, stderr: stderr}).healthProbeCLI(args[1:])
	}
	if hasCommandHelp(args[1:]) {
		return printCommandHelp(stdout, args[0])
	}
	if runtime.GOOS != "linux" {
		return errors.New("spm only supports Linux with systemd")
	}

	unitDir := "/etc/systemd/system"
	configDir := "/etc/spm/services"
	if !system {
		var err error
		unitDir, err = userUnitDir()
		if err != nil {
			return err
		}
		configDir = filepath.Join(filepath.Dir(filepath.Dir(unitDir)), "spm", "services")
	} else if currentUser, err := user.Current(); err != nil {
		return fmt.Errorf("resolve current user: %w", err)
	} else if currentUser.Uid != "0" {
		return errors.New("system mode requires root; run: sudo spm --system <command>")
	}
	a := &application{
		runner:    execRunner{},
		stdin:     os.Stdin,
		stdout:    stdout,
		stderr:    stderr,
		unitDir:   unitDir,
		configDir: configDir,
		system:    system,
		sleep:     time.Sleep,
	}
	if !system {
		if currentUser, userErr := user.Current(); userErr == nil && currentUser.Uid == "0" {
			a.warn("Running as root manages root's user services under /root, not another user's services.")
		}
	}
	return a.run(args)
}

func (a *application) run(args []string) error {
	switch args[0] {
	case "start":
		return a.start(args[1:])
	case "schedule":
		return a.schedule(args[1:])
	case "run":
		name, err := oneName(args[1:])
		if err != nil {
			return err
		}
		return a.control("start", name)
	case "list", "ls", "status-all":
		options, err := parseList(args[1:])
		if err != nil {
			return err
		}
		return a.listWith(options)
	case "signal", "kill":
		return a.signal(args[1:])
	case "scale":
		return a.scale(args[1:])
	case "doctor":
		if len(args) != 1 {
			return errors.New("doctor does not accept arguments")
		}
		return a.doctor()
	case "status":
		return a.status(oneName(args[1:]))
	case "show", "describe":
		name, err := oneName(args[1:])
		if err != nil {
			return err
		}
		return a.describe(name)
	case "restart", "stop", "enable", "disable":
		return a.controlMany(args[0], args[1:])
	case "reload":
		return a.controlMany("reload", args[1:])
	case "set", "unset", "clone", "history", "rollback":
		return a.configCommand(args[0], args[1:])
	case "delete", "remove", "rm":
		return a.removeMany(args[1:])
	case "logs":
		return a.logs(args[1:])
	case "cat", "edit":
		name, err := oneName(args[1:])
		return a.inspectUnit(args[0], name, err)
	case "startup":
		return a.startup(args[1:])
	case "apply":
		return a.apply(args[1:])
	case "validate", "diff":
		return a.inspectDeclaration(args[0], args[1:])
	case "security":
		name, err := oneName(args[1:])
		if err != nil {
			return err
		}
		if !a.exists(name) {
			return fmt.Errorf("service %q is not managed by spm", name)
		}
		return a.runner.Interactive(a.input(), a.stdout, a.stderr, "systemd-analyze", append(a.systemctlArgs("security"), a.serviceUnits(name)[0])...)
	case "export":
		return a.exportConfig(args[1:])
	case "health":
		name, err := oneName(args[1:])
		if err != nil {
			return err
		}
		return a.healthStatus(name)
	case "health-probe":
		return a.healthProbeCLI(args[1:])
	case "import":
		return a.importUnit(args[1:])
	default:
		return fmt.Errorf("unknown command %q; run spm help", args[0])
	}
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `spm - manage persistent systemd services without writing unit files

Usage:
  spm [--system] <command> [options]

Common commands:
  spm start [options] <script> [arguments]
  spm start [options] -- <command> [arguments]
  spm start <name>
  spm schedule [options] <script|-- command>
  spm run <name>
  spm list [--json] [--watch[=DURATION]]
  spm status <name>
  spm show|describe <name>
  spm logs [options] [name...|all]
  spm cat|edit <name>
  spm restart [--update] <name...|all>
  spm stop|reload|enable|disable <name...|all>
  spm signal <SIGNAL> <name...|all> [--all-processes]
  spm scale <name> <count>
  spm doctor
  spm set|unset <name> [options]
  spm clone <source> <destination>
  spm history|rollback <name>
  spm delete <name...|all> [--yes]
  spm startup
  spm apply spm.toml
  spm validate spm.toml
  spm diff spm.toml
  spm security <name>
  spm export <name|all>
  spm health <name>
  spm import <unit-file> [--name NAME]
  spm completion bash|zsh|fish

Global options:
  --system                    Manage system services in /etc/systemd/system;
                              requires root

Start options:
  -n, --name NAME             Service name
  --cwd DIRECTORY             Working directory, defaults to the current directory
  --interpreter COMMAND       Interpreter used to run the script
  -e, --env KEY=VALUE         Add an environment variable, repeatable
  --env-file FILE             Load environment variables from a file
  --restart POLICY            no, on-success, on-failure, on-abnormal, on-abort,
                              on-watchdog, always; defaults to always
  --restart-sec DURATION      Restart delay, defaults to 3s
  --start-limit-interval DURATION
                              systemd start rate limit interval
  --start-limit-burst COUNT   Starts allowed inside the rate limit interval
  --restart-max-delay DUR     Grow restart delays up to DUR (systemd 254+)
  --restart-steps COUNT       Steps from --restart-sec to the maximum delay
  --cron-restart EXPRESSION   Restart on a 5-field cron schedule
  --timezone ZONE             Time zone for cron schedules, for example Asia/Singapore
  --type TYPE                 exec, simple, notify, notify-reload, forking, or idle
  --pid-file PATH             PID file for --type forking
  -i, --instances COUNT       Run COUNT instances from a template unit
  --max-memory SIZE           Set the cgroup MemoryMax limit
  --max-cpu PERCENT           Set CPUQuota, for example 200%
  --max-tasks COUNT           Set TasksMax
  --nice VALUE                Set process priority
  --open-files COUNT          Set LimitNOFILE
  --stop-signal SIGNAL        Signal used for graceful stop
  --stop-timeout DURATION     Graceful stop timeout
  --kill-mode MODE            systemd cgroup kill mode
  --sandbox basic|strict      Apply a systemd security preset
  --after UNIT                Order after a systemd unit, repeatable
  --wants UNIT                Add a weak dependency, repeatable
  --requires UNIT             Add a required dependency, repeatable
  --wait                      Wait until the service is active
  --wait-timeout DURATION     Readiness wait timeout, defaults to 30s
  --wait-http URL             Wait for an HTTP 2xx/3xx response
  --wait-tcp ADDRESS          Wait for a TCP connection
  --wait-command COMMAND      Wait for a successful shell command
  --health-http URL           Periodic HTTP health check
  --health-tcp ADDRESS        Periodic TCP health check
  --health-command COMMAND    Periodic shell command health check
  --health-interval DURATION  Check interval, defaults to 30s
  --health-timeout DURATION   Per-attempt timeout, defaults to 5s
  --health-retries COUNT      Attempts per check, defaults to 3
  --health-retry-delay DUR    Delay between attempts, defaults to 1s
  --health-start-period DUR   Delay before the first check
  --health-action ACTION      none, restart, or stop; defaults to none
  --user USER                 Run a system service as USER; requires --system
  --group GROUP               Run a system service as GROUP; requires --system
  --exec-start-pre COMMAND    Run before ExecStart, repeatable
  --exec-start-post COMMAND   Run after ExecStart, repeatable
  --exec-stop COMMAND         Custom stop command
  --exec-reload COMMAND       Custom reload command
  --on-failure UNIT           Activate a unit when this service fails, repeatable
  --property KEY=VALUE        Add a safe [Service] property, repeatable
  --description TEXT          Unit description
  --no-enable                 Start now without enabling at boot
  --enable                    Enable at boot when replacing a disabled service
  --no-cron-restart           Remove an inherited cron restart schedule
  --clear-env                 Remove inherited --env values
  --no-start-limit            Remove inherited start rate limits
  --no-max-memory             Remove an inherited MemoryMax limit
  --clear-properties          Remove inherited [Service] properties
  --clear-exec-hooks          Remove inherited ExecStartPre/Post/Stop/Reload
  --clear-on-failure          Remove inherited OnFailure units
  --mise                      Optionally run through "mise exec --"
  --no-mise                   Disable an inherited --mise setting
  --force                     Replace an existing spm service

Examples:
  spm start ./server --name api
  spm start --name api -- node app.js --port 3000
  spm schedule --name backup --cron "0 2 * * *" -- ./backup
  sudo spm --system start /opt/api/server --name api
  spm logs api --follow
`)
}

func hasCommandHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func printCommandHelp(w io.Writer, command string) error {
	switch command {
	case "start":
		fmt.Fprint(w, `Usage:
  spm start [options] <script> [arguments]
  spm start [options] -- <command> [arguments]
  spm start <name>

Options:
  -n, --name NAME             Service name
  --cwd DIRECTORY             Working directory
  --interpreter COMMAND       Script interpreter
  -e, --env KEY=VALUE         Environment variable, repeatable
  --env-file FILE             Environment file
  --restart POLICY            Restart policy, defaults to always
  --restart-sec DURATION      Restart delay, defaults to 3s
  --start-limit-interval DURATION
  --start-limit-burst COUNT
  --restart-max-delay DUR     Exponential restart delay ceiling
  --restart-steps COUNT       Restart delay steps
  --cron-restart EXPRESSION   Restart on a 5-field cron schedule
  --timezone ZONE             Time zone for cron schedules
  --type TYPE                 systemd service type, defaults to exec
  --pid-file PATH             PID file for --type forking
  -i, --instances COUNT       Run COUNT template instances
  --max-memory SIZE           cgroup MemoryMax limit
  --max-cpu PERCENT           cgroup CPUQuota limit
  --max-tasks COUNT           cgroup TasksMax limit
  --nice VALUE                Process priority
  --open-files COUNT          File descriptor limit
  --stop-signal SIGNAL        Graceful stop signal
  --stop-timeout DURATION     Graceful stop timeout
  --kill-mode MODE            systemd kill mode
  --sandbox basic|strict      Security hardening preset
  --after UNIT                Ordering dependency, repeatable
  --wants UNIT                Weak dependency, repeatable
  --requires UNIT             Required dependency, repeatable
  --wait                      Wait until active
  --wait-timeout DURATION     Readiness timeout
  --wait-http URL             HTTP readiness probe
  --wait-tcp ADDRESS          TCP readiness probe
  --wait-command COMMAND      Command readiness probe
  --health-http URL           Periodic HTTP health probe
  --health-tcp ADDRESS        Periodic TCP health probe
  --health-command COMMAND    Periodic command health probe
  --health-interval DURATION  Health interval
  --health-timeout DURATION   Attempt timeout
  --health-retries COUNT      Attempts per check
  --health-retry-delay DUR    Delay between attempts
  --health-start-period DUR   Initial delay
  --health-action ACTION      none, restart, or stop
  --user USER                 System service account; requires --system
  --group GROUP               System service group; requires --system
  --exec-start-pre COMMAND    Pre-start command, repeatable
  --exec-start-post COMMAND   Post-start command, repeatable
  --exec-stop COMMAND         Stop command
  --exec-reload COMMAND       Reload command
  --on-failure UNIT           Failure handler unit, repeatable
  --property KEY=VALUE        Safe [Service] property, repeatable
  --description TEXT          Unit description
  --no-enable                 Do not enable at boot
  --enable                    Enable at boot
  --no-cron-restart           Remove an inherited cron schedule
  --clear-env                 Remove inherited environment values
  --no-start-limit            Remove inherited start rate limits
  --no-max-memory             Remove inherited MemoryMax limit
  --clear-properties          Remove inherited [Service] properties
  --clear-exec-hooks          Remove inherited lifecycle commands
  --clear-on-failure          Remove inherited failure handlers
  --mise                      Optionally run through "mise exec --"
  --no-mise                   Disable inherited mise integration
  --force                     Replace and restart an existing service

Examples:
  spm start ./server --name api
  spm start --name api -- node app.js --port 3000
  spm start worker.py --name worker --interpreter python3
`)
	case "schedule":
		fmt.Fprint(w, `Usage:
  spm schedule [start options] [timer options] <script> [arguments]
  spm schedule [start options] [timer options] -- <command> [arguments]

Options:
  --cron EXPRESSION           Five-field cron schedule
  --on-boot DURATION          Run after boot or user manager startup
  --interval DURATION         Repeat after the previous activation
  --randomized-delay DUR      Add a random delay to timer triggers
  --persistent                Run once after a missed schedule
  --timezone ZONE             Time zone for --cron, for example Asia/Singapore
  --run-now                   Also run the task immediately
  --force                     Replace an existing service or schedule
  --no-enable                 Install without enabling the timer
`)
	case "run":
		fmt.Fprintln(w, "Usage:\n  spm run <name>")
	case "logs":
		fmt.Fprint(w, `Usage:
  spm logs [options] [name...|all]

Without a name, logs from all managed services are merged.

Options:
  -f, --follow                Follow new log entries
  -n, --lines N               Show the last N entries
  --since TIME                Show entries since TIME, for example 1h or today
  --until TIME                Show entries until TIME
  --grep PATTERN              Filter messages by pattern
  --boot                      Only show entries from the current boot
  --health                    Show health check and recovery logs
  -o, --output FORMAT         journalctl output format

Options may appear before or after the service name.
`)
	case "list", "ls", "status-all":
		fmt.Fprintln(w, "Usage:\n  spm list [--json] [--watch[=DURATION]]\n\nOptions:\n  --json             Print machine-readable JSON\n  --watch[=DURATION] Refresh until interrupted, defaults to 2s")
	case "signal", "kill":
		fmt.Fprintln(w, "Usage:\n  spm signal <SIGNAL> <name...|all> [--all-processes]\n\nSend SIGNAL, for example SIGUSR2 or HUP, to the main process.\n\nOptions:\n  --all-processes  Signal every process in the service cgroup")
	case "scale":
		fmt.Fprintln(w, "Usage:\n  spm scale <name> <count>\n\nChange the instance count of a service started with --instances.")
	case "doctor":
		fmt.Fprintln(w, "Usage:\n  spm doctor\n\nCheck the systemd manager, journal, lingering, mise, and sandbox support.")
	case "status":
		fmt.Fprintln(w, "Usage:\n  spm status <name>")
	case "show", "describe":
		fmt.Fprintf(w, "Usage:\n  spm %s <name>\n", command)
	case "cat", "edit":
		fmt.Fprintf(w, "Usage:\n  spm %s <name>\n", command)
	case "restart", "stop", "reload", "enable", "disable":
		if command == "restart" {
			fmt.Fprintln(w, "Usage:\n  spm restart [--update] <name...|all>\n\nOptions:\n  --update  Regenerate units from the saved spm configuration before restarting")
		} else {
			fmt.Fprintf(w, "Usage:\n  spm %s <name...|all>\n", command)
		}
	case "set", "unset":
		fmt.Fprintf(w, "Usage:\n  spm %s <name> [options]\n", command)
	case "clone":
		fmt.Fprintln(w, "Usage:\n  spm clone <source> <destination>")
	case "history":
		fmt.Fprintln(w, "Usage:\n  spm history <name>")
	case "rollback":
		fmt.Fprintln(w, "Usage:\n  spm rollback <name> [--revision N]")
	case "delete", "remove", "rm":
		fmt.Fprintln(w, "Usage:\n  spm delete <name...|all> [--yes]\n\nOptions:\n  -y, --yes  Confirm deleting all managed services")
	case "startup":
		fmt.Fprintln(w, "Usage:\n  spm startup")
	case "apply":
		fmt.Fprintln(w, "Usage:\n  spm apply [--dry-run] spm.toml")
	case "validate", "diff":
		fmt.Fprintf(w, "Usage:\n  spm %s spm.toml\n", command)
	case "security":
		fmt.Fprintln(w, "Usage:\n  spm security <name>")
	case "export":
		fmt.Fprintln(w, "Usage:\n  spm export <name|all>")
	case "health":
		fmt.Fprintln(w, "Usage:\n  spm health <name>")
	case "import":
		fmt.Fprintln(w, "Usage:\n  spm import <unit-file> [--name NAME]")
	case "version":
		fmt.Fprintln(w, "Usage:\n  spm version")
	case "completion":
		fmt.Fprintln(w, "Usage:\n  spm completion bash|zsh|fish\n\nGenerate context-aware shell completion code.")
	default:
		return fmt.Errorf("unknown command %q; run spm help", command)
	}
	return nil
}

func userUnitDir() (string, error) {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		if !filepath.IsAbs(configHome) {
			return "", errors.New("XDG_CONFIG_HOME must be an absolute path")
		}
		return filepath.Join(configHome, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func (a *application) systemctlArgs(args ...string) []string {
	if a.system {
		return args
	}
	return append([]string{"--user"}, args...)
}

func (a *application) runSystemctl(args ...string) ([]byte, error) {
	return a.runner.Run("systemctl", a.systemctlArgs(args...)...)
}

func (a *application) interactiveSystemctl(args ...string) error {
	return a.runner.Interactive(a.input(), a.stdout, a.stderr, "systemctl", a.systemctlArgs(args...)...)
}

func oneName(args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("exactly one service name is required")
	}
	if err := validateName(args[0]); err != nil {
		return "", err
	}
	return args[0], nil
}

func validateName(name string) error {
	if name == "all" {
		return errors.New(`service name "all" is reserved for batch operations`)
	}
	if name == "" || len(name) > 128 {
		return errors.New("service name must contain between 1 and 128 characters")
	}
	if strings.HasSuffix(name, ".cron-restart") {
		return fmt.Errorf("service name %q uses the reserved suffix .cron-restart", name)
	}
	for i, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.'
		if !valid || i == 0 && (r == '.' || r == '-') {
			return fmt.Errorf("invalid service name %q; use letters, numbers, dot, underscore, and dash", name)
		}
	}
	return nil
}

func unitName(name string) string {
	return "spm-" + name + ".service"
}

func templateUnitName(name string) string {
	return "spm-" + name + "@.service"
}

func instanceUnitName(name string, index int) string {
	return "spm-" + name + "@" + strconv.Itoa(index) + ".service"
}

func (a *application) templatePath(name string) string {
	return filepath.Join(a.unitDir, templateUnitName(name))
}

func (a *application) isTemplate(name string) bool {
	info, err := os.Stat(a.templatePath(name))
	return err == nil && info.Mode().IsRegular()
}

// instanceCount returns 0 for a plain service and the saved instance count for
// a template service.
func (a *application) instanceCount(name string) int {
	if !a.isTemplate(name) {
		return 0
	}
	if config, err := a.readServiceConfig(name); err == nil && config.Instances > 0 {
		return config.Instances
	}
	return 1
}

func instanceUnits(name string, count int) []string {
	units := make([]string, count)
	for index := range units {
		units[index] = instanceUnitName(name, index)
	}
	return units
}

// serviceUnits returns the concrete service units managed for name.
func (a *application) serviceUnits(name string) []string {
	if count := a.instanceCount(name); count > 0 {
		return instanceUnits(name, count)
	}
	return []string{unitName(name)}
}

// unitLabel maps spm-api.service to api and spm-api@1.service to api@1.
func unitLabel(unit string) string {
	return strings.TrimSuffix(strings.TrimPrefix(unit, "spm-"), ".service")
}

func (a *application) savedRestartPolicy(name string) string {
	if config, err := a.readServiceConfig(name); err == nil && config.Restart != "" {
		return config.Restart
	}
	return "always"
}

func cronRestartServiceName(name string) string {
	return "spm-" + name + ".cron-restart.service"
}

func cronRestartTimerName(name string) string {
	return "spm-" + name + ".cron-restart.timer"
}

func (a *application) unitPath(name string) string {
	return filepath.Join(a.unitDir, unitName(name))
}

func (a *application) cronRestartServicePath(name string) string {
	return filepath.Join(a.unitDir, cronRestartServiceName(name))
}

func (a *application) cronRestartTimerPath(name string) string {
	return filepath.Join(a.unitDir, cronRestartTimerName(name))
}

func scheduleTimerName(name string) string {
	return "spm-" + name + ".timer"
}

func (a *application) scheduleTimerPath(name string) string {
	return filepath.Join(a.unitDir, scheduleTimerName(name))
}

func (a *application) hasSchedule(name string) bool {
	info, err := os.Stat(a.scheduleTimerPath(name))
	return err == nil && info.Mode().IsRegular()
}

func (a *application) serviceConfigPath(name string) string {
	directory := a.configDir
	if directory == "" {
		directory = filepath.Join(a.unitDir, ".spm")
	}
	return filepath.Join(directory, name+".json")
}

func (a *application) hasCronRestart(name string) bool {
	info, err := os.Stat(a.cronRestartTimerPath(name))
	return err == nil && info.Mode().IsRegular()
}

func (a *application) removeCronRestartFiles(name string) error {
	for _, path := range []string{a.cronRestartTimerPath(name), a.cronRestartServicePath(name)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove cron restart unit %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func (a *application) control(action, name string) error {
	if !a.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	if a.isTemplate(name) {
		return a.controlInstances(action, name)
	}
	if a.hasSchedule(name) {
		var args []string
		switch action {
		case "enable":
			args = []string{"enable", "--now", scheduleTimerName(name)}
		case "disable":
			args = []string{"disable", "--now", scheduleTimerName(name)}
		case "stop":
			if _, err := a.runSystemctl("stop", scheduleTimerName(name)); err != nil {
				return fmt.Errorf("stop schedule for %s: %w", name, err)
			}
			args = []string{"stop", unitName(name)}
		case "reload":
			return fmt.Errorf("scheduled task %q cannot be reloaded; use `spm run %s`", name, name)
		default:
			args = []string{action, unitName(name)}
		}
		if _, err := a.runSystemctl(args...); err != nil {
			return fmt.Errorf("%s scheduled task %s: %w", action, name, err)
		}
		fmt.Fprintf(a.stdout, "%s %s\n", name, actionPastTense(action))
		return nil
	}
	hasHealth := a.hasHealth(name)
	if hasHealth && (action == "stop" || action == "disable" || action == "restart") {
		timerAction := "stop"
		args := []string{"stop", healthTimerName(name)}
		if action == "disable" {
			timerAction = "disable"
			args = []string{"disable", "--now", healthTimerName(name)}
		}
		if _, err := a.runSystemctl(args...); err != nil {
			return fmt.Errorf("%s health timer for %s: %w", timerAction, name, err)
		}
	}
	cronTimer := ""
	if a.hasCronRestart(name) {
		cronTimer = cronRestartTimerName(name)
	}
	if cronTimer != "" && (action == "stop" || action == "disable" || action == "restart") {
		timerAction := "stop"
		args := []string{"stop", cronTimer}
		if action == "disable" {
			timerAction = "disable"
			args = []string{"disable", "--now", cronTimer}
		}
		if _, err := a.runSystemctl(args...); err != nil {
			return fmt.Errorf("%s cron restart for %s: %w", timerAction, name, err)
		}
	}
	args := []string{action, unitName(name)}
	if action == "disable" {
		args = []string{"disable", "--now", unitName(name)}
	}
	if _, err := a.runSystemctl(args...); err != nil {
		return fmt.Errorf("%s %s: %w", action, name, err)
	}
	if cronTimer != "" && (action == "start" || action == "restart" || action == "enable") {
		timerAction := action
		if action == "restart" {
			timerAction = "start"
		}
		if _, err := a.runSystemctl(timerAction, cronTimer); err != nil {
			return fmt.Errorf("%s cron restart for %s: %w", timerAction, name, err)
		}
	}
	if hasHealth && (action == "start" || action == "restart" || action == "enable") {
		timerAction := "start"
		if action == "enable" {
			timerAction = "enable"
		}
		if _, err := a.runSystemctl(timerAction, healthTimerName(name)); err != nil {
			return fmt.Errorf("%s health timer for %s: %w", timerAction, name, err)
		}
	}
	fmt.Fprintf(a.stdout, "%s %s\n", name, actionPastTense(action))
	return nil
}

// controlInstances restarts and reloads instances one at a time so that the
// remaining instances keep serving while each one is replaced.
func (a *application) controlInstances(action, name string) error {
	units := a.serviceUnits(name)
	switch action {
	case "restart", "reload":
		restart := a.savedRestartPolicy(name)
		for index, unit := range units {
			if _, err := a.runSystemctl(action, unit); err != nil {
				return fmt.Errorf("%s %s: %w", action, unitLabel(unit), err)
			}
			if action == "restart" && index < len(units)-1 {
				if err := a.checkUnitStarted(unit, restart); err != nil {
					return fmt.Errorf("rolling restart stopped: %w", err)
				}
			}
		}
	case "disable":
		if _, err := a.runSystemctl(append([]string{"disable", "--now"}, units...)...); err != nil {
			return fmt.Errorf("%s %s: %w", action, name, err)
		}
	default:
		if _, err := a.runSystemctl(append([]string{action}, units...)...); err != nil {
			return fmt.Errorf("%s %s: %w", action, name, err)
		}
	}
	fmt.Fprintf(a.stdout, "%s %s (%d instances)\n", name, actionPastTense(action), len(units))
	return nil
}

func (a *application) controlMany(action string, selectors []string) error {
	update := false
	filtered := selectors[:0]
	for _, selector := range selectors {
		if selector == "--update" {
			if action != "restart" {
				return errors.New("--update is only supported by restart")
			}
			update = true
			continue
		}
		filtered = append(filtered, selector)
	}
	selectors = filtered
	names, err := a.resolveServiceNames(selectors)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(a.stdout, "No services managed by spm.")
		return nil
	}
	var errs []error
	for _, name := range names {
		var err error
		if update {
			err = a.updateFromSavedConfig(name)
		} else {
			err = a.control(action, name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func actionPastTense(action string) string {
	switch action {
	case "start":
		return "started"
	case "stop":
		return "stopped"
	case "restart":
		return "restarted"
	case "enable":
		return "enabled"
	case "disable":
		return "disabled and stopped"
	case "reload":
		return "reloaded"
	default:
		return action
	}
}

func (a *application) exists(name string) bool {
	info, err := os.Stat(a.unitPath(name))
	return err == nil && info.Mode().IsRegular() || a.isTemplate(name)
}

func (a *application) remove(name string) error {
	if !a.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	if a.isTemplate(name) {
		units := a.serviceUnits(name)
		if _, err := a.runSystemctl(append([]string{"disable", "--now"}, units...)...); err != nil {
			return fmt.Errorf("stop and disable %s: %w", name, err)
		}
		if err := os.Remove(a.templatePath(name)); err != nil {
			return fmt.Errorf("remove template unit file: %w", err)
		}
		if err := a.removeSavedConfig(name); err != nil {
			return err
		}
		if _, err := a.runSystemctl("daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd after removing %s: %w", name, err)
		}
		_, _ = a.runSystemctl(append([]string{"reset-failed"}, units...)...)
		fmt.Fprintf(a.stdout, "%s deleted\n", name)
		return nil
	}
	hadCronRestart := a.hasCronRestart(name)
	hadSchedule := a.hasSchedule(name)
	hadHealth := a.hasHealth(name)
	if hadCronRestart {
		if _, err := a.runSystemctl("disable", "--now", cronRestartTimerName(name)); err != nil {
			return fmt.Errorf("disable cron restart for %s: %w", name, err)
		}
	}
	if hadSchedule {
		if _, err := a.runSystemctl("disable", "--now", scheduleTimerName(name)); err != nil {
			return fmt.Errorf("disable schedule for %s: %w", name, err)
		}
	}
	if hadHealth {
		if _, err := a.runSystemctl("disable", "--now", healthTimerName(name)); err != nil {
			return fmt.Errorf("disable health timer for %s: %w", name, err)
		}
	}
	if _, err := a.runSystemctl("disable", "--now", unitName(name)); err != nil {
		return fmt.Errorf("stop and disable %s: %w", name, err)
	}
	if err := os.Remove(a.unitPath(name)); err != nil {
		return fmt.Errorf("remove unit file: %w", err)
	}
	if err := a.removeCronRestartFiles(name); err != nil {
		return err
	}
	if err := os.Remove(a.scheduleTimerPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove schedule timer: %w", err)
	}
	if err := a.removeHealthFiles(name); err != nil {
		return fmt.Errorf("remove health units: %w", err)
	}
	if err := a.removeSavedConfig(name); err != nil {
		return err
	}
	if _, err := a.runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd after removing %s: %w", name, err)
	}
	resetUnits := []string{unitName(name)}
	if hadCronRestart {
		resetUnits = append(resetUnits, cronRestartServiceName(name), cronRestartTimerName(name))
	}
	if hadSchedule {
		resetUnits = append(resetUnits, scheduleTimerName(name))
	}
	if hadHealth {
		resetUnits = append(resetUnits, healthServiceName(name), healthTimerName(name), healthRecoveryName(name))
	}
	_, _ = a.runSystemctl(append([]string{"reset-failed"}, resetUnits...)...)
	fmt.Fprintf(a.stdout, "%s deleted\n", name)
	return nil
}

func (a *application) removeSavedConfig(name string) error {
	if err := os.Remove(a.serviceConfigPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove saved service config: %w", err)
	}
	if err := os.RemoveAll(a.historyDirectory(name)); err != nil {
		return fmt.Errorf("remove saved service history: %w", err)
	}
	return nil
}

func (a *application) removeMany(selectors []string) error {
	yes := false
	filtered := selectors[:0]
	for _, selector := range selectors {
		if selector == "--yes" || selector == "-y" {
			yes = true
			continue
		}
		filtered = append(filtered, selector)
	}
	selectors = filtered
	if len(selectors) == 1 && selectors[0] == "all" && !yes {
		return errors.New("deleting all managed services requires --yes")
	}
	names, err := a.resolveServiceNames(selectors)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(a.stdout, "No services managed by spm.")
		return nil
	}
	var errs []error
	for _, name := range names {
		if err := a.remove(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (a *application) resolveServiceNames(selectors []string) ([]string, error) {
	if len(selectors) == 0 {
		return nil, errors.New("at least one service name or all is required")
	}
	if len(selectors) == 1 && selectors[0] == "all" {
		return a.managedNames()
	}
	for _, selector := range selectors {
		if selector == "all" {
			return nil, errors.New(`"all" cannot be combined with service names`)
		}
	}
	seen := make(map[string]bool, len(selectors))
	names := make([]string, 0, len(selectors))
	for _, name := range selectors {
		if err := validateName(name); err != nil {
			return nil, err
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

type serviceState struct {
	active      string
	sub         string
	pid         string
	memory      string
	cpuNSec     string
	restarts    string
	enabled     string
	activeSince uint64
	result      string
	execCode    string
	execStatus  string
}

func (a *application) readScheduleStates(names []string) map[string]serviceState {
	states := make(map[string]serviceState)
	args := []string{"show"}
	for _, name := range names {
		if a.hasSchedule(name) {
			args = append(args, scheduleTimerName(name))
		}
	}
	if len(args) == 1 {
		return states
	}
	args = append(args, "--property=Id", "--property=ActiveState", "--property=SubState", "--property=UnitFileState", "--property=ActiveEnterTimestampMonotonic")
	output, err := a.runSystemctl(args...)
	if err != nil {
		return states
	}
	for _, block := range strings.Split(strings.TrimSpace(string(output)), "\n\n") {
		values := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				values[key] = value
			}
		}
		id := values["Id"]
		if !strings.HasPrefix(id, "spm-") || !strings.HasSuffix(id, ".timer") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(id, "spm-"), ".timer")
		activeSince, _ := strconv.ParseUint(values["ActiveEnterTimestampMonotonic"], 10, 64)
		states[name] = serviceState{active: values["ActiveState"], sub: values["SubState"], enabled: values["UnitFileState"], activeSince: activeSince}
	}
	return states
}

func (a *application) managedNames() ([]string, error) {
	entries, err := os.ReadDir(a.unitDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read unit directory: %w", err)
	}
	var names []string
	seen := map[string]bool{}
	for _, entry := range entries {
		filename := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(filename, "spm-") || !strings.HasSuffix(filename, ".service") || strings.HasSuffix(filename, ".cron-restart.service") || strings.HasSuffix(filename, ".health.service") || strings.HasSuffix(filename, ".health-recover.service") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(filename, "spm-"), ".service"), "@")
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (a *application) readState(name string) serviceState {
	return a.readUnitStates([]string{unitName(name)})[unitName(name)]
}

func (a *application) readStates(names []string) map[string]serviceState {
	units := make([]string, len(names))
	for i, name := range names {
		units[i] = unitName(name)
	}
	unitStates := a.readUnitStates(units)
	states := make(map[string]serviceState, len(names))
	for _, name := range names {
		if state, ok := unitStates[unitName(name)]; ok {
			states[name] = state
		}
	}
	return states
}

// readUnitStates reads the runtime state of service units with one
// systemctl call, keyed by unit name.
func (a *application) readUnitStates(units []string) map[string]serviceState {
	states := make(map[string]serviceState, len(units))
	if len(units) == 0 {
		return states
	}
	args := append([]string{"show"}, units...)
	args = append(args, "--property=Id", "--property=ActiveState", "--property=SubState", "--property=MainPID", "--property=MemoryCurrent", "--property=CPUUsageNSec", "--property=NRestarts", "--property=UnitFileState", "--property=ActiveEnterTimestampMonotonic", "--property=Result", "--property=ExecMainCode", "--property=ExecMainStatus")
	output, err := a.runSystemctl(args...)
	if err != nil {
		return states
	}
	for _, block := range strings.Split(strings.TrimSpace(string(output)), "\n\n") {
		values := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				values[key] = value
			}
		}
		id := values["Id"]
		if !strings.HasPrefix(id, "spm-") || !strings.HasSuffix(id, ".service") {
			continue
		}
		pid := values["MainPID"]
		if pid == "0" {
			pid = ""
		}
		activeSince, _ := strconv.ParseUint(values["ActiveEnterTimestampMonotonic"], 10, 64)
		states[id] = serviceState{active: values["ActiveState"], sub: values["SubState"], pid: pid, memory: values["MemoryCurrent"], cpuNSec: values["CPUUsageNSec"], restarts: values["NRestarts"], enabled: values["UnitFileState"], activeSince: activeSince, result: values["Result"], execCode: values["ExecMainCode"], execStatus: values["ExecMainStatus"]}
	}
	return states
}

func readBootUptime() time.Duration {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func formatUptime(bootUptime time.Duration, activeSinceUSec uint64) string {
	if bootUptime <= 0 || activeSinceUSec == 0 {
		return "-"
	}
	uptime := bootUptime - time.Duration(activeSinceUSec)*time.Microsecond
	if uptime < 0 {
		return "-"
	}
	return uptime.Truncate(time.Second).String()
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func formatBytes(raw string) string {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return "-"
	}
	const (
		kiB = 1024
		miB = 1024 * kiB
		giB = 1024 * miB
	)
	switch {
	case value >= giB:
		return fmt.Sprintf("%.1f GiB", float64(value)/giB)
	case value >= miB:
		return fmt.Sprintf("%.1f MiB", float64(value)/miB)
	case value >= kiB:
		return fmt.Sprintf("%.1f KiB", float64(value)/kiB)
	default:
		return fmt.Sprintf("%d B", value)
	}
}

func (a *application) status(name string, parseErr error) error {
	if parseErr != nil {
		return parseErr
	}
	if !a.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	units := a.serviceUnits(name)
	if a.hasSchedule(name) {
		units = append([]string{scheduleTimerName(name)}, units...)
	}
	args := append([]string{"status"}, units...)
	args = append(args, "--no-pager", "--full")
	err := a.interactiveSystemctl(args...)
	if err == nil {
		return nil
	}
	var exitError interface{ ExitCode() int }
	if errors.As(err, &exitError) && exitError.ExitCode() == 3 {
		return nil
	}
	return err
}

func (a *application) describe(name string) error {
	if !a.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	if a.isTemplate(name) {
		return a.describeInstances(name)
	}
	state := a.readState(name)
	if scheduleState, ok := a.readScheduleStates([]string{name})[name]; ok {
		state.active = scheduleState.active
		state.sub = scheduleState.sub
		state.enabled = scheduleState.enabled
		state.activeSince = scheduleState.activeSince
	}
	status := state.active
	if state.sub != "" && state.sub != state.active {
		status += "/" + state.sub
	}
	rows := [][2]string{
		{"status", valueOr(status, "unknown")},
		{"name", name},
		{"enabled", valueOr(state.enabled, "unknown")},
		{"pid", valueOr(state.pid, "-")},
		{"memory", formatBytes(state.memory)},
		{"restarts", valueOr(state.restarts, "0")},
		{"uptime", formatUptime(readBootUptime(), state.activeSince)},
		{"result", valueOr(state.result, "-")},
		{"exec status", formatExecStatus(state)},
		{"schedule", valueOr(a.readSchedule(name), "-")},
		{"health", a.readHealthStates([]string{name})[name]},
	}
	rows = append(rows, a.configRows(name)...)
	rows = append(rows, [2]string{"unit file", a.unitPath(name)}, [2]string{"config file", a.serviceConfigPath(name)})
	return a.printRows(rows)
}

func (a *application) describeInstances(name string) error {
	units := a.serviceUnits(name)
	states := a.readUnitStates(units)
	bootUptime := readBootUptime()
	rows := [][2]string{{"name", name}, {"instances", strconv.Itoa(len(units))}}
	for _, unit := range units {
		state := states[unit]
		rows = append(rows, [2]string{unitLabel(unit), fmt.Sprintf("%s pid=%s memory=%s restarts=%s uptime=%s", valueOr(formatStatus(state), "unknown"), valueOr(state.pid, "-"), formatBytes(state.memory), valueOr(state.restarts, "0"), formatUptime(bootUptime, state.activeSince))})
	}
	rows = append(rows, a.configRows(name)...)
	rows = append(rows, [2]string{"unit file", a.templatePath(name)}, [2]string{"config file", a.serviceConfigPath(name)})
	return a.printRows(rows)
}

func (a *application) configRows(name string) [][2]string {
	config, err := a.readServiceConfig(name)
	if err != nil {
		return nil
	}
	runUser := config.User
	if runUser == "" {
		runUser = "current user"
		if a.system {
			runUser = "root"
		}
	}
	return [][2]string{
		{"command", strings.Join(config.Command, " ")},
		{"cwd", valueOr(config.Cwd, "-")},
		{"type", valueOr(config.Type, "exec")},
		{"mise", strconv.FormatBool(config.Mise)},
		{"restart", valueOr(config.Restart, "-")},
		{"restart delay", valueOr(config.RestartSec, "-")},
		{"timezone", valueOr(config.Timezone, "system")},
		{"memory limit", valueOr(config.MaxMemory, "-")},
		{"user", runUser},
		{"group", valueOr(config.Group, "default")},
		{"env file", valueOr(config.EnvFile, "-")},
		{"env keys", environmentKeys(config.Env)},
	}
}

func (a *application) printRows(rows [][2]string) error {
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%s\n", row[0], row[1])
	}
	return w.Flush()
}

func formatStatus(state serviceState) string {
	if state.sub != "" && state.sub != state.active {
		return state.active + "/" + state.sub
	}
	return state.active
}

func formatExecStatus(state serviceState) string {
	if state.execCode == "" {
		return valueOr(state.execStatus, "-")
	}
	if state.execStatus == "" {
		return state.execCode
	}
	return state.execCode + "/" + state.execStatus
}

func environmentKeys(values []string) string {
	keys := make([]string, 0, len(values))
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "-"
	}
	return strings.Join(keys, ",")
}

func (a *application) readCronRestart(name string) string {
	data, err := os.ReadFile(a.cronRestartTimerPath(name))
	if err != nil {
		return ""
	}
	const prefix = "# CronExpression="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func readTimerTimezone(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "# Timezone=") {
			return strings.TrimPrefix(line, "# Timezone=")
		}
	}
	return ""
}

func (a *application) readSchedule(name string) string {
	if expression := a.readCronRestart(name); expression != "" {
		if timezone := readTimerTimezone(a.cronRestartTimerPath(name)); timezone != "" {
			expression += " " + timezone
		}
		return "restart: " + expression
	}
	data, err := os.ReadFile(a.scheduleTimerPath(name))
	if err != nil {
		return ""
	}
	const prefix = "# CronExpression="
	var values []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			values = append(values, strings.TrimPrefix(line, prefix))
		} else if strings.HasPrefix(line, "# Timezone=") && len(values) > 0 {
			values[len(values)-1] += " " + strings.TrimPrefix(line, "# Timezone=")
		} else if strings.HasPrefix(line, "# TimerOnBoot=") {
			values = append(values, "boot+"+strings.TrimPrefix(line, "# TimerOnBoot="))
		} else if strings.HasPrefix(line, "# TimerInterval=") {
			values = append(values, "every "+strings.TrimPrefix(line, "# TimerInterval="))
		} else if strings.HasPrefix(line, "# TimerRandomizedDelay=") {
			values = append(values, "jitter "+strings.TrimPrefix(line, "# TimerRandomizedDelay="))
		}
	}
	return strings.Join(values, ", ")
}

func (a *application) logs(args []string) error {
	options, err := parseLogs(args)
	if err != nil {
		return err
	}
	names := options.names
	if len(names) == 0 || len(names) == 1 && names[0] == "all" {
		names, err = a.managedNames()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Fprintln(a.stdout, "No services managed by spm.")
			return nil
		}
	}
	var units []string
	for _, name := range names {
		if !a.exists(name) {
			return fmt.Errorf("service %q is not managed by spm", name)
		}
		if !options.health {
			units = append(units, a.serviceUnits(name)...)
		} else if a.hasHealth(name) {
			units = append(units, healthServiceName(name), healthRecoveryName(name))
		} else if len(options.names) == 1 && options.names[0] != "all" {
			return fmt.Errorf("service %q has no health check", name)
		}
	}
	if len(units) == 0 {
		return errors.New("none of the selected services has a health check")
	}
	journalArgs := []string{}
	for _, unit := range units {
		journalArgs = append(journalArgs, "--unit", unit)
	}
	journalArgs = append(journalArgs, "--no-pager")
	var fallbackArgs []string
	if !a.system {
		journalArgs = append([]string{"--user"}, journalArgs...)
		for _, unit := range units {
			fallbackArgs = append(fallbackArgs, "--user-unit", unit)
		}
		fallbackArgs = append(fallbackArgs, "--no-pager")
	}
	if options.follow {
		journalArgs = append(journalArgs, "--follow")
		if fallbackArgs != nil {
			fallbackArgs = append(fallbackArgs, "--follow")
		}
	}
	if options.lines != "" {
		journalArgs = append(journalArgs, "--lines", options.lines)
		if fallbackArgs != nil {
			fallbackArgs = append(fallbackArgs, "--lines", options.lines)
		}
	}
	if options.since != "" {
		journalArgs = append(journalArgs, "--since", options.since)
		if fallbackArgs != nil {
			fallbackArgs = append(fallbackArgs, "--since", options.since)
		}
	}
	if options.until != "" {
		journalArgs = append(journalArgs, "--until", options.until)
		if fallbackArgs != nil {
			fallbackArgs = append(fallbackArgs, "--until", options.until)
		}
	}
	if options.grep != "" {
		journalArgs = append(journalArgs, "--grep", options.grep)
		if fallbackArgs != nil {
			fallbackArgs = append(fallbackArgs, "--grep", options.grep)
		}
	}
	if options.boot {
		journalArgs = append(journalArgs, "--boot")
		if fallbackArgs != nil {
			fallbackArgs = append(fallbackArgs, "--boot")
		}
	}
	if options.output != "" {
		journalArgs = append(journalArgs, "--output", options.output)
		if fallbackArgs != nil {
			fallbackArgs = append(fallbackArgs, "--output", options.output)
		}
	}
	if err := a.runner.Interactive(a.input(), a.stdout, a.stderr, "journalctl", journalArgs...); err == nil || terminatedBySignal(err) {
		// A signal usually means the reader went away, for example
		// `spm logs | head`, so it is neither an error nor a reason to retry.
		return nil
	} else if fallbackArgs == nil {
		return err
	}
	return a.runner.Interactive(a.input(), a.stdout, a.stderr, "journalctl", fallbackArgs...)
}

func terminatedBySignal(err error) bool {
	var exitError *exec.ExitError
	return errors.As(err, &exitError) && exitError.ExitCode() == -1
}

type logsOptions struct {
	names  []string
	follow bool
	lines  string
	since  string
	until  string
	grep   string
	output string
	boot   bool
	health bool
}

func parseLogs(args []string) (logsOptions, error) {
	options := logsOptions{lines: "100"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		key, inlineValue, hasInlineValue := strings.Cut(arg, "=")
		switch key {
		case "-f", "--follow":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.follow = true
		case "--boot":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.boot = true
		case "--health":
			if hasInlineValue {
				return options, fmt.Errorf("%s does not accept a value", key)
			}
			options.health = true
		case "-n", "--lines":
			value := inlineValue
			if !hasInlineValue {
				if i+1 >= len(args) {
					return options, fmt.Errorf("%s requires a number", key)
				}
				i++
				value = args[i]
			}
			if _, err := strconv.ParseUint(value, 10, 32); err != nil {
				return options, fmt.Errorf("invalid line count %q", value)
			}
			options.lines = value
		case "--since", "--until", "--grep", "-o", "--output":
			value := inlineValue
			if !hasInlineValue {
				if i+1 >= len(args) {
					return options, fmt.Errorf("%s requires a value", key)
				}
				i++
				value = args[i]
			}
			if value == "" || strings.ContainsAny(value, "\r\n") {
				return options, fmt.Errorf("invalid value for %s", key)
			}
			switch key {
			case "--since":
				options.since = value
			case "--until":
				options.until = value
			case "--grep":
				options.grep = value
			default:
				options.output = value
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return options, fmt.Errorf("unknown logs option %q", arg)
			}
			options.names = append(options.names, arg)
		}
	}
	for _, name := range options.names {
		if name == "all" {
			if len(options.names) != 1 {
				return options, errors.New(`"all" cannot be combined with service names`)
			}
			continue
		}
		if err := validateName(name); err != nil {
			return options, err
		}
	}
	return options, nil
}

func (a *application) input() io.Reader {
	if a.stdin != nil {
		return a.stdin
	}
	return strings.NewReader("")
}

func (a *application) inspectUnit(action, name string, parseErr error) error {
	if parseErr != nil {
		return parseErr
	}
	if !a.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	unit := unitName(name)
	if a.isTemplate(name) {
		unit = templateUnitName(name)
	}
	args := []string{action, unit}
	if action == "cat" {
		args = append(args, "--no-pager")
	}
	return a.interactiveSystemctl(args...)
}

func (a *application) startup(args []string) error {
	if len(args) != 0 {
		return errors.New("startup does not accept arguments")
	}
	if a.system {
		fmt.Fprintln(a.stdout, "System services do not need user lingering. Enabled services already start at boot.")
		return nil
	}
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve current user: %w", err)
	}
	if enabled, checkErr := a.lingerEnabled(currentUser.Username); checkErr == nil && enabled {
		fmt.Fprintf(a.stdout, "User lingering is already enabled for %s.\n", currentUser.Username)
		return nil
	}
	if _, err := a.runner.Run("loginctl", "enable-linger", currentUser.Username); err != nil {
		return fmt.Errorf("enable user lingering: %w; try: sudo loginctl enable-linger %s", err, currentUser.Username)
	}
	fmt.Fprintf(a.stdout, "User lingering enabled for %s. Enabled services can start at boot without an interactive login.\n", currentUser.Username)
	return nil
}

func (a *application) lingerEnabled(username string) (bool, error) {
	output, err := a.runner.Run("loginctl", "show-user", username, "--property=Linger", "--value")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(string(output))) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected linger state %q", strings.TrimSpace(string(output)))
	}
}
