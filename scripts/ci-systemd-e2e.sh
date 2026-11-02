#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $(id -u) -ne 0 ]]; then
  echo "This integration test must run as root." >&2
  exit 1
fi
if [[ $# -ne 1 ]]; then
  echo "Usage: $0 PATH-TO-SPM" >&2
  exit 1
fi

SPM=$(realpath "$1")
PREFIX=${SPM_CI_PREFIX:-ci-$$}
SERVICE=${PREFIX}-service
SCHEDULE=${PREFIX}-schedule
DEPENDENCY=${PREFIX}-dependency
DEPENDENT=${PREFIX}-dependent
DECLARED=${PREFIX}-declared
CRASHING=${PREFIX}-crashing
MARKER=/tmp/${PREFIX}-schedule-ran
TOML=/tmp/${PREFIX}.toml

diagnose() {
  local exit_code=$1
  local line=$2
  local command=$3
  trap - ERR
  set +e
  echo "spm systemd integration test failed at line $line: $command" >&2
  echo "systemd state: $(systemctl is-system-running 2>/dev/null || true)" >&2
  systemctl --no-pager --full status \
    "spm-$SERVICE.service" \
    "spm-$SERVICE.health.service" \
    "spm-$SERVICE.health.timer" \
    "spm-$SERVICE.cron-restart.timer" \
    "spm-$SCHEDULE.service" \
    "spm-$SCHEDULE.timer" \
    "spm-$DEPENDENCY.service" \
    "spm-$DEPENDENT.service" \
    "spm-$DECLARED.service" \
    "spm-$CRASHING.service" >&2
  journalctl --no-pager -n 100 \
    -u "spm-$SERVICE.service" \
    -u "spm-$SERVICE.health.service" \
    -u "spm-$SCHEDULE.service" \
    -u "spm-$CRASHING.service" >&2
  echo "spm systemd integration test failed at line $line: $command" >&2
  exit "$exit_code"
}

cleanup() (
  trap - ERR
  set +e
  "$SPM" --system delete "$SERVICE" >/dev/null 2>&1
  "$SPM" --system delete "$SCHEDULE" >/dev/null 2>&1
  "$SPM" --system delete "$DEPENDENT" >/dev/null 2>&1
  "$SPM" --system delete "$DEPENDENCY" >/dev/null 2>&1
  "$SPM" --system delete "$DECLARED" >/dev/null 2>&1
  "$SPM" --system delete "$CRASHING" >/dev/null 2>&1
  rm -f "$MARKER" "$TOML"
)
trap 'diagnose "$?" "$LINENO" "$BASH_COMMAND"' ERR
trap cleanup EXIT
cleanup

SYSTEMD_STATE=$(systemctl is-system-running 2>/dev/null || true)
case "$SYSTEMD_STATE" in
  running | degraded) ;;
  *) echo "systemd reports '$SYSTEMD_STATE'; continuing while its system bus is usable." >&2 ;;
esac
systemctl daemon-reload
getent passwd nobody >/dev/null
getent group nogroup >/dev/null

"$SPM" --system start \
  --name "$SERVICE" \
  --cwd /tmp \
  --user nobody \
  --group nogroup \
  --restart no \
  --wait \
  --wait-command /bin/true \
  --wait-timeout 10s \
  --health-command /bin/true \
  --health-interval 1h \
  --health-timeout 2s \
  --health-retries 1 \
  -- /bin/sh -c 'echo SPM_CI_READY; exec /bin/sleep 300'

test "$(systemctl is-active spm-$SERVICE.service)" = active
test "$(systemctl is-enabled spm-$SERVICE.service)" = enabled
test "$(systemctl show spm-$SERVICE.service --property=User --value)" = nobody
test "$(systemctl show spm-$SERVICE.service --property=Group --value)" = nogroup
"$SPM" --system show "$SERVICE" | grep -F "$SERVICE"
"$SPM" --system logs --lines 20 --output cat --grep SPM_CI_READY "$SERVICE" | grep -F SPM_CI_READY

"$SPM" --system set "$SERVICE" --cron-restart "*/30 * * * *"
test "$(systemctl is-active spm-$SERVICE.cron-restart.timer)" = active
test "$(systemctl is-enabled spm-$SERVICE.cron-restart.timer)" = enabled
"$SPM" --system unset "$SERVICE" --cron-restart
test ! -e /etc/systemd/system/spm-$SERVICE.cron-restart.timer

systemctl start spm-$SERVICE.health.service
"$SPM" --system show "$SERVICE" | grep -F healthy

# systemd versions differ on whether `systemctl start` returns success after
# observing an initially-active service that then fails during the same start
# transaction. The unit and its restart counter are the assertions here, so an
# exit caused by the intentional crash is acceptable.
if ! "$SPM" --system start --name "$CRASHING" --cwd /tmp --restart on-failure --restart-sec 200ms --no-enable -- /bin/sh -c 'sleep 0.2; exit 23'; then
  echo "spm start observed the intentional crash; checking native restart state" >&2
fi
test "$(systemctl show "spm-$CRASHING.service" --property=Restart --value)" = on-failure
for _ in $(seq 1 40); do
  RESTARTS=$(systemctl show "spm-$CRASHING.service" --property=NRestarts --value)
  [[ ${RESTARTS:-0} -ge 2 ]] && break
  sleep 0.25
done
[[ ${RESTARTS:-0} -ge 2 ]]
"$SPM" --system stop "$CRASHING"

PID_BEFORE=$(systemctl show spm-$SERVICE.service --property=MainPID --value)
"$SPM" --system restart "$SERVICE"
PID_AFTER=$(systemctl show spm-$SERVICE.service --property=MainPID --value)
test "$PID_BEFORE" != "$PID_AFTER"
test "$PID_AFTER" != 0

"$SPM" --system schedule \
  --name "$SCHEDULE" \
  --cwd /tmp \
  --cron "*/30 * * * *" \
  --persistent \
  --run-now \
  -- /usr/bin/touch "$MARKER"
test -e "$MARKER"

"$SPM" --system start --name "$DEPENDENCY" --cwd /tmp --restart no --no-enable -- /bin/sleep 300
"$SPM" --system stop "$DEPENDENCY"
"$SPM" --system start --name "$DEPENDENT" --cwd /tmp --restart no --no-enable \
  --requires "spm-$DEPENDENCY.service" --after "spm-$DEPENDENCY.service" -- /bin/sleep 300
systemctl is-active --quiet "spm-$DEPENDENCY.service"
systemctl is-active --quiet "spm-$DEPENDENT.service"
systemctl stop "spm-$DEPENDENT.service" "spm-$DEPENDENCY.service"
systemctl start "spm-$DEPENDENT.service"
systemctl is-active --quiet "spm-$DEPENDENCY.service"
systemctl is-active --quiet "spm-$DEPENDENT.service"

cat >"$TOML" <<EOF
schema = 1
[[services]]
name = "$DECLARED"
command = ["/bin/sleep", "300"]
restart = "no"
enable = false
[services.linux]
user = "nobody"
group = "nogroup"
description = "Linux declaration integration"
[services.windows]
command = ["cmd.exe", "/c", "exit", "0"]
[services.macos]
command = ["/bin/sleep", "300"]
EOF
"$SPM" --system validate "$TOML"
"$SPM" --system apply --dry-run "$TOML" | grep -F $'create\t'"$DECLARED"
"$SPM" --system apply "$TOML"
systemctl is-active --quiet "spm-$DECLARED.service"
"$SPM" --system diff "$TOML" | grep -F $'unchanged\t'"$DECLARED"
test "$(systemctl is-active spm-$SCHEDULE.timer)" = active
test "$(systemctl is-enabled spm-$SCHEDULE.timer)" = enabled

rm -f "$MARKER"
"$SPM" --system run "$SCHEDULE"
test -e "$MARKER"

cleanup
test ! -e /etc/systemd/system/spm-$SERVICE.service
test ! -e /etc/systemd/system/spm-$SCHEDULE.service
test ! -e /etc/systemd/system/spm-$DEPENDENCY.service
test ! -e /etc/systemd/system/spm-$DEPENDENT.service
test ! -e /etc/systemd/system/spm-$DECLARED.service
test ! -e /etc/systemd/system/spm-$CRASHING.service
test ! -e /etc/spm/services/$SERVICE.json
test ! -e /etc/spm/services/$SCHEDULE.json

echo "spm systemd integration test passed"
