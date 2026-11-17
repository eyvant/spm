package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/eyvant/spm/internal/runtime/common"
)

type Task struct {
	record    Record
	statePath string
}

type portableSchedulerConfig struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
}

// OpenDaemonLog opens the scheduler daemon's own log, scheduler.log in the
// data root, timestamping every line.
func OpenDaemonLog(root string, fallback io.Writer) (io.Writer, func()) {
	log := common.OpenEventLog(DaemonLogPath(root), fallback)
	return log, func() { _ = log.Close() }
}

// DaemonLogPath is where the scheduler daemon of a data root logs.
func DaemonLogPath(root string) string {
	return filepath.Join(root, common.SchedulerLogName)
}

func DiscoverTasks(root string) ([]Task, error) {
	servicesDir := filepath.Join(root, "services")
	entries, err := os.ReadDir(servicesDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(servicesDir, entry.Name())
		data, err := os.ReadFile(filepath.Join(directory, "config.json"))
		if err != nil {
			continue
		}
		var config portableSchedulerConfig
		if json.Unmarshal(data, &config) != nil || config.Mode != "schedule" || !config.Enabled {
			continue
		}
		recordPath := filepath.Join(directory, "schedule.json")
		record, err := ReadRecord(recordPath)
		if err != nil || record.Schedule == "" && record.TimerInterval == "" {
			continue
		}
		stateDir := filepath.Join(directory, "schedule-state")
		tasks = append(tasks, Task{
			record: record, statePath: filepath.Join(stateDir, "schedule-state.json"),
		})
	}
	return tasks, nil
}

func NextOccurrence(record Record, state State, after time.Time) (time.Time, error) {
	var next time.Time
	if record.Schedule != "" {
		expression, err := ParseCron(record.Schedule)
		if err != nil {
			return time.Time{}, err
		}
		location, err := LoadLocation(record.Timezone)
		if err != nil {
			return time.Time{}, err
		}
		if candidate, ok := NextCronOccurrence(expression, after, location); ok {
			next = candidate
		}
	}
	if record.TimerInterval != "" {
		interval, err := ParseDuration(record.TimerInterval)
		if err != nil {
			return time.Time{}, err
		}
		anchor := state.IntervalAnchor
		if anchor.IsZero() {
			anchor = record.CreatedAt
		}
		candidate := anchor.Add(interval)
		if !candidate.After(after) {
			steps := int64(after.Sub(anchor)/interval) + 1
			candidate = anchor.Add(time.Duration(steps) * interval)
		}
		if next.IsZero() || candidate.Before(next) {
			next = candidate
		}
	}
	return next, nil
}

func RunDaemon(ctx context.Context, root string, trigger func(string) error, stderr io.Writer) error {
	if !filepath.IsAbs(root) {
		return errors.New("scheduler requires an absolute root")
	}
	stateDir := filepath.Join(root, "scheduler-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(stateDir, "run.lock")
	for {
		locked, err := AcquireLock(lockPath)
		if err != nil {
			return err
		}
		if locked {
			break
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
	}
	defer os.RemoveAll(lockPath)
	dispatched := map[string]time.Time{}
	for {
		tasks, err := DiscoverTasks(root)
		if err != nil {
			return err
		}
		now := time.Now()
		var next time.Time
		for _, task := range tasks {
			state := ReadState(task.statePath)
			due, shouldRun, dueErr := LatestDueOccurrence(task.record, state, now, "daemon")
			if dueErr != nil {
				fmt.Fprintf(stderr, "scheduler: %s: %v\n", task.record.Name, dueErr)
				continue
			}
			lastDispatch := dispatched[task.record.Name]
			if shouldRun && due.After(lastDispatch) {
				if triggerErr := trigger(task.record.Name); triggerErr != nil {
					fmt.Fprintf(stderr, "scheduler: trigger %s: %v\n", task.record.Name, triggerErr)
					retry := now.Add(5 * time.Second)
					if next.IsZero() || retry.Before(next) {
						next = retry
					}
				} else {
					dispatched[task.record.Name] = due
					lastDispatch = due
				}
			}
			after := now
			if state.LastScheduledAt.After(after) {
				after = state.LastScheduledAt
			}
			if lastDispatch.After(after) {
				after = lastDispatch
			}
			candidate, nextErr := NextOccurrence(task.record, state, after)
			if nextErr != nil {
				fmt.Fprintf(stderr, "scheduler: next %s: %v\n", task.record.Name, nextErr)
				continue
			}
			if !candidate.IsZero() && (next.IsZero() || candidate.Before(next)) {
				next = candidate
			}
		}
		wait := time.Minute
		if len(tasks) > 0 {
			wait = 24 * time.Hour
		}
		if !next.IsZero() {
			wait = time.Until(next)
			if wait < 0 {
				wait = 0
			}
		}
		if wait < time.Second && next.IsZero() {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
	}
}
