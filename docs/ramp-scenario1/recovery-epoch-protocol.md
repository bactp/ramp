# RecoveryEpoch protocol (Scenario 1)

The protocol one RecoveryPoint executes, and the invariant it exists to
establish. It replaces the pre-fix flow audited in
[recovery-epoch-correctness-audit.md](recovery-epoch-correctness-audit.md).

## 1. The invariant

For every committed RecoveryPoint `RP-E` at logical position `P`:

```
Video state belongs to epoch E at P
AND Redis state belongs to epoch E at P
AND both artifacts remain reproducibly restorable after the source continues
```

Operationally: if the source commits `RP-E` at `P` and then advances to `Q > P`
and fails, recovery from `RP-E` produces `Video = P` and `Redis = P` — never
`Redis = Q`.

The second clause is the one the prototype did not have. A live Redis replica
is a *readiness* mechanism: it keeps the target warm and it shortens recovery.
It is not a recovery point, because the moment the source advances past `P` the
replica no longer holds `P` and nothing in the system does. Every committed
RecoveryPoint therefore carries an **epoch-specific immutable Redis RDB**
alongside the container checkpoint, and replication stays exactly where it was.

## 2. Lifecycle

```
Pending
   |
Preparing            prerequisites verified BEFORE the application is touched
   |
Quiescing            application held at P; position sampled twice to prove it
   |
BarrierEstablished   epoch marker written to the primary; WAIT n acknowledged
   |
CapturingRedis       synchronous SAVE on the quiesced primary -> immutable RDB
   |
CapturingVideo       kubelet Container Checkpoint API -> immutable CRIU tar
   |
Validating           ten mandatory checks, all recorded
   |
Committed            ------------------------------- RESUME (always)
```

Any failure short-circuits:

```
<any phase> -> Aborting -> Failed      ----------- RESUME (always)
```

`RESUME` is a defer, so it also runs on validation failure, on artifact-store
failure, on context cancellation and on a manager restart. A failed epoch never
leaves the source application paused.

## 3. Sequence

```mermaid
sequenceDiagram
    autonumber
    participant RAMP as RAMP<br/>(RecoveryPoint reconciler)
    participant V as Video<br/>(source workload)
    participant RP as Redis Primary<br/>(workload01)
    participant RR as Redis Replica<br/>(workload02, target)
    participant CA as Checkpoint Agent<br/>(kubelet + agent)
    participant AS as Artifact Store<br/>(MinIO)

    Note over RAMP: PREPARE — nothing is stopped yet
    RAMP->>RP: INFO replication (reachable? role=master?)
    RAMP->>AS: stat (bucket reachable?)
    RAMP->>V: GET_STATE (readable? on a known node?)

    Note over RAMP,V: QUIESCE
    RAMP->>V: QUIESCE (control file / POST /quiesce)
    V-->>RAMP: quiesced=true, position=P
    RAMP->>V: GET_STATE
    V-->>RAMP: position=P
    RAMP->>V: GET_STATE (after quiesceVerifySeconds)
    V-->>RAMP: position=P  (two equal samples = really stopped)

    Note over RAMP,RR: BARRIER — one connection, so WAIT means something
    RAMP->>RP: SET ramp:epoch E / SET ramp:epoch:E:position P
    RP->>RR: replicate
    RAMP->>RP: WAIT 1 <timeout>
    RR-->>RP: ACK offset
    RP-->>RAMP: 1 replica acknowledged

    Note over RAMP,AS: CAPTURE REDIS — the immutable epoch artifact
    RAMP->>RP: GET position (== P?)
    RAMP->>RP: rm dump.rdb ; SAVE
    RP-->>RAMP: RDB bytes (contains position P and the epoch marker)
    RAMP->>RP: GET position (still == P?)
    RAMP->>AS: PUT ramp-redis-epoch/<group>-epoch-E.rdb (sha256)
    Note over RR: the replica keeps streaming — untouched, still warm

    Note over RAMP,AS: CAPTURE VIDEO — while still quiesced
    RAMP->>V: GET_STATE (quiesced=true, position=P?)
    RAMP->>CA: kubelet Container Checkpoint (via node proxy)
    CA-->>AS: upload checkpoint-....tar
    RAMP->>AS: wait for the object to land
    RAMP->>V: GET_STATE (checkpoint metadata position = P)

    Note over RAMP: VALIDATE — all ten checks must pass
    RAMP->>AS: stat redis artifact (readable?)

    Note over RAMP: COMMIT RecoveryPoint RP-E

    RAMP->>V: RESUME
    V-->>RAMP: quiesced=false, resumes at P+1 one tick later
```

## 4. Why each step is there

| Step | Without it |
| --- | --- |
| PREPARE before QUIESCE | the application is stopped and only then does the epoch discover the artifact store is down — an outage for nothing |
| QUIESCE | the position read at the start of capture is stale by the time the checkpoint completes; artifacts can only ever be "close" |
| two position samples | one sample cannot distinguish "stopped" from "read between two ticks" |
| epoch marker + `WAIT` on one connection | `connected_replicas > 0` says a socket is attached, not that P was received. `WAIT` on a fresh connection acknowledges nothing, because Redis scopes it to that connection's own writes |
| marker keys written into the dataset | they land inside the RDB, so "this artifact is epoch E at P" is a property of the artifact, not a claim in a status field |
| `SAVE` on the quiesced primary | the primary is the instance the quiesced writer wrote to; after `WAIT` the replica is only guaranteed to be *at least* at P, not exactly at P |
| position read before AND after SAVE | proves the dataset did not move across the save |
| checkpoint after the Redis artifact | if the checkpoint fails, the orphaned RDB is a stray object; if the order were reversed, a committed-looking checkpoint would have no Redis state to pair with |
| VALIDATE before COMMIT | an epoch that cannot prove both artifacts are at P is not a recovery point |
| RESUME in a defer | one failed epoch must not leave production paused |

## 5. Recovery

```
restore Redis from RP-E's RDB
      |
   MEASURE Redis  ---- must be P, and must carry ramp:epoch:E:position = P
      |
restore Video from RP-E's checkpoint
      |
   MEASURE Video  ---- must be P, same instance fingerprint as the source
      |
RESUME Video      ---- only now is the application allowed to write
```

The ordering is a correctness requirement. The restored Video reconnects to
Redis and writes its own position within a second of starting, so a check made
after both members are up reports a consistent system even when Redis was
recovered to the wrong state and then silently overwritten — which is exactly
how the pre-fix defect stayed invisible.

Two properties make the measurement window reliable rather than a race:

* the Redis restore is a load-and-restart of an instance that has been detached
  from replication, verified before anything else is started;
* the Video checkpoint was taken **while quiesced**, and a CRI checkpoint
  carries the container's writable layer, so the restored process comes back
  still quiesced at exactly P and cannot write until recovery releases it.

## 6. Status surface

`kubectl get recoverypoint <name> -o yaml` carries, for both outcomes:

* `status.phase`, `status.lastSuccessfulStage`, `status.failureReason`
* `status.logicalPosition` (P), `status.runId`
* `status.quiesce` — quiesced flag, timestamps, the position samples, instance fingerprint
* `status.barrier` — endpoint, replication id, marker offset, acks required/achieved
* `status.artifacts[]` — type, epoch, ref, `immutable`, checksum, logical position
* `status.validation.checks[]` — every mandatory check with its detail
* `status.timings` — every stage boundary
* `status.conditions` — `Prepared`, `ApplicationQuiesced`, `RedisBarrierEstablished`,
  `RedisStateCaptured`, `VideoStateCaptured`, `Validated`, `Committed`, `ApplicationResumed`

## 7. Reproducing

```bash
scripts/ramp-scenario1/20-run-epoch.sh video-stream-rg default   # one epoch
scripts/ramp-scenario1/95-qgtp-experiment.sh                     # the Q > P gate
scripts/ramp-scenario1/96-negative-tests.sh                      # four abort paths
scripts/ramp-scenario1/97-regression-tests.sh                    # timer + restart safety
```
