#!/usr/bin/env bash
# Scenario 1 end to end, with the timestamps the evaluation needs.
#
#   epoch -> commit RecoveryPoint -> prepare path -> HOT
#         -> inject bounded source failure -> execute path -> group operational
set -euo pipefail
cd "$(dirname "$0")"
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL01="${WL01:-$HOME/workload01.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
OUT="${OUT:-$(cd ../.. && pwd)/evidence/scenario1/run-$(date +%Y%m%dT%H%M%S)}"
mkdir -p "$OUT"
T="$OUT/timings.txt"
mark() { echo "$1=$(date -Is)" | tee -a "$T"; }

K()  { kubectl --kubeconfig "$MGMT" "$@"; }
K2() { kubectl --kubeconfig "$WL02" "$@"; }

echo "evidence -> $OUT"
mark T_run_start

# 1. Recovery Epoch -------------------------------------------------------
./20-run-epoch.sh video-stream-rg default > "$OUT/epoch.yaml" 2>&1 || {
  echo "epoch failed; see $OUT/epoch.yaml"; exit 1; }
# Read the epoch this run created, NOT recoverygroup.status.latestRecoveryPoint:
# the group controller resyncs on a 30s interval, so the status field can still
# name the PREVIOUS epoch for a few seconds and the timings would be wrong.
RP=$(awk '/^  name: video-stream-rg-epoch-/{print $2; exit}' "$OUT/epoch.yaml")
[ -n "$RP" ] || { echo "could not determine the epoch created by this run"; exit 1; }
echo "recovery_point=$RP" | tee -a "$T"
K get recoverypoint "$RP" -o yaml > "$OUT/recoverypoint.yaml"
{
  echo "T_checkpoint_start=$(K get recoverypoint "$RP" -o jsonpath='{.status.timings.captureStart}')"
  echo "T_barrier_reached=$(K get recoverypoint "$RP" -o jsonpath='{.status.timings.barrierReached}')"
  echo "T_checkpoint_complete=$(K get recoverypoint "$RP" -o jsonpath='{.status.timings.captureComplete}')"
  echo "T_recoverypoint_commit=$(K get recoverypoint "$RP" -o jsonpath='{.status.timings.commitTime}')"
} | tee -a "$T"

# 2. Prepare the path (WARM -> HOT) ---------------------------------------
mark T_prepare_start
RP_NAME="$RP" ./30-prepare-path.sh > "$OUT/prepare.txt" 2>&1
for _ in $(seq 1 40); do
  R=$(K  get recoverypath video-workload01-to-workload02 -o jsonpath='{.status.readiness}')
  OB=$(K get recoverypath video-workload01-to-workload02 -o jsonpath='{.status.preparedRecoveryPoint.name}')
  [ "$R" = "HOT" ] && [ "$OB" = "$RP" ] && break
  sleep 5
done
mark T_hot_reached
echo "readiness_at_hot=$R" | tee -a "$T"
K get recoverypath video-workload01-to-workload02 -o yaml > "$OUT/recoverypath-HOT.yaml"

# Readiness is latched here: after the source dies the replication link drops,
# so the path necessarily degrades. The recovery decision is made against the
# readiness that held BEFORE the failure -- which is the point of keeping paths hot.
HOT_RP=$(K get recoverypath video-workload01-to-workload02 -o jsonpath='{.status.preparedRecoveryPoint.name}')
RP_POS=$(K get recoverypoint "$HOT_RP" -o jsonpath='{.status.validation.checkpointPosition}')
echo "latched_recoverypoint=$HOT_RP position=$RP_POS" | tee -a "$T"

kubectl --kubeconfig "$WL01" exec -n ramp-demo deploy/video-session -- \
  cat /tmp/ramp-video-state.json > "$OUT/prefailure-video-state.json" 2>&1 || true

# 3. Failure --------------------------------------------------------------
mark T_failure
./40-inject-failure.sh > "$OUT/failure.txt" 2>&1
mark T_failure_complete

# 4. Recovery -------------------------------------------------------------
mark T_recovery_start
K2 exec -n ramp-demo deploy/redis-standby -- redis-cli REPLICAOF NO ONE >/dev/null
for _ in $(seq 1 30); do
  ROLE=$(K2 exec -n ramp-demo deploy/redis-standby -- redis-cli INFO replication 2>/dev/null \
          | tr -d '\r' | awk -F: '/^role:/{print $2}')
  [ "$ROLE" = "master" ] && break
  sleep 1
done
mark T_redis_ready
REDIS_POS=$(K2 exec -n ramp-demo deploy/redis-standby -- redis-cli GET ramp:video:position | tr -d '\r')
echo "redis_position_after_promotion=$REDIS_POS" | tee -a "$T"

# Container-checkpoint leg. Expected to FAIL in this stack; recorded either way.
./50-recover.sh > "$OUT/restore-attempt.txt" 2>&1 || true
grep -E 'RUNC_RESTORE_RC|criu failed' "$OUT/restore-attempt.txt" | tee -a "$T" || true
mark T_checkpoint_restore_attempt_done

# Fallback leg so the application actually returns to service.
kubectl --kubeconfig "$WL02" apply -f ../../examples/ramp-scenario1/50-video-recovered-workload02.yaml >/dev/null
kubectl --kubeconfig "$WL02" rollout status deploy/video-session -n ramp-demo --timeout=180s >/dev/null
mark T_video_restored

for _ in $(seq 1 30); do
  S=$(K2 exec -n ramp-demo deploy/video-session -- cat /tmp/ramp-video-state.json 2>/dev/null || true)
  echo "$S" | grep -q '"redis_linked": true' && break
  sleep 2
done
mark T_group_ready
echo "$S" | tee "$OUT/recovered-video-state.json"

K2 get pods -n ramp-demo -o wide          > "$OUT/target-pods.txt" 2>&1
K get recoverygroup,recoverypoint,recoverypath -A > "$OUT/ramp-objects.txt" 2>&1
echo; echo "===== TIMINGS ====="; cat "$T"
