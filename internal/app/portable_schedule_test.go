package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestScheduledHelperProcess(t *testing.T) {
	if os.Getenv("SPM_TEST_SCHEDULE_HELPER") != "1" {
		return
	}
	os.Exit(0)
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
