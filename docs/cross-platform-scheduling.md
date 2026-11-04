# Cross-platform scheduling design

Status: implemented baseline

This document describes how `spm schedule` and scheduled declarations in
`spm.toml` provide consistent behavior on Linux, Windows, and macOS. Linux uses
systemd timers. Windows and macOS use native minute-level activation and the
shared short-lived SPM runner described below.

## Goals

- Support the standard five-field cron syntax on all three platforms.
- Keep observable scheduling behavior consistent across native service
  managers.
- Continue to use systemd, Windows Task Scheduler, and launchd for activation,
  permissions, and recovery after a reboot.
- Avoid requiring a permanently running spm scheduling daemon.
- Make both `spm schedule` and `spm apply spm.toml` use the same scheduling
  implementation.

## Non-goals

- Sub-minute cron schedules.
- Quartz cron extensions, seconds or year fields, or aliases such as `@daily`.
- Distributed locking between machines.
- Guaranteed execution while a machine is powered off.
- Replacing the operating system's native service or task manager.

## User-facing configuration

The existing declaration fields remain the portable interface:

```toml
schema = 1

[[services]]
name = "backup"
command = ["./backup", "--incremental"]
cwd = "/opt/backup"
schedule = "*/15 9-17 * * 1-5"
timezone = "Asia/Singapore"
persistent = true
timer_randomized_delay = "2min"
enable = true
```

Interval schedules use the same service declaration:

```toml
[[services]]
name = "cache-sync"
command = ["./sync-cache"]
timer_on_boot = "5min"
timer_interval = "30min"
timer_randomized_delay = "2min"
```

`schedule`, `timer_on_boot`, and `timer_interval` are triggers. At least one
trigger is required for a scheduled task. `schedule` and `cron_restart` remain
mutually exclusive.

## Portable cron grammar

SPM accepts a standard five-field cron expression:

```text
minute hour day-of-month month day-of-week
```

Each field supports:

- `*`
- a number
- a comma-separated list, such as `1,15`
- an inclusive range, such as `1-5`
- a step, such as `*/5` or `1-10/3`

Month values are `1-12`. Day-of-week values are `0-7`, with both `0` and `7`
meaning Sunday. Names such as `MON` and `JAN` are not part of the first portable
version.

When both day-of-month and day-of-week are restricted, a match in either field
causes the job to run. This is the traditional cron OR rule. SPM owns this rule
instead of relying on platform-specific calendar behavior.

## Canonical scheduling semantics

SPM defines the behavior below. Native backends must not silently substitute a
different behavior.

### Precision and time zones

- Cron precision is one minute.
- `timezone` accepts an IANA time-zone name such as `Asia/Singapore`.
- When `timezone` is omitted, the machine's local time zone is used.
- SPM should embed the IANA time-zone database so Windows does not depend on an
  external zoneinfo installation.

### Daylight-saving transitions

- A nonexistent local time during a forward transition is skipped.
- If `persistent = true`, a skipped occurrence makes the job eligible for one
  catch-up run.
- A repeated local minute during a backward transition runs only once.
- The execution identity used for deduplication is the scheduled local date and
  time in the declared time zone, not the time at which the runner happened to
  start.

### Missed schedules

- Without `persistent`, occurrences missed while the machine or scheduler is
  unavailable are not replayed.
- With `persistent`, one or more missed occurrences produce at most one catch-up
  run.
- A catch-up run records the newest missed occurrence as its scheduled time.
- SPM does not replay an unbounded backlog of old runs.

### Random delay

- `timer_randomized_delay` delays an eligible run by a uniformly selected value
  from zero through the configured duration.
- The shared runner calculates the delay on Windows and macOS. Linux uses
  systemd's `RandomizedDelaySec` with the same externally visible bounds.
- A delayed run still belongs to its original scheduled occurrence and is not
  duplicated if another activation happens during the delay.

### Concurrency

- A scheduled task has at most one active invocation.
- If the previous invocation is still active, a new occurrence is skipped.
- Occurrences are not queued.
- A manual `spm run <name>` uses the same lock but does not change the next
  scheduled occurrence.

### Interval and boot triggers

- `timer_interval` is measured from a persisted interval anchor rather than the
  most recent process start.
- Portable intervals have a minimum duration of one minute.
- Restarting spm or reloading a native task does not reset the interval anchor.
- `timer_on_boot` means the delay after operating-system boot.
- A user-scoped backend that cannot run until login performs at most one late
  boot activation after the user session becomes available.

## Runner model

On Windows and macOS, the native scheduler starts a short-lived internal
runner. The runner is not a permanently running scheduling daemon. Linux can
use systemd's equivalent timer semantics directly and does not need a
minute-level runner for schedules systemd can represent.

```text
native activation
       |
       v
spm internal task-runner <name>
       |
       +-- load declaration and state
       +-- determine whether an occurrence is due
       +-- acquire the task lock
       +-- apply catch-up and random-delay rules
       +-- run the declared command
       +-- record the result
       `-- exit
```

The internal command name is an implementation detail and does not need to be a
documented user command. It must be bounded and non-interactive so native task
managers can invoke it safely.

The runner is responsible for all semantics that otherwise differ between
platforms:

- cron evaluation and time-zone conversion;
- day-of-month/day-of-week OR behavior;
- daylight-saving deduplication;
- catch-up detection;
- random delay;
- single-instance locking; and
- persistent execution state.

## Native backend strategy

Native schedules are an activation optimization, not the source of scheduling
semantics.

Linux installs exact native calendar triggers. The current Windows and macOS
baseline installs a minute-level activation and lets the runner evaluate the
canonical expression. Compiling exact Windows and macOS triggers is a future
power and wake-up optimization; it does not change scheduling semantics.

For minute-level activation, the runner evaluates the canonical cron expression
and exits quickly when no occurrence is due. A future backend optimization may
choose a narrower native trigger only if it cannot miss an eligible occurrence.

### Linux

- Use a systemd `.timer` for activation and a oneshot `.service` for the
  declared command.
- Compile exact calendar expressions to `OnCalendar` when possible.
- Use timer activation at boot for catch-up checks.
- Use systemd persistence and randomized delay only where they preserve the
  portable SPM semantics.

### Windows

- Use Windows Task Scheduler rather than the Service Control Manager for task
  activation.
- Install calendar, boot, logon, and repetition triggers through Task Scheduler
  XML or the Task Scheduler API.
- User-scoped tasks run with the interactive user's credentials. System-scoped
  tasks use an explicitly selected service account and may require elevation.
- Configure native multiple-instance behavior to ignore overlapping starts, in
  addition to the runner lock.

### macOS

- Use launchd `StartCalendarInterval`, `StartInterval`, and `RunAtLoad` where
  they preserve the portable semantics.
- Expand lists, ranges, and steps into an array of calendar dictionaries when
  practical.
- Use a minute-level calendar activation when launchd cannot express a trigger
  exactly.
- LaunchAgents implement user scope and LaunchDaemons implement system scope.

## Persistent state

Each runner-backed scheduled task needs a small atomic state record. The
platform backend chooses the scope-appropriate directory, but the data model is
shared:

```json
{
  "version": 1,
  "interval_anchor": "2026-11-04T00:00:00Z",
  "last_scheduled_at": "2026-11-04T09:15:00+08:00",
  "last_started_at": "2026-11-04T09:15:42+08:00",
  "last_finished_at": "2026-11-04T09:15:49+08:00",
  "last_exit_code": 0
}
```

State writes must be atomic. Stale locks are reclaimed after the owning process
exits. Removing a task removes its scheduling state; disabling a task preserves
state so re-enabling it does not reset interval or deduplication history.

## Lifecycle behavior

The following commands must operate on scheduled tasks consistently:

- `spm schedule` creates or replaces a scheduled task.
- `spm apply` creates or updates it from `spm.toml`.
- `spm validate` validates portable grammar and platform-independent semantics.
- `spm diff` shows trigger and command changes without modifying native state.
- `spm run` starts one manual invocation through the platform's oneshot path.
- `spm enable` and `spm disable` control native activation.
- `spm start` and `spm stop` retain their existing scheduled-task meanings.
- `spm status`, `spm list`, and `spm show` report native activation state and,
  for runner-backed tasks, the last runner result.
- `spm delete` removes native triggers, task configuration, locks, and state.
- `spm export` emits the portable scheduling fields.

Applying an unchanged declaration must be idempotent. Updating a command without
changing its schedule must not reset scheduling history.

## Validation and safety limits

- Reject invalid cron expressions before modifying native state.
- Reject unsupported cron extensions instead of changing their meaning.
- Limit native trigger expansion. Expressions exceeding the limit use the
  minute-level runner fallback rather than failing or creating thousands of
  triggers.
- Validate IANA time zones on every platform.
- Resolve and store the spm executable path used by native activation.
- Quote command arguments using platform-specific rules; do not reconstruct a
  command through a shell unless the declaration explicitly invokes one.
- Preserve the previous native task and state if replacement fails partway.

## Implementation phases

1. Extract cron parsing and occurrence calculation into a platform-independent
   package with DST and OR-semantics tests.
2. Add the runner, atomic state, and cross-process task lock.
3. Extend Linux calendar conversion to the complete portable cron grammar and
   OR semantics without changing its CLI or declaration format.
4. Add launchd activation and lifecycle support.
5. Add Windows Task Scheduler activation and lifecycle support.
6. Enable scheduled declarations in the macOS and Windows `apply`, `validate`,
   `diff`, and `export` paths.
7. Add cross-platform conformance tests that feed identical clocks, state, and
   declarations into every backend.

## Conformance requirements

A scheduling feature is portable only when all supported backends pass the same
behavioral fixtures for:

- cron parsing and next-occurrence calculation;
- day-of-month/day-of-week OR behavior;
- local and named time zones;
- forward and backward daylight-saving transitions;
- one-shot catch-up after multiple missed occurrences;
- random-delay bounds;
- overlap suppression;
- interval anchors across reloads; and
- idempotent create, update, disable, enable, and delete operations.

Platform-specific integration tests additionally verify the generated systemd
units, Task Scheduler definitions, and launchd property lists.
