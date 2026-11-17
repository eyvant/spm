#!/usr/bin/env bash
set -Eeuo pipefail

[[ $# -eq 2 ]] || { echo "usage: $0 PATH-TO-SPM user|system" >&2; exit 2; }
SOURCE_DIR=$(cd "$(dirname "$1")" && pwd)
SOURCE_SPM=$SOURCE_DIR/$(basename "$1")
SCOPE=$2
[[ $SCOPE == user || $SCOPE == system ]] || { echo "invalid scope: $SCOPE" >&2; exit 2; }
PREFIX=$(printf '%s-%s' "${SPM_CI_PREFIX:-ci-$$}" "$SCOPE" | tr '[:upper:]_' '[:lower:]-')
# Each scope runs its own copy of spm and its runtimes, so replacing
# spm-supervisor for the upgrade test does not touch the other scope, which
# runs in parallel.
BIN_DIR=${RUNNER_TEMP:-/tmp}/$PREFIX-bin
rm -rf "$BIN_DIR"
mkdir -p "$BIN_DIR"
cp "$SOURCE_SPM" "$BIN_DIR/spm"
for runtime in spm-scheduler spm-supervisor spm-supervisor-next; do
  if [[ -f $SOURCE_DIR/$runtime ]]; then
    cp "$SOURCE_DIR/$runtime" "$BIN_DIR/$runtime"
  fi
done
SPM=$BIN_DIR/spm
MAIN=$PREFIX-main
DEPENDENCY=$PREFIX-dependency
DEPENDENT=$PREFIX-dependent
DECLARED=$PREFIX-declared
SCHEDULE=$PREFIX-scheduled
SCHEDULE_DECLARED=$PREFIX-scheduled-declared
ROTATION=$PREFIX-log-rotation
SERVICE_ROTATION=$PREFIX-service-log-rotation
TICKER=$PREFIX-ticker
HOT_JOB=$PREFIX-hot-job
STEADY=$PREFIX-steady
HOT_MARKER=${RUNNER_TEMP:-/tmp}/$PREFIX-hot-marker.log
CRASHING=$PREFIX-crashing
MARKER="SPM_MACOS_READY_$PREFIX"
TOML=${RUNNER_TEMP:-/tmp}/$PREFIX.toml
CRASH_LOG=${RUNNER_TEMP:-/tmp}/$PREFIX-restarts.log
SCHEDULE_MARKER=${RUNNER_TEMP:-/tmp}/$PREFIX-schedule-marker.log
SCHEDULE_TOML=${RUNNER_TEMP:-/tmp}/$PREFIX-schedule.toml

if [[ $SCOPE == system ]]; then
  command_spm() { sudo --preserve-env=GOCOVERDIR "$SPM" --system "$@"; }
  PLIST_DIR=/Library/LaunchDaemons
  DATA_ROOT='/Library/Application Support/spm'
else
  command_spm() { "$SPM" "$@"; }
  PLIST_DIR=$HOME/Library/LaunchAgents
  DATA_ROOT="$HOME/Library/Application Support/spm"
fi

diagnose() {
  local code=$?
  trap - ERR
  set +e
  echo "macOS $SCOPE integration failed" >&2
  command_spm list >&2
  command_spm status "$MAIN" >&2
  command_spm status "$DEPENDENT" >&2
  exit "$code"
}

cleanup() {
  trap - ERR
  set +e
  for name in "$TICKER" "$SERVICE_ROTATION" "$HOT_JOB" "$STEADY" "$CRASHING" "$ROTATION" "$SCHEDULE_DECLARED" "$SCHEDULE" "$DEPENDENT" "$DEPENDENCY" "$DECLARED" "$MAIN"; do
    command_spm stop "$name" >/dev/null 2>&1
    command_spm delete --yes "$name" >/dev/null 2>&1
  done
  rm -f "$TOML"
  rm -f "$CRASH_LOG"
  rm -f "$SCHEDULE_MARKER" "$SCHEDULE_TOML" "$HOT_MARKER"
}
trap diagnose ERR
trap cleanup EXIT
cleanup

wait_running() {
  local name=$1
  for _ in {1..80}; do
    if command_spm status "$name" 2>/dev/null | grep -Eq '^status[[:space:]]+running$'; then
      return 0
    fi
    sleep 0.25
  done
  echo "$name did not become running" >&2
  return 1
}

wait_log() {
  local name=$1 pattern=$2
  for _ in {1..40}; do
    if command_spm logs --lines 100 --grep "$pattern" "$name" 2>/dev/null | grep -F "$pattern"; then
      return 0
    fi
    sleep 0.25
  done
  echo "$name did not log $pattern" >&2
  return 1
}

if [[ ${SPM_CI_E2E_MODE:-full} == full ]]; then
  start_args=(start --name "$MAIN" --no-enable --restart no --cwd /tmp)
  [[ $SCOPE == system ]] && start_args+=(--user root --group wheel)
  start_args+=(-- /bin/sh -c "echo $MARKER; exec /bin/sleep 20")
  command_spm "${start_args[@]}"
  wait_running "$MAIN"
  wait_log "$MAIN" "$MARKER"
  # What spm records about the service is kept apart from its output.
  command_spm logs --spm "$MAIN" | grep -F 'starting /bin/sh -c'
  ! command_spm logs --spm "$MAIN" | grep -Fx "$MARKER"
  command_spm native launchd print "$MAIN" | grep -F "com.eyvant.spm.$MAIN"

  PID_BEFORE=$(command_spm status "$MAIN" | awk '$1 == "pid" { print $2 }')
  command_spm restart "$MAIN"
  wait_running "$MAIN"
  PID_AFTER=$(command_spm status "$MAIN" | awk '$1 == "pid" { print $2 }')
  [[ -n $PID_BEFORE && -n $PID_AFTER && $PID_BEFORE != "$PID_AFTER" ]]

  command_spm enable "$MAIN"
  test -f "$PLIST_DIR/com.eyvant.spm.$MAIN.plist"
  command_spm disable "$MAIN"
  test ! -e "$PLIST_DIR/com.eyvant.spm.$MAIN.plist"
  command_spm run "$MAIN"
  wait_running "$MAIN"

  dependency_args=(start --name "$DEPENDENCY" --no-enable --restart no -- /bin/sleep 20)
  dependent_args=(start --name "$DEPENDENT" --no-enable --restart no --requires "$DEPENDENCY" --after "$DEPENDENCY" -- /bin/sleep 20)
  if [[ $SCOPE == system ]]; then
    dependency_args=(start --name "$DEPENDENCY" --no-enable --restart no --user root --group wheel -- /bin/sleep 20)
    dependent_args=(start --name "$DEPENDENT" --no-enable --restart no --user root --group wheel --requires "$DEPENDENCY" --after "$DEPENDENCY" -- /bin/sleep 20)
  fi
  command_spm "${dependency_args[@]}"
  command_spm stop "$DEPENDENCY"
  command_spm "${dependent_args[@]}"
  wait_running "$DEPENDENCY"
  wait_running "$DEPENDENT"

  crash_args=(start --name "$CRASHING" --no-enable --restart on-failure --restart-sec 1s -- /bin/sh -c "echo restart >> '$CRASH_LOG'; sleep 0.2; exit 23")
  if [[ $SCOPE == system ]]; then
    crash_args=(start --name "$CRASHING" --no-enable --restart on-failure --restart-sec 1s --user root --group wheel -- /bin/sh -c "echo restart >> '$CRASH_LOG'; sleep 0.2; exit 23")
  fi
  command_spm "${crash_args[@]}"
  for _ in {1..80}; do
    [[ -f $CRASH_LOG ]] && RESTARTS=$(wc -l <"$CRASH_LOG") || RESTARTS=0
    [[ $RESTARTS -ge 2 ]] && break
    sleep 0.25
  done
  [[ $RESTARTS -ge 2 ]]

  cat >"$TOML" <<EOF
schema = 1
[[services]]
name = "$DECLARED"
command = ["/bin/sleep", "20"]
restart = "no"
enable = false
[services.macos]
description = "macOS declaration integration"
EOF
  command_spm validate "$TOML"
  command_spm apply --dry-run "$TOML" | grep -F $'create\t'"$DECLARED"
  command_spm apply "$TOML"
  wait_running "$DECLARED"
  command_spm diff "$TOML" | grep -F $'unchanged\t'"$DECLARED"
fi

echo "==> macOS $SCOPE native scheduling, portable cron, state, and lifecycle"
schedule_args=(schedule --name "$SCHEDULE" --cron '*/5 * * * * *' --timezone UTC --persistent --randomized-delay 1s --cwd /tmp)
if [[ $SCOPE == system ]]; then
  schedule_args+=(--user root --group wheel)
fi
schedule_args+=(-- /bin/sh -c "date -u +%FT%TZ >> '$SCHEDULE_MARKER'")
command_spm "${schedule_args[@]}"
SCHEDULE_PLIST=$PLIST_DIR/com.eyvant.spm.$SCHEDULE.plist
test -f "$SCHEDULE_PLIST"
! grep -F '<key>StartInterval</key>' "$SCHEDULE_PLIST"
grep -F 'spm-scheduler' "$SCHEDULE_PLIST"
grep -F '<string>run</string>' "$SCHEDULE_PLIST"
grep -F '<string>daemon</string>' "$SCHEDULE_PLIST"
SCHEDULER_PLIST=$PLIST_DIR/com.eyvant.spm.scheduler.plist
test -f "$SCHEDULER_PLIST"
grep -F '<string>daemon</string>' "$SCHEDULER_PLIST"
grep -F '<key>KeepAlive</key>' "$SCHEDULER_PLIST"
command_spm native launchd print "$SCHEDULE" | grep -F "com.eyvant.spm.$SCHEDULE"
for _ in {1..30}; do
  [[ -s $SCHEDULE_MARKER ]] && break
  sleep 1
done
[[ -s $SCHEDULE_MARKER ]]
command_spm status "$SCHEDULE" | grep -Eq '^status[[:space:]]+waiting$'
command_spm status "$SCHEDULE" | grep -Eq '^last exit code[[:space:]]+0$'
LINES_BEFORE=$(wc -l <"$SCHEDULE_MARKER")
command_spm run "$SCHEDULE"
LINES_AFTER=$(wc -l <"$SCHEDULE_MARKER")
[[ $LINES_AFTER -gt $LINES_BEFORE ]]
command_spm disable "$SCHEDULE"
test ! -e "$SCHEDULE_PLIST"
command_spm enable "$SCHEDULE"
test -f "$SCHEDULE_PLIST"

echo "==> macOS $SCOPE scheduler upgrade interrupts neither a running job nor a service"
if [[ $SCOPE == system ]]; then
  DOMAIN=system
  launchd_pid() { sudo launchctl print "$DOMAIN/$1" 2>/dev/null | awk '$1 == "pid" { print $3; exit }'; }
  run_as=(--user root --group wheel)
else
  DOMAIN=gui/$(id -u)
  launchd_pid() { launchctl print "$DOMAIN/$1" 2>/dev/null | awk '$1 == "pid" { print $3; exit }'; }
  run_as=()
fi
command_spm start --name "$STEADY" --no-enable --restart no ${run_as[@]+"${run_as[@]}"} -- /bin/sleep 300
wait_running "$STEADY"
STEADY_PID=$(command_spm status "$STEADY" | awk '$1 == "pid" { print $2 }')
# Every 5 seconds, a run that lasts 8 seconds: the run lock keeps one at a time.
command_spm schedule --name "$HOT_JOB" --cron '*/5 * * * * *' --timezone UTC --cwd /tmp ${run_as[@]+"${run_as[@]}"} \
  -- /bin/sh -c "echo start >> '$HOT_MARKER'; sleep 8; echo end >> '$HOT_MARKER'"
RUN_PID=
for _ in {1..60}; do
  RUN_PID=$(launchd_pid "com.eyvant.spm.$HOT_JOB")
  [[ -n $RUN_PID ]] && break
  sleep 0.5
done
[[ -n $RUN_PID ]] || { echo "$HOT_JOB never started a run" >&2; false; }
ENDS_BEFORE=$(grep -c '^end$' "$HOT_MARKER" || true)
DAEMON_PID=$(launchd_pid com.eyvant.spm.scheduler)
[[ -n $DAEMON_PID ]]

command_spm __upgrade-runtime --scheduler | tee /dev/stderr | grep -F 'spm scheduler restarted'

NEW_DAEMON_PID=
for _ in {1..40}; do
  NEW_DAEMON_PID=$(launchd_pid com.eyvant.spm.scheduler)
  [[ -n $NEW_DAEMON_PID && $NEW_DAEMON_PID != "$DAEMON_PID" ]] && break
  sleep 0.25
done
[[ -n $NEW_DAEMON_PID && $NEW_DAEMON_PID != "$DAEMON_PID" ]] || { echo "scheduler daemon was not replaced" >&2; false; }
ps -o command= -p "$NEW_DAEMON_PID" | grep -F 'spm-scheduler'
for _ in {1..40}; do
  ps -p "$DAEMON_PID" >/dev/null || break
  sleep 0.25
done
! ps -p "$DAEMON_PID" >/dev/null || { echo "old scheduler daemon $DAEMON_PID is still running" >&2; false; }
# The run that was in progress keeps going and finishes normally.
ps -p "$RUN_PID" >/dev/null || { echo "the in-progress run of $HOT_JOB was killed" >&2; false; }
for _ in {1..40}; do
  [[ $(grep -c '^end$' "$HOT_MARKER" || true) -gt $ENDS_BEFORE ]] && break
  sleep 0.5
done
[[ $(grep -c '^end$' "$HOT_MARKER" || true) -gt $ENDS_BEFORE ]] || { echo "the in-progress run did not finish" >&2; false; }
# The service was not touched.
[[ $(command_spm status "$STEADY" | awk '$1 == "pid" { print $2 }') == "$STEADY_PID" ]] || { echo "$STEADY was restarted" >&2; false; }
# The new daemon keeps scheduling.
STARTS_AFTER=$(grep -c '^start$' "$HOT_MARKER")
for _ in {1..40}; do
  [[ $(grep -c '^start$' "$HOT_MARKER") -gt $STARTS_AFTER ]] && break
  sleep 0.5
done
[[ $(grep -c '^start$' "$HOT_MARKER") -gt $STARTS_AFTER ]] || { echo "no run was scheduled after the upgrade" >&2; false; }
command_spm status "$HOT_JOB" | grep -Eq '^last exit code[[:space:]]+0$'
command_spm stop "$HOT_JOB" >/dev/null 2>&1 || true
command_spm delete --yes "$HOT_JOB" "$STEADY"
# The scheduler daemon timestamps its own log.
command_spm logs --scheduler | grep -E '^\[[0-9T:+-]+Z?\] scheduler [0-9]+ \(build [0-9a-f]+\) started'

echo "==> macOS $SCOPE supervisor upgrade keeps the service and its output"
command_spm start --name "$TICKER" --no-enable --restart no --cwd /tmp ${run_as[@]+"${run_as[@]}"} \
  -- /bin/sh -c 'i=0; while true; do i=$((i + 1)); echo "tick $i"; sleep 0.5; done'
wait_running "$TICKER"
wait_log "$TICKER" 'tick 2'
TICKER_PID=$(command_spm status "$TICKER" | awk '$1 == "pid" { print $2 }')
TICKER_SUPERVISOR=$(launchd_pid "com.eyvant.spm.$TICKER")
# launchd runs spm-supervisor; status reports the service process below it.
[[ -n $TICKER_PID && -n $TICKER_SUPERVISOR && $TICKER_PID != "$TICKER_SUPERVISOR" ]]
ps -o command= -p "$TICKER_SUPERVISOR" | grep -F 'spm-supervisor launchd'
if [[ -f $BIN_DIR/spm-supervisor-next ]]; then
  cp "$BIN_DIR/spm-supervisor-next" "$BIN_DIR/spm-supervisor.new"
  mv -f "$BIN_DIR/spm-supervisor.new" "$BIN_DIR/spm-supervisor"
  command_spm __upgrade-runtime --supervisor | tee /dev/stderr | grep -F "$TICKER switched to the new supervisor (kept running)"
  [[ $(command_spm status "$TICKER" | awk '$1 == "pid" { print $2 }') == "$TICKER_PID" ]] || { echo "$TICKER was restarted" >&2; false; }
  [[ $(launchd_pid "com.eyvant.spm.$TICKER") == "$TICKER_SUPERVISOR" ]] || { echo "launchd saw the supervisor exit" >&2; false; }
  command_spm logs --spm "$TICKER" | grep -F "took over running process $TICKER_PID"
  LAST_TICK=$(command_spm logs --lines 1 "$TICKER")
  for _ in {1..20}; do
    [[ $(command_spm logs --lines 1 "$TICKER") != "$LAST_TICK" ]] && break
    sleep 0.25
  done
  [[ $(command_spm logs --lines 1 "$TICKER") != "$LAST_TICK" ]] || { echo "output stopped after the supervisor upgrade" >&2; false; }
fi
command_spm stop "$TICKER"
command_spm logs --spm "$TICKER" | grep -F "passing SIGTERM on to process $TICKER_PID"
command_spm delete --yes "$TICKER"

echo "==> macOS $SCOPE declarative scheduled task"
cat >"$SCHEDULE_TOML" <<EOF
schema = 1
[[services]]
name = "$SCHEDULE_DECLARED"
command = ["/usr/bin/true"]
schedule = "*/5 9-17/2 1 * 1-5/2"
timezone = "Asia/Singapore"
persistent = true
timer_randomized_delay = "1s"
enable = false
[services.macos]
description = "macOS declarative schedule"
EOF
command_spm validate "$SCHEDULE_TOML"
command_spm apply --dry-run "$SCHEDULE_TOML" | grep -F $'create\t'"$SCHEDULE_DECLARED"
command_spm apply "$SCHEDULE_TOML"
test ! -e "$PLIST_DIR/com.eyvant.spm.$SCHEDULE_DECLARED.plist"
command_spm cat "$SCHEDULE_DECLARED" | grep -F 'spm-scheduler'
command_spm diff "$SCHEDULE_TOML" | grep -F $'unchanged\t'"$SCHEDULE_DECLARED"

echo "==> macOS $SCOPE scheduled log rotation"
rotation_args=(schedule --name "$ROTATION" --on-boot 24h --no-enable)
if [[ $SCOPE == system ]]; then
  rotation_args+=(--user root --group wheel)
fi
rotation_args+=(-- /bin/sh -c 'dd if=/dev/zero bs=1048576 count=11 2>/dev/null')
command_spm "${rotation_args[@]}"
command_spm run "$ROTATION"
ROTATION_LOG="$DATA_ROOT/services/$ROTATION/service.log"
if [[ $SCOPE == system ]]; then
  test "$(sudo stat -f %z "$ROTATION_LOG.1")" -eq $((10 * 1024 * 1024))
  test "$(sudo stat -f %z "$ROTATION_LOG")" -eq $((1024 * 1024))
else
  test "$(stat -f %z "$ROTATION_LOG.1")" -eq $((10 * 1024 * 1024))
  test "$(stat -f %z "$ROTATION_LOG")" -eq $((1024 * 1024))
fi
command_spm logs --spm "$ROTATION" | grep -F 'scheduled run exited with code 0'

echo "==> macOS $SCOPE service log rotation"
command_spm start --name "$SERVICE_ROTATION" --no-enable --restart no ${run_as[@]+"${run_as[@]}"} \
  -- /bin/sh -c 'dd if=/dev/zero bs=1048576 count=11 2>/dev/null; exec /bin/sleep 60'
SERVICE_ROTATION_LOG="$DATA_ROOT/services/$SERVICE_ROTATION/service.log"
file_size() { if [[ $SCOPE == system ]]; then sudo stat -f %z "$1" 2>/dev/null; else stat -f %z "$1" 2>/dev/null; fi; }
for _ in {1..40}; do
  [[ $(file_size "$SERVICE_ROTATION_LOG") == $((1024 * 1024)) ]] && break
  sleep 0.25
done
test "$(file_size "$SERVICE_ROTATION_LOG.1")" -eq $((10 * 1024 * 1024))
test "$(file_size "$SERVICE_ROTATION_LOG")" -eq $((1024 * 1024))
command_spm delete --yes "$SERVICE_ROTATION"

cleanup
for name in "$MAIN" "$DEPENDENCY" "$DEPENDENT" "$DECLARED" "$SCHEDULE" "$SCHEDULE_DECLARED" "$ROTATION" "$SERVICE_ROTATION" "$TICKER" "$CRASHING"; do
  test ! -e "$PLIST_DIR/com.eyvant.spm.$name.plist"
  test ! -e "$PLIST_DIR/com.eyvant.spm.$name.boot.plist"
done
test ! -e "$PLIST_DIR/com.eyvant.spm.scheduler.plist"
rm -rf "$BIN_DIR"
echo "spm macOS $SCOPE integration passed"
