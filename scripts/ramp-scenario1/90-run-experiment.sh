#!/usr/bin/env bash
# One full, reproducible recovery experiment.
#
#   MODE=baseline    everything happens at failure time
#   MODE=optimized   ArgoCD wiring, workload object, Service, staged artifact and
#                    checkpoint image are all prepared while the source is healthy
#
# Both end with the same nine application-level health checks, and both report
# RTO to APPLICATION ready -- not to pod ready.
set -uo pipefail
cd "$(dirname "$0")"
MODE="${MODE:-optimized}"
# MODE=poll behaves like baseline but never asks ArgoCD to sync, so the recovery
# waits for ArgoCD's own reconciliation interval.
if [ "$MODE" = "poll" ]; then export RAMP_NO_TRIGGER=1; RUN_MODE=baseline; else RUN_MODE="$MODE"; fi
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
NODE="${TARGET_NODE:-workload02-md-0-rx5mn-pjkrh}"
NS=ramp-demo

echo "########## experiment: MODE=$MODE ##########"
./10-reset.sh >/dev/null 2>&1

# A fresh epoch, so the fingerprint belongs to the process now running.
./20-run-epoch.sh video-stream-rg default >/tmp/ramp-epoch.txt 2>&1 || { echo "epoch failed"; tail -5 /tmp/ramp-epoch.txt; exit 1; }
RPN=$(awk '/^  name: video-stream-rg-epoch-/{print $2; exit}' /tmp/ramp-epoch.txt)
OBJ=$(kubectl --kubeconfig "$MGMT" get recoverypoint "$RPN" -o jsonpath='{.status.artifacts[?(@.type=="containerCheckpoint")].ref}'); OBJ="${OBJ##*/}"
TAG="docker.io/ramp/checkpoint-video-session:${RPN}"
echo "recovery point=$RPN"

# Artifact staging and image build are PREPARATION in both modes -- without the
# image on the node there is nothing to restore from at all.
RP_NAME="$RPN" ./30-prepare-path.sh >/dev/null 2>&1
kubectl --kubeconfig "$WL02" delete job ramp-imgbuild -n $NS --ignore-not-found >/dev/null 2>&1
cat <<YAML | kubectl --kubeconfig "$WL02" apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: ramp-imgbuild, namespace: ${NS}}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: Never
      nodeName: ${NODE}
      volumes: [{name: host, hostPath: {path: /, type: Directory}},{name: s, configMap: {name: ramp-imgbuild}}]
      containers:
        - name: b
          image: busybox:1.36
          securityContext: {privileged: true}
          volumeMounts: [{name: host, mountPath: /host},{name: s, mountPath: /ramp}]
          command: ["/bin/sh","-c"]
          args: ["cp /ramp/build-checkpoint-image.py /host/run/ && chroot /host sh -c \\"ctr -n k8s.io images rm ${TAG} >/dev/null 2>&1; python3 /run/build-checkpoint-image.py /var/lib/kubelet/checkpoints/${OBJ} ${TAG} docker.io/library/python:3.12-slim\\""]
YAML
kubectl --kubeconfig "$WL02" wait --for=condition=complete job/ramp-imgbuild -n $NS --timeout=120s >/dev/null 2>&1

if [ "$RUN_MODE" = "baseline" ]; then
  # A real baseline must start with NOTHING prepared on the target. The DR
  # Application prunes, so removing the manifests from git removes the
  # Deployment and Service too.
  GU=$(kubectl --kubeconfig "$MGMT" get secret git-user-secret -n default -o jsonpath='{.data.username}' | base64 -d)
  GP=$(kubectl --kubeconfig "$MGMT" get secret git-user-secret -n default -o jsonpath='{.data.password}' | base64 -d)
  GITEA=http://192.168.28.158:32100; OWNER=nephio
  . ./lib-gitea.sh
  for f in workload02-dr/video-session/deployment.yaml workload02-dr/video-session/svc.yaml; do
    SHA=$(file_sha dr "$f")
    [ -n "$SHA" ] && curl -sS -u "$GU:$GP" -X DELETE -H 'Content-Type: application/json' \
      -d "{\"sha\":\"$SHA\",\"message\":\"RAMP: unprepare for baseline\"}" \
      "$GITEA/api/v1/repos/$OWNER/dr/contents/$f" >/dev/null
  done
  RAMP_NO_TRIGGER=0 trigger_sync "$WL02" workload02-dr HEAD
  for _ in $(seq 1 30); do
    kubectl --kubeconfig "$WL02" get deploy video-session -n $NS >/dev/null 2>&1 || break
    sleep 2
  done
  echo "unprepared: target deployment $(kubectl --kubeconfig "$WL02" get deploy video-session -n $NS >/dev/null 2>&1 && echo STILL-PRESENT || echo absent)"
fi

if [ "$RUN_MODE" = "optimized" ]; then
  TARGET_NODE="$NODE" ./61-prepare-gitops-path.sh >/tmp/ramp-prepare.txt 2>&1
  for _ in $(seq 1 20); do
    R=$(kubectl --kubeconfig "$WL02" get deploy video-session -n $NS -o jsonpath='{.spec.replicas}' 2>/dev/null)
    [ "$R" = "0" ] && break
    sleep 3
  done
  echo "prepared: target deployment replicas=$(kubectl --kubeconfig "$WL02" get deploy video-session -n $NS -o jsonpath='{.spec.replicas}' 2>/dev/null)"
fi

MODE="$RUN_MODE" RP="$RPN" CKPT_IMAGE="$TAG" TARGET_NODE="$NODE" ./80-measure-recovery.sh
