package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	defaultWatchInterval = 2 * time.Second
	cpuSampleInterval    = 500 * time.Millisecond
)

type listOptions struct {
	json          bool
	watch         bool
	watchInterval time.Duration
}

// listRow is one service unit in `spm list`. Template services produce one
// row per instance.
type listRow struct {
	Name          string   `json:"name"`
	Instance      *int     `json:"instance,omitempty"`
	Unit          string   `json:"unit"`
	Status        string   `json:"status"`
	ActiveState   string   `json:"active_state"`
	SubState      string   `json:"sub_state"`
	Health        string   `json:"health"`
	PID           int      `json:"pid,omitempty"`
	CPUPercent    *float64 `json:"cpu_percent"`
	MemoryBytes   *uint64  `json:"memory_bytes"`
	Restarts      int      `json:"restarts"`
	Enabled       string   `json:"enabled"`
	UptimeSeconds *int64   `json:"uptime_seconds"`
	Schedule      string   `json:"schedule,omitempty"`
}

type cpuSample struct {
	at    time.Time
	usage map[string]uint64
}

func parseList(args []string) (listOptions, error) {
	options := listOptions{}
	for _, arg := range args {
		key, value, hasValue := strings.Cut(arg, "=")
		switch key {
		case "--json":
			if hasValue {
				return options, errors.New("--json does not accept a value")
			}
			options.json = true
		case "--watch", "-w":
			options.watch = true
			options.watchInterval = defaultWatchInterval
			if hasValue {
				interval, err := time.ParseDuration(value)
				if err != nil || interval < 500*time.Millisecond {
					return options, fmt.Errorf("invalid watch interval %q; use a duration of at least 500ms", value)
				}
				options.watchInterval = interval
			}
		default:
			return options, fmt.Errorf("unknown list option %q", arg)
		}
	}
	if options.json && options.watch {
		return options, errors.New("--json cannot be combined with --watch")
	}
	return options, nil
}

func (a *application) list() error {
	return a.listWith(listOptions{})
}

func (a *application) listWith(options listOptions) error {
	if !options.watch {
		rows, err := a.collectListRows(nil)
		if err != nil {
			return err
		}
		return a.printListRows(rows, options.json)
	}
	var previous *cpuSample
	for iteration := 0; a.watchLimit == 0 || iteration < a.watchLimit; iteration++ {
		if iteration > 0 {
			a.pause(options.watchInterval)
		}
		rows, sample, err := a.collectListRowsWithSample(previous)
		if err != nil {
			return err
		}
		previous = sample
		if supportsColor(a.stdout) {
			fmt.Fprint(a.stdout, "\x1b[H\x1b[2J")
		}
		fmt.Fprintf(a.stdout, "spm list, every %s, %s\n\n", options.watchInterval, time.Now().Format("2006-01-02 15:04:05 MST"))
		if err := a.printListRows(rows, false); err != nil {
			return err
		}
	}
	return nil
}

func (a *application) pause(duration time.Duration) {
	if a.sleep != nil {
		a.sleep(duration)
		return
	}
	time.Sleep(duration)
}

// collectListRows reads all rows. Without a previous sample it takes two CPU
// samples so that one-shot `spm list` can still show CPU usage.
func (a *application) collectListRows(previous *cpuSample) ([]listRow, error) {
	rows, _, err := a.collectListRowsWithSample(previous)
	return rows, err
}

func (a *application) collectListRowsWithSample(previous *cpuSample) ([]listRow, *cpuSample, error) {
	names, err := a.managedNames()
	if err != nil {
		return nil, nil, err
	}
	type entry struct {
		name     string
		unit     string
		instance *int
	}
	var entries []entry
	var units []string
	for _, name := range names {
		if a.isTemplate(name) {
			for index, unit := range a.serviceUnits(name) {
				instance := index
				entries = append(entries, entry{name: name, unit: unit, instance: &instance})
				units = append(units, unit)
			}
			continue
		}
		entries = append(entries, entry{name: name, unit: unitName(name)})
		units = append(units, unitName(name))
	}
	states := a.readUnitStates(units)
	sample := newCPUSample(states)
	if previous == nil && anyActive(states) {
		first := sample
		a.pause(cpuSampleInterval)
		states = a.readUnitStates(units)
		sample = newCPUSample(states)
		previous = first
	}
	scheduleStates := a.readScheduleStates(names)
	health := a.readHealthStates(names)
	bootUptime := readBootUptime()
	rows := make([]listRow, 0, len(entries))
	for _, item := range entries {
		state := states[item.unit]
		cpu := cpuPercent(previous, sample, item.unit, state)
		if scheduleState, ok := scheduleStates[item.name]; ok && item.instance == nil {
			state.active = scheduleState.active
			state.sub = scheduleState.sub
			state.enabled = scheduleState.enabled
			state.activeSince = scheduleState.activeSince
		}
		row := listRow{
			Name:        item.name,
			Instance:    item.instance,
			Unit:        item.unit,
			Status:      valueOr(formatStatus(state), "unknown"),
			ActiveState: state.active,
			SubState:    state.sub,
			Health:      health[item.name],
			CPUPercent:  cpu,
			Enabled:     valueOr(state.enabled, "unknown"),
			Schedule:    a.readSchedule(item.name),
		}
		row.PID, _ = strconv.Atoi(state.pid)
		row.Restarts, _ = strconv.Atoi(state.restarts)
		if memory, err := strconv.ParseUint(state.memory, 10, 64); err == nil {
			row.MemoryBytes = &memory
		}
		if uptime, ok := uptimeDuration(bootUptime, state.activeSince); ok && state.active == "active" {
			seconds := int64(uptime / time.Second)
			row.UptimeSeconds = &seconds
		}
		rows = append(rows, row)
	}
	return rows, sample, nil
}

func newCPUSample(states map[string]serviceState) *cpuSample {
	sample := &cpuSample{at: time.Now(), usage: make(map[string]uint64, len(states))}
	for unit, state := range states {
		if usage, err := strconv.ParseUint(state.cpuNSec, 10, 64); err == nil {
			sample.usage[unit] = usage
		}
	}
	return sample
}

func anyActive(states map[string]serviceState) bool {
	for _, state := range states {
		if state.active == "active" {
			return true
		}
	}
	return false
}

// cpuPercent returns the CPU usage between two samples, where 100 means one
// fully used core. It returns nil when usage cannot be computed.
func cpuPercent(previous, current *cpuSample, unit string, state serviceState) *float64 {
	if state.active != "active" || previous == nil || current == nil {
		return nil
	}
	before, okBefore := previous.usage[unit]
	after, okAfter := current.usage[unit]
	elapsed := current.at.Sub(previous.at)
	if !okBefore || !okAfter || elapsed <= 0 {
		return nil
	}
	value := 0.0
	if after > before {
		value = float64(after-before) / float64(elapsed.Nanoseconds()) * 100
	}
	value = float64(int64(value*10+0.5)) / 10
	return &value
}

func (a *application) printListRows(rows []listRow, asJSON bool) error {
	if asJSON {
		if rows == nil {
			rows = []listRow{}
		}
		encoder := json.NewEncoder(a.stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.stdout, "No services managed by spm.")
		return nil
	}
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tHEALTH\tPID\tCPU\tMEMORY\tRESTARTS\tENABLED\tUPTIME\tSCHEDULE")
	for _, row := range rows {
		name := row.Name
		if row.Instance != nil {
			name += "@" + strconv.Itoa(*row.Instance)
		}
		pid := "-"
		if row.PID > 0 {
			pid = strconv.Itoa(row.PID)
		}
		cpu := "-"
		if row.CPUPercent != nil {
			cpu = strconv.FormatFloat(*row.CPUPercent, 'f', 1, 64) + "%"
		}
		memory := "-"
		if row.MemoryBytes != nil {
			memory = formatBytes(strconv.FormatUint(*row.MemoryBytes, 10))
		}
		uptime := "-"
		if row.UptimeSeconds != nil {
			uptime = (time.Duration(*row.UptimeSeconds) * time.Second).String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", name, row.Status, row.Health, pid, cpu, memory, row.Restarts, row.Enabled, uptime, valueOr(row.Schedule, "-"))
	}
	return w.Flush()
}

func uptimeDuration(bootUptime time.Duration, activeSinceUSec uint64) (time.Duration, bool) {
	if bootUptime <= 0 || activeSinceUSec == 0 {
		return 0, false
	}
	uptime := bootUptime - time.Duration(activeSinceUSec)*time.Microsecond
	if uptime < 0 {
		return 0, false
	}
	return uptime.Truncate(time.Second), true
}
