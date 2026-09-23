#!/usr/bin/env bash
# Path-directed preparation: move ONE RecoveryPath from WARM to HOT.
#
# This is the elastic part of "readiness as a resource". Rather than every node
# continuously pulling every artifact in the store on a fixed period, RAMP
# stages exactly the artifact that THIS path's current RecoveryPoint needs,
# onto the target cluster, when someone decides this path is worth keeping hot.
#
# It is deliberately NOT done by the Readiness Controller. That controller's
# whole contract is OBSERVE -> evaluate -> publish; staging is an actuation.
# Promoting this script into a Preparation Controller is the natural next step
# (see docs/ramp-scenario1/04-results.md, "Next smallest step").
set -euo pipefail

KUBECONFIG_MGMT="${KUBECONFIG_MGMT:-$HOME/mgmt.kubeconfig}"
PATH_NAME="${1:-video-workload01-to-workload02}"
NS="${2:-default}"

get_path() { kubectl --kubeconfig "$KUBECONFIG_MGMT" get recoverypath "$PATH_NAME" -n "$NS" -o jsonpath="$1"; }

TARGET=$(get_path '{.spec.targetCluster}')
TGT_NS=$(get_path '{.spec.targetPrereqs.namespace}')
STAGE_DIR=$(get_path '{.spec.targetPrereqs.checkpointStagingPath}')
# The node the path has selected for placement; artifacts are staged THERE.
TARGET_NODE="${TARGET_NODE:-$(get_path '{.status.targetPlacement.node}')}"
# RP_NAME pins preparation to a specific RecoveryPoint, and callers are expected
# to pass it: preparing "whatever the path currently reports" is how an earlier
# experiment staged the PREVIOUS epoch's artifact. The fallback is the path's
# CANDIDATE point -- the one preparation is supposed to be working towards --
# and only then the prepared one, never the group's bare latest.
RP="${RP_NAME:-$(get_path '{.status.candidateRecoveryPoint.name}')}"
[ -n "$RP" ] || RP="$(get_path '{.status.preparedRecoveryPoint.name}')"

if [ -z "$RP" ]; then
  echo "path $PATH_NAME has no committed RecoveryPoint to prepare for; nothing to stage" >&2
  exit 1
fi

# Only containerCheckpoint artifacts need staging. A redis-replication member's
# recovery state is already on the target by construction -- that asymmetry is
# the point of having heterogeneous drivers.
REF=$(kubectl --kubeconfig "$KUBECONFIG_MGMT" get recoverypoint "$RP" -n "$NS" \
        -o jsonpath='{.status.artifacts[?(@.type=="containerCheckpoint")].ref}')
OBJ="${REF##*/}"
BUCKET=$(echo "$REF" | sed -E 's#^minio://([^/]+)/.*#\1#')

TGT_KUBECONFIG="${TGT_KUBECONFIG:-$HOME/${TARGET}.kubeconfig}"
JOB="ramp-stage-${OBJ:0:12}-$(echo "$OBJ" | md5sum | cut -c1-8)"
JOB=$(echo "$JOB" | tr '[:upper:]_.' '[:lower:]--' | cut -c1-60)

[ -n "$TARGET_NODE" ] || { echo "no placement node: set TARGET_NODE or wait for the RecoveryPath to select one" >&2; exit 1; }
echo "T_stage_start=$(date -Is)"
echo "staging $OBJ (bucket $BUCKET) onto $TARGET node $TARGET_NODE:$STAGE_DIR"

kubectl --kubeconfig "$TGT_KUBECONFIG" delete job "$JOB" -n "$TGT_NS" --ignore-not-found >/dev/null 2>&1 || true

cat <<YAML | kubectl --kubeconfig "$TGT_KUBECONFIG" apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB}
  namespace: ${TGT_NS}
  labels:
    ramp.dcn.ssu.ac.kr/component: stage-action
spec:
  backoffLimit: 2
  # Every epoch produces a new artifact and therefore a new staging Job. Without
  # a TTL they accumulate forever in the target namespace -- 12 of them piled up
  # in one afternoon of experiments before this was noticed.
  ttlSecondsAfterFinished: 300
  template:
    metadata:
      labels:
        ramp.dcn.ssu.ac.kr/component: stage-action
    spec:
      restartPolicy: Never
      # PINNED to the path's placement node. Letting the scheduler choose staged
      # the artifact on whichever worker was free, which is not necessarily the
      # node the restore runs on -- so the readiness evidence and the recovery
      # could refer to different machines.
      nodeName: ${TARGET_NODE}
      volumes:
        - name: staging
          hostPath:
            path: ${STAGE_DIR}
            type: DirectoryOrCreate
      containers:
        - name: stage
          # python:3.12-slim rather than minio/mc: the mc release tags are not
          # anonymously pullable from this testbed (verified: "pull access
          # denied ... insufficient_scope"), and this image is already in use
          # by the application under test, so it is known to pull here.
          # Stdlib-only AWS SigV4 GET keeps the stager dependency-free.
          image: python:3.12-slim
          command: ["python3","-u","-c"]
          args:
            - |
              import datetime, hashlib, hmac, os, sys, urllib.request

              host   = os.environ["MINIO_ENDPOINT"]
              bucket = os.environ["BUCKET"]
              key    = os.environ["OBJECT"]
              ak, sk = os.environ["MINIO_ACCESS_KEY"], os.environ["MINIO_SECRET_KEY"]
              dest   = "/staging/" + key

              if os.path.exists(dest) and os.path.getsize(dest) > 0:
                  print("already staged: %s (%d bytes)" % (dest, os.path.getsize(dest)))
                  sys.exit(0)

              t         = datetime.datetime.utcnow()
              amzdate   = t.strftime("%Y%m%dT%H%M%SZ")
              datestamp = t.strftime("%Y%m%d")
              ph        = hashlib.sha256(b"").hexdigest()
              path      = "/%s/%s" % (bucket, key)
              cr = "GET\n%s\n\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n\nhost;x-amz-content-sha256;x-amz-date\n%s" % (
                  urllib.parse.quote(path), host, ph, amzdate, ph)
              scope = "%s/us-east-1/s3/aws4_request" % datestamp
              sts   = "AWS4-HMAC-SHA256\n%s\n%s\n%s" % (amzdate, scope, hashlib.sha256(cr.encode()).hexdigest())
              def sign(k, m): return hmac.new(k, m.encode(), hashlib.sha256).digest()
              k   = sign(sign(sign(sign(("AWS4"+sk).encode(), datestamp), "us-east-1"), "s3"), "aws4_request")
              sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()

              req = urllib.request.Request(
                  "http://%s%s" % (host, urllib.parse.quote(path)),
                  headers={"Host": host, "x-amz-date": amzdate, "x-amz-content-sha256": ph,
                           "Authorization": "AWS4-HMAC-SHA256 Credential=%s/%s, "
                                            "SignedHeaders=host;x-amz-content-sha256;x-amz-date, "
                                            "Signature=%s" % (ak, scope, sig)})
              # Stage atomically: a half-written file must never look staged.
              tmp = dest + ".partial"
              n = 0
              with urllib.request.urlopen(req, timeout=300) as r, open(tmp, "wb") as f:
                  while True:
                      chunk = r.read(1 << 20)
                      if not chunk:
                          break
                      f.write(chunk); n += len(chunk)
              os.replace(tmp, dest)
              print("staged %s -> %s (%d bytes)" % (key, dest, n))
          env:
            - name: MINIO_ENDPOINT
              value: 192.168.28.158:32000
            - name: BUCKET
              value: "${BUCKET}"
            - name: OBJECT
              value: "${OBJ}"
            - name: MINIO_ACCESS_KEY
              valueFrom: {secretKeyRef: {name: minio-credentials, key: MINIO_ACCESS_KEY}}
            - name: MINIO_SECRET_KEY
              valueFrom: {secretKeyRef: {name: minio-credentials, key: MINIO_SECRET_KEY}}
          volumeMounts:
            - name: staging
              mountPath: /staging
YAML

kubectl --kubeconfig "$TGT_KUBECONFIG" wait --for=condition=complete "job/$JOB" -n "$TGT_NS" --timeout=180s
kubectl --kubeconfig "$TGT_KUBECONFIG" logs "job/$JOB" -n "$TGT_NS" | tail -3
echo "T_stage_complete=$(date -Is)"

# Drop any cached negative staging probe so the Readiness Controller re-measures.
kubectl --kubeconfig "$TGT_KUBECONFIG" delete pod -n "$TGT_NS" \
  -l ramp.dcn.ssu.ac.kr/component=stage-probe --ignore-not-found >/dev/null 2>&1 || true
