#!/usr/bin/env bash
# Measure recovery to APPLICATION READY, not pod ready.
#
# Pod readiness is a Kubernetes fact; it says nothing about whether a
# distributed application is correct. The number reported here is the first
# instant at which 70-verify-app-health.sh passes every mandatory check --
# the restored instance is serving, is the instance that was checkpointed, is
# advancing, and agrees with Redis.
set -uo pipefail
cd "$(dirname "$0")"

MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL01="${WL01:-$HOME/workload01.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
RP="${RP:?RP must be set}"
# baseline  = everything happens at failure time (naive GitOps restore)
# optimized = Application/workload/artifact pre-staged by 61-prepare-gitops-path.sh
MODE="${MODE:-optimized}"
# The polling loop pays the probe dwell on every attempt, so use a short window
# while searching for the instant, then re-run the full check for the record.
PROBE_WINDOW="${PROBE_WINDOW:-1}"
# The ArgoCD Application that actually owns the recovery manifests. Getting this
# name wrong does not fail loudly -- the jsonpath just returns empty and the
# wait loop runs to exhaustion, which is exactly how a measurement bug once
# showed up as a flat "61s ArgoCD sync" in three consecutive runs.
ARGO_APP="${ARGO_APP:-${TGT_CLUSTER:-workload02}-dr}"
CKPT_IMAGE="${CKPT_IMAGE:?CKPT_IMAGE must be set}"
TARGET_NODE="${TARGET_NODE:?TARGET_NODE must be set}"
NS=ramp-demo
VIDEO_URL="${VIDEO_URL:-http://192.168.28.122:30808/}"
TGT_REDIS_HOST="${TGT_REDIS_HOST:-192.168.28.122}"
TGT_REDIS_PORT="${TGT_REDIS_PORT:-30380}"
OUT="${OUT:-$(cd ../.. && pwd)/evidence/scenario1/recovery-$(date +%Y%m%dT%H%M%S)}"
mkdir -p "$OUT"; T="$OUT/timings.txt"

ms() { date +%s.%3N; }
mark() { echo "$1=$(date -Is) epoch=$(ms)" | tee -a "$T"; }
el() { python3 -c "print('%.1fs' % ($2-$1))"; }

echo "mode=$MODE  evidence -> $OUT"
curl -sS --max-time 5 "http://192.168.28.238:30808/" > "$OUT/00-prefailure-source-state.json" 2>&1 || true
cat "$OUT/00-prefailure-source-state.json"; echo

# ---- failure ---------------------------------------------------------------
T0=$(ms); mark T_failure
kubectl --kubeconfig "$WL01" scale deploy/video-session deploy/redis-primary -n $NS --replicas=0 >/dev/null
kubectl --kubeconfig "$WL01" wait --for=delete pod -l app=video-session -n $NS --timeout=90s >/dev/null 2>&1 || true
kubectl --kubeconfig "$WL01" wait --for=delete pod -l app=redis-primary -n $NS --timeout=90s >/dev/null 2>&1 || true
T1=$(ms); mark T_failure_complete

# ---- recovery --------------------------------------------------------------
R0=$(ms); mark T_recovery_start
kubectl --kubeconfig "$WL02" exec -n $NS deploy/redis-standby -- redis-cli REPLICAOF NO ONE >/dev/null 2>&1
for _ in $(seq 1 60); do
  ROLE=$(kubectl --kubeconfig "$WL02" exec -n $NS deploy/redis-standby -- redis-cli INFO replication 2>/dev/null | tr -d '\r' | awk -F: '/^role:/{print $2}')
  [ "$ROLE" = "master" ] && break
  sleep 0.3
done
R1=$(ms); mark T_redis_ready

if [ "$MODE" = "baseline" ]; then
  CKPT_IMAGE="$CKPT_IMAGE" TARGET_NODE="$TARGET_NODE" ./60-gitops-restore.sh > "$OUT/10-activate.txt" 2>&1
else
  CKPT_IMAGE="$CKPT_IMAGE" TARGET_NODE="$TARGET_NODE" ./62-activate-gitops-path.sh > "$OUT/10-activate.txt" 2>&1
fi
R2=$(ms); mark T_git_committed

# ArgoCD has applied the activation when the target Deployment carries the
# checkpoint image and is scaled up. Fail loudly if the Application cannot be
# read at all, rather than silently waiting out the loop.
if ! kubectl --kubeconfig "$WL02" -n argocd get application "$ARGO_APP" >/dev/null 2>&1; then
  echo "FATAL: ArgoCD Application '$ARGO_APP' not found on $WL02" >&2; exit 1
fi
APPLIED=0
for _ in $(seq 1 200); do
  IMG=$(kubectl --kubeconfig "$WL02" get deploy video-session -n $NS -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
  N=$(kubectl --kubeconfig "$WL02" get deploy video-session -n $NS -o jsonpath='{.spec.replicas}' 2>/dev/null)
  if [ "$IMG" = "$CKPT_IMAGE" ] && [ "$N" = "1" ]; then APPLIED=1; break; fi
  sleep 0.2
done
[ "$APPLIED" = "1" ] || echo "WARNING: activation not observed within the wait window" >&2
R3=$(ms); mark T_argocd_applied

# pod ready (the number people usually quote)
for _ in $(seq 1 200); do
  RDY=$(kubectl --kubeconfig "$WL02" get pods -n $NS -l app=video-session -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)
  [ "$RDY" = "true" ] && break
  sleep 0.3
done
R4=$(ms); mark T_pod_ready

# APPLICATION ready: every distributed health check passes
for _ in $(seq 1 200); do
  if QUIET=1 LIVENESS_WINDOW="$PROBE_WINDOW" RP="$RP" VIDEO_URL="$VIDEO_URL" \
       REDIS_HOST="$TGT_REDIS_HOST" REDIS_PORT="$TGT_REDIS_PORT" \
       ./70-verify-app-health.sh >/dev/null 2>&1; then break; fi
  sleep 0.3
done
R5=$(ms); mark T_app_ready

RP="$RP" VIDEO_URL="$VIDEO_URL" REDIS_HOST="$TGT_REDIS_HOST" REDIS_PORT="$TGT_REDIS_PORT" \
  ./70-verify-app-health.sh 2>&1 | tee "$OUT/20-app-health.txt"
curl -sS --max-time 5 "$VIDEO_URL" > "$OUT/21-recovered-state.json" 2>&1 || true

{
  echo
  echo "===== RECOVERY BREAKDOWN ====="
  printf '  %-34s %s\n' "failure detection window"      "$(el $T0 $T1)"
  printf '  %-34s %s\n' "redis promotion"               "$(el $R0 $R1)"
  printf '  %-34s %s\n' "git commit (activate)"         "$(el $R1 $R2)"
  printf '  %-34s %s\n' "argocd sync + apply"           "$(el $R2 $R3)"
  printf '  %-34s %s\n' "pod ready (restore)"           "$(el $R3 $R4)"
  printf '  %-34s %s\n' "pod ready -> APPLICATION ready" "$(el $R4 $R5)"
  printf '  %-34s %s\n' "  (of which: probe dwell)"       "${PROBE_WINDOW}.0s"
  echo   "  ----------------------------------"
  printf '  %-34s %s\n' "RTO to pod ready"              "$(el $R0 $R4)"
  printf '  %-34s %s\n' "RTO to APPLICATION ready"      "$(el $R0 $R5)"
} | tee -a "$T"
