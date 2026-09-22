#!/usr/bin/env bash
set -euo pipefail
WL01="${WL01:-$HOME/workload01.kubeconfig}"
NS="${NS:-ramp-demo}"
kubectl --kubeconfig "$WL01" scale deploy/video-session deploy/redis-primary -n "$NS" --replicas=1
kubectl --kubeconfig "$WL01" rollout status deploy/redis-primary -n "$NS" --timeout=120s
kubectl --kubeconfig "$WL01" rollout status deploy/video-session -n "$NS" --timeout=120s
