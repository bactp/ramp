#!/usr/bin/env bash
# Two regressions that are invisible in a happy-path run and that both
# invalidate measurements rather than breaking anything loudly.
#
#   R1  timer catch-up: a process paused for N seconds must NOT execute N ticks
#       when it resumes. The old `next_tick += 1.0` did exactly that, so every
#       restored workload manufactured dozens of logical positions in seconds
#       and every RPO number taken off the counter was really measuring the
#       catch-up burst.
#
#   R2  restart safety: an epoch whose manager dies mid-flight must end up
#       Failed -- never Committed from artifacts captured at different times --
#       and the application it had paused must be released.
set -uo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL01="${WL01:-$HOME/workload01.kubeconfig}"
GROUP="${GROUP:-video-stream-rg}"
NS="${NS:-ramp-demo}"
PAUSE="${PAUSE:-20}"
OUT="${OUT:-$ROOT/evidence/recovery-epoch-regression-$(date +%Y%m%dT%H%M%S)}"
mkdir -p "$OUT"

K()  { kubectl --kubeconfig "$MGMT" "$@"; }
K1() { kubectl --kubeconfig "$WL01" "$@"; }
vstate() { K1 exec -n "$NS" deploy/video-session -- sh -c 'cat /tmp/ramp-video-state.json' 2>/dev/null; }
vfield() { vstate | python3 -c "import sys,json;print(json.load(sys.stdin)[\"$1\"])" 2>/dev/null || echo "?"; }
RC=0

# ------------------------------------------------------------------ R1 -----
echo "########## R1: no catch-up burst after a ${PAUSE}s pause ##########"
BEFORE=$(vfield position)
K1 exec -n "$NS" deploy/video-session -- sh -c 'printf "{\"quiesce\": true, \"epoch\": 0}" > /tmp/ramp-video-control'
sleep "$PAUSE"
DURING=$(vfield position)
K1 exec -n "$NS" deploy/video-session -- sh -c 'rm -f /tmp/ramp-video-control'
sleep 5
AFTER=$(vfield position)
GAIN=$(( AFTER - DURING ))
{
  echo "position before pause = $BEFORE"
  echo "position after ${PAUSE}s pause = $DURING  (must equal $BEFORE: a quiesced app does not advance)"
  echo "position 5s after resume = $AFTER"
  echo "positions gained in the 5s after a ${PAUSE}s pause = $GAIN"
  echo "pre-fix behaviour would have been ~$(( PAUSE + 5 )) (one tick per elapsed wall-clock second)"
  if [ "$DURING" = "$BEFORE" ] && [ "$GAIN" -le 8 ]; then
    echo "verdict=PASS"
  else
    echo "verdict=FAIL"
  fi
} | tee "$OUT/r1-timer-catchup.txt"
grep -q '^verdict=PASS' "$OUT/r1-timer-catchup.txt" || RC=1

# ------------------------------------------------------------------ R2 -----
echo
echo "########## R2: epoch interrupted by a manager restart ##########"
# -x matches the process NAME. A -f pattern would also match the shell that
# launched the manager (its command line contains the string), and killing that
# instead leaves the manager running and the test measuring nothing.
MGR_PID=$(pgrep -x ramp-manager | head -1)
# The recorded argv starts with a relative "./bin/ramp-manager"; this script
# runs from scripts/ramp-scenario1, so it has to be re-rooted before it can be
# re-executed. Getting this wrong silently leaves the manager DOWN and the test
# then reports a false failure with no manager in the loop at all.
MGR_CMD=$(tr '\0' ' ' < "/proc/${MGR_PID:-0}/cmdline" 2>/dev/null | sed "s#^\./bin/#$ROOT/bin/#")
if [ -z "$MGR_PID" ]; then
  echo "ramp-manager is not running as a local process; skipping R2" | tee "$OUT/r2-restart-safety.txt"
else
  # A long quiesce-verification window makes the mid-flight moment easy to hit
  # without racing the controller.
  BEFORE2=$(vfield position)
  # Name the epoch up front: the run happens in the background and the object
  # has to be identified while it is still mid-flight, not afterwards.
  LAST=$(K get recoverypoint -n default \
           -o jsonpath="{range .items[?(@.spec.recoveryGroupRef=='${GROUP}')]}{.spec.epoch}{'\n'}{end}" \
         | sort -n | tail -1)
  RP="${GROUP}-epoch-$(( ${LAST:-0} + 1 ))"
  ( EXTRA_SPEC='  quiesceVerifySeconds: 25' EXPECT_PHASE=Failed ./20-run-epoch.sh "$GROUP" default \
      > "$OUT/r2-epoch.log" 2>&1 ) &
  EPOCH_JOB=$!
  sleep 12
  PHASE_MID=$(K get recoverypoint "$RP" -o jsonpath='{.status.phase}')
  QUIESCED_MID=$(vfield quiesced)
  echo "killing the manager mid-epoch (RP=$RP phase=$PHASE_MID appQuiesced=$QUIESCED_MID)"
  kill "$MGR_PID"; sleep 3

  # setsid, not just nohup: the restarted manager has to outlive this script,
  # and the script must not end up wait()ing on it. Without the new session the
  # test hangs forever at exit with the manager as its child.
  ( cd "$ROOT" && MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:-nephio1234}" MINIO_SECRET_KEY="${MINIO_SECRET_KEY:-secret1234}" \
      KUBECONFIG="$MGMT" setsid $MGR_CMD < /dev/null >> "$ROOT/evidence/logs/ramp-manager.log" 2>&1 & )
  for _ in $(seq 1 30); do
    pgrep -x ramp-manager >/dev/null && break
    sleep 1
  done
  pgrep -x ramp-manager >/dev/null || { echo "FATAL: manager did not restart"; exit 1; }
  sleep 25
  wait $EPOCH_JOB 2>/dev/null || true

  PHASE_END=$(K get recoverypoint "$RP" -o jsonpath='{.status.phase}')
  REASON=$(K get recoverypoint "$RP" -o jsonpath='{.status.failureReason}')
  MSG=$(K get recoverypoint "$RP" -o jsonpath='{.status.message}')
  sleep 5
  QUIESCED_END=$(vfield quiesced)
  AFTER2=$(vfield position)
  K get recoverypoint "$RP" -o yaml > "$OUT/r2-recoverypoint.yaml"
  {
    echo "recoveryPoint=$RP"
    echo "phase when the manager was killed=$PHASE_MID"
    echo "application quiesced when the manager was killed=$QUIESCED_MID"
    echo "phase after the manager restarted=$PHASE_END"
    echo "failureReason=$REASON"
    echo "message=$MSG"
    echo "application quiesced after restart=$QUIESCED_END"
    echo "application position before=$BEFORE2 after=$AFTER2"
    if [ "$PHASE_END" = "Failed" ] && [ "$REASON" = "EpochInterrupted" ] && [ "$QUIESCED_END" = "False" ] \
       && python3 -c "import sys;sys.exit(0 if int('$AFTER2')>int('$BEFORE2') else 1)"; then
      echo "verdict=PASS"
    else
      echo "verdict=FAIL"
    fi
  } | tee "$OUT/r2-restart-safety.txt"
  grep -q '^verdict=PASS' "$OUT/r2-restart-safety.txt" || RC=1
fi

echo; echo "evidence -> $OUT"
exit $RC
