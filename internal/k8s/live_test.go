package k8s

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func pdbFor(name string, allowed int32, sel map[string]string) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: sel}},
		Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: allowed},
	}
}

func TestLiveDisruption(t *testing.T) {
	two := int32(2)
	cs := fake.NewSimpleClientset(
		pdbFor("wide", 3, map[string]string{"app": "web"}),
		pdbFor("narrow", 1, map[string]string{"app": "web", "tier": "fe"}),
		pdbFor("other", 0, map[string]string{"app": "db"}),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns"}, Spec: appsv1.DeploymentSpec{Replicas: &two},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web2", Namespace: "ns"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"},
			Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 4, NumberReady: 4}},
	)
	c := NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	ctx := context.Background()

	l, err := c.LiveDisruption(ctx, "ns", Owner{Kind: "Deployment", Name: "web"}, map[string]string{"app": "web", "tier": "fe"})
	if err != nil {
		t.Fatal(err)
	}
	if !l.PDBProtected || l.PDBAllowed != 1 || l.PDBName != "narrow" {
		t.Fatalf("two matching PDBs: %+v, want the smallest budget (narrow, 1)", l)
	}
	if !l.Controller || l.Desired != 2 || l.Ready != 1 {
		t.Fatalf("deployment counts: %+v", l)
	}
	if l, _ := c.LiveDisruption(ctx, "ns", Owner{Kind: "Deployment", Name: "web2"}, map[string]string{"app": "x"}); l.PDBProtected || l.Desired != 1 {
		t.Fatalf("no PDB / nil replicas: %+v", l)
	}
	if l, _ := c.LiveDisruption(ctx, "ns", Owner{Kind: "DaemonSet", Name: "agent"}, nil); l.Desired != 4 || l.Ready != 4 || l.PDBProtected {
		t.Fatalf("daemonset: %+v", l)
	}
	if l, err := c.LiveDisruption(ctx, "ns", Owner{Kind: "ksvc", Name: "svc"}, map[string]string{"app": "web"}); err != nil || l.Controller || !l.PDBProtected {
		t.Fatalf("ksvc: %+v %v (PDB still applies, no controller count)", l, err)
	}
	if _, err := c.LiveDisruption(ctx, "ns", Owner{Kind: "StatefulSet", Name: "gone"}, nil); !errors.Is(err, ErrWorkloadGone) {
		t.Fatalf("missing workload: %v, want ErrWorkloadGone", err)
	}
}

// A Knative Service has no replica count of its own: Knative creates one
// Deployment per revision (named <revision>-deployment, labelled
// serving.knative.dev/revision and .../service) and its autoscaler writes the
// desired count into it. The ksvc's desired / ready are the sums over its
// revisions' Deployments; the other kinds without a controller (group, Pod) stay
// without a count.
func TestLiveDisruptionKsvcSumsRevisionDeployments(t *testing.T) {
	dep := func(name, svc string, replicas, ready int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{
				"serving.knative.dev/service": svc, "serving.knative.dev/revision": name[:len(name)-len("-deployment")]}},
			Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
			Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
		}
	}
	cs := fake.NewSimpleClientset(
		dep("site-00001-deployment", "site", 0, 0), // old revision scaled to zero
		dep("site-00002-deployment", "site", 3, 2), // active revision, one replica not Ready
		dep("site-00003-deployment", "site", 1, 1), // canary
		dep("other-00001-deployment", "other", 5, 5),
	)
	c := NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	ctx := context.Background()

	l, err := c.LiveDisruption(ctx, "ns", Owner{Kind: "ksvc", Name: "site"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Controller || l.Desired != 4 || l.Ready != 3 {
		t.Fatalf("ksvc counts = %+v, want desired 4 / ready 3 summed over its revisions", l)
	}
	if l, _ := c.LiveDisruption(ctx, "ns", Owner{Kind: "ksvc", Name: "unknown"}, nil); l.Controller {
		t.Fatalf("a ksvc without revision Deployments must stay without a count: %+v", l)
	}
	for _, o := range []Owner{{Kind: "group", Name: "site"}, {Kind: "Pod", Name: "site"}} {
		if l, err := c.LiveDisruption(ctx, "ns", o, nil); err != nil || l.Controller {
			t.Fatalf("%s: %+v %v, want no controller count", o.Kind, l, err)
		}
	}
}
