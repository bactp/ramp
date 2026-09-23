#!/usr/bin/env bash
# THE correctness experiment: commit at P, let the source run on to Q, fail at
# Q, and recover from the epoch committed at P.
#
#   A  commit RecoveryPoint RP-E at position P
#   B  resume; let the source advance to Q >= P + ADVANCE
#   C  inject the bounded source failure at Q
#   D  restore Redis from RP-E    -> MEASURE, must be P (not Q)
#   E  restore Video from RP-E    -> MEASURE, must be P
#   F  let both run; both must progress P, P+1, P+2 ...
#
# The measurement order in D/E is the whole point. The restored Video writes its
# own position into Redis within a second of starting, so any check performed
# after both members are up would report a consistent system even if Redis had
# been recovered to Q and then silently rolled back to P.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL01="${WL01:-$HOME/workload01.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
NS="${NS:-ramp-demo}"
PATH_NAME="${PATH_NAME:-video-workload01-to-workload02}"
GROUP="${GROUP:-video-stream-rg}"
ADVANCE="${ADVANCE:-20}"
# TARGET_NODE is taken from the path's own placement decision below, so that
# preparation and readiness cannot disagree about where the recovery lands.
VIDEO_URL="${VIDEO_URL:-http://192.168.28.122:30808/}"
OUT="${OUT:-$ROOT/evidence/recovery-epoch-qgtp-$(date +%Y%m%dT%H%M%S)}"
mkdir -p "$OUT"

K()  { kubectl --kubeconfig "$MGMT" "$@"; }
K1() { kubectl --kubeconfig "$WL01" "$@"; }
K2() { kubectl --kubeconfig "$WL02" "$@"; }
ts() { date -Is; }
mark() { echo "$1=$(ts)" | tee -a "$OUT/timings.txt"; }
say() { echo; echo "=== $* ==="; }

echo "evidence -> $OUT"
mark T_experiment_start

# ------------------------------------------------------- A: commit at P ----
say "A. Recovery Epoch"
mark T_epoch_begin
./20-run-epoch.sh "$GROUP" default > "$OUT/epoch.log" 2>&1 || {
  echo "epoch failed; see $OUT/epoch.log"; tail -30 "$OUT/epoch.log"; exit 1; }
RP=$(awk -F= '/^recovery_point=/{print $2}' "$OUT/epoch.log" | tail -1)
[ -n "$RP" ] || { echo "could not determine the epoch created by this run"; exit 1; }
K get recoverypoint "$RP" -o yaml > "$OUT/recoverypoint.yaml"
K get recoverygroup "$GROUP" -o yaml > "$OUT/recoverygroup.yaml"

EPOCH=$(K get recoverypoint "$RP" -o jsonpath='{.spec.epoch}')
P=$(K get recoverypoint "$RP" -o jsonpath='{.status.logicalPosition}')
REDIS_REF=$(K get recoverypoint "$RP" -o jsonpath='{.status.artifacts[?(@.type=="redisSnapshot")].ref}')
REDIS_SUM=$(K get recoverypoint "$RP" -o jsonpath='{.status.artifacts[?(@.type=="redisSnapshot")].checksum}')
CKPT_REF=$(K get recoverypoint "$RP" -o jsonpath='{.status.artifacts[?(@.type=="containerCheckpoint")].ref}')
CKPT_SUM=$(K get recoverypoint "$RP" -o jsonpath='{.status.artifacts[?(@.type=="containerCheckpoint")].checksum}')
echo "recovery_point=$RP epoch=$EPOCH P=$P" | tee -a "$OUT/timings.txt"
for f in prepareStart quiesceStart quiesceComplete barrierReached redisSnapshotStart \
         redisSnapshotComplete videoCheckpointStart videoCheckpointComplete \
         validateComplete commitTime resumeTime; do
  echo "T_${f}=$(K get recoverypoint "$RP" -o jsonpath="{.status.timings.${f}}")" | tee -a "$OUT/timings.txt"
done

[ "$(K get recoverypoint "$RP" -o jsonpath='{.status.phase}')" = "Committed" ] || { echo "RP not Committed"; exit 1; }
[ -n "$REDIS_REF" ] || { echo "RP has no immutable redis artifact"; exit 1; }

# PREPARE the target while the source is still healthy: stage the artifact,
# build the CRI checkpoint image on the placement node, and put the ArgoCD
# wiring and the (scaled-to-zero) workload object in place. None of this is on
# the recovery path.
#
# 36-prepare-target.sh is told WHICH RecoveryPoint it prepares and stamps that
# claim on the target, so the readiness controller can verify it. The previous
# version passed RP_NAME to one step only, and an unpinned step once staged the
# previous epoch's artifact while the path still looked prepared.
mark T_prepare_start
TARGET_NODE=$(K get recoverypath "$PATH_NAME" -o jsonpath='{.status.targetPlacement.node}')
[ -n "$TARGET_NODE" ] || { echo "the path has not selected a placement node"; exit 1; }
echo "placement_node=$TARGET_NODE" | tee -a "$OUT/timings.txt"
CKPT_IMAGE=$(RP="$RP" TARGET_NODE="$TARGET_NODE" ./36-prepare-target.sh 2>&1 \
             | tee "$OUT/prepare-target.log" | awk -F= '/^CKPT_IMAGE=/{print $2}')
[ -n "$CKPT_IMAGE" ] || { echo "target preparation failed"; tail -25 "$OUT/prepare-target.log"; exit 1; }
echo "checkpoint_image=$CKPT_IMAGE" | tee -a "$OUT/timings.txt"
mark T_prepare_complete

# Wait for the readiness controller to PROMOTE this RecoveryPoint to prepared.
# Recovering from a point the path has not accepted as executable would be
# testing the scripts, not the system.
for _ in $(seq 1 60); do
  [ "$(K get recoverypath "$PATH_NAME" -o jsonpath='{.status.preparedRecoveryPoint.name}')" = "$RP" ] && break
  sleep 3
done
mark T_promoted
K get recoverypath "$PATH_NAME" -o yaml > "$OUT/recoverypath-before-failure.yaml"
echo "prepared_recovery_point=$(K get recoverypath "$PATH_NAME" -o jsonpath='{.status.preparedRecoveryPoint.name}')" | tee -a "$OUT/timings.txt"
echo "path_readiness=$(K get recoverypath "$PATH_NAME" -o jsonpath='{.status.readiness}')" | tee -a "$OUT/timings.txt"

# ------------------------------------------------- B: source advances ------
say "B. source continues past P (target Q >= P+$ADVANCE)"
mark T_advance_start
K1 exec -n "$NS" deploy/video-session -- cat /tmp/ramp-video-state.json > "$OUT/video-state-after-commit.json" 2>/dev/null || true
while :; do
  CUR=$(K1 exec -n "$NS" deploy/video-session -- sh -c 'cat /tmp/ramp-video-state.json' 2>/dev/null \
        | python3 -c 'import sys,json;print(json.load(sys.stdin)["position"])' 2>/dev/null || echo 0)
  [ "$CUR" -ge "$(( P + ADVANCE ))" ] && break
  sleep 2
done
mark T_advance_done
K1 exec -n "$NS" deploy/video-session -- cat /tmp/ramp-video-state.json > "$OUT/video-state-before-failure.json"
K1 exec -n "$NS" deploy/redis-primary -- redis-cli INFO replication > "$OUT/redis-primary-before-failure.txt" 2>&1
K2 exec -n "$NS" deploy/redis-standby -- redis-cli INFO replication > "$OUT/redis-standby-before-failure.txt" 2>&1
STANDBY_POS_BEFORE=$(K2 exec -n "$NS" deploy/redis-standby -- redis-cli GET ramp:video:position | tr -d '\r')
echo "live_standby_position_before_failure=$STANDBY_POS_BEFORE" | tee -a "$OUT/timings.txt"

# artifact immutability AFTER the source moved on
MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:-nephio1234}" MINIO_SECRET_KEY="${MINIO_SECRET_KEY:-secret1234}" \
  "$ROOT/bin/rampctl" --key "$REDIS_REF" --sha256 "$REDIS_SUM" --out "$OUT/redis-epoch-artifact-recheck.rdb" \
  > "$OUT/artifact-immutability.txt" 2>&1 \
  && echo "redis artifact sha256 still $REDIS_SUM after source advanced to >= $(( P + ADVANCE ))" >> "$OUT/artifact-immutability.txt"
K get recoverypoint "$RP" -o jsonpath='{.status.artifacts}' | python3 -m json.tool >> "$OUT/artifact-immutability.txt"

# ------------------------------------------------------ C: fail at Q -------
say "C. bounded source failure"
Q=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["position"])' "$OUT/video-state-before-failure.json")
echo "Q=$Q" | tee -a "$OUT/timings.txt"
mark T_failure
./40-inject-failure.sh >> "$OUT/failure.log" 2>&1
mark T_failure_complete
echo "observed_logical_loss_Q_minus_P=$(( Q - P ))" | tee -a "$OUT/timings.txt"

# the standby still holds Q; this is what a naive promotion would recover
STANDBY_POS_AT_FAILURE=$(K2 exec -n "$NS" deploy/redis-standby -- redis-cli GET ramp:video:position | tr -d '\r')
echo "live_standby_position_at_failure=$STANDBY_POS_AT_FAILURE" | tee -a "$OUT/timings.txt"
K2 exec -n "$NS" deploy/redis-standby -- redis-cli INFO replication > "$OUT/redis-standby-at-failure.txt" 2>&1 || true

# --------------------------------------------- D+E: recover from RP-E ------
say "D+E. recover explicitly from $RP (NOT from the live replica)"
mark T_recovery_begin
RP="$RP" OUT="$OUT" MGMT="$MGMT" WL02="$WL02" NS="$NS" PATH_NAME="$PATH_NAME" \
  CKPT_IMAGE="$CKPT_IMAGE" TARGET_NODE="$TARGET_NODE" VIDEO_URL="$VIDEO_URL" \
  ./50-recover.sh > "$OUT/recover.log" 2>&1 || true
tail -25 "$OUT/recover.log"
mark T_recovery_end

REDIS_INITIAL=$(awk -F= '/^redis_restored_position=/{print $2}' "$OUT/redis-restored-state.txt" 2>/dev/null || echo "?")
VIDEO_INITIAL=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["position"])' "$OUT/video-restored-initial.json" 2>/dev/null || echo "?")
VIDEO_PROGRESS=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["position"])' "$OUT/video-restored-progress.json" 2>/dev/null || echo "?")
VIDEO_SESSION=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["session"])' "$OUT/video-restored-initial.json" 2>/dev/null || echo "?")
SRC_SESSION=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["session"])' "$OUT/video-state-before-failure.json" 2>/dev/null || echo "?")

# ----------------------------------------------- F: normal progression -----
say "F. progression after recovery"
sleep 8
POD2=$(K2 get pod -n "$NS" -l app=redis-standby -o jsonpath='{.items[0].metadata.name}')
REDIS_AFTER=$(K2 exec -n "$NS" "$POD2" -- redis-cli GET ramp:video:position | tr -d '\r')
K2 exec -n "$NS" "$POD2" -- redis-cli INFO replication > "$OUT/redis-standby-after-recovery.txt" 2>&1 || true
echo "redis_position_after_resume=$REDIS_AFTER" | tee -a "$OUT/timings.txt"
mark T_group_ready

# --------------------------------------------------------- verdict ---------
python3 - "$OUT" <<PY | tee "$OUT/result-summary.md"
import json, os, sys
out = sys.argv[1]
P, Q = $P, $Q
redis_initial, video_initial = "$REDIS_INITIAL", "$VIDEO_INITIAL"
video_progress, redis_after = "$VIDEO_PROGRESS", "$REDIS_AFTER"
standby_at_failure = "$STANDBY_POS_AT_FAILURE"

def num(x):
    try: return int(x)
    except Exception: return None

checks = [
    ("Q > P (the source really advanced past the recovery point)", num("$Q") is not None and Q > P),
    ("live replica held Q, not P, at failure time", num(standby_at_failure) is not None and num(standby_at_failure) > P),
    ("Redis restored INITIALLY to P (measured before Video started)", num(redis_initial) == P),
    ("Redis did NOT recover to Q", num(redis_initial) != Q),
    ("Video restored INITIALLY to P", num(video_initial) == P),
    ("restored Video is the SAME process instance as the source", "$VIDEO_SESSION" == "$SRC_SESSION"),
    ("both members progress forward from P after recovery",
     num(video_progress) is not None and num(video_progress) > P and num(redis_after) is not None and num(redis_after) > P),
    ("no catch-up burst: restored Video advanced <= 12 positions in ~6 s",
     num(video_progress) is not None and num(video_progress) - P <= 12),
]
passed = all(ok for _, ok in checks)

print("# Q > P recovery correctness experiment\n")
print("| quantity | value |")
print("| --- | --- |")
print(f"| epoch E | $EPOCH |")
print(f"| RecoveryPoint | \`$RP\` |")
print(f"| committed position P | {P} |")
print(f"| source position at failure Q | {Q} |")
print(f"| ObservedLogicalLoss = Q - P | {Q - P} |")
print(f"| live replica position at failure | {standby_at_failure} |")
print(f"| Redis restored initial position | {redis_initial} |")
print(f"| Video restored initial position | {video_initial} |")
print(f"| Video position ~6 s later | {video_progress} |")
print(f"| Redis position after resume | {redis_after} |")
print(f"| source instance fingerprint | $SRC_SESSION |")
print(f"| restored instance fingerprint | $VIDEO_SESSION |")
print()
for name, ok in checks:
    print(f"- [{'x' if ok else ' '}] {name}")
print()
print("## VERDICT: " + ("PASS" if passed else "FAIL"))
json.dump({"epoch": $EPOCH, "recoveryPoint": "$RP", "P": P, "Q": Q,
           "observedLogicalLoss": Q - P,
           "liveReplicaAtFailure": num(standby_at_failure),
           "redisRestoredInitial": num(redis_initial),
           "videoRestoredInitial": num(video_initial),
           "videoProgress": num(video_progress),
           "redisAfterResume": num(redis_after),
           "sourceInstance": "$SRC_SESSION", "restoredInstance": "$VIDEO_SESSION",
           "checks": {n: ok for n, ok in checks}, "pass": passed},
          open(os.path.join(out, "result.json"), "w"), indent=2)
sys.exit(0 if passed else 1)
PY
VERDICT=$?

K get recoverypoint "$RP" -o yaml > "$OUT/recoverypoint-after-recovery.yaml"
K get recoverypath "$PATH_NAME" -o yaml > "$OUT/recoverypath-after-recovery.yaml"
tail -400 "$ROOT/evidence/logs/ramp-manager.log" > "$OUT/controller.log" || true
python3 - "$OUT" <<'PY'
import json, os, sys
out = sys.argv[1]
t = {}
for ln in open(os.path.join(out, "timings.txt")):
    k, _, v = ln.strip().partition("=")
    if k:
        t[k] = v
json.dump(t, open(os.path.join(out, "timing.json"), "w"), indent=2)
PY
echo; echo "evidence -> $OUT"
exit $VERDICT
