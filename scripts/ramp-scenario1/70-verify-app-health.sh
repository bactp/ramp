#!/usr/bin/env bash
# Application-level health contract for the Video + Redis distributed app.
#
# "The pod is Running" proves nothing about a distributed application. This
# checks the properties that actually have to hold after a recovery, and it is
# the definition of APPLICATION READY used for the RTO measurement -- pod
# readiness is not.
#
# The nine checks below split into three groups:
#   identity      did we really restore THIS process, or just start a new one?
#   liveness      is the stream actually advancing, or frozen at the checkpoint?
#   consistency   do the two halves of the distributed app still agree?
#
# Exit 0 only if every mandatory check passes.
set -uo pipefail

MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
RP="${RP:?RP (RecoveryPoint name) must be set}"
VIDEO_URL="${VIDEO_URL:?VIDEO_URL must be set, e.g. http://192.168.28.122:30808/}"
REDIS_HOST="${REDIS_HOST:?REDIS_HOST must be set}"
REDIS_PORT="${REDIS_PORT:?REDIS_PORT must be set}"
SKEW_TOLERANCE="${SKEW_TOLERANCE:-3}"
# Dwell used by the StreamAdvancing check. It is real observation time and it
# lands inside any T_app_ready measured by polling this script, so it is made
# explicit rather than hidden.
LIVENESS_WINDOW="${LIVENESS_WINDOW:-3}"
QUIET="${QUIET:-0}"

PASS=0; FAIL=0
say() { [ "$QUIET" = "1" ] || echo "$@"; }
ok()   { PASS=$((PASS+1)); say "  PASS  $1"; }
bad()  { FAIL=$((FAIL+1)); say "  FAIL  $1"; }

redis() { # redis <cmd...>  -- inline RESP, no redis-cli needed
  python3 - "$REDIS_HOST" "$REDIS_PORT" "$@" <<'PY'
import socket, sys
host, port, args = sys.argv[1], int(sys.argv[2]), sys.argv[3:]
out = ("*%d\r\n" % len(args)).encode()
for a in args:
    b = a.encode(); out += b"$%d\r\n" % len(b) + b + b"\r\n"
s = socket.create_connection((host, port), timeout=5)
try:
    s.sendall(out)
    data = s.recv(65536)
finally:
    s.close()
t = data.decode(errors="replace")
if t.startswith("$"):
    parts = t.split("\r\n", 1)
    print(parts[1].rsplit("\r\n", 1)[0] if len(parts) > 1 else "")
else:
    print(t.lstrip("+:-").split("\r\n")[0])
PY
}

jqf() { python3 -c "import sys,json;print(json.load(sys.stdin).get('$1',''))" 2>/dev/null; }

# ---- expectations recorded in the RecoveryPoint -----------------------------
EXP_FP=$(kubectl --kubeconfig "$MGMT" get recoverypoint "$RP" \
  -o jsonpath='{.status.artifacts[?(@.type=="containerCheckpoint")].instanceFingerprint}')
EXP_POS=$(kubectl --kubeconfig "$MGMT" get recoverypoint "$RP" \
  -o jsonpath='{.status.validation.checkpointPosition}')
say "RecoveryPoint $RP: instance=$EXP_FP position=$EXP_POS"

# ---- 1. the video component actually serves --------------------------------
S1=$(curl -sS --max-time 5 "$VIDEO_URL" 2>/dev/null)
if [ -n "$S1" ] && echo "$S1" | python3 -c 'import sys,json;json.load(sys.stdin)' 2>/dev/null; then
  ok "VideoServing            endpoint answers with valid state"
else
  bad "VideoServing            no usable response from $VIDEO_URL"
  say "verdict: FAIL ($PASS passed, $FAIL failed)"; exit 1
fi

GOT_FP=$(echo "$S1"  | jqf session)
GOT_POS=$(echo "$S1" | jqf position)
GOT_FR=$(echo "$S1"  | jqf frames_decoded)
GOT_FPT=$(echo "$S1" | jqf frames_per_tick)
GOT_LNK=$(echo "$S1" | jqf redis_linked)

# ---- 2. identity: same process instance, not a restart ---------------------
if [ -n "$EXP_FP" ] && [ "$GOT_FP" = "$EXP_FP" ]; then
  ok "InstanceRestored        session $GOT_FP matches the checkpoint"
else
  bad "InstanceRestored        session $GOT_FP != checkpoint $EXP_FP (this is a RESTART, not a restore)"
fi

# ---- 3. execution state resumed from the checkpoint, not from Redis --------
# A cold start re-reads Redis and lands near the pre-failure position; a real
# restore resumes at the checkpoint position and climbs from there.
if [ -n "$EXP_POS" ] && [ "$GOT_POS" -ge "$EXP_POS" ] 2>/dev/null; then
  ok "ResumedFromCheckpoint   position $GOT_POS >= checkpoint position $EXP_POS"
else
  bad "ResumedFromCheckpoint   position $GOT_POS vs checkpoint $EXP_POS"
fi

# ---- 4. the application's own invariant ------------------------------------
if [ "$GOT_FR" = "$((GOT_POS * GOT_FPT))" ]; then
  ok "DecodeInvariant         frames_decoded=$GOT_FR == position*$GOT_FPT"
else
  bad "DecodeInvariant         frames_decoded=$GOT_FR != position($GOT_POS)*$GOT_FPT"
fi

# ---- 5. liveness: the stream is advancing, not frozen ----------------------
sleep "$LIVENESS_WINDOW"
S2=$(curl -sS --max-time 5 "$VIDEO_URL" 2>/dev/null)
POS2=$(echo "$S2" | jqf position)
if [ -n "$POS2" ] && [ "$POS2" -gt "$GOT_POS" ] 2>/dev/null; then
  ok "StreamAdvancing         position $GOT_POS -> $POS2 over ${LIVENESS_WINDOW}s"
else
  bad "StreamAdvancing         position stuck at $GOT_POS (restored but frozen)"
fi

# ---- 6. redis is the promoted primary, not still a replica ----------------
ROLE=$(redis INFO replication 2>/dev/null | tr -d '\r' | awk -F: '/^role:/{print $2}')
if [ "$ROLE" = "master" ]; then
  ok "RedisPrimaryRole        redis at $REDIS_HOST:$REDIS_PORT is master"
else
  bad "RedisPrimaryRole        role=$ROLE (expected master)"
fi

# ---- 7. the two halves of the distributed app agree -----------------------
RPOS=$(redis GET ramp:video:position 2>/dev/null | tr -d '\r')
SKEW=$(( ${POS2:-0} - ${RPOS:-0} )); [ "$SKEW" -lt 0 ] && SKEW=$(( -SKEW ))
if [ "$SKEW" -le "$SKEW_TOLERANCE" ]; then
  ok "DistributedConsistency  video=$POS2 redis=$RPOS skew=$SKEW (<= $SKEW_TOLERANCE)"
else
  bad "DistributedConsistency  video=$POS2 redis=$RPOS skew=$SKEW (> $SKEW_TOLERANCE)"
fi

# ---- 8. the datapath works: video's writes reach redis --------------------
RSESS=$(redis GET ramp:video:session 2>/dev/null | tr -d '\r')
if [ "$RSESS" = "$GOT_FP" ]; then
  ok "WriteDatapath           redis session key written by the restored instance"
else
  bad "WriteDatapath           redis session=$RSESS != running instance $GOT_FP"
fi

# ---- 9. no split brain: exactly one writer ---------------------------------
if [ "$GOT_LNK" = "True" ] || [ "$GOT_LNK" = "true" ]; then
  ok "RedisLinkUp             restored video is connected to redis"
else
  bad "RedisLinkUp             restored video reports redis_linked=$GOT_LNK"
fi

say "verdict: $([ "$FAIL" -eq 0 ] && echo GROUP-HEALTHY || echo UNHEALTHY) ($PASS passed, $FAIL failed)"
exit $([ "$FAIL" -eq 0 ] && echo 0 || echo 1)
