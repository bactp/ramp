# End-to-end GitOps restore: VERIFIED

The full actuation chain of the existing Transition Operator, reproduced for the
Scenario 1 workload and verified to genuinely restore process memory.

```
kubelet Container Checkpoint  ->  tar on node
   -> checkpoint-agent        ->  MinIO bucket `checkpoints`
   -> OCI checkpoint image    ->  FROM scratch + tar + org.criu.checkpoint.*
   -> nephio/workload01.git   ->  path video-session/, image rewritten to the
                                  checkpoint image
   -> nephio/dr.git           ->  path workload02-dr/, an ArgoCD Application
   -> ArgoCD workload02-dr    ->  app-of-apps, automated sync
   -> ArgoCD argocd-workload01-video-session -> syncs the package
   -> containerd              ->  reads org.criu.checkpoint.* and RESTORES
```

## Timing

```
T_gitops_start   04:32:38
T_git_pushed     04:32:41    3 s   (3 files via the Gitea API)
T_argocd_synced  04:32:46    5 s   (app-of-apps created + synced the child app)
T_workload_ready 04:32:47    1 s   (deployment rolled out)
                             ---
                             9 s   total
```

## Proof it is a restore, not a restart

The video member generates a random `session` UUID once in memory at process
start and never reads it back from Redis.

| | session uuid | position |
|---|---|---|
| source process, workload01, never restarted | `319f64cb-fca9-42cf-8f56-ef074dc42104` | 59388 |
| epoch-6 checkpoint captured at | same process | **165** |
| **pod created by ArgoCD on workload02** | **`319f64cb-fca9-42cf-8f56-ef074dc42104`** | resumed **165 → 200** |

and containerd's own runtime annotations on that container:

```
checkpointImage = docker.io/ramp/checkpoint-video-session:epoch-6
checkpointedAt  = 2026-09-21T10:06:58Z
restored        = true
imageRef        = docker.io/library/python@sha256:2f17fc04...   (the ROOTFS image)
```

The Deployment carries `app.kubernetes.io/instance: argocd-workload01-video-session`,
i.e. it was created by ArgoCD from git, not by kubectl.

`redis_linked: true`: the restored process reconnected to Redis on the target.

## The one link not exercised here

The checkpoint image was imported directly into the target node's containerd
rather than pushed to a registry, so the manifest uses `imagePullPolicy: Never`
and a `nodeSelector` pin. The registry-pull link is separately proven by the
pre-existing `default/video` app, which restores from
`docker.io/phuongbac/checkpoint-default_video-...` on both clusters.

In production both the pin and the pull policy disappear; the only extra step is
`buildah push` (or the equivalent), which the Transition Operator already does.
