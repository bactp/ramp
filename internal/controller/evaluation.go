package controller

import (
	"fmt"
	"sort"
	"strings"
	"time"

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

// Per-stage activation costs used to derive estimatedRTO. These are measured
// lab constants, not predictions: they say "here is the work still outstanding
// at this readiness level", which is what the readiness level means.
const (
	costActivateRedisPromotion = 2 * time.Second
	costRestoreStagedVideo     = 15 * time.Second
	costPullArtifactToNode     = 30 * time.Second // checkpoint-agent PULL_INTERVAL
	costColdRebuild            = 5 * time.Minute
)

func (e *evaluation) conclude(rp *rampv1alpha1.RecoveryPoint) rampv1alpha1.RecoveryPathStatus {
	sort.SliceStable(e.checks, func(i, j int) bool { return e.checks[i].Name < e.checks[j].Name })

	unmet := []string{}
	byName := map[string]bool{}
	for _, c := range e.checks {
		byName[c.Name] = c.Status == "True"
		if c.Mandatory && c.Status != "True" {
			unmet = append(unmet, c.Name)
		}
	}

	st := rampv1alpha1.RecoveryPathStatus{
		Checks:               e.checks,
		UnmetMandatoryChecks: unmet,
	}
	now := metav1.Now()
	st.LastValidatedTime = &now
	if rp != nil {
		st.ObservedRecoveryPoint = rp.Name
		st.ObservedEpoch = rp.Spec.Epoch
	}

	// ---- the state machine ------------------------------------------------
	//   HOT  : every mandatory prerequisite holds; failure-time work is activation
	//   WARM : a usable RecoveryPoint exists and recovery is feasible, but
	//          preparation actions are still outstanding
	//   COLD : no sufficiently prepared target path exists
	hasRecoveryPoint := byName[rampv1alpha1.CheckRecoveryPointCommitted]
	targetUsable := byName[rampv1alpha1.CheckTargetClusterReachable]

	var reason, message string
	switch {
	case len(unmet) == 0:
		st.Readiness = rampv1alpha1.ReadinessHot
		st.EstimatedRTO = (costActivateRedisPromotion + costRestoreStagedVideo).String()
		reason = "AllPrerequisitesMet"
		message = "all mandatory prerequisites hold; failure-time work is activation only"
	case hasRecoveryPoint && targetUsable:
		st.Readiness = rampv1alpha1.ReadinessWarm
		est := costActivateRedisPromotion + costRestoreStagedVideo
		if !byName[rampv1alpha1.CheckVideoCheckpointStaged] {
			est += costPullArtifactToNode
		}
		st.EstimatedRTO = est.String()
		reason = "PreparationOutstanding"
		message = fmt.Sprintf("recovery is feasible from %s but %d preparation action(s) remain: %s",
			st.ObservedRecoveryPoint, len(unmet), strings.Join(unmet, ", "))
	default:
		st.Readiness = rampv1alpha1.ReadinessCold
		st.EstimatedRTO = costColdRebuild.String()
		reason = "NoPreparedPath"
		message = fmt.Sprintf("no sufficiently prepared target path exists; unmet: %s", strings.Join(unmet, ", "))
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
