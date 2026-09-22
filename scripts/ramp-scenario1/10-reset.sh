#!/usr/bin/env bash
# Return the testbed to the pre-failure steady state so a run is repeatable.
set -euo pipefail
WL01="${WL01:-$HOME/workload01.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
NS=ramp-demo
K1() { kubectl --kubeconfig "$WL01" "$@"; }
K2() { kubectl --kubeconfig "$WL02" "$@"; }

# The target workload is owned by ArgoCD with selfHeal: true, so deleting the
# Deployment is useless -- it is recreated from the DR repo within seconds. The
# steady state has to be restored in GIT, by parking the recovery manifest back
# at replicas: 0, and only then is the object allowed to go away.
echo "-- parking the target recovery manifest back to replicas: 0 --"
GITEA="${GITEA:-http://192.168.28.158:32100}"; OWNER="${OWNER:-nephio}"
APP=video-session; TGT_CLUSTER=workload02; TARGET_NODE="${TARGET_NODE:-workload02-md-0-rx5mn-pjkrh}"
DR_PATH="${TGT_CLUSTER}-dr/${APP}"
GU=$(kubectl --kubeconfig "${MGMT:-$HOME/mgmt.kubeconfig}" get secret git-user-secret -n default -o jsonpath='{.data.username}' | base64 -d)
GP=$(kubectl --kubeconfig "${MGMT:-$HOME/mgmt.kubeconfig}" get secret git-user-secret -n default -o jsonpath='{.data.password}' | base64 -d)
. "$(dirname "$0")/lib-gitea.sh"
. "$(dirname "$0")/lib-recovery-manifest.sh"
if [ -n "$(file_sha dr "${DR_PATH}/deployment.yaml")" ]; then
  W=$(mktemp -d)
  render_recovery_manifests "$W" 0 "python:3.12-slim" IfNotPresent prepared
  put_file dr "${DR_PATH}/deployment.yaml" "$W/deployment.yaml" "RAMP reset: park ${APP} at replicas 0"
  rm -rf "$W"
  trigger_sync "$WL02" "${TGT_CLUSTER}-dr"
  for _ in $(seq 1 30); do
    R=$(K2 get deploy $APP -n $NS -o jsonpath='{.spec.replicas}' 2>/dev/null)
    [ "$R" = "0" ] && break
    sleep 2
  done
  echo "   target $APP replicas=$(K2 get deploy $APP -n $NS -o jsonpath='{.spec.replicas}' 2>/dev/null)"
fi
echo "-- clearing completed helper Jobs and probes on both clusters --"
K2 delete job -n $NS -l ramp.dcn.ssu.ac.kr/component=video-restore --ignore-not-found >/dev/null 2>&1 || true
K2 delete job -n $NS -l ramp.dcn.ssu.ac.kr/component=stage-action  --ignore-not-found >/dev/null 2>&1 || true
K2 delete job ramp-imgbuild -n $NS --ignore-not-found >/dev/null 2>&1 || true
K2 delete pod -n $NS -l ramp.dcn.ssu.ac.kr/component=stage-probe --ignore-not-found >/dev/null 2>&1 || true
K1 delete job -n $NS -l ramp.dcn.ssu.ac.kr/component=video-restore --ignore-not-found >/dev/null 2>&1 || true
K1 delete job ramp-video-restore -n $NS --ignore-not-found >/dev/null 2>&1 || true

echo "-- bringing the source application back up --"
K1 scale deploy/redis-primary deploy/video-session -n $NS --replicas=1
K1 rollout status deploy/redis-primary -n $NS --timeout=120s
K1 rollout status deploy/video-session -n $NS --timeout=120s

echo "-- re-establishing cross-cluster replication --"
K2 exec -n $NS deploy/redis-standby -- redis-cli REPLICAOF 192.168.28.238 30379
for _ in $(seq 1 60); do
  LINK=$(K2 exec -n $NS deploy/redis-standby -- redis-cli INFO replication 2>/dev/null \
          | tr -d '\r' | awk -F: '/master_link_status/{print $2}')
  [ "$LINK" = "up" ] && break
  sleep 2
done
echo "replication link: ${LINK:-unknown}"
K2 exec -n $NS deploy/redis-standby -- redis-cli INFO replication | tr -d '\r' | head -6
