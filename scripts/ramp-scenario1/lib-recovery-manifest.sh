# Render the recovery manifests for the container-checkpoint driver.
#
# WHERE THESE GO, AND WHY IT MATTERS
# ----------------------------------
# They go in the DR repo (nephio/dr.git, path <target>-dr/), never in the
# source cluster's own package repo.
#
# Learned the hard way: the source cluster's ArgoCD Application syncs
# nephio/<source>.git with path "." and recurse: true. A recovery manifest
# placed there is therefore applied back onto the SOURCE cluster, where it
# overwrites the live workload with a copy pinned to a node in the target
# cluster -- the pod goes Pending with "node(s) didn't match Pod's node
# affinity/selector" and the application is down for a reason that has nothing
# to do with the recovery.
#
# The DR repo is synced only by the target's <target>-dr Application, so it is
# the only safe home for target-shaped manifests.
#
# WHY THE ANNOTATIONS ARE NOT DECORATION
# --------------------------------------
# The manifest carries the RecoveryPoint it was prepared for, that point's
# epoch, the checkpoint image built from that point's artifact, and the node the
# artifacts were staged on. RAMP's readiness evaluation reads exactly these four
# and refuses to call the plan prepared for any other RecoveryPoint.
#
# Without them, "the target is prepared" meant "some script ran at some time
# against whatever was latest then" -- which is how a Q>P experiment once staged
# epoch N-1's checkpoint and failed to restore. Preparation now has to say what
# it prepared, and the controller checks the claim.
render_recovery_manifests() { # <dir> <replicas> <image> <pullPolicy> <state> <recoveryPoint> <epoch> <checkpointImage>
  local dir="$1" replicas="$2" image="$3" pull="$4" state="$5"
  local rp="${6:-}" epoch="${7:-}" ckpt_image="${8:-}"
  mkdir -p "$dir"
  cat > "$dir/deployment.yaml" <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${APP}
  namespace: ${NS}
  annotations:
    ramp.dcn.ssu.ac.kr/recovery-driver: container-checkpoint
    ramp.dcn.ssu.ac.kr/state: ${state}
    ramp.dcn.ssu.ac.kr/prepared-recovery-point: "${rp}"
    ramp.dcn.ssu.ac.kr/prepared-epoch: "${epoch}"
    ramp.dcn.ssu.ac.kr/checkpoint-image: "${ckpt_image}"
    ramp.dcn.ssu.ac.kr/target-node: "${TARGET_NODE}"
  labels:
    app: ${APP}
spec:
  replicas: ${replicas}
  selector:
    matchLabels:
      app: ${APP}
  template:
    metadata:
      labels:
        app: ${APP}
        ramp.dcn.ssu.ac.kr/role: video
    spec:
      nodeSelector:
        kubernetes.io/hostname: ${TARGET_NODE}
      containers:
        - name: video
          image: ${image}
          imagePullPolicy: ${pull}
          ports:
            - {containerPort: 8080, name: http}
          env:
            - {name: REDIS_HOST, value: "redis.${NS}.svc.cluster.local"}
            - {name: REDIS_PORT, value: "6379"}
            - {name: FRAMES_PER_TICK, value: "30"}
YAML
  cat > "$dir/svc.yaml" <<YAML
apiVersion: v1
kind: Service
metadata:
  name: ${APP}
  namespace: ${NS}
spec:
  type: NodePort
  selector:
    app: ${APP}
  ports:
    - {port: 8080, targetPort: 8080, nodePort: 30808, name: http}
YAML
}
