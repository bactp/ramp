#!/usr/bin/env bash
# The four negative tests. Each one asserts the same two things:
#
#   1. the epoch did NOT commit
#   2. the source application was RESUMED anyway
#
# (2) is the one that is easy to get wrong and expensive to get wrong: an epoch
# that aborts while holding the quiesce takes the production workload down for
# a reason that has nothing to do with a failure.
set -uo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL01="${WL01:-$HOME/workload01.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
GROUP="${GROUP:-video-stream-rg}"
NS="${NS:-ramp-demo}"
OUT="${OUT:-$ROOT/evidence/recovery-epoch-negative-$(date +%Y%m%dT%H%M%S)}"
mkdir -p "$OUT"

K()  { kubectl --kubeconfig "$MGMT" "$@"; }
K1() { kubectl --kubeconfig "$WL01" "$@"; }
K2() { kubectl --kubeconfig "$WL02" "$@"; }
vstate() { K1 exec -n "$NS" deploy/video-session -- sh -c 'cat /tmp/ramp-video-state.json' 2>/dev/null; }
vfield() { vstate | python3 -c "import sys,json;print(json.load(sys.stdin)[\"$1\"])" 2>/dev/null || echo "?"; }

RESULTS=()
run_case() { # run_case <id> <title> <expected-reason> <extra-spec> [barrier-timeout]
  local id="$1" title="$2" want="$3" extra="$4" bt="${5:-30}"
  echo; echo "########## $id: $title ##########"
  local before after quiesced phase reason msg rp
  before=$(vfield position)

  EXTRA_SPEC="$extra" BARRIER_TIMEOUT="$bt" EXPECT_PHASE=Failed \
    ./20-run-epoch.sh "$GROUP" default > "$OUT/$id.log" 2>&1
  rp=$(awk -F= '/^recovery_point=/{print $2}' "$OUT/$id.log" | tail -1)
  K get recoverypoint "$rp" -o yaml > "$OUT/$id-recoverypoint.yaml" 2>/dev/null

  phase=$(K get recoverypoint "$rp" -o jsonpath='{.status.phase}' 2>/dev/null)
  reason=$(K get recoverypoint "$rp" -o jsonpath='{.status.failureReason}' 2>/dev/null)
  msg=$(K get recoverypoint "$rp" -o jsonpath='{.status.message}' 2>/dev/null)
  local stage; stage=$(K get recoverypoint "$rp" -o jsonpath='{.status.lastSuccessfulStage}' 2>/dev/null)

  # The application must be running again: not flagged quiesced, and actually
  # advancing. "not flagged" alone would pass even for a wedged process.
  sleep 4
  quiesced=$(vfield quiesced)
  after=$(vfield position)

  local ok=1
  [ "$phase" != "Committed" ] || ok=0
  [ "$reason" = "$want" ] || ok=0
  [ "$quiesced" = "False" ] || ok=0
  python3 -c "import sys; sys.exit(0 if int('$after') > int('$before') else 1)" 2>/dev/null || ok=0

  {
    echo "case=$id"
    echo "recoveryPoint=$rp"
    echo "phase=$phase"
    echo "lastSuccessfulStage=$stage"
    echo "failureReason=$reason (expected $want)"
    echo "message=$msg"
    echo "application quiesced after epoch=$quiesced"
    echo "application position before=$before after=$after (must advance)"
    echo "verdict=$([ $ok = 1 ] && echo PASS || echo FAIL)"
  } | tee "$OUT/$id-result.txt"
  RESULTS+=("$id|$title|$phase|$reason|$quiesced|$before->$after|$([ $ok = 1 ] && echo PASS || echo FAIL)")
}

# --- Test 1: replication acknowledgement unavailable -----------------------
# The replica is removed BEFORE the barrier. connected_replicas would still be
# briefly non-zero on the primary, which is exactly why that was never a
# barrier; WAIT is what actually fails here.
echo "removing the replica so no acknowledgement is possible"
K2 scale deploy/redis-standby -n "$NS" --replicas=0 >/dev/null
K2 wait --for=delete pod -l app=redis-standby -n "$NS" --timeout=90s >/dev/null 2>&1
sleep 3
run_case test1-redis-ack-failure "Redis replica ACK unavailable" ReplicaAckNotAchieved "" 10
echo "restoring the replica"
K2 scale deploy/redis-standby -n "$NS" --replicas=1 >/dev/null
K2 rollout status deploy/redis-standby -n "$NS" --timeout=120s >/dev/null
K2 exec -n "$NS" deploy/redis-standby -- redis-cli REPLICAOF 192.168.28.238 30379 >/dev/null
for _ in $(seq 1 60); do
  L=$(K2 exec -n "$NS" deploy/redis-standby -- redis-cli INFO replication 2>/dev/null | tr -d '\r' | awk -F: '/master_link_status/{print $2}')
  [ "$L" = "up" ] && break; sleep 2
done

# --- Test 2: Redis epoch artifact capture fails ----------------------------
run_case test2-redis-snapshot-failure "Redis epoch snapshot fails" RedisSnapshotFailed \
  '  faultInjection:
    failRedisSnapshot: true'

# --- Test 3: video checkpoint fails AFTER the redis artifact exists --------
# The orphaned RDB stays in the artifact store on purpose. It must be visible
# as an object and invisible as a recovery point.
run_case test3-video-checkpoint-failure "Video checkpoint fails after the Redis artifact was created" CheckpointFailed \
  '  faultInjection:
    failVideoCheckpoint: true'
ORPHAN_RP=$(awk -F= '/^recovery_point=/{print $2}' "$OUT/test3-video-checkpoint-failure.log" | tail -1)
ORPHAN_REF=$(K get recoverypoint "$ORPHAN_RP" -o jsonpath='{.status.artifacts[?(@.type=="redisSnapshot")].ref}')
{
  echo "orphan redis artifact created by the aborted epoch: ${ORPHAN_REF:-<none>}"
  MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:-nephio1234}" MINIO_SECRET_KEY="${MINIO_SECRET_KEY:-secret1234}" \
    "$ROOT/bin/rampctl" --key "$ORPHAN_REF" --out /dev/null 2>&1 || true
  echo "group's latest recovery point is still: $(K get recoverygroup "$GROUP" -o jsonpath='{.status.latestRecoveryPoint}')"
  echo "aborted epoch recoveryEligible=$(K get recoverypoint "$ORPHAN_RP" -o jsonpath='{.status.phase}/{.status.validation.validated}')"
} | tee "$OUT/test3-orphan-artifact.txt"

# --- Test 4: the two artifacts disagree about the position -----------------
run_case test4-position-mismatch "Redis artifact and Video state disagree" ValidationFailed \
  '  faultInjection:
    forcePositionSkew: 3'

echo; echo "########## summary ##########"
{
  echo "| test | expectation | phase | failureReason | app quiesced after | app position | verdict |"
  echo "| --- | --- | --- | --- | --- | --- | --- |"
  for r in "${RESULTS[@]}"; do
    IFS='|' read -r id title phase reason q pos v <<<"$r"
    echo "| $id | $title | $phase | $reason | $q | $pos | **$v** |"
  done
} | tee "$OUT/result-summary.md"
tail -300 "$ROOT/evidence/logs/ramp-manager.log" > "$OUT/controller.log" || true
echo; echo "evidence -> $OUT"
for r in "${RESULTS[@]}"; do case "$r" in *FAIL) exit 1 ;; esac; done
exit 0
