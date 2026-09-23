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

// The restore-artifact probe.
//
// It answers one question about ONE node: can the video member be restored
// HERE, right now, without fetching or building anything?
//
// Two things have to be true, and the old stage probe checked neither
// completely:
//
//   - the checkpoint tar is staged in the kubelet checkpoint directory ON THIS
//     NODE. The old probe let the scheduler place it on any worker, so it could
//     confirm the artifact on a node the workload would never run on.
//   - the CRI checkpoint IMAGE built from that tar is in this node's containerd.
//     That image is what the restore actually consumes: the Q>P recovery runs
//     35-build-checkpoint-image.sh before it can restore anything. A path whose
//     tar exists but whose image does not is not activation-ready, and calling
//     it HOT is the defect this check exists to remove.
//
// Neither fact is visible from the management plane: the tar is in a hostPath
// and containerd's image store is not in the Node object (kubelet reports only
// the 50 largest images, and a 10 MB checkpoint image is never among them --
// verified on this testbed). So it is measured by a node-pinned pod.
//
// Unlike the old probe the answer is NOT latched. An image can be removed after
// it was confirmed, which is exactly the degradation Test E exercises, so the
// probe is re-run once its result is older than preparationProbeTTL.
//
// Re-running must not create a hole. A first attempt deleted the completed pod
// and created a replacement, which left every refresh cycle with a window where
// no terminal result existed -- the check went False, and the path dropped out
// of HOT roughly once a minute purely because it was re-measuring. That is the
// mirror image of the defect this work removes: readiness lost for no reason.
//
// So probes are generational. Each identity (path, node, tar, image) gets one
// pod per TTL-sized time bucket; the verdict is the newest TERMINAL pod of that
// identity, and the previous generation is kept until the new one has finished.
// Coverage is therefore continuous, and a verdict is at most ~2xTTL old.
const preparationProbeTTL = 45 * time.Second

// Probe exit codes, so the controller can report WHICH half is missing without
// having to read pod logs across clusters.
const (
	probeExitOK           = 0
	probeExitTarMissing   = 10
	probeExitImageMissing = 11
)

type restoreArtifactProbe struct {
	done      bool
	ok        bool
	tarOK     bool
	imageOK   bool
	node      string
	reason    string
	message   string
	probeName string
	// keep names every probe pod this evaluation still needs, so the reconcile's
	// single cleanup pass does not delete the generation currently in flight or
	// the one still providing the verdict.
	keep []string
}

// probeRestoreArtifact creates or reads the node-pinned probe for
// (path, node, tar, image).
func (r *RecoveryPathReconciler) probeRestoreArtifact(
	ctx context.Context,
	tgt *clusters.Cluster,
	path *rampv1alpha1.RecoveryPath,
	node, objectKey, image string,
) restoreArtifactProbe {

	ns := path.Spec.TargetPrereqs.Namespace
	dir := path.Spec.TargetPrereqs.CheckpointStagingPath
	if dir == "" {
		dir = "/var/lib/kubelet/checkpoints"
	}
	identity := node + "|" + objectKey + "|" + image
	bucket := time.Now().Unix() / int64(preparationProbeTTL/time.Second)
	name := probeName(path.Name, identity, bucket)
	prev := probeName(path.Name, identity, bucket-1)
	res := restoreArtifactProbe{node: node, probeName: name, keep: []string{name, prev}}

	// Always make sure the CURRENT generation exists, so the answer keeps
	// refreshing without ever being absent.
	cur := &corev1.Pod{}
	curErr := tgt.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, cur)
	if apierrors.IsNotFound(curErr) {
		if cerr := tgt.Client.Create(ctx, r.buildProbe(name, ns, node, dir, objectKey, image, identity)); cerr != nil &&
			!apierrors.IsAlreadyExists(cerr) {
			res.reason, res.message = "ProbeNotCreated",
				fmt.Sprintf("could not create restore-artifact probe %s/%s on node %s: %v", ns, name, node, cerr)
			return res
		}
	} else if curErr != nil {
		res.reason, res.message = "ProbeUnreadable", fmt.Sprintf("reading probe %s/%s: %v", ns, name, curErr)
		return res
	}

	// The verdict is the newest TERMINAL pod for this identity, whichever
	// generation it belongs to.
	pod := newestTerminalProbe(ctx, tgt, ns, identity)
	if pod == nil {
		res.reason, res.message = "ProbePending",
			fmt.Sprintf("restore-artifact probe %s/%s on node %s has not produced a result yet", ns, name, node)
		return res
	}

	{
		res.done = true
		code := int32(-1)
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil {
				code = cs.State.Terminated.ExitCode
			}
		}
		res.keep = append(res.keep, pod.Name)
		switch code {
		case probeExitOK:
			res.ok, res.tarOK, res.imageOK = true, true, true
			res.reason = "RestoreArtifactReadyOnNode"
			res.message = fmt.Sprintf("checkpoint tar %s and CRI checkpoint image %s are both present on node %s",
				objectKey, image, node)
		case probeExitTarMissing:
			res.reason = "CheckpointTarMissingOnNode"
			res.message = fmt.Sprintf("%s/%s is not staged on node %s", dir, objectKey, node)
		case probeExitImageMissing:
			res.tarOK = true
			res.reason = "CheckpointImageMissingOnNode"
			res.message = fmt.Sprintf("the checkpoint tar is staged on node %s but the CRI checkpoint image %s is not in its containerd; "+
				"restoring would first have to build it", node, image)
		default:
			res.reason = "ProbeInconclusive"
			res.message = fmt.Sprintf("probe %s/%s exited %d on node %s", ns, name, code, node)
		}
		if pod.Status.StartTime != nil {
			age := time.Since(pod.Status.StartTime.Time).Round(time.Second)
			res.message += fmt.Sprintf(" (measured %s ago)", age)
		}
		return res
	}
}

// newestTerminalProbe returns the most recently STARTED terminal probe pod for
// one artifact identity, or nil if none has finished yet.
func newestTerminalProbe(ctx context.Context, tgt *clusters.Cluster, ns, identity string) *corev1.Pod {
	pods := &corev1.PodList{}
	if err := tgt.Client.List(ctx, pods, client.InNamespace(ns),
		client.MatchingLabels{"ramp.dcn.ssu.ac.kr/component": "stage-probe"}); err != nil {
		return nil
	}
	var best *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Annotations["ramp.dcn.ssu.ac.kr/identity"] != identity {
			continue
		}
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			continue
		}
		if best == nil || (p.Status.StartTime != nil && best.Status.StartTime != nil &&
			p.Status.StartTime.After(best.Status.StartTime.Time)) {
			best = p
		}
	}
	return best
}

// retireStaleProbes deletes this controller's probes that no evaluation in this
// reconcile is using.
//
// It takes a SET, not a single name. A reconcile legitimately probes two
// RecoveryPoints at once -- the prepared one and the candidate being prepared --
// and an earlier version that kept only "the current probe" had the two
// evaluations delete each other's pods on every pass, so neither ever reached a
// terminal phase and both were permanently inconclusive.
func (r *RecoveryPathReconciler) retireStaleProbes(ctx context.Context, tgt *clusters.Cluster, ns string, keep map[string]bool) {
	pods := &corev1.PodList{}
	if err := tgt.Client.List(ctx, pods, client.InNamespace(ns),
		client.MatchingLabels{"ramp.dcn.ssu.ac.kr/component": "stage-probe"}); err != nil {
		return
	}
	for i := range pods.Items {
		if keep[pods.Items[i].Name] {
			continue
		}
		_ = tgt.Client.Delete(ctx, &pods.Items[i], client.GracePeriodSeconds(0))
	}
}

func (r *RecoveryPathReconciler) buildProbe(name, ns, node, dir, objectKey, image, identity string) *corev1.Pod {
	img := r.StageProbeImage
	if img == "" {
		img = "busybox:1.36"
	}
	hostPathDir := corev1.HostPathDirectory
	tr := true
	// chroot into the host to reach containerd: `ctr` and the containerd socket
	// live on the node, and the image store is not exposed through any API the
	// management plane can reach.
	script := fmt.Sprintf(
		`test -s /host%s/%s || exit %d; `+
			`chroot /host ctr -n k8s.io images ls -q 2>/dev/null | grep -qx %q || exit %d; `+
			`echo OK`,
		dir, objectKey, probeExitTarMissing, image, probeExitImageMissing)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"ramp.dcn.ssu.ac.kr/component": "stage-probe"},
			Annotations: map[string]string{
				"ramp.dcn.ssu.ac.kr/artifact": objectKey,
				"ramp.dcn.ssu.ac.kr/image":    image,
				"ramp.dcn.ssu.ac.kr/node":     node,
				"ramp.dcn.ssu.ac.kr/identity": identity,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// PINNED, not scheduled. Readiness evidence from another node is not
			// evidence for this path.
			NodeName:    node,
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           img,
				Command:         []string{"sh", "-c", script},
				SecurityContext: &corev1.SecurityContext{Privileged: &tr},
				VolumeMounts:    []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "host",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/", Type: &hostPathDir}},
			}},
			TerminationGracePeriodSeconds: ptr64(0),
		},
	}
}

// probeName is stable per (path, artifact identity, generation). The identity
// makes a new epoch, a new image or a placement move produce a fresh answer
// instead of inheriting an old one; the generation makes the answer refresh
// without ever going absent.
func probeName(pathName, identity string, bucket int64) string {
	h := fnv32(identity)
	base := "ramp-prep-" + pathName
	if len(base) > 40 {
		base = base[:40]
	}
	return fmt.Sprintf("%s-%08x-%d", base, h, bucket%1000)
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
