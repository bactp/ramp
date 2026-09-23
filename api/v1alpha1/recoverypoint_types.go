package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RecoveryPointPhase is the Recovery Epoch state machine. The order is fixed:
//
//	Pending -> Preparing -> Quiescing -> BarrierEstablished
//	        -> CapturingRedis -> CapturingVideo -> Validating -> Committed
//
// Any failure short-circuits through Aborting to Failed. ONLY Committed+validated
// is recovery eligible; every other phase is explicitly not.
type RecoveryPointPhase string

const (
	RecoveryPointPending            RecoveryPointPhase = "Pending"
	RecoveryPointPreparing          RecoveryPointPhase = "Preparing"
	RecoveryPointQuiescing          RecoveryPointPhase = "Quiescing"
	RecoveryPointBarrierEstablished RecoveryPointPhase = "BarrierEstablished"
	RecoveryPointCapturingRedis     RecoveryPointPhase = "CapturingRedis"
	RecoveryPointCapturingVideo     RecoveryPointPhase = "CapturingVideo"
	RecoveryPointValidating         RecoveryPointPhase = "Validating"
	RecoveryPointCommitted          RecoveryPointPhase = "Committed"
	RecoveryPointAborting           RecoveryPointPhase = "Aborting"
	RecoveryPointFailed             RecoveryPointPhase = "Failed"
)

// ArtifactType distinguishes heterogeneous recovery state within one epoch.
type ArtifactType string

const (
	// ArtifactContainerCheckpoint is a CRIU/kubelet container checkpoint tar.
	ArtifactContainerCheckpoint ArtifactType = "containerCheckpoint"

	// ArtifactRedisSnapshot is an epoch-specific, immutable Redis RDB taken
	// while the application was quiesced at the epoch's logical position. This
	// -- not the live replica -- is what a RecoveryPoint restores Redis from.
	ArtifactRedisSnapshot ArtifactType = "redisSnapshot"

	// ArtifactReplicationState is the *diagnostic* record of where the live
	// replication stream stood when the epoch committed. It is deliberately NOT
	// a recovery artifact: the replica keeps moving after commit, so it cannot
	// identify the committed epoch afterwards. Replication is the readiness
	// mechanism; the RDB snapshot is the recovery point.
	ArtifactReplicationState ArtifactType = "replicationState"
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
	// in-memory position and the position frozen into the Redis epoch
	// snapshot. With a real quiesce this is expected to be 0; the knob stays so
	// that the tolerance is explicit rather than assumed.
	// +optional
	// +kubebuilder:default=0
	MaxPositionSkew int64 `json:"maxPositionSkew,omitempty"`

	// RequiredReplicaAcks is the number of replicas that must acknowledge the
	// epoch marker write before the barrier counts as established. It is passed
	// to Redis WAIT; connected_replicas is not an acknowledgement.
	// +optional
	// +kubebuilder:default=1
	RequiredReplicaAcks int32 `json:"requiredReplicaAcks,omitempty"`

	// QuiesceVerifySeconds is the interval between the two logical-position
	// samples that prove the application really stopped advancing.
	// +optional
	// +kubebuilder:default=3
	QuiesceVerifySeconds int32 `json:"quiesceVerifySeconds,omitempty"`
}

// AnnFaultInjection is a TEST-ONLY annotation carrying a JSON fault-injection
// directive; see internal/controller/faultinjection.go.
//
// It used to be spec.faultInjection -- a first-class, schema-validated field of
// the production API whose only purpose was to make the negative tests fail on
// command. An API field is a contract offered to users, and "please corrupt
// this epoch" is not one this project wants to offer. As an annotation it stays
// exactly as usable by the test suite, stays out of the CRD schema, and reads as
// what it is.
const AnnFaultInjection = "ramp.dcn.ssu.ac.kr/test-fault-injection"

// RecoveryArtifact is one member's captured recovery state. Artifacts within a
// RecoveryPoint are heterogeneous by design.
type RecoveryArtifact struct {
	Member string         `json:"member"`
	Driver RecoveryDriver `json:"driver"`
	Type   ArtifactType   `json:"type"`

	// Epoch is stamped on every artifact so that "these artifacts belong to the
	// same recovery epoch" is checkable on the artifact itself, not inferred
	// from which object happens to carry it.
	// +optional
	Epoch int64 `json:"epoch,omitempty"`

	// Ref is the canonical, resolvable reference to the artifact.
	//   containerCheckpoint -> minio://<bucket>/<object>
	//   redisSnapshot       -> minio://<bucket>/<object>
	//   replicationState    -> redis-repl://<host>:<port>/<offset>  (diagnostic)
	// +optional
	Ref string `json:"ref,omitempty"`

	// Immutable marks an artifact that cannot change after commit. A
	// replicationState reference is NOT immutable and is never restorable.
	// +optional
	Immutable bool `json:"immutable,omitempty"`

	// Checksum is the artifact's content digest (algorithm in ChecksumAlgorithm).
	// +optional
	Checksum string `json:"checksum,omitempty"`
	// +optional
	ChecksumAlgorithm string `json:"checksumAlgorithm,omitempty"`

	// --- containerCheckpoint fields ---
	// +optional
	NodePath string `json:"nodePath,omitempty"`
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// --- redis fields ---
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
	// that merely re-read the replicated state (new fingerprint).
	// +optional
	InstanceFingerprint string `json:"instanceFingerprint,omitempty"`

	// +optional
	CapturedAt *metav1.Time `json:"capturedAt,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// QuiesceStatus records that the application really was held still, and for
// how long. Without it "the artifacts agree" is an accident, not a property.
type QuiesceStatus struct {
	// +optional
	Quiesced bool `json:"quiesced"`
	// +optional
	QuiescedAt *metav1.Time `json:"quiescedAt,omitempty"`
	// +optional
	ResumedAt *metav1.Time `json:"resumedAt,omitempty"`
	// Position is the logical position P the application was frozen at.
	// +optional
	Position int64 `json:"position,omitempty"`
	// VerifySamples are the two (or more) successive position samples taken
	// while quiesced; they must all be equal.
	// +optional
	VerifySamples []int64 `json:"verifySamples,omitempty"`
	// +optional
	InstanceFingerprint string `json:"instanceFingerprint,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// BarrierStatus records the Redis replication acknowledgement for this epoch.
type BarrierStatus struct {
	// +optional
	Established bool `json:"established"`
	// +optional
	EstablishedAt *metav1.Time `json:"establishedAt,omitempty"`
	// Endpoint is the primary the marker was written to.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// MarkerOffset is the primary replication offset immediately after the
	// epoch marker keys were written.
	// +optional
	MarkerOffset int64 `json:"markerOffset,omitempty"`
	// +optional
	ReplicationID string `json:"replicationId,omitempty"`
	// AckedReplicas is what Redis WAIT actually returned.
	// +optional
	AckedReplicas int64 `json:"ackedReplicas,omitempty"`
	// RequiredReplicas is what was demanded.
	// +optional
	RequiredReplicas int64 `json:"requiredReplicas,omitempty"`
	// +optional
	Position int64 `json:"position,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// EpochTimings records stage boundaries for the RTO/RPO analysis.
type EpochTimings struct {
	// +optional
	PrepareStart *metav1.Time `json:"prepareStart,omitempty"`
	// +optional
	QuiesceStart *metav1.Time `json:"quiesceStart,omitempty"`
	// +optional
	QuiesceComplete *metav1.Time `json:"quiesceComplete,omitempty"`
	// +optional
	BarrierReached *metav1.Time `json:"barrierReached,omitempty"`
	// +optional
	RedisSnapshotStart *metav1.Time `json:"redisSnapshotStart,omitempty"`
	// +optional
	RedisSnapshotComplete *metav1.Time `json:"redisSnapshotComplete,omitempty"`
	// +optional
	CaptureStart *metav1.Time `json:"captureStart,omitempty"`
	// +optional
	VideoCheckpointStart *metav1.Time `json:"videoCheckpointStart,omitempty"`
	// +optional
	VideoCheckpointComplete *metav1.Time `json:"videoCheckpointComplete,omitempty"`
	// +optional
	CaptureComplete *metav1.Time `json:"captureComplete,omitempty"`
	// +optional
	ValidateComplete *metav1.Time `json:"validateComplete,omitempty"`
	// +optional
	CommitTime *metav1.Time `json:"commitTime,omitempty"`
	// +optional
	ResumeTime *metav1.Time `json:"resumeTime,omitempty"`
}

// ValidationResult is the cross-driver consistency verdict.
type ValidationResult struct {
	// Validated is true only when every mandatory consistency check passed.
	Validated bool `json:"validated"`
	// +optional
	CheckpointPosition int64 `json:"checkpointPosition,omitempty"`
	// RedisSnapshotPosition is the position frozen into the immutable Redis
	// artifact -- NOT the live replica's current position.
	// +optional
	RedisSnapshotPosition int64 `json:"redisSnapshotPosition,omitempty"`
	// ReplicatedPosition is the live replication-stream position at commit,
	// kept as a diagnostic only.
	// +optional
	ReplicatedPosition int64 `json:"replicatedPosition,omitempty"`
	// +optional
	ObservedSkew int64 `json:"observedSkew,omitempty"`
	// Checks is the per-condition validation record.
	// +optional
	Checks []ValidationCheck `json:"checks,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// ValidationCheck is one mandatory pre-commit condition.
type ValidationCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// +optional
	Detail string `json:"detail,omitempty"`
}

// RecoveryPointStatus is the observed state of a Recovery Epoch.
type RecoveryPointStatus struct {
	// +optional
	Phase RecoveryPointPhase `json:"phase,omitempty"`

	// LogicalPosition is P: the single application position this whole epoch
	// is anchored to.
	// +optional
	LogicalPosition int64 `json:"logicalPosition,omitempty"`

	// LastSuccessfulStage is the furthest phase that completed. On a failure it
	// is what tells an operator where the epoch stopped.
	// +optional
	LastSuccessfulStage RecoveryPointPhase `json:"lastSuccessfulStage,omitempty"`

	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// RunID is the manager instance that owns the in-flight epoch. If a
	// reconcile finds a mid-flight epoch stamped with a different RunID the
	// manager restarted mid-epoch, and the epoch is aborted rather than resumed
	// against an application that has since moved on.
	// +optional
	RunID string `json:"runId,omitempty"`

	// +optional
	Quiesce QuiesceStatus `json:"quiesce,omitempty"`
	// +optional
	Barrier BarrierStatus `json:"barrier,omitempty"`
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

// RestorableArtifact returns the immutable artifact of the requested type, or
// nil. A replicationState record is never restorable and never returned here.
func (rp *RecoveryPoint) RestorableArtifact(t ArtifactType) *RecoveryArtifact {
	if t == ArtifactReplicationState {
		return nil
	}
	for i := range rp.Status.Artifacts {
		if rp.Status.Artifacts[i].Type == t && rp.Status.Artifacts[i].Immutable {
			return &rp.Status.Artifacts[i]
		}
	}
	return nil
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rp
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.recoveryGroupRef`
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.spec.epoch`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="P",type=integer,JSONPath=`.status.logicalPosition`
// +kubebuilder:printcolumn:name="Validated",type=boolean,JSONPath=`.status.validation.validated`
// +kubebuilder:printcolumn:name="Skew",type=integer,JSONPath=`.status.validation.observedSkew`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RecoveryPoint is a committed set of mutually consistent, immutable recovery
// artifacts for a RecoveryGroup, produced by one Recovery Epoch.
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
