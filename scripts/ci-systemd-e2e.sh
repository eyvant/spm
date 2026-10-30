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
MARKER=/tmp/${PREFIX}-schedule-ran

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
    "spm-$SCHEDULE.timer" >&2
  journalctl --no-pager -n 100 \
    -u "spm-$SERVICE.service" \
    -u "spm-$SERVICE.health.service" \
    -u "spm-$SCHEDULE.service" >&2
  exit "$exit_code"
}

cleanup() (
  trap - ERR
  set +e
  "$SPM" --system delete "$SERVICE" >/dev/null 2>&1
  "$SPM" --system delete "$SCHEDULE" >/dev/null 2>&1
  rm -f "$MARKER"
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
test "$(systemctl is-active spm-$SCHEDULE.timer)" = active
test "$(systemctl is-enabled spm-$SCHEDULE.timer)" = enabled

rm -f "$MARKER"
"$SPM" --system run "$SCHEDULE"
test -e "$MARKER"

cleanup
test ! -e /etc/systemd/system/spm-$SERVICE.service
test ! -e /etc/systemd/system/spm-$SCHEDULE.service
test ! -e /etc/spm/services/$SERVICE.json
test ! -e /etc/spm/services/$SCHEDULE.json

echo "spm systemd integration test passed"
