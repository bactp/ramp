#!/usr/bin/env bash
# Restore Redis from a COMMITTED RecoveryPoint's immutable epoch artifact.
#
# This replaces "promote whatever the standby currently holds". Promotion is
# only correct while the standby is still at the epoch position; the moment the
# source advances past P the standby carries Q, and promoting it recovers a
# state no RecoveryPoint ever committed.
#
# The replica is NOT removed from the design: it stays the continuous readiness
# mechanism that keeps the target warm. It is simply not the recovery point.
#
#   usage: RP=<recoverypoint> ./48-restore-redis-epoch.sh [outdir]
set -euo pipefail
cd "$(dirname "$0")"
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
NS="${NS:-ramp-demo}"
RAMPCTL="${RAMPCTL:-$(cd ../.. && pwd)/bin/rampctl}"
OUT="${1:-$(mktemp -d)}"
mkdir -p "$OUT"

K()  { kubectl --kubeconfig "$MGMT" "$@"; }
K2() { kubectl --kubeconfig "$WL02" "$@"; }
ts() { date -Is; }
jp() { K get recoverypoint "$RP" -o jsonpath="$1"; }

RP="${RP:?RP (RecoveryPoint name) must be set}"
PHASE=$(jp '{.status.phase}')
[ "$PHASE" = "Committed" ] || { echo "RecoveryPoint $RP is $PHASE, not Committed; refusing"; exit 1; }

EPOCH=$(jp '{.spec.epoch}')
P=$(jp '{.status.logicalPosition}')
REF=$(jp '{.status.artifacts[?(@.type=="redisSnapshot")].ref}')
SUM=$(jp '{.status.artifacts[?(@.type=="redisSnapshot")].checksum}')
[ -n "$REF" ] || { echo "RecoveryPoint $RP carries no immutable redisSnapshot artifact; refusing"; exit 1; }

echo "restoring redis from epoch=$EPOCH position=$P artifact=$REF"
echo "T_redis_restore_start=$(ts)" | tee -a "$OUT/timings.txt"

# 1. Fetch the immutable artifact and verify it is byte-for-byte the object the
#    RecoveryPoint committed. A checksum mismatch here means the "immutable"
#    claim is false and the recovery must not proceed.
MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:-nephio1234}" MINIO_SECRET_KEY="${MINIO_SECRET_KEY:-secret1234}" \
  "$RAMPCTL" --key "$REF" --sha256 "$SUM" --out "$OUT/redis-epoch-${EPOCH}.rdb"

# 2. Detach from any live replication stream FIRST. Restoring the artifact into
#    an instance that is still following the source would be pointless: the next
#    resync overwrites it.
K2 exec -n "$NS" deploy/redis-standby -- redis-cli REPLICAOF NO ONE >/dev/null

# 3. Place the epoch RDB. Redis only loads an RDB at startup, so this is staged
#    and then activated by a restart -- which is why /data is a volume.
base64 -w0 "$OUT/redis-epoch-${EPOCH}.rdb" | \
  K2 exec -i -n "$NS" deploy/redis-standby -- sh -c 'base64 -d > /data/dump.rdb && ls -l /data/dump.rdb'

# 4. Activate it. NOSAVE so the running (post-Q) dataset cannot overwrite the
#    artifact on the way out.
POD=$(K2 get pod -n "$NS" -l app=redis-standby -o jsonpath='{.items[0].metadata.name}')
K2 exec -n "$NS" "$POD" -- redis-cli SHUTDOWN NOSAVE >/dev/null 2>&1 || true

for _ in $(seq 1 60); do
  if K2 exec -n "$NS" "$POD" -- redis-cli PING 2>/dev/null | grep -q PONG; then break; fi
  sleep 1
done
echo "T_redis_ready=$(ts)" | tee -a "$OUT/timings.txt"

# 5. PROVE the restored state is the committed epoch, BEFORE anything is allowed
#    to write to it. Two independent witnesses, both carried inside the RDB:
#    the application position key, and the per-epoch marker RAMP wrote at the
#    barrier. Checking only the first would accept a replica that happened to
#    be at P; the marker ties the dataset to epoch E specifically.
ROLE=$(K2 exec -n "$NS" "$POD" -- redis-cli INFO replication | tr -d '\r' | awk -F: '/^role:/{print $2}')
REDIS_POS=$(K2 exec -n "$NS" "$POD" -- redis-cli GET ramp:video:position | tr -d '\r')
MARKER=$(K2 exec -n "$NS" "$POD" -- redis-cli GET "ramp:epoch:${EPOCH}:position" | tr -d '\r')
EPOCH_KEY=$(K2 exec -n "$NS" "$POD" -- redis-cli GET ramp:epoch | tr -d '\r')

{
  echo "redis_role=$ROLE"
  echo "redis_restored_position=$REDIS_POS"
  echo "recovery_point_position=$P"
  echo "epoch_marker_ramp:epoch:${EPOCH}:position=$MARKER"
  echo "epoch_marker_ramp:epoch=$EPOCH_KEY"
} | tee "$OUT/redis-restored-state.txt"

FAIL=0
[ "$ROLE" = "master" ] || { echo "FAIL: restored redis role=$ROLE"; FAIL=1; }
[ "$REDIS_POS" = "$P" ] || { echo "FAIL: restored redis position $REDIS_POS != recovery point position $P"; FAIL=1; }
[ "$MARKER" = "$P" ]    || { echo "FAIL: epoch marker $MARKER != $P"; FAIL=1; }
[ "$EPOCH_KEY" = "$EPOCH" ] || { echo "FAIL: dataset epoch marker $EPOCH_KEY != $EPOCH"; FAIL=1; }
[ "$FAIL" = 0 ] || exit 1
echo "REDIS RESTORED TO EPOCH $EPOCH POSITION $P (verified before any application write)"
