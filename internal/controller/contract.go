package controller

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
)

// Activation cost assumptions.
//
// These are MEASURED lab constants from the Q>P experiment
// (evidence/recovery-epoch-qgtp-20260922T084434/timing.json), not predictions
// and not a learned model. They are stated here, published in
// status.contract.activationSteps on every evaluation, and documented in
// docs/ramp-scenario1/contract-aware-readiness.md, so the estimate can be
// re-derived by hand rather than trusted.
//
// The rule is: estimatedActivationLatency = sum of the steps still REQUIRED at
// failure time. Preparation removes steps from that sum; it never makes the
// remaining ones cheaper.
const (
	// --- always on the failure-time path (activation itself) ---
	costRedisEpochRestore = 3 * time.Second // fetch RDB, detach, load, restart, verify at P
	costActivationCommit  = 2 * time.Second // one git commit + one ArgoCD sync trigger
	costContainerRestore  = 6 * time.Second // ArgoCD apply -> containerd CRIU restore -> pod ready
	costVerifyAndRelease  = 1 * time.Second // verify restored position, release the quiesce

	// --- only required when preparation has NOT been done ---
	costStageArtifactToNode = 30 * time.Second // checkpoint-agent PULL_INTERVAL
	costBuildCheckpointImg  = 45 * time.Second // export rootfs + assemble + ctr import on the node
	costPrepareActivation   = 25 * time.Second // write DR manifests, ArgoCD converge on replicas:0
	costProvisionRedisRT    = 60 * time.Second // deploy a Redis runtime on the target from nothing
	costRebuildFromScratch  = 5 * time.Minute  // no usable recovery point: full redeploy
)

// contractInputs is everything the contract evaluation needs.
type contractInputs struct {
	contract rampv1alpha1.RecoveryContract
	// prepared is the point readiness is evaluated against; nil means none.
	prepared *rampv1alpha1.RecoveryPointRef
	// met reports, per readiness check name, whether it currently holds.
	met map[string]bool
	now time.Time
}

// evaluateContract turns the check results into the two contract verdicts.
//
// RPO and RTO are kept as separate dimensions on purpose. A prepared point can
// be perfectly restorable and too old; a fresh point can be unprepared. Folding
// them into one "ready" bit hides which of the two an operator has to fix, and
// the two are fixed by completely different actions (run an epoch vs. prepare
// the target).
func evaluateContract(in contractInputs) rampv1alpha1.ContractStatus {
	st := rampv1alpha1.ContractStatus{
		RPO: in.contract.RPO,
		RTO: in.contract.RTO,
	}

	// ---------------------------------------------------------------- RPO --
	// Freshness is measured in TIME, from the epoch's commit, because that is
	// what an RPO is: how much application history the recovery may lose. The
	// logical position tells you WHERE the point is, not how stale it is.
	switch {
	case in.prepared == nil || in.prepared.CommitTime == nil:
		st.FreshEnough = false
	case in.contract.RPO.Duration <= 0:
		// No RPO declared: freshness cannot fail a contract that does not exist.
		st.RecoveryPointAge = metav1.Duration{Duration: in.now.Sub(in.prepared.CommitTime.Time).Round(time.Second)}
		st.FreshEnough = true
	default:
		age := in.now.Sub(in.prepared.CommitTime.Time).Round(time.Second)
		if age < 0 {
			age = 0
		}
		st.RecoveryPointAge = metav1.Duration{Duration: age}
		remaining := in.contract.RPO.Duration - age
		if remaining < 0 {
			remaining = 0
		}
		st.FreshnessRemaining = metav1.Duration{Duration: remaining}
		st.FreshEnough = age <= in.contract.RPO.Duration
	}

	// ---------------------------------------------------------------- RTO --
	steps := activationSteps(in.met, in.prepared != nil)
	var total time.Duration
	for _, s := range steps {
		if s.Required {
			total += s.Cost.Duration
		}
	}
	st.ActivationSteps = steps
	st.EstimatedActivationLatency = metav1.Duration{Duration: total}
	// An undeclared RTO cannot be violated.
	st.RTOWithinContract = in.contract.RTO.Duration <= 0 || total <= in.contract.RTO.Duration
	return st
}

// activationSteps enumerates the failure-time work, marking each step as
// required or already paid for by preparation.
func activationSteps(met map[string]bool, havePrepared bool) []rampv1alpha1.ActivationStep {
	step := func(name string, cost time.Duration, required bool, reason string) rampv1alpha1.ActivationStep {
		return rampv1alpha1.ActivationStep{
			Name: name, Cost: metav1.Duration{Duration: cost}, Required: required, Reason: reason,
		}
	}
	if !havePrepared {
		return []rampv1alpha1.ActivationStep{
			step("RebuildFromScratch", costRebuildFromScratch, true,
				"no prepared RecoveryPoint: recovery would be a redeploy, not an activation"),
		}
	}

	out := []rampv1alpha1.ActivationStep{}

	// Preparation shortfalls first: these are the steps that would have to
	// happen at failure time because they did not happen before it.
	out = append(out, step("ProvisionRedisRuntime", costProvisionRedisRT,
		!met[rampv1alpha1.CheckRedisTargetRuntimeReady],
		"target Redis runtime already up and warm"))
	out = append(out, step("StageCheckpointArtifact", costStageArtifactToNode,
		!met[rampv1alpha1.CheckRestoreArtifactReady],
		"checkpoint tar present on the placement node"))
	out = append(out, step("BuildCheckpointImage", costBuildCheckpointImg,
		!met[rampv1alpha1.CheckRestoreArtifactReady],
		"CRI checkpoint image present on the placement node"))
	out = append(out, step("PrepareActivationPlan", costPrepareActivation,
		!met[rampv1alpha1.CheckActivationPlanPrepared],
		"target workload object and ArgoCD wiring already in place"))

	// Activation itself: always required, never removable by preparation.
	out = append(out, step("RestoreRedisFromEpochArtifact", costRedisEpochRestore, true,
		"fetch the epoch RDB, load it, restart, verify the position is P"))
	out = append(out, step("CommitAndSyncActivation", costActivationCommit, true,
		"one commit switching image and replicas, one sync trigger"))
	out = append(out, step("RestoreContainerFromCheckpoint", costContainerRestore, true,
		"containerd CRIU restore to pod ready"))
	out = append(out, step("VerifyAndRelease", costVerifyAndRelease, true,
		"verify the restored position is P, then release the quiesce"))
	return out
}

// freshnessMessage renders the RPO verdict for the readiness check.
func freshnessMessage(st rampv1alpha1.ContractStatus, prepared *rampv1alpha1.RecoveryPointRef) (string, string) {
	if prepared == nil {
		return "NoPreparedRecoveryPoint", "no prepared RecoveryPoint to measure freshness against"
	}
	if st.RPO.Duration <= 0 {
		return "NoRPODeclared", fmt.Sprintf("%s is %s old; the RecoveryGroup declares no RPO",
			prepared.Name, st.RecoveryPointAge.Duration)
	}
	if st.FreshEnough {
		return "WithinRPO", fmt.Sprintf("%s age %s <= RPO %s (%s of freshness remaining)",
			prepared.Name, st.RecoveryPointAge.Duration, st.RPO.Duration, st.FreshnessRemaining.Duration)
	}
	return "RecoveryPointStale", fmt.Sprintf(
		"%s age %s > RPO %s: the prepared point is still fully restorable, but recovering from it would lose more than the contract allows",
		prepared.Name, st.RecoveryPointAge.Duration, st.RPO.Duration)
}

// rtoMessage renders the RTO verdict for the readiness check.
func rtoMessage(st rampv1alpha1.ContractStatus) (string, string) {
	var outstanding []string
	for _, s := range st.ActivationSteps {
		if s.Required && s.Name != "RestoreRedisFromEpochArtifact" && s.Name != "CommitAndSyncActivation" &&
			s.Name != "RestoreContainerFromCheckpoint" && s.Name != "VerifyAndRelease" {
			outstanding = append(outstanding, s.Name)
		}
	}
	detail := "failure-time work is activation only"
	if len(outstanding) > 0 {
		detail = "preparation still outstanding at failure time: " + joinLines(outstanding)
	}
	if st.RTO.Duration <= 0 {
		return "NoRTODeclared", fmt.Sprintf("estimated activation latency %s; the RecoveryGroup declares no RTO (%s)",
			st.EstimatedActivationLatency.Duration, detail)
	}
	if st.RTOWithinContract {
		return "WithinRTO", fmt.Sprintf("estimated activation latency %s <= RTO %s (%s)",
			st.EstimatedActivationLatency.Duration, st.RTO.Duration, detail)
	}
	return "EstimatedRTOExceeded", fmt.Sprintf("estimated activation latency %s > RTO %s (%s)",
		st.EstimatedActivationLatency.Duration, st.RTO.Duration, detail)
}
