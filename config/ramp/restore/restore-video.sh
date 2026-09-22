#!/bin/bash
# Restore the video member's container from its staged CRIU checkpoint.
#
# Runs inside a privileged pod on the TARGET node and chroots into the host,
# because runc, criu and the staged artifact all live on the node. This is the
# concrete actuation behind the RecoveryPath action "restore video container
# from the staged checkpoint artifact". In the target architecture this is the
# Transition Operator's job; RAMP only decided that the path was executable.
set -euo pipefail

CKPT="${CKPT:?CKPT (checkpoint object name) must be set}"
STAGE_DIR="${STAGE_DIR:-/var/lib/kubelet/checkpoints}"
CTR_ID="${CTR_ID:-ramp-restored-video}"
W=/run/ramp-restore

mkdir -p /run/criuwork
echo "RESTORE_START=$(date -Is)"
# $W already holds the helper scripts copied in by the Job; clear only the
# working subdirectories so a re-run starts clean without deleting them.
rm -rf "$W/ckpt" "$W/bundle" "$W/oci" "$W/mounts"
mkdir -p "$W/ckpt" "$W/bundle/rootfs"
tar xf "$STAGE_DIR/$CKPT" -C "$W/ckpt"
echo "checkpoint extracted: $(ls "$W/ckpt/checkpoint" | wc -l) CRIU images"

IMG=$(python3 -c "import json;print(json.load(open('$W/ckpt/config.dump'))['rootfsImageName'])")
echo "rootfs image: $IMG"

# Materialise the rootfs the checkpointed process expects.
#
# `ctr images mount` is NOT used: on containerd 2.1.4 it reports success while
# leaving the destination empty, which then surfaces inside CRIU as an opaque
# "criu failed: type RESTORE errno 0" with no restore.log at all. Exporting the
# image and unpacking its layers is slower but verifiable, and the guard below
# turns an empty rootfs into a loud failure instead of a silent one.
ctr -n k8s.io images pull "$IMG" >/dev/null 2>&1 || true
rm -f "$W/img.tar"
ctr -n k8s.io images export --platform linux/amd64 "$W/img.tar" "$IMG"
mkdir -p "$W/oci" && tar xf "$W/img.tar" -C "$W/oci"
python3 "$W/unpack.py" "$W"

ENTRIES=$(ls "$W/bundle/rootfs" | wc -l)
echo "rootfs entries: $ENTRIES"
if [ "$ENTRIES" -eq 0 ]; then
  echo "FATAL: bundle rootfs is empty; CRIU cannot restore without the container filesystem"
  exit 1
fi

python3 "$W/mkspec.py" "$W"

runc delete -f "$CTR_ID" >/dev/null 2>&1 || true
echo "RUNC_RESTORE_START=$(date -Is)"
set +e
runc restore --image-path "$W/ckpt/checkpoint" --work-path /run/criuwork \
     --bundle "$W/bundle" --detach "$CTR_ID" 2>&1 | tail -25
RC=${PIPESTATUS[0]}
set -e
echo "RUNC_RESTORE_RC=$RC"
echo "RUNC_RESTORE_END=$(date -Is)"
for L in /run/criuwork/restore.log "$W/ckpt/checkpoint/restore.log"; do
  [ -f "$L" ] && { echo "--- criu $L ---"; grep -iE "Error|err:" "$L" | tail -15; }
done

runc list 2>/dev/null || true
if [ "$RC" -eq 0 ]; then
  sleep 4
  echo "--- restored in-memory state (read from the live restored container) ---"
  runc exec "$CTR_ID" cat /tmp/ramp-video-state.json 2>/dev/null \
    || cat "$W/bundle/rootfs/tmp/ramp-video-state.json" 2>/dev/null \
    || echo "state file unreadable"
fi
echo "RESTORE_END=$(date -Is)"
exit 0
