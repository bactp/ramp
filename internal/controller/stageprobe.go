package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
	"github.com/dcn-ssu/ramp/internal/clusters"
)

// probeStaged answers: is this checkpoint artifact actually present on a node
// of the target cluster, ready to be restored without a pull first?
//
// It is measured rather than inferred. The checkpoint artifact lives in a
// hostPath the management plane cannot reach, so the probe is a one-shot Pod
// scheduled on the target that stats the file and exits 0 or 1. One probe is
// created per (path, artifact) and then reused: the answer cannot change back
// to false for a given artifact once the agent has pulled it, so re-probing
// every reconcile would be pure noise.
func (r *RecoveryPathReconciler) probeStaged(
	ctx context.Context,
	tgt *clusters.Cluster,
	path *rampv1alpha1.RecoveryPath,
	objectKey string,
) (staged bool, reason, message string) {
	name := stageProbeName(path.Name, objectKey)
	ns := path.Spec.TargetPrereqs.Namespace
	dir := path.Spec.TargetPrereqs.CheckpointStagingPath
	if dir == "" {
		dir = "/var/lib/kubelet/checkpoints"
	}

	pod := &corev1.Pod{}
	err := tgt.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod)

	if apierrors.IsNotFound(err) {
		// Each committed epoch produces a new artifact and therefore a new probe
		// name. Retire this path's probes for every OTHER artifact first, or
		// they accumulate in the target namespace one per epoch, forever.
		r.retireStaleProbes(ctx, tgt, ns, name)
		if cerr := tgt.Client.Create(ctx, r.buildStageProbe(name, ns, dir, objectKey)); cerr != nil {
			return false, "StageProbeNotCreated", fmt.Sprintf("could not create staging probe %s/%s: %v", ns, name, cerr)
		}
		return false, "StageProbePending", fmt.Sprintf("staging probe %s/%s created; artifact not yet confirmed on a target node", ns, name)
	}
	if err != nil {
		return false, "StageProbeUnreadable", fmt.Sprintf("reading staging probe %s/%s: %v", ns, name, err)
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return true, "ArtifactStagedOnTarget",
			fmt.Sprintf("%s/%s is present on target node %s (verified by probe %s)", dir, objectKey, pod.Spec.NodeName, name)
	case corev1.PodFailed:
		// The agent syncs on an interval; a miss now just means "not yet".
		// Delete so the next reconcile re-probes rather than latching a stale no.
		_ = tgt.Client.Delete(ctx, pod, client.GracePeriodSeconds(0))
		return false, "ArtifactNotStaged",
			fmt.Sprintf("%s/%s is not yet on a target node; the checkpoint-agent has not finished syncing it", dir, objectKey)
	default:
		return false, "StageProbeRunning",
			fmt.Sprintf("staging probe %s/%s is %s", ns, name, pod.Status.Phase)
	}
}

// retireStaleProbes deletes this controller's probe Pods in the target
// namespace except the one for the artifact currently under evaluation.
func (r *RecoveryPathReconciler) retireStaleProbes(ctx context.Context, tgt *clusters.Cluster, ns, keep string) {
	pods := &corev1.PodList{}
	if err := tgt.Client.List(ctx, pods, client.InNamespace(ns),
		client.MatchingLabels{"ramp.dcn.ssu.ac.kr/component": "stage-probe"}); err != nil {
		return
	}
	for i := range pods.Items {
		if pods.Items[i].Name == keep {
			continue
		}
		_ = tgt.Client.Delete(ctx, &pods.Items[i], client.GracePeriodSeconds(0))
	}
}

func (r *RecoveryPathReconciler) buildStageProbe(name, ns, dir, objectKey string) *corev1.Pod {
	image := r.StageProbeImage
	if image == "" {
		image = "busybox:1.36"
	}
	hostPathDir := corev1.HostPathDirectoryOrCreate
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"ramp.dcn.ssu.ac.kr/component": "stage-probe",
			},
			Annotations: map[string]string{
				"ramp.dcn.ssu.ac.kr/artifact": objectKey,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Must land where the checkpoint-agent DaemonSet runs, i.e. on a
			// worker, which is also where a restore would run.
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "node-role.kubernetes.io/control-plane",
							Operator: corev1.NodeSelectorOpDoesNotExist,
						}},
					}},
				},
			}},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   image,
				Command: []string{"sh", "-c", fmt.Sprintf("test -s /staging/%s", objectKey)},
				VolumeMounts: []corev1.VolumeMount{{
					Name: "staging", MountPath: "/staging", ReadOnly: true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "staging",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: dir, Type: &hostPathDir},
				},
			}},
			TerminationGracePeriodSeconds: ptr64(0),
		},
	}
}

// stageProbeName is stable per (path, artifact) so probes are reused, and short
// enough to stay a valid object name for long checkpoint filenames.
func stageProbeName(pathName, objectKey string) string {
	h := fnv32(objectKey)
	base := fmt.Sprintf("ramp-stage-%s", pathName)
	if len(base) > 45 {
		base = base[:45]
	}
	return fmt.Sprintf("%s-%08x", base, h)
}

func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func ptr64(v int64) *int64 { return &v }

var _ = time.Second
