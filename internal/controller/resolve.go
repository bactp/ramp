package controller

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rampv1alpha1 "github.com/dcn-ssu/ramp/api/v1alpha1"
)

// appBundleGVK is the existing AppBundle Operator's type. RAMP talks to it
// unstructured on purpose: it must not take a Go-module dependency on, or
// impose an API change on, a repository it does not own.
var appBundleGVK = schema.GroupVersionKind{
	Group: "app.example.com", Version: "v1alpha1", Kind: "AppBundle",
}

// ResolvedComponent is an AppBundle component mapped onto live runtime objects.
type ResolvedComponent struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	PodName    string
	NodeName   string
	PodPhase   corev1.PodPhase
	PodReady   bool
}

// GetAppBundle fetches the AppBundle that owns the application graph.
func GetAppBundle(ctx context.Context, c client.Client, ref rampv1alpha1.AppBundleReference) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(appBundleGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("AppBundle %s/%s not found in cluster %q", ref.Namespace, ref.Name, ref.Cluster)
		}
		return nil, fmt.Errorf("getting AppBundle %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return u, nil
}

// componentResourceRef digs the deployed resource out of the AppBundle status.
// This is the whole reason RAMP can avoid inventing its own application graph:
// the AppBundle Operator already records what each component actually created.
func componentResourceRef(ab *unstructured.Unstructured, group, component string) (apiVersion, kind, namespace, name string, err error) {
	groups, found, _ := unstructured.NestedSlice(ab.Object, "status", "groupStatuses")
	if !found {
		return "", "", "", "", fmt.Errorf("AppBundle %s has no status.groupStatuses yet", ab.GetName())
	}
	for _, g := range groups {
		gm, ok := g.(map[string]interface{})
		if !ok || gm["name"] != group {
			continue
		}
		comps, _, _ := unstructured.NestedSlice(gm, "componentStatuses")
		for _, cRaw := range comps {
			cm, ok := cRaw.(map[string]interface{})
			if !ok || cm["name"] != component {
				continue
			}
			rr, found, _ := unstructured.NestedMap(cm, "resourceRef")
			if !found {
				return "", "", "", "", fmt.Errorf("AppBundle component %s/%s has no status resourceRef (phase=%v)", group, component, cm["phase"])
			}
			s := func(k string) string { v, _ := rr[k].(string); return v }
			return s("apiVersion"), s("kind"), s("namespace"), s("name"), nil
		}
		return "", "", "", "", fmt.Errorf("AppBundle group %q has no component %q", group, component)
	}
	return "", "", "", "", fmt.Errorf("AppBundle %s has no group %q", ab.GetName(), group)
}

// ResolveComponent maps {group, component} to the live workload object and, for
// workload kinds, to the single Pod the recovery drivers will act on.
func ResolveComponent(ctx context.Context, c client.Client, ab *unstructured.Unstructured, ref rampv1alpha1.ComponentReference) (*ResolvedComponent, error) {
	apiVersion, kind, ns, name, err := componentResourceRef(ab, ref.Group, ref.Component)
	if err != nil {
		return nil, err
	}
	rc := &ResolvedComponent{APIVersion: apiVersion, Kind: kind, Namespace: ns, Name: name}

	// Only workload kinds have pods to act on. Services/ConfigMaps resolve to
	// the object alone -- and those are exactly the components that belong to
	// the AppBundle but NOT to the RecoveryGroup.
	if kind != "Deployment" {
		return rc, nil
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, dep); err != nil {
		return rc, fmt.Errorf("getting Deployment %s/%s for component %s/%s: %w", ns, name, ref.Group, ref.Component, err)
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return rc, fmt.Errorf("deployment %s/%s has an unusable selector: %w", ns, name, err)
	}

	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return rc, fmt.Errorf("listing pods for %s/%s: %w", ns, name, err)
	}
	running := make([]corev1.Pod, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			running = append(running, p)
		}
	}
	if len(running) == 0 {
		return rc, fmt.Errorf("component %s/%s (%s/%s) has no Running pod", ref.Group, ref.Component, ns, name)
	}
	// Deterministic choice so repeated epochs act on the same pod.
	sort.Slice(running, func(i, j int) bool { return running[i].Name < running[j].Name })
	p := running[0]

	rc.PodName = p.Name
	rc.NodeName = p.Spec.NodeName
	rc.PodPhase = p.Status.Phase
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			rc.PodReady = true
		}
	}
	return rc, nil
}

// LabelSelectorForComponent returns the AppBundle Operator's own tracking
// labels for a component. RAMP reuses them rather than introducing a second
// application-labelling scheme.
func LabelSelectorForComponent(appBundle, group, component string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{
		"app.example.com/appbundle": appBundle,
		"app.example.com/group":     group,
		"app.example.com/component": component,
	})
}
