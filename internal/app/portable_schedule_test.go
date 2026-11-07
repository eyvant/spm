package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPortableSchedulerTriggersDueTaskAndStops(t *testing.T) {
	root := localTempDir(t)
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "services", "job")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte(`{"enabled":true,"mode":"schedule"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record := portableScheduleRecord{Version: 1, Name: "job", Command: []string{"true"}, Schedule: "* * * * * *", CreatedAt: time.Now().Add(-time.Second)}
	if err := writePortableSchedule(filepath.Join(directory, "schedule.json"), record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	triggered := make(chan string, 1)
	result := make(chan error, 1)
	go func() {
		result <- runPortableScheduler(ctx, root, func(name string) error {
			triggered <- name
			cancel()
			return nil
		}, io.Discard)
	}()
	select {
	case name := <-triggered:
		if name != "job" {
			t.Fatalf("triggered %q, want job", name)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("scheduler did not trigger the due task")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("scheduler returned %v, want context cancellation", err)
	}
}

func TestPortableCronGrammarAndORRule(t *testing.T) {
	expression, err := parseCronExpression("*/15 9-17/2 1,20 * 1-5/2")
	if err != nil {
		t.Fatal(err)
	}
	location := time.FixedZone("test", 8*60*60)
	for _, test := range []struct {
		value time.Time
		want  bool
	}{
		{time.Date(2026, 11, 2, 9, 15, 0, 0, location), true},   // Monday matches weekday.
		{time.Date(2026, 11, 20, 11, 30, 0, 0, location), true}, // Day 20 matches day-of-month.
		{time.Date(2026, 11, 22, 11, 30, 0, 0, location), false},
		{time.Date(2026, 11, 20, 10, 30, 0, 0, location), false},
	} {
		if got := expression.matches(test.value); got != test.want {
			t.Errorf("matches(%s) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestPortableCronSupportsSeconds(t *testing.T) {
	expression, err := parseCronExpression("30 */5 9-17 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	location := time.FixedZone("test", 8*60*60)
	if !expression.matches(time.Date(2026, 11, 6, 9, 10, 30, 0, location)) {
		t.Fatal("six-field cron did not match its scheduled second")
	}
	if expression.matches(time.Date(2026, 11, 6, 9, 10, 31, 0, location)) {
		t.Fatal("six-field cron matched the wrong second")
	}
	next, ok := nextCronOccurrence(expression, time.Date(2026, 11, 6, 9, 10, 30, 0, location), location)
	if !ok || !next.Equal(time.Date(2026, 11, 6, 9, 15, 30, 0, location)) {
		t.Fatalf("next occurrence = %s, %v", next, ok)
	}
}

func TestFiveFieldCronRemainsMinuteBased(t *testing.T) {
	expression, err := parseCronExpression("10 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	location := time.UTC
	if !expression.matches(time.Date(2026, 11, 6, 9, 10, 0, 0, location)) || expression.matches(time.Date(2026, 11, 6, 9, 10, 1, 0, location)) {
		t.Fatal("five-field cron did not retain second-zero semantics")
	}
}

func TestNextPortableScheduleOccurrenceUsesSeconds(t *testing.T) {
	record := portableScheduleRecord{Schedule: "15 * * * * *", Timezone: "UTC", CreatedAt: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)}
	next, err := nextPortableScheduleOccurrence(record, portableScheduleState{}, time.Date(2026, 11, 6, 9, 10, 14, 500_000_000, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 6, 9, 10, 15, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next occurrence = %s, want %s", next, want)
	}
}

func TestNextPortableScheduleOccurrenceCoversIntervalAndErrors(t *testing.T) {
	created := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	record := portableScheduleRecord{CreatedAt: created, TimerInterval: "15min"}
	next, err := nextPortableScheduleOccurrence(record, portableScheduleState{}, created.Add(31*time.Minute))
	if err != nil || !next.Equal(created.Add(45*time.Minute)) {
		t.Fatalf("interval next = %s, %v", next, err)
	}
	anchor := created.Add(2 * time.Minute)
	next, err = nextPortableScheduleOccurrence(record, portableScheduleState{IntervalAnchor: anchor}, created)
	if err != nil || !next.Equal(anchor.Add(15*time.Minute)) {
		t.Fatalf("anchored next = %s, %v", next, err)
	}
	record.Schedule = "0 9 * * *"
	next, err = nextPortableScheduleOccurrence(record, portableScheduleState{}, created)
	if err != nil || next.IsZero() {
		t.Fatalf("combined next = %s, %v", next, err)
	}
	for _, invalid := range []portableScheduleRecord{{Schedule: "bad"}, {Schedule: "0 9 * * *", Timezone: "Bad/Zone"}, {TimerInterval: "bad"}} {
		if _, err := nextPortableScheduleOccurrence(invalid, portableScheduleState{}, created); err == nil {
			t.Errorf("invalid record %#v unexpectedly succeeded", invalid)
		}
	}
}

func TestPortableSchedulerCancellationWhileLocked(t *testing.T) {
	root, err := filepath.Abs(localTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "scheduler-state", "run.lock")
	if err := os.MkdirAll(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockPath, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runPortableScheduler(ctx, root, func(string) error { return nil }, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("scheduler cancellation = %v", err)
	}
	if err := runPortableScheduler(context.Background(), "relative", func(string) error { return nil }, io.Discard); err == nil {
		t.Fatal("relative scheduler root unexpectedly succeeded")
	}
}

func TestDiscoverPortableSchedulerTasksOnlyIncludesEnabledTimers(t *testing.T) {
	root := localTempDir(t)
	writeTask := func(name string, enabled bool, schedule string) {
		directory := filepath.Join(root, "services", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		config := fmt.Sprintf("{\"mode\":\"schedule\",\"enabled\":%t}\n", enabled)
		if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		record := portableScheduleRecord{Version: portableScheduleVersion, Name: name, Command: []string{"true"}, Schedule: schedule, CreatedAt: time.Now().UTC()}
		if err := writePortableSchedule(filepath.Join(directory, "schedule.json"), record); err != nil {
			t.Fatal(err)
		}
	}
	writeTask("enabled", true, "*/5 * * * *")
	writeTask("disabled", false, "*/5 * * * *")
	tasks, err := discoverPortableSchedulerTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].record.Name != "enabled" {
		t.Fatalf("unexpected scheduler tasks: %#v", tasks)
	}
}

func TestPortableScheduleSummary(t *testing.T) {
	got := portableScheduleSummary("0 4 * * *", "Asia/Shanghai", "5min", "30min", "2min")
	want := "0 4 * * * Asia/Shanghai, boot+5min, every 30min, jitter 2min"
	if got != want {
		t.Fatalf("portableScheduleSummary() = %q, want %q", got, want)
	}
}

func TestScheduledHelperProcess(t *testing.T) {
	if os.Getenv("SPM_TEST_SCHEDULE_HELPER") != "1" {
		return
	}
	code, _ := strconv.Atoi(os.Getenv("SPM_TEST_SCHEDULE_EXIT"))
	os.Exit(code)
}

func TestPortableRunnerFailureLoggingAndEnvironment(t *testing.T) {
	directory, err := filepath.Abs(localTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(directory, "task.env")
	if err := os.WriteFile(envFile, []byte("# comment\nFROM_FILE='yes'\nOVERRIDE=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(directory, "schedule.json")
	record := portableScheduleRecord{
		Version: portableScheduleVersion, Name: "failing", Command: []string{os.Args[0], "-test.run=TestScheduledHelperProcess"},
		Cwd: directory, EnvFile: envFile, Env: []string{"SPM_TEST_SCHEDULE_HELPER=1", "SPM_TEST_SCHEDULE_EXIT=7", "OVERRIDE=new", "INVALID"},
		TimerInterval: "1h", CreatedAt: time.Now().UTC(),
	}
	if err := writePortableSchedule(recordPath, record); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(directory, "state")
	logPath := filepath.Join(directory, "task.log")
	err = runPortableSchedule([]string{"--config", recordPath, "--state-dir", stateDir, "--log", logPath, "--activation", "timer", "--force"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("failing scheduled command unexpectedly succeeded")
	}
	state := readPortableScheduleState(filepath.Join(stateDir, "schedule-state.json"))
	if state.LastExitCode == nil || *state.LastExitCode != 7 {
		t.Fatalf("exit state = %#v", state)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatal(err)
	}
	environment := scheduledEnvironment(record)
	joined := strings.Join(environment, "\n")
	for _, want := range []string{"FROM_FILE=yes", "OVERRIDE=new", "SPM_TEST_SCHEDULE_EXIT=7"} {
		if !strings.Contains(joined, want) {
			t.Errorf("scheduled environment missing %q", want)
		}
	}
	if got := formatScheduleTime(time.Time{}); got != "-" {
		t.Errorf("zero schedule time = %q", got)
	}
	if got := scheduleExitCode(nil); got != "-" {
		t.Errorf("nil exit code = %q", got)
	}

	for _, args := range [][]string{{"--config"}, {"--unknown"}, {"--config", "relative", "--state-dir", "relative"}} {
		if err := runPortableSchedule(args, io.Discard, io.Discard); err == nil {
			t.Errorf("runPortableSchedule(%v) unexpectedly succeeded", args)
		}
	}
}

func TestPortableScheduleLockDetectsLiveAndStaleOwners(t *testing.T) {
	directory := localTempDir(t)
	live := filepath.Join(directory, "live.lock")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if locked, err := acquirePortableScheduleLock(live); err != nil || locked {
		t.Fatalf("live lock = %v, %v", locked, err)
	}
	stale := filepath.Join(directory, "stale.lock")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pid"), []byte("99999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if locked, err := acquirePortableScheduleLock(stale); err != nil || !locked {
		t.Fatalf("stale lock = %v, %v", locked, err)
	}
}

func TestPortableRunnerExecutesAndRecordsResult(t *testing.T) {
	directory := localTempDir(t)
	directory, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(directory, "schedule.json")
	record := portableScheduleRecord{
		Version: portableScheduleVersion, Name: "job", Command: []string{os.Args[0], "-test.run=TestScheduledHelperProcess"},
		Cwd: directory, Env: []string{"SPM_TEST_SCHEDULE_HELPER=1"}, Schedule: "0 0 1 1 *", CreatedAt: time.Now().UTC(),
	}
	if err := writePortableSchedule(recordPath, record); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(directory, "state")
	if err := runPortableSchedule([]string{"--config", recordPath, "--state-dir", stateDir, "--force"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	state := readPortableScheduleState(filepath.Join(stateDir, "schedule-state.json"))
	if state.LastExitCode == nil || *state.LastExitCode != 0 || state.LastFinishedAt.IsZero() {
		t.Fatalf("unexpected runner state: %#v", state)
	}
	if !state.LastScheduledAt.IsZero() {
		t.Fatalf("manual run changed scheduling history: %#v", state)
	}
}

func TestPortableCronAcceptsSundaySevenAndSteps(t *testing.T) {
	expression, err := parseCronExpression("0 */2 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	if !expression.matches(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("Sunday expressed as 7 did not match")
	}
}

func TestLatestDueOccurrenceDeduplicatesAndCatchesUpOnce(t *testing.T) {
	created := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	record := portableScheduleRecord{Version: 1, Name: "job", Command: []string{"true"}, Schedule: "*/5 * * * *", Persistent: true, CreatedAt: created}
	state := portableScheduleState{Version: 1, LastCheckedAt: created, LastScheduledAt: created.Add(5 * time.Minute)}
	due, ok, err := latestDueOccurrence(record, state, created.Add(17*time.Minute), "timer")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !due.Equal(created.Add(15*time.Minute)) {
		t.Fatalf("due = %s, %v; want 08:15", due, ok)
	}
	state.LastScheduledAt = due
	if _, ok, err := latestDueOccurrence(record, state, created.Add(17*time.Minute), "timer"); err != nil || ok {
		t.Fatalf("duplicate occurrence = %v, %v", ok, err)
	}
}

func TestValidatePortableScheduleChecksTimezone(t *testing.T) {
	record := portableScheduleRecord{Version: 1, Name: "job", Command: []string{"true"}, Schedule: "0 9 * * *", Timezone: "Asia/Singapore", CreatedAt: time.Now()}
	if err := validatePortableSchedule(record); err != nil {
		t.Fatal(err)
	}
	record.Timezone = "Not/A_Zone"
	if err := validatePortableSchedule(record); err == nil {
		t.Fatal("invalid timezone unexpectedly accepted")
	}
}

func TestPortableDurationsUseCrossPlatformUnits(t *testing.T) {
	for value, want := range map[string]time.Duration{"30min": 30 * time.Minute, "2h": 2 * time.Hour, "1day": 24 * time.Hour, "1.5seconds": 1500 * time.Millisecond, "1h30min": 90 * time.Minute} {
		got, err := parsePortableDuration(value)
		if err != nil || got != want {
			t.Errorf("parsePortableDuration(%q) = %s, %v; want %s", value, got, err, want)
		}
	}
}

func TestPortableOccurrenceKeyDeduplicatesDSTFallback(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	record := portableScheduleRecord{Schedule: "30 1 * * *", Timezone: "America/New_York"}
	first := time.Date(2026, 11, 1, 1, 30, 0, 0, location)
	second := first.Add(time.Hour)
	firstKey, err := portableOccurrenceKey(record, first)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := portableOccurrenceKey(record, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstKey != secondKey {
		t.Fatalf("fallback keys differ: %q != %q", firstKey, secondKey)
	}
}

func TestPreparePortableScheduleUpdatePreservesAnchorForUnchangedSchedule(t *testing.T) {
	directory := localTempDir(t)
	directory, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "schedule.json")
	created := time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)
	previous := portableScheduleRecord{Version: 1, Name: "job", Command: []string{"old"}, Schedule: "0 9 * * *", CreatedAt: created}
	if err := writePortableSchedule(path, previous); err != nil {
		t.Fatal(err)
	}
	next := portableScheduleRecord{Version: 1, Name: "job", Command: []string{"new"}, Schedule: previous.Schedule, CreatedAt: time.Now()}
	if err := preparePortableScheduleUpdate(path, filepath.Join(directory, "state"), &next); err != nil {
		t.Fatal(err)
	}
	if !next.CreatedAt.Equal(created) {
		t.Fatalf("created_at changed from %s to %s", created, next.CreatedAt)
	}
}
