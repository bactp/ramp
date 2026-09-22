# RecoveryEpoch correctness: results

All numbers below come from the live testbed (management cluster `sre`, source
`workload01`, target `workload02`) on 2026-09-22. Every run's raw evidence is
under `evidence/` — see [the index](#7-evidence-index).

## 1. What was wrong, in one paragraph

A committed RecoveryPoint recorded the Redis member as
`redis-repl://<host>:<port>/<offset>` — a pointer at a live server. The replica
keeps applying the stream after commit, so once the source advanced from `P` to
`Q` nothing in the system still held `P`. Recovery promoted the replica, which
produced `Video = P, Redis = Q`; the restored Video then reconnected and wrote
its own position, rolling Redis back from `Q` to `P+1` and making every
end-state health check pass. On top of that the application was never paused
during capture, so the two artifacts could only ever be *close* (hence the
`maxPositionSkew: 2` tolerance), and the workload's `next_tick += 1.0` schedule
made a restored process replay one tick per second of downtime.

## 2. The correctness gate: Q > P

`scripts/ramp-scenario1/95-qgtp-experiment.sh`, final run
`evidence/recovery-epoch-qgtp-20260922T084434/`:

| quantity | value |
| --- | --- |
| epoch E | 29 |
| committed position **P** | **2112** |
| source position at failure **Q** | **2151** |
| ObservedLogicalLoss = Q − P | **39** |
| live replica position at failure | 2152 |
| **Redis restored initial position** | **2112** (measured before Video was started) |
| **Video restored initial position** | **2112** |
| Video position ~6 s after release | 2118 |
| Redis position after resume | 2127 |
| source instance fingerprint | `5ad2f48c-d435-483c-bd13-44ee39afa759` |
| restored instance fingerprint | `5ad2f48c-d435-483c-bd13-44ee39afa759` (identical ⇒ CRIU restore, not a restart) |

All eight assertions pass, including the two the old design could not satisfy:
*Redis restored initially to P* and *Redis did NOT recover to Q*. The live
replica was at 2152 at failure time — i.e. the naive promotion was available and
would have been wrong by 40 positions.

**VERDICT: PASS.**

### Why the measurement is trustworthy

The measurement happens in the window where **only one member is running**:

```
restore Redis -> verify GET ramp:video:position == P
              -> verify GET ramp:epoch:29:position == P   (marker inside the RDB)
              -> verify GET ramp:epoch == 29
restore Video -> read its first published state == P
              -> only then release it
```

The restored Video cannot corrupt the evidence even by accident: the checkpoint
was taken while it was quiesced, a CRI checkpoint carries the container's
writable layer, so it comes back **still quiesced at P** and cannot write until
`50-recover.sh` releases it explicitly.

## 3. Negative tests

`scripts/ramp-scenario1/96-negative-tests.sh`,
`evidence/recovery-epoch-negative-20260922T084632/`:

| test | phase | failureReason | last successful stage | app quiesced after | app position | verdict |
| --- | --- | --- | --- | --- | --- | --- |
| 1 Redis replica ACK unavailable | Failed | `ReplicaAckNotAchieved` | Quiescing | false | 6 → 12 | PASS |
| 2 Redis epoch snapshot fails | Failed | `RedisSnapshotFailed` | BarrierEstablished | false | 16 → 22 | PASS |
| 3 Video checkpoint fails after the Redis artifact exists | Failed | `CheckpointFailed` | CapturingRedis | false | 22 → 29 | PASS |
| 4 Redis artifact and Video state disagree | Failed | `ValidationFailed` | CapturingVideo | false | 30 → 36 | PASS |

Test 1 is a real fault (the replica is scaled to zero), not an injected one:
`WAIT 1 10000` returned 0 and the epoch refused to proceed. Note that
`connected_replicas` would still have been momentarily non-zero — which is
exactly why it was never a barrier.

Test 3's orphan is explicitly accounted for:

```
orphan redis artifact: minio://checkpoints/ramp-redis-epoch/video-stream-rg-epoch-32.rdb
  (fetchable, 373 bytes — the object exists)
group's latest recovery point is still: video-stream-rg-epoch-29
aborted epoch: phase=Failed validated=false  ⇒ never recovery eligible
```

An object in the store is not a recovery point. `RecoveryEligible()` gates on
`Committed AND validated`, and `RestorableArtifact()` refuses to return a
`replicationState` record at all.

In all four cases the application position advanced after the abort, so
"resumed" means *running*, not merely *flag cleared*.

## 4. Regression tests

`scripts/ramp-scenario1/97-regression-tests.sh`,
`evidence/recovery-epoch-regression-20260922T084741/`:

**R1 — timer catch-up.** Paused for 20 s at position 36; position after the pause
was still 36; 5 s after resume it was 40. Four positions in five seconds. The
pre-fix `next_tick += 1.0` would have produced ~25. PASS.

**R2 — manager restart mid-epoch.** The manager was killed while epoch 34 was in
`Quiescing` with the application paused. After restart:

```
phase                      = Failed
failureReason              = EpochInterrupted
message                    = ... runId 0b9616a0-… is gone; it stopped after stage Preparing.
                             An interrupted epoch is never resumed ...
application quiesced after = False
application position       = 41 -> 75   (running again)
```

PASS. The epoch did not silently complete from artifacts captured at different
times, and the application the dead process had paused was released.

## 5. Timing

### Capture overhead (the quiesce window)

Eight committed epochs on the corrected controller:

```
   E      P  quiesce+verify  barrier  redisRDB   ckpt  validate  quiesce window
  15   7122               4        0         0      6         0              10
  16   7314               5        0         0      5         0              10
  17    110               4        0         0      6         0              10
  18     66               5        0         0      5         1              11
  19    102               4        0         0      6         0              10
  26    144              26*       0         0      6         0              32
  29   2112               4        0         0      5         0               9
  35    121               4        0         0      6         0              10
                                                        (* quiesceVerifySeconds=25)
```

**Median quiesce window: 10 s**, of which ~6 s is the kubelet Container
Checkpoint call and ~4 s is the quiesce handshake plus the configured 3 s
double-sample. The Redis barrier and the epoch RDB are each under a second
(509-byte dataset). The window scales with `quiesceVerifySeconds`, which is the
knob that trades application pause time against confidence that the application
really stopped.

This is real downtime for the source workload's *logical progress* — the process
keeps running and keeps serving, it just does not advance the stream.

### Recovery Activation Latency

Deliberately **not** called RTO: there is no failure detection in this system
yet, so nothing measured here includes detection. From
`evidence/recovery-epoch-qgtp-20260922T084434/timing.json`:

| interval | value |
| --- | --- |
| `T_recovery_start → T_redis_ready` (load epoch RDB, restart, verify at P) | 3 s |
| `T_video_restore_start → T_video_pod_ready` (ArgoCD commit + containerd CRIU restore) | 6 s |
| `T_video_pod_ready → T_video_resumed` (verify at P, release) | 1 s |
| **Recovery Activation Latency** (`T_recovery_start` → restored group released at P) | **11 s** |
| `T_group_ready − T_recovery_start` | 25 s (includes a deliberate 6 s progression-observation dwell) |
| `T_group_ready − T_failure` | 57 s (of which 32 s is the failure injection itself waiting for pods to terminate) |

The path was `HOT` at failure time: artifact staged, checkpoint image built on
the target node, ArgoCD wiring and a scaled-to-zero workload object already in
place. All of that is preparation and none of it is in the 11 s.

### RPO

For this RecoveryGroup the recovery loss of `RP-E` is

```
ObservedLogicalLoss = Q - P = 2151 - 2112 = 39 positions (~39 s of stream)
```

reported as **one** number for the group. Per-member figures ("Redis RPO = 0")
are diagnostics only: the Redis replica being at 2152 did not make the
RecoveryGroup's loss smaller, because 2152 was never a committed recovery point.
`Q − P` is a function of epoch cadence, and epoch cadence is a policy this task
does not set.

## 6. Definition of Done

| # | requirement | status |
| --- | --- | --- |
| 1 | Video actually quiesced during capture | yes — `status.quiesce.verifySamples: [P, P]` |
| 2 | Redis replication ACK verified for the epoch state | yes — `WAIT 1` on the marker's own connection |
| 3 | Redis has an epoch-specific immutable artifact | yes — `redisSnapshot`, sha256, epoch-unique key |
| 4 | Video has an epoch-specific immutable artifact | yes — `containerCheckpoint` in MinIO |
| 5 | Both validated against the same P | yes — skew 0, tolerance 0 |
| 6 | Commit only after validation | yes — ten mandatory checks |
| 7 | Failed captures never commit | yes — negative tests 1–4 |
| 8 | Application always resumed | yes — defer; verified by position advance in every abort |
| 9 | Source continues P → Q after commit | yes |
| 10 | Source can fail at Q > P | yes |
| 11 | Recovery from RP-P restores Redis to P | yes — 2112, verified before Video started |
| 12 | Recovery from RP-P restores Video to P | yes — 2112, same instance fingerprint |
| 13 | Redis does NOT initially recover to Q | yes — replica held 2152; it was not used |
| 14 | Video does not hide a Redis mismatch | yes — restored still-quiesced; measured before release |
| 15 | Both progress normally from P | yes — 2112 → 2118 in 6 s, 1 position/s |
| 16 | Timing and correctness evidence saved | yes — `evidence/recovery-epoch-*` |

## 7. Evidence index

| directory | what it is |
| --- | --- |
| `evidence/recovery-epoch-qgtp-20260922T084434/` | **authoritative Q > P run** on the final build — PASS |
| `evidence/recovery-epoch-negative-20260922T084632/` | **authoritative negative tests** on the final build — 4/4 PASS |
| `evidence/recovery-epoch-regression-20260922T084741/` | **authoritative regression tests** on the final build — R1 + R2 PASS |
| `evidence/recovery-epoch-qgtp-20260922T080236/` | first end-to-end PASS, before the cache-safety fix |
| `evidence/recovery-epoch-qgtp-20260922T075731/` | Redis restored to P correctly; Video restored to P but stayed frozen — the run that established that a CRI checkpoint carries the quiesce control file, and that the recovery must release it |
| `evidence/recovery-epoch-qgtp-20260922T075230/` | video restore failed: `runc restore` path, superseded by the containerd checkpoint-image path |
| `evidence/recovery-epoch-qgtp-20260922T074337/` | video restore failed: the staging step was not pinned to this epoch's RecoveryPoint and staged the previous epoch's artifact |
| `evidence/recovery-epoch-negative-20260922T080609/` | earlier negative run, 2/4 — exposed the epoch-numbering bug (`recoverygroup.status.latestEpoch` lags, so back-to-back epochs reused a number) |
| `evidence/recovery-epoch-regression-20260922T08{0916,1039,1954}/` | earlier regression runs; R2 fixtures being debugged |

The failed runs are kept deliberately: each one is the reason for a specific fix
listed in the task report, and a run that only ever shows the final state is not
evidence of anything.
