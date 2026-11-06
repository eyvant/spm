# Cross-platform scheduling design

Status: implemented

This document describes how `spm schedule` and scheduled declarations in
`spm.toml` provide consistent behavior on Linux, Windows, and macOS. Linux uses
systemd timers. Windows and macOS use a scope-level scheduler daemon that sleeps
until the nearest occurrence and delegates execution to Task Scheduler or
launchd so native account isolation is preserved.

## Goals

- Support standard five-field cron and optional leading seconds on all three
  platforms.
- Keep observable scheduling behavior consistent across native service
  managers.
- Continue to use systemd, Windows Task Scheduler, and launchd for activation,
  permissions, and recovery after a reboot.
- Run the Windows/macOS scheduler only while its scope has enabled schedules.
- Make both `spm schedule` and `spm apply spm.toml` use the same scheduling
  implementation.

## Non-goals

- Quartz year fields or aliases such as `@daily`.
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

SPM accepts standard five-field cron and a six-field form with leading seconds:

```text
minute hour day-of-month month day-of-week
second minute hour day-of-month month day-of-week
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

- Five-field cron has one-minute grammar and fires at second zero.
- Six-field cron has one-second grammar. It is not a hard real-time guarantee;
  process startup can be delayed by operating-system load.
- `timezone` accepts an IANA time-zone name such as `Asia/Singapore`.
- When `timezone` is omitted, the machine's local time zone is used.
- SPM should embed the IANA time-zone database so Windows does not depend on an
  external zoneinfo installation.

### Daylight-saving transitions

- A nonexistent local time during a forward transition is skipped.
- If `persistent = true`, a skipped occurrence makes the job eligible for one
  catch-up run.
- A repeated local occurrence during a backward transition runs only once.
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
- Portable intervals may use second-level durations, with a minimum of one
  second.
- Restarting spm or reloading a native task does not reset the interval anchor.
- `timer_on_boot` means the delay after operating-system boot.
- A user-scoped backend that cannot run until login performs at most one late
  boot activation after the user session becomes available.

## Scheduler and runner model

On Windows and macOS, each user/system scope with at least one enabled calendar
or interval schedule has one scheduler daemon. It calculates the nearest
occurrence and sleeps directly until that instant. Removing or disabling the
last schedule removes the daemon registration. Linux uses systemd timer
semantics directly and does not need the daemon.

```text
scope scheduler daemon
       |
       v
native per-task executor
       |
       +-- load declaration and state
       +-- determine whether an occurrence is due
       +-- acquire the task lock
       +-- apply catch-up and random-delay rules
       +-- run the declared command
       +-- record the result
       `-- exit
```

The per-task executor remains a short-lived internal runner. This separation
lets a system scheduler request execution without itself impersonating the
task account: Task Scheduler and launchd retain ownership of account switching.

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

Linux installs exact native calendar triggers. On Windows and macOS, the daemon
is the timing authority while the native manager is the execution and recovery
authority. The daemon never polls once per minute; it arms one timer for the
nearest `next_run` and recalculates after activation or configuration changes.

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
- Keep one dormant Task Scheduler executor per task and one logon/boot-activated
  scope scheduler. The scheduler starts due executors with `schtasks /Run`.
- User-scoped tasks run with the interactive user's credentials. System-scoped
  tasks use an explicitly selected service account and may require elevation.
- Configure native multiple-instance behavior to ignore overlapping starts, in
  addition to the runner lock.

### macOS

- Keep one loaded, dormant launchd executor per task and one `RunAtLoad`,
  `KeepAlive` scope scheduler. The scheduler uses `launchctl kickstart` for due
  executors.
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

On Windows and macOS, both scheduled command output (`service.log`) and daemon
diagnostics (`scheduler.log`) rotate at 10 MiB. SPM retains five numbered
backups and enforces the limit during writes, not only when a task starts.

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

Running `spm enable <name>` after upgrading rewrites an older Windows/macOS
minute-triggered task into the shared-daemon layout while preserving its
scheduling state. Recreating it with `spm schedule --force` has the same effect.

## Validation and safety limits

- Reject invalid cron expressions before modifying native state.
- Reject unsupported cron extensions instead of changing their meaning.
- Keep Windows/macOS native executors trigger-free so complex expressions do
  not expand into thousands of native calendar entries.
- Validate IANA time zones on every platform.
- Resolve and store the spm executable path used by native activation.
- Quote command arguments using platform-specific rules; do not reconstruct a
  command through a shell unless the declaration explicitly invokes one.
- Preserve the previous native task and state if replacement fails partway.

## Implementation map

- Shared Go code owns cron parsing, occurrence calculation, DST handling,
  portable state, and locking.
- Linux converts the same cron grammar to systemd `OnCalendar` and uses
  `AccuracySec=1s`.
- Windows registers the scope scheduler and dormant executors with Task
  Scheduler.
- macOS registers the scope scheduler and dormant executors with launchd.
- Declarative and imperative commands call the same backend lifecycle paths.

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
