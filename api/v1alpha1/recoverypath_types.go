package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Readiness is the elastic recovery-readiness level of a RecoveryPath.
type Readiness string

const (
	// ReadinessHot means every mandatory prerequisite for satisfying the
	// RecoveryContract already holds. Failure-time work is activation only.
	ReadinessHot Readiness = "HOT"
	// ReadinessWarm means recovery is feasible from a committed RecoveryPoint,
	// but one or more preparation actions are still outstanding.
	ReadinessWarm Readiness = "WARM"
	// ReadinessCold means no sufficiently prepared target path currently exists.
	ReadinessCold Readiness = "COLD"
)

// Check names are stable identifiers so operators can reason about them.
const (
	CheckTargetClusterReachable    = "TargetClusterReachable"
	CheckRecoveryPointCommitted    = "RecoveryPointCommitted"
	CheckVideoCheckpointAvailable  = "VideoCheckpointAvailable"
	CheckVideoCheckpointStaged     = "VideoCheckpointStaged"
	CheckRedisStandbyReady         = "RedisStandbyReady"
	CheckRestoreCapabilityAvailable = "RestoreCapabilityAvailable"
	CheckTargetResourceReady       = "TargetResourceReady"
	CheckTargetNamespaceReady      = "TargetNamespaceReady"
)

// TargetPrerequisites are the target-side facts the readiness evaluation needs.
// These are deliberately explicit rather than discovered: Scenario 1
// implements deterministic evaluation of ONE manually specified path, not an
// optimizer that searches for paths.
type TargetPrerequisites struct {
	// Namespace the recovered RecoveryGroup will land in on the target.
	// +required
	Namespace string `json:"namespace"`

	// StandbyEndpoint is the target-side Redis standby the redis-replication
	// driver will promote.
	// +optional
	StandbyEndpoint *RedisEndpoint `json:"standbyEndpoint,omitempty"`

	// MaxReplicationLagBytes is the largest tolerated standby lag, in
	// replication-stream bytes, for RedisStandbyReady to hold.
	// +optional
	// +kubebuilder:default=4096
	MaxReplicationLagBytes int64 `json:"maxReplicationLagBytes,omitempty"`

	// CheckpointStagingPath is the on-node directory the checkpoint-agent
	// syncs artifacts into on the target cluster.
	// +optional
	// +kubebuilder:default="/var/lib/kubelet/checkpoints"
	CheckpointStagingPath string `json:"checkpointStagingPath,omitempty"`

	// RestoreCapabilityDaemonSet is the DaemonSet whose readiness proves the
	// target can stage and restore checkpoint artifacts.
	// +optional
	RestoreCapabilityDaemonSet *NamespacedName `json:"restoreCapabilityDaemonSet,omitempty"`

	// MinAllocatable is the minimum free capacity required on the target node.
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
	// the group's latest committed RecoveryPoint".
	// +optional
	RecoveryPointRef string `json:"recoveryPointRef,omitempty"`

	// TargetPrereqs are the target-side prerequisites to evaluate.
	// +required
	TargetPrereqs TargetPrerequisites `json:"targetPrereqs"`

	// RecoveryActions documents the ordered plan this path would execute. It is
	// descriptive in Scenario 1 (actuation stays with the Transition Operator /
	// runbook) but it is part of the path definition, not of the target cluster.
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

// RecoveryPathStatus is the observed readiness of the path.
type RecoveryPathStatus struct {
	// +optional
	// +kubebuilder:validation:Enum=HOT;WARM;COLD
	Readiness Readiness `json:"readiness,omitempty"`

	// Checks carries every evaluated prerequisite with its reason, so that
	// `kubectl get recoverypath <p> -o yaml` alone explains the readiness level.
	// +optional
	Checks []ReadinessCheck `json:"checks,omitempty"`

	// ObservedRecoveryPoint is the RecoveryPoint this evaluation was made against.
	// +optional
	ObservedRecoveryPoint string `json:"observedRecoveryPoint,omitempty"`
	// +optional
	ObservedEpoch int64 `json:"observedEpoch,omitempty"`

	// EstimatedRTO is a deterministic sum of the remaining activation steps for
	// the current readiness level. No prediction model is involved.
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

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rpath
// +kubebuilder:printcolumn:name="Readiness",type=string,JSONPath=`.status.readiness`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceCluster`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetCluster`
// +kubebuilder:printcolumn:name="RecoveryPoint",type=string,JSONPath=`.status.observedRecoveryPoint`
// +kubebuilder:printcolumn:name="EstRTO",type=string,JSONPath=`.status.estimatedRTO`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
