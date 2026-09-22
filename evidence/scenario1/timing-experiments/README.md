# Recovery timing: three variants, measured to APPLICATION ready

All three end with the same nine application-level checks passing
(`scripts/ramp-scenario1/70-verify-app-health.sh`). "Application ready" means
those checks pass -- the restored instance is serving, is the instance that was
checkpointed, is advancing, and agrees with Redis. Pod readiness is reported
separately because it is not the same thing and is systematically optimistic.

| variant | what is different | RTO to pod ready | **RTO to APPLICATION ready** |
|---|---|---|---|
| `MODE=poll` | ArgoCD left to its own reconciliation interval | 287.6 s | **291.1 s** |
| `MODE=baseline` | sync triggered explicitly; nothing prepared on the target | 6.9 s | **8.5 s** |
| `MODE=optimized` | sync triggered; ArgoCD wiring, workload object, Service, artifact and image all pre-staged | 6.2 s | **7.8 s** |

## Where the time actually goes (optimized run)

```
redis promotion                    0.5s
git commit (activate)              1.3s
argocd sync + apply                0.7s
pod ready (restore)                3.7s
pod ready -> APPLICATION ready     1.6s   (1.0s of which is the probe's own dwell)
------------------------------------------
RTO to APPLICATION ready           7.8s
```

## Conclusions

1. **The whole optimization is the explicit sync trigger.** 291 s -> 8.5 s, a
   34x reduction, and it is not a tuning knob: it is the difference between
   waiting for ArgoCD's reconciliation interval and telling it to sync now. The
   Transition Operator already does this in
   `helpers.TriggerArgoCDSyncWithKubeClient`; any RAMP driver must too.
2. **Pre-staging buys about 0.7 s on top of that**, almost all of it from
   writing one file instead of two at failure time. The ArgoCD machinery itself
   is not the cost. Pre-staging still matters for a different reason: it is what
   makes the checkpoint image and the artifact present on the target at all,
   without which there is nothing to restore.
3. **ArgoCD sync is ~0.5 s**, not a bottleneck.

## A correction worth keeping

`00-WRONG-measurement-61s-artifact.log` records three consecutive runs that
reported a flat "argocd sync + apply = 61s". That number was **an artifact of the
measurement harness**, not a property of ArgoCD: the wait loop polled an ArgoCD
Application name that no longer existed after the DR-repo redesign, so the
jsonpath returned empty and the loop simply ran to exhaustion --
120 iterations x ~0.5 s. ArgoCD had in fact synced the correct commit SHA one
second after the push, which the Application's `status.operationState` showed
all along.

Two lessons, both now encoded in the harness: a wait loop must fail loudly when
the object it polls is missing, and a suspiciously round, highly repeatable
constant is more likely to be your own timeout than the system's behaviour.
