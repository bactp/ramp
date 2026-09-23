# PreparedRecoveryPoint

## 1. The defect

`RecoveryPathReconciler` evaluated `RecoveryGroup.status.latestRecoveryPoint`
(or `spec.recoveryPointRef`). The consequence:

```
RP1 committed, prepared on the target        -> HOT
RP2 committed                                -> latest = RP2
                                                the path immediately evaluates RP2
                                                RP2's artifacts are not on the target
                                             -> WARM
```

RP1 was still fully executable the whole time. Nothing was lost; nothing broke;
the target did not change. The path stopped claiming readiness **because newer
state existed** — so committing an epoch, which is supposed to improve the
recovery position, degraded the readiness of the path that was already able to
recover. Preparing RP2 has to be free of charge against RP1's readiness, or
epoch cadence and readiness fight each other.

The root confusion is that one field was being asked to mean three things.

## 2. Three distinct RecoveryPoints

```
LatestRecoveryPoint     the newest committed + validated point for the group.
                        INFORMATION. Readiness is never evaluated against it.

CandidateRecoveryPoint  a newer point this path is working towards. It has no
                        effect on readiness at all until it is promoted.

PreparedRecoveryPoint   the point this path can activate RIGHT NOW: its
                        artifacts are in the store, its checkpoint image is on
                        the placement node, and the activation plan on the
                        target names it. Readiness is evaluated against THIS.
```

All three appear in `status`, so "why is this path HOT on an old epoch?" is
answerable from `kubectl get recoverypath -o yaml` alone.

## 3. State diagram

```
                                  new RP2 committed
                                         │
                                         ▼
   ┌──────────────────────┐      ┌────────────────────┐
   │ RP1 PREPARED  → HOT  │─────▶│ RP1 PREPARED → HOT │
   │ candidate = none     │      │ candidate = RP2    │
   └──────────────────────┘      └─────────┬──────────┘
              ▲                            │  36-prepare-target.sh RP=RP2
              │                            │   1. stage tar on placement node
              │                            │   2. build CRI checkpoint image
              │                            │   3. write + converge activation plan
              │                            ▼
              │                  ┌────────────────────────┐
              │                  │ all RP2 preparation    │
              │                  │ checks hold?           │
              │                  └───────┬────────────────┘
              │                    no    │    yes
              │              ┌───────────┘    └──────────────┐
              │              ▼                               ▼
              │     keep prepared = RP1            ATOMIC PROMOTION
              │     (readiness unaffected)         prepared  = RP2
              │                                    candidate = cleared
              └────────────────────────────────────────────┘
```

The prepared point is never cleared to make room for a candidate. It is
*replaced*, in one status write, and only once the candidate has been shown to
be executable in its own right.

And the stale case, which is a different axis entirely:

```
   RP1 prepared
        │
        ├── age <= RecoveryContract.RPO ──────────────▶ HOT
        │
        └── age >  RecoveryContract.RPO
                 and RP2 not prepared yet ────────────▶ WARM
                       (RP1 is still perfectly restorable;
                        it just loses more than the contract allows)
```

## 4. Promotion

`preparationComplete()` is the gate. A candidate is promoted only when **all**
of these hold, evaluated against the candidate itself:

| fact | how it is established |
| --- | --- |
| `RedisEpochArtifactAvailable` | the epoch's immutable RDB is `Stat`-able in MinIO |
| `VideoCheckpointAvailable` | the epoch's checkpoint tar is `Stat`-able in MinIO |
| `RestoreArtifactReady` | a node-pinned probe on the placement node finds both the tar in the kubelet checkpoint directory **and** the CRI checkpoint image in that node's containerd |
| `ActivationPlanPrepared` | the target workload object exists, at `replicas: 0`, pinned to the placement node, annotated `prepared-recovery-point: <this RP>` |

Promotion is a single status write: `prepared = candidate; candidate = nil`.
There is no window in which the path has no prepared point because a newer one
is arriving.

## 5. Preparation is attributed, not inferred

Every preparation step is told which RecoveryPoint it prepares, and stamps that
claim where the controller can check it:

```yaml
metadata:
  annotations:
    ramp.dcn.ssu.ac.kr/prepared-recovery-point: video-stream-rg-epoch-41
    ramp.dcn.ssu.ac.kr/prepared-epoch: "41"
    ramp.dcn.ssu.ac.kr/checkpoint-image: docker.io/ramp/checkpoint-video-session:video-stream-rg-epoch-41
    ramp.dcn.ssu.ac.kr/target-node: workload02-md-0-rx5mn-pjkrh
    ramp.dcn.ssu.ac.kr/state: prepared
```

`activationPlan()` rejects the plan when the annotation names a different
RecoveryPoint, and the checkpoint image it probes for on the node is the one the
plan declares — not a tag the controller guesses. This closes the hole that made
the earlier Q>P experiment need `RP_NAME=<RP>` passed by hand: a run that staged
the previous epoch's artifact still looked prepared.

So this is now impossible:

```
preparedRecoveryPoint = RP-41
but the target still carries the checkpoint image from RP-40
```

The status records the evidence:

```yaml
status:
  preparedRecoveryPoint:
    name: video-stream-rg-epoch-41
    epoch: 41
    logicalPosition: 26011
    commitTime: "2026-09-22T16:21:03Z"
    preparedAt: "2026-09-22T16:22:18Z"
    executable: true
    placement: {cluster: workload02, node: workload02-md-0-rx5mn-pjkrh}
    artifacts:
      - {kind: redisSnapshot,         ref: "minio://checkpoints/ramp-redis-epoch/…-epoch-41.rdb"}
      - {kind: videoCheckpoint,       ref: "minio://checkpoints/checkpoint-…tar"}
      - {kind: activationPlan,        ref: "docker.io/ramp/checkpoint-video-session:…-epoch-41", node: …}
      - {kind: videoCheckpointOnNode, ref: "checkpoint-…tar",  node: workload02-md-0-rx5mn-pjkrh}
      - {kind: checkpointImage,       ref: "docker.io/ramp/…", node: workload02-md-0-rx5mn-pjkrh}
```

## 6. The prepared point is re-verified, never trusted

`resolvePrepared()` reloads the promoted point and `evaluatePoint()` re-runs
every check on it each reconcile. A path that kept claiming HOT from a status
field it wrote earlier would be exactly the false readiness this work exists to
remove — so deleting the checkpoint image from the node degrades the path
(Test E) even though it was promoted minutes earlier.

The node probe is deliberately **not latched**: the previous stage probe cached
"the artifact is present" forever, on the theory that an artifact never goes
away. It can, and the check has to notice. The probe result is re-measured once
it is older than `preparationProbeTTL` (45 s), which bounds detection latency.

## 7. Reproducing

```bash
scripts/ramp-scenario1/36-prepare-target.sh              # RP=<point> prepares one point
scripts/ramp-scenario1/98-prepared-recovery-point-tests.sh   # Tests A-G
```
