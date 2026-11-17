package supervisor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eyvant/spm/internal/runtime/common"
)

// StateVersion is the format version of config.json and state.json.
const StateVersion = 1

// Config is a service's config.json.
type Config struct {
	Version          int      `json:"version"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Cwd              string   `json:"cwd"`
	Command          []string `json:"command"`
	Env              []string `json:"env,omitempty"`
	EnvFile          string   `json:"env_file,omitempty"`
	Restart          string   `json:"restart"`
	RestartSec       string   `json:"restart_sec"`
	Enabled          bool     `json:"enabled"`
	RunAs            string   `json:"run_as,omitempty"`
	After            []string `json:"after,omitempty"`
	Wants            []string `json:"wants,omitempty"`
	Requires         []string `json:"requires,omitempty"`
	Mode             string   `json:"mode,omitempty"`
	Schedule         string   `json:"schedule,omitempty"`
	Timezone         string   `json:"timezone,omitempty"`
	TimerOnBoot      string   `json:"timer_on_boot,omitempty"`
	TimerInterval    string   `json:"timer_interval,omitempty"`
	TimerRandomDelay string   `json:"timer_randomized_delay,omitempty"`
	Persistent       bool     `json:"persistent,omitempty"`
}

// State is a service's state.json, written by the supervisor.
type State struct {
	Version       int       `json:"version"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	SupervisorPID int       `json:"supervisor_pid,omitempty"`
	PID           int       `json:"pid,omitempty"`
	Restarts      int       `json:"restarts"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	StoppedAt     time.Time `json:"stopped_at,omitempty"`
	ExitCode      *int      `json:"exit_code,omitempty"`
	Error         string    `json:"error,omitempty"`
	// Build is the BuildID of the supervisor that wrote this state.
	Build string `json:"build,omitempty"`
	// OutputHandle and OutputHolder pass the read end of the service's
	// output pipe across a Windows supervisor handoff: the detaching
	// supervisor leaves it with an output holder process, and the next
	// supervisor duplicates it from there.
	OutputHandle uint64 `json:"output_handle,omitempty"`
	OutputHolder int    `json:"output_holder,omitempty"`
}

// Control is a request the CLI drops into control.json for the supervisor.
type Control struct {
	Action      string `json:"action"`
	RequestedAt int64  `json:"requested_at"`
	// Executable is the supervisor binary to switch to on "reexec".
	Executable string `json:"executable,omitempty"`
}

// Layout locates service files below a data root. Global is set for Windows
// system services; their job objects live in the Global\ namespace so an
// elevated CLI in another session can open them.
type Layout struct {
	Root   string
	Global bool
}

func (l Layout) ServicesDir() string           { return filepath.Join(l.Root, "services") }
func (l Layout) ServiceDir(name string) string { return filepath.Join(l.ServicesDir(), name) }
func (l Layout) ConfigPath(name string) string {
	return filepath.Join(l.ServiceDir(name), "config.json")
}
func (l Layout) StatePath(name string) string { return filepath.Join(l.ServiceDir(name), "state.json") }
func (l Layout) ControlPath(name string) string {
	return filepath.Join(l.ServiceDir(name), "control.json")
}

// LogPath is the service's own output.
func (l Layout) LogPath(name string) string {
	return filepath.Join(l.ServiceDir(name), common.ServiceLogName)
}

// EventLogPath is what the supervisor records about the service.
func (l Layout) EventLogPath(name string) string {
	return filepath.Join(l.ServiceDir(name), common.EventLogName)
}

// OutputTakenPath names, during a Windows supervisor handoff, the output
// holder whose pipe the new supervisor has taken.
func (l Layout) OutputTakenPath(name string) string {
	return filepath.Join(l.ServiceDir(name), "output-taken")
}

func openEvents(l Layout, name string) *common.EventLog {
	return common.OpenEventLog(l.EventLogPath(name), nil)
}

func (l Layout) EnsureDirectories() error {
	for _, directory := range []string{l.Root, l.ServicesDir()} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
	}
	return nil
}

func (l Layout) ReadConfig(name string) (Config, error) {
	var config Config
	data, err := os.ReadFile(l.ConfigPath(name))
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, err
	}
	if config.Version != StateVersion || config.Name != name || len(config.Command) == 0 {
		return config, fmt.Errorf("invalid service configuration for %q", name)
	}
	return config, nil
}

// ReadState returns the recorded state with dead processes cleared.
func (l Layout) ReadState(name string) State {
	var state State
	data, err := os.ReadFile(l.StatePath(name))
	if err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.Name == "" {
		state.Name = name
		state.Version = StateVersion
		state.Status = "stopped"
	}
	if state.PID > 0 && !common.ProcessAlive(state.PID) {
		state.PID = 0
		if state.Status == "running" || state.Status == "starting" {
			state.Status = "stopped"
		}
	}
	if state.SupervisorPID > 0 && !common.ProcessAlive(state.SupervisorPID) {
		state.SupervisorPID = 0
	}
	return state
}

func (l Layout) WriteState(state State) error {
	if err := l.EnsureDirectories(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return common.WriteFileAtomic(l.StatePath(state.Name), append(data, '\n'), 0o600)
}

func (l Layout) TakeControl(name string) (Control, bool) {
	var control Control
	data, err := os.ReadFile(l.ControlPath(name))
	if err != nil || json.Unmarshal(data, &control) != nil {
		return control, false
	}
	if err := os.Remove(l.ControlPath(name)); err != nil {
		return control, false
	}
	return control, true
}

func (l Layout) WriteControl(name, action string) error {
	return l.WriteControlRequest(name, Control{Action: action})
}

func (l Layout) WriteControlRequest(name string, control Control) error {
	control.RequestedAt = time.Now().UnixNano()
	data, _ := json.Marshal(control)
	return common.WriteFileAtomic(l.ControlPath(name), data, 0o600)
}

func ShouldRestart(policy string, waitErr error, exitCode int) bool {
	switch policy {
	case "always":
		return true
	case "on-success":
		return exitCode == 0
	case "on-failure", "on-abnormal", "on-abort", "on-watchdog":
		return waitErr != nil || exitCode != 0
	default:
		return false
	}
}

// Environment is the process environment plus the service's env file and
// variables; later definitions win and names compare case-insensitively.
func Environment(config Config) []string {
	env := append([]string(nil), os.Environ()...)
	if config.EnvFile != "" {
		if file, err := os.Open(config.EnvFile); err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if key, value, ok := strings.Cut(line, "="); ok && key != "" {
					value = strings.Trim(value, "\"'")
					env = SetEnv(env, key, value)
				}
			}
			_ = file.Close()
		}
	}
	for _, item := range config.Env {
		if key, value, ok := strings.Cut(item, "="); ok {
			env = SetEnv(env, key, value)
		}
	}
	return env
}

func SetEnv(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	for i, item := range env {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}
