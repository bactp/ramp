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
# the only safe home for target-shaped manifests. This is presumably why the
# repo exists.
render_recovery_manifests() { # render_recovery_manifests <dir> <replicas> <image> <pullPolicy> <state>
  local dir="$1" replicas="$2" image="$3" pull="$4" state="$5"
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
