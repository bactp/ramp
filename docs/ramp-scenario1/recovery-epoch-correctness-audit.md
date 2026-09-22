# RecoveryEpoch correctness audit (pre-fix)

Audit of the implementation as of commit `25b6ea0`, before the correctness
closure work. Everything below was read out of the source tree and the running
testbed (workload01 source, workload02 target, out-of-cluster `ramp-manager`).

## 0. What one epoch did

`internal/controller/recoverypoint_controller.go:Reconcile` → `runEpoch`.
One reconcile invocation ran the whole workflow start to finish:

```
PREPARE      resolve AppBundle components               (status update)
QUIESCE      poll RedisReplication.Capture until
             connected_replicas > 0                      (status update)
CAPTURE      read /tmp/ramp-video-state.json via exec
             kubelet checkpoint via node proxy
             wait for the tar to land in MinIO
             re-read Redis INFO + consistency key         (status update)
VALIDATE     |ckptPosition - replPosition| <= maxPositionSkew
COMMIT       phase=Committed
```

## 1. Where the barrier began

`recoverypoint_controller.go:112-153`. The loop condition was

```go
capState.ConnectedReplicas > 0 && capState.LogicalPosition >= 0
```

so the "barrier" was satisfied the first time the primary reported *any*
attached replica and the key `ramp:video:position` existed. It did not depend
on the epoch at all: the same condition is true continuously, before and after
any epoch, so no barrier was actually established.

## 2. How the Video logical position was read

`readMemberState` → `clusters.Exec` → `cat /tmp/ramp-video-state.json` inside the
video container. That file is rewritten by the application on **every tick**
(1 Hz), so the value read is a sample of a moving quantity.

## 3. Was Video stopped or quiesced?

**No.** There was no quiesce mechanism anywhere — not in the controller, not in
the application (`examples/ramp-scenario1/20-appbundle-video-stream-service.yaml`
has a single `while True:` loop with no control input). The controller's own
comment stated the design explicitly: *"The barrier does not stop the
application."* Consequences:

* the position read at the start of CAPTURE is already stale by the time the
  kubelet checkpoint is taken (checkpoint takes ~2-10 s; the app ticks at 1 Hz);
* Redis was re-read *after* the checkpoint, so the two positions were sampled
  seconds apart;
* validation therefore could not be exact — which is why `maxPositionSkew`
  (default 2) exists. The tolerance is a symptom of the missing quiesce, not a
  design requirement.

## 4. How Redis replication ACK was verified

It was not. `drivers/redisreplication.go:Capture` parses `INFO replication` and
reports `connected_slaves`. `connected_slaves > 0` says a replica socket is
attached; it says nothing about whether the replica has received, let alone
acknowledged, the bytes that carry position P. No `WAIT`, no offset comparison
against a marker written for this epoch.

## 5. What Redis state was stored in the RecoveryPoint

`RecoveryArtifact{Type: replicationState, Ref: "redis-repl://<host>:<port>/<offset>"}`
plus `replicationOffset`, `replicationId`, `logicalPosition`.

That is **a pointer to a live server**, not a recovery artifact. Concretely,
`redis-repl://192.168.28.238:30379/838626` names the primary and an offset in a
stream that keeps advancing.

## 6. Was that state immutable?

**No — this is the core defect.** The replica keeps applying the stream after
commit. Five minutes after epoch E commits at P=100, the replica holds Q=400.
The RecoveryPoint's `logicalPosition: 100` is metadata that no longer describes
anything reachable: nothing in the system retains the Redis contents at P.

## 7. How Redis was restored

`scripts/ramp-scenario1/50-recover.sh`: `REPLICAOF NO ONE` on the standby, then
read back `GET ramp:video:position`. This *promotes whatever the replica happens
to hold*, i.e. Q. The script then printed that position as `T_redis_ready
position=$REDIS_POS` without ever comparing it to the RecoveryPoint's P.

## 8. When Video was resumed

Never, because it was never paused. After a failed epoch nothing needed
cleaning up — but equally, nothing existed to guarantee cleanup once a quiesce
was introduced.

## 9. What happened if capture failed halfway

`r.fail()` set `phase=Failed` and returned. Because terminal phases are
short-circuited at the top of `Reconcile`, the epoch was never retried, which is
correct. But:

* a half-finished epoch left no record of *which* stage completed;
* if the manager process died mid-epoch, the RecoveryPoint stayed in
  `Quiescing`/`Capturing` forever, and the next reconcile after restart would
  re-run the entire workflow from PREPARE against a mutated application —
  silently producing a RecoveryPoint whose artifacts came from different points
  in time.

## 10. Application timer artifact

```python
next_tick = time.time() + 1.0
while True:
    ...
    if time.time() >= next_tick:
        next_tick += 1.0      # <-- absolute schedule
```

`next_tick += 1.0` keeps the *absolute* schedule. A process frozen by CRIU for
N seconds executes N catch-up ticks back-to-back on restore. The
`restore-verified/README.md` evidence shows exactly this: the restored process
"resumed 165 -> 215" within seconds. Any RPO or recovery-latency number measured
against that counter was measuring the catch-up, not the application.

## 11. Net correctness statement (pre-fix)

A committed RecoveryPoint of this prototype guaranteed:

* a container checkpoint artifact exists in MinIO, taken at approximately P;
* Redis had, at some moment near P, a replica attached.

It did **not** guarantee that a Redis state corresponding to P is retrievable
after the source advances. Recovery from RP-E therefore produced
`Video ≈ P, Redis = Q`, and because the restored Video reconnects to Redis and
writes its own position, Redis was then silently rolled back from Q to P+1 —
after which every end-state health check (`video position == redis position`)
passes. The failure mode was invisible to every check the prototype had.
