package k8s

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type errStr string

func (e errStr) Error() string { return string(e) }

// P1-7: a discovery error is "unknown" (retried after a short backoff), never a
// permanent "unsupported".
func TestInPlaceSupportRetriesAfterError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	fd := cs.Discovery().(*fakediscovery.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods"}, {Name: "pods/resize"}}}}
	failing := true
	cs.PrependReactor("get", "resource", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errStr("boom")
		}
		return false, nil, nil
	})
	c := NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	now := time.Unix(1_700_000_000, 0)
	c.clock = func() time.Time { return now }
	if ok, known := c.InPlaceSupport(context.Background()); ok || known {
		t.Fatalf("error must be unknown, got ok=%v known=%v", ok, known)
	}
	if c.InPlaceSupported(context.Background()) {
		t.Fatal("InPlaceSupported must be false while unknown")
	}
	failing = false
	if _, known := c.InPlaceSupport(context.Background()); known {
		t.Fatal("retried before the backoff elapsed")
	}
	now = now.Add(inPlaceRetry)
	if ok, known := c.InPlaceSupport(context.Background()); !ok || !known {
		t.Fatalf("after recovery: ok=%v known=%v, want supported", ok, known)
	}
}
