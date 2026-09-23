#!/usr/bin/env bash
# Prepare the target for ONE named RecoveryPoint, on ONE named node.
#
# This is the whole preparation half of a recovery, in the order the readiness
# checks verify it:
#
#   1. stage the container checkpoint tar on the placement node   (30-...)
#   2. build the CRI checkpoint image from it, on that node       (35-...)
#   3. put the activation plan in git and let ArgoCD converge it  (61-...)
#
# None of this is on the failure-time path. That is the point: after it, the
# RecoveryPath's CandidateRecoveryPoint is promotable to PreparedRecoveryPoint
# and failure-time work is one commit plus one sync.
#
# Every step is told WHICH RecoveryPoint it is preparing. Nothing here infers it
# from "the latest committed point right now" -- that inference is what let an
# earlier experiment stage epoch N-1's checkpoint and then fail to restore.
#
#   usage: RP=<recoverypoint> [TARGET_NODE=<node>] ./36-prepare-target.sh
set -euo pipefail
cd "$(dirname "$0")"
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
PATH_NAME="${PATH_NAME:-video-workload01-to-workload02}"
NS="${NS:-ramp-demo}"
RP="${RP:?RP (RecoveryPoint name) must be set}"

K() { kubectl --kubeconfig "$MGMT" "$@"; }

PHASE=$(K get recoverypoint "$RP" -o jsonpath='{.status.phase}')
[ "$PHASE" = "Committed" ] || { echo "RecoveryPoint $RP is $PHASE, not Committed; refusing to prepare for it"; exit 1; }
EPOCH=$(K get recoverypoint "$RP" -o jsonpath='{.spec.epoch}')

# The node comes from the path's own placement decision, so preparation and
# readiness cannot disagree about where the recovery lands. Artifact readiness
# proven on one node is not readiness for another.
if [ -z "${TARGET_NODE:-}" ]; then
  TARGET_NODE=$(K get recoverypath "$PATH_NAME" -o jsonpath='{.status.targetPlacement.node}')
fi
[ -n "$TARGET_NODE" ] || { echo "no target placement node available; set TARGET_NODE or wait for the path to select one"; exit 1; }

echo "preparing $PATH_NAME for $RP (epoch $EPOCH) on node $TARGET_NODE"
echo "T_prepare_start=$(date -Is)"

echo "--- 1/3 staging the checkpoint artifact ---"
RP_NAME="$RP" TARGET_NODE="$TARGET_NODE" ./30-prepare-path.sh "$PATH_NAME" default

echo "--- 2/3 building the CRI checkpoint image on $TARGET_NODE ---"
CKPT_IMAGE=$(RP="$RP" TARGET_NODE="$TARGET_NODE" ./35-build-checkpoint-image.sh | awk -F= '/^CKPT_IMAGE=/{print $2}')
[ -n "$CKPT_IMAGE" ] || { echo "checkpoint image build produced no tag"; exit 1; }
echo "checkpoint image: $CKPT_IMAGE"

echo "--- 3/3 preparing the activation plan ---"
RP="$RP" CKPT_IMAGE="$CKPT_IMAGE" TARGET_NODE="$TARGET_NODE" ./61-prepare-gitops-path.sh

# The plan is converged when the target workload object carries THIS
# RecoveryPoint's attribution. Waiting for "the deployment exists" would accept
# a leftover from a previous epoch.
for _ in $(seq 1 60); do
  GOT=$(kubectl --kubeconfig "$WL02" get deploy video-session -n "$NS" \
          -o jsonpath='{.metadata.annotations.ramp\.dcn\.ssu\.ac\.kr/prepared-recovery-point}' 2>/dev/null || true)
  [ "$GOT" = "$RP" ] && break
  sleep 2
done
echo "activation plan prepared for: ${GOT:-<none>} (expected $RP)"
echo "T_prepare_complete=$(date -Is)"
echo "CKPT_IMAGE=${CKPT_IMAGE}"
