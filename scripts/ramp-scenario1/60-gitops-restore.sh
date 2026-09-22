#!/usr/bin/env bash
# End-to-end GitOps restore, following the Transition Operator's own convention.
#
# This is the actuation half of a future RAMP RecoveryDriver. Nothing here is
# invented: it reproduces exactly what the Transition Operator's stateful
# transition path does (helpers.CreateAndPushLiveStateBackupRestore +
# TriggerArgoCDSyncWithKubeClient), for the Scenario 1 workload.
#
#   package repo   nephio/<source-cluster>.git   path <app>/
#        the workload manifests, with the container image rewritten from the
#        original image to the checkpoint image
#   DR repo        nephio/dr.git                 path <target>-dr/
#        an ArgoCD Application pointing at that package
#   ArgoCD         <target>-dr on the TARGET cluster (automated sync)
#        app-of-apps: picks up the new Application, which syncs the package
#   containerd     sees org.criu.checkpoint.* on the image and RESTORES
#
# Registry note: this run pre-imports the checkpoint image into the target
# node's containerd instead of pushing it to a registry, so the manifest uses
# imagePullPolicy: Never and pins the node. The registry-pull link is already
# proven by the pre-existing default/video app, which restores from
# docker.io/phuongbac/checkpoint-...  In production the image goes to a registry
# and both the pin and the pull policy disappear.
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

api() { # api <METHOD> <path> [body-file]
  local m="$1" p="$2" b="${3:-}"
  if [ -n "$b" ]; then
    curl -sS -u "$GU:$GP" -X "$m" -H 'Content-Type: application/json' --data-binary @"$b" "$GITEA/api/v1$p"
  else
    curl -sS -u "$GU:$GP" -X "$m" "$GITEA/api/v1$p"
  fi
}

put_file() { # put_file <repo> <path> <local-file> <message>
  local repo="$1" path="$2" src="$3" msg="$4"
  local sha body
  # An absent file returns an error object with no "sha", which is exactly the
  # signal we want: POST to create, PUT (with sha) to update.
  sha=$(api GET "/repos/$OWNER/$repo/contents/$path" \
        | python3 -c 'import sys,json
try:
    d = json.load(sys.stdin)
    print(d.get("sha", "") if isinstance(d, dict) else "")
except Exception:
    print("")' 2>/dev/null || true)
  body=$(python3 - "$src" "$msg" "$sha" <<'PY'
import base64, json, sys
content = base64.b64encode(open(sys.argv[1], "rb").read()).decode()
out = {"content": content, "message": sys.argv[2]}
if sys.argv[3]:
    out["sha"] = sys.argv[3]
print(json.dumps(out))
PY
)
  printf '%s' "$body" > /tmp/ramp-gitea-body.json
  if [ -n "$sha" ]; then
    api PUT "/repos/$OWNER/$repo/contents/$path" /tmp/ramp-gitea-body.json >/dev/null
    echo "  updated $repo:$path"
  else
    api POST "/repos/$OWNER/$repo/contents/$path" /tmp/ramp-gitea-body.json >/dev/null
    echo "  created $repo:$path"
  fi
}

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
echo "T_gitops_start=$(date -Is)"

# Naive flow: nothing was prepared, so the whole recovery manifest set is
# written and converged at failure time. Kept for comparison against the
# prepare/activate split in 61/62.
render_recovery_manifests "$WORK" 1 "$CKPT_IMAGE" Never activated
put_file dr "${DR_PATH}/deployment.yaml" "$WORK/deployment.yaml" "RAMP restore: ${APP} from ${CKPT_IMAGE}"
put_file dr "${DR_PATH}/svc.yaml"        "$WORK/svc.yaml"        "RAMP restore: ${APP} service"
echo "T_git_pushed=$(date -Is)"

trigger_sync "$TGT_KUBECONFIG" "${TGT_CLUSTER}-dr"
for _ in $(seq 1 60); do
  kubectl --kubeconfig "$TGT_KUBECONFIG" get deploy "$APP" -n "$NS" >/dev/null 2>&1 && break
  sleep 2
done
echo "T_argocd_applied=$(date -Is)"
