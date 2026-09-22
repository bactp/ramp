# 00 — Existing System Audit (evidence-based)

Audit host: `sre-control` (192.168.28.158). All statements below are backed by
commands run against the live clusters / source tree on 2026-09-21. Raw captures
are in [`evidence/audit/`](../../evidence/audit/).

---

## 1. Cluster topology

| kubeconfig | context | apiserver | nodes | role for RAMP |
|---|---|---|---|---|
| `~/mgmt.kubeconfig` | `kubernetes-admin@kubernetes` | in-cluster (192.168.28.158) | sre-control, sre-worker01, sre-worker02 (v1.28.9) | **management plane** |
| `~/workload01.kubeconfig` | `workload01-admin@workload01` | https://192.168.28.238:6443 | cp `…-d4bn5`, worker `…-gnqcb` (v1.32.8) | **source cluster** |
| `~/workload02.kubeconfig` | `workload02-admin@workload02` | https://192.168.28.122:6443 | cp `…-q5vlg`, worker `…-pjkrh` (v1.32.8) | **recovery target** |
| `~/workload03.kubeconfig` | `workload03-admin@workload03` | — | cp + worker (v1.32.8) | spare |
| `~/sre-test1.kubeconfig`, `~/sre-test2.kubeconfig` | — | — | 1cp+2w each | SREGym, unrelated |

All six clusters are reachable and `Ready`.

### 1.1 Network topology — the single most important finding

The workload clusters' **node network `10.6.0.0/24` is NOT routable** from the
management node, and **not routable between workload clusters** either:

```
sre-control -> 10.6.0.160  : ping FAIL, :10250 closed
wl02 pod    -> 10.6.0.160  : ping FAIL, :22 FAIL
```

What *is* routable is the shared transit network `192.168.28.0/24`. Every CAPI
`Machine` carries a floating IP there (`kubectl get machines -A -o wide` on mgmt):

| cluster | node | InternalIP | Floating IP |
|---|---|---|---|
| workload01 | `workload01-control-plane-d4bn5` | 10.6.0.196 | **192.168.28.238** |
| workload01 | `workload01-md-0-4qfvd-gnqcb` (worker) | 10.6.0.160 | **192.168.28.110** |
| workload02 | `workload02-control-plane-q5vlg` | 10.6.0.129 | **192.168.28.122** |
| workload02 | `workload02-md-0-rx5mn-pjkrh` (worker) | 10.6.0.127 | **192.168.28.129** |

Verified reachability from a pod **inside workload02**:

```
192.168.28.158:32000 OK   (MinIO on mgmt)
192.168.28.158:32100 OK   (Gitea on mgmt)
192.168.28.238:6443  OK   (workload01 apiserver)
192.168.28.238:30080 OK   (workload01 NodePort via floating IP)
```

and from `sre-control`:

```
192.168.28.110:10250 OPEN   -> https://192.168.28.110:10250/healthz = HTTP 401 (reachable, needs auth)
```

**Consequences for RAMP**
1. Cross-cluster application traffic (Redis replication) **must** use a NodePort
   published on a `192.168.28.x` floating IP. Internal `10.6.0.x` and pod CIDRs
   are useless across clusters.
2. The kubelet *is* reachable — but only via the floating IP, which is **not**
   present on the Kubernetes `Node` object (see §3.2).

---

## 2. RAMP-related CRDs actually installed

```
kubectl get crd -o name | egrep -i 'clusterpolicy|transition|checkpoint|recovery|appbundle|ramp'
```

| cluster | result |
|---|---|
| **mgmt** | `checkpoints.transition.dcnlab.ssu.ac.kr`, `clusterpolicies.transition.dcnlab.ssu.ac.kr`, `nodehealths.transition.dcnlab.ssu.ac.kr`, `packagepolicies.transition.dcnlab.ssu.ac.kr` |
| workload01 / 02 / 03 | **none** |

There are **no `Recovery*` and no `AppBundle` CRDs anywhere**. RAMP is greenfield
at the API level; everything it integrates with already exists only on mgmt.

### 2.1 Live objects

* `clusterpolicy`: **`No resources found`** — the ClusterPolicy control loop is
  currently dormant. Nothing in the lab is being driven by it today.
* `packagepolicy`: none.
* `nodehealth`: 3 objects (`10.0.0.125`, `workload01-control-plane-d4bn5`,
  `workload01-md-0-4qfvd-gnqcb`) — heartbeat records, `status.condition: Healthy`,
  cpus/memTotal/ip/arch/os. Note `10.0.0.125` is a stale record from an older
  network layout, which corroborates §1.1 (the node network was renumbered).
* `checkpoint`: **1 object**, and it is the Rosetta stone for the whole pipeline:

```yaml
# checkpoint-clusterpolicy-video-77b57c57bc-cqnzv-video   (created 2026-08-12)
spec:
  schedule: '*/2 * * * *'
  clusterRef:       {name: workload01, repository: http://192.168.28.158:32100/nephio/workload01.git}
  targetClusterRef: {name: workload02}
  podRef:  {namespace: default, name: video-77b57c57bc-cqnzv,
            containerRef.containers[0]: {name: vlc, image: tuongvx/vlc-app:latest, port 8080}}
  resourceRef: {kind: Deployment, name: video, namespace: default,
                annotations: {transition.dcnlab.ssu.ac.kr/cluster-policy: "true",
                              transition.dcnlab.ssu.ac.kr/packageName: video}}
  registry: {url: docker.io, repository: phuongbac, secretRef: {name: reg-credentials, namespace: default}}
status:
  phase: Completed
  originalImage:       tuongvx/vlc-app:latest
  lastCheckpointImage: docker.io/phuongbac/checkpoint-default_video-77b57c57bc-cqnzv-vlc:latest
  lastCheckpointTime:  "2026-08-12T15:30:26Z"
```

This proves the intended Scenario-1 topology (**workload01 → workload02**, video
workload, OCI checkpoint artifacts) already existed and once worked. It has not
run since 2026-08-12, consistent with the network renumbering in §1.1.

---

## 3. The Transition Operator

Source: `~/transition-operator` (branch `main`, clean tree, HEAD `f661563`).
Go module `github.com/vitu1234/transition-operator`; remote is
`github.com/bactp/transition-operator-ai-assisted`. `~/transition-operator-ai-assisted`
is the **same repo one commit behind** (HEAD `f8a2b62`); `~/transition-operator`
is the canonical, newer checkout (it alone carries `deploy/`, `mcp/`, `test-scripts/`).
All audited repos are clean — no uncommitted user work was touched.

### 3.1 Runtime status — **the operator is not running**

`kubectl get deploy -A` on mgmt lists `kagent`, `context-manager`, `failover-agent`,
`failover-agent-mcp` … but **no transition-operator Deployment**, and no
`checkpoint-agent` DaemonSet on workload01 or workload02. The CRDs and CRs exist,
the controller does not. The established lab pattern is therefore an
**out-of-cluster controller run** from `sre-control`.

### 3.2 Checkpoint pipeline as implemented

`internal/controller/checkpoint_controller.go` (1209 LOC):

```
CheckpointReconciler (mgmt)
  └─ resolve pod -> node, build kubelet URL  https://<NodeInternalIP>:10250
  └─ POST /checkpoint/{ns}/{pod}/{container}          [kubelet Container Checkpoint API]
       -> kubelet writes  /var/lib/kubelet/checkpoints/checkpoint-<pod>_<ns>-<ctr>-<ts>.tar
  └─ miniohelper.WaitForFileInMinio(bucket=checkpoints, 300s)
  └─ miniohelper.DownloadFileFromMinio
  └─ buildah from scratch / add <tar> / config / commit / push   -> docker.io/phuongbac/checkpoint-…
  └─ status.lastCheckpointImage
```

The node-local half is the **checkpoint-agent** (`~/checkpoint-agent`, single file
`agent-og/main.go`, deployed by `~/transition-operator/deploy/checkpoint-agent-daemonset.yaml`):

* `fsnotify` watch on `/var/lib/kubelet/checkpoints` → `FPutObject` to MinIO bucket `checkpoints`
* periodic `syncFromMinio` (every `PULL_INTERVAL`, default 30s) → **downloads every
  object in the bucket to the local node** — i.e. it *stages* checkpoint artifacts
  on whatever node it runs on. This is directly reusable as RAMP's WARM→HOT staging primitive.
* heartbeat POST to `CONTROLLER_URL` → the operator's `heartbeat.StartServer` → `NodeHealth` CRs
* `CreateRestorePod()` (unused by the operator today) builds a privileged pod running
  `runc restore --image-path /checkpoints/<file> --id <id>` against the node hostPath.

MinIO: `minio-system/minio` NodePort **32000** on mgmt (`/minio/health/live` = 200).
Gitea: `gitea/gitea` NodePort **32100**. Registry credentials: `Secret default/reg-credentials`
on mgmt (keys `registry`, `username`, `password`).

### 3.3 Two defects found in the checkpoint path

Both are in `checkpoint_controller.go` around L197-213:

```go
for _, addr := range node.Status.Addresses {
    if addr.Type == corev1.NodeInternalIP {
        nodeKubeletIP := addr.Address        // (a) SHADOWS the outer variable
        nodeKubeletURL = fmt.Sprintf(...)
        NewKubeletClient(ctx, nodeKubeletIP, workloadClusterClient)  // result discarded
    }
}
...
r.initializeClients(ctx, &Checkpoint, nodeKubeletIP, workloadClusterClient)  // always ""
```

* **(a) Variable shadowing.** The inner `:=` creates a new binding, so the outer
  `nodeKubeletIP` stays `""`. `NewKubeletClient` then falls back to its
  `https://localhost:10250` default. The kubelet call cannot reach the right node.
* **(b) `NodeInternalIP` is the wrong address in this lab.** Verified:
  `kubectl get node workload01-md-0-4qfvd-gnqcb -o jsonpath='{.status.addresses}'`
  returns **only** `InternalIP=10.6.0.160` and `Hostname=…` — there is **no
  `ExternalIP` on the Node object at all**. The reachable floating IP
  (192.168.28.110) exists only on the mgmt-side CAPI `Machine`. So even after
  fixing (a), a `NodeExternalIP` preference would not help without a CAPI join.

These are **recorded, not patched** — see `01-integration-design.md` §6 for why
RAMP routes around them instead of modifying the Transition Operator.

### 3.4 Verified: the kubelet Container Checkpoint API works here

`ContainerCheckpoint` is explicitly enabled on the workload kubelets:

```
$ kubectl --kubeconfig workload01.kubeconfig get --raw \
    "/api/v1/nodes/workload01-md-0-4qfvd-gnqcb/proxy/configz" | jq .kubeletconfig.featureGates
{"ContainerCheckpoint": true}
```

and a **live checkpoint succeeded** via the workload apiserver's node proxy:

```
$ curl -X POST --cert/--key (workload01 admin certs) \
    https://192.168.28.238:6443/api/v1/nodes/workload01-md-0-4qfvd-gnqcb/proxy/checkpoint/default/video-768db8fb67-xktb4/vlc
{"items":["/var/lib/kubelet/checkpoints/checkpoint-video-768db8fb67-xktb4_default-vlc-2026-09-21T09:03:10Z.tar"]}
real 0m2.857s
```

**The apiserver node-proxy is a working transport to the exact same kubelet
endpoint the Transition Operator uses, and it is immune to the §1.1 routing
problem.** This is the integration point RAMP builds on.

### 3.5 ClusterPolicy semantics

`api/v1/clusterpolicy_types.go` + `internal/controller/clusterpolicy_controller.go` (1605 LOC):

* **spec** — `clusterSelector{name,repo,repoType,provider}` (source), `packageSelectors[]`
  (`name`, `packagePath`, `packageType: Stateful|Stateless`, `liveStatePackage`,
  `backupInformation[]{name,backupType: Schedule|Manual, schedulePeriod}`),
  `targetClusterPolicy{preferClusters[],avoidClusters[]}` with `weight`.
* **status.phase** — the AI-assisted gate: `Recommended → AwaitingApproval →
  Approved → Executing → Completed` (or `Rejected`). `status.recommendation`
  (`recommendedTargetCluster`, `reason`, `recommendedBy`, `faultType`, `approvedBy`)
  is written by the **failover-agent-mcp** `propose_failover` tool
  (`~/transition-operator/mcp/failover-agent-mcp/internal/tools/propose_failover.go`);
  `kagent/failover-agent` + `failover-agent-mcp` are running on mgmt.
  `Approved` is the human gate that unblocks `canAutoTransition()`.
* **trigger** — `StartWorkloadClusterControlPlaneHealthMonitor` and
  `StartWorkloadClusterCNIHealthMonitor` detect faults, `notifyAgentOfFaultOnce`
  asks the agent, and after approval `TransitionSelectedLiveWorkloads` runs.
* **target selection already exists**: `helpers.DetermineTargetRepo(clusterPolicy)`
  picks from `preferClusters` by weight. It is *policy* selection — it has no
  notion of whether the target is actually **prepared**. That gap is precisely
  what RAMP `RecoveryPath` readiness fills.
* **actuation is GitOps, not imperative**: for a `Stateful` package it looks up the
  `Checkpoint` CR, rewrites the container image in the source Gitea repo manifests
  from `status.originalImage` to `status.lastCheckpointImage`, pushes to the `dr`
  repo, then `TriggerArgoCDSyncWithKubeClient(target, "<target>-dr", "argocd")`.
  ArgoCD is installed and running on **both** workload01 and workload02.

---

## 4. AppBundle Operator

Source: `~/appbundle-operator` (already cloned, branch `main`, clean, HEAD `472eccc`,
remote `github.com/lehuannhatrang/appbundle-operator`). **Not installed on any cluster.**

Answering the audit questions from the brief:

1. **Groups / Components** — `spec.groups[]{name, order, components[]{name, order,
   template (RawExtension), porchPackageRef}}`. A component is either an inline
   Kubernetes resource template *or* a Porch package reference.
2. **`spec.dependencies`** — `[]{from, to, type: strict|timeout, timeoutSeconds}`,
   enforced by `checkDependencies()` against the per-group phase map; `strict`
   blocks the `to` group until `from` reaches `Deployed`.
3. **Runtime ResourceRefs** — `status.groupStatuses[].componentStatuses[].resourceRef`
   = `{apiVersion, kind, name, namespace}` of the resource actually created.
4. **Tracking labels injected** (`appbundle_controller.go` L311-313, L681-683, L728-730):
   `app.example.com/appbundle`, `app.example.com/group`, `app.example.com/component`.
5. **Porch discovery** — `reconcileComponentWithPorch` creates a `PackageVariant`,
   `waitForPackageVariantReady`, then `discoverPackageVariantResources` enumerates
   what Porch rendered; ArgoCD sync-waves are assigned from group/component order.
6. **Readiness** — `waitForResourceReady` dispatches to `isDeploymentReady`,
   `isStatefulSetReady`, `isDaemonSetReady`, `isJobComplete`, `isPodReady`;
   `isInfrastructureResource(kind)` classifies Service/ConfigMap/Secret/Namespace
   as infrastructure that needs no readiness wait.
7. **Cluster state** — `kubectl get crd | grep -i appbundle` → nothing; no
   `appbundle` objects; no operator pods. It must be installed by this work.
8. **Can Video + Redis be expressed without modifying the operator?** **Yes.**
   `config/samples/app_v1alpha1_appbundle_microservices.yaml` shows plain inline
   templates with ordered groups and no Porch, which is exactly the shape needed.

### 4.1 Critical implementation question — answered

> Can the existing AppBundle CR + tracking labels + status ResourceRefs provide
> enough application identity and resource discovery for a RecoveryGroup Controller?

**Yes, with no extension to AppBundle.** `status…componentStatuses[].resourceRef`
gives `{group, component} → {apiVersion, kind, namespace, name}`, and the injected
`app.example.com/{appbundle,group,component}` labels are propagated to the created
workload objects, so the controller can go
`component → workload object → selector → Pods → container` without any new
labelling scheme. RAMP therefore references AppBundle components and **copies no
templates** into `RecoveryGroup`.

---

## 5. Nephio / Porch health

`nephio-system/nephio-controller` shows **RESTARTS 8156** over 68d. Its logs show
it is not crash-looping on error but continuously re-reconciling
`BootstrapPackageController` / `BootstrapSecretController`, repeatedly retrying
`namespace: config-management-system, does not exist, retry...` for
workload02/workload03 config-sync secrets. Porch itself (`porch-server`,
`porch-controllers`, `function-runner`) is stable at 2 restarts.

**Decision: Nephio/Porch is excluded from the Scenario-1 critical path.** The
AppBundle for Scenario 1 uses inline templates (`porchIntegration` disabled), so
a Nephio outage cannot fail the experiment. Porch/PackageVariant remains available
as the later production deployment path — AppBundle supports both shapes natively.

---

## 6. Summary of what exists vs. what is missing

| Capability | State | Evidence |
|---|---|---|
| kubelet Container Checkpoint API | **works** (2.8 s) via apiserver node proxy | §3.4 |
| Direct kubelet `:10250` from mgmt | **broken** (node net unroutable) | §1.1, §3.3 |
| checkpoint-agent (node→MinIO, MinIO→node staging) | code exists, **not deployed** | §3.2 |
| MinIO artifact store | **works**, NodePort 32000 | §3.2 |
| OCI registry + creds | secret present, `buildah` **not installed** on sre-control | §3.2 |
| Transition Operator controller | CRDs installed, **controller not running** | §3.1 |
| ClusterPolicy objects | **none** — loop dormant | §2.1 |
| ClusterPolicy → ArgoCD `dr` restore path | implemented, ArgoCD present both clusters | §3.5 |
| AppBundle operator | source present, **not installed** | §4 |
| Video workload | running on workload01 **and** workload02 (`default`, 41d) | §2.1 |
| Redis | **does not exist anywhere** | — |
| Recovery* APIs | **do not exist** | §2 |

### Work implied for Scenario 1

1. Install the AppBundle CRD + run its controller against **workload01**.
2. Deploy the Video + Redis application as an AppBundle in a **new `ramp-demo`
   namespace** (the existing `default/video` deployments are left untouched).
3. Deploy Redis standby on **workload02**, replicating over a `192.168.28.x`
   floating-IP NodePort (§1.1).
4. Deploy the existing checkpoint-agent DaemonSet to workload01 (capture/upload)
   **and** workload02 (MinIO→node staging = the WARM→HOT transition).
5. Add the three RAMP APIs and two controllers (`00` has no equivalent today).
