package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
	"github.com/dcn-ssu/ramp/internal/clusters"
	"github.com/dcn-ssu/ramp/internal/drivers"
)

// RecoveryGroupReconciler owns the recovery-consistency domain: which
// AppBundle components belong to it, which RecoveryDriver each member is bound
// to, and which RecoveryPoint is currently the group's latest committed one.
//
// It deliberately does NOT: run epochs (that is RecoveryPointReconciler),
// evaluate targets (RecoveryPathReconciler), or execute anything (Transition
// Operator / checkpoint-agent).
type RecoveryGroupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clusters *clusters.Registry
}

// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverygroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverygroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ramp.dcn.ssu.ac.kr,resources=recoverypoints,verbs=get;list;watch

const recoveryGroupResyncInterval = 30 * time.Second

func (r *RecoveryGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rg := &rampv1alpha1.RecoveryGroup{}
	if err := r.Get(ctx, req.NamespacedName, rg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	status := rampv1alpha1.RecoveryGroupStatus{
		ObservedGeneration: rg.Generation,
		Conditions:         rg.Status.Conditions,
	}

	src, err := r.Clusters.Get(rg.Spec.AppBundleRef.Cluster)
	if err != nil {
		status.Phase = rampv1alpha1.RecoveryGroupDegraded
		setCondition(&status.Conditions, "MembersResolved", metav1.ConditionFalse, "ClusterNotRegistered", err.Error(), rg.Generation)
		return r.finish(ctx, rg, status, err)
	}

	ab, err := GetAppBundle(ctx, src.Client, rg.Spec.AppBundleRef)
	if err != nil {
		status.Phase = rampv1alpha1.RecoveryGroupDegraded
		setCondition(&status.Conditions, "MembersResolved", metav1.ConditionFalse, "AppBundleUnavailable", err.Error(), rg.Generation)
		return r.finish(ctx, rg, status, nil)
	}

	// --- resolve every member through the AppBundle, then probe its driver ---
	allResolved := true
	for _, m := range rg.Spec.Members {
		ms := rampv1alpha1.MemberStatus{Name: m.Name, RecoveryDriver: m.RecoveryDriver}

		rc, err := ResolveComponent(ctx, src.Client, ab, m.ComponentRef)
		if err != nil {
			ms.Resolved = false
			ms.Message = err.Error()
			allResolved = false
			if rc != nil {
				ms.APIVersion, ms.Kind, ms.Namespace, ms.ResourceName = rc.APIVersion, rc.Kind, rc.Namespace, rc.Name
			}
			status.Members = append(status.Members, ms)
			continue
		}

		ms.Resolved = true
		ms.APIVersion, ms.Kind, ms.Namespace, ms.ResourceName = rc.APIVersion, rc.Kind, rc.Namespace, rc.Name
		ms.PodName, ms.NodeName = rc.PodName, rc.NodeName

		switch m.RecoveryDriver {
		case rampv1alpha1.DriverContainerCheckpoint:
			// The driver is ready when there is a concrete pod/node/container to
			// checkpoint. The kubelet call itself is only made during CAPTURE.
			if rc.PodName == "" || rc.NodeName == "" {
				ms.DriverReady = false
				ms.Message = "no running pod/node to checkpoint"
			} else if m.Container == "" {
				ms.DriverReady = false
				ms.Message = "member has recoveryDriver=container-checkpoint but no spec.container"
			} else {
				ms.DriverReady = true
				ms.Message = fmt.Sprintf("checkpointable container %q on node %s", m.Container, rc.NodeName)
			}
		case rampv1alpha1.DriverRedisReplication:
			d := &drivers.RedisReplication{}
			capState, err := d.Capture(m.SourceEndpoint, m.ConsistencyKeys)
			if err != nil {
				ms.DriverReady = false
				ms.Message = err.Error()
			} else {
				ms.DriverReady = true
				ms.Message = fmt.Sprintf("role=%s offset=%d replicas=%d logicalPosition=%d",
					capState.Role, capState.ReplicationOffset, capState.ConnectedReplicas, capState.LogicalPosition)
			}
		default:
			ms.DriverReady = false
			ms.Message = fmt.Sprintf("unknown recoveryDriver %q", m.RecoveryDriver)
		}

		if !ms.DriverReady {
			allResolved = false
		}
		status.Members = append(status.Members, ms)
	}

	if allResolved {
		status.Phase = rampv1alpha1.RecoveryGroupResolved
		setCondition(&status.Conditions, "MembersResolved", metav1.ConditionTrue, "AllMembersResolved",
			fmt.Sprintf("%d members resolved through AppBundle %s/%s", len(rg.Spec.Members), rg.Spec.AppBundleRef.Namespace, rg.Spec.AppBundleRef.Name), rg.Generation)
	} else {
		status.Phase = rampv1alpha1.RecoveryGroupDegraded
		setCondition(&status.Conditions, "MembersResolved", metav1.ConditionFalse, "MemberNotReady",
			"one or more members could not be resolved or their driver is not ready", rg.Generation)
	}

	// --- adopt the newest RECOVERY-ELIGIBLE RecoveryPoint -------------------
	// An incomplete epoch must never become recovery eligible, so the filter is
	// RecoveryEligible() (Committed AND validated), not "newest".
	rps := &rampv1alpha1.RecoveryPointList{}
	if err := r.List(ctx, rps, client.InNamespace(rg.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing recovery points: %w", err)
	}
	eligible := make([]rampv1alpha1.RecoveryPoint, 0, len(rps.Items))
	for _, rp := range rps.Items {
		if rp.Spec.RecoveryGroupRef != rg.Name {
			continue
		}
		if rp.Spec.Epoch > status.LatestEpoch {
			status.LatestEpoch = rp.Spec.Epoch
		}
		if rp.RecoveryEligible() {
			eligible = append(eligible, rp)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].Spec.Epoch > eligible[j].Spec.Epoch })
	if len(eligible) > 0 {
		status.LatestRecoveryPoint = eligible[0].Name
		status.LatestRecoveryPointTime = eligible[0].Status.Timings.CommitTime
		setCondition(&status.Conditions, "RecoveryPointAvailable", metav1.ConditionTrue, "Committed",
			fmt.Sprintf("epoch %d committed and validated", eligible[0].Spec.Epoch), rg.Generation)
	} else {
		setCondition(&status.Conditions, "RecoveryPointAvailable", metav1.ConditionFalse, "NoCommittedRecoveryPoint",
			"no RecoveryPoint has reached phase=Committed with validated=true", rg.Generation)
	}

	log.V(1).Info("reconciled RecoveryGroup", "phase", status.Phase, "latestRecoveryPoint", status.LatestRecoveryPoint)
	return r.finish(ctx, rg, status, nil)
}

func (r *RecoveryGroupReconciler) finish(ctx context.Context, rg *rampv1alpha1.RecoveryGroup, status rampv1alpha1.RecoveryGroupStatus, err error) (ctrl.Result, error) {
	rg.Status = status
	if uerr := r.Status().Update(ctx, rg); uerr != nil {
		return ctrl.Result{}, fmt.Errorf("updating RecoveryGroup status: %w", uerr)
	}
	return ctrl.Result{RequeueAfter: recoveryGroupResyncInterval}, err
}

func (r *RecoveryGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rampv1alpha1.RecoveryGroup{}).
		Owns(&rampv1alpha1.RecoveryPoint{}).
		Named("recoverygroup").
		Complete(r)
}
