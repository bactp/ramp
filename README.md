# RAMP — Readiness-Aware Management Plane for Workload Recovery

A research prototype that treats **recovery readiness as an elastic resource**
rather than something produced by fixed periodic preparation of every workload.

It adds three APIs and three controllers to the existing DCN Kubernetes
multi-cluster testbed, reusing the Transition Operator, checkpoint-agent,
AppBundle Operator and MinIO artifact store **without modifying any of them**.

```
RecoveryGroup   the minimum recovery-consistency domain
RecoveryPoint   one Recovery Epoch: PREPARE -> BARRIER -> CAPTURE -> VALIDATE -> COMMIT
RecoveryPath    an executable recovery plan + the current readiness of its prerequisites (HOT/WARM/COLD)
```

## The claim Scenario 1 tests

Not "checkpoint N pods, restore N pods". The claim is that one recovery-
consistency domain can hold members recovered by **different mechanisms**, and
that their joint usability is a checkable property:

* `video` recovers by **container/runtime checkpoint** — its decode state lives
  in process memory and no replication mechanism can reproduce it.
* `redis` recovers by **Redis-native replication** — it is deliberately *not*
  checkpointed just because the video member is.
* RAMP coordinates them as one RecoveryGroup and refuses to commit a
  RecoveryPoint whose artifacts cannot be shown to refer to the same
  application position.

## Layout

```
api/v1alpha1/            RecoveryGroup, RecoveryPoint, RecoveryPath
internal/controller/     three reconcilers (never one monolith)
internal/drivers/        container-checkpoint and redis-replication
internal/clusters/       multi-cluster registry + apiserver exec
internal/artifacts/      read-only view of the MinIO checkpoint store
internal/rampredis/      minimal RESP client
config/ramp/             CRDs, checkpoint-agent target-role overlay, restore scripts
examples/ramp-scenario1/ AppBundle, RecoveryGroup, RecoveryPath, standby
scripts/ramp-scenario1/  reset / epoch / prepare / inject / recover / run-all
docs/ramp-scenario1/     audit, design, scenario, deployment, results
evidence/                captured runtime evidence
```

## Docs

| | |
|---|---|
| [00 — Existing system audit](docs/ramp-scenario1/00-existing-system-audit.md) | What is actually deployed, and the network facts that shaped everything |
| [01 — Integration design](docs/ramp-scenario1/01-integration-design.md) | Architecture, responsibility table, boundary decisions |
| [02 — Scenario 1](docs/ramp-scenario1/02-scenario1-video-redis.md) | Video + Redis, the epoch, the path |
| [03 — Deployment and test](docs/ramp-scenario1/03-deployment-and-test.md) | How to run it |
| [04 — Results](docs/ramp-scenario1/04-results.md) | What worked, what failed, timings |
| [05 — GitOps recovery driver](docs/ramp-scenario1/05-gitops-recovery-driver.md) | The verified actuation chain and the RecoveryDriver interface it implies |
| [06 — Application health and timing](docs/ramp-scenario1/06-application-health-and-timing.md) | The nine-check health contract, RTO to *application* ready, and what actually optimises ArgoCD |

## Build

```bash
make build      # go build -o bin/ramp-manager ./cmd/manager
make manifests  # regenerate CRDs + deepcopy with controller-gen
```
