#!/usr/bin/env bash
# Begin a Recovery Epoch for a RecoveryGroup.
#
# An epoch is a first-class object rather than a controller-internal step so
# that a failed epoch stays on record: the history of what was and was not
# recoverable is itself evidence.
set -euo pipefail

KUBECONFIG_MGMT="${KUBECONFIG_MGMT:-$HOME/mgmt.kubeconfig}"
GROUP="${1:-video-stream-rg}"
NS="${2:-default}"
# Optional YAML fragment merged into spec (e.g. quiesceVerifySeconds).
EXTRA_SPEC="${EXTRA_SPEC:-}"
# TEST ONLY: a JSON fault-injection directive, carried as an annotation rather
# than a spec field so that "corrupt this epoch" is not part of the API.
FAULT_INJECTION="${FAULT_INJECTION:-}"

# Next epoch = highest EXISTING RecoveryPoint epoch + 1, read from the objects
# themselves rather than from recoverygroup.status.latestEpoch. That status
# field only refreshes on the group controller's resync interval, so two epochs
# started back to back both got the same number, the second `apply` landed on
# the first epoch's (already terminal) object, and the run silently reported the
# previous epoch's result.
CURRENT=$(kubectl --kubeconfig "$KUBECONFIG_MGMT" get recoverypoint -n "$NS" \
            -o jsonpath="{range .items[?(@.spec.recoveryGroupRef=='${GROUP}')]}{.spec.epoch}{'\n'}{end}" 2>/dev/null \
          | sort -n | tail -1)
EPOCH=$(( ${CURRENT:-0} + 1 ))
NAME="${GROUP}-epoch-${EPOCH}"

echo "T_epoch_requested=$(date -Is)"
cat <<YAML | kubectl --kubeconfig "$KUBECONFIG_MGMT" apply -f -
apiVersion: ramp.dcn.ssu.ac.kr/v1alpha1
kind: RecoveryPoint
metadata:
  name: ${NAME}
  namespace: ${NS}
  annotations:
    ramp.dcn.ssu.ac.kr/test-fault-injection: '${FAULT_INJECTION}'
spec:
  recoveryGroupRef: ${GROUP}
  epoch: ${EPOCH}
  barrierTimeoutSeconds: ${BARRIER_TIMEOUT:-30}
  captureTimeoutSeconds: 300
  # Zero, not two. The old tolerance existed because the application kept
  # running during capture, so the two artifacts could only ever be "close".
  # With a real quiesce they are the same position or the epoch is wrong.
  maxPositionSkew: 0
  requiredReplicaAcks: 1
  quiesceVerifySeconds: 3
${EXTRA_SPEC}
YAML

echo "waiting for epoch ${EPOCH} to reach a terminal phase..."
for _ in $(seq 1 150); do
  PHASE=$(kubectl --kubeconfig "$KUBECONFIG_MGMT" get recoverypoint "$NAME" -n "$NS" \
            -o jsonpath='{.status.phase}' 2>/dev/null || true)
  case "$PHASE" in
    Committed|Failed) break ;;
  esac
  sleep 2
done

kubectl --kubeconfig "$KUBECONFIG_MGMT" get recoverypoint "$NAME" -n "$NS" -o yaml
echo "T_epoch_observed=$(date -Is)"
echo "recovery_point=${NAME}"
[ "${EXPECT_PHASE:-Committed}" = "$(kubectl --kubeconfig "$KUBECONFIG_MGMT" get recoverypoint "$NAME" -n "$NS" -o jsonpath='{.status.phase}')" ]
