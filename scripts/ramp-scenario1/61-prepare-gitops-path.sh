#!/usr/bin/env bash
# PREPARE half of the GitOps recovery driver (WARM -> HOT).
#
# The insight this script encodes: creating the ArgoCD Application is
# PREPARATION, not ACTIVATION. In the naive flow the recovery has to traverse
# two ArgoCD levels at failure time -- the app-of-apps notices a new file, then
# the child Application syncs the workload. Both can be paid BEFORE the failure.
#
# So PREPARE pushes the package and the Application with replicas: 0 and the
# ORIGINAL image, and lets ArgoCD converge on that. At failure time the workload
# object, the Application, the image and the artifact are all already in place;
# activation is one small commit plus one sync.
#
# This is readiness-as-an-elastic-resource made concrete: the cost moves out of
# the RTO and into a preparation step someone chose to pay for this path.
set -euo pipefail

GITEA="${GITEA:-http://192.168.28.158:32100}"
OWNER="${OWNER:-nephio}"
SRC_CLUSTER="${SRC_CLUSTER:-workload01}"
TGT_CLUSTER="${TGT_CLUSTER:-workload02}"
APP="${APP:-video-session}"
NS="${NS:-ramp-demo}"
ROOTFS_IMAGE="${ROOTFS_IMAGE:-python:3.12-slim}"
DR_PATH="${DR_PATH:-${TGT_CLUSTER}-dr/${APP}}"
TARGET_NODE="${TARGET_NODE:?TARGET_NODE must be set}"
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
TGT_KUBECONFIG="${TGT_KUBECONFIG:-$HOME/${TGT_CLUSTER}.kubeconfig}"

GU=$(kubectl --kubeconfig "$MGMT" get secret git-user-secret -n default -o jsonpath='{.data.username}' | base64 -d)
GP=$(kubectl --kubeconfig "$MGMT" get secret git-user-secret -n default -o jsonpath='{.data.password}' | base64 -d)
. "$(dirname "$0")/lib-gitea.sh"
. "$(dirname "$0")/lib-recovery-manifest.sh"

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
echo "T_prepare_start=$(date -Is)"

# Prepared state: the workload object, the Service and the ArgoCD wiring all
# exist on the target, scaled to zero and still on the ORIGINAL image. Nothing
# here is on the RTO path.
render_recovery_manifests "$WORK" 0 "$ROOTFS_IMAGE" IfNotPresent prepared
put_file dr "${DR_PATH}/deployment.yaml" "$WORK/deployment.yaml" "RAMP prepare: ${APP} scaled to zero on ${TGT_CLUSTER}"
put_file dr "${DR_PATH}/svc.yaml"        "$WORK/svc.yaml"        "RAMP prepare: ${APP} service on ${TGT_CLUSTER}"

# The target's DR Application recurses over <target>-dr/, so it picks these up
# directly. No child Application is needed, which also removes one ArgoCD level
# from the activation path.
trigger_sync "$TGT_KUBECONFIG" "${TGT_CLUSTER}-dr"
for _ in $(seq 1 60); do
  kubectl --kubeconfig "$TGT_KUBECONFIG" get deploy "$APP" -n "$NS" >/dev/null 2>&1 && break
  sleep 2
done
kubectl --kubeconfig "$TGT_KUBECONFIG" -n argocd get application "${TGT_CLUSTER}-dr" 2>&1 || true
kubectl --kubeconfig "$TGT_KUBECONFIG" get deploy,svc "$APP" -n "$NS" 2>&1 || true
echo "T_prepare_complete=$(date -Is)"
