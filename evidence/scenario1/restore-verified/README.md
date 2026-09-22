# Container-checkpoint restore: VERIFIED WORKING

Supersedes the earlier (wrong) conclusion in 04-results.md that a kubelet
checkpoint cannot be restored on containerd.

## Discriminator

The video member generates a random `session` UUID **once, in memory, at process
start**, and never reads it back from Redis. So:

* a genuine CRIU restore keeps the SAME uuid
* a cold restart produces a NEW uuid

| | session uuid | position |
|---|---|---|
| source process on workload01 (never restarted) | `319f64cb-fca9-42cf-8f56-ef074dc42104` | 59388 (still running) |
| epoch-6 checkpoint captured at | same process | **165** |
| restored on workload02 from the checkpoint image | **`319f64cb-fca9-42cf-8f56-ef074dc42104`** | resumed 165 -> 215 |
| (earlier cold-restart fallback, for contrast) | `2a004627-1053-4a53-ad42-71504ff5af27` | jumped to Redis position |

Identical uuid + resumption from 165 rather than 59388 = the process memory was
restored, not restarted.

## Runtime's own claim

```
checkpointImage = docker.io/ramp/checkpoint-video-session:epoch-6
checkpointedAt  = 2026-09-21T10:06:58Z
restored        = true
imageRef        = docker.io/library/python@sha256:2f17fc04...   (the ROOTFS image)
```

These annotations appear ONLY in the CRI runtime config -- they are absent from
the Pod and Deployment manifests -- so containerd generated them itself.

`redis_linked: true` in the restored state: the restored process also
reconnected to the promoted Redis on the target.
