package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Readiness is the elastic recovery-readiness level of a RecoveryPath.
//
// The levels are defined against the RecoveryContract, not against "how many
// boxes are ticked":
//
//	HOT  : an EXECUTABLE PREPARED path exists and currently satisfies both the
//	       RPO (the prepared RecoveryPoint is fresh enough) and the RTO (the
//	       remaining failure-time work fits the budget). Failure-time work is
//	       activation only.
//	WARM : recovery is feasible, but either preparation work remains or the
//	       contract cannot currently be guaranteed (e.g. the prepared point has
//	       gone stale while its successor is still being prepared).
//	COLD : no currently executable or sufficiently feasible recovery path.
type Readiness string

const (
	ReadinessHot  Readiness = "HOT"
	ReadinessWarm Readiness = "WARM"
	ReadinessCold Readiness = "COLD"
)

// Check names are stable identifiers so operators can reason about them.
//
// The names are grouped by what they are about, because that grouping is what
// makes a degraded path diagnosable:
//
//	target-side, RecoveryPoint-independent  TargetClusterReachable,
//	                                        TargetNamespaceReady,
//	                                        TargetPlacementFeasible,
//	                                        RestoreCapabilityAvailable,
//	                                        RedisTargetRuntimeReady
//	about the PREPARED RecoveryPoint        RecoveryPointCommitted,
//	                                        RedisEpochArtifactAvailable,
//	                                        VideoCheckpointAvailable,
//	                                        RestoreArtifactReady,
//	                                        ActivationPlanPrepared
//	about the CONTRACT                      RecoveryPointFreshEnough,
//	                                        EstimatedRTOWithinContract
const (
	CheckTargetClusterReachable     = "TargetClusterReachable"
	CheckTargetNamespaceReady       = "TargetNamespaceReady"
	CheckRestoreCapabilityAvailable = "RestoreCapabilityAvailable"

	// CheckTargetPlacementFeasible replaces the old TargetResourceReady. That
	// check took the maximum CPU across nodes and the maximum memory across
	// nodes and compared each independently -- so it could pass with the CPU
	// coming from one node and the memory from another, describing a machine
	// that does not exist. This one names ONE concrete node that satisfies
	// every node-level prerequisite simultaneously.
	CheckTargetPlacementFeasible = "TargetPlacementFeasible"

	// CheckRedisTargetRuntimeReady replaces RedisStandbyReady. The rename is
	// the point: since RecoveryPoints carry an immutable epoch RDB, the replica
	// is no longer the thing that gets promoted. What it contributes to
	// readiness now is that the target Redis RUNTIME is already up, reachable
	// and carrying a warm dataset, so recovery is "load the epoch RDB and
	// restart" rather than "deploy Redis, then load". It is a preparation
	// mechanism, never the recovery artifact.
	CheckRedisTargetRuntimeReady = "RedisTargetRuntimeReady"

	CheckRecoveryPointCommitted      = "RecoveryPointCommitted"
	CheckRedisEpochArtifactAvailable = "RedisEpochArtifactAvailable"
	CheckVideoCheckpointAvailable    = "VideoCheckpointAvailable"

	// CheckRestoreArtifactReady is what the RESTORE actually consumes, on the
	// node it will actually run on. For Scenario 1 that is the checkpoint tar
	// staged on the placement node AND the CRI checkpoint image built from it
	// present in that node's containerd. The old VideoCheckpointStaged check
	// proved only that the tar existed on *some* worker, which is neither the
	// artifact the restore uses nor necessarily the node it runs on.
	CheckRestoreArtifactReady = "RestoreArtifactReady"

	// CheckActivationPlanPrepared is the GitOps half: the target workload
	// object, its Service and the ArgoCD wiring already exist, scaled to zero,
	// pinned to the placement node, and annotated with the RecoveryPoint they
	// were prepared for. Without it, failure-time work includes writing and
	// converging manifests, which is not activation.
	CheckActivationPlanPrepared = "ActivationPlanPrepared"

	CheckRecoveryPointFreshEnough   = "RecoveryPointFreshEnough"
	CheckEstimatedRTOWithinContract = "EstimatedRTOWithinContract"
)

// Annotation keys the preparation steps stamp on the target workload object so
// that "this target is prepared" is a claim about ONE RecoveryPoint, verifiable
// by the controller, rather than "whatever was latest when a script ran".
const (
	AnnPreparedRecoveryPoint = "ramp.dcn.ssu.ac.kr/prepared-recovery-point"
	AnnPreparedEpoch         = "ramp.dcn.ssu.ac.kr/prepared-epoch"
	AnnCheckpointImage       = "ramp.dcn.ssu.ac.kr/checkpoint-image"
	AnnTargetNode            = "ramp.dcn.ssu.ac.kr/target-node"
	AnnActivationState       = "ramp.dcn.ssu.ac.kr/state"
)

// WorkloadReference names the target-side workload object that carries the
// activation plan.
type WorkloadReference struct {
	// +required
	Namespace string `json:"namespace"`
	// +required
	Name string `json:"name"`
}

// TargetPrerequisites are the target-side facts the readiness evaluation needs.
// These are deliberately explicit rather than discovered: Scenario 1
// implements deterministic evaluation of ONE manually specified path, not an
// optimizer that searches for paths.
type TargetPrerequisites struct {
	// Namespace the recovered RecoveryGroup will land in on the target.
	// +required
	Namespace string `json:"namespace"`

	// Node pins placement to one target node. Empty means RAMP picks the first
	// feasible node in name order (and keeps the currently prepared node while
	// it stays feasible, so placement does not flap).
	// +optional
	Node string `json:"node,omitempty"`

	// StandbyEndpoint is the target-side Redis runtime. It is the instance the
	// epoch RDB is restored INTO; it is not itself the recovery point.
	// +optional
	StandbyEndpoint *RedisEndpoint `json:"standbyEndpoint,omitempty"`

	// MaxReplicationLagBytes is the largest tolerated lag, in replication-stream
	// bytes, for the target Redis runtime to count as warm.
	// +optional
	// +kubebuilder:default=4096
	MaxReplicationLagBytes int64 `json:"maxReplicationLagBytes,omitempty"`

	// CheckpointStagingPath is the on-node directory the checkpoint-agent
	// syncs artifacts into on the target cluster.
	// +optional
	// +kubebuilder:default="/var/lib/kubelet/checkpoints"
	CheckpointStagingPath string `json:"checkpointStagingPath,omitempty"`

	// RestoreCapabilityDaemonSet is the DaemonSet whose readiness ON THE
	// PLACEMENT NODE proves that node can stage and restore checkpoints.
	// +optional
	RestoreCapabilityDaemonSet *NamespacedName `json:"restoreCapabilityDaemonSet,omitempty"`

	// ActivationPlanWorkload is the target workload object that preparation
	// creates ahead of the failure and that activation merely scales up and
	// re-images. Its annotations bind the prepared target to one RecoveryPoint.
	// +optional
	ActivationPlanWorkload *WorkloadReference `json:"activationPlanWorkload,omitempty"`

	// MinAllocatable is the minimum capacity a target node must have free.
	// +optional
	MinAllocatableCPUMillis int64 `json:"minAllocatableCpuMillis,omitempty"`
	// +optional
	MinAllocatableMemoryMiB int64 `json:"minAllocatableMemoryMiB,omitempty"`
}

// NamespacedName is a plain object reference.
type NamespacedName struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// RecoveryPathSpec is an executable recovery plan plus its prerequisites.
// A path is NOT merely a target cluster.
type RecoveryPathSpec struct {
	// RecoveryGroupRef is the group this path can recover.
	// +required
	RecoveryGroupRef string `json:"recoveryGroupRef"`

	// SourceCluster / TargetCluster are registered cluster names.
	// +required
	SourceCluster string `json:"sourceCluster"`
	// +required
	TargetCluster string `json:"targetCluster"`

	// RecoveryPointRef pins the path to one RecoveryPoint. Empty means "track
	// the group's committed RecoveryPoints", which is NOT the same as "always
	// evaluate the newest one": see RecoveryPathStatus.
	// +optional
	RecoveryPointRef string `json:"recoveryPointRef,omitempty"`

	// TargetPrereqs are the target-side prerequisites to evaluate.
	// +required
	TargetPrereqs TargetPrerequisites `json:"targetPrereqs"`

	// RecoveryActions documents the ordered plan this path would execute.
	// +optional
	RecoveryActions []string `json:"recoveryActions,omitempty"`
}

// ReadinessCheck is one deterministic prerequisite evaluation.
type ReadinessCheck struct {
	Name string `json:"name"`
	// Status is "True", "False" or "Unknown".
	Status string `json:"status"`
	// Mandatory checks must all be True for HOT.
	Mandatory bool `json:"mandatory"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
}

// RecoveryPointRef identifies a RecoveryPoint and the epoch it belongs to.
type RecoveryPointRef struct {
	Name string `json:"name"`
	// +optional
	Epoch int64 `json:"epoch,omitempty"`
	// +optional
	LogicalPosition int64 `json:"logicalPosition,omitempty"`
	// +optional
	CommitTime *metav1.Time `json:"commitTime,omitempty"`
}

// PreparedArtifact is one piece of target-side preparation, recorded with
// enough identity to prove it belongs to the prepared epoch and, where the
// artifact is node-bound, to the prepared node.
type PreparedArtifact struct {
	// Kind is the role the artifact plays: videoCheckpoint, checkpointImage,
	// redisSnapshot, activationPlan.
	Kind string `json:"kind"`
	// Ref is the artifact reference (minio:// URI, image tag, object name).
	// +optional
	Ref string `json:"ref,omitempty"`
	// +optional
	Checksum string `json:"checksum,omitempty"`
	// Node is set for artifacts that only exist on one node.
	// +optional
	Node string `json:"node,omitempty"`
	// +optional
	VerifiedAt *metav1.Time `json:"verifiedAt,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// TargetPlacement is ONE concrete node that satisfies every node-level
// prerequisite at once. A path that cannot name one is not executable, however
// much aggregate capacity the cluster has.
type TargetPlacement struct {
	Cluster string `json:"cluster"`
	Node    string `json:"node"`

	// AllocatableCPUMillis / AllocatableMemoryMiB are what Kubernetes reports
	// the node offers after system reservations.
	// +optional
	AllocatableCPUMillis int64 `json:"allocatableCpuMillis,omitempty"`
	// +optional
	AllocatableMemoryMiB int64 `json:"allocatableMemoryMiB,omitempty"`

	// RequestedCPUMillis / RequestedMemoryMiB are the sums of the resource
	// REQUESTS of the non-terminated pods already on the node. Allocatable
	// minus requested is what a scheduler would treat as free; allocatable
	// alone is not "free capacity" and is not reported as such.
	// +optional
	RequestedCPUMillis int64 `json:"requestedCpuMillis,omitempty"`
	// +optional
	RequestedMemoryMiB int64 `json:"requestedMemoryMiB,omitempty"`

	// +optional
	FreeCPUMillis int64 `json:"freeCpuMillis,omitempty"`
	// +optional
	FreeMemoryMiB int64 `json:"freeMemoryMiB,omitempty"`

	// +optional
	SelectedAt *metav1.Time `json:"selectedAt,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

// PreparedRecoveryPoint is the RecoveryPoint this path can activate RIGHT NOW,
// together with the evidence that the target really is prepared for THAT epoch.
//
// It is deliberately distinct from the group's latest committed RecoveryPoint.
// A newer RecoveryPoint existing is not a reason to stop being able to recover
// from the one already prepared.
type PreparedRecoveryPoint struct {
	RecoveryPointRef `json:",inline"`

	// Placement is the node the preparation belongs to. Node-bound artifacts in
	// Artifacts were verified on THIS node; evidence from any other node is not
	// evidence for this path.
	// +optional
	Placement *TargetPlacement `json:"placement,omitempty"`

	// +optional
	Artifacts []PreparedArtifact `json:"artifacts,omitempty"`

	// PreparedAt is when this RecoveryPoint was promoted from candidate.
	// +optional
	PreparedAt *metav1.Time `json:"preparedAt,omitempty"`

	// Executable reports whether the preparation still holds on this reconcile.
	// A promoted RecoveryPoint whose image was deleted stays recorded here --
	// it is still the point this path would use -- but it stops being
	// executable, and the failing check says why.
	// +optional
	Executable bool `json:"executable"`
}

// CandidateRecoveryPoint is a newer committed RecoveryPoint this path is
// working towards. It NEVER replaces the prepared one until its own
// preparation is complete.
type CandidateRecoveryPoint struct {
	RecoveryPointRef `json:",inline"`

	// ObservedAt is when this candidate was first noticed.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// MissingPreparation lists, by check name, what still has to hold before
	// this candidate can be promoted.
	// +optional
	MissingPreparation []string `json:"missingPreparation,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// ActivationStep is one deterministic unit of failure-time work, with the cost
// assumption used for it. The estimate is a sum of these, never a prediction.
type ActivationStep struct {
	Name string `json:"name"`
	// Cost is the assumed duration of this step.
	Cost metav1.Duration `json:"cost"`
	// Required is false for steps already paid for by preparation.
	Required bool `json:"required"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ContractStatus is the RecoveryContract evaluated against the CURRENT prepared
// RecoveryPoint. Freshness and preparation are separate dimensions on purpose:
// a point can be prepared but stale, or fresh but unprepared, and collapsing
// them hides which one an operator has to fix.
type ContractStatus struct {
	// --- RPO ---
	// RPO is the contract value taken from the RecoveryGroup.
	// +optional
	RPO metav1.Duration `json:"rpo,omitempty"`
	// RecoveryPointAge is now - preparedRecoveryPoint.commitTime.
	// +optional
	RecoveryPointAge metav1.Duration `json:"recoveryPointAge,omitempty"`
	// FreshnessRemaining is RPO - age; negative values are clamped to zero and
	// FreshEnough is false.
	// +optional
	FreshnessRemaining metav1.Duration `json:"freshnessRemaining,omitempty"`
	// +optional
	FreshEnough bool `json:"freshEnough"`

	// --- RTO ---
	// +optional
	RTO metav1.Duration `json:"rto,omitempty"`
	// EstimatedActivationLatency is the sum of the required ActivationSteps.
	// It is explicitly NOT an end-to-end RTO: no failure detection exists yet.
	// +optional
	EstimatedActivationLatency metav1.Duration `json:"estimatedActivationLatency,omitempty"`
	// +optional
	RTOWithinContract bool `json:"rtoWithinContract"`
	// ActivationSteps is the breakdown the estimate was summed from, so the
	// number can be audited instead of trusted.
	// +optional
	ActivationSteps []ActivationStep `json:"activationSteps,omitempty"`
}

// RecoveryPathStatus is the observed readiness of the path.
type RecoveryPathStatus struct {
	// +optional
	// +kubebuilder:validation:Enum=HOT;WARM;COLD
	Readiness Readiness `json:"readiness,omitempty"`

	// LatestRecoveryPoint is the newest committed, validated RecoveryPoint
	// known for the group. It is INFORMATION, not the thing readiness is
	// evaluated against.
	// +optional
	LatestRecoveryPoint *RecoveryPointRef `json:"latestRecoveryPoint,omitempty"`

	// CandidateRecoveryPoint is a newer point being prepared, if any.
	// +optional
	CandidateRecoveryPoint *CandidateRecoveryPoint `json:"candidateRecoveryPoint,omitempty"`

	// PreparedRecoveryPoint is what this path would actually recover from.
	// +optional
	PreparedRecoveryPoint *PreparedRecoveryPoint `json:"preparedRecoveryPoint,omitempty"`

	// TargetPlacement is the concrete node the prepared path lands on.
	// +optional
	TargetPlacement *TargetPlacement `json:"targetPlacement,omitempty"`

	// Contract is the RecoveryContract evaluated against the prepared point.
	// +optional
	Contract ContractStatus `json:"contract,omitempty"`

	// Checks carries every evaluated prerequisite with its reason, so that
	// `kubectl get recoverypath <p> -o yaml` alone explains the readiness level.
	// +optional
	Checks []ReadinessCheck `json:"checks,omitempty"`

	// ObservedRecoveryPoint mirrors preparedRecoveryPoint.name. It is kept so
	// that existing recovery scripts keep resolving to the point that is
	// actually executable -- which, before this change, they did not.
	// +optional
	ObservedRecoveryPoint string `json:"observedRecoveryPoint,omitempty"`
	// +optional
	ObservedEpoch int64 `json:"observedEpoch,omitempty"`

	// EstimatedRTO mirrors contract.estimatedActivationLatency as a string.
	// +optional
	EstimatedRTO string `json:"estimatedRTO,omitempty"`

	// UnmetMandatoryChecks lists, by name, why the path is not HOT.
	// +optional
	UnmetMandatoryChecks []string `json:"unmetMandatoryChecks,omitempty"`

	// +optional
	LastValidatedTime *metav1.Time `json:"lastValidatedTime,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Dur is a nil-safe accessor for a contract duration.
func Dur(d metav1.Duration) time.Duration { return d.Duration }

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rpath
// +kubebuilder:printcolumn:name="Readiness",type=string,JSONPath=`.status.readiness`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetCluster`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.targetPlacement.node`
// +kubebuilder:printcolumn:name="Prepared",type=string,JSONPath=`.status.preparedRecoveryPoint.name`
// +kubebuilder:printcolumn:name="Candidate",type=string,JSONPath=`.status.candidateRecoveryPoint.name`
// +kubebuilder:printcolumn:name="Latest",type=string,JSONPath=`.status.latestRecoveryPoint.name`
// +kubebuilder:printcolumn:name="Age",type=string,JSONPath=`.status.contract.recoveryPointAge`
// +kubebuilder:printcolumn:name="EstRTO",type=string,JSONPath=`.status.estimatedRTO`

// RecoveryPath is an executable recovery plan for a RecoveryGroup together
// with the current readiness of its prerequisites.
type RecoveryPath struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RecoveryPathSpec   `json:"spec,omitempty"`
	Status RecoveryPathStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RecoveryPathList contains a list of RecoveryPath.
type RecoveryPathList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RecoveryPath `json:"items"`
}

func init() { SchemeBuilder.Register(&RecoveryPath{}, &RecoveryPathList{}) }
