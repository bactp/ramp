# 06 — Application health, and what RTO actually measures

Two corrections to earlier work in this repository, both of which changed the
numbers.

---

## 1. "The pod is Running" proved nothing

The Scenario 1 workload originally declared a `video-session` Service on port
8080 with **nothing listening behind it**. The video member ticked a counter and
wrote to Redis, but served no traffic, so there was no way to ask the
application whether it was working — only Kubernetes whether the pod was up.

For a *distributed* application that gap matters: a restored container can be
Running and Ready while being the wrong instance, or frozen, or disagreeing with
its datastore. The video component now serves its own state over HTTP
(single-threaded `select()` loop, no threads or child processes, to keep the
CRIU dump simple), exposed on NodePort 30808 so the management plane can probe
it across clusters.

## 2. The health contract

`scripts/ramp-scenario1/70-verify-app-health.sh` — nine checks in three groups.
Recovery is not complete until all nine pass.

| group | check | what it rules out |
|---|---|---|
| **identity** | `VideoServing` | the service answers at all |
| | `InstanceRestored` | *a restart masquerading as a restore* |
| | `ResumedFromCheckpoint` | resumed from Redis instead of from memory |
| **liveness** | `DecodeInvariant` | corrupted in-memory state |
| | `StreamAdvancing` | restored but frozen |
| **consistency** | `RedisPrimaryRole` | standby never promoted |
| | `DistributedConsistency` | the two halves disagree |
| | `WriteDatapath` | video writes not reaching Redis |
| | `RedisLinkUp` | video isolated from its datastore |

`InstanceRestored` is the load-bearing one. The video member generates a random
`session` UUID **once, in memory, at process start** and never persists or
re-reads it. RAMP records it in the RecoveryPoint as
`status.artifacts[].instanceFingerprint` (an API field added for this). After a
recovery:

* same fingerprint → the process memory was genuinely restored
* new fingerprint → a restart that re-read replicated state

Without it, "the application came back" is unfalsifiable. Measured across every
run in `evidence/scenario1/timing-experiments/`, the restored instance carried
the same fingerprint as the checkpoint, and resumed from the checkpoint position
rather than the pre-failure one.

## 3. T_app_ready, not T_pod_ready

`scripts/ramp-scenario1/80-measure-recovery.sh` now reports both, and they are
not the same:

```
RTO to pod ready            6.2s
RTO to APPLICATION ready    7.8s
```

Pod readiness is a `tcpSocket` probe on 8080 — it goes true as soon as the
restored process binds its listener, which is before Redis has been reconfirmed
and before the distributed state has been shown to agree. Quoting the pod number
overstates recovery by ~1.6 s here, and would overstate it much further for an
application with a slower warm-up.

The measurement loop also pays the probe's own liveness dwell (1 s while
searching, 3 s in the final report), and that is stated in the breakdown rather
than hidden.

## 4. Timing: three variants

| variant | RTO to pod ready | **RTO to APPLICATION ready** |
|---|---|---|
| ArgoCD left to its own reconciliation interval | 287.6 s | **291.1 s** |
| explicit sync trigger, nothing prepared | 6.9 s | **8.5 s** |
| explicit sync trigger + fully prepared path | 6.2 s | **7.8 s** |

Optimized breakdown:

```
redis promotion                    0.5s
git commit (activate)              1.3s
argocd sync + apply                0.7s     <- not the bottleneck
pod ready (restore)                3.7s
pod ready -> APPLICATION ready     1.6s     (1.0s probe dwell)
------------------------------------------
                                   7.8s
```

### What actually optimises ArgoCD

**The explicit sync trigger — 291 s → 8.5 s.** Not a tuning knob: it is the
difference between waiting for ArgoCD's reconciliation interval and submitting a
sync operation. The Transition Operator already does exactly this in
`helpers.TriggerArgoCDSyncWithKubeClient` (it sets `.operation` on the
Application with `syncStrategy.apply.force`). `lib-gitea.sh:trigger_sync()`
reproduces it, and additionally pins the sync to the **commit SHA the git write
returned**, so the sync cannot apply a stale cached revision.

Pre-staging the path buys a further ~0.7 s, almost all of it from writing one
file at failure time instead of two. Its real value is elsewhere: it is what
puts the checkpoint image and the artifact on the target at all, and it is what
`RecoveryPath` readiness is *about*.

## 5. A measurement bug worth recording

Three consecutive runs reported a flat `argocd sync + apply = 61s`, and I twice
"optimised" against it — first by pre-creating the ArgoCD Application, then by
pinning the sync to a commit SHA. Neither moved the number, which should have
been the clue.

The 61 s was **the measurement harness**, not ArgoCD. After the DR-repo
redesign the wait loop was still polling a child Application name that no longer
existed; the jsonpath returned empty, the condition never became true, and the
loop ran to exhaustion — 120 iterations × ~0.5 s. Meanwhile the Application's
`status.operationState` recorded `Succeeded` at the correct commit SHA **one
second** after the push.

Both fixes are now in the harness: the loop aborts if the Application cannot be
read, and it waits on the observable effect (the Deployment carrying the
checkpoint image at 1 replica) rather than on a status string. The wrong run is
kept at `evidence/scenario1/timing-experiments/00-WRONG-measurement-61s-artifact.log`.

The general lesson: a suspiciously round, highly repeatable constant is more
likely to be your own timeout than the system's behaviour.

## 6. A second self-inflicted fault, also instructive

The first DR design put the recovery manifests in the **source** cluster's
package repo (`nephio/workload01.git`). The source cluster's own ArgoCD
Application syncs that repo with `path: .` and `recurse: true`, so it applied
the target-shaped manifest — including a `nodeSelector` pinning to a workload02
node — back onto **workload01**. The live source workload was replaced by a pod
stuck in `Pending` with `node(s) didn't match Pod's node affinity/selector`.

Recovery manifests belong in the DR repo, which only the target's `<target>-dr`
Application syncs. That is presumably why the repo exists. The rule is now
enforced by construction in `lib-recovery-manifest.sh`, with the reasoning in
its header.
