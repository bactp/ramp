package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
)

// RecoveryPathReconciler is the Readiness Controller:
//
//	OBSERVE -> evaluate the PREPARED recovery point against the RecoveryContract
//	        -> promote a candidate once its preparation is complete
//	        -> publish readiness
//
// It never checkpoints, never restores and never promotes a replica. The one
// thing it does decide is which committed RecoveryPoint this path is currently
// able to execute -- and that is deliberately NOT "the newest one".
//
// THE DEFECT THIS REPLACES. The previous version evaluated
// RecoveryGroup.status.latestRecoveryPoint. The moment a new epoch committed,
// the path started measuring itself against a RecoveryPoint whose artifacts
// were not on the target yet, and dropped from HOT to WARM -- while the
// previous point was still fully executable. Readiness was spent because newer
// state existed, not because anything was lost. Preparing a new epoch must not
// cost the readiness the old one already bought.
type RecoveryPathReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clusters *clusters.Registry
	Store    *artifacts.Store

	// StageProbeImage is the image used by the node-pinned probe that proves an
	// artifact and its checkpoint image really are on the placement node.
	StageProbeImage string
}

// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypaths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypaths/status,verbs=get;update;patch

const recoveryPathResyncInterval = 10 * time.Second

// targetContext is everything about the target that does NOT depend on which
// RecoveryPoint is being evaluated. It is computed once per reconcile and
// shared by the prepared-point evaluation and the candidate evaluation.
type targetContext struct {
	cluster   *clusters.Cluster
	err       error
	placement *rampv1alpha1.TargetPlacement
	nodes     []nodeCandidate
	nsActive  bool
	nsMessage string
	nsReason  string
	redisOK   bool
	redisRsn  string
	redisMsg  string
}

func (r *RecoveryPathReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	path := &rampv1alpha1.RecoveryPath{}
	if err := r.Get(ctx, req.NamespacedName, path); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	rg := &rampv1alpha1.RecoveryGroup{}
	rgErr := r.Get(ctx, types.NamespacedName{Namespace: path.Namespace, Name: path.Spec.RecoveryGroupRef}, rg)

	// ---- the committed, validated RecoveryPoints this path may consider -----
	eligible, err := r.eligiblePoints(ctx, path, rg, rgErr)
	if err != nil {
		return ctrl.Result{}, err
	}
	var latest *rampv1alpha1.RecoveryPoint
	if len(eligible) > 0 {
		latest = &eligible[0]
	}

	// ---- target facts that do not depend on the RecoveryPoint --------------
	tc := r.targetFacts(ctx, path, rg, rgErr)

	// ---- what is currently prepared, re-verified from scratch --------------
	// The previously promoted point is re-checked every reconcile rather than
	// trusted: artifacts can be deleted and nodes can go away, and a path that
	// keeps claiming HOT from a status field it wrote earlier is exactly the
	// false readiness this controller exists to prevent.
	prepared := r.resolvePrepared(ctx, path, eligible)

	var preparedEval *pointEvaluation
	if prepared != nil {
		e := r.evaluatePoint(ctx, path, tc, prepared)
		preparedEval = &e
	}

	// ---- candidate selection and promotion ---------------------------------
	//
	//   observe a newer committed RP -> candidate
	//   evaluate its target-side preparation
	//   all of it holds             -> atomic promotion to prepared
	//
	// The prepared point is never cleared to make room for a candidate. It is
	// replaced, in one status write, only once the candidate is proven
	// executable in its own right.
	var candidate *rampv1alpha1.RecoveryPoint
	var candidateEval *pointEvaluation
	switch {
	case latest == nil:
		// nothing to do
	case prepared == nil:
		candidate = latest
	case latest.Spec.Epoch > prepared.Spec.Epoch:
		candidate = latest
	}
	if candidate != nil {
		e := r.evaluatePoint(ctx, path, tc, candidate)
		candidateEval = &e
		if e.preparationComplete() {
			log.Info("promoting candidate RecoveryPoint to prepared",
				"path", path.Name, "candidate", candidate.Name, "epoch", candidate.Spec.Epoch,
				"previouslyPrepared", nameOrNone(prepared))
			prepared = candidate
			preparedEval = candidateEval
			candidate, candidateEval = nil, nil
		}
	}

	// One cleanup pass per reconcile, keeping every probe this pass actually
	// used. Retiring inside the probe helper made the prepared and candidate
	// evaluations delete each other's pods.
	if tc.cluster != nil {
		keep := map[string]bool{}
		for _, e := range []*pointEvaluation{preparedEval, candidateEval} {
			if e == nil {
				continue
			}
			for _, n := range e.probeKeep {
				keep[n] = true
			}
		}
		r.retireStaleProbes(ctx, tc.cluster, path.Spec.TargetPrereqs.Namespace, keep)
	}

	// ---- publish -----------------------------------------------------------
	//
	// Only when something an operator would act on has changed, or the
	// heartbeat is due.
	//
	// Writing unconditionally is what a readiness controller most obviously
	// wants to do, and it produces a hot loop: the status carries a handful of
	// fields that tick on their own (lastValidatedTime, every check's
	// lastProbeTime, the placement's selectedAt, the prepared point's age), so
	// every pass differs from the last, every pass writes, every write fires
	// the watch, and the watch drives the next pass. Measured on this testbed
	// before the fix: ~1 reconcile per second against a 10 s resync, ~10 status
	// writes per second, and ~11 conflict errors per minute -- each pass also
	// listing every node and pod on the target cluster and stat-ing two MinIO
	// objects. The published values were correct the whole time, which is
	// exactly why it went unnoticed.
	prev := path.Status.DeepCopy()
	status := r.buildStatus(path, rg, rgErr, tc, latest, candidate, candidateEval, prepared, preparedEval)

	unchanged := semanticStatus(&status) == semanticStatus(prev)
	fresh := prev.LastValidatedTime != nil && time.Since(prev.LastValidatedTime.Time) < statusHeartbeat
	if unchanged && fresh {
		return ctrl.Result{RequeueAfter: recoveryPathResyncInterval}, nil
	}

	path.Status = status
	if err := r.Status().Update(ctx, path); err != nil {
		// A conflict means this reconcile read a cached object that another
		// write has already superseded. It is expected, self-healing and not a
		// fault: requeue quietly instead of logging it as an error.
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("updating RecoveryPath status: %w", err)
	}
	log.V(1).Info("evaluated recovery path",
		"readiness", status.Readiness, "prepared", refName(status.PreparedRecoveryPoint),
		"candidate", candName(status.CandidateRecoveryPoint), "unmet", status.UnmetMandatoryChecks)
	return ctrl.Result{RequeueAfter: recoveryPathResyncInterval}, nil
}

// statusHeartbeat bounds how long the published status may go unrefreshed while
// nothing changes. It is the staleness of the DISPLAYED recoveryPointAge only:
// the contract is still evaluated every resync, so a freshness or RTO verdict
// that flips changes the semantic status and is published immediately.
const statusHeartbeat = 30 * time.Second

// semanticStatus is the decision-relevant projection of the status: everything
// an operator or another controller would act on, and none of the fields that
// advance on their own. Timestamps, the prepared point's age and the remaining
// freshness are deliberately excluded -- they are how the status describes the
// verdict, not the verdict.
func semanticStatus(s *rampv1alpha1.RecoveryPathStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "readiness=%s;unmet=%s;", s.Readiness, strings.Join(s.UnmetMandatoryChecks, ","))
	fmt.Fprintf(&b, "latest=%s;candidate=%s;", refOrEmpty(s.LatestRecoveryPoint), candName(s.CandidateRecoveryPoint))
	if p := s.PreparedRecoveryPoint; p != nil {
		fmt.Fprintf(&b, "prepared=%s/%d/exec=%t;", p.Name, p.Epoch, p.Executable)
		for _, a := range p.Artifacts {
			fmt.Fprintf(&b, "art=%s|%s|%s;", a.Kind, a.Ref, a.Node)
		}
	} else {
		b.WriteString("prepared=<none>;")
	}
	if c := s.CandidateRecoveryPoint; c != nil {
		fmt.Fprintf(&b, "candMissing=%s;", strings.Join(c.MissingPreparation, ","))
	}
	if p := s.TargetPlacement; p != nil {
		// Free capacity is excluded on purpose: it moves whenever any unrelated
		// pod is scheduled on the node, and the verdict is "this node is
		// feasible", not "it has exactly this much room".
		fmt.Fprintf(&b, "place=%s/%s/%s;", p.Cluster, p.Node, p.Reason)
	}
	fmt.Fprintf(&b, "contract=%s/%s/%t/%s/%t;", s.Contract.RPO, s.Contract.RTO, s.Contract.FreshEnough,
		s.Contract.EstimatedActivationLatency, s.Contract.RTOWithinContract)
	for _, st := range s.Contract.ActivationSteps {
		fmt.Fprintf(&b, "step=%s|%t;", st.Name, st.Required)
	}
	// (name, status, reason) and NOT the message. Reasons are the stable,
	// enumerable verdict -- RecoveryPointStale vs WithinRPO,
	// CheckpointImageMissingOnNode vs RestoreArtifactReadyOnNode -- while
	// messages carry live detail that moves on its own: the prepared point's
	// age counts up every second and the target Redis offset advances with
	// every application tick. Including messages made every pass differ and put
	// the write rate back at ~1/s with nothing having changed.
	//
	// Consequence, accepted deliberately: two situations that share a reason but
	// differ only in message are published on the heartbeat rather than
	// immediately. No verdict is ever delayed by it.
	for _, c := range s.Checks {
		fmt.Fprintf(&b, "check=%s|%s|%s;", c.Name, c.Status, c.Reason)
	}
	for _, c := range s.Conditions {
		fmt.Fprintf(&b, "cond=%s|%s|%s;", c.Type, c.Status, c.Reason)
	}
	return b.String()
}

func refOrEmpty(r *rampv1alpha1.RecoveryPointRef) string {
	if r == nil {
		return "<none>"
	}
	return r.Name
}

// eligiblePoints returns the committed+validated RecoveryPoints for this path's
// group, newest epoch first. spec.recoveryPointRef pins the path to exactly one.
func (r *RecoveryPathReconciler) eligiblePoints(ctx context.Context, path *rampv1alpha1.RecoveryPath,
	rg *rampv1alpha1.RecoveryGroup, rgErr error) ([]rampv1alpha1.RecoveryPoint, error) {

	if rgErr != nil {
		return nil, nil
	}
	list := &rampv1alpha1.RecoveryPointList{}
	if err := r.List(ctx, list, client.InNamespace(path.Namespace)); err != nil {
		return nil, fmt.Errorf("listing RecoveryPoints: %w", err)
	}
	out := make([]rampv1alpha1.RecoveryPoint, 0, len(list.Items))
	for i := range list.Items {
		rp := list.Items[i]
		if rp.Spec.RecoveryGroupRef != rg.Name || !rp.RecoveryEligible() {
			continue
		}
		if path.Spec.RecoveryPointRef != "" && rp.Name != path.Spec.RecoveryPointRef {
			continue
		}
		out = append(out, rp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Epoch > out[j].Spec.Epoch })
	return out, nil
}

// resolvePrepared returns the RecoveryPoint this path previously promoted, if it
// is still committed and eligible.
func (r *RecoveryPathReconciler) resolvePrepared(ctx context.Context, path *rampv1alpha1.RecoveryPath,
	eligible []rampv1alpha1.RecoveryPoint) *rampv1alpha1.RecoveryPoint {

	if path.Status.PreparedRecoveryPoint == nil {
		return nil
	}
	want := path.Status.PreparedRecoveryPoint.Name
	for i := range eligible {
		if eligible[i].Name == want {
			return &eligible[i]
		}
	}
	return nil
}

// targetFacts evaluates everything about the target that is independent of the
// RecoveryPoint under consideration.
func (r *RecoveryPathReconciler) targetFacts(ctx context.Context, path *rampv1alpha1.RecoveryPath,
	rg *rampv1alpha1.RecoveryGroup, rgErr error) targetContext {

	tc := targetContext{}
	tgt, err := r.Clusters.Get(path.Spec.TargetCluster)
	if err != nil {
		tc.err = err
		return tc
	}
	tc.cluster = tgt

	// namespace
	ns := &corev1.Namespace{}
	nsErr := tgt.Client.Get(ctx, types.NamespacedName{Name: path.Spec.TargetPrereqs.Namespace}, ns)
	switch {
	case nsErr == nil && ns.Status.Phase == corev1.NamespaceActive:
		tc.nsActive, tc.nsReason = true, "NamespaceActive"
		tc.nsMessage = fmt.Sprintf("namespace %q is Active on %s", ns.Name, path.Spec.TargetCluster)
	case apierrors.IsNotFound(nsErr):
		tc.nsReason = "NamespaceMissing"
		tc.nsMessage = fmt.Sprintf("namespace %q does not exist on %s", path.Spec.TargetPrereqs.Namespace, path.Spec.TargetCluster)
	default:
		tc.nsReason = "NamespaceUnreadable"
		tc.nsMessage = fmt.Sprintf("%v", nsErr)
	}

	// placement -- one concrete node
	prefer, preferReason := path.Spec.TargetPrereqs.Node, "PinnedBySpec"
	if prefer == "" && path.Status.TargetPlacement != nil {
		prefer, preferReason = path.Status.TargetPlacement.Node, "PreservedPreviousPlacement"
	}
	pl, cands, perr := selectPlacement(ctx, tgt, path, prefer, preferReason)
	tc.nodes = cands
	if perr == nil {
		tc.placement = pl
	}

	// target Redis RUNTIME. Since RecoveryPoints carry an immutable epoch RDB,
	// this is no longer "the thing we promote". What it buys is that the target
	// Redis process, its Service and its data volume already exist and are warm,
	// so activation is "load the epoch RDB and restart" instead of "deploy
	// Redis, wait for it, then load".
	tc.redisOK, tc.redisRsn, tc.redisMsg = r.redisRuntime(path, rg, rgErr)
	return tc
}

func (r *RecoveryPathReconciler) redisRuntime(path *rampv1alpha1.RecoveryPath,
	rg *rampv1alpha1.RecoveryGroup, rgErr error) (bool, string, string) {

	if path.Spec.TargetPrereqs.StandbyEndpoint == nil {
		return false, "NoStandbyEndpoint", "spec.targetPrereqs.standbyEndpoint is not set"
	}
	var keys []string
	if rgErr == nil {
		for _, m := range rg.Spec.Members {
			if m.RecoveryDriver == rampv1alpha1.DriverRedisReplication {
				keys = m.ConsistencyKeys
			}
		}
	}
	st, err := (&drivers.RedisReplication{}).InspectStandby(path.Spec.TargetPrereqs.StandbyEndpoint, keys)
	if err != nil {
		return false, "RedisRuntimeUnreachable", err.Error()
	}
	// role=master here means a previous recovery already promoted it, or it was
	// never attached. The runtime is still usable as a restore target, so this
	// is reported but not fatal -- what matters for readiness is that the
	// process is reachable and will accept the epoch RDB.
	switch {
	case st.Role == "slave" && st.LinkStatus == "up":
		return true, "RuntimeWarmAndReplicating",
			fmt.Sprintf("target Redis %s is up, replicating (ackedOffset=%d, lastIO=%ds); "+
				"recovery restores the epoch RDB into it, it is not promoted as the recovery point",
				st.Endpoint, st.AckedOffset, st.LastIOSecondsAgo)
	case st.Role == "slave":
		return false, "ReplicationLinkDown",
			fmt.Sprintf("target Redis %s is up but master_link_status=%q, so its warm dataset is stale; "+
				"the epoch RDB would still restore correctly, but the runtime is not in its prepared state",
				st.Endpoint, st.LinkStatus)
	default:
		return true, "RuntimeReachableNotReplicating",
			fmt.Sprintf("target Redis %s is up with role=%s (not attached to the source); "+
				"it can still receive the epoch RDB, which is what recovery actually restores", st.Endpoint, st.Role)
	}
}

// pointEvaluation is the verdict for ONE RecoveryPoint on this path.
type pointEvaluation struct {
	point *rampv1alpha1.RecoveryPoint

	redisArtifactOK bool
	redisReason     string
	redisMessage    string

	videoArtifactOK bool
	videoReason     string
	videoMessage    string

	restoreOK      bool
	restoreReason  string
	restoreMessage string

	planOK      bool
	planReason  string
	planMessage string

	checkpointImage string
	objectKey       string
	node            string
	probeKeep       []string
	artifacts       []rampv1alpha1.PreparedArtifact
}

// preparationComplete is the promotion gate: every RecoveryPoint-specific
// preparation fact must hold on the selected node before a candidate may
// replace the prepared point.
func (e pointEvaluation) preparationComplete() bool {
	return e.redisArtifactOK && e.videoArtifactOK && e.restoreOK && e.planOK
}

func (e pointEvaluation) missing() []string {
	var out []string
	if !e.redisArtifactOK {
		out = append(out, rampv1alpha1.CheckRedisEpochArtifactAvailable)
	}
	if !e.videoArtifactOK {
		out = append(out, rampv1alpha1.CheckVideoCheckpointAvailable)
	}
	if !e.restoreOK {
		out = append(out, rampv1alpha1.CheckRestoreArtifactReady)
	}
	if !e.planOK {
		out = append(out, rampv1alpha1.CheckActivationPlanPrepared)
	}
	return out
}

// evaluatePoint checks everything that is specific to one RecoveryPoint.
func (r *RecoveryPathReconciler) evaluatePoint(ctx context.Context, path *rampv1alpha1.RecoveryPath,
	tc targetContext, rp *rampv1alpha1.RecoveryPoint) pointEvaluation {

	e := pointEvaluation{point: rp}
	now := metav1.Now()

	// --- the immutable Redis epoch artifact ---------------------------------
	if a := rp.RestorableArtifact(rampv1alpha1.ArtifactRedisSnapshot); a == nil {
		e.redisReason = "NoRedisEpochArtifact"
		e.redisMessage = "the RecoveryPoint carries no immutable redisSnapshot artifact; " +
			"a live replica cannot serve as the recovery point once the source advances past it"
	} else {
		key := strings.TrimPrefix(a.Ref, fmt.Sprintf("minio://%s/", r.Store.Bucket()))
		oi, ok, err := r.Store.Stat(ctx, key)
		switch {
		case err != nil:
			e.redisReason, e.redisMessage = "ArtifactStoreUnreachable", err.Error()
		case !ok:
			e.redisReason = "ArtifactMissing"
			e.redisMessage = fmt.Sprintf("%s is not present in the artifact store", key)
		default:
			e.redisArtifactOK = true
			e.redisReason = "ArtifactPresent"
			e.redisMessage = fmt.Sprintf("%s (%d bytes) frozen at logical position %d",
				oi.Key, oi.SizeBytes, a.LogicalPosition)
			e.artifacts = append(e.artifacts, rampv1alpha1.PreparedArtifact{
				Kind: "redisSnapshot", Ref: a.Ref, Checksum: a.Checksum,
				VerifiedAt: &now, Message: e.redisMessage,
			})
		}
	}

	// --- the container checkpoint in the shared store ------------------------
	ckpt := rp.RestorableArtifact(rampv1alpha1.ArtifactContainerCheckpoint)
	if ckpt == nil {
		e.videoReason = "NoCheckpointArtifact"
		e.videoMessage = "the RecoveryPoint carries no containerCheckpoint artifact"
	} else {
		e.objectKey = strings.TrimPrefix(ckpt.Ref, fmt.Sprintf("minio://%s/", r.Store.Bucket()))
		oi, ok, err := r.Store.Stat(ctx, e.objectKey)
		switch {
		case err != nil:
			e.videoReason, e.videoMessage = "ArtifactStoreUnreachable", err.Error()
		case !ok:
			e.videoReason = "ArtifactMissing"
			e.videoMessage = fmt.Sprintf("%s is not present in the artifact store", e.objectKey)
		default:
			e.videoArtifactOK = true
			e.videoReason = "ArtifactPresent"
			e.videoMessage = fmt.Sprintf("%s (%d bytes) present in bucket %s", oi.Key, oi.SizeBytes, r.Store.Bucket())
			e.artifacts = append(e.artifacts, rampv1alpha1.PreparedArtifact{
				Kind: "videoCheckpoint", Ref: ckpt.Ref, Checksum: ckpt.Checksum,
				VerifiedAt: &now, Message: e.videoMessage,
			})
		}
	}

	// --- the activation plan, and the image IT declares ----------------------
	// Order matters: the checkpoint image to look for on the node comes from the
	// activation plan, because the plan is what names the image the restore will
	// actually run. Guessing a tag here would let a path be "ready" for an image
	// nothing is going to use.
	e.planOK, e.planReason, e.planMessage, e.checkpointImage = r.activationPlan(ctx, path, tc, rp)
	if e.planOK {
		e.artifacts = append(e.artifacts, rampv1alpha1.PreparedArtifact{
			Kind: "activationPlan", Ref: e.checkpointImage, Node: nodeName(tc.placement),
			VerifiedAt: &now, Message: e.planMessage,
		})
	}

	// --- the artifacts the restore consumes, ON THE PLACEMENT NODE -----------
	switch {
	case tc.cluster == nil || tc.placement == nil:
		e.restoreReason = "NoTargetPlacement"
		e.restoreMessage = "no feasible target node, so no node to verify restore artifacts on"
	case e.objectKey == "":
		e.restoreReason = "NoCheckpointArtifact"
		e.restoreMessage = "the RecoveryPoint carries no containerCheckpoint artifact to stage"
	case e.checkpointImage == "":
		e.restoreReason = "NoCheckpointImageDeclared"
		e.restoreMessage = fmt.Sprintf(
			"no activation plan declares a CRI checkpoint image for %s, so there is nothing for containerd to restore from; "+
				"run 35-build-checkpoint-image.sh and 61-prepare-gitops-path.sh for this RecoveryPoint", rp.Name)
	default:
		e.node = tc.placement.Node
		p := r.probeRestoreArtifact(ctx, tc.cluster, path, tc.placement.Node, e.objectKey, e.checkpointImage)
		e.restoreOK, e.restoreReason, e.restoreMessage, e.probeKeep = p.ok, p.reason, p.message, p.keep
		if p.ok {
			e.artifacts = append(e.artifacts,
				rampv1alpha1.PreparedArtifact{Kind: "videoCheckpointOnNode", Ref: e.objectKey,
					Node: p.node, VerifiedAt: &now, Message: "staged in the kubelet checkpoint directory"},
				rampv1alpha1.PreparedArtifact{Kind: "checkpointImage", Ref: e.checkpointImage,
					Node: p.node, VerifiedAt: &now, Message: "present in the node's containerd image store"})
		}
	}
	return e
}

// activationPlan verifies the GitOps half of preparation, and returns the CRI
// checkpoint image the plan declares.
//
// The plan is only accepted when its annotations say it was prepared for THIS
// RecoveryPoint on THIS node. Before this, preparation was "whatever the last
// script run happened to stage", which is how the Q>P experiment once restored
// the previous epoch's checkpoint.
func (r *RecoveryPathReconciler) activationPlan(ctx context.Context, path *rampv1alpha1.RecoveryPath,
	tc targetContext, rp *rampv1alpha1.RecoveryPoint) (ok bool, reason, message, image string) {

	ref := path.Spec.TargetPrereqs.ActivationPlanWorkload
	if ref == nil {
		return false, "NoActivationPlanConfigured",
			"spec.targetPrereqs.activationPlanWorkload is not set, so RAMP cannot verify that the target is prepared for any particular RecoveryPoint", ""
	}
	if tc.cluster == nil {
		return false, "TargetUnreachable", "target cluster is not reachable", ""
	}
	dep := &appsv1.Deployment{}
	if err := tc.cluster.Client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "ActivationPlanMissing", fmt.Sprintf(
				"target workload %s/%s does not exist; at failure time the manifests would still have to be written and converged",
				ref.Namespace, ref.Name), ""
		}
		return false, "ActivationPlanUnreadable", fmt.Sprintf("reading %s/%s: %v", ref.Namespace, ref.Name, err), ""
	}

	ann := dep.Annotations
	preparedFor := ann[rampv1alpha1.AnnPreparedRecoveryPoint]
	image = ann[rampv1alpha1.AnnCheckpointImage]
	planNode := ann[rampv1alpha1.AnnTargetNode]

	if preparedFor == "" {
		return false, "ActivationPlanUnattributed", fmt.Sprintf(
			"target workload %s/%s carries no %s annotation, so it cannot be shown to belong to any RecoveryPoint",
			ref.Namespace, ref.Name, rampv1alpha1.AnnPreparedRecoveryPoint), image
	}
	if preparedFor != rp.Name {
		return false, "ActivationPlanForDifferentRecoveryPoint", fmt.Sprintf(
			"target workload %s/%s is prepared for %s, not %s", ref.Namespace, ref.Name, preparedFor, rp.Name), ""
	}
	if image == "" {
		return false, "NoCheckpointImageDeclared", fmt.Sprintf(
			"target workload %s/%s declares no %s annotation", ref.Namespace, ref.Name, rampv1alpha1.AnnCheckpointImage), ""
	}
	if tc.placement != nil && planNode != "" && planNode != tc.placement.Node {
		return false, "ActivationPlanOnDifferentNode", fmt.Sprintf(
			"the activation plan is pinned to node %s but the feasible placement is %s; "+
				"artifact readiness on one node is not readiness for the other", planNode, tc.placement.Node), image
	}
	// The prepared plan sits at replicas 0 on purpose: activation is the commit
	// that scales it up and swaps in the checkpoint image.
	sel := dep.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
	if tc.placement != nil && sel != "" && sel != tc.placement.Node {
		return false, "ActivationPlanOnDifferentNode", fmt.Sprintf(
			"the activation plan's nodeSelector pins it to %s but the feasible placement is %s", sel, tc.placement.Node), image
	}
	return true, "ActivationPlanPrepared", fmt.Sprintf(
		"target workload %s/%s exists (replicas=%d, state=%q), prepared for %s epoch %s, pinned to node %s, checkpoint image %s",
		ref.Namespace, ref.Name, deref32(dep.Spec.Replicas), ann[rampv1alpha1.AnnActivationState],
		preparedFor, ann[rampv1alpha1.AnnPreparedEpoch], orDash(planNode, sel), image), image
}

// buildStatus turns everything observed into the published status.
func (r *RecoveryPathReconciler) buildStatus(
	path *rampv1alpha1.RecoveryPath,
	rg *rampv1alpha1.RecoveryGroup, rgErr error,
	tc targetContext,
	latest *rampv1alpha1.RecoveryPoint,
	candidate *rampv1alpha1.RecoveryPoint, candidateEval *pointEvaluation,
	prepared *rampv1alpha1.RecoveryPoint, preparedEval *pointEvaluation,
) rampv1alpha1.RecoveryPathStatus {

	ev := &evaluation{path: path}
	now := metav1.Now()

	// ---- RecoveryPoint-independent target facts ----------------------------
	if tc.err != nil {
		ev.add(rampv1alpha1.CheckTargetClusterReachable, false, true, "ClusterNotRegistered", tc.err.Error())
	} else {
		ready := 0
		for _, n := range tc.nodes {
			if n.ready {
				ready++
			}
		}
		ev.add(rampv1alpha1.CheckTargetClusterReachable, ready > 0, true, "NodesReady",
			fmt.Sprintf("%d/%d evaluated nodes Ready on %s", ready, len(tc.nodes), path.Spec.TargetCluster))
		ev.add(rampv1alpha1.CheckTargetNamespaceReady, tc.nsActive, true, tc.nsReason, tc.nsMessage)
	}

	if tc.placement != nil {
		ev.add(rampv1alpha1.CheckTargetPlacementFeasible, true, true, "NodeSelected", fmt.Sprintf(
			"node %s on %s satisfies every node-level prerequisite at once "+
				"(allocatable %dm CPU / %dMiB, requested %dm / %dMiB, free %dm / %dMiB; required %dm / %dMiB) [%s]",
			tc.placement.Node, tc.placement.Cluster,
			tc.placement.AllocatableCPUMillis, tc.placement.AllocatableMemoryMiB,
			tc.placement.RequestedCPUMillis, tc.placement.RequestedMemoryMiB,
			tc.placement.FreeCPUMillis, tc.placement.FreeMemoryMiB,
			path.Spec.TargetPrereqs.MinAllocatableCPUMillis, path.Spec.TargetPrereqs.MinAllocatableMemoryMiB,
			tc.placement.Reason))
		ev.add(rampv1alpha1.CheckRestoreCapabilityAvailable, true, true, "AgentReadyOnPlacementNode",
			fmt.Sprintf("a ready restore-capability agent is running on the placement node %s", tc.placement.Node))
	} else if tc.err == nil {
		ev.add(rampv1alpha1.CheckTargetPlacementFeasible, false, true, "NoFeasibleNode",
			"no single target node satisfies every node-level prerequisite: "+describeRejections(tc.nodes))
		ev.add(rampv1alpha1.CheckRestoreCapabilityAvailable, false, true, "NoPlacementNode",
			"restore capability is a per-node property and no placement node could be selected")
	}

	if tc.err == nil {
		ev.add(rampv1alpha1.CheckRedisTargetRuntimeReady, tc.redisOK, true, tc.redisRsn, tc.redisMsg)
	}

	// ---- facts about the PREPARED RecoveryPoint ----------------------------
	if rgErr != nil {
		ev.add(rampv1alpha1.CheckRecoveryPointCommitted, false, true, "RecoveryGroupUnavailable",
			fmt.Sprintf("RecoveryGroup %q: %v", path.Spec.RecoveryGroupRef, rgErr))
	} else if prepared == nil {
		reason, msg := "NoPreparedRecoveryPoint", "no RecoveryPoint has completed target-side preparation for this path"
		if latest != nil {
			msg = fmt.Sprintf("%s is committed but this path has not finished preparing any RecoveryPoint yet", latest.Name)
		}
		ev.add(rampv1alpha1.CheckRecoveryPointCommitted, false, true, reason, msg)
	} else {
		ev.add(rampv1alpha1.CheckRecoveryPointCommitted, true, true, "PreparedAndCommitted",
			fmt.Sprintf("prepared RecoveryPoint %s: epoch %d committed at %s, logical position %d, cross-driver skew %d",
				prepared.Name, prepared.Spec.Epoch, tstr(prepared.Status.Timings.CommitTime),
				prepared.Status.LogicalPosition, prepared.Status.Validation.ObservedSkew))
	}

	if preparedEval != nil {
		ev.add(rampv1alpha1.CheckRedisEpochArtifactAvailable, preparedEval.redisArtifactOK, true,
			preparedEval.redisReason, preparedEval.redisMessage)
		ev.add(rampv1alpha1.CheckVideoCheckpointAvailable, preparedEval.videoArtifactOK, true,
			preparedEval.videoReason, preparedEval.videoMessage)
		ev.add(rampv1alpha1.CheckRestoreArtifactReady, preparedEval.restoreOK, true,
			preparedEval.restoreReason, preparedEval.restoreMessage)
		ev.add(rampv1alpha1.CheckActivationPlanPrepared, preparedEval.planOK, true,
			preparedEval.planReason, preparedEval.planMessage)
	} else {
		for _, n := range []string{
			rampv1alpha1.CheckRedisEpochArtifactAvailable,
			rampv1alpha1.CheckVideoCheckpointAvailable,
			rampv1alpha1.CheckRestoreArtifactReady,
			rampv1alpha1.CheckActivationPlanPrepared,
		} {
			ev.add(n, false, true, "NoPreparedRecoveryPoint", "nothing is prepared, so there is nothing to verify")
		}
	}

	// ---- the contract ------------------------------------------------------
	met := map[string]bool{}
	for _, c := range ev.checks {
		met[c.Name] = c.Status == "True"
	}
	var preparedRef *rampv1alpha1.RecoveryPointRef
	if prepared != nil {
		preparedRef = pointRef(prepared)
	}
	contract := rampv1alpha1.RecoveryContract{}
	if rgErr == nil {
		contract = rg.Spec.RecoveryContract
	}
	cs := evaluateContract(contractInputs{contract: contract, prepared: preparedRef, met: met, now: time.Now()})

	fr, fm := freshnessMessage(cs, preparedRef)
	ev.add(rampv1alpha1.CheckRecoveryPointFreshEnough, cs.FreshEnough, true, fr, fm)
	rr, rm := rtoMessage(cs)
	ev.add(rampv1alpha1.CheckEstimatedRTOWithinContract, cs.RTOWithinContract, true, rr, rm)

	// ---- assemble ----------------------------------------------------------
	st := ev.conclude()
	st.Contract = cs
	st.TargetPlacement = tc.placement
	if latest != nil {
		st.LatestRecoveryPoint = pointRef(latest)
	}
	if prepared != nil {
		p := &rampv1alpha1.PreparedRecoveryPoint{
			RecoveryPointRef: *preparedRef,
			Placement:        tc.placement,
			PreparedAt:       &now,
			Executable:       preparedEval != nil && preparedEval.preparationComplete(),
		}
		// Preserve the original promotion timestamp across reconciles.
		if old := path.Status.PreparedRecoveryPoint; old != nil && old.Name == prepared.Name && old.PreparedAt != nil {
			p.PreparedAt = old.PreparedAt
		}
		if preparedEval != nil {
			p.Artifacts = preparedEval.artifacts
		}
		st.PreparedRecoveryPoint = p
		st.ObservedRecoveryPoint = prepared.Name
		st.ObservedEpoch = prepared.Spec.Epoch
	}
	if candidate != nil {
		c := &rampv1alpha1.CandidateRecoveryPoint{
			RecoveryPointRef: *pointRef(candidate),
			ObservedAt:       &now,
		}
		if old := path.Status.CandidateRecoveryPoint; old != nil && old.Name == candidate.Name && old.ObservedAt != nil {
			c.ObservedAt = old.ObservedAt
		}
		if candidateEval != nil {
			c.MissingPreparation = candidateEval.missing()
		}
		c.Message = fmt.Sprintf(
			"a newer committed RecoveryPoint is being prepared; the path keeps recovering from %s until %s is proven executable",
			nameOrNone(prepared), candidate.Name)
		st.CandidateRecoveryPoint = c
	}
	st.EstimatedRTO = cs.EstimatedActivationLatency.Duration.String()
	return st
}

// --- small helpers ---------------------------------------------------------

func pointRef(rp *rampv1alpha1.RecoveryPoint) *rampv1alpha1.RecoveryPointRef {
	return &rampv1alpha1.RecoveryPointRef{
		Name:            rp.Name,
		Epoch:           rp.Spec.Epoch,
		LogicalPosition: rp.Status.LogicalPosition,
		CommitTime:      rp.Status.Timings.CommitTime,
	}
}

func nameOrNone(rp *rampv1alpha1.RecoveryPoint) string {
	if rp == nil {
		return "<none>"
	}
	return rp.Name
}

func refName(p *rampv1alpha1.PreparedRecoveryPoint) string {
	if p == nil {
		return "<none>"
	}
	return p.Name
}

func candName(c *rampv1alpha1.CandidateRecoveryPoint) string {
	if c == nil {
		return "<none>"
	}
	return c.Name
}

func nodeName(p *rampv1alpha1.TargetPlacement) string {
	if p == nil {
		return ""
	}
	return p.Node
}

func deref32(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

func orDash(a, b string) string {
	if a != "" {
		return a
	}
	if b != "" {
		return b
	}
	return "-"
}

// short abbreviates a long identifier for status messages.
func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}

func tstr(t *metav1.Time) string {
	if t == nil {
		return "<unset>"
	}
	return t.Format(time.RFC3339)
}

func (r *RecoveryPathReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rampv1alpha1.RecoveryPath{}).
		Named("recoverypath").
		Complete(r)
}
