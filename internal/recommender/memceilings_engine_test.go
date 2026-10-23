package recommender

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
)

func ceilingEngine(lookups bool, cs *fake.Clientset) *Engine {
	cfg := config.Config{MinSamples: 1, ClusterLookups: lookups, PricingMode: "static"}
	return NewEngine(cfg, nil, k8s.NewFromClients(cs, nil, nil), nil, pricing.New(cfg, nil))
}

func readyNode(mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestEngineMemCeilings(t *testing.T) {
	ctx := context.Background()
	t.Run("off by default: nothing is listed", func(t *testing.T) {
		cs := fake.NewSimpleClientset(readyNode("8Gi"))
		listed := 0
		cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) { listed++; return false, nil, nil })
		if got := ceilingEngine(false, cs).memCeilings(ctx).For("ns"); got != 0 {
			t.Fatalf("ceiling %v without the opt-in", got)
		}
		if listed != 0 {
			t.Fatalf("listed nodes %d times without the opt-in", listed)
		}
	})
	t.Run("on: read once per TTL", func(t *testing.T) {
		cs := fake.NewSimpleClientset(readyNode("8Gi"))
		listed := 0
		cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) { listed++; return false, nil, nil })
		e := ceilingEngine(true, cs)
		now := time.Unix(1_800_000_000, 0)
		e.SetClock(func() time.Time { return now })
		if got := e.memCeilings(ctx).For("ns"); got != 8*1024*1024*1024 {
			t.Fatalf("ceiling %v, want 8Gi", got)
		}
		e.memCeilings(ctx)
		if listed != 1 {
			t.Fatalf("listed %d times inside the TTL, want 1", listed)
		}
		now = now.Add(ceilingsTTL + time.Second)
		e.memCeilings(ctx)
		if listed != 2 {
			t.Fatalf("listed %d times after the TTL, want 2", listed)
		}
	})
	t.Run("forbidden: no ceiling, no panic", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("nodes is forbidden")
		})
		if got := ceilingEngine(true, cs).memCeilings(ctx).For("ns"); got != 0 {
			t.Fatalf("ceiling %v after a failed read", got)
		}
	})
}
