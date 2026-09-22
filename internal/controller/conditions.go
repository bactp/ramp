package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// setCondition upserts a condition, preserving lastTransitionTime when the
// status has not actually changed so that `kubectl get -o yaml` shows when a
// state was entered rather than when it was last re-observed.
func setCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, generation int64) {
	if reason == "" {
		reason = "Unknown"
	}
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}
