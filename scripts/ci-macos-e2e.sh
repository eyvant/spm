#!/usr/bin/env bash
set -Eeuo pipefail

[[ $# -eq 2 ]] || { echo "usage: $0 PATH-TO-SPM user|system" >&2; exit 2; }
SPM=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
SCOPE=$2
[[ $SCOPE == user || $SCOPE == system ]] || { echo "invalid scope: $SCOPE" >&2; exit 2; }
PREFIX=$(printf '%s-%s' "${SPM_CI_PREFIX:-ci-$$}" "$SCOPE" | tr '[:upper:]_' '[:lower:]-')
MAIN=$PREFIX-main
DEPENDENCY=$PREFIX-dependency
DEPENDENT=$PREFIX-dependent
DECLARED=$PREFIX-declared
SCHEDULE=$PREFIX-scheduled
SCHEDULE_DECLARED=$PREFIX-scheduled-declared
ROTATION=$PREFIX-log-rotation
CRASHING=$PREFIX-crashing
MARKER="SPM_MACOS_READY_$PREFIX"
TOML=${RUNNER_TEMP:-/tmp}/$PREFIX.toml
CRASH_LOG=${RUNNER_TEMP:-/tmp}/$PREFIX-restarts.log
SCHEDULE_MARKER=${RUNNER_TEMP:-/tmp}/$PREFIX-schedule-marker.log
SCHEDULE_TOML=${RUNNER_TEMP:-/tmp}/$PREFIX-schedule.toml

if [[ $SCOPE == system ]]; then
  command_spm() { sudo "$SPM" --system "$@"; }
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
  for name in "$CRASHING" "$ROTATION" "$SCHEDULE_DECLARED" "$SCHEDULE" "$DEPENDENT" "$DEPENDENCY" "$DECLARED" "$MAIN"; do
    command_spm stop "$name" >/dev/null 2>&1
    command_spm delete --yes "$name" >/dev/null 2>&1
  done
  rm -f "$TOML"
  rm -f "$CRASH_LOG"
  rm -f "$SCHEDULE_MARKER" "$SCHEDULE_TOML"
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

start_args=(start --name "$MAIN" --no-enable --restart no --cwd /tmp)
[[ $SCOPE == system ]] && start_args+=(--user root --group wheel)
start_args+=(-- /bin/sh -c "echo $MARKER; exec /bin/sleep 300")
command_spm "${start_args[@]}"
wait_running "$MAIN"
wait_log "$MAIN" "$MARKER"
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

dependency_args=(start --name "$DEPENDENCY" --no-enable --restart no -- /bin/sleep 300)
dependent_args=(start --name "$DEPENDENT" --no-enable --restart no --requires "$DEPENDENCY" --after "$DEPENDENCY" -- /bin/sleep 300)
if [[ $SCOPE == system ]]; then
  dependency_args=(start --name "$DEPENDENCY" --no-enable --restart no --user root --group wheel -- /bin/sleep 300)
  dependent_args=(start --name "$DEPENDENT" --no-enable --restart no --user root --group wheel --requires "$DEPENDENCY" --after "$DEPENDENCY" -- /bin/sleep 300)
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
command = ["/bin/sleep", "300"]
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
grep -F '__scheduled-run' "$SCHEDULE_PLIST"
grep -F '<string>daemon</string>' "$SCHEDULE_PLIST"
SCHEDULER_PLIST=$PLIST_DIR/com.eyvant.spm.scheduler.plist
test -f "$SCHEDULER_PLIST"
grep -F '__darwin-schedule-daemon' "$SCHEDULER_PLIST"
grep -F '<key>KeepAlive</key>' "$SCHEDULER_PLIST"
command_spm native launchd print "$SCHEDULE" | grep -F "com.eyvant.spm.$SCHEDULE"
for _ in {1..100}; do
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
command_spm cat "$SCHEDULE_DECLARED" | grep -F '__scheduled-run'
command_spm diff "$SCHEDULE_TOML" | grep -F $'unchanged\t'"$SCHEDULE_DECLARED"

echo "==> macOS $SCOPE scheduled log rotation"
rotation_args=(schedule --name "$ROTATION" --on-boot 1day --no-enable)
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

cleanup
for name in "$MAIN" "$DEPENDENCY" "$DEPENDENT" "$DECLARED" "$SCHEDULE" "$SCHEDULE_DECLARED" "$ROTATION" "$CRASHING"; do
  test ! -e "$PLIST_DIR/com.eyvant.spm.$name.plist"
  test ! -e "$PLIST_DIR/com.eyvant.spm.$name.boot.plist"
done
test ! -e "$PLIST_DIR/com.eyvant.spm.scheduler.plist"
echo "spm macOS $SCOPE integration passed"
