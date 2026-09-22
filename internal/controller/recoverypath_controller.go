package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

// RecoveryPathReconciler is the Readiness Controller. Its entire job is:
//
//	OBSERVE -> evaluate recovery-path prerequisites -> publish readiness
//
// It never checkpoints, never restores, never promotes a replica, and never
// writes to a ClusterPolicy. Those are, respectively, the Checkpoint Agent's,
// the Transition Operator's, the recovery execution's and the operator's jobs.
// Evaluation is deterministic: no scoring, no optimizer, no prediction.
type RecoveryPathReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clusters *clusters.Registry
	Store    *artifacts.Store

	// StageProbeImage is the image used by the one-shot pod that proves an
	// artifact really is staged on the target node.
	StageProbeImage string
}

// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypaths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypaths/status,verbs=get;update;patch

const recoveryPathResyncInterval = 20 * time.Second

func (r *RecoveryPathReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	path := &rampv1alpha1.RecoveryPath{}
	if err := r.Get(ctx, req.NamespacedName, path); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ev := &evaluation{path: path}

	// --- the RecoveryGroup and its committed recovery point ------------------
	rg := &rampv1alpha1.RecoveryGroup{}
	rgErr := r.Get(ctx, types.NamespacedName{Namespace: path.Namespace, Name: path.Spec.RecoveryGroupRef}, rg)

	var rp *rampv1alpha1.RecoveryPoint
	if rgErr != nil {
		ev.add(rampv1alpha1.CheckRecoveryPointCommitted, false, true, "RecoveryGroupUnavailable",
			fmt.Sprintf("RecoveryGroup %q: %v", path.Spec.RecoveryGroupRef, rgErr))
	} else {
		name := path.Spec.RecoveryPointRef
		if name == "" {
			name = rg.Status.LatestRecoveryPoint
		}
		if name == "" {
			ev.add(rampv1alpha1.CheckRecoveryPointCommitted, false, true, "NoRecoveryPoint",
				"RecoveryGroup has no committed, validated RecoveryPoint")
		} else {
			cand := &rampv1alpha1.RecoveryPoint{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: path.Namespace, Name: name}, cand); err != nil {
				ev.add(rampv1alpha1.CheckRecoveryPointCommitted, false, true, "RecoveryPointMissing",
					fmt.Sprintf("RecoveryPoint %q: %v", name, err))
			} else if !cand.RecoveryEligible() {
				ev.add(rampv1alpha1.CheckRecoveryPointCommitted, false, true, "RecoveryPointNotEligible",
					fmt.Sprintf("RecoveryPoint %q is phase=%s validated=%t; an incomplete epoch is never recovery eligible",
						name, cand.Status.Phase, cand.Status.Validation.Validated))
			} else {
				rp = cand
				ev.add(rampv1alpha1.CheckRecoveryPointCommitted, true, true, "Committed",
					fmt.Sprintf("epoch %d committed at %s, cross-driver skew %d",
						cand.Spec.Epoch, tstr(cand.Status.Timings.CommitTime), cand.Status.Validation.ObservedSkew))
			}
		}
	}

	// --- target cluster reachability ---------------------------------------
	tgt, tgtErr := r.Clusters.Get(path.Spec.TargetCluster)
	var targetNodes *corev1.NodeList
	if tgtErr != nil {
		ev.add(rampv1alpha1.CheckTargetClusterReachable, false, true, "ClusterNotRegistered", tgtErr.Error())
	} else {
		targetNodes = &corev1.NodeList{}
		if err := tgt.Client.List(ctx, targetNodes); err != nil {
			ev.add(rampv1alpha1.CheckTargetClusterReachable, false, true, "APIServerUnreachable",
				fmt.Sprintf("listing nodes on %s: %v", path.Spec.TargetCluster, err))
			targetNodes = nil
		} else {
			ready := 0
			for _, n := range targetNodes.Items {
				for _, c := range n.Status.Conditions {
					if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
						ready++
					}
				}
			}
			ev.add(rampv1alpha1.CheckTargetClusterReachable, ready > 0, true, "NodesReady",
				fmt.Sprintf("%d/%d nodes Ready on %s", ready, len(targetNodes.Items), path.Spec.TargetCluster))
		}
	}

	// --- target namespace ---------------------------------------------------
	if tgtErr == nil {
		ns := &corev1.Namespace{}
		err := tgt.Client.Get(ctx, types.NamespacedName{Name: path.Spec.TargetPrereqs.Namespace}, ns)
		switch {
		case err == nil && ns.Status.Phase == corev1.NamespaceActive:
			ev.add(rampv1alpha1.CheckTargetNamespaceReady, true, true, "NamespaceActive",
				fmt.Sprintf("namespace %q is Active on %s", ns.Name, path.Spec.TargetCluster))
		case apierrors.IsNotFound(err):
			ev.add(rampv1alpha1.CheckTargetNamespaceReady, false, true, "NamespaceMissing",
				fmt.Sprintf("namespace %q does not exist on %s", path.Spec.TargetPrereqs.Namespace, path.Spec.TargetCluster))
		default:
			ev.add(rampv1alpha1.CheckTargetNamespaceReady, false, true, "NamespaceUnreadable", fmt.Sprintf("%v", err))
		}
	}

	// --- video checkpoint artifact available in the shared store ------------
	ckptKey := ""
	if rp != nil {
		for _, a := range rp.Status.Artifacts {
			if a.Type == rampv1alpha1.ArtifactContainerCheckpoint {
				ckptKey = strings.TrimPrefix(a.Ref, fmt.Sprintf("minio://%s/", r.Store.Bucket()))
			}
		}
	}
	switch {
	case rp == nil:
		ev.add(rampv1alpha1.CheckVideoCheckpointAvailable, false, true, "NoRecoveryPoint",
			"no committed RecoveryPoint to take an artifact from")
	case ckptKey == "":
		ev.add(rampv1alpha1.CheckVideoCheckpointAvailable, false, true, "NoCheckpointArtifact",
			"the committed RecoveryPoint carries no containerCheckpoint artifact")
	default:
		oi, ok, err := r.Store.Stat(ctx, ckptKey)
		switch {
		case err != nil:
			ev.add(rampv1alpha1.CheckVideoCheckpointAvailable, false, true, "ArtifactStoreUnreachable", err.Error())
		case !ok:
			ev.add(rampv1alpha1.CheckVideoCheckpointAvailable, false, true, "ArtifactMissing",
				fmt.Sprintf("%s is not present in the artifact store", ckptKey))
		default:
			ev.add(rampv1alpha1.CheckVideoCheckpointAvailable, true, true, "ArtifactPresent",
				fmt.Sprintf("%s (%d bytes) present in bucket %s", oi.Key, oi.SizeBytes, r.Store.Bucket()))
		}
	}

	// --- restore capability on the target -----------------------------------
	// The checkpoint-agent is what stages artifacts onto target nodes and what
	// the restore step acts through. Its readiness IS the target's restore
	// capability; RAMP does not reimplement either half.
	if tgtErr == nil && path.Spec.TargetPrereqs.RestoreCapabilityDaemonSet != nil {
		ref := path.Spec.TargetPrereqs.RestoreCapabilityDaemonSet
		ds := &appsv1.DaemonSet{}
		err := tgt.Client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, ds)
		switch {
		case err != nil:
			ev.add(rampv1alpha1.CheckRestoreCapabilityAvailable, false, true, "DaemonSetUnavailable",
				fmt.Sprintf("%s/%s on %s: %v", ref.Namespace, ref.Name, path.Spec.TargetCluster, err))
		case ds.Status.NumberReady == 0:
			ev.add(rampv1alpha1.CheckRestoreCapabilityAvailable, false, true, "NoReadyAgent",
				fmt.Sprintf("%s/%s has 0/%d ready", ref.Namespace, ref.Name, ds.Status.DesiredNumberScheduled))
		default:
			ev.add(rampv1alpha1.CheckRestoreCapabilityAvailable, true, true, "AgentReady",
				fmt.Sprintf("%s/%s ready on %d/%d nodes", ref.Namespace, ref.Name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
		}
	}

	// --- artifact actually staged on a target node --------------------------
	// This is the check that separates WARM from HOT: an artifact that exists
	// only in the shared store still needs a pull before it can be restored.
	if tgtErr == nil && ckptKey != "" {
		staged, reason, msg := r.probeStaged(ctx, tgt, path, ckptKey)
		ev.add(rampv1alpha1.CheckVideoCheckpointStaged, staged, true, reason, msg)
	} else if ckptKey == "" {
		ev.add(rampv1alpha1.CheckVideoCheckpointStaged, false, true, "NoArtifact", "no checkpoint artifact to stage")
	}

	// --- redis standby synchronised -----------------------------------------
	if path.Spec.TargetPrereqs.StandbyEndpoint == nil {
		ev.add(rampv1alpha1.CheckRedisStandbyReady, false, true, "NoStandbyEndpoint",
			"spec.targetPrereqs.standbyEndpoint is not set")
	} else {
		var keys []string
		if rgErr == nil {
			for _, m := range rg.Spec.Members {
				if m.RecoveryDriver == rampv1alpha1.DriverRedisReplication {
					keys = m.ConsistencyKeys
				}
			}
		}
		st, err := (&drivers.RedisReplication{}).InspectStandby(path.Spec.TargetPrereqs.StandbyEndpoint, keys)
		switch {
		case err != nil:
			ev.add(rampv1alpha1.CheckRedisStandbyReady, false, true, "StandbyUnreachable", err.Error())
		case st.Role != "slave":
			ev.add(rampv1alpha1.CheckRedisStandbyReady, false, true, "NotAReplica",
				fmt.Sprintf("standby %s has role %q; it is not tracking the source", st.Endpoint, st.Role))
		case st.LinkStatus != "up":
			ev.add(rampv1alpha1.CheckRedisStandbyReady, false, true, "ReplicationLinkDown",
				fmt.Sprintf("standby %s master_link_status=%q", st.Endpoint, st.LinkStatus))
		default:
			maxLag := path.Spec.TargetPrereqs.MaxReplicationLagBytes
			if maxLag == 0 {
				maxLag = 4096
			}

			var rpOffset int64 = -1
			var rpReplID string
			if rp != nil {
				for _, a := range rp.Status.Artifacts {
					if a.Type == rampv1alpha1.ArtifactReplicationState {
						rpOffset, rpReplID = a.ReplicationOffset, a.ReplicationID
					}
				}
			}

			switch {
			case rpOffset < 0:
				// Nothing to compare against; link being up is all we can assert.
				ev.add(rampv1alpha1.CheckRedisStandbyReady, true, true, "StandbyLinkUp",
					fmt.Sprintf("standby %s role=slave link=up ackedOffset=%d (no replicationState artifact to compare)",
						st.Endpoint, st.AckedOffset))

			case rpReplID != "" && st.ReplicationID != "" && rpReplID != st.ReplicationID:
				// Replication offsets are per-stream. If the primary has been
				// recreated the stream restarts at zero under a new replid, and
				// comparing offsets across ids is meaningless -- it reports a
				// huge bogus "lag". The honest verdict is that this recovery
				// point belongs to a stream the standby no longer follows, so a
				// new epoch is needed.
				ev.add(rampv1alpha1.CheckRedisStandbyReady, false, true, "RecoveryPointFromDifferentReplicationStream",
					fmt.Sprintf("standby %s follows replication stream %s but the recovery point was taken from %s; "+
						"their offsets are not comparable and a new epoch is required",
						st.Endpoint, short(st.ReplicationID), short(rpReplID)))

			default:
				// Within one stream, the standby racing ahead of the recovery
				// point is normal and fine; only falling BEHIND it matters.
				lag := rpOffset - st.AckedOffset
				if lag > maxLag {
					ev.add(rampv1alpha1.CheckRedisStandbyReady, false, true, "StandbyBehindRecoveryPoint",
						fmt.Sprintf("standby %s is %d bytes behind the recovery point offset (tolerance %d)",
							st.Endpoint, lag, maxLag))
				} else {
					ev.add(rampv1alpha1.CheckRedisStandbyReady, true, true, "StandbySynchronized",
						fmt.Sprintf("standby %s role=slave link=up ackedOffset=%d logicalPosition=%d lastIO=%ds",
							st.Endpoint, st.AckedOffset, st.LogicalPosition, st.LastIOSecondsAgo))
				}
			}
		}
	}

	// --- target has room to run the recovered group -------------------------
	if targetNodes != nil {
		req := path.Spec.TargetPrereqs
		var bestCPU, bestMem int64
		for _, n := range targetNodes.Items {
			cpu := n.Status.Allocatable[corev1.ResourceCPU]
			mem := n.Status.Allocatable[corev1.ResourceMemory]
			if m := cpu.MilliValue(); m > bestCPU {
				bestCPU = m
			}
			if m := mem.Value() / (1024 * 1024); m > bestMem {
				bestMem = m
			}
		}
		okCPU := req.MinAllocatableCPUMillis == 0 || bestCPU >= req.MinAllocatableCPUMillis
		okMem := req.MinAllocatableMemoryMiB == 0 || bestMem >= req.MinAllocatableMemoryMiB
		ev.add(rampv1alpha1.CheckTargetResourceReady, okCPU && okMem, true, "AllocatableChecked",
			fmt.Sprintf("best target node allocatable: %dm CPU, %dMiB memory (required %dm / %dMiB)",
				bestCPU, bestMem, req.MinAllocatableCPUMillis, req.MinAllocatableMemoryMiB))
		_ = resource.Quantity{}
	}

	// --- deterministic state machine ---------------------------------------
	status := ev.conclude(rp)
	path.Status = status
	if err := r.Status().Update(ctx, path); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating RecoveryPath status: %w", err)
	}
	log.V(1).Info("evaluated recovery path", "readiness", status.Readiness, "unmet", status.UnmetMandatoryChecks)
	return ctrl.Result{RequeueAfter: recoveryPathResyncInterval}, nil
}

// short abbreviates a Redis replication id for status messages.
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
