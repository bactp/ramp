# Contract-aware readiness

`RecoveryGroup.spec.recoveryContract` used to be carried and never consulted.
`EstimatedRTO` was published and never compared to anything. HOT was documented
as *"every mandatory prerequisite for satisfying the RecoveryContract already
holds"* and implemented as *"every check I happened to write returned true"*.

This document is what those words mean now.

## 1. HOT

```
HOT =  a PreparedRecoveryPoint exists
   AND it is committed and validated
   AND it is fresh enough for the RPO
   AND the target cluster is reachable
   AND the target namespace is Active
   AND ONE concrete target node satisfies every node-level prerequisite
   AND restore capability is ready ON THAT NODE
   AND the target Redis runtime is up and warm
   AND the epoch's immutable Redis RDB is in the artifact store
   AND the epoch's container checkpoint is in the artifact store
   AND the checkpoint tar AND its CRI checkpoint image are ON THAT NODE
   AND the activation plan on the target names THIS RecoveryPoint
   AND the estimated activation latency fits the RTO
```

Every conjunct is a step the actual recovery procedure performs. The last four
are new, and three of them are why a path could previously be HOT while a real
recovery would have had to build an image and write manifests first.

## 2. Readiness mapping

| level | meaning | typical cause |
| --- | --- | --- |
| **HOT** | an executable prepared path exists and currently satisfies RPO and RTO | — |
| **WARM** | recovery is feasible, but the contract is not currently guaranteed, or preparation is incomplete | prepared point stale while its successor is still being prepared; checkpoint image missing but rebuildable; estimated latency over the RTO |
| **COLD** | no currently executable recovery path | target unreachable, no feasible node, no committed point, artifacts gone from the store |

The discriminator between WARM and COLD is deliberately *"is there something to
fall back on"*, not *"how many checks failed"*:

```go
targetUsable        = TargetClusterReachable && TargetNamespaceReady && TargetPlacementFeasible
haveExecutablePoint = RecoveryPointCommitted && RedisEpochArtifactAvailable && VideoCheckpointAvailable

len(unmet) == 0                          -> HOT
!targetUsable || !haveExecutablePoint    -> COLD
otherwise                                -> WARM
```

A stale-but-restorable point is WARM, never COLD: the artifacts are all there
and a recovery would still work — it would just lose more history than the
contract permits. That distinction is the whole reason freshness and preparation
are separate dimensions.

The WARM `reason` says which one to fix:

| reason | fix |
| --- | --- |
| `PreparedRecoveryPointStale` | run a new epoch (or finish preparing the candidate) |
| `EstimatedRTOExceedsContract` | prepare more of the target, or relax the RTO |
| `PreparationOutstanding` | run `36-prepare-target.sh` |

## 3. RPO — freshness

```
RecoveryPointAge   = now - preparedRecoveryPoint.commitTime
FreshEnough        = RecoveryPointAge <= RecoveryContract.RPO
FreshnessRemaining = max(0, RPO - age)
```

Measured in **time**, not logical position. An RPO is "how much application
history may the recovery lose"; the logical position says *where* the point is,
not how stale it is. An undeclared RPO (`0`) cannot be violated.

Published as:

```yaml
status:
  contract:
    rpo: 30m0s
    recoveryPointAge: 4s
    freshnessRemaining: 29m56s
    freshEnough: true
```

and as the check `RecoveryPointFreshEnough`, whose failure message is explicit
that nothing is broken:

```
video-stream-rg-epoch-41 age 7h7m51s > RPO 30m0s: the prepared point is still
fully restorable, but recovering from it would lose more than the contract allows
```

### Freshness and preparation do not collapse

```
prepared = true,  age 8s, RPO 5s   ->  NOT HOT   (stale)
prepared = false, age 1s, RPO 60s  ->  NOT HOT   (unprepared)
```

Two different problems, two different fixes, two different checks.

### Why the shipped RPO is 30m

Epochs are created by hand today, so the age of the prepared point is bounded by
how often someone runs one. A tight RPO would make the path permanently WARM and
would say nothing about the system. Driving epoch creation *from* this field is
the next task; until it exists, 30m is the honest setting. The
`98-prepared-recovery-point-tests.sh` Test D narrows the RPO on purpose to
observe the True → False transition at a known deadline.

## 4. RTO — activation feasibility

`EstimatedActivationLatency` is a **sum over the failure-time steps that are
still required**, not a prediction and not a learned model:

```
EstimatedActivationLatency = Σ cost(step) for every step still Required
EstimatedRTOWithinContract = EstimatedActivationLatency <= RecoveryContract.RTO
```

The costs are measured lab constants from the Q>P run
(`evidence/recovery-epoch-qgtp-20260922T084434/timing.json`):

| step | cost | required when |
| --- | --- | --- |
| `ProvisionRedisRuntime` | 60 s | the target Redis runtime is not up |
| `StageCheckpointArtifact` | 30 s | the tar is not on the placement node (checkpoint-agent `PULL_INTERVAL`) |
| `BuildCheckpointImage` | 45 s | the CRI checkpoint image is not on the placement node |
| `PrepareActivationPlan` | 25 s | the target workload object / ArgoCD wiring is not in place |
| `RestoreRedisFromEpochArtifact` | 3 s | **always** — measured `T_recovery_start → T_redis_ready` |
| `CommitAndSyncActivation` | 2 s | **always** |
| `RestoreContainerFromCheckpoint` | 6 s | **always** — measured `T_video_restore_start → T_video_pod_ready` |
| `VerifyAndRelease` | 1 s | **always** |

Fully prepared, that is **12 s**, against an observed end-to-end activation of
11 s in the Q>P experiment.

Preparation removes steps from the sum; it never makes the remaining ones
cheaper. The breakdown is published every evaluation, so the number can be
re-derived instead of trusted:

```yaml
status:
  contract:
    rto: 1m0s
    estimatedActivationLatency: 12s
    rtoWithinContract: true
    activationSteps:
      - {name: ProvisionRedisRuntime,          cost: 1m0s, required: false, reason: "target Redis runtime already up and warm"}
      - {name: StageCheckpointArtifact,        cost: 30s,  required: false, reason: "checkpoint tar present on the placement node"}
      - {name: BuildCheckpointImage,           cost: 45s,  required: false, reason: "CRI checkpoint image present on the placement node"}
      - {name: PrepareActivationPlan,          cost: 25s,  required: false, reason: "target workload object and ArgoCD wiring already in place"}
      - {name: RestoreRedisFromEpochArtifact,  cost: 3s,   required: true}
      - {name: CommitAndSyncActivation,        cost: 2s,   required: true}
      - {name: RestoreContainerFromCheckpoint, cost: 6s,   required: true}
      - {name: VerifyAndRelease,               cost: 1s,   required: true}
```

**This is still not an end-to-end RTO.** There is no failure detection, so
nothing here includes the time between the failure and the decision to recover.
It is the *activation* budget, and it is compared against `recoveryContract.rto`
as the best available proxy until detection exists.

## 5. Target placement

The old `TargetResourceReady` computed `max(CPU over nodes)` and
`max(memory over nodes)` and compared them independently — it could pass with
the CPU from one machine and the memory from another, describing a node that
does not exist.

`TargetPlacementFeasible` names **one** node that satisfies everything at once:

```
for each candidate node (control-plane excluded unless spec.targetPrereqs.node pins it):
    Ready and not cordoned
    a ready restore-capability agent pod on THIS node
    free CPU    = allocatable - Σ requests of non-terminated pods on this node
    free memory = allocatable - Σ requests of non-terminated pods on this node
    free >= minAllocatable{Cpu,Memory}
choose: the currently prepared node if it is still feasible, else the first feasible in name order
```

Free capacity is **allocatable minus requests**, because allocatable is capacity
after system reservations, not unused capacity; reporting it as free is how a
path claims room it does not have. This is not a scheduler — no affinity, no
taints beyond cordon, no preemption — and it does not need to be: it only has to
be honest about the one node it names.

Everything node-bound is then verified **on that node**: the staging job is
pinned to it (`nodeName`), the restore-artifact probe is pinned to it, and the
activation plan's `nodeSelector` must match it. Evidence from any other node is
not evidence for this path.

## 6. Restore readiness: tar ≠ image

The recovery does not restore a tar. It restores a **CRI checkpoint image**:

```
container checkpoint tar   (kubelet -> checkpoint-agent -> MinIO -> staged on node)
        ↓  35-build-checkpoint-image.sh, on the node
CRI checkpoint image       (org.criu.checkpoint.* annotations, in containerd)
        ↓  61/62 GitOps activation
containerd / CRIU restore
```

The old checks stopped at the tar, so a path was HOT while the image the restore
consumes did not exist anywhere — and the Q>P experiment had to run
`35-build-checkpoint-image.sh` and `61-prepare-gitops-path.sh` before it could
recover at all. `RestoreArtifactReady` now requires both halves, on the
placement node, and `ActivationPlanPrepared` requires the GitOps side.

Neither fact is visible from the management plane: the tar lives in a hostPath,
and containerd's image store is not in the Node object (kubelet reports only the
50 largest images; a 10 MB checkpoint image is never among them — verified on
this testbed). So a node-pinned privileged pod measures both, and its exit code
says which half is missing:

```
0   both present
10  tar missing on this node
11  tar present, CRI checkpoint image missing on this node
```

## 7. What the Redis replica contributes now

It is **not** the recovery artifact and it is **not** promoted. That was fixed in
the RecoveryEpoch work: a RecoveryPoint carries an immutable epoch RDB, because
a replica keeps moving after commit and cannot identify the committed epoch.

What the replica buys for readiness is that the target Redis **runtime** — the
process, its Service, its data volume — is already up and holding a warm
dataset, so activation is *load the epoch RDB and restart* rather than *deploy
Redis, wait for it, then load*. That is worth 60 s of `ProvisionRedisRuntime`,
and the check is named `RedisTargetRuntimeReady` to stop the old name implying a
promotion that no longer happens.

## 8. Readiness has to be stable, not just correct

The restore-artifact probe re-measures on a timer, because an artifact that was
confirmed can be deleted. The first implementation did that by deleting the
finished probe pod and creating a replacement — which left a window on every
refresh cycle where no terminal result existed. `RestoreArtifactReady` went
False during that window and the path dropped out of HOT roughly once a minute,
purely because it was re-measuring.

That is the same class of bug as evaluating the latest RecoveryPoint instead of
the prepared one: **readiness lost for a reason that is not a loss.** It was
caught by Test B recording `readiness=WARM` in its observation string while its
assertion, a fraction of a second later, saw HOT.

The fix is generational probes: each artifact identity gets one pod per
45-second bucket, the verdict is the newest *terminal* pod for that identity,
and the previous generation is kept until the new one has finished. Coverage is
continuous and a verdict is at most ~2×TTL old, which is the detection latency
for a removed artifact.

`99-readiness-stability.sh` samples the path every 5 s for 3 minutes — several
probe generations — and fails if a prepared, fresh, fully-staged path leaves HOT
even once.

## 9. Publishing: the status is written on change, not on every pass

A readiness controller re-evaluates on a timer and the obvious thing to do with
the result is write it. That produces a hot loop, because a readiness status
contains several fields that advance on their own: `lastValidatedTime`, every
check's `lastProbeTime`, the placement's `selectedAt`, the prepared point's age,
and — in the free-text messages — the target Redis replication offset. Every
pass therefore differs from the last, every pass writes, every write fires the
object's own watch, and the watch drives the next pass.

Measured on this testbed before the fix, with a 10 s resync configured:

| | before | after |
| --- | --- | --- |
| status writes | ~1150 per 2 min (~10/s) | 4 per 2 min (the heartbeat floor) |
| reconciles | 644 per 10 min | ~12 per 2 min (the resync) |
| conflict errors | ~21 per 2 min | 0 |

Each of those reconciles also listed every node and every pod on the target
cluster and stat-ed two MinIO objects, so the cost was not confined to etcd. The
published *values* were correct throughout, which is why it survived a full test
suite unnoticed — the tests asserted what the status said, never how often it
said it.

The publish step now compares a **semantic projection** of the status against
what is already published, and writes only on a real change or when the 30 s
heartbeat is due. The projection covers readiness, unmet checks, the latest /
candidate / prepared references, prepared artifacts and their nodes, placement,
the contract verdicts and the activation-step breakdown, and each check's
`(name, status, reason)`.

Two exclusions are deliberate:

* **check messages** — reasons are the stable, enumerable verdict
  (`RecoveryPointStale` vs `WithinRPO`); messages carry live detail that moves
  on its own.
* **`recoveryPointAge` and `freshnessRemaining`** — they tick every second, and
  they describe the verdict rather than being it. `freshEnough` *is* in the
  projection, so the True → False transition is still published on the reconcile
  that observes it.

Consequences, accepted deliberately: the displayed age can lag by up to the
heartbeat, and a change visible only in a message waits for the heartbeat. No
verdict is ever delayed — the worst case for any decision is one resync
interval.

A status write that conflicts is requeued quietly instead of being logged as an
error: it means this reconcile read a cached object a later write had already
superseded, which is expected and self-healing.
