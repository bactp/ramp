package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RecoveryDriver names the recovery mechanism bound to a member. Different
// members of the same RecoveryGroup deliberately use different drivers -- that
// heterogeneity is the property Scenario 1 exists to demonstrate.
type RecoveryDriver string

const (
	// DriverContainerCheckpoint recovers a member from a CRIU/kubelet container
	// checkpoint artifact. Capture is delegated to the kubelet Container
	// Checkpoint API; artifact transport is the existing checkpoint-agent ->
	// MinIO pipeline owned by the Transition Operator.
	DriverContainerCheckpoint RecoveryDriver = "container-checkpoint"

	// DriverRedisReplication recovers a member by Redis-native continuity. The
	// member is NOT checkpointed; the recovery state is a replica that is
	// already carrying the data, and recovery is a promotion.
	DriverRedisReplication RecoveryDriver = "redis-replication"
)

// AppBundleReference points at the AppBundle that owns the application graph.
// RAMP does not re-describe the application: deployment structure stays in
// AppBundle, and RecoveryGroup only names which of its components form a
// recovery-consistency domain.
type AppBundleReference struct {
	// Name of the AppBundle object.
	// +required
	Name string `json:"name"`
	// Namespace of the AppBundle object.
	// +required
	Namespace string `json:"namespace"`
	// Cluster is the registered cluster name the AppBundle lives in.
	// +required
	Cluster string `json:"cluster"`
}

// ComponentReference addresses one component inside an AppBundle. Resolution
// goes through AppBundle .status.groupStatuses[].componentStatuses[].resourceRef,
// so no Kubernetes template is ever copied into a RecoveryGroup.
type ComponentReference struct {
	// Group is the AppBundle group name.
	// +required
	Group string `json:"group"`
	// Component is the AppBundle component name inside that group.
	// +required
	Component string `json:"component"`
}

// RedisEndpoint is how the redis-replication driver reaches a Redis instance.
// Addresses are explicit because workload node networks are not routable
// between clusters in this testbed; only the shared transit network is.
type RedisEndpoint struct {
	// Host is a routable address (floating IP or DNS name).
	// +required
	Host string `json:"host"`
	// Port is the Redis port on that address.
	// +required
	Port int32 `json:"port"`
}

// RecoveryMember is one element of the recovery-consistency domain.
type RecoveryMember struct {
	// Name is the member's identity inside the RecoveryGroup.
	// +required
	Name string `json:"name"`

	// ComponentRef binds this member to an AppBundle component.
	// +required
	ComponentRef ComponentReference `json:"componentRef"`

	// RecoveryDriver selects the recovery mechanism for this member.
	// +kubebuilder:validation:Enum=container-checkpoint;redis-replication
	// +required
	RecoveryDriver RecoveryDriver `json:"recoveryDriver"`

	// Container names the container to checkpoint. Required for
	// recoveryDriver=container-checkpoint.
	// +optional
	Container string `json:"container,omitempty"`

	// SourceEndpoint is the driver's source-side address
	// (redis-replication: the primary).
	// +optional
	SourceEndpoint *RedisEndpoint `json:"sourceEndpoint,omitempty"`

	// ConsistencyKeys are Redis keys whose values pin the application logical
	// position captured into a RecoveryPoint. Used by redis-replication to make
	// the recovery point meaningful to the application, not just byte-consistent.
	// +optional
	ConsistencyKeys []string `json:"consistencyKeys,omitempty"`
}

// RecoveryContract is the recovery objective the group is held to.
type RecoveryContract struct {
	// RTO is the recovery time objective.
	// +optional
	RTO metav1.Duration `json:"rto,omitempty"`
	// RPO is the recovery point objective.
	// +optional
	RPO metav1.Duration `json:"rpo,omitempty"`
}

// RecoveryGroupSpec defines the desired state of a RecoveryGroup.
type RecoveryGroupSpec struct {
	// AppBundleRef is the authoritative application graph.
	// +required
	AppBundleRef AppBundleReference `json:"appBundleRef"`

	// Members is the recovery-consistency domain. This is deliberately a SUBSET
	// of the AppBundle's components: Services/ConfigMaps/Namespaces are
	// deployment concerns and become RecoveryPath prerequisites instead.
	// +required
	Members []RecoveryMember `json:"members"`

	// RecoveryContract is the objective this group is held to.
	// +optional
	RecoveryContract RecoveryContract `json:"recoveryContract,omitempty"`
}

// MemberStatus is the resolved runtime view of a member.
type MemberStatus struct {
	Name           string         `json:"name"`
	RecoveryDriver RecoveryDriver `json:"recoveryDriver"`

	// Resolved reports whether the AppBundle component could be mapped to a
	// live runtime resource.
	Resolved bool `json:"resolved"`

	// APIVersion/Kind/Namespace/Name come from the AppBundle component's
	// status.resourceRef -- copied, never re-derived.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	ResourceName string `json:"resourceName,omitempty"`

	// PodName/NodeName are the runtime target the driver acts on.
	// +optional
	PodName string `json:"podName,omitempty"`
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// DriverReady reports whether the bound driver's prerequisites hold on the
	// source side (e.g. checkpoint feature reachable, Redis answering).
	DriverReady bool `json:"driverReady"`

	// +optional
	Message string `json:"message,omitempty"`
}

// RecoveryGroupPhase is the coarse state of the group.
type RecoveryGroupPhase string

const (
	RecoveryGroupPending  RecoveryGroupPhase = "Pending"
	RecoveryGroupResolved RecoveryGroupPhase = "Resolved"
	RecoveryGroupDegraded RecoveryGroupPhase = "Degraded"
)

// RecoveryGroupStatus is the observed state of a RecoveryGroup.
type RecoveryGroupStatus struct {
	// +optional
	Phase RecoveryGroupPhase `json:"phase,omitempty"`
	// +optional
	Members []MemberStatus `json:"members,omitempty"`

	// LatestEpoch is the highest epoch number observed for this group.
	// +optional
	LatestEpoch int64 `json:"latestEpoch,omitempty"`

	// LatestRecoveryPoint names the newest RecoveryPoint that reached
	// phase=Committed AND validated=true. An incomplete epoch never appears here.
	// +optional
	LatestRecoveryPoint string `json:"latestRecoveryPoint,omitempty"`
	// +optional
	LatestRecoveryPointTime *metav1.Time `json:"latestRecoveryPointTime,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rg
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=`.status.latestEpoch`,priority=1
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.status.latestEpoch`
// +kubebuilder:printcolumn:name="LatestRecoveryPoint",type=string,JSONPath=`.status.latestRecoveryPoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RecoveryGroup is the minimum recovery-consistency domain: the set of
// components that cannot be recovered independently without risking
// application correctness or the recovery objective.
type RecoveryGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RecoveryGroupSpec   `json:"spec,omitempty"`
	Status RecoveryGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RecoveryGroupList contains a list of RecoveryGroup.
type RecoveryGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RecoveryGroup `json:"items"`
}

func init() { SchemeBuilder.Register(&RecoveryGroup{}, &RecoveryGroupList{}) }
