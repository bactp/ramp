// Package drivers implements the per-member recovery mechanisms. A driver
// knows how to CAPTURE a member's recovery state and how to report whether its
// prerequisites hold; it never decides policy and never performs a transition.
package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"k8s.io/client-go/rest"
)

// CheckpointResult is what one kubelet Container Checkpoint call produced.
type CheckpointResult struct {
	// NodePath is the absolute path the kubelet wrote on the node.
	NodePath string
	// ObjectKey is the artifact-store key the checkpoint-agent will upload it
	// under: the agent uses the file's basename.
	ObjectKey string
	// Node is the node the container was running on.
	Node string
	// Started/Completed bound the kubelet call itself.
	Started   time.Time
	Completed time.Time
}

// kubeletCheckpointResponse matches the kubelet Container Checkpoint API reply.
type kubeletCheckpointResponse struct {
	Items []string `json:"items"`
}

// ContainerCheckpoint captures a member by invoking the kubelet Container
// Checkpoint API -- the same endpoint the Transition Operator's
// CheckpointReconciler calls.
//
// TRANSPORT NOTE. The Transition Operator dials the kubelet directly at
// https://<NodeInternalIP>:10250. In this testbed that cannot work: the
// workload node network 10.6.0.0/24 is not routable from the management plane,
// and the Kubernetes Node objects carry no ExternalIP to fall back to (the
// reachable floating IP exists only on the mgmt-side CAPI Machine). See
// docs/ramp-scenario1/00-existing-system-audit.md Sec.1.1 and Sec.3.3.
//
// RAMP therefore reaches the SAME kubelet endpoint through the workload
// apiserver's node proxy, which is reachable wherever the apiserver is. This is
// a transport adaptation, not a second checkpoint implementation: same API,
// same on-node artifact, same checkpoint-agent -> MinIO pipeline afterwards.
type ContainerCheckpoint struct {
	// RestConfig addresses the SOURCE workload cluster's apiserver.
	RestConfig *rest.Config
}

// Capture performs the checkpoint and returns where the artifact landed.
func (d *ContainerCheckpoint) Capture(ctx context.Context, node, namespace, pod, container string) (*CheckpointResult, error) {
	if node == "" || namespace == "" || pod == "" || container == "" {
		return nil, fmt.Errorf("checkpoint requires node/namespace/pod/container, got %q/%q/%q/%q", node, namespace, pod, container)
	}

	cfg := rest.CopyConfig(d.RestConfig)
	// Deliberately leave cfg.Timeout at zero. client-go turns it into a
	// ?timeout= query parameter, and the kubelet's checkpoint handler rejects
	// the request with "cannot parse value of timeout parameter" rather than
	// ignoring it. The bound comes from the caller's context instead.
	cfg.Timeout = 0
	rc, err := rest.RESTClientFor(withCoreV1Defaults(cfg))
	if err != nil {
		return nil, fmt.Errorf("building REST client for checkpoint: %w", err)
	}

	// A checkpoint of a live container is a stop-the-world CRIU dump; give it
	// room but never unbounded.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	started := time.Now()
	raw, err := rc.Post().
		Resource("nodes").
		Name(node).
		SubResource("proxy").
		Suffix("checkpoint", namespace, pod, container).
		DoRaw(ctx)
	completed := time.Now()
	if err != nil {
		return nil, fmt.Errorf("kubelet checkpoint %s/%s/%s on %s: %w (body: %s)",
			namespace, pod, container, node, err, truncate(string(raw), 400))
	}

	var resp kubeletCheckpointResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parsing kubelet checkpoint reply %q: %w", truncate(string(raw), 400), err)
	}
	if len(resp.Items) == 0 {
		return nil, fmt.Errorf("kubelet reported no checkpoint artifact for %s/%s/%s", namespace, pod, container)
	}

	nodePath := resp.Items[0]
	return &CheckpointResult{
		NodePath:  nodePath,
		ObjectKey: path.Base(nodePath),
		Node:      node,
		Started:   started,
		Completed: completed,
	}, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
