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

# Next epoch = highest observed + 1. The RecoveryGroup controller tracks the
# high-water mark including failed epochs, so numbers are never reused.
CURRENT=$(kubectl --kubeconfig "$KUBECONFIG_MGMT" get recoverygroup "$GROUP" -n "$NS" \
            -o jsonpath='{.status.latestEpoch}' 2>/dev/null || echo 0)
EPOCH=$(( ${CURRENT:-0} + 1 ))
NAME="${GROUP}-epoch-${EPOCH}"

echo "T_epoch_requested=$(date -Is)"
cat <<YAML | kubectl --kubeconfig "$KUBECONFIG_MGMT" apply -f -
apiVersion: ramp.dcn.ssu.ac.kr/v1alpha1
kind: RecoveryPoint
metadata:
  name: ${NAME}
  namespace: ${NS}
spec:
  recoveryGroupRef: ${GROUP}
  epoch: ${EPOCH}
  barrierTimeoutSeconds: 30
  captureTimeoutSeconds: 300
  maxPositionSkew: 2
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
[ "$(kubectl --kubeconfig "$KUBECONFIG_MGMT" get recoverypoint "$NAME" -n "$NS" -o jsonpath='{.status.phase}')" = "Committed" ]
