package k8s

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
)

// LiveDisruption is the disruption state of a workload read from the API server
// at the moment a disruptive (container-restarting) resize is about to restart a
// Ready pod. The refresh snapshot (Recommendation.PDBAllowed / Replicas) can be
// up to REFRESH_EVERY old: an HPA scale-up, a sibling going NotReady or another
// eviction in between would otherwise be invisible.
type LiveDisruption struct {
	// PDB coverage of the target pod (by ITS labels, the way the eviction API
	// matches). With several matching budgets the smallest disruptionsAllowed
	// wins (the eviction API refuses a pod covered by more than one).
	PDBName      string
	PDBProtected bool
	PDBAllowed   int
	// Controller is true for Deployment/StatefulSet/DaemonSet: Desired and Ready
	// are then the controller's spec.replicas (desiredNumberScheduled for a
	// DaemonSet) and status.readyReplicas (numberReady). False for kinds without
	// a desired count (ksvc, group, bare Pod).
	Controller bool
	Desired    int
	Ready      int
}

// ErrWorkloadGone: the controller of the pod no longer exists (deleted between
// the snapshot and the live read). Callers hold the disruptive resize.
var ErrWorkloadGone = errors.New("workload not found")

// LiveDisruption reads the PDBs of ns and the owner's controller right now. It
// only uses the "list" verb (the engine's ClusterRole has list, not get, on PDBs
// and workloads): the controller is listed with a metadata.name field selector
// and filtered by name again (fake clients ignore field selectors).
func (c *Client) LiveDisruption(ctx context.Context, ns string, owner Owner, podLabels map[string]string) (LiveDisruption, error) {
	var out LiveDisruption
	pdbs, err := c.cs.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, fmt.Errorf("list PodDisruptionBudgets in %s: %w", ns, err)
	}
	for i := range pdbs.Items {
		p := &pdbs.Items[i]
		if p.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(p.Spec.Selector)
		if err != nil || sel.Empty() || !sel.Matches(labels.Set(podLabels)) {
			continue
		}
		allowed := int(p.Status.DisruptionsAllowed)
		if !out.PDBProtected || allowed < out.PDBAllowed {
			out.PDBName, out.PDBAllowed = p.Name, allowed
		}
		out.PDBProtected = true
	}

	opts := metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", owner.Name).String()}
	found := false
	switch owner.Kind {
	case "Deployment":
		l, err := c.cs.AppsV1().Deployments(ns).List(ctx, opts)
		if err != nil {
			return out, fmt.Errorf("list Deployment %s/%s: %w", ns, owner.Name, err)
		}
		for i := range l.Items {
			if d := &l.Items[i]; d.Name == owner.Name {
				found, out.Desired, out.Ready = true, replicasOf(d.Spec.Replicas), int(d.Status.ReadyReplicas)
			}
		}
	case "StatefulSet":
		l, err := c.cs.AppsV1().StatefulSets(ns).List(ctx, opts)
		if err != nil {
			return out, fmt.Errorf("list StatefulSet %s/%s: %w", ns, owner.Name, err)
		}
		for i := range l.Items {
			if s := &l.Items[i]; s.Name == owner.Name {
				found, out.Desired, out.Ready = true, replicasOf(s.Spec.Replicas), int(s.Status.ReadyReplicas)
			}
		}
	case "DaemonSet":
		l, err := c.cs.AppsV1().DaemonSets(ns).List(ctx, opts)
		if err != nil {
			return out, fmt.Errorf("list DaemonSet %s/%s: %w", ns, owner.Name, err)
		}
		for i := range l.Items {
			if d := &l.Items[i]; d.Name == owner.Name {
				found, out.Desired, out.Ready = true, int(d.Status.DesiredNumberScheduled), int(d.Status.NumberReady)
			}
		}
	case "ksvc":
		// Knative keeps one Deployment per revision (named <revision>-deployment, see
		// knative.dev/serving pkg/reconciler/revision/resources/names) and stamps the
		// revision's labels, serving.knative.dev/service among them, on it; its
		// autoscaler writes the desired count into spec.replicas. The unit's desired
		// and ready are the sums over its revisions (old ones sit at 0). No such
		// Deployment (Knative not reconciling it yet) leaves the unit without a count,
		// as before.
		l, err := c.cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{lblKnativeService: owner.Name}).String()})
		if err != nil {
			return out, fmt.Errorf("list Deployments of Knative Service %s/%s: %w", ns, owner.Name, err)
		}
		for i := range l.Items {
			d := &l.Items[i]
			if d.Labels[lblKnativeService] != owner.Name {
				continue // fake clients may ignore selectors
			}
			found = true
			out.Desired += replicasOf(d.Spec.Replicas)
			out.Ready += int(d.Status.ReadyReplicas)
		}
		if !found {
			return out, nil
		}
	default:
		return out, nil // group / Pod: no controller with a desired count
	}
	if !found {
		return out, fmt.Errorf("%s %s/%s: %w", owner.Kind, ns, owner.Name, ErrWorkloadGone)
	}
	out.Controller = true
	return out, nil
}

// replicasOf is spec.replicas with the API default (1) for nil.
func replicasOf(r *int32) int {
	if r == nil {
		return 1
	}
	return int(*r)
}
