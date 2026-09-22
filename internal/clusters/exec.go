package clusters

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// Exec runs a command in a container and returns stdout. It goes through the
// apiserver exec subresource because the management plane has no route to the
// workload pod network in this testbed; the apiserver is the only reachable
// door into a workload cluster.
func Exec(ctx context.Context, c *Cluster, namespace, pod, container string, cmd []string) (string, error) {
	req := c.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.RestConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("building executor for %s/%s: %w", namespace, pod, err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf("exec %v in %s/%s: %w (stderr: %s)", cmd, namespace, pod, err, stderr.String())
	}
	return stdout.String(), nil
}
