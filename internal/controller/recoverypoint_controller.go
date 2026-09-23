package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
	"github.com/dcn-ssu/ramp/internal/artifacts"
	"github.com/dcn-ssu/ramp/internal/clusters"
	"github.com/dcn-ssu/ramp/internal/drivers"
)

// RecoveryPointReconciler runs one Recovery Epoch:
//
//	PREPARE -> QUIESCE -> BARRIER -> CAPTURE REDIS -> CAPTURE VIDEO
//	        -> VALIDATE -> COMMIT -> RESUME
//
// The epoch is the only thing allowed to declare recovery state usable, and the
// only thing that may pause the source application. Two properties drive the
// shape of the code:
//
//	(1) The application is held at ONE logical position P for the whole capture,
//	    so the artifacts are not merely close, they are the same point.
//	(2) Every member's recovery state is turned into an IMMUTABLE artifact.
//	    A live Redis replica keeps moving after commit and therefore cannot
//	    identify the committed epoch afterwards; it is a readiness mechanism,
//	    not a recovery point.
type RecoveryPointReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clusters *clusters.Registry
	Store    *artifacts.Store

	// APIReader reads straight from the apiserver, bypassing the informer
	// cache. Claiming an epoch MUST NOT be decided on cached data: a status
	// update made during the epoch queues another reconcile, and that reconcile
	// can observe the object as it was BEFORE the epoch started. Acting on that
	// means re-quiescing the application and re-running a finished epoch.
	APIReader client.Reader

	// RunID identifies this manager process. It is stamped on an epoch when it
	// starts so that an epoch interrupted by a manager restart is aborted
	// rather than silently resumed against an application that has moved on.
	RunID string
}

// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypoints/status,verbs=get;update;patch

func (r *RecoveryPointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rp := &rampv1alpha1.RecoveryPoint{}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if err := reader.Get(ctx, req.NamespacedName, rp); err != nil {
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
			return r.abort(ctx, rp, nil, "RecoveryGroupNotFound",
				fmt.Sprintf("RecoveryGroup %q does not exist", rp.Spec.RecoveryGroupRef))
		}
		return ctrl.Result{}, err
	}

	// An epoch is claimed exactly once. Anything already past Pending is either
	// finished, abandoned by this process, or orphaned by a previous one --
	// never something to start again. Re-entering would re-quiesce a running
	// application and produce a RecoveryPoint from artifacts captured at
	// different points in time, which is precisely the defect this controller
	// exists to eliminate.
	if rp.Status.Phase != "" && rp.Status.Phase != rampv1alpha1.RecoveryPointPending {
		if rp.Status.RunID != "" && rp.Status.RunID != r.RunID {
			log.Info("aborting epoch interrupted by a manager restart",
				"epoch", rp.Spec.Epoch, "stage", rp.Status.LastSuccessfulStage, "ownerRunId", rp.Status.RunID)
			return r.abortInterrupted(ctx, rp, rg, "EpochInterrupted", fmt.Sprintf(
				"the manager process that owned this epoch (runId %s) is gone; it stopped after stage %s. "+
					"An interrupted epoch is never resumed: its completed stages describe an application state that is no longer held.",
				rp.Status.RunID, orStage(rp.Status.LastSuccessfulStage)))
		}
		// Our own RunID, mid-flight, and yet control is back here: the epoch's
		// reconcile returned without reaching a terminal phase (an update
		// failed, or the context was cancelled). Same verdict, same cleanup.
		log.Info("aborting abandoned epoch", "epoch", rp.Spec.Epoch, "stage", rp.Status.LastSuccessfulStage)
		return r.abortInterrupted(ctx, rp, rg, "EpochAbandoned", fmt.Sprintf(
			"the reconcile running this epoch returned at stage %s without reaching a terminal phase",
			orStage(rp.Status.LastSuccessfulStage)))
	}

	log.Info("running recovery epoch", "group", rg.Name, "epoch", rp.Spec.Epoch)
	return r.runEpoch(ctx, rp, rg)
}

// epochMember is a RecoveryGroup member resolved to a live runtime target.
type epochMember struct {
	spec     rampv1alpha1.RecoveryMember
	resolved *ResolvedComponent
}

func (r *RecoveryPointReconciler) runEpoch(ctx context.Context, rp *rampv1alpha1.RecoveryPoint,
	rg *rampv1alpha1.RecoveryGroup) (ctrl.Result, error) {

	log := logf.FromContext(ctx)
	fi := parseFaultInjection(rp)

	// ======================================================== PREPARE ======
	rp.Status.Phase = rampv1alpha1.RecoveryPointPreparing
	rp.Status.RunID = r.RunID
	rp.Status.Timings.PrepareStart = nowp()
	rp.Status.Message = "verifying prerequisites before touching the running application"
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	src, err := r.Clusters.Get(rg.Spec.AppBundleRef.Cluster)
	if err != nil {
		return r.abort(ctx, rp, nil, "ClusterNotRegistered", err.Error())
	}
	ab, err := GetAppBundle(ctx, src.Client, rg.Spec.AppBundleRef)
	if err != nil {
		return r.abort(ctx, rp, nil, "AppBundleUnavailable", err.Error())
	}

	var (
		members   []epochMember
		videoM    *epochMember
		redisM    *epochMember
		prepareOK []string
	)
	for _, m := range rg.Spec.Members {
		rc, err := ResolveComponent(ctx, src.Client, ab, m.ComponentRef)
		if err != nil {
			return r.abort(ctx, rp, nil, "MemberUnresolved", fmt.Sprintf("member %q: %v", m.Name, err))
		}
		members = append(members, epochMember{spec: m, resolved: rc})
	}
	for i := range members {
		switch members[i].spec.RecoveryDriver {
		case rampv1alpha1.DriverContainerCheckpoint:
			videoM = &members[i]
		case rampv1alpha1.DriverRedisReplication:
			redisM = &members[i]
		}
	}
	if videoM == nil {
		return r.abort(ctx, rp, nil, "NoCheckpointMember",
			"RecoveryGroup has no container-checkpoint member to quiesce and checkpoint")
	}
	if redisM == nil {
		return r.abort(ctx, rp, nil, "NoConsistencyAnchor",
			"RecoveryGroup has no redis-replication member to anchor the epoch barrier to")
	}
	if len(redisM.spec.ConsistencyKeys) == 0 {
		return r.abort(ctx, rp, nil, "NoConsistencyKey",
			"the redis-replication member declares no consistencyKeys, so no logical position can be pinned")
	}
	positionKey := redisM.spec.ConsistencyKeys[0]

	// PREPARE must fail BEFORE quiescing if anything the epoch depends on is
	// missing. Stopping a running application and only then discovering that
	// the artifact store is unreachable is a pointless outage.
	rd := &drivers.RedisReplication{}
	pre, err := rd.Capture(redisM.spec.SourceEndpoint, redisM.spec.ConsistencyKeys)
	if err != nil {
		return r.abort(ctx, rp, nil, "RedisPrimaryUnreachable", err.Error())
	}
	prepareOK = append(prepareOK, fmt.Sprintf("redis primary %s role=%s connectedReplicas=%d",
		pre.Endpoint, pre.Role, pre.ConnectedReplicas))

	if _, _, err := r.Store.Stat(ctx, "ramp-store-probe-nonexistent"); err != nil {
		return r.abort(ctx, rp, nil, "ArtifactStoreUnreachable", err.Error())
	}
	prepareOK = append(prepareOK, fmt.Sprintf("artifact store bucket %s reachable", r.Store.Bucket()))

	if _, err := readMemberState(ctx, src, videoM.resolved.Namespace, videoM.resolved.PodName, videoM.spec.Container); err != nil {
		return r.abort(ctx, rp, nil, "ApplicationUnreadable",
			fmt.Sprintf("member %q: %v", videoM.spec.Name, err))
	}
	prepareOK = append(prepareOK, fmt.Sprintf("checkpoint member %s/%s reachable on node %s",
		videoM.resolved.Namespace, videoM.resolved.PodName, videoM.resolved.NodeName))

	setCondition(&rp.Status.Conditions, "Prepared", metav1.ConditionTrue, "PrerequisitesMet",
		joinLines(prepareOK), rp.Generation)
	rp.Status.LastSuccessfulStage = rampv1alpha1.RecoveryPointPreparing

	// ======================================================== QUIESCE ======
	// From here on the source application is paused, so EVERY exit path must
	// resume it. That is what the defer is for -- commit, abort, validation
	// failure, panic and context cancellation all go through it.
	rp.Status.Phase = rampv1alpha1.RecoveryPointQuiescing
	rp.Status.Timings.QuiesceStart = nowp()
	rp.Status.Message = "quiescing the application"
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	barrierTimeout := time.Duration(orDefaultInt32(rp.Spec.BarrierTimeoutSeconds, 30)) * time.Second
	verifyInterval := time.Duration(orDefaultInt32(rp.Spec.QuiesceVerifySeconds, 3)) * time.Second

	var resumed bool
	resume := func() {
		if resumed || fi.SkipQuiesce {
			return
		}
		resumed = true
		if err := resumeMember(src, videoM.resolved.Namespace, videoM.resolved.PodName, videoM.spec.Container); err != nil {
			log.Error(err, "FAILED TO RESUME the source application", "epoch", rp.Spec.Epoch)
			rp.Status.Quiesce.Message = fmt.Sprintf("resume failed: %v", err)
			setCondition(&rp.Status.Conditions, "ApplicationResumed", metav1.ConditionFalse,
				"ResumeFailed", err.Error(), rp.Generation)
			return
		}
		rp.Status.Quiesce.Quiesced = false
		rp.Status.Quiesce.ResumedAt = nowp()
		rp.Status.Timings.ResumeTime = rp.Status.Quiesce.ResumedAt
		setCondition(&rp.Status.Conditions, "ApplicationResumed", metav1.ConditionTrue,
			"Resumed", "the source application was released after the epoch finished", rp.Generation)
	}
	defer resume()

	var P int64
	var qState videoState
	if fi.SkipQuiesce {
		// Reproduces the pre-fix behaviour on demand: sample the position of a
		// still-running application. Kept so the defect can be demonstrated
		// against the same controller, not so it can be used.
		st, err := readMemberState(ctx, src, videoM.resolved.Namespace, videoM.resolved.PodName, videoM.spec.Container)
		if err != nil {
			return r.abort(ctx, rp, resume, "ApplicationUnreadable", err.Error())
		}
		P, qState = st.Position, st
		rp.Status.Quiesce = rampv1alpha1.QuiesceStatus{
			Quiesced: false, Position: P, InstanceFingerprint: st.Session,
			Message: "test fault injection (skipQuiesce): application was NOT paused (pre-fix behaviour)",
		}
	} else {
		st, err := quiesceMember(ctx, src, videoM.resolved.Namespace, videoM.resolved.PodName,
			videoM.spec.Container, rp.Spec.Epoch, barrierTimeout)
		if err != nil {
			return r.abort(ctx, rp, resume, "QuiesceFailed", err.Error())
		}
		pos, samples, vst, err := verifyQuiesced(ctx, src, videoM.resolved.Namespace,
			videoM.resolved.PodName, videoM.spec.Container, verifyInterval)
		if err != nil {
			return r.abort(ctx, rp, resume, "QuiesceNotStable", err.Error())
		}
		P, qState = pos, vst
		_ = st
		rp.Status.Quiesce = rampv1alpha1.QuiesceStatus{
			Quiesced: true, QuiescedAt: nowp(), Position: P,
			VerifySamples: samples, InstanceFingerprint: vst.Session,
			Message: fmt.Sprintf("position held at %d across %d samples %s apart", P, len(samples), verifyInterval),
		}
	}
	rp.Status.LogicalPosition = P
	rp.Status.Timings.QuiesceComplete = nowp()
	rp.Status.LastSuccessfulStage = rampv1alpha1.RecoveryPointQuiescing
	setCondition(&rp.Status.Conditions, "ApplicationQuiesced",
		boolCondition(rp.Status.Quiesce.Quiesced), "QuiesceStage",
		rp.Status.Quiesce.Message, rp.Generation)
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("application quiesced", "epoch", rp.Spec.Epoch, "position", P, "instance", qState.Session)

	// ======================================================== BARRIER ======
	rp.Status.Phase = rampv1alpha1.RecoveryPointBarrierEstablished
	rp.Status.Message = "establishing the Redis replication barrier for this epoch"

	acks := int(orDefaultInt32(rp.Spec.RequiredReplicaAcks, 1))
	bar, barErr := rd.Barrier(redisM.spec.SourceEndpoint, rp.Spec.Epoch, P, acks, barrierTimeout)
	if bar != nil {
		rp.Status.Barrier = rampv1alpha1.BarrierStatus{
			Established: barErr == nil, Endpoint: bar.Endpoint, MarkerOffset: bar.MarkerOffset,
			ReplicationID: bar.ReplicationID, AckedReplicas: bar.AckedReplicas,
			RequiredReplicas: bar.Required, Position: bar.Position,
		}
		if barErr == nil {
			t := metav1.NewTime(bar.At)
			rp.Status.Barrier.EstablishedAt = &t
			rp.Status.Barrier.Message = fmt.Sprintf(
				"WAIT %d acknowledged by %d replica(s) at replication offset %d (stream %s)",
				acks, bar.AckedReplicas, bar.MarkerOffset, short(bar.ReplicationID))
		}
	}
	if barErr != nil {
		setCondition(&rp.Status.Conditions, "RedisBarrierEstablished", metav1.ConditionFalse,
			"ReplicaAckNotAchieved", barErr.Error(), rp.Generation)
		return r.abort(ctx, rp, resume, "ReplicaAckNotAchieved", barErr.Error())
	}
	rp.Status.Timings.BarrierReached = nowp()
	rp.Status.LastSuccessfulStage = rampv1alpha1.RecoveryPointBarrierEstablished
	setCondition(&rp.Status.Conditions, "RedisBarrierEstablished", metav1.ConditionTrue,
		"ReplicaAcknowledged", rp.Status.Barrier.Message, rp.Generation)
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("replication barrier acknowledged", "epoch", rp.Spec.Epoch,
		"position", P, "offset", bar.MarkerOffset, "ackedReplicas", bar.AckedReplicas)

	// =================================================== CAPTURE REDIS =====
	// The epoch's Redis recovery state is an RDB taken while the writer is
	// quiesced -- an object that cannot change afterwards. The live replica
	// stays exactly as it is: it remains the warm-standby readiness mechanism.
	rp.Status.Phase = rampv1alpha1.RecoveryPointCapturingRedis
	rp.Status.Timings.CaptureStart = nowp()
	rp.Status.Timings.RedisSnapshotStart = nowp()
	rp.Status.Message = "capturing the epoch-specific Redis snapshot"
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	captureTimeout := time.Duration(orDefaultInt32(rp.Spec.CaptureTimeoutSeconds, 300)) * time.Second
	var artifactsOut []rampv1alpha1.RecoveryArtifact

	if fi.FailRedisSnapshot {
		return r.abort(ctx, rp, resume, "RedisSnapshotFailed",
			"test fault injection (failRedisSnapshot): epoch-specific Redis capture forced to fail")
	}

	snapCtx, cancelSnap := context.WithTimeout(ctx, captureTimeout)
	snap, err := rd.CaptureSnapshot(snapCtx, src, redisM.resolved.Namespace, redisM.resolved.PodName,
		containerOrDefault(redisM.spec.Container, "redis"), positionKey, rp.Spec.Epoch)
	cancelSnap()
	if err != nil {
		return r.abort(ctx, rp, resume, "RedisSnapshotFailed", err.Error())
	}
	if snap.PositionBefore != P || snap.PositionAfter != P {
		return r.abort(ctx, rp, resume, "RedisSnapshotPositionDrift", fmt.Sprintf(
			"the Redis dataset moved while being snapshotted: quiesced position %d, redis position %d before / %d after SAVE",
			P, snap.PositionBefore, snap.PositionAfter))
	}
	if snap.EpochMarker != P {
		return r.abort(ctx, rp, resume, "RedisEpochMarkerMissing", fmt.Sprintf(
			"epoch marker %s in the snapshotted dataset is %d, expected %d",
			drivers.EpochPositionKey(rp.Spec.Epoch), snap.EpochMarker, P))
	}

	redisKey := fmt.Sprintf("ramp-redis-epoch/%s-epoch-%d.rdb", rg.Name, rp.Spec.Epoch)
	digest, err := r.Store.Put(ctx, redisKey, snap.Data, "application/octet-stream")
	if err != nil {
		return r.abort(ctx, rp, resume, "RedisArtifactUploadFailed", err.Error())
	}
	rp.Status.Timings.RedisSnapshotComplete = nowp()

	redisPos := P
	if fi.ForcePositionSkew != 0 {
		redisPos = P + fi.ForcePositionSkew
	}
	artifactsOut = append(artifactsOut, rampv1alpha1.RecoveryArtifact{
		Member: redisM.spec.Name, Driver: redisM.spec.RecoveryDriver,
		Type: rampv1alpha1.ArtifactRedisSnapshot, Epoch: rp.Spec.Epoch,
		Ref:       r.Store.Ref(redisKey),
		Immutable: true, Checksum: digest, ChecksumAlgorithm: "sha256",
		SizeBytes:         int64(len(snap.Data)),
		ReplicationOffset: bar.MarkerOffset, ReplicationID: bar.ReplicationID,
		LogicalPosition: redisPos,
		CapturedAt:      &metav1.Time{Time: snap.Completed},
		Message: fmt.Sprintf("synchronous SAVE on the quiesced primary %s/%s; %s",
			redisM.resolved.Namespace, snap.Pod, snap.Raw),
	})
	// The live replication stream is recorded too, but explicitly as a
	// diagnostic: it is what made the target warm, not what the epoch restores.
	post, err := rd.Capture(redisM.spec.SourceEndpoint, redisM.spec.ConsistencyKeys)
	if err == nil {
		artifactsOut = append(artifactsOut, rampv1alpha1.RecoveryArtifact{
			Member: redisM.spec.Name, Driver: redisM.spec.RecoveryDriver,
			Type: rampv1alpha1.ArtifactReplicationState, Epoch: rp.Spec.Epoch,
			Ref:               fmt.Sprintf("redis-repl://%s/%d", post.Endpoint, post.ReplicationOffset),
			Immutable:         false,
			ReplicationOffset: post.ReplicationOffset, ReplicationID: post.ReplicationID,
			LogicalPosition: post.LogicalPosition,
			CapturedAt:      &metav1.Time{Time: post.CapturedAt},
			Message: "DIAGNOSTIC ONLY: the live stream keeps advancing after commit and " +
				"cannot identify this epoch; recovery uses the redisSnapshot artifact",
		})
	}
	rp.Status.Artifacts = artifactsOut
	rp.Status.LastSuccessfulStage = rampv1alpha1.RecoveryPointCapturingRedis
	setCondition(&rp.Status.Conditions, "RedisStateCaptured", metav1.ConditionTrue, "SnapshotUploaded",
		fmt.Sprintf("immutable RDB %s (%d bytes, sha256 %s) frozen at position %d",
			r.Store.Ref(redisKey), len(snap.Data), short(digest), P), rp.Generation)
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("redis epoch artifact created", "epoch", rp.Spec.Epoch, "ref", r.Store.Ref(redisKey),
		"bytes", len(snap.Data), "position", P)

	// =================================================== CAPTURE VIDEO =====
	rp.Status.Phase = rampv1alpha1.RecoveryPointCapturingVideo
	rp.Status.Timings.VideoCheckpointStart = nowp()
	rp.Status.Message = "checkpointing the quiesced application container"
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	if fi.FailVideoCheckpoint {
		return r.abort(ctx, rp, resume, "CheckpointFailed",
			"test fault injection (failVideoCheckpoint): container checkpoint forced to fail AFTER the Redis epoch artifact was created")
	}

	preCkpt, err := readMemberState(ctx, src, videoM.resolved.Namespace, videoM.resolved.PodName, videoM.spec.Container)
	if err != nil {
		return r.abort(ctx, rp, resume, "ApplicationUnreadable", err.Error())
	}
	if !fi.SkipQuiesce && (!preCkpt.Quiesced || preCkpt.Position != P) {
		return r.abort(ctx, rp, resume, "QuiesceLost", fmt.Sprintf(
			"application is quiesced=%t at position %d immediately before the checkpoint, expected quiesced=true at %d",
			preCkpt.Quiesced, preCkpt.Position, P))
	}

	cd := &drivers.ContainerCheckpoint{RestConfig: src.RestConfig}
	res, err := cd.Capture(ctx, videoM.resolved.NodeName, videoM.resolved.Namespace,
		videoM.resolved.PodName, videoM.spec.Container)
	if err != nil {
		return r.abort(ctx, rp, resume, "CheckpointFailed", fmt.Sprintf("member %q: %v", videoM.spec.Name, err))
	}
	oi, err := r.Store.WaitFor(ctx, res.ObjectKey, captureTimeout, 2*time.Second)
	if err != nil {
		return r.abort(ctx, rp, resume, "ArtifactNotLanded", fmt.Sprintf("member %q: %v", videoM.spec.Name, err))
	}

	// The position the CHECKPOINT carries is the position the application was
	// held at, re-read from the still-quiesced process. Reading it after the
	// dump rather than before is what makes it a statement about the artifact.
	postCkpt, err := readMemberState(ctx, src, videoM.resolved.Namespace, videoM.resolved.PodName, videoM.spec.Container)
	if err != nil {
		return r.abort(ctx, rp, resume, "ApplicationUnreadable", err.Error())
	}

	artifactsOut = append(artifactsOut, rampv1alpha1.RecoveryArtifact{
		Member: videoM.spec.Name, Driver: videoM.spec.RecoveryDriver,
		Type: rampv1alpha1.ArtifactContainerCheckpoint, Epoch: rp.Spec.Epoch,
		Ref:       r.Store.Ref(res.ObjectKey),
		Immutable: true, Checksum: oi.ETag, ChecksumAlgorithm: "etag-md5",
		NodePath: res.NodePath, NodeName: res.Node, SizeBytes: oi.SizeBytes,
		LogicalPosition: postCkpt.Position, InstanceFingerprint: postCkpt.Session,
		CapturedAt: &metav1.Time{Time: res.Completed},
		Message: fmt.Sprintf("kubelet checkpoint took %s while quiesced=%t; instance %s",
			res.Completed.Sub(res.Started).Round(time.Millisecond), postCkpt.Quiesced, postCkpt.Session),
	})
	rp.Status.Artifacts = artifactsOut
	rp.Status.Timings.VideoCheckpointComplete = nowp()
	rp.Status.Timings.CaptureComplete = nowp()
	rp.Status.LastSuccessfulStage = rampv1alpha1.RecoveryPointCapturingVideo
	setCondition(&rp.Status.Conditions, "VideoStateCaptured", metav1.ConditionTrue, "CheckpointLanded",
		fmt.Sprintf("immutable checkpoint %s (%d bytes) at position %d",
			r.Store.Ref(res.ObjectKey), oi.SizeBytes, postCkpt.Position), rp.Generation)

	// ======================================================= VALIDATE ======
	rp.Status.Phase = rampv1alpha1.RecoveryPointValidating
	if err := r.Status().Update(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}

	v := r.validate(ctx, rp, rg, P, postCkpt, bar)
	rp.Status.Validation = v
	rp.Status.Timings.ValidateComplete = nowp()

	if !v.Validated {
		setCondition(&rp.Status.Conditions, "Validated", metav1.ConditionFalse, v.Reason, v.Message, rp.Generation)
		return r.abort(ctx, rp, resume, v.Reason, v.Message)
	}
	rp.Status.LastSuccessfulStage = rampv1alpha1.RecoveryPointValidating
	setCondition(&rp.Status.Conditions, "Validated", metav1.ConditionTrue, v.Reason, v.Message, rp.Generation)

	// ========================================================= COMMIT ======
	rp.Status.Phase = rampv1alpha1.RecoveryPointCommitted
	rp.Status.Timings.CommitTime = nowp()
	rp.Status.LastSuccessfulStage = rampv1alpha1.RecoveryPointCommitted
	rp.Status.Message = fmt.Sprintf(
		"epoch %d committed at logical position %d with %d immutable artifacts",
		rp.Spec.Epoch, P, countImmutable(artifactsOut))
	setCondition(&rp.Status.Conditions, "Committed", metav1.ConditionTrue, "EpochCommitted", rp.Status.Message, rp.Generation)

	// ========================================================= RESUME ======
	// Before the status is written, so that the committed object already shows
	// the application released rather than a transient "still quiesced".
	resume()

	log.Info("recovery point committed", "epoch", rp.Spec.Epoch, "position", P,
		"redisSnapshotPosition", v.RedisSnapshotPosition, "checkpointPosition", v.CheckpointPosition)
	return ctrl.Result{}, r.Status().Update(ctx, rp)
}

// validate is the gate between "artifacts exist" and "a RecoveryPoint exists".
// Every check is mandatory and recorded, so a refused commit explains itself.
func (r *RecoveryPointReconciler) validate(ctx context.Context, rp *rampv1alpha1.RecoveryPoint,
	rg *rampv1alpha1.RecoveryGroup, P int64, ckpt videoState, bar *drivers.BarrierResult) rampv1alpha1.ValidationResult {

	v := rampv1alpha1.ValidationResult{CheckpointPosition: ckpt.Position, ReplicatedPosition: -1, RedisSnapshotPosition: -1}
	add := func(name string, ok bool, detail string) bool {
		v.Checks = append(v.Checks, rampv1alpha1.ValidationCheck{Name: name, Passed: ok, Detail: detail})
		return ok
	}

	var redisArt, videoArt *rampv1alpha1.RecoveryArtifact
	for i := range rp.Status.Artifacts {
		a := &rp.Status.Artifacts[i]
		switch a.Type {
		case rampv1alpha1.ArtifactRedisSnapshot:
			redisArt = a
			v.RedisSnapshotPosition = a.LogicalPosition
		case rampv1alpha1.ArtifactContainerCheckpoint:
			videoArt = a
		case rampv1alpha1.ArtifactReplicationState:
			v.ReplicatedPosition = a.LogicalPosition
		}
	}

	quiesceOK := add("ApplicationQuiescedAtP",
		rp.Status.Quiesce.Quiesced && rp.Status.Quiesce.Position == P,
		fmt.Sprintf("quiesced=%t at position %d, samples %v",
			rp.Status.Quiesce.Quiesced, rp.Status.Quiesce.Position, rp.Status.Quiesce.VerifySamples))

	barrierDetail := "no barrier was established"
	if bar != nil {
		barrierDetail = fmt.Sprintf("WAIT %d -> %d acks at offset %d for position %d",
			bar.Required, bar.AckedReplicas, bar.MarkerOffset, bar.Position)
	}
	barrierOK := add("RedisBarrierAcknowledgedP",
		bar != nil && bar.AckedReplicas >= bar.Required && bar.Position == P, barrierDetail)

	redisExists := add("RedisArtifactExists", redisArt != nil && redisArt.Immutable && redisArt.Ref != "",
		refOrNone(redisArt))
	videoExists := add("VideoArtifactExists", videoArt != nil && videoArt.Immutable && videoArt.Ref != "",
		refOrNone(videoArt))

	redisPresent := true
	if redisArt != nil {
		key := storeKey(r.Store, redisArt.Ref)
		if _, ok, err := r.Store.Stat(ctx, key); err != nil || !ok {
			redisPresent = false
			add("RedisArtifactReadable", false, fmt.Sprintf("%s: present=%t err=%v", key, ok, err))
		} else {
			add("RedisArtifactReadable", true, fmt.Sprintf("%s present in %s", key, r.Store.Bucket()))
		}
	} else {
		redisPresent = add("RedisArtifactReadable", false, "no redisSnapshot artifact")
	}

	sameEpoch := true
	for _, a := range rp.Status.Artifacts {
		if a.Epoch != rp.Spec.Epoch {
			sameEpoch = false
		}
	}
	add("ArtifactsShareEpoch", sameEpoch, fmt.Sprintf("all artifacts stamped epoch=%d", rp.Spec.Epoch))

	ckptPosOK := add("VideoArtifactPositionIsP", videoArt != nil && videoArt.LogicalPosition == P,
		fmt.Sprintf("checkpoint metadata logical position %d vs epoch position %d", ckpt.Position, P))

	skew := int64(0)
	redisPosOK := false
	if redisArt != nil {
		skew = redisArt.LogicalPosition - P
		if skew < 0 {
			skew = -skew
		}
		v.ObservedSkew = skew
		redisPosOK = add("RedisArtifactPositionIsP", redisArt.LogicalPosition == P,
			fmt.Sprintf("redis snapshot logical position %d vs epoch position %d", redisArt.LogicalPosition, P))
	} else {
		redisPosOK = add("RedisArtifactPositionIsP", false, "no redisSnapshot artifact")
	}

	crossOK := add("CrossMemberSkewWithinTolerance", skew <= rp.Spec.MaxPositionSkew,
		fmt.Sprintf("observed skew %d, tolerance %d", skew, rp.Spec.MaxPositionSkew))

	membersOK := add("EveryMemberRepresented", countImmutable(rp.Status.Artifacts) >= len(rg.Spec.Members),
		fmt.Sprintf("%d immutable artifacts for %d members", countImmutable(rp.Status.Artifacts), len(rg.Spec.Members)))

	v.Validated = quiesceOK && barrierOK && redisExists && videoExists && redisPresent &&
		sameEpoch && ckptPosOK && redisPosOK && crossOK && membersOK

	if v.Validated {
		v.Reason = "ConsistentAcrossDrivers"
		v.Message = fmt.Sprintf(
			"video checkpoint and immutable redis snapshot both belong to epoch %d at logical position %d (skew %d)",
			rp.Spec.Epoch, P, skew)
		return v
	}
	var failed []string
	for _, c := range v.Checks {
		if !c.Passed {
			failed = append(failed, fmt.Sprintf("%s (%s)", c.Name, c.Detail))
		}
	}
	v.Reason = "ValidationFailed"
	v.Message = "epoch is not a restorable recovery point: " + joinLines(failed)
	return v
}

// abort is the single failure exit. It always releases the application first.
func (r *RecoveryPointReconciler) abort(ctx context.Context, rp *rampv1alpha1.RecoveryPoint,
	resume func(), reason, msg string) (ctrl.Result, error) {

	rp.Status.Phase = rampv1alpha1.RecoveryPointAborting
	rp.Status.FailureReason = reason
	rp.Status.Message = msg
	rp.Status.Validation.Validated = false
	if rp.Status.Validation.Reason == "" {
		rp.Status.Validation.Reason = reason
	}
	if resume != nil {
		resume()
	}
	// Any artifact this epoch already uploaded stays in the store, deliberately
	// unreferenced by any committed RecoveryPoint. It is an orphan, not a
	// recovery point, and the failed object on record says why.
	rp.Status.Phase = rampv1alpha1.RecoveryPointFailed
	setCondition(&rp.Status.Conditions, "Committed", metav1.ConditionFalse, reason,
		fmt.Sprintf("epoch aborted after stage %s: %s", orStage(rp.Status.LastSuccessfulStage), msg), rp.Generation)
	return ctrl.Result{}, r.Status().Update(ctx, rp)
}

// abortInterrupted handles an epoch that will never be completed, releasing any
// application it may have left paused.
//
// The ApplicationResumed condition reports what was actually established, not
// what was attempted. Three outcomes are genuinely different and were
// previously collapsed into an unconditional True:
//
//	True     the quiesce was released and the application confirmed running
//	False    a member was found and the release failed
//	Unknown  no member could be resolved, so nothing can be said either way
//
// Publishing True for the third case is worse than publishing nothing: it tells
// an operator the cleanup succeeded on a workload the controller never reached,
// and a source application left paused is exactly the failure this condition
// exists to make visible.
func (r *RecoveryPointReconciler) abortInterrupted(ctx context.Context, rp *rampv1alpha1.RecoveryPoint,
	rg *rampv1alpha1.RecoveryGroup, reason, msg string) (ctrl.Result, error) {

	log := logf.FromContext(ctx)

	var (
		attempted  int
		released   int
		unresolved []string
		failures   []string
	)
	src, clusterErr := r.Clusters.Get(rg.Spec.AppBundleRef.Cluster)
	var ab *unstructured.Unstructured
	var abErr error
	if clusterErr == nil {
		ab, abErr = GetAppBundle(ctx, src.Client, rg.Spec.AppBundleRef)
	}
	for _, m := range rg.Spec.Members {
		if m.RecoveryDriver != rampv1alpha1.DriverContainerCheckpoint {
			continue
		}
		attempted++
		switch {
		case clusterErr != nil:
			unresolved = append(unresolved, fmt.Sprintf("%s (cluster %s: %v)", m.Name, rg.Spec.AppBundleRef.Cluster, clusterErr))
			continue
		case abErr != nil:
			unresolved = append(unresolved, fmt.Sprintf("%s (AppBundle: %v)", m.Name, abErr))
			continue
		}
		rc, rerr := ResolveComponent(ctx, src.Client, ab, m.ComponentRef)
		if rerr != nil {
			unresolved = append(unresolved, fmt.Sprintf("%s (%v)", m.Name, rerr))
			continue
		}
		if err := resumeMember(src, rc.Namespace, rc.PodName, m.Container); err != nil {
			log.Error(err, "could not release the application after an interrupted epoch", "member", m.Name)
			failures = append(failures, fmt.Sprintf("%s (%v)", m.Name, err))
			continue
		}
		// Released -- and confirm it, rather than assuming the write landed.
		if st, err := readMemberState(ctx, src, rc.Namespace, rc.PodName, m.Container); err != nil {
			unresolved = append(unresolved, fmt.Sprintf("%s (released, but state unreadable: %v)", m.Name, err))
		} else if st.Quiesced {
			failures = append(failures, fmt.Sprintf("%s (still reports quiesced=true at position %d)", m.Name, st.Position))
		} else {
			released++
		}
	}

	switch {
	case attempted > 0 && released == attempted:
		setCondition(&rp.Status.Conditions, "ApplicationResumed", metav1.ConditionTrue, "ResumedAfterAbort",
			fmt.Sprintf("%d/%d checkpoint member(s) confirmed running after the epoch was abandoned", released, attempted),
			rp.Generation)
		rp.Status.Quiesce.Quiesced = false
		rp.Status.Quiesce.ResumedAt = nowp()
		rp.Status.Timings.ResumeTime = rp.Status.Quiesce.ResumedAt
	case len(failures) > 0:
		setCondition(&rp.Status.Conditions, "ApplicationResumed", metav1.ConditionFalse, "ResumeFailed",
			"the application could not be released: "+joinLines(failures), rp.Generation)
		rp.Status.Quiesce.Message = "resume failed: " + joinLines(failures)
	case len(unresolved) > 0:
		// Nothing was reached, so nothing is known. Do NOT claim success.
		setCondition(&rp.Status.Conditions, "ApplicationResumed", metav1.ConditionUnknown, "ResumeUnverified",
			"the checkpoint member could not be resolved, so whether it is still quiesced is unknown: "+
				joinLines(unresolved)+". If the source is paused it has to be released by hand.", rp.Generation)
		rp.Status.Quiesce.Message = "resume unverified: " + joinLines(unresolved)
	default:
		setCondition(&rp.Status.Conditions, "ApplicationResumed", metav1.ConditionUnknown, "NoCheckpointMember",
			"the RecoveryGroup has no container-checkpoint member, so no application was paused by this epoch",
			rp.Generation)
	}
	return r.abort(ctx, rp, nil, reason, msg)
}

// readMemberState reads the container-checkpoint member's in-memory state --
// its logical position, quiesce flag and the identity of this process instance
// -- from the state file the application maintains. It goes through the
// apiserver exec subresource because, as with the kubelet checkpoint call, the
// management plane has no route to the pod network.
func readMemberState(ctx context.Context, c *clusters.Cluster, namespace, pod, container string) (videoState, error) {
	out, err := clusters.Exec(ctx, c, namespace, pod, container, []string{"sh", "-c", "cat " + statePath})
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

func containerOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func nowp() *metav1.Time { t := metav1.Now(); return &t }

func countImmutable(as []rampv1alpha1.RecoveryArtifact) int {
	n := 0
	for _, a := range as {
		if a.Immutable {
			n++
		}
	}
	return n
}

func refOrNone(a *rampv1alpha1.RecoveryArtifact) string {
	if a == nil {
		return "absent"
	}
	return fmt.Sprintf("%s immutable=%t checksum=%s", a.Ref, a.Immutable, short(a.Checksum))
}

func orStage(p rampv1alpha1.RecoveryPointPhase) string {
	if p == "" {
		return "Pending"
	}
	return string(p)
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// storeKey turns a minio:// artifact ref back into a bucket key.
func storeKey(s *artifacts.Store, ref string) string {
	prefix := fmt.Sprintf("minio://%s/", s.Bucket())
	if len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
		return ref[len(prefix):]
	}
	return ref
}

// NewRunID returns a fresh manager-process identity.
func NewRunID() string { return uuid.NewString() }

func (r *RecoveryPointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RunID == "" {
		r.RunID = NewRunID()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&rampv1alpha1.RecoveryPoint{}).
		Named("recoverypoint").
		Complete(r)
}
