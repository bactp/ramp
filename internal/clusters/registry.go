// Package clusters holds the management-plane view of the workload clusters
// RAMP reasons about. RAMP runs its own manager against the management cluster
// (where its CRDs live, next to the Transition Operator's) and reaches into
// workload clusters with per-cluster clients, exactly as the Transition
// Operator already does via CAPI kubeconfig secrets.
package clusters

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cluster is one registered workload cluster.
type Cluster struct {
	Name       string
	Kubeconfig string
	RestConfig *rest.Config
	Client     client.Client
	Clientset  *kubernetes.Clientset
}

// Registry maps registered cluster names to clients.
type Registry struct {
	byName map[string]*Cluster
}

// Flag implements flag.Value so the manager can take repeated
// --cluster=name=/path/to/kubeconfig arguments.
type Flag struct {
	Entries []string
}

func (f *Flag) String() string { return strings.Join(f.Entries, ",") }

func (f *Flag) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("expected name=path, got %q", v)
	}
	f.Entries = append(f.Entries, v)
	return nil
}

// NewRegistry builds a Registry from name=path entries using the given scheme.
func NewRegistry(entries []string, opts client.Options) (*Registry, error) {
	r := &Registry{byName: map[string]*Cluster{}}
	for _, e := range entries {
		parts := strings.SplitN(e, "=", 2)
		name, path := parts[0], parts[1]
		cfg, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: loading kubeconfig %s: %w", name, path, err)
		}
		// These are control-plane probes, not data-path calls: fail fast so an
		// unreachable target surfaces as a readiness check rather than a hang.
		cfg.Timeout = defaultTimeout
		c, err := client.New(cfg, opts)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: building client: %w", name, err)
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: building clientset: %w", name, err)
		}
		r.byName[name] = &Cluster{
			Name: name, Kubeconfig: path, RestConfig: cfg, Client: c, Clientset: cs,
		}
	}
	return r, nil
}

// Get returns the registered cluster or an error naming what is registered, so
// a misconfigured RecoveryPath produces an actionable status message.
func (r *Registry) Get(name string) (*Cluster, error) {
	if c, ok := r.byName[name]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("cluster %q is not registered (known: %s)", name, strings.Join(r.Names(), ", "))
}

// Names lists registered cluster names in a stable order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
