# PreparedRecoveryPoint + contract-aware readiness: results

Live testbed (management `sre`, source `workload01`, target `workload02`),
2026-09-22. Raw evidence:

| run | directory |
| --- | --- |
| state machine, Tests A–G | `evidence/prepared-recovery-point-20260922T162158/` |
| readiness stability | `evidence/readiness-stability-20260922T161819/` |
| Q > P RecoveryEpoch regression | `evidence/recovery-epoch-qgtp-20260922T162526/` |
| RecoveryEpoch negative tests (fault injection via annotation) | `evidence/recovery-epoch-negative-20260922T161357/` |

Superseded runs are kept because each one is the reason for a specific fix:

| directory | what it showed |
| --- | --- |
| `evidence/prepared-recovery-point-20260922T160332/` | first full pass of A–G, before the probe-flap fix |
| `evidence/prepared-recovery-point-20260922T161007/` | the run whose Test B recorded `readiness=WARM` in its observation while asserting HOT a fraction later — the evidence that the path was flapping out of HOT every refresh cycle |
| `evidence/recovery-epoch-qgtp-20260922T161512/` | Q>P regression on the readiness changes, before the flap fix |

## 1. Result table

| Test | Expected | Observed | Pass |
|------|----------|----------|------|
| A — RP1 prepared | prepared=RP1, candidate=none, HOT | `prepared=epoch-46 candidate=none latest=epoch-46 readiness=HOT` | **PASS** |
| B — RP2 arrives | HOT stays on RP1, RP2 becomes candidate | `prepared=epoch-46 candidate=epoch-47 latest=epoch-47 readiness=HOT` | **PASS** |
| C — RP2 prepared | HOT switches to RP2, artifacts identify RP2 | `prepared=epoch-47 candidate=none readiness=HOT artifactsPointAtRP2=yes` | **PASS** |
| D — RP stale | FreshEnough True→False at the deadline, HOT lost | `freshEnough=false age=2m38s rpo=156s readiness=WARM transitionAt=16:25:23Z` | **PASS** |
| E — restore image removed | RestoreArtifactReady=False, HOT lost | `False / CheckpointImageMissingOnNode, readiness=WARM, detected in 16 s` | **PASS** |
| F — target placement invalid | HOT lost with an explicit reason | `TargetPlacementFeasible=False, readiness=COLD` | **PASS** |
| G1 — RTO 60s > estimated | contract pass, HOT | `rto=1m0s estimated=12s within=true readiness=HOT` | **PASS** |
| G2 — RTO 5s < estimated | contract fail, HOT lost | `rto=5s estimated=12s within=false readiness=WARM` | **PASS** |
| Stability — 3 min, 4 probe generations | never leaves HOT | `36 samples, 0 non-HOT` | **PASS** |
| Q > P regression | Redis and Video both restored to P | `P=409 Q=435 redis=409 video=409` | **PASS** |

## 2. Test B is the point of the whole change

```
epoch-46 committed and prepared               -> HOT
epoch-47 committed                            -> latest    = epoch-47
                                                 candidate = epoch-47
                                                 prepared  = epoch-46
                                                 readiness = HOT          <-- unchanged
```

Before this work the second line would have moved the path to WARM, because it
evaluated `RecoveryGroup.status.latestRecoveryPoint`. Nothing had been lost:
epoch-46's RDB, checkpoint tar, checkpoint image and activation plan were all
still exactly where they were. Committing an epoch — which is supposed to
*improve* the recovery position — degraded the readiness of a path that could
already recover.

The candidate record says so explicitly:

```yaml
candidateRecoveryPoint:
  name: video-stream-rg-epoch-47
  epoch: 47
  missingPreparation: [RestoreArtifactReady, ActivationPlanPrepared]
  message: "a newer committed RecoveryPoint is being prepared; the path keeps
            recovering from video-stream-rg-epoch-46 until epoch-47 is proven executable"
```

## 3. Promotion is atomic and attributed (Test C)

After `36-prepare-target.sh RP=epoch-47`, every prepared artifact identifies
epoch-47 — the test asserts that none of them still references epoch-46:

```yaml
preparedRecoveryPoint:
  name: video-stream-rg-epoch-47
  epoch: 47
  logicalPosition: 250
  commitTime: "2026-09-22T16:22:45Z"
  preparedAt:  "2026-09-22T16:23:40Z"
  executable: true
  placement: {cluster: workload02, node: workload02-md-0-rx5mn-pjkrh}
  artifacts:
    - kind: redisSnapshot         ref: minio://checkpoints/ramp-redis-epoch/…epoch-47.rdb  sha256 945d6cf7…
    - kind: videoCheckpoint       ref: minio://checkpoints/checkpoint-…2026-09-22T16:22:40Z.tar
    - kind: activationPlan        ref: docker.io/ramp/checkpoint-video-session:…epoch-47   node: workload02-md-0-rx5mn-pjkrh
    - kind: videoCheckpointOnNode ref: checkpoint-…2026-09-22T16:22:40Z.tar                node: workload02-md-0-rx5mn-pjkrh
    - kind: checkpointImage       ref: docker.io/ramp/checkpoint-video-session:…epoch-47   node: workload02-md-0-rx5mn-pjkrh
```

The last two are node-bound and carry the node they were verified on. The
activation plan is accepted only because the target Deployment is annotated
`prepared-recovery-point: video-stream-rg-epoch-47`; an annotation naming a
different point is rejected with `ActivationPlanForDifferentRecoveryPoint`.

So `prepared = epoch-47 while the target still holds epoch-46's image` cannot
be reported.

## 4. Contract enforcement

### RPO (Test D)

The RPO was narrowed to `age + 20s` so the True → False transition could be
observed at a known deadline. From `evidence/prepared-recovery-point-20260922T162158/rpo-contract-test.txt`:

```
prepared RecoveryPoint : video-stream-rg-epoch-47
RPO set to             : 156s

observed at            age        freshEnough  readiness remaining
...                    2m20s      True         HOT       16s
...                    2m36s      True         HOT       0s
2026-09-22T16:25:23Z   2m38s      False        WARM      0s     <-- transition
```

WARM, not COLD: the point is still fully restorable, it just loses more history
than the contract allows. The check says exactly that.

### RTO (Test G)

```
estimated activation latency = 12s   (sum of the required activation steps)
RTO = 60s -> EstimatedRTOWithinContract = true,  readiness HOT
RTO =  5s -> EstimatedRTOWithinContract = false, readiness WARM
```

12 s is the sum of the four always-required steps (3 + 2 + 6 + 1); the four
preparation steps are marked `required: false` because preparation already paid
for them. The measured end-to-end activation in the Q>P run was 11 s. The full
breakdown is in `evidence/prepared-recovery-point-20260922T162158/rto-contract-test.txt` and in
`status.contract.activationSteps` on every evaluation.

## 5. Degradation (Tests E and F)

| removed | check | reason | readiness |
| --- | --- | --- | --- |
| CRI checkpoint image deleted from the placement node | `RestoreArtifactReady=False` | `CheckpointImageMissingOnNode` | HOT → WARM in 16 s |
| placement node cordoned | `TargetPlacementFeasible=False` | `NoFeasibleNode` | HOT → COLD |

Test E's reason is the definitive probe verdict (exit code 11: tar present,
image absent), not merely "the probe has not finished". Test F's message names
every rejected node and why:

```
no single target node satisfies every node-level prerequisite:
  workload02-control-plane-q5vlg: control-plane node (pin it via spec.targetPrereqs.node to use it anyway);
  workload02-md-0-rx5mn-pjkrh: node is cordoned
```

Both recovered to HOT automatically after the image was rebuilt / the node was
uncordoned.

## 6. Target placement

One concrete node, with free capacity computed as allocatable minus the requests
already on it:

```yaml
targetPlacement:
  cluster: workload02
  node: workload02-md-0-rx5mn-pjkrh
  allocatableCpuMillis: 8000    requestedCpuMillis: 2010    freeCpuMillis: 5990
  allocatableMemoryMiB: 15888   requestedMemoryMiB: 824     freeMemoryMiB: 15064
  reason: PreservedPreviousPlacement
```

The predecessor reported `best target node allocatable: 8000m CPU, 15888MiB` —
maxima taken independently across nodes, and allocatable presented as free. Both
are gone.

## 7. Q > P regression (Definition of Done item 13)

Re-run after all readiness changes, and again after the stability fix
(`evidence/recovery-epoch-qgtp-20260922T162526/`):

```
P = 409          Q = 435          ObservedLogicalLoss = 26
live replica at failure = 436
Redis restored initial position = 409     (measured before Video started)
Video restored initial position = 409     (same instance fingerprint)
VERDICT: PASS
```

The four RecoveryEpoch negative tests also still pass with fault injection moved
out of the API and onto a test-only annotation.

## 8. Definition of Done

| # | requirement | status |
| --- | --- | --- |
| 1 | latest RP is no longer equated with prepared RP | yes — three distinct status fields |
| 2 | Candidate and Prepared are distinguishable | yes — Test B |
| 3 | a newer RP does not destroy a valid HOT path | yes — Test B |
| 4 | promotion only after preparation succeeds | yes — `preparationComplete()`, Test C |
| 5 | preparation artifacts tied to the exact epoch | yes — annotations + `preparedRecoveryPoint.artifacts` |
| 6 | freshness enforced against RPO | yes — Test D |
| 7 | estimated activation latency checked against RTO | yes — Test G |
| 8 | HOT means both RPO and RTO satisfiable | yes — both are mandatory checks |
| 9 | actual restore artifact/image readiness checked | yes — `RestoreArtifactReady`, Test E |
| 10 | placement is one concrete feasible node | yes — `selectPlacement` |
| 11 | node-local artifact readiness matches that node | yes — probe pinned with `nodeName` |
| 12 | loss of artifacts or placement degrades readiness | yes — Tests E and F |
| 13 | Q > P RecoveryEpoch correctness still passes | yes |
| 14 | evidence saved and reproducible | yes — three scripts, three evidence directories |

## 9. Reproducing

```bash
scripts/ramp-scenario1/98-prepared-recovery-point-tests.sh   # Tests A-G
scripts/ramp-scenario1/99-readiness-stability.sh             # no-flap regression
scripts/ramp-scenario1/95-qgtp-experiment.sh                 # Q > P regression
scripts/ramp-scenario1/96-negative-tests.sh                  # epoch abort paths
```
