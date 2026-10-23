package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func node(name, mem string, ready, unschedulable bool) *corev1.Node {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
		},
	}
}

func limitRange(ns, name string, t corev1.LimitType, max string) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type: t, Max: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(max)},
		}}},
	}
}

func TestMemoryCeilings(t *testing.T) {
	cs := fake.NewSimpleClientset([]runtime.Object{
		node("small", "8Gi", true, false),
		node("big-cordoned", "64Gi", true, true),
		node("big-notready", "32Gi", false, false),
		node("mid", "16Gi", true, false),
		limitRange("a", "lr1", corev1.LimitTypeContainer, "4Gi"),
		limitRange("a", "lr2", corev1.LimitTypeContainer, "2Gi"),
		limitRange("a", "pod-level", corev1.LimitTypePod, "1Gi"), // Pod-type max is not a per-container ceiling
		limitRange("b", "lr", corev1.LimitTypeContainer, "32Gi"),
	}...)
	c := NewFromClients(cs, nil, nil)
	got, err := c.MemoryCeilings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const gi = 1024 * 1024 * 1024
	if got.NodeMax != 16*gi {
		t.Fatalf("NodeMax = %v, want 16Gi (cordoned and NotReady nodes are not eligible)", got.NodeMax)
	}
	for ns, want := range map[string]float64{"a": 2 * gi, "b": 16 * gi, "none": 16 * gi} {
		if v := got.For(ns); v != want {
			t.Errorf("For(%q) = %v, want %v", ns, v/gi, want/gi)
		}
	}
	if (MemCeilings{}).For("x") != 0 {
		t.Fatal("empty ceilings must mean no ceiling")
	}
}
