package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const portableScheduleVersion = 1

type portableScheduleRecord struct {
	Version          int       `json:"version"`
	Name             string    `json:"name"`
	Command          []string  `json:"command"`
	Cwd              string    `json:"cwd"`
	Env              []string  `json:"env,omitempty"`
	EnvFile          string    `json:"env_file,omitempty"`
	Schedule         string    `json:"schedule,omitempty"`
	Timezone         string    `json:"timezone,omitempty"`
	TimerOnBoot      string    `json:"timer_on_boot,omitempty"`
	TimerInterval    string    `json:"timer_interval,omitempty"`
	TimerRandomDelay string    `json:"timer_randomized_delay,omitempty"`
	Persistent       bool      `json:"persistent,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type portableScheduleState struct {
	Version         int       `json:"version"`
	IntervalAnchor  time.Time `json:"interval_anchor,omitempty"`
	LastCheckedAt   time.Time `json:"last_checked_at,omitempty"`
	LastScheduledAt time.Time `json:"last_scheduled_at,omitempty"`
	LastScheduleKey string    `json:"last_schedule_key,omitempty"`
	LastStartedAt   time.Time `json:"last_started_at,omitempty"`
	LastFinishedAt  time.Time `json:"last_finished_at,omitempty"`
	LastExitCode    *int      `json:"last_exit_code,omitempty"`
}

type cronExpression struct {
	second, minute, hour, day, month, weekday cronField
	hasSeconds                                bool
	dayWildcard, weekdayWildcard              bool
}

type cronField struct {
	minimum int
	values  []bool
}

func parseCronExpression(expression string) (cronExpression, error) {
	var result cronExpression
	if strings.ContainsAny(expression, "\r\n") {
		return result, errors.New("expression cannot contain a newline")
	}
	fields := strings.Fields(expression)
	if len(fields) != 5 && len(fields) != 6 {
		return result, fmt.Errorf("expected 5 or 6 fields, got %d", len(fields))
	}
	offset := 0
	if len(fields) == 6 {
		result.hasSeconds = true
		offset = 1
		field, err := parsePortableCronField(fields[0], 0, 59, false)
		if err != nil {
			return result, fmt.Errorf("second: %w", err)
		}
		result.second = field
	} else {
		result.second = cronField{minimum: 0, values: make([]bool, 60)}
		result.second.values[0] = true
	}
	definitions := []struct {
		name       string
		raw        string
		minimum    int
		maximum    int
		normalize7 bool
		target     *cronField
	}{
		{"minute", fields[offset], 0, 59, false, &result.minute},
		{"hour", fields[offset+1], 0, 23, false, &result.hour},
		{"day of month", fields[offset+2], 1, 31, false, &result.day},
		{"month", fields[offset+3], 1, 12, false, &result.month},
		{"day of week", fields[offset+4], 0, 7, true, &result.weekday},
	}
	for _, definition := range definitions {
		field, err := parsePortableCronField(definition.raw, definition.minimum, definition.maximum, definition.normalize7)
		if err != nil {
			return result, fmt.Errorf("%s: %w", definition.name, err)
		}
		*definition.target = field
	}
	result.dayWildcard = fields[offset+2] == "*"
	result.weekdayWildcard = fields[offset+4] == "*"
	return result, nil
}

func parsePortableCronField(raw string, minimum, maximum int, normalizeSunday bool) (cronField, error) {
	fieldMaximum := maximum
	if normalizeSunday {
		fieldMaximum = 6
	}
	result := cronField{minimum: minimum, values: make([]bool, fieldMaximum-minimum+1)}
	if raw == "" {
		return result, errors.New("field is empty")
	}
	for _, item := range strings.Split(raw, ",") {
		if item == "" {
			return result, fmt.Errorf("invalid list %q", raw)
		}
		base, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			if strings.Contains(stepText, "/") {
				return result, fmt.Errorf("invalid step %q", item)
			}
			var err error
			step, err = strconv.Atoi(stepText)
			if err != nil || step < 1 || step > maximum-minimum+1 {
				return result, fmt.Errorf("invalid step %q", item)
			}
		}
		start, end := minimum, maximum
		switch {
		case base == "*":
		case strings.Contains(base, "-"):
			bounds := strings.Split(base, "-")
			if len(bounds) != 2 {
				return result, fmt.Errorf("invalid range %q", item)
			}
			var err error
			start, err = parseCronNumber(bounds[0], minimum, maximum)
			if err != nil {
				return result, err
			}
			end, err = parseCronNumber(bounds[1], minimum, maximum)
			if err != nil {
				return result, err
			}
			if start > end {
				return result, fmt.Errorf("range %q starts after it ends", base)
			}
		default:
			if hasStep {
				return result, fmt.Errorf("step %q requires * or a range", item)
			}
			value, err := parseCronNumber(base, minimum, maximum)
			if err != nil {
				return result, err
			}
			start, end = value, value
		}
		for value := start; value <= end; value += step {
			if normalizeSunday && value == 7 {
				value = 0
				result.values[value-result.minimum] = true
				value = 7
				continue
			}
			result.values[value-result.minimum] = true
		}
	}
	return result, nil
}

func (field cronField) matches(value int) bool {
	index := value - field.minimum
	return index >= 0 && index < len(field.values) && field.values[index]
}

func (expression cronExpression) matches(value time.Time) bool {
	if !expression.second.matches(value.Second()) || !expression.minute.matches(value.Minute()) || !expression.hour.matches(value.Hour()) || !expression.month.matches(int(value.Month())) {
		return false
	}
	dayMatch := expression.day.matches(value.Day())
	weekdayMatch := expression.weekday.matches(int(value.Weekday()))
	switch {
	case expression.dayWildcard && expression.weekdayWildcard:
		return true
	case expression.dayWildcard:
		return weekdayMatch
	case expression.weekdayWildcard:
		return dayMatch
	default:
		return dayMatch || weekdayMatch
	}
}

func (expression cronExpression) resolution() time.Duration {
	if expression.hasSeconds {
		return time.Second
	}
	return time.Minute
}

func previousCronOccurrence(expression cronExpression, now, notBefore time.Time, location *time.Location) (time.Time, bool) {
	localNow := now.In(location).Truncate(expression.resolution())
	localStart := notBefore.In(location)
	for day := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 12, 0, 0, 0, location); !day.Before(time.Date(localStart.Year(), localStart.Month(), localStart.Day(), 12, 0, 0, 0, location)); day = day.AddDate(0, 0, -1) {
		if !expression.month.matches(int(day.Month())) {
			continue
		}
		dayMatch := expression.day.matches(day.Day())
		weekdayMatch := expression.weekday.matches(int(day.Weekday()))
		if expression.dayWildcard && !weekdayMatch || expression.weekdayWildcard && !dayMatch || !expression.dayWildcard && !expression.weekdayWildcard && !dayMatch && !weekdayMatch {
			continue
		}
		limitHour, limitMinute, limitSecond := 23, 59, 59
		if day.Year() == localNow.Year() && day.YearDay() == localNow.YearDay() {
			limitHour, limitMinute, limitSecond = localNow.Hour(), localNow.Minute(), localNow.Second()
		}
		for hour := limitHour; hour >= 0; hour-- {
			if !expression.hour.matches(hour) {
				continue
			}
			minuteLimit := 59
			if hour == limitHour {
				minuteLimit = limitMinute
			}
			for minute := minuteLimit; minute >= 0; minute-- {
				if !expression.minute.matches(minute) {
					continue
				}
				secondLimit := 59
				if hour == limitHour && minute == minuteLimit {
					secondLimit = limitSecond
				}
				for second := secondLimit; second >= 0; second-- {
					if !expression.second.matches(second) {
						continue
					}
					candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, second, 0, location)
					if candidate.After(now) || !candidate.After(notBefore) || !expression.matches(candidate) {
						continue
					}
					return candidate, true
				}
			}
		}
	}
	return time.Time{}, false
}

func nextCronOccurrence(expression cronExpression, after time.Time, location *time.Location) (time.Time, bool) {
	localAfter := after.In(location)
	lastDay := localAfter.AddDate(5, 0, 1)
	for day := time.Date(localAfter.Year(), localAfter.Month(), localAfter.Day(), 12, 0, 0, 0, location); day.Before(lastDay); day = day.AddDate(0, 0, 1) {
		if !expression.month.matches(int(day.Month())) {
			continue
		}
		dayMatch := expression.day.matches(day.Day())
		weekdayMatch := expression.weekday.matches(int(day.Weekday()))
		if expression.dayWildcard && !weekdayMatch || expression.weekdayWildcard && !dayMatch || !expression.dayWildcard && !expression.weekdayWildcard && !dayMatch && !weekdayMatch {
			continue
		}
		startHour, startMinute, startSecond := 0, 0, 0
		if day.Year() == localAfter.Year() && day.YearDay() == localAfter.YearDay() {
			startHour, startMinute, startSecond = localAfter.Hour(), localAfter.Minute(), localAfter.Second()
		}
		for hour := startHour; hour < 24; hour++ {
			if !expression.hour.matches(hour) {
				continue
			}
			minuteStart := 0
			if hour == startHour {
				minuteStart = startMinute
			}
			for minute := minuteStart; minute < 60; minute++ {
				if !expression.minute.matches(minute) {
					continue
				}
				secondStart := 0
				if hour == startHour && minute == minuteStart {
					secondStart = startSecond
				}
				for second := secondStart; second < 60; second++ {
					if !expression.second.matches(second) {
						continue
					}
					candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, second, 0, location)
					if !candidate.After(after) || !expression.matches(candidate) {
						continue
					}
					return candidate, true
				}
			}
		}
	}
	return time.Time{}, false
}

func loadScheduleLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid time zone %q: %w", name, err)
	}
	return location, nil
}

func parsePortableDuration(value string) (time.Duration, error) {
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
		return duration, nil
	}
	units := []struct {
		name       string
		multiplier time.Duration
	}{{"milliseconds", time.Millisecond}, {"millisecond", time.Millisecond}, {"minutes", time.Minute}, {"minute", time.Minute}, {"seconds", time.Second}, {"second", time.Second}, {"hours", time.Hour}, {"hour", time.Hour}, {"weeks", 7 * 24 * time.Hour}, {"week", 7 * 24 * time.Hour}, {"days", 24 * time.Hour}, {"day", 24 * time.Hour}, {"msec", time.Millisecond}, {"min", time.Minute}, {"ms", time.Millisecond}, {"sec", time.Second}, {"s", time.Second}, {"h", time.Hour}, {"d", 24 * time.Hour}, {"w", 7 * 24 * time.Hour}}
	remaining := value
	var total time.Duration
	for remaining != "" {
		numberEnd := 0
		dot := false
		for numberEnd < len(remaining) {
			character := remaining[numberEnd]
			if character >= '0' && character <= '9' {
				numberEnd++
				continue
			}
			if character == '.' && !dot && numberEnd > 0 {
				dot = true
				numberEnd++
				continue
			}
			break
		}
		if numberEnd == 0 || remaining[numberEnd-1] == '.' {
			return 0, fmt.Errorf("invalid portable duration %q", value)
		}
		amount, err := strconv.ParseFloat(remaining[:numberEnd], 64)
		if err != nil || amount <= 0 {
			return 0, fmt.Errorf("invalid portable duration %q", value)
		}
		remaining = remaining[numberEnd:]
		matched := false
		for _, unit := range units {
			if strings.HasPrefix(remaining, unit.name) {
				total += time.Duration(amount * float64(unit.multiplier))
				remaining = remaining[len(unit.name):]
				matched = true
				break
			}
		}
		if !matched {
			return 0, fmt.Errorf("invalid portable duration %q", value)
		}
	}
	if total <= 0 {
		return 0, fmt.Errorf("invalid portable duration %q", value)
	}
	return total, nil
}

func validatePortableSchedule(record portableScheduleRecord) error {
	if record.Version != portableScheduleVersion || record.Name == "" || len(record.Command) == 0 {
		return errors.New("invalid scheduled task record")
	}
	if record.Schedule == "" && record.TimerOnBoot == "" && record.TimerInterval == "" {
		return errors.New("schedule requires a cron, boot, or interval trigger")
	}
	if record.Schedule != "" {
		if _, err := parseCronExpression(record.Schedule); err != nil {
			return fmt.Errorf("invalid schedule: %w", err)
		}
		if _, err := loadScheduleLocation(record.Timezone); err != nil {
			return err
		}
	} else if record.Timezone != "" {
		return errors.New("timezone requires a cron schedule")
	}
	for name, value := range map[string]string{"timer_on_boot": record.TimerOnBoot, "timer_interval": record.TimerInterval, "timer_randomized_delay": record.TimerRandomDelay} {
		if value != "" {
			duration, err := parsePortableDuration(value)
			if err != nil {
				return fmt.Errorf("invalid %s: %w", name, err)
			}
			if name == "timer_interval" && duration < time.Second {
				return errors.New("timer_interval must be at least 1s")
			}
		}
	}
	if record.Persistent && record.Schedule == "" {
		return errors.New("persistent requires a cron schedule")
	}
	return nil
}

func writePortableSchedule(path string, record portableScheduleRecord) error {
	if record.Version == 0 {
		record.Version = portableScheduleVersion
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if err := validatePortableSchedule(record); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

func preparePortableScheduleUpdate(path, stateDir string, record *portableScheduleRecord) error {
	previous, err := readPortableSchedule(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if samePortableSchedule(previous, *record) {
		record.CreatedAt = previous.CreatedAt
		return nil
	}
	if err := os.RemoveAll(stateDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reset changed schedule state: %w", err)
	}
	return nil
}

func samePortableSchedule(left, right portableScheduleRecord) bool {
	return left.Schedule == right.Schedule && left.Timezone == right.Timezone && left.TimerOnBoot == right.TimerOnBoot && left.TimerInterval == right.TimerInterval && left.TimerRandomDelay == right.TimerRandomDelay && left.Persistent == right.Persistent
}

func readPortableSchedule(path string) (portableScheduleRecord, error) {
	var record portableScheduleRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	return record, validatePortableSchedule(record)
}

func readPortableScheduleState(path string) portableScheduleState {
	state := portableScheduleState{Version: portableScheduleVersion}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.Version != portableScheduleVersion {
		state = portableScheduleState{Version: portableScheduleVersion}
	}
	return state
}

func writePortableScheduleState(path string, state portableScheduleState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

func latestDueOccurrence(record portableScheduleRecord, state portableScheduleState, now time.Time, activation string) (time.Time, bool, error) {
	var due time.Time
	if record.Schedule != "" {
		expression, err := parseCronExpression(record.Schedule)
		if err != nil {
			return time.Time{}, false, err
		}
		location, err := loadScheduleLocation(record.Timezone)
		if err != nil {
			return time.Time{}, false, err
		}
		current := now.In(location).Truncate(expression.resolution())
		if expression.matches(current) {
			due = current
		} else if record.Persistent || activation == "daemon" {
			start := state.LastCheckedAt
			if start.IsZero() {
				start = record.CreatedAt
			}
			if activation == "daemon" && !record.Persistent {
				start = now.Add(-2 * time.Minute)
			}
			// Five years covers the longest gap in the supported grammar, including
			// leap-day schedules, while bounding recovery from corrupt state.
			minimum := now.AddDate(-5, 0, 0)
			if start.Before(minimum) {
				start = minimum
			}
			if candidate, ok := previousCronOccurrence(expression, current, start, location); ok {
				due = candidate
			}
		}
	}
	if record.TimerInterval != "" {
		interval, err := parsePortableDuration(record.TimerInterval)
		if err != nil {
			return time.Time{}, false, err
		}
		anchor := state.IntervalAnchor
		if anchor.IsZero() {
			anchor = record.CreatedAt
		}
		if !now.Before(anchor.Add(interval)) {
			steps := int64(now.Sub(anchor) / interval)
			candidate := anchor.Add(time.Duration(steps) * interval)
			if candidate.After(due) {
				due = candidate
			}
		}
	}
	if activation == "boot" && record.TimerOnBoot != "" && now.After(due) {
		due = now.Truncate(time.Second)
	}
	if due.IsZero() || !state.LastScheduledAt.IsZero() && !due.After(state.LastScheduledAt) {
		return time.Time{}, false, nil
	}
	return due, true, nil
}

func runPortableSchedule(args []string, stdout, stderr io.Writer) error {
	var configPath, stateDir, logPath, activation string
	force := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--config", "--state-dir", "--log", "--activation":
			if index+1 >= len(args) {
				return fmt.Errorf("%s requires a value", args[index])
			}
			value := args[index+1]
			index++
			switch args[index-1] {
			case "--config":
				configPath = value
			case "--state-dir":
				stateDir = value
			case "--log":
				logPath = value
			case "--activation":
				activation = value
			}
		case "--force":
			force = true
		default:
			return fmt.Errorf("unknown scheduled runner option %q", args[index])
		}
	}
	if !filepath.IsAbs(configPath) || !filepath.IsAbs(stateDir) {
		return errors.New("scheduled runner requires absolute --config and --state-dir paths")
	}
	record, err := readPortableSchedule(configPath)
	if err != nil {
		return fmt.Errorf("read scheduled task: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(stateDir, "run.lock")
	locked, err := acquirePortableScheduleLock(lockPath)
	if err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer os.RemoveAll(lockPath)
	statePath := filepath.Join(stateDir, "schedule-state.json")
	state := readPortableScheduleState(statePath)
	if activation == "boot" && record.TimerOnBoot != "" && !force {
		delay, err := parsePortableDuration(record.TimerOnBoot)
		if err != nil {
			return err
		}
		time.Sleep(delay)
	}
	now := time.Now()
	due, shouldRun, err := latestDueOccurrence(record, state, now, activation)
	if err != nil {
		return err
	}
	if !force {
		state.LastCheckedAt = now
	}
	if !force && !shouldRun {
		return writePortableScheduleState(statePath, state)
	}
	if force {
		due = now
	} else if shouldRun {
		key, keyErr := portableOccurrenceKey(record, due)
		if keyErr != nil {
			return keyErr
		}
		if key == state.LastScheduleKey {
			shouldRun = false
		} else {
			state.LastScheduleKey = key
		}
	}
	if !force && !shouldRun {
		return writePortableScheduleState(statePath, state)
	}
	if record.TimerRandomDelay != "" && !force {
		maximum, err := parsePortableDuration(record.TimerRandomDelay)
		if err != nil {
			return err
		}
		if maximum > 0 {
			time.Sleep(time.Duration(rand.Int63n(int64(maximum) + 1)))
		}
	}
	if !force {
		state.LastScheduledAt = due
	}
	state.LastStartedAt = time.Now()
	if state.IntervalAnchor.IsZero() {
		state.IntervalAnchor = record.CreatedAt
	}
	if err := writePortableScheduleState(statePath, state); err != nil {
		return err
	}
	output, closeOutput, err := scheduledOutput(stdout, stderr, logPath)
	if err != nil {
		return err
	}
	defer closeOutput()
	command := exec.Command(record.Command[0], record.Command[1:]...)
	command.Dir = record.Cwd
	command.Env = scheduledEnvironment(record)
	command.Stdout, command.Stderr = output, output
	err = command.Run()
	exitCode := 0
	if err != nil {
		exitCode = 1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	state.LastFinishedAt = time.Now()
	state.LastExitCode = &exitCode
	if stateErr := writePortableScheduleState(statePath, state); stateErr != nil {
		return errors.Join(err, stateErr)
	}
	return err
}

func acquirePortableScheduleLock(path string) (bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := os.Mkdir(path, 0o700); err == nil {
			_ = os.WriteFile(filepath.Join(path, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
			return true, nil
		} else if !os.IsExist(err) {
			return false, err
		}
		data, _ := os.ReadFile(filepath.Join(path, "pid"))
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 && portableProcessAlive(pid) {
			return false, nil
		}
		if err := os.RemoveAll(path); err != nil {
			return false, err
		}
	}
	return false, nil
}

func portableOccurrenceKey(record portableScheduleRecord, due time.Time) (string, error) {
	if record.Schedule != "" {
		location, err := loadScheduleLocation(record.Timezone)
		if err != nil {
			return "", err
		}
		expression, err := parseCronExpression(record.Schedule)
		if err != nil {
			return "", err
		}
		format := "2006-01-02T15:04"
		if expression.hasSeconds {
			format = "2006-01-02T15:04:05"
		}
		return "cron:" + due.In(location).Format(format), nil
	}
	return "timer:" + due.UTC().Format(time.RFC3339Nano), nil
}

func scheduledOutput(stdout, stderr io.Writer, path string) (io.Writer, func(), error) {
	if path == "" {
		return io.MultiWriter(stdout, stderr), func() {}, nil
	}
	file, err := openRotatingLog(path, defaultLogMaxBytes, defaultLogBackups)
	if err != nil {
		return nil, func() {}, err
	}
	return file, func() { _ = file.Close() }, nil
}

func scheduledEnvironment(record portableScheduleRecord) []string {
	environment := append([]string(nil), os.Environ()...)
	if record.EnvFile != "" {
		if file, err := os.Open(record.EnvFile); err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if key, value, ok := strings.Cut(line, "="); ok {
					environment = setPortableEnv(environment, key, strings.Trim(value, "\"'"))
				}
			}
			_ = file.Close()
		}
	}
	for _, item := range record.Env {
		if key, value, ok := strings.Cut(item, "="); ok {
			environment = setPortableEnv(environment, key, value)
		}
	}
	return environment
}

func setPortableEnv(environment []string, key, value string) []string {
	prefix := key + "="
	for index, item := range environment {
		itemKey, _, _ := strings.Cut(item, "=")
		if itemKey == key || runtime.GOOS == "windows" && strings.EqualFold(itemKey, key) {
			environment[index] = prefix + value
			return environment
		}
	}
	return append(environment, prefix+value)
}

func formatScheduleTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func scheduleExitCode(value *int) string {
	if value == nil {
		return "-"
	}
	return strconv.Itoa(*value)
}
