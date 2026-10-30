package app

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

var completionCommands = []string{
	"start", "schedule", "run", "list", "ls", "status-all", "status", "show", "describe", "logs", "cat", "edit",
	"restart", "stop", "reload", "enable", "disable", "delete", "remove", "rm", "set", "unset",
	"clone", "history", "rollback", "startup", "apply", "validate", "diff", "security",
	"export", "health", "import", "signal", "kill", "scale", "doctor", "completion", "help", "version",
}

var signalCompletionValues = []string{"SIGHUP", "SIGINT", "SIGQUIT", "SIGTERM", "SIGKILL", "SIGUSR1", "SIGUSR2", "SIGWINCH", "SIGCONT", "SIGSTOP"}

var startCompletionOptions = []string{
	"--name", "-n", "--cwd", "--interpreter", "--env", "-e", "--env-file", "--restart",
	"--restart-sec", "--start-limit-interval", "--start-limit-burst", "--cron-restart",
	"--max-memory", "--max-cpu", "--max-tasks", "--nice", "--open-files", "--stop-signal",
	"--stop-timeout", "--kill-mode", "--sandbox", "--after", "--wants", "--requires",
	"--on-failure", "--wait", "--wait-timeout", "--wait-http", "--wait-tcp", "--wait-command",
	"--health-http", "--health-tcp", "--health-command", "--health-interval", "--health-timeout",
	"--health-retries", "--health-retry-delay", "--health-start-period", "--health-action",
	"--user", "--group", "--exec-start-pre", "--exec-start-post", "--exec-stop", "--exec-reload",
	"--property", "--description", "--no-enable", "--enable", "--no-cron-restart", "--clear-env",
	"--no-start-limit", "--no-max-memory", "--clear-properties", "--clear-exec-hooks",
	"--clear-on-failure", "--mise", "--no-mise", "--force", "-f", "--help", "-h",
	"--type", "--pid-file", "--timezone", "--instances", "-i", "--restart-max-delay", "--restart-steps",
}

var scheduleCompletionOptions = append(append([]string(nil), startCompletionOptions...),
	"--cron", "--on-boot", "--interval", "--randomized-delay", "--persistent", "--run-now")

var configCompletionOptions = []string{
	"--cwd", "--description", "--restart", "--restart-sec", "--cron-restart", "--schedule",
	"--timer-on-boot", "--timer-interval", "--timer-randomized-delay", "--max-memory", "--env-file",
	"--user", "--group", "--enable", "--no-enable", "--persistent", "--no-persistent", "--mise",
	"--no-mise", "--env", "--property", "--exec-start-pre", "--exec-start-post", "--exec-stop",
	"--exec-reload", "--after", "--wants", "--requires", "--on-failure", "--health-http",
	"--health-tcp", "--health-command", "--health-interval", "--health-timeout", "--health-retries",
	"--health-retry-delay", "--health-start-period", "--health-action", "--no-health",
	"--start-limit-interval", "--start-limit-burst", "--command", "--type", "--pid-file",
	"--timezone", "--instances", "--help", "-h",
}

func printCompletion(w io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New("completion requires one shell: bash, zsh, or fish")
	}
	var script string
	switch args[0] {
	case "bash":
		script = bashCompletionScript
	case "zsh":
		script = zshCompletionScript
	case "fish":
		script = fishCompletionScript
	default:
		return fmt.Errorf("unsupported completion shell %q; use bash, zsh, or fish", args[0])
	}
	_, err := io.WriteString(w, script)
	return err
}

func runCompletionQuery(args []string, stdout io.Writer) error {
	system := len(args) > 0 && args[0] == "--system"
	unitDir := "/etc/systemd/system"
	if !system {
		var err error
		unitDir, err = userUnitDir()
		if err != nil {
			return nil
		}
	}
	a := &application{stdout: stdout, stderr: io.Discard, unitDir: unitDir, system: system}
	for _, candidate := range a.completionCandidates(args) {
		fmt.Fprintln(stdout, candidate)
	}
	return nil
}

func (a *application) completionCandidates(args []string) []string {
	current := ""
	if len(args) > 0 {
		current = args[len(args)-1]
		args = args[:len(args)-1]
	}
	if len(args) > 0 && args[0] == "--system" {
		args = args[1:]
	} else if len(args) == 0 && current == "--system" {
		return []string{"--system"}
	}
	if len(args) == 0 {
		globals := []string{"--system", "--help", "-h", "--version", "-v"}
		return filterCompletionCandidates(append(globals, completionCommands...), current)
	}
	command := args[0]
	commandArgs := args[1:]
	if command == "help" {
		return filterCompletionCandidates(completionCommands, current)
	}
	if values := completionValueCandidates(commandArgs, current); values != nil {
		return values
	}
	if strings.HasPrefix(current, "-") {
		return filterCompletionCandidates(completionOptions(command), current)
	}
	switch command {
	case "start":
		if len(commandArgs) == 0 {
			return filterCompletionCandidates(append(a.completionServiceNames(false), startCompletionOptions...), current)
		}
	case "schedule":
		return filterCompletionCandidates(scheduleCompletionOptions, current)
	case "run", "status", "show", "describe", "cat", "edit", "security", "health", "history":
		return filterCompletionCandidates(a.completionServiceNames(false), current)
	case "logs":
		return filterCompletionCandidates(a.completionServiceNames(true), current)
	case "signal", "kill":
		if len(commandArgs) == 0 {
			return filterCompletionCandidates(signalCompletionValues, strings.ToUpper(current))
		}
		return filterCompletionCandidates(a.completionServiceNames(true), current)
	case "scale":
		if len(commandArgs) == 0 {
			var names []string
			for _, name := range a.completionServiceNames(false) {
				if a.isTemplate(name) {
					names = append(names, name)
				}
			}
			return filterCompletionCandidates(names, current)
		}
	case "restart", "stop", "reload", "enable", "disable", "delete", "remove", "rm":
		return filterCompletionCandidates(a.completionServiceNames(true), current)
	case "set", "unset", "rollback":
		if len(commandArgs) == 0 {
			return filterCompletionCandidates(a.completionServiceNames(false), current)
		}
		return filterCompletionCandidates(completionOptions(command), current)
	case "clone":
		if len(commandArgs) < 2 {
			return filterCompletionCandidates(a.completionServiceNames(false), current)
		}
	case "export":
		return filterCompletionCandidates(a.completionServiceNames(true), current)
	case "completion":
		return filterCompletionCandidates([]string{"bash", "zsh", "fish"}, current)
	}
	return nil
}

func (a *application) completionServiceNames(includeAll bool) []string {
	names, err := a.managedNames()
	if err != nil {
		return nil
	}
	if includeAll {
		names = append(names, "all")
	}
	return names
}

func completionOptions(command string) []string {
	switch command {
	case "start":
		return startCompletionOptions
	case "schedule":
		return scheduleCompletionOptions
	case "logs":
		return []string{"--follow", "-f", "--lines", "-n", "--since", "--until", "--grep", "--boot", "--health", "--output", "-o", "--help", "-h"}
	case "list", "ls", "status-all":
		return []string{"--json", "--watch", "-w", "--help", "-h"}
	case "signal", "kill":
		return []string{"--all-processes", "--help", "-h"}
	case "restart":
		return []string{"--update", "--help", "-h"}
	case "delete", "remove", "rm":
		return []string{"--yes", "--help", "-h"}
	case "set", "unset":
		return configCompletionOptions
	case "rollback":
		return []string{"--revision", "--help", "-h"}
	case "apply":
		return []string{"--dry-run", "--help", "-h"}
	case "import":
		return []string{"--name", "--help", "-h"}
	default:
		return []string{"--help", "-h"}
	}
}

func completionValueCandidates(args []string, current string) []string {
	if len(args) == 0 {
		return nil
	}
	previous := args[len(args)-1]
	var values []string
	switch previous {
	case "--restart":
		values = []string{"no", "on-success", "on-failure", "on-abnormal", "on-abort", "on-watchdog", "always"}
	case "--health-action":
		values = []string{"none", "restart", "stop"}
	case "--sandbox":
		values = []string{"basic", "strict"}
	case "--kill-mode":
		values = []string{"control-group", "mixed", "process", "none"}
	case "--type":
		values = serviceTypes
	case "--timezone":
		values = []string{"UTC", "Asia/Singapore", "Asia/Shanghai", "Asia/Tokyo", "Asia/Hong_Kong", "Europe/London", "America/New_York"}
	case "--output", "-o":
		values = []string{"short", "short-iso", "short-iso-precise", "cat", "json", "json-pretty"}
	default:
		return nil
	}
	return filterCompletionCandidates(values, current)
}

func filterCompletionCandidates(values []string, prefix string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] && strings.HasPrefix(value, prefix) {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

const bashCompletionScript = `# bash completion for spm
_spm_completion() {
    local -a candidates
    mapfile -t candidates < <(command spm __complete "${COMP_WORDS[@]:1:$COMP_CWORD}" 2>/dev/null)
    COMPREPLY=("${candidates[@]}")
}
complete -o default -o bashdefault -F _spm_completion spm
`

const zshCompletionScript = `#compdef spm
_spm_completion() {
    local -a candidates
    local output
    output=$(command spm __complete "${(@)words[2,$CURRENT]}" 2>/dev/null)
    if [[ -n $output ]]; then
        candidates=("${(@f)output}")
        compadd -a candidates
    else
        _files
    fi
}
compdef _spm_completion spm
`

const fishCompletionScript = `function __spm_completion
    set -l tokens (commandline -opc)
    set -l current (commandline -ct)
    command spm __complete $tokens[2..-1] $current 2>/dev/null
end
complete -c spm -f -a '(__spm_completion)'
complete -c spm -n '__fish_seen_subcommand_from apply validate diff import' -F
`
