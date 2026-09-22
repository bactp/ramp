# 04 — Results

Reference run: `evidence/scenario1/run-20260921T095857/`, executed by
`scripts/ramp-scenario1/00-run-scenario.sh` on 2026-09-21.

---

## 1. Timing

| Marker | Time | Δ |
|---|---|---|
| `T_run_start` | 09:58:57 | |
| `T_checkpoint_start` (capture begins) | 09:58:59 | |
| `T_barrier_reached` | 09:58:59 | < 1 s |
| `T_checkpoint_complete` | 09:59:04 | **5 s** capture (kubelet call itself 1.3–1.9 s) |
| `T_recoverypoint_commit` | 09:59:04 | epoch total **5 s** |
| `T_prepare_start` | 09:59:05 | |
| `T_hot_reached` | 09:59:38 | **33 s** WARM→HOT (7 s staging + readiness re-probe) |
| `T_failure` | 09:59:39 | |
| `T_failure_complete` | 10:00:10 | 31 s for pods to terminate |
| `T_recovery_start` | 10:00:10 | |
| `T_redis_ready` | 10:00:11 | **1 s** — Redis promotion |
| `T_checkpoint_restore_attempt_done` | 10:00:23 | 12 s — failed in that run; the mechanism was wrong, see §4.1 |
| `T_video_restored` | 10:00:26 | |
| `T_group_ready` | 10:00:26 | |

**Observed RTO = 16 s** (`T_group_ready − T_recovery_start`), against the
`RecoveryPath.status.estimatedRTO` of **17 s** published while the path was HOT.
The estimate is a sum of the remaining activation steps, not a prediction, and
it landed within one second of the measurement.

### RPO — and why the two mechanisms differ

| | position | |
|---|---|---|
| committed RecoveryPoint (epoch 5) | **295** | what the container checkpoint captured |
| source at the moment of failure | **335** | `prefailure-video-state.json` |
| target after Redis promotion | **335** | `redis_position_after_promotion` |
| target after recovery | 336 → continuing | `recovered-video-state.json` |

* The **redis-replication** leg delivered **RPO = 0 positions**: continuous
  replication meant the standby already held position 335.
* The **container-checkpoint** leg was 40 positions (≈40 s) behind, because a
  checkpoint is a point-in-time artifact and the epoch was taken 40 s before the
  failure.

That gap is the substance of the argument. A design that checkpointed both
members would have had an RPO of 40 for *all* state. A design that replicated
both members cannot exist — process memory is not replicable. Recovering each
member by the mechanism suited to it, and validating that the results are
mutually consistent, is what the RecoveryGroup exists to coordinate.

---

## 2. What the heterogeneous recovery point actually contained

`RecoveryPoint video-stream-rg-epoch-5`, `phase: Committed`, `validated: true`:

```yaml
artifacts:
- member: redis
  driver: redis-replication
  type:   replicationState
  ref:    redis-repl://192.168.28.238:30379/<offset>
  replicationOffset: <offset>
  replicationId:     e2491b64e6400f7c508ef3e405a6ac35d429d12d
  logicalPosition:   295
- member: video
  driver: container-checkpoint
  type:   containerCheckpoint
  ref:    minio://checkpoints/checkpoint-video-session-…tar
  nodePath: /var/lib/kubelet/checkpoints/checkpoint-…tar
  sizeBytes: ~10 MB
  logicalPosition: 295
validation:
  checkpointPosition: 295
  replicatedPosition: 295
  observedSkew: 0
  reason: ConsistentAcrossDrivers
  validated: true
```

Two artifacts, two mechanisms, two formats — one position. The validation is
falsifiable: `maxPositionSkew` is a spec field, and exceeding it marks the epoch
`Failed` rather than `Committed`, which makes it permanently ineligible for
recovery.

**An incomplete epoch never becomes recovery eligible.**
`video-stream-rg-epoch-1` is still in the cluster with `phase: Failed`,
`validated: false` (its capture failed on the kubelet `?timeout=` parameter
bug). It was never adopted as `latestRecoveryPoint`, and the readiness
controller's only gate is `RecoveryPoint.RecoveryEligible()` —
`Committed && validated`.

---

## 3. Readiness behaved as an elastic resource

Observed, not asserted:

| Event | Readiness | `unmetMandatoryChecks` | estimatedRTO |
|---|---|---|---|
| path created, no epoch | COLD | RecoveryPointCommitted, VideoCheckpointAvailable, VideoCheckpointStaged | 5m0s |
| epoch committed | WARM | VideoCheckpointStaged | 47s |
| path prepared | **HOT** | — | 17s |
| **newer** epoch committed | WARM | VideoCheckpointStaged | 47s |
| path prepared again | **HOT** | — | 17s |
| source failed | WARM | RedisStandbyReady | 47s |

The HOT→WARM drop on a *newer and better* recovery point is the clearest single
piece of evidence: readiness is not a property of the target cluster. It is
something that is spent and must be replenished per path, per epoch.

A HOT path, in full:

```
 True RecoveryPointCommitted      Committed: epoch 5 committed, cross-driver skew 0
 True RedisStandbyReady           StandbySynchronized: role=slave link=up ackedOffset=…
 True RestoreCapabilityAvailable  AgentReady: kube-system/checkpoint-agent ready on 2/2 nodes
 True TargetClusterReachable      NodesReady: 2/2 nodes Ready on workload02
 True TargetNamespaceReady        NamespaceActive
 True TargetResourceReady         AllocatableChecked: 8000m CPU, 15888MiB (required 2000m/2048MiB)
 True VideoCheckpointAvailable    ArtifactPresent: …tar (10051072 bytes) in bucket checkpoints
 True VideoCheckpointStaged       ArtifactStagedOnTarget: present on node workload02-md-0-rx5mn-pjkrh
```

### The finding that most supports the thesis

The existing checkpoint-agent stages by pulling **every object in the bucket**
every `PULL_INTERVAL`. Measured on this testbed: **238 objects / 5.32 GB**, with
the artifact the current RecoveryPoint needs sorting **last** alphabetically. A
10 MB artifact therefore costs **5.31 GB of transfer** before it is staged —
staging latency is a function of total artifact-store size, not of what the
recovery path needs.

Replacing that with path-directed staging (`30-prepare-path.sh`, which pulls
exactly the object the current RecoveryPoint names) staged the artifact in
**7 seconds**. That is the difference between fixed periodic preparation of
everything and spending readiness where it is needed — the concrete form of the
claim RAMP makes.

---

## 4. What failed

### 4.1 Container-checkpoint restore — CORRECTED: it works

**An earlier revision of this document concluded that a kubelet checkpoint
cannot be restored on a containerd cluster. That conclusion was wrong.** It was
reached by reasoning from how CRI-O implements checkpoint restore rather than by
testing the mechanism this lab actually uses, and by attempting the wrong
mechanism (bare `runc restore`) five times. Evidence in
`evidence/scenario1/restore-verified/`.

**containerd 2.1.4 restores from a checkpoint OCI image.** The pre-existing
`default/video` containers on *both* workload clusters carry, in their CRI
runtime config and **not** in any Pod or Deployment manifest:

```
checkpointImage = docker.io/phuongbac/checkpoint-default_video-77b57c57bc-cqnzv-vlc:latest
checkpointedAt  = 2026-08-12T15:28:00Z
restored        = true
```

containerd generated those itself. The lab's restore path — checkpoint → OCI
image (`buildah from scratch` + the tar + two `org.criu.checkpoint.*`
annotations) → rewrite the image in the Gitea manifest → ArgoCD sync → the
runtime restores instead of starting fresh — **is real and has been working all
along**. `imageID` reporting the *rootfs* image rather than the checkpoint image
is the correct signature of a restore, not evidence of failure.

#### Reproduced for Scenario 1's own workload

`config/ramp/restore/build-checkpoint-image.py` reproduces the buildah image
without buildah (not installed on sre-control) and imports it straight into the
target node's containerd, so nothing has to be published externally.

The discriminator is the video member's `session` UUID: generated once in
memory at process start, never read back from Redis. A CRIU restore keeps it; a
cold restart replaces it.

| | session uuid | position |
|---|---|---|
| source process, workload01, never restarted | `319f64cb-…4d42104` | 59388 |
| epoch-6 checkpoint captured at | same process | **165** |
| **restored on workload02** | **`319f64cb-…4d42104`** | resumed **165 → 215** |
| cold-restart fallback, for contrast | `2a004627-…` (new) | jumped to the Redis position |

Same UUID, and resumption from 165 rather than 59388: the process memory was
restored. `redis_linked: true` — it also reconnected to the promoted Redis on
the target. The runtime confirms `restored = true`.

So Scenario 1 does demonstrate the full claim: **video recovered by
container/runtime checkpoint, Redis recovered by replication, coordinated as one
RecoveryGroup against one validated recovery point.**

The GitOps actuation half was subsequently verified end to end as well -- git
push to restored workload in 9 seconds, same in-memory session UUID. See
[05-gitops-recovery-driver.md](05-gitops-recovery-driver.md).

#### The real blocker, and it is a genuine interop bug

Restore failed three times before working, each time for a different reason in
the *metadata*, never in the checkpoint data:

| Attempt | Error | Cause |
|---|---|---|
| 1 | `parent snapshot … does not exist` | imported with `ctr images import --no-unpack`; containerd needs the layer unpacked into a snapshot |
| 2 | `parse "dummy://sha256:9e8797…": invalid port` | `config.dump.rootfsImageRef` was a **bare digest** |
| 3 | `failed to pull checkpoint base image docker.io/ramp/videobase@sha256:9e8797…: pull access denied` | the reference was well-formed but **not pullable** |

That third error exposes the underlying problem:

> **containerd's restore path PULLS `rootfsImageRef` from a registry. It does not
> look it up in the local image store — even though the image is already on the
> node.**

And the kubelet writes into `rootfsImageRef` whichever identifier the node
happened to hold. Measured on this testbed, for the *same Deployment*, hours
apart:

| value recorded | what it is | pullable? |
|---|---|---|
| `docker.io/library/python@sha256:2c941e86…` | Hub index digest | ✅ |
| `sha256:9e87977b…` | node-local image ID (config digest) | ❌ |

When the second form is recorded, restore is impossible even though the artifact
is perfect and the rootfs image is sitting on the node. `build-checkpoint-image.py`
normalises the field to a registry-resolvable tag, which is what made the restore
succeed.

**This is why the historic `default/video` restores worked and a naive retry
would not**: `tuongvx/vlc-app` was recorded as a proper public reference. It is a
latent fragility in the existing pipeline, not something RAMP introduced, and it
is worth reporting upstream.

### 4.2 Two defects found in components RAMP does not own

Both recorded with evidence, neither patched (RAMP's diff against every
pre-existing repository is zero):

* **Transition Operator**, `checkpoint_controller.go` ~L197-213 — a shadowed
  `nodeKubeletIP := addr.Address` inside the address loop means the outer
  variable stays `""` and the kubelet client silently falls back to
  `https://localhost:10250`. Separately, it only considers `NodeInternalIP`,
  which is unroutable from mgmt here, and the Node objects carry no
  `ExternalIP` — a correct fix must join the Node to its CAPI `Machine`.
* **checkpoint-agent DaemonSet** (`deploy/checkpoint-agent-daemonset.yaml`)
  mounts the checkpoint directory `readOnly: true`. Correct for the source
  (upload-only) role; it makes the target/staging role impossible —
  `syncFromMinio()` fails on every object with `read-only file system`. RAMP
  adds `config/ramp/checkpoint-agent/checkpoint-agent-target-role.yaml` as an
  overlay rather than editing the upstream manifest.

### 4.3 A soundness bug in RAMP, found by running it

After `10-reset.sh` recreated the Redis primary, `RedisStandbyReady` reported
`standby is 27499 bytes behind the recovery point offset` while the standby was
in fact healthy and fully caught up. The check was comparing the standby's
replication offset with the offset stored in a RecoveryPoint **taken from a
different replication stream**: recreating the primary restarts the stream at
offset 0 under a new `master_replid`, so offsets across ids are not comparable
and the subtraction produced a meaningless number.

Fixed in `internal/controller/recoverypath_controller.go`: the comparison is now
guarded by `master_replid`, and a mismatch reports
`RecoveryPointFromDifferentReplicationStream` — which tells the operator the
actionable thing ("take a new epoch") instead of a fabricated lag figure.
Verified: the check now reports the stream mismatch, and after a fresh epoch the
path returns to HOT normally.

Worth stating plainly: this class of bug — a comparison that is arithmetically
fine and semantically void — would not have surfaced from unit tests over
synthetic data. It surfaced because the scenario was reset and re-run against
real Redis.

### 4.4 Nephio

`nephio-controller` shows 8156 restarts and a continuous retry loop on missing
`config-management-system` namespaces for workload02/03. As the brief
anticipated, it was kept off the critical path: the Scenario-1 AppBundle uses
inline templates with `porchIntegration` disabled, so Nephio's state cannot fail
the experiment. Porch itself is stable and remains available for the later
package-driven deployment path.

---

## 5. What worked

* **AppBundle as the application graph.** `status…componentStatuses[].resourceRef`
  plus the `app.example.com/*` labels were sufficient for RecoveryGroup
  resolution with **no extension to AppBundle and no second labelling scheme**.
  The critical implementation question from the brief is answered **yes**.
* **AppBundle ≠ RecoveryGroup, concretely.** 4 deployment components, 2 recovery
  members; the two Services are prerequisites, not members.
* **Heterogeneous epoch.** Committed recovery points carrying a
  `containerCheckpoint` and a `replicationState` artifact validated against each
  other at skew 0.
* **Epoch integrity.** A failed epoch (`epoch-1`) stayed `Failed` and was never
  adopted.
* **Kubelet Container Checkpoint via the apiserver node proxy.** 1.3–1.9 s,
  ~10 MB, reliable across five runs — a working transport where the existing
  direct-kubelet path cannot reach the node at all.
* **Deterministic, explainable readiness.** COLD/WARM/HOT with per-check reasons;
  `kubectl get recoverypath -o yaml` alone explains the level.
* **Path-directed preparation.** 7 s targeted staging vs 5.31 GB of blind sync.
* **Redis-native recovery.** Cross-cluster replication over the floating-IP
  NodePort, promotion in 1 s, RPO 0.
* **Container-checkpoint restore, verified end to end** — the restored process
  on workload02 carried the *same in-memory session UUID* as the source and
  resumed from the checkpoint position, and the runtime reported
  `restored = true`.
* **Zero modification** to `transition-operator`, `checkpoint-agent` or
  `appbundle-operator`.

---

## 6. Next smallest step

Toward **R4 — RecoveryGroup Controller**

1. Derive candidate membership from the AppBundle instead of listing it: a
   component whose runtime resource has a PVC or whose driver capability
   registry reports checkpointability is a candidate member; Services and
   ConfigMaps are prerequisites. Keep explicit override.
2. A `RecoveryDriver` registry so drivers are registered rather than switched on
   in a `switch` statement.

Toward **R5 — Recovery Epoch & Coordinated C/R**

3. **Move artifact promotion into the epoch.** Restore is proven; what is still
   manual is turning a committed RecoveryPoint's checkpoint tar into the OCI
   checkpoint image. Today that is `config/ramp/restore/build-checkpoint-image.py`
   run by hand on the target node. It belongs in the Transition Operator (which
   already does it with buildah), with RAMP only requesting it — plus the
   `rootfsImageRef` normalisation from §4.1, without which restore is a coin
   flip.
4. **Report the `rootfsImageRef` bug upstream** (kubelet records a non-pullable
   local image ID; containerd's restore path pulls it). Two lines of evidence are
   in §4.1.
5. Automatic epoch scheduling driven by the RecoveryContract's RPO, replacing
   manual `20-run-epoch.sh` invocation.

Toward **R6 — Recovery Path & Readiness Manager**

6. **Promote `30-prepare-path.sh` into a Preparation Controller**, kept separate
   from the Readiness Controller so the observe/act boundary holds. This is what
   makes readiness genuinely elastic: it decides *which* paths to keep hot under
   a preparation budget.
7. **Latch readiness.** Recovery must use the last-known-good evaluation, not a
   mid-incident one — the source failure necessarily drives the live path to
   WARM. Add `status.lastHotTime` and `status.latchedRecoveryPoint`.
8. **The ClusterPolicy adapter.** Make `canAutoTransition()` require a HOT
   `RecoveryPath` for the proposed target, and surface the unmet checks into
   `ClusterPolicy.status.recommendation.reason` so the AI approval gate can
   explain *why* a target is not ready. No RAMP API change needed.
