#!/usr/bin/env bash
# Execute the HOT RecoveryPath.
#
# Two members, two mechanisms, one recovery point:
#   redis -> native promotion of the standby that was already carrying the data
#   video -> CRIU restore of the container's in-memory execution state
# Neither mechanism could recover the other member, which is the whole reason
# they have to be coordinated as one RecoveryGroup.
set -euo pipefail
MGMT="${MGMT:-$HOME/mgmt.kubeconfig}"
WL02="${WL02:-$HOME/workload02.kubeconfig}"
PATH_NAME="${PATH_NAME:-video-workload01-to-workload02}"
NS="${NS:-ramp-demo}"

ts() { date -Is; }
K2() { kubectl --kubeconfig "$WL02" "$@"; }

READINESS=$(kubectl --kubeconfig "$MGMT" get recoverypath "$PATH_NAME" -o jsonpath='{.status.readiness}')
RP=$(kubectl --kubeconfig "$MGMT" get recoverypath "$PATH_NAME" -o jsonpath='{.status.observedRecoveryPoint}')
echo "executing path $PATH_NAME (readiness=$READINESS, recoveryPoint=$RP)"
[ "$READINESS" = "HOT" ] || echo "WARNING: path is $READINESS, not HOT; activation will take longer than estimatedRTO"

RP_POS=$(kubectl --kubeconfig "$MGMT" get recoverypoint "$RP" -o jsonpath='{.status.validation.checkpointPosition}')
CKPT_REF=$(kubectl --kubeconfig "$MGMT" get recoverypoint "$RP" \
             -o jsonpath='{.status.artifacts[?(@.type=="containerCheckpoint")].ref}')
CKPT_OBJ="${CKPT_REF##*/}"
echo "recovery point position = $RP_POS ; checkpoint artifact = $CKPT_OBJ"

echo "T_recovery_start=$(ts)"

# ---------------------------------------------------------------- redis ----
# Promotion, not restore. The data is already here; what changes is the role.
echo "--- promoting redis standby ---"
K2 exec -n "$NS" deploy/redis-standby -- redis-cli REPLICAOF NO ONE
for _ in $(seq 1 30); do
  ROLE=$(K2 exec -n "$NS" deploy/redis-standby -- redis-cli INFO replication 2>/dev/null | tr -d '\r' | awk -F: '/^role:/{print $2}')
  [ "$ROLE" = "master" ] && break
  sleep 1
done
REDIS_POS=$(K2 exec -n "$NS" deploy/redis-standby -- redis-cli GET ramp:video:position | tr -d '\r')
echo "T_redis_ready=$(ts)  role=$ROLE  position=$REDIS_POS"

# ---------------------------------------------------------------- video ----
# runc restore from the artifact staged on this node.
echo "--- restoring video container from checkpoint ---"
K2 delete job ramp-video-restore -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
K2 create configmap ramp-restore-script -n "$NS" \
   --from-file="$(dirname "$0")/../../config/ramp/restore/restore-video.sh" \
   --from-file="$(dirname "$0")/../../config/ramp/restore/unpack.py" \
   --from-file="$(dirname "$0")/../../config/ramp/restore/mkspec.py" \
   --dry-run=client -o yaml | K2 apply -f - >/dev/null

TARGET_NODE=$(K2 get pods -n "$NS" -l app=redis-standby -o jsonpath='{.items[0].spec.nodeName}')
cat <<YAML | K2 apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: ramp-video-restore, namespace: ${NS}}
spec:
  backoffLimit: 0
  template:
    metadata:
      labels: {ramp.dcn.ssu.ac.kr/component: video-restore}
    spec:
      restartPolicy: Never
      hostPID: true
      nodeName: ${TARGET_NODE}
      volumes:
        - name: host
          hostPath: {path: /, type: Directory}
        - name: script
          configMap: {name: ramp-restore-script, defaultMode: 0755}
      containers:
        - name: restore
          image: busybox:1.36
          securityContext: {privileged: true}
          env:
            - {name: CKPT, value: "${CKPT_OBJ}"}
          volumeMounts:
            - {name: host, mountPath: /host}
            - {name: script, mountPath: /ramp}
          command: ["/bin/sh","-c"]
          args:
            - mkdir -p /host/run/ramp-restore &&
              cp /ramp/unpack.py /ramp/mkspec.py /host/run/ramp-restore/ &&
              cp /ramp/restore-video.sh /host/run/ramp-restore-video.sh &&
              chroot /host /bin/bash /run/ramp-restore-video.sh
YAML
K2 wait --for=condition=complete job/ramp-video-restore -n "$NS" --timeout=240s 2>/dev/null \
  || K2 wait --for=condition=failed job/ramp-video-restore -n "$NS" --timeout=10s 2>/dev/null || true
K2 logs job/ramp-video-restore -n "$NS" 2>&1 | tail -40
echo "T_video_restore_attempt_done=$(ts)"
