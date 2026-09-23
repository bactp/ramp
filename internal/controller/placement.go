package controller

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
	"github.com/dcn-ssu/ramp/internal/clusters"
)

// nodeCandidate is one target node with everything the placement decision needs.
type nodeCandidate struct {
	name              string
	ready             bool
	schedulable       bool
	restoreCapable    bool
	allocCPUMillis    int64
	allocMemMiB       int64
	requestedCPUMilli int64
	requestedMemMiB   int64
	rejected          string // empty means feasible
}

func (n nodeCandidate) freeCPU() int64 { return n.allocCPUMillis - n.requestedCPUMilli }
func (n nodeCandidate) freeMem() int64 { return n.allocMemMiB - n.requestedMemMiB }

// selectPlacement picks ONE concrete target node that satisfies every
// node-level prerequisite simultaneously.
//
// The predecessor of this function compared max(CPU over nodes) and
// max(memory over nodes) independently, which can describe a node that does not
// exist -- CPU from one machine, memory from another. A recovery lands on one
// node or it does not land at all, so the decision has to be made per node.
//
// "Free" here is allocatable minus the resource REQUESTS of the non-terminated
// pods already on the node, which is what a scheduler would consider available.
// Allocatable on its own is capacity after system reservations, not free
// capacity, and reporting it as free is how a path claims room it does not have.
//
// No optimiser: the currently prepared node wins while it stays feasible (so
// placement does not flap and preparation is not invalidated for nothing),
// otherwise the first feasible node in name order.
func selectPlacement(
	ctx context.Context,
	tgt *clusters.Cluster,
	path *rampv1alpha1.RecoveryPath,
	preferNode, preferReason string,
) (*rampv1alpha1.TargetPlacement, []nodeCandidate, error) {

	nodes := &corev1.NodeList{}
	if err := tgt.Client.List(ctx, nodes); err != nil {
		return nil, nil, fmt.Errorf("listing nodes on %s: %w", path.Spec.TargetCluster, err)
	}

	// Which nodes can actually run the restore: the checkpoint-agent's pods are
	// the restore capability, and capability is per node, not per cluster.
	capable := map[string]bool{}
	capabilityKnown := false
	if ref := path.Spec.TargetPrereqs.RestoreCapabilityDaemonSet; ref != nil {
		ds := &appsv1.DaemonSet{}
		if err := tgt.Client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, ds); err == nil {
			pods := &corev1.PodList{}
			if err := tgt.Client.List(ctx, pods, client.InNamespace(ref.Namespace),
				client.MatchingLabels(ds.Spec.Selector.MatchLabels)); err == nil {
				capabilityKnown = true
				for i := range pods.Items {
					p := &pods.Items[i]
					if p.Spec.NodeName == "" || p.Status.Phase != corev1.PodRunning {
						continue
					}
					for _, cs := range p.Status.ContainerStatuses {
						if cs.Ready {
							capable[p.Spec.NodeName] = true
						}
					}
				}
			}
		}
	}

	req := path.Spec.TargetPrereqs
	var cands []nodeCandidate
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if req.Node != "" && n.Name != req.Node {
			continue
		}
		c := nodeCandidate{name: n.Name, schedulable: !n.Spec.Unschedulable}
		// Control-plane nodes are excluded unless the path pins one explicitly.
		// They are not where workloads run here, and the staging job and restore
		// job both avoid them -- so choosing one would put the placement
		// decision and the artifacts on different machines, which is exactly the
		// split this function exists to prevent.
		_, isControlPlane := n.Labels["node-role.kubernetes.io/control-plane"]
		if isControlPlane && req.Node == "" {
			c.rejected = "control-plane node (pin it via spec.targetPrereqs.node to use it anyway)"
			cands = append(cands, c)
			continue
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				c.ready = true
			}
		}
		cpu := n.Status.Allocatable[corev1.ResourceCPU]
		mem := n.Status.Allocatable[corev1.ResourceMemory]
		c.allocCPUMillis = cpu.MilliValue()
		c.allocMemMiB = mem.Value() / (1024 * 1024)
		c.restoreCapable = !capabilityKnown || capable[n.Name]

		// Sum the requests already committed on this node.
		pods := &corev1.PodList{}
		if err := tgt.Client.List(ctx, pods, client.MatchingFields{"spec.nodeName": n.Name}); err != nil {
			// The field index is only available if it was registered; fall back
			// to listing everything and filtering, rather than silently
			// reporting a node as emptier than it is.
			all := &corev1.PodList{}
			if lerr := tgt.Client.List(ctx, all); lerr == nil {
				pods.Items = nil
				for j := range all.Items {
					if all.Items[j].Spec.NodeName == n.Name {
						pods.Items = append(pods.Items, all.Items[j])
					}
				}
			}
		}
		for j := range pods.Items {
			p := &pods.Items[j]
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			for _, ctr := range p.Spec.Containers {
				rc := ctr.Resources.Requests[corev1.ResourceCPU]
				rm := ctr.Resources.Requests[corev1.ResourceMemory]
				c.requestedCPUMilli += rc.MilliValue()
				c.requestedMemMiB += rm.Value() / (1024 * 1024)
			}
		}

		switch {
		case !c.ready:
			c.rejected = "node is not Ready"
		case !c.schedulable:
			c.rejected = "node is cordoned"
		case !c.restoreCapable:
			c.rejected = "no ready restore-capability agent on this node"
		case req.MinAllocatableCPUMillis > 0 && c.freeCPU() < req.MinAllocatableCPUMillis:
			c.rejected = fmt.Sprintf("free CPU %dm < required %dm", c.freeCPU(), req.MinAllocatableCPUMillis)
		case req.MinAllocatableMemoryMiB > 0 && c.freeMem() < req.MinAllocatableMemoryMiB:
			c.rejected = fmt.Sprintf("free memory %dMiB < required %dMiB", c.freeMem(), req.MinAllocatableMemoryMiB)
		}
		cands = append(cands, c)
	}

	sort.Slice(cands, func(i, j int) bool { return cands[i].name < cands[j].name })

	pick := func(name string) *nodeCandidate {
		for i := range cands {
			if cands[i].name == name && cands[i].rejected == "" {
				return &cands[i]
			}
		}
		return nil
	}
	chosen := pick(preferNode)
	reason := preferReason
	if chosen == nil {
		for i := range cands {
			if cands[i].rejected == "" {
				chosen = &cands[i]
				reason = "FirstFeasibleNode"
				break
			}
		}
	}
	if chosen == nil {
		return nil, cands, nil
	}
	now := metav1.Now()
	return &rampv1alpha1.TargetPlacement{
		Cluster:              path.Spec.TargetCluster,
		Node:                 chosen.name,
		AllocatableCPUMillis: chosen.allocCPUMillis,
		AllocatableMemoryMiB: chosen.allocMemMiB,
		RequestedCPUMillis:   chosen.requestedCPUMilli,
		RequestedMemoryMiB:   chosen.requestedMemMiB,
		FreeCPUMillis:        chosen.freeCPU(),
		FreeMemoryMiB:        chosen.freeMem(),
		SelectedAt:           &now,
		Reason:               reason,
	}, cands, nil
}

// describeRejections renders why no node was feasible.
func describeRejections(cands []nodeCandidate) string {
	if len(cands) == 0 {
		return "no target node matched the path's node constraint"
	}
	out := ""
	for i, c := range cands {
		if i > 0 {
			out += "; "
		}
		r := c.rejected
		if r == "" {
			r = "feasible"
		}
		out += fmt.Sprintf("%s: %s", c.name, r)
	}
	return out
}

var _ = fields.OneTermEqualSelector
