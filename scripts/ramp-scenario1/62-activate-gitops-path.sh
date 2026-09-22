#!/usr/bin/env bash
# ACTIVATE half of the GitOps recovery driver. This is the only part on the RTO
# path, and it is deliberately as small as possible:
#
#   one git commit  (image -> checkpoint image, replicas 0 -> 1)
#   one sync trigger
#
# Everything else -- the Application, the workload object, the Service, the
# staged artifact, the checkpoint image on the node -- was paid for in
# 61-prepare-gitops-path.sh while the source was still healthy.
set -euo pipefail

GITEA="${GITEA:-http://192.168.28.158:32100}"
OWNER="${OWNER:-nephio}"
SRC_CLUSTER="${SRC_CLUSTER:-workload01}"
TGT_CLUSTER="${TGT_CLUSTER:-workload02}"
APP="${APP:-video-session}"
NS="${NS:-ramp-demo}"
CKPT_IMAGE="${CKPT_IMAGE:?CKPT_IMAGE must be set}"
TARGET_NODE="${TARGET_NODE:?TARGET_NODE must be set}"
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
TGT_KUBECONFIG="${TGT_KUBECONFIG:-$HOME/${TGT_CLUSTER}.kubeconfig}"
DR_PATH="${DR_PATH:-${TGT_CLUSTER}-dr/${APP}}"

GU=$(kubectl --kubeconfig "$MGMT" get secret git-user-secret -n default -o jsonpath='{.data.username}' | base64 -d)
GP=$(kubectl --kubeconfig "$MGMT" get secret git-user-secret -n default -o jsonpath='{.data.password}' | base64 -d)
. "$(dirname "$0")/lib-gitea.sh"
. "$(dirname "$0")/lib-recovery-manifest.sh"

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

# The entire activation: one file, two fields changed -- replicas 0 -> 1 and the
# image swapped to the checkpoint image that containerd will restore from.
render_recovery_manifests "$WORK" 1 "$CKPT_IMAGE" Never activated
put_file dr "${DR_PATH}/deployment.yaml" "$WORK/deployment.yaml" \
  "RAMP activate: restore ${APP} on ${TGT_CLUSTER} from ${CKPT_IMAGE}"
trigger_sync "$TGT_KUBECONFIG" "${TGT_CLUSTER}-dr"
