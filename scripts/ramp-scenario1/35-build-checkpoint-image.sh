#!/usr/bin/env bash
# PREPARATION: turn a RecoveryPoint's container-checkpoint artifact into the CRI
# checkpoint image containerd restores from, on the target node.
#
# Extracted from 90-run-experiment.sh so that every flow that restores a video
# member builds the image the same way. This is preparation, not activation: it
# happens while the source is still healthy and is not on the recovery path.
#
#   usage: RP=<recoverypoint> TARGET_NODE=<node> ./35-build-checkpoint-image.sh
#   prints: CKPT_IMAGE=<tag>
set -euo pipefail
cd "$(dirname "$0")"
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
NS="${NS:-ramp-demo}"
RP="${RP:?RP must be set}"
TARGET_NODE="${TARGET_NODE:?TARGET_NODE must be set}"
ROOTFS_IMAGE="${ROOTFS_IMAGE:-docker.io/library/python:3.12-slim}"

K2() { kubectl --kubeconfig "$WL02" "$@"; }

REF=$(kubectl --kubeconfig "$MGMT" get recoverypoint "$RP" \
        -o jsonpath='{.status.artifacts[?(@.type=="containerCheckpoint")].ref}')
OBJ="${REF##*/}"
TAG="${CKPT_IMAGE:-docker.io/ramp/checkpoint-video-session:${RP}}"
echo "building $TAG from $OBJ on $TARGET_NODE" >&2

K2 create configmap ramp-imgbuild -n "$NS" \
   --from-file=../../config/ramp/restore/build-checkpoint-image.py \
   --dry-run=client -o yaml | K2 apply -f - >/dev/null
K2 delete job ramp-imgbuild -n "$NS" --ignore-not-found >/dev/null 2>&1
cat <<YAML | K2 apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: ramp-imgbuild, namespace: ${NS}}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: Never
      nodeName: ${TARGET_NODE}
      volumes: [{name: host, hostPath: {path: /, type: Directory}},{name: s, configMap: {name: ramp-imgbuild}}]
      containers:
        - name: b
          image: busybox:1.36
          securityContext: {privileged: true}
          volumeMounts: [{name: host, mountPath: /host},{name: s, mountPath: /ramp}]
          command: ["/bin/sh","-c"]
          args: ["cp /ramp/build-checkpoint-image.py /host/run/ && chroot /host sh -c \"ctr -n k8s.io images rm ${TAG} >/dev/null 2>&1; python3 /run/build-checkpoint-image.py /var/lib/kubelet/checkpoints/${OBJ} ${TAG} ${ROOTFS_IMAGE}\""]
YAML
K2 wait --for=condition=complete job/ramp-imgbuild -n "$NS" --timeout=180s >/dev/null 2>&1 \
  || { echo "checkpoint image build FAILED" >&2; K2 logs job/ramp-imgbuild -n "$NS" >&2 || true; exit 1; }
K2 logs job/ramp-imgbuild -n "$NS" >&2 2>/dev/null || true
echo "CKPT_IMAGE=${TAG}"
