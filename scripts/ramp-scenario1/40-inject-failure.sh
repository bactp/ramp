#!/usr/bin/env bash
# Bounded, reversible source failure.
#
# Scaling the RecoveryGroup's two members to zero on workload01 removes exactly
# the thing under test -- the source application -- and nothing else. The
# cluster, its control plane, the checkpoint-agent, ArgoCD, Longhorn and the
# whole management plane stay up, so the experiment can be repeated without
# rebuilding the lab. Undo with 45-undo-failure.sh.
set -euo pipefail
WL01="${WL01:-$HOME/workload01.kubeconfig}"
NS="${NS:-ramp-demo}"

echo "T_failure=$(date -Is)"
kubectl --kubeconfig "$WL01" scale deploy/video-session deploy/redis-primary -n "$NS" --replicas=0
kubectl --kubeconfig "$WL01" wait --for=delete pod -l app=video-session -n "$NS" --timeout=90s 2>/dev/null || true
kubectl --kubeconfig "$WL01" wait --for=delete pod -l app=redis-primary -n "$NS" --timeout=90s 2>/dev/null || true
echo "T_failure_complete=$(date -Is)"
kubectl --kubeconfig "$WL01" get pods -n "$NS"
