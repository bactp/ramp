package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
	"github.com/dcn-ssu/ramp/internal/artifacts"
	"github.com/dcn-ssu/ramp/internal/clusters"
	"github.com/dcn-ssu/ramp/internal/drivers"
	"github.com/dcn-ssu/ramp/internal/rampredis"
)

// RecoveryPointReconciler runs one Recovery Epoch:
//
//	PREPARE -> QUIESCE/BARRIER -> CAPTURE -> VALIDATE -> COMMIT
//
// The epoch is the only thing allowed to declare recovery state usable. It is
// separate from RecoveryGroupReconciler because coordinating heterogeneous
// state is a different job from owning the membership of the domain, and
// separate from RecoveryPathReconciler because producing a recovery point is
// not the same as deciding whether a target can consume one.
type RecoveryPointReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clusters *clusters.Registry
	Store    *artifacts.Store
}

// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypoints/status,verbs=get;update;patch

func (r *RecoveryPointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rp := &rampv1alpha1.RecoveryPoint{}
	if err := r.Get(ctx, req.NamespacedName, rp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal states are terminal. An epoch is never retried in place: a new
	// epoch gets a new RecoveryPoint, so the history of what was and was not
	// recoverable stays auditable.
	if rp.Status.Phase == rampv1alpha1.RecoveryPointCommitted || rp.Status.Phase == rampv1alpha1.RecoveryPointFailed {
		return ctrl.Result{}, nil
	}

	rg := &rampv1alpha1.RecoveryGroup{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rp.Namespace, Name: rp.Spec.RecoveryGroupRef}, rg); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, rp, "RecoveryGroupNotFound", fmt.Sprintf("RecoveryGroup %q does not exist", rp.Spec.RecoveryGroupRef))
		}
		return ctrl.Result{}, err
	}

	log.Info("running recovery epoch", "group", rg.Name, "epoch", rp.Spec.Epoch)
	return r.runEpoch(ctx, rp, rg)
}

func (r *RecoveryPointReconciler) runEpoch(ctx context.Context, rp *rampv1alpha1.RecoveryPoint, rg *rampv1alpha1.RecoveryGroup) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := func() *metav1.Time { t := metav1.Now(); return &t }

	// ---------------------------------------------------------- PREPARE ----
	rp.Status.Phase = rampv1alpha1.RecoveryPointPreparing
	rp.Status.Timings.PrepareStart = now()
	rp.Status.Message = "resolving members and driver prerequisites"
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	src, err := r.Clusters.Get(rg.Spec.AppBundleRef.Cluster)
	if err != nil {
		return r.fail(ctx, rp, "ClusterNotRegistered", err.Error())
	}
	ab, err := GetAppBundle(ctx, src.Client, rg.Spec.AppBundleRef)
	if err != nil {
		return r.fail(ctx, rp, "AppBundleUnavailable", err.Error())
	}

	type member struct {
		spec     rampv1alpha1.RecoveryMember
		resolved *ResolvedComponent
	}
	var members []member
	for _, m := range rg.Spec.Members {
		rc, err := ResolveComponent(ctx, src.Client, ab, m.ComponentRef)
		if err != nil {
			return r.fail(ctx, rp, "MemberUnresolved", fmt.Sprintf("member %q: %v", m.Name, err))
		}
		members = append(members, member{spec: m, resolved: rc})
	}

	// -------------------------------------------------- QUIESCE / BARRIER ---
	// The barrier does not stop the application. It establishes a point that
	// every member's recovery state can be related to: RAMP waits until the
	// Redis member has actually absorbed the application's current logical
	// position and its replica has acknowledged it. Capturing the container
	// before that barrier would produce an in-memory position that no
	// replicated Redis state could match.
	rp.Status.Phase = rampv1alpha1.RecoveryPointQuiescing
	rp.Status.Message = "waiting for replication barrier"
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	var (
		redisMember   *rampv1alpha1.RecoveryMember
		barrierOffset int64
		barrierPos    int64
	)
	for i := range members {
		if members[i].spec.RecoveryDriver == rampv1alpha1.DriverRedisReplication {
			redisMember = &members[i].spec
		}
	}
	if redisMember == nil {
		return r.fail(ctx, rp, "NoConsistencyAnchor",
			"RecoveryGroup has no redis-replication member to anchor the epoch barrier to")
	}

	barrierTimeout := time.Duration(orDefaultInt32(rp.Spec.BarrierTimeoutSeconds, 30)) * time.Second
	rd := &drivers.RedisReplication{}
	barrierDeadline := time.Now().Add(barrierTimeout)
	for {
		capState, err := rd.Capture(redisMember.SourceEndpoint, redisMember.ConsistencyKeys)
		if err == nil && capState.ConnectedReplicas > 0 && capState.LogicalPosition >= 0 {
			barrierOffset, barrierPos = capState.ReplicationOffset, capState.LogicalPosition
			break
		}
		if time.Now().After(barrierDeadline) {
			reason := "barrier not reached within timeout"
			if err != nil {
				reason = err.Error()
			}
			return r.fail(ctx, rp, "BarrierTimeout", reason)
		}
		select {
		case <-ctx.Done():
			return ctrl.Result{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	rp.Status.Timings.BarrierReached = now()
	log.Info("barrier reached", "replicationOffset", barrierOffset, "logicalPosition", barrierPos)

	// ---------------------------------------------------------- CAPTURE ----
	rp.Status.Phase = rampv1alpha1.RecoveryPointCapturing
	rp.Status.Timings.CaptureStart = now()
	rp.Status.Message = "capturing heterogeneous member state"
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	captureTimeout := time.Duration(orDefaultInt32(rp.Spec.CaptureTimeoutSeconds, 300)) * time.Second
	var (
		artifactsOut  []rampv1alpha1.RecoveryArtifact
		ckptPosition  = int64(-1)
		replPosition  = int64(-1)
	)

	for _, m := range members {
		switch m.spec.RecoveryDriver {

		case rampv1alpha1.DriverContainerCheckpoint:
			// Capture the in-memory logical position FIRST, straight from the
			// application's own state file, so the artifact can be compared with
			// the Redis-side position. Without this the two artifacts would be
			// incomparable and "consistent" would be unfalsifiable.
			memberState, posErr := readMemberState(ctx, src, m.resolved.Namespace, m.resolved.PodName, m.spec.Container)

			d := &drivers.ContainerCheckpoint{RestConfig: src.RestConfig}
			res, err := d.Capture(ctx, m.resolved.NodeName, m.resolved.Namespace, m.resolved.PodName, m.spec.Container)
			if err != nil {
				return r.fail(ctx, rp, "CheckpointFailed", fmt.Sprintf("member %q: %v", m.spec.Name, err))
			}

			// The artifact only counts as captured once the existing
			// checkpoint-agent has landed it in the shared store. That hand-off
			// is the Transition Operator's pipeline; RAMP only observes it.
			oi, err := r.Store.WaitFor(ctx, res.ObjectKey, captureTimeout, 2*time.Second)
			if err != nil {
				return r.fail(ctx, rp, "ArtifactNotLanded", fmt.Sprintf("member %q: %v", m.spec.Name, err))
			}

			art := rampv1alpha1.RecoveryArtifact{
				Member:    m.spec.Name,
				Driver:    m.spec.RecoveryDriver,
				Type:      rampv1alpha1.ArtifactContainerCheckpoint,
				Ref:       r.Store.Ref(res.ObjectKey),
				NodePath:  res.NodePath,
				NodeName:  res.Node,
				SizeBytes: oi.SizeBytes,
				CapturedAt: &metav1.Time{Time: res.Completed},
			}
			if posErr != nil {
				art.Message = fmt.Sprintf("in-memory state unavailable: %v", posErr)
			} else {
				art.LogicalPosition = memberState.Position
				art.InstanceFingerprint = memberState.Session
				ckptPosition = memberState.Position
				art.Message = fmt.Sprintf("kubelet checkpoint took %s; instance %s",
					res.Completed.Sub(res.Started).Round(time.Millisecond), memberState.Session)
			}
			artifactsOut = append(artifactsOut, art)

		case rampv1alpha1.DriverRedisReplication:
			capState, err := rd.Capture(m.spec.SourceEndpoint, m.spec.ConsistencyKeys)
			if err != nil {
				return r.fail(ctx, rp, "ReplicationCaptureFailed", fmt.Sprintf("member %q: %v", m.spec.Name, err))
			}
			replPosition = capState.LogicalPosition
			artifactsOut = append(artifactsOut, rampv1alpha1.RecoveryArtifact{
				Member:            m.spec.Name,
				Driver:            m.spec.RecoveryDriver,
				Type:              rampv1alpha1.ArtifactReplicationState,
				Ref:               fmt.Sprintf("redis-repl://%s/%d", capState.Endpoint, capState.ReplicationOffset),
				ReplicationOffset: capState.ReplicationOffset,
				ReplicationID:     capState.ReplicationID,
				LogicalPosition:   capState.LogicalPosition,
				CapturedAt:        &metav1.Time{Time: capState.CapturedAt},
				Message:           fmt.Sprintf("role=%s connectedReplicas=%d", capState.Role, capState.ConnectedReplicas),
			})
		}
	}
	rp.Status.Artifacts = artifactsOut
	rp.Status.Timings.CaptureComplete = now()

	// --------------------------------------------------------- VALIDATE ----
	// This is the correctness question Scenario 1 exists to answer: can these
	// heterogeneous states jointly form an executable application recovery
	// point? They can only if the position the container carries in memory and
	// the position Redis committed refer to the same stream, within tolerance.
	rp.Status.Phase = rampv1alpha1.RecoveryPointValidating
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	v := rampv1alpha1.ValidationResult{
		CheckpointPosition: ckptPosition,
		ReplicatedPosition: replPosition,
	}
	switch {
	case len(artifactsOut) != len(rg.Spec.Members):
		v.Validated = false
		v.Reason = "IncompleteEpoch"
		v.Message = fmt.Sprintf("captured %d artifacts for %d members", len(artifactsOut), len(rg.Spec.Members))
	case ckptPosition < 0 || replPosition < 0:
		v.Validated = false
		v.Reason = "PositionUnavailable"
		v.Message = fmt.Sprintf("checkpointPosition=%d replicatedPosition=%d: cannot prove the artifacts refer to the same stream", ckptPosition, replPosition)
	default:
		skew := ckptPosition - replPosition
		if skew < 0 {
			skew = -skew
		}
		v.ObservedSkew = skew
		maxSkew := rp.Spec.MaxPositionSkew
		if maxSkew == 0 {
			maxSkew = 2
		}
		if skew <= maxSkew {
			v.Validated = true
			v.Reason = "ConsistentAcrossDrivers"
			v.Message = fmt.Sprintf("container-checkpoint position %d and redis-replication position %d differ by %d (tolerance %d)",
				ckptPosition, replPosition, skew, maxSkew)
		} else {
			v.Validated = false
			v.Reason = "PositionSkewExceeded"
			v.Message = fmt.Sprintf("container-checkpoint position %d and redis-replication position %d differ by %d, tolerance is %d",
				ckptPosition, replPosition, skew, maxSkew)
		}
	}
	rp.Status.Validation = v
	rp.Status.Timings.ValidateComplete = now()

	if !v.Validated {
		// An epoch that fails validation must not become recovery eligible.
		rp.Status.Phase = rampv1alpha1.RecoveryPointFailed
		rp.Status.Message = v.Message
		setCondition(&rp.Status.Conditions, "Validated", metav1.ConditionFalse, v.Reason, v.Message, rp.Generation)
		setCondition(&rp.Status.Conditions, "Committed", metav1.ConditionFalse, "ValidationFailed", "epoch did not commit", rp.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, rp)
	}

	// ----------------------------------------------------------- COMMIT ----
	rp.Status.Phase = rampv1alpha1.RecoveryPointCommitted
	rp.Status.Timings.CommitTime = now()
	rp.Status.Message = fmt.Sprintf("epoch %d committed with %d heterogeneous artifacts", rp.Spec.Epoch, len(artifactsOut))
	setCondition(&rp.Status.Conditions, "Validated", metav1.ConditionTrue, v.Reason, v.Message, rp.Generation)
	setCondition(&rp.Status.Conditions, "Committed", metav1.ConditionTrue, "EpochCommitted", rp.Status.Message, rp.Generation)

	log.Info("recovery point committed", "epoch", rp.Spec.Epoch,
		"checkpointPosition", ckptPosition, "replicatedPosition", replPosition, "skew", v.ObservedSkew)
	return ctrl.Result{}, r.Status().Update(ctx, rp)
}

func (r *RecoveryPointReconciler) fail(ctx context.Context, rp *rampv1alpha1.RecoveryPoint, reason, msg string) (ctrl.Result, error) {
	rp.Status.Phase = rampv1alpha1.RecoveryPointFailed
	rp.Status.Message = msg
	rp.Status.Validation.Validated = false
	if rp.Status.Validation.Reason == "" {
		rp.Status.Validation.Reason = reason
	}
	setCondition(&rp.Status.Conditions, "Committed", metav1.ConditionFalse, reason, msg, rp.Generation)
	return ctrl.Result{}, r.Status().Update(ctx, rp)
}

// readMemberState reads the container-checkpoint member's in-memory state --
// its logical position and the identity of this process instance -- from the
// state file the application maintains. It goes through the apiserver exec
// subresource because, as with the kubelet checkpoint call, the management
// plane has no route to the pod network.
func readMemberState(ctx context.Context, c *clusters.Cluster, namespace, pod, container string) (videoState, error) {
	out, err := clusters.Exec(ctx, c, namespace, pod, container,
		[]string{"sh", "-c", "cat /tmp/ramp-video-state.json"})
	if err != nil {
		return videoState{}, err
	}
	return parseMemberState(out)
}

func orDefaultInt32(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

func (r *RecoveryPointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rampv1alpha1.RecoveryPoint{}).
		Named("recoverypoint").
		Complete(r)
}

var _ = corev1.Pod{}
var _ = rampredis.DialTimeout
