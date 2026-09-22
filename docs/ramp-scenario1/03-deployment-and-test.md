# 03 — Deployment and Test

Everything below was run from `sre-control` as `ubuntu`. Controllers run
**out of cluster**, which is the established pattern in this lab: the audit
found the Transition Operator's CRDs and CRs present on mgmt with no Deployment
anywhere (audit §3.1).

## 0. Prerequisites

```bash
# kubeconfigs already present in $HOME
ls ~/{mgmt,workload01,workload02}.kubeconfig
# go toolchain
go version      # go1.26.5
```

MinIO credentials come from the management cluster and are never committed:

```bash
kubectl --kubeconfig ~/mgmt.kubeconfig get secret minio -n minio-system \
  -o jsonpath='{.data.rootUser}'     | base64 -d
kubectl --kubeconfig ~/mgmt.kubeconfig get secret minio -n minio-system \
  -o jsonpath='{.data.rootPassword}' | base64 -d
```

## 1. AppBundle Operator on the source cluster

```bash
kubectl --kubeconfig ~/workload01.kubeconfig apply -f ~/appbundle-operator/config/crd/bases/

cd ~/appbundle-operator && go build -o /tmp/appbundle-manager ./cmd/
KUBECONFIG=~/workload01.kubeconfig /tmp/appbundle-manager \
  --metrics-bind-address=0 --health-probe-bind-address=0 --leader-elect=false
```

The upstream repository is used unmodified.

## 2. The application

```bash
kubectl --kubeconfig ~/workload01.kubeconfig apply -f examples/ramp-scenario1/00-namespace.yaml
kubectl --kubeconfig ~/workload02.kubeconfig apply -f examples/ramp-scenario1/00-namespace.yaml

# source: AppBundle (video + redis primary, ordered data -> streaming)
kubectl --kubeconfig ~/workload01.kubeconfig apply -f examples/ramp-scenario1/20-appbundle-video-stream-service.yaml

# target: redis standby replicating over the shared transit network
kubectl --kubeconfig ~/workload02.kubeconfig apply -f examples/ramp-scenario1/25-redis-standby-workload02.yaml
```

Verify the AppBundle actually reached `Deployed` with `resourceRef`s — this is
what RecoveryGroup resolution depends on:

```bash
kubectl --kubeconfig ~/workload01.kubeconfig get appbundle video-stream-service -n ramp-demo \
  -o jsonpath='{.status.phase}{"\n"}{range .status.groupStatuses[*]}{.name}: {range .componentStatuses[*]}{.name}={.resourceRef.kind}/{.resourceRef.name} {end}{"\n"}{end}'
```

and that replication is up across clusters:

```bash
kubectl --kubeconfig ~/workload02.kubeconfig exec -n ramp-demo deploy/redis-standby -- \
  redis-cli INFO replication | grep -E 'role|master_link_status|slave_repl_offset'
```

## 3. checkpoint-agent on both clusters

Two **roles**, and the difference matters (see `01-integration-design.md` §2):

```bash
# SOURCE role - upstream manifest, unmodified (read-only checkpoint mount)
kubectl --kubeconfig ~/workload01.kubeconfig apply -f config/ramp/checkpoint-agent/checkpoint-agent-daemonset.yaml

# TARGET role - RAMP overlay, writable mount so staging can work
kubectl --kubeconfig ~/workload02.kubeconfig apply -f config/ramp/checkpoint-agent/checkpoint-agent-daemonset.yaml
kubectl --kubeconfig ~/workload02.kubeconfig apply -f config/ramp/checkpoint-agent/checkpoint-agent-target-role.yaml
```

Then patch the real MinIO credentials into `kube-system/minio-credentials` on
both clusters, and copy the secret into `ramp-demo` on the target (the staging
Job runs there).

## 4. RAMP

```bash
cd ~/ramp
make build          # or: go build -o bin/ramp-manager ./cmd/manager
kubectl --kubeconfig ~/mgmt.kubeconfig apply -f config/ramp/crd/

MINIO_ACCESS_KEY=… MINIO_SECRET_KEY=… KUBECONFIG=~/mgmt.kubeconfig \
  ./bin/ramp-manager \
    --cluster=workload01=$HOME/workload01.kubeconfig \
    --cluster=workload02=$HOME/workload02.kubeconfig \
    --artifact-store-endpoint=192.168.28.158:32000 \
    --health-probe-bind-address=:8082
```

```bash
kubectl --kubeconfig ~/mgmt.kubeconfig apply -f examples/ramp-scenario1/30-recoverygroup.yaml
kubectl --kubeconfig ~/mgmt.kubeconfig apply -f examples/ramp-scenario1/40-recoverypath.yaml
```

## 5. Run the scenario

```bash
./scripts/ramp-scenario1/00-run-scenario.sh
```

| Script | What it does |
|---|---|
| `10-reset.sh` | Return to the pre-failure steady state (idempotent, run before a repeat) |
| `20-run-epoch.sh` | Create the next `RecoveryPoint` and wait for a terminal phase |
| `30-prepare-path.sh` | **Path-directed staging**: pull only this path's artifact to the target. `RP_NAME` pins the epoch |
| `40-inject-failure.sh` | Scale the two RecoveryGroup members to zero on workload01 |
| `45-undo-failure.sh` | Scale them back |
| `50-recover.sh` | Promote the Redis standby, then attempt the container-checkpoint restore |
| `00-run-scenario.sh` | All of the above with timestamps, into `evidence/scenario1/run-<ts>/` |

## 6. Observing readiness

```bash
kubectl --kubeconfig ~/mgmt.kubeconfig get recoverygroup,recoverypoint,recoverypath -A

# why is the path HOT / WARM / COLD?
kubectl --kubeconfig ~/mgmt.kubeconfig get recoverypath video-workload01-to-workload02 -o yaml
```

The `status.checks[]` list carries a `reason` and `message` per prerequisite and
`status.unmetMandatoryChecks` names exactly what is missing, so the YAML alone
explains the level without reading controller logs.

## 7. Two traps worth knowing

Both were found by running this, not by reading code:

* **Do not edit a shell script while it is executing.** Bash reads scripts
  incrementally by byte offset; rewriting `00-run-scenario.sh` mid-run produced
  a bogus `syntax error near unexpected token` at a line that was perfectly
  valid. Re-run from a copy or after the run finishes.
* **`recoverygroup.status.latestRecoveryPoint` lags by up to one resync (30s).**
  A script that has just created an epoch must use the name it created, not that
  field, or it will stage the previous epoch's artifact and never reach HOT.
  `20-run-epoch.sh` prints the object it made; `30-prepare-path.sh` takes
  `RP_NAME`.
