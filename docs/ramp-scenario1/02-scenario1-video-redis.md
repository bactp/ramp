# 02 — Scenario 1: Video Streaming Service + Redis

## 1. Why this application

The scenario has to prove something stronger than "checkpoint N pods, restore N
pods". It has to show that a single recovery-consistency domain can contain
members whose recovery states are produced by **different mechanisms** and whose
joint usability is a checkable property.

The application is therefore built so that the two kinds of state are
physically separable and individually insufficient:

| State | Lives in | Recoverable by | NOT recoverable by |
|---|---|---|---|
| `position` — committed stream position | Redis | replication | container checkpoint alone (it would be stale by up to one epoch) |
| `frames_decoded`, `session` uuid, frame ring | the video process's memory | container checkpoint | replication (Redis never sees it) |

They are bound by an application invariant the code enforces every tick:

```
frames_decoded == position * FRAMES_PER_TICK
```

That invariant is what makes "are these two artifacts mutually consistent?" a
question with an answer, rather than an assertion.

## 2. Topology

```
workload01 (SOURCE)                        workload02 (TARGET)
  ns ramp-demo                               ns ramp-demo
    AppBundle video-stream-service             redis-standby  (replica)
      group data      -> redis-primary         Service redis  NodePort 30380
      group streaming -> video-session         checkpoint-agent (target role, RW)
    Service redis     NodePort 30379           runc 1.3.1 + criu 4.2.1
    checkpoint-agent (source role, RO)
                      |
   replication 192.168.28.238:30379 ---------------> redis-standby
                      |
        kubelet checkpoint -> /var/lib/kubelet/checkpoints
                      |
                  MinIO bucket `checkpoints` (mgmt :32000)
```

`192.168.28.238` is workload01's control-plane **floating IP**; NodePorts are
reachable there from workload02, while the `10.6.0.0/24` node network is not
(audit §1.1). This is why the endpoints in the CRs are IP:NodePort rather than
in-cluster Service DNS.

The pre-existing `default/video` VLC deployments on both clusters were left
untouched; everything here lives in a new `ramp-demo` namespace.

## 3. AppBundle vs RecoveryGroup, concretely

`AppBundle video-stream-service` — 2 groups, 4 components, deployment dependency
`data → streaming` (strict). Verified `status.phase: Deployed` with a
`resourceRef` recorded for every component.

`RecoveryGroup video-stream-rg` — **2 members**:

```yaml
members:
  - name: redis
    componentRef: {group: data, component: redis}
    recoveryDriver: redis-replication
    sourceEndpoint: {host: 192.168.28.238, port: 30379}
    consistencyKeys: ["ramp:video:position"]
  - name: video
    componentRef: {group: streaming, component: video}
    recoveryDriver: container-checkpoint
    container: video
recoveryContract: {rto: 60s, rpo: 5s}
```

The two Services are AppBundle components and **not** RecoveryGroup members.
They reappear as RecoveryPath prerequisites (`TargetNamespaceReady`, and the
standby endpoint the promotion acts on).

No Kubernetes template is copied into the RecoveryGroup. Members are resolved
at runtime through the AppBundle's own status:

```
component {data, redis} -> status…resourceRef {apps/v1 Deployment ramp-demo/redis-primary}
                        -> selector -> Pod redis-primary-…  -> node workload01-md-0-4qfvd-gnqcb
```

## 4. The Recovery Epoch

```
PREPARE    resolve both members through the AppBundle; bind drivers
   |
QUIESCE /  wait until the Redis primary has a connected replica AND the
BARRIER    application's logical position is readable. The application is NOT
   |       stopped; the barrier establishes a position both artifacts can be
   |       related to. Capturing the container before this point would produce
   |       an in-memory position no replicated Redis state could match.
CAPTURE    redis : INFO replication -> master_repl_offset, replid,
   |               GET ramp:video:position -> logical position
   |       video : read the container's in-memory state (the app's own state
   |               file, via apiserver exec) to learn its logical position,
   |               THEN POST the kubelet Container Checkpoint API,
   |               THEN wait for the checkpoint-agent to land the tar in MinIO
   |
VALIDATE   |checkpointPosition - replicatedPosition| <= maxPositionSkew
   |       plus the application invariant frames == position * FPT, checked
   |       while reading the in-memory state. Either failing means the epoch
   |       is NOT recovery eligible.
   |
COMMIT     phase=Committed, validation.validated=true
```

Only `phase == Committed && validation.validated` makes a RecoveryPoint
recovery eligible (`RecoveryPoint.RecoveryEligible()`), and that is the only
gate the Readiness Controller consults. A failed epoch stays on record as its
own object rather than being retried in place, so the history of what was and
was not recoverable is auditable — `video-stream-rg-epoch-1` (Failed) is still
listed next to the committed epochs.

Observed epoch (epoch 4):

```
barrierReached   09:54:36
captureStart     09:54:36
captureComplete  09:54:42      (kubelet checkpoint itself: 1.3-1.9 s)
commitTime       09:54:42
validation       checkpointPosition 37, replicatedPosition 37, skew 0, validated true
artifacts        containerCheckpoint  minio://checkpoints/checkpoint-…tar  (~10 MB)
                 replicationState     redis-repl://192.168.28.238:30379/<offset>
```

## 5. The RecoveryPath

One path, `video-workload01-to-workload02`, deterministically evaluated against
8 mandatory checks. Its readiness is genuinely dynamic — observed transitions:

| Event | Readiness | Why |
|---|---|---|
| path created, no epoch yet | **COLD** | `RecoveryPointCommitted` false |
| epoch committed | **WARM** | artifact in MinIO but not on a target node |
| path prepared (artifact staged) | **HOT** | all 8 checks true, estimatedRTO 17s |
| newer epoch committed | **WARM** | path adopts the newer point, whose artifact is not staged |
| path prepared again | **HOT** | |

That HOT→WARM drop on a *newer, better* recovery point is the clearest
demonstration that readiness is a resource that is spent and must be
replenished, not a static property of a target cluster.

## 6. Failure injection

Deliberately bounded and reversible:

```
kubectl --kubeconfig workload01.kubeconfig \
  scale deploy/video-session deploy/redis-primary -n ramp-demo --replicas=0
```

This removes exactly the RecoveryGroup's two members. The source cluster, its
control plane, checkpoint-agent, ArgoCD, Longhorn and the entire management
plane stay up, so the run is repeatable (`scripts/ramp-scenario1/10-reset.sh`
restores the steady state). Nothing in the lab is destroyed.

## 7. Recovery

```
source failure
      |
select the path that was HOT *before* the failure      <- see note below
      |
promote redis standby   REPLICAOF NO ONE               ~1 s
      |
verify role=master and the committed position
      |
restore video from the staged checkpoint image         <- verified, 04-results.md Sec.4.1
      |
video reconnects to the promoted Redis
      |
validate the group invariant
      |
application operational on workload02
```

**Note on latched readiness.** Once the source is gone the replication link
drops, so `RedisStandbyReady` goes false and the path necessarily degrades to
WARM. The recovery decision must therefore use the readiness that held *before*
the failure. The runner records it explicitly (`latched_recoverypoint`). This is
a real design consequence that only showed up by running the scenario: a
readiness value is a statement about a moment, and a recovery controller needs
to latch the last-known-good evaluation rather than re-evaluate mid-incident.
