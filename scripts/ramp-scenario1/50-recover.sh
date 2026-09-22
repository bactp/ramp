#!/usr/bin/env bash
# Execute a recovery FROM ONE COMMITTED RecoveryPoint.
#
# Two members, two mechanisms, one recovery point:
#   redis -> load the epoch's IMMUTABLE RDB artifact and prove it is at P
#   video -> restore the container's in-memory execution state from the epoch's
#            CRI checkpoint image (containerd's own restore path, the one the
#            Transition Operator uses)
#
# ORDERING IS A CORRECTNESS REQUIREMENT, NOT A CONVENIENCE:
#
#   restore Redis -> MEASURE Redis -> restore Video -> MEASURE Video -> resume
#
# The restored Video reconnects to Redis and writes its own position within a
# second of starting. Any check performed after both members are up therefore
# reports a consistent system even when Redis was recovered to the WRONG state
# and then silently overwritten. The measurement has to happen in the window
# where only one member is running, which is why Redis is verified by
# 48-restore-redis-epoch.sh before this script activates Video at all.
#
#   usage: RP=<recoverypoint> CKPT_IMAGE=<tag> TARGET_NODE=<node> ./50-recover.sh
set -euo pipefail
cd "$(dirname "$0")"
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
PATH_NAME="${PATH_NAME:-video-workload01-to-workload02}"
NS="${NS:-ramp-demo}"
OUT="${OUT:-$(mktemp -d)}"
VIDEO_URL="${VIDEO_URL:-http://192.168.28.122:30808/}"
TARGET_NODE="${TARGET_NODE:?TARGET_NODE must be set}"
CKPT_IMAGE="${CKPT_IMAGE:?CKPT_IMAGE must be set}"
mkdir -p "$OUT"

ts() { date -Is; }
K()  { kubectl --kubeconfig "$MGMT" "$@"; }
K2() { kubectl --kubeconfig "$WL02" "$@"; }

# RP may be pinned explicitly -- the Q>P experiment recovers from an OLD epoch
# on purpose. Otherwise take what the path is tracking.
if [ -z "${RP:-}" ]; then
  RP=$(K get recoverypath "$PATH_NAME" -o jsonpath='{.status.observedRecoveryPoint}')
fi
READINESS=$(K get recoverypath "$PATH_NAME" -o jsonpath='{.status.readiness}')
EPOCH=$(K get recoverypoint "$RP" -o jsonpath='{.spec.epoch}')
P=$(K get recoverypoint "$RP" -o jsonpath='{.status.logicalPosition}')

echo "recovering from $RP (epoch=$EPOCH, P=$P, path readiness=$READINESS)"
echo "video checkpoint image = $CKPT_IMAGE"

echo "T_recovery_start=$(ts)" | tee -a "$OUT/timings.txt"

# ---------------------------------------------------------------- redis ----
echo "--- restoring redis from the epoch artifact ---"
RP="$RP" MGMT="$MGMT" WL02="$WL02" NS="$NS" ./48-restore-redis-epoch.sh "$OUT" 2>&1 | tee "$OUT/redis-restore.log"

# ---------------------------------------------------------------- video ----
# Start catching the restored process's FIRST published state before the
# activation commit, so the recorded position is the one it resumed with and not
# whatever it had advanced to by the time a poll happened to arrive.
echo "--- restoring video container from the epoch checkpoint image ---"
echo "T_video_restore_start=$(ts)" | tee -a "$OUT/timings.txt"
( for _ in $(seq 1 3000); do
    B=$(curl -sS --max-time 2 "$VIDEO_URL" 2>/dev/null) || { sleep 0.1; continue; }
    case "$B" in *'"position"'*) printf '%s' "$B" > "$OUT/video-restored-initial.json"; exit 0 ;; esac
    sleep 0.1
  done ) &
POLLER=$!

CKPT_IMAGE="$CKPT_IMAGE" TARGET_NODE="$TARGET_NODE" ./62-activate-gitops-path.sh > "$OUT/activate.log" 2>&1
echo "T_activation_committed=$(ts)" | tee -a "$OUT/timings.txt"

for _ in $(seq 1 300); do
  RDY=$(K2 get pods -n "$NS" -l app=video-session -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || true)
  [ "$RDY" = "true" ] && break
  sleep 0.5
done
echo "T_video_pod_ready=$(ts)" | tee -a "$OUT/timings.txt"

for _ in $(seq 1 100); do
  [ -s "$OUT/video-restored-initial.json" ] && break
  sleep 0.2
done
kill "$POLLER" 2>/dev/null || true
wait "$POLLER" 2>/dev/null || true
echo "T_video_ready=$(ts)" | tee -a "$OUT/timings.txt"

K2 get pods -n "$NS" -o wide > "$OUT/target-pods.txt" 2>&1 || true
K2 get deploy video-session -n "$NS" -o yaml > "$OUT/target-video-deploy.yaml" 2>&1 || true

VPOS=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["position"])' "$OUT/video-restored-initial.json" 2>/dev/null || echo "?")

# ------------------------------------------------------------- unquiesce ---
# The epoch checkpointed the application WHILE IT WAS QUIESCED, and a CRI
# checkpoint carries the container's writable layer -- including the quiesce
# control file. The restored process therefore comes back frozen at exactly P
# and stays there until recovery releases it.
#
# That is a feature, not an accident to work around: it means the restored
# member cannot write anything into Redis before the recovery has verified both
# members are at P. Releasing it is the last step of the recovery, after the
# measurement, which is the only ordering in which "Redis is at P" is a
# falsifiable claim.
echo "--- releasing the restored application ---"
curl -sS --max-time 5 "${VIDEO_URL%/}/resume" > "$OUT/video-resume.json" 2>/dev/null || true
echo "T_video_resumed=$(ts)" | tee -a "$OUT/timings.txt"

# Progress sample: the restored process must advance one position per tick, not
# replay the wall-clock time it spent checkpointed.
sleep 6
curl -sS --max-time 5 "$VIDEO_URL" > "$OUT/video-restored-progress.json" 2>/dev/null || true
echo "T_group_ready=$(ts)" | tee -a "$OUT/timings.txt"
echo "video_restored_initial_position=$VPOS" | tee -a "$OUT/timings.txt"

echo
echo "==== recovery of $RP ===="
echo "recovery point position P   = $P"
grep -E '^redis_restored_position' "$OUT/redis-restored-state.txt" || true
echo "video restored initial pos  = $VPOS"
[ "$VPOS" = "$P" ] && echo "VIDEO RESTORED AT P" || echo "WARNING: video restored initial position $VPOS != P=$P"
