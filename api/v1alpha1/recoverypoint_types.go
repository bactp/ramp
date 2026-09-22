package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RecoveryPointPhase is the Recovery Epoch state machine. The order is fixed:
//
//	Pending -> Preparing -> Quiescing -> Capturing -> Validating -> Committed
//
// Any failure short-circuits to Failed. ONLY Committed+validated is recovery
// eligible; every other phase is explicitly not.
type RecoveryPointPhase string

const (
	RecoveryPointPending    RecoveryPointPhase = "Pending"
	RecoveryPointPreparing  RecoveryPointPhase = "Preparing"
	RecoveryPointQuiescing  RecoveryPointPhase = "Quiescing"
	RecoveryPointCapturing  RecoveryPointPhase = "Capturing"
	RecoveryPointValidating RecoveryPointPhase = "Validating"
	RecoveryPointCommitted  RecoveryPointPhase = "Committed"
	RecoveryPointFailed     RecoveryPointPhase = "Failed"
)

// ArtifactType distinguishes heterogeneous recovery state within one epoch.
type ArtifactType string

const (
	ArtifactContainerCheckpoint ArtifactType = "containerCheckpoint"
	ArtifactReplicationState    ArtifactType = "replicationState"
)

// RecoveryPointSpec defines one Recovery Epoch to be executed.
type RecoveryPointSpec struct {
	// RecoveryGroupRef is the group this epoch captures.
	// +required
	RecoveryGroupRef string `json:"recoveryGroupRef"`

	// Epoch is the monotonically increasing epoch number.
	// +required
	Epoch int64 `json:"epoch"`

	// BarrierTimeoutSeconds bounds the QUIESCE/BARRIER stage.
	// +optional
	// +kubebuilder:default=30
	BarrierTimeoutSeconds int32 `json:"barrierTimeoutSeconds,omitempty"`

	// CaptureTimeoutSeconds bounds the CAPTURE stage (including artifact
	// landing in the shared artifact store).
	// +optional
	// +kubebuilder:default=300
	CaptureTimeoutSeconds int32 `json:"captureTimeoutSeconds,omitempty"`

	// MaxPositionSkew is the largest tolerated difference, in application
	// logical positions, between the container-checkpoint member's captured
	// in-memory position and the Redis-committed position. This is the
	// cross-driver consistency tolerance that makes the epoch meaningful.
	// +optional
	// +kubebuilder:default=2
	MaxPositionSkew int64 `json:"maxPositionSkew,omitempty"`
}

// RecoveryArtifact is one member's captured recovery state. Artifacts within a
// RecoveryPoint are heterogeneous by design.
type RecoveryArtifact struct {
	Member string         `json:"member"`
	Driver RecoveryDriver `json:"driver"`
	Type   ArtifactType   `json:"type"`

	// Ref is the canonical, resolvable reference to the artifact.
	//   containerCheckpoint -> minio://<bucket>/<object>
	//   replicationState    -> redis-repl://<host>:<port>/<offset>
	// +optional
	Ref string `json:"ref,omitempty"`

	// --- containerCheckpoint fields ---
	// +optional
	NodePath string `json:"nodePath,omitempty"`
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// --- replicationState fields ---
	// +optional
	ReplicationOffset int64 `json:"replicationOffset,omitempty"`
	// +optional
	ReplicationID string `json:"replicationId,omitempty"`

	// LogicalPosition is the application-level position this artifact
	// corresponds to. It is what makes two heterogeneous artifacts comparable.
	// +optional
	LogicalPosition int64 `json:"logicalPosition,omitempty"`

	// InstanceFingerprint identifies the specific process instance whose state
	// was captured -- for the video member, the session id it generated in
	// memory at start and never persists. After a recovery it is the evidence
	// that distinguishes a genuine restore (same fingerprint) from a restart
	// that merely re-read the replicated state (new fingerprint). Without it
	// "the application came back" is unfalsifiable.
	// +optional
	InstanceFingerprint string `json:"instanceFingerprint,omitempty"`

	// +optional
	CapturedAt *metav1.Time `json:"capturedAt,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// EpochTimings records stage boundaries for the RTO/RPO analysis.
type EpochTimings struct {
	// +optional
	PrepareStart *metav1.Time `json:"prepareStart,omitempty"`
	// +optional
	BarrierReached *metav1.Time `json:"barrierReached,omitempty"`
	// +optional
	CaptureStart *metav1.Time `json:"captureStart,omitempty"`
	// +optional
	CaptureComplete *metav1.Time `json:"captureComplete,omitempty"`
	// +optional
	ValidateComplete *metav1.Time `json:"validateComplete,omitempty"`
	// +optional
	CommitTime *metav1.Time `json:"commitTime,omitempty"`
}

// ValidationResult is the cross-driver consistency verdict.
type ValidationResult struct {
	// Validated is true only when every mandatory consistency check passed.
	Validated bool `json:"validated"`
	// +optional
	CheckpointPosition int64 `json:"checkpointPosition,omitempty"`
	// +optional
	ReplicatedPosition int64 `json:"replicatedPosition,omitempty"`
	// +optional
	ObservedSkew int64 `json:"observedSkew,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// RecoveryPointStatus is the observed state of a Recovery Epoch.
type RecoveryPointStatus struct {
	// +optional
	Phase RecoveryPointPhase `json:"phase,omitempty"`
	// +optional
	Artifacts []RecoveryArtifact `json:"artifacts,omitempty"`
	// +optional
	Validation ValidationResult `json:"validation,omitempty"`
	// +optional
	Timings EpochTimings `json:"timings,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RecoveryEligible reports whether this RecoveryPoint may be used to recover.
// This is the single gate the readiness controller is allowed to consult.
func (rp *RecoveryPoint) RecoveryEligible() bool {
	return rp.Status.Phase == RecoveryPointCommitted && rp.Status.Validation.Validated
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rp
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.recoveryGroupRef`
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.spec.epoch`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Validated",type=boolean,JSONPath=`.status.validation.validated`
// +kubebuilder:printcolumn:name="Skew",type=integer,JSONPath=`.status.validation.observedSkew`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RecoveryPoint is a committed set of mutually consistent recovery states for
// a RecoveryGroup, produced by one Recovery Epoch.
type RecoveryPoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RecoveryPointSpec   `json:"spec,omitempty"`
	Status RecoveryPointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RecoveryPointList contains a list of RecoveryPoint.
type RecoveryPointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RecoveryPoint `json:"items"`
}

func init() { SchemeBuilder.Register(&RecoveryPoint{}, &RecoveryPointList{}) }
