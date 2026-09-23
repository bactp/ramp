package controller

import (
	"encoding/json"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
)

// faultInjection forces a specific stage of the epoch to fail.
//
// It exists so that the negative tests -- "a failed capture must never commit,
// and must always resume the application" -- can be executed against the real
// controller instead of being argued on paper. It is deliberately NOT part of
// the API: it arrives as a JSON annotation, so the CRD a user sees offers no
// way to ask for a corrupted epoch.
//
//	kubectl annotate recoverypoint <name> \
//	  ramp.dcn.ssu.ac.kr/test-fault-injection='{"failVideoCheckpoint":true}'
type faultInjection struct {
	// FailRedisSnapshot aborts the epoch during CAPTURE REDIS.
	FailRedisSnapshot bool `json:"failRedisSnapshot,omitempty"`
	// FailVideoCheckpoint aborts the epoch during CAPTURE VIDEO, i.e. AFTER the
	// Redis epoch artifact already exists.
	FailVideoCheckpoint bool `json:"failVideoCheckpoint,omitempty"`
	// ForcePositionSkew perturbs the recorded Redis artifact position, so
	// VALIDATE sees two artifacts that disagree.
	ForcePositionSkew int64 `json:"forcePositionSkew,omitempty"`
	// SkipQuiesce runs the epoch the old way -- without pausing the application
	// -- so the pre-fix behaviour stays reproducible for comparison.
	SkipQuiesce bool `json:"skipQuiesce,omitempty"`
}

// parseFaultInjection reads the test-only annotation. Anything unparseable is
// treated as "no injection": a malformed test directive must never silently
// change how a real epoch behaves.
func parseFaultInjection(rp *rampv1alpha1.RecoveryPoint) *faultInjection {
	fi := &faultInjection{}
	raw, ok := rp.Annotations[rampv1alpha1.AnnFaultInjection]
	if !ok || raw == "" {
		return fi
	}
	var parsed faultInjection
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return fi
	}
	return &parsed
}
