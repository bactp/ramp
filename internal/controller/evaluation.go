package controller

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
)

// evaluation accumulates readiness checks and turns them into a readiness
// level. The rule is a plain state machine, on purpose: Scenario 1 needs a
// result an operator can re-derive by reading the checks, not a score.
type evaluation struct {
	path   *rampv1alpha1.RecoveryPath
	checks []rampv1alpha1.ReadinessCheck
}

func (e *evaluation) add(name string, ok, mandatory bool, reason, message string) {
	status := "False"
	if ok {
		status = "True"
	}
	now := metav1.Now()
	e.checks = append(e.checks, rampv1alpha1.ReadinessCheck{
		Name:          name,
		Status:        status,
		Mandatory:     mandatory,
		Reason:        reason,
		Message:       message,
		LastProbeTime: &now,
	})
}

// conclude maps the checks onto HOT / WARM / COLD.
//
// The mapping is deliberately explicit about WHY a path is not HOT, because the
// three ways to lose HOT need three different responses:
//
//	stale prepared point      -> run a new epoch (or prepare the candidate faster)
//	preparation missing       -> prepare the target
//	target unusable           -> fix the target
//
// HOT now means what its documentation always claimed: an executable prepared
// path exists AND it currently satisfies both halves of the RecoveryContract.
// Every mandatory check is a prerequisite of the actual recovery procedure --
// including the ones the previous version omitted, which is how a path could be
// HOT while the checkpoint image the restore consumes did not exist anywhere.
func (e *evaluation) conclude() rampv1alpha1.RecoveryPathStatus {
	sort.SliceStable(e.checks, func(i, j int) bool { return e.checks[i].Name < e.checks[j].Name })

	unmet := []string{}
	byName := map[string]bool{}
	for _, c := range e.checks {
		byName[c.Name] = c.Status == "True"
		if c.Mandatory && c.Status != "True" {
			unmet = append(unmet, c.Name)
		}
	}

	now := metav1.Now()
	st := rampv1alpha1.RecoveryPathStatus{
		Checks:               e.checks,
		UnmetMandatoryChecks: unmet,
		LastValidatedTime:    &now,
	}

	targetUsable := byName[rampv1alpha1.CheckTargetClusterReachable] &&
		byName[rampv1alpha1.CheckTargetNamespaceReady] &&
		byName[rampv1alpha1.CheckTargetPlacementFeasible]
	haveExecutablePoint := byName[rampv1alpha1.CheckRecoveryPointCommitted] &&
		byName[rampv1alpha1.CheckRedisEpochArtifactAvailable] &&
		byName[rampv1alpha1.CheckVideoCheckpointAvailable]

	var reason, message string
	switch {
	case len(unmet) == 0:
		st.Readiness = rampv1alpha1.ReadinessHot
		reason = "ContractSatisfiable"
		message = "an executable prepared recovery point exists and currently satisfies both RPO and RTO; " +
			"failure-time work is activation only"

	case !targetUsable || !haveExecutablePoint:
		// Nothing executable to fall back on: either the target cannot host a
		// recovery at all, or no committed point with its artifacts exists.
		st.Readiness = rampv1alpha1.ReadinessCold
		reason = "NoExecutableRecoveryPath"
		message = fmt.Sprintf("no currently executable recovery path; unmet: %s", strings.Join(unmet, ", "))

	default:
		// Recovery is feasible from the prepared point, but the contract cannot
		// be guaranteed or preparation is incomplete. This is the case the whole
		// candidate/prepared split exists for: a stale-but-restorable point is
		// WARM, not COLD, and a newer point still being prepared does not move
		// the path at all.
		st.Readiness = rampv1alpha1.ReadinessWarm
		switch {
		case !byName[rampv1alpha1.CheckRecoveryPointFreshEnough]:
			reason = "PreparedRecoveryPointStale"
		case !byName[rampv1alpha1.CheckEstimatedRTOWithinContract]:
			reason = "EstimatedRTOExceedsContract"
		case !byName[rampv1alpha1.CheckRestoreArtifactReady] || !byName[rampv1alpha1.CheckActivationPlanPrepared]:
			reason = "PreparationOutstanding"
		default:
			reason = "ContractNotGuaranteed"
		}
		message = fmt.Sprintf("recovery is feasible but the contract is not currently guaranteed; unmet: %s",
			strings.Join(unmet, ", "))
	}

	setCondition(&st.Conditions, "Ready",
		boolCondition(st.Readiness == rampv1alpha1.ReadinessHot), reason, message, e.path.Generation)
	setCondition(&st.Conditions, "RecoveryFeasible",
		boolCondition(st.Readiness != rampv1alpha1.ReadinessCold), reason, message, e.path.Generation)
	return st
}

func boolCondition(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}
