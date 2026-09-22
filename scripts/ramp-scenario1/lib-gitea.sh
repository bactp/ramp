# Shared Gitea helpers. Sourced by the GitOps prepare/activate scripts.
api() { # api <METHOD> <path> [body-file]
  local m="$1" p="$2" b="${3:-}"
  if [ -n "$b" ]; then
    curl -sS -u "$GU:$GP" -X "$m" -H 'Content-Type: application/json' --data-binary @"$b" "$GITEA/api/v1$p"
  else
    curl -sS -u "$GU:$GP" -X "$m" "$GITEA/api/v1$p"
  fi
}

file_sha() { # file_sha <repo> <path>  -- empty when the file does not exist
  api GET "/repos/$OWNER/$1/contents/$2" | python3 -c 'import sys,json
try:
    d = json.load(sys.stdin)
    print(d.get("sha", "") if isinstance(d, dict) else "")
except Exception:
    print("")' 2>/dev/null || true
}

put_file() { # put_file <repo> <path> <local-file> <message>
  local repo="$1" path="$2" src="$3" msg="$4" sha
  sha=$(file_sha "$repo" "$path")
  python3 - "$src" "$msg" "$sha" > /tmp/ramp-gitea-body.json <<'PY'
import base64, json, sys
out = {"content": base64.b64encode(open(sys.argv[1], "rb").read()).decode(),
       "message": sys.argv[2]}
if sys.argv[3]:
    out["sha"] = sys.argv[3]
print(json.dumps(out))
PY
  local verb resp
  if [ -n "$sha" ]; then verb=PUT; else verb=POST; fi
  resp=$(api "$verb" "/repos/$OWNER/$repo/contents/$path" /tmp/ramp-gitea-body.json)
  # Remember the commit this write produced. Syncing ArgoCD at an explicit SHA
  # is what makes the sync immediate; see trigger_sync.
  LAST_COMMIT_SHA=$(printf '%s' "$resp" | python3 -c 'import sys,json
try:
    print(json.load(sys.stdin).get("commit", {}).get("sha", ""))
except Exception:
    print("")' 2>/dev/null)
  export LAST_COMMIT_SHA
  echo "  $([ "$verb" = PUT ] && echo updated || echo created) $repo:$path @ ${LAST_COMMIT_SHA:0:12}"
}

# Force ArgoCD to reconcile NOW, at a specific revision.
#
# Measured on this installation: annotating refresh=hard and submitting a sync
# operation with revision "HEAD" both complete in ~1s -- and both sync the
# revision the repo-server still has CACHED. The new commit is only picked up at
# the next repository poll, which cost a flat ~61s in every run and dominated
# the entire RTO.
#
# Passing the exact commit SHA that the git write returned removes the cache
# from the path: ArgoCD has to fetch that revision, so the sync applies the new
# manifests on the first attempt.
trigger_sync() { # trigger_sync <kubeconfig> <application> [revision]
  local kc="$1" app="$2" rev="${3:-${LAST_COMMIT_SHA:-HEAD}}"
  # RAMP_NO_TRIGGER=1 leaves ArgoCD to its own polling. Used to measure what the
  # explicit trigger is actually worth.
  if [ "${RAMP_NO_TRIGGER:-0}" = "1" ]; then return 0; fi
  kubectl --kubeconfig "$kc" -n argocd annotate application "$app" \
    argocd.argoproj.io/refresh=hard --overwrite >/dev/null 2>&1 || true
  kubectl --kubeconfig "$kc" -n argocd patch application "$app" --type=merge -p \
    "{\"operation\":{\"sync\":{\"revision\":\"${rev}\",\"syncStrategy\":{\"apply\":{\"force\":true}}},\"initiatedBy\":{\"username\":\"ramp\"}}}" \
    >/dev/null 2>&1 || true
}

refresh_app() { trigger_sync "$@"; }
