#!/usr/bin/env bash
set -Eeuo pipefail

[[ $# -eq 1 ]] || { echo "usage: $0 PATH-TO-SPM" >&2; exit 2; }
SPM=$(realpath "$1")
PREFIX=${SPM_CI_PREFIX:-ci-user-$$}
MAIN=$PREFIX-main
DEPENDENCY=$PREFIX-dependency
DEPENDENT=$PREFIX-dependent
DECLARED=$PREFIX-declared
SCHEDULE=$PREFIX-schedule
SCHEDULE_DECLARED=$PREFIX-schedule-declared
CRASHING=$PREFIX-crashing
TOML=/tmp/$PREFIX.toml
MARKER=SPM_LINUX_USER_READY_$PREFIX
SCHEDULE_MARKER=/tmp/$PREFIX-schedule-marker

cleanup() {
  trap - ERR
  set +e
  for name in "$CRASHING" "$SCHEDULE_DECLARED" "$SCHEDULE" "$DEPENDENT" "$DEPENDENCY" "$DECLARED" "$MAIN"; do
    "$SPM" delete --yes "$name" >/dev/null 2>&1
  done
  rm -f "$TOML"
  rm -f "$SCHEDULE_MARKER"
  rm -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/spm-$CRASHING.service.d/crash.conf"
  rmdir "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/spm-$CRASHING.service.d" 2>/dev/null || true
}
diagnose() {
  local code=$?
  trap - ERR
  set +e
  systemctl --user --no-pager --full status "spm-$MAIN.service" "spm-$DEPENDENT.service" >&2
  journalctl --user --no-pager -n 100 -u "spm-$MAIN.service" >&2
  exit "$code"
}
trap diagnose ERR
trap cleanup EXIT
cleanup

"$SPM" start --name "$MAIN" --cwd /tmp --restart no --wait --wait-command /bin/true -- /bin/sh -c "echo $MARKER; exec /bin/sleep 300"
systemctl --user is-active --quiet "spm-$MAIN.service"
systemctl --user is-enabled --quiet "spm-$MAIN.service"
"$SPM" logs --lines 20 --output cat --grep "$MARKER" "$MAIN" | grep -F "$MARKER"
PID_BEFORE=$(systemctl --user show "spm-$MAIN.service" --property=MainPID --value)
"$SPM" restart "$MAIN"
PID_AFTER=$(systemctl --user show "spm-$MAIN.service" --property=MainPID --value)
[[ $PID_BEFORE != "$PID_AFTER" && $PID_AFTER != 0 ]]
"$SPM" disable "$MAIN"
test "$(systemctl --user is-enabled "spm-$MAIN.service" || true)" = disabled
"$SPM" enable "$MAIN"
systemctl --user is-active --quiet "spm-$MAIN.service"

"$SPM" start --name "$DEPENDENCY" --no-enable --restart no -- /bin/sleep 300
"$SPM" stop "$DEPENDENCY"
"$SPM" start --name "$DEPENDENT" --no-enable --restart no \
  --requires "spm-$DEPENDENCY.service" --after "spm-$DEPENDENCY.service" -- /bin/sleep 300
systemctl --user is-active --quiet "spm-$DEPENDENCY.service"
systemctl --user is-active --quiet "spm-$DEPENDENT.service"

"$SPM" start --name "$CRASHING" --no-enable --restart on-failure --restart-sec 100ms -- /bin/sleep 300
CRASH_DROPIN=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/spm-$CRASHING.service.d
mkdir -p "$CRASH_DROPIN"
printf '[Service]\nExecStart=\nExecStart=/bin/sh -c "sleep 0.2; exit 23"\n' >"$CRASH_DROPIN/crash.conf"
systemctl --user daemon-reload
systemctl --user restart "spm-$CRASHING.service"
test "$(systemctl --user show "spm-$CRASHING.service" --property=Restart --value)" = on-failure
for _ in {1..40}; do
  RESTARTS=$(systemctl --user show "spm-$CRASHING.service" -p NRestarts --value)
  [[ ${RESTARTS:-0} -ge 2 ]] && break
  sleep 0.25
done
[[ ${RESTARTS:-0} -ge 2 ]]

cat >"$TOML" <<EOF
schema = 1
[[services]]
name = "$DECLARED"
command = ["/bin/sleep", "300"]
restart = "no"
enable = false
[services.linux]
description = "Linux user declaration integration"
[services.windows]
command = ["cmd.exe", "/c", "exit", "0"]
[services.macos]
command = ["/bin/sleep", "300"]
EOF
"$SPM" validate "$TOML"
"$SPM" apply --dry-run "$TOML" | grep -F $'create\t'"$DECLARED"
"$SPM" apply "$TOML"
systemctl --user is-active --quiet "spm-$DECLARED.service"
"$SPM" diff "$TOML" | grep -F $'unchanged\t'"$DECLARED"

"$SPM" schedule --name "$SCHEDULE" --cron '*/5 * * 1 * 1-5' --timezone UTC --persistent --run-now -- /usr/bin/touch "$SCHEDULE_MARKER"
test -e "$SCHEDULE_MARKER"
SCHEDULE_TIMER=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/spm-$SCHEDULE.timer
test -f "$SCHEDULE_TIMER"
grep -c '^OnCalendar=' "$SCHEDULE_TIMER" | grep -Fx 2
grep -Fx 'AccuracySec=1s' "$SCHEDULE_TIMER"
systemctl --user is-active --quiet "spm-$SCHEDULE.timer"

cat >>"$TOML" <<EOF

[[services]]
name = "$SCHEDULE_DECLARED"
command = ["/usr/bin/true"]
schedule = "*/5 9-17/2 1 * 1-5/2"
timezone = "UTC"
persistent = true
enable = false
EOF
"$SPM" validate "$TOML"
"$SPM" apply "$TOML"
test -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/spm-$SCHEDULE_DECLARED.timer"
test "$(systemctl --user is-enabled "spm-$SCHEDULE_DECLARED.timer" || true)" = disabled
"$SPM" diff "$TOML" | grep -F $'unchanged\t'"$SCHEDULE_DECLARED"

cleanup
echo "spm Linux user integration passed"
