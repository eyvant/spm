//go:build windows

// Package supervisor is the process supervisor behind every Windows service
// managed by spm. It is built into spm-supervisor.exe, which stays running for
// as long as its service runs, so this package changes rarely and is
// versioned on its own (see Version).
package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

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
}

// Control is a request the CLI drops into control.json for the supervisor.
type Control struct {
	Action      string `json:"action"`
	RequestedAt int64  `json:"requested_at"`
}

// Layout locates service files below a data root. Global is set for system
// services; their job objects live in the Global\ namespace so an elevated
// CLI in another session can open them.
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
func (l Layout) LogPath(name string) string { return filepath.Join(l.ServiceDir(name), "service.log") }

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
		return config, fmt.Errorf("invalid Windows service configuration for %q", name)
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
	control := Control{Action: action, RequestedAt: time.Now().UnixNano()}
	data, _ := json.Marshal(control)
	return common.WriteFileAtomic(l.ControlPath(name), data, 0o600)
}

// Supervise runs the service command, restarts it according to its policy,
// and answers stop/restart/detach requests until the service stops.
//
// When the previous supervisor detached (see Detach), Supervise adopts the
// service process that is still running instead of starting a new one, so a
// supervisor upgrade does not interrupt the service.
func Supervise(l Layout, name string) error {
	config, err := l.ReadConfig(name)
	if err != nil {
		return fmt.Errorf("read supervisor config: %w", err)
	}
	if err := l.EnsureDirectories(); err != nil {
		return err
	}
	previous := l.ReadState(name)
	if previous.SupervisorPID > 0 && previous.SupervisorPID != os.Getpid() {
		return fmt.Errorf("service %s is already supervised by process %d", name, previous.SupervisorPID)
	}
	log, err := os.OpenFile(l.LogPath(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	state := State{Version: StateVersion, Name: name, Status: "starting", SupervisorPID: os.Getpid()}
	adopted, adoptErr := adopt(l, name, previous.PID)
	if adopted != nil {
		state.Restarts, state.StartedAt = previous.Restarts, previous.StartedAt
		fmt.Fprintf(log, "[%s] spm: supervisor %d (version %d) took over running process %d\r\n", time.Now().Format(time.RFC3339), os.Getpid(), Version, adopted.pid)
	} else if !errors.Is(adoptErr, errNothingToAdopt) {
		// A live process is waiting to be taken over. Starting a second
		// one, or terminating it, would turn a failed handoff into a restart;
		// leave it to whoever is holding its job, which restores the
		// previous supervisor.
		fmt.Fprintf(log, "[%s] spm: supervisor %d could not take over running process %d: %v\r\n", time.Now().Format(time.RFC3339), os.Getpid(), previous.PID, adoptErr)
		return fmt.Errorf("cannot take over running process %d: %w", previous.PID, adoptErr)
	}
	_ = l.WriteState(state)
	delay, err := time.ParseDuration(config.RestartSec)
	if err != nil || delay < 0 {
		delay = 3 * time.Second
	}
	for {
		current := adopted
		adopted = nil
		if current == nil {
			fmt.Fprintf(log, "[%s] spm: starting %s\r\n", time.Now().Format(time.RFC3339), strings.Join(config.Command, " "))
			started := time.Now()
			var jobErr error
			current, jobErr, err = startChild(l, name, config, log)
			if err != nil {
				state.Status, state.Error, state.PID, state.SupervisorPID = "failed", err.Error(), 0, 0
				state.StoppedAt = time.Now()
				fmt.Fprintf(log, "[%s] spm: start failed: %v\r\n", time.Now().Format(time.RFC3339), err)
				_ = log.Close()
				_ = l.WriteState(state)
				return nil
			}
			if jobErr != nil {
				fmt.Fprintf(log, "[%s] spm: warning: cannot create process job: %v\r\n", time.Now().Format(time.RFC3339), jobErr)
			}
			state.StartedAt = started
		}
		state.Status, state.Error, state.PID, state.StoppedAt, state.ExitCode = "running", "", current.pid, time.Time{}, nil
		_ = l.WriteState(state)
		action, exitCode := monitor(l, name, current)
		if action == "detach" {
			// Whoever asked for the detach holds the job open, so the
			// service keeps running until the next supervisor adopts it.
			current.release()
			state.SupervisorPID = 0
			fmt.Fprintf(log, "[%s] spm: supervisor %d detached; process %d keeps running\r\n", time.Now().Format(time.RFC3339), os.Getpid(), current.pid)
			_ = l.WriteState(state)
			return nil
		}
		current.close()
		state.PID, state.ExitCode, state.StoppedAt = 0, &exitCode, time.Now()
		fmt.Fprintf(log, "[%s] spm: process exited with code %d\r\n", time.Now().Format(time.RFC3339), exitCode)
		if action == "stop" {
			state.Status, state.SupervisorPID = "stopped", 0
			_ = log.Close()
			_ = l.WriteState(state)
			return nil
		}
		var waitErr error
		if exitCode != 0 {
			waitErr = fmt.Errorf("process exited with code %d", exitCode)
		}
		if action == "restart" || ShouldRestart(config.Restart, waitErr, exitCode) {
			state.Status = "restarting"
			state.Restarts++
			_ = l.WriteState(state)
			if action != "restart" && delay > 0 {
				time.Sleep(delay)
			}
			continue
		}
		state.SupervisorPID = 0
		if exitCode == 0 {
			state.Status = "stopped"
		} else {
			state.Status = "failed"
			state.Error = fmt.Sprintf("process exited with code %d", exitCode)
		}
		_ = log.Close()
		_ = l.WriteState(state)
		return nil
	}
}

// monitor waits for the process to exit or for a control request. stop and
// restart terminate the process first; detach leaves it running.
func monitor(l Layout, name string, current *child) (string, int) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case code := <-current.done:
			return "", int(code)
		case <-ticker.C:
			control, ok := l.TakeControl(name)
			if !ok {
				continue
			}
			switch control.Action {
			case "stop", "restart":
				current.kill()
				return control.Action, int(<-current.done)
			case "detach":
				return "detach", 0
			}
		}
	}
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

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// CreateKillJob puts pid into a job that terminates it when the returned
// handle closes, including when the supervisor itself exits.
func CreateKillJob(pid int) (syscall.Handle, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createJob := kernel32.NewProc("CreateJobObjectW")
	setJob := kernel32.NewProc("SetInformationJobObject")
	openProcess := kernel32.NewProc("OpenProcess")
	assignProcess := kernel32.NewProc("AssignProcessToJobObject")
	handle, _, createErr := createJob.Call(0, 0)
	if handle == 0 {
		return 0, createErr
	}
	job := syscall.Handle(handle)
	info := jobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	result, _, setErr := setJob.Call(handle, 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if result == 0 {
		CloseHandle(job)
		return 0, setErr
	}
	process, _, openErr := openProcess.Call(0x0100|0x0001, 0, uintptr(pid)) // SET_QUOTA | TERMINATE
	if process == 0 {
		CloseHandle(job)
		return 0, openErr
	}
	defer CloseHandle(syscall.Handle(process))
	result, _, assignErr := assignProcess.Call(handle, process)
	if result == 0 {
		CloseHandle(job)
		return 0, assignErr
	}
	return job, nil
}

func CloseHandle(handle syscall.Handle) {
	if handle == 0 {
		return
	}
	closeHandle := syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle")
	_, _, _ = closeHandle.Call(uintptr(handle))
}

func KillTree(pid int) {
	if pid <= 0 {
		return
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")
	terminateProcess := kernel32.NewProc("TerminateProcess")
	process, _, _ := openProcess.Call(0x0001, 0, uintptr(pid)) // PROCESS_TERMINATE
	if process == 0 {
		return
	}
	defer CloseHandle(syscall.Handle(process))
	_, _, _ = terminateProcess.Call(process, 1)
}
