package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Pods that no longer exist keep their usage in the metrics backend; OwnerOf
// attributes them by generated name only when the name is unambiguous.
func TestSnapshotOwnerOfGonePods(t *testing.T) {
	oldRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-old", Namespace: "apps",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
	cs := fake.NewSimpleClientset(webRS, oldRS,
		snapPod("web-abc-x7k2m", nil),
		snapPod("agent-q9z4b", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}}
		}),
		snapPod("db-0", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db"}}
		}),
	)
	snap, err := NewFromClients(cs, emptyDyn(), nil).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		pod  string
		want Owner
		ok   bool
	}{
		{"web-abc-x7k2m", Owner{"Deployment", "web"}, true}, // live
		{"web-abc-b7k2m", Owner{"Deployment", "web"}, true}, // gone, current ReplicaSet
		{"web-old-zz9k2", Owner{"Deployment", "web"}, true}, // gone, previous ReplicaSet (no pods left)
		{"agent-h5t2w", Owner{"DaemonSet", "agent"}, true},  // gone DaemonSet pod
		{"db-0", Owner{"StatefulSet", "db"}, true},          // live
		{"db-1", Owner{}, false},                            // StatefulSet names are never guessed
		{"web-abc-gone1", Owner{}, false},                   // not a generated suffix (vowels, '1')
		{"web-abc-x7k2", Owner{}, false},                    // wrong suffix length
		{"web-b7k2m", Owner{}, false},                       // "web" is a Deployment, not a ReplicaSet
		{"other-xyz-b7k2m", Owner{}, false},                 // unknown ReplicaSet
		{"bare", Owner{}, false},                            // no suffix at all
	}
	for _, tc := range cases {
		got, ok := snap.OwnerOf("apps", tc.pod)
		if ok != tc.ok || got != tc.want {
			t.Errorf("OwnerOf(%q) = %+v, %v; want %+v, %v", tc.pod, got, ok, tc.want, tc.ok)
		}
	}
	// Another namespace never borrows this namespace's ReplicaSets.
	if _, ok := snap.OwnerOf("other", "web-abc-b7k2m"); ok {
		t.Error("gone pod attributed across namespaces")
	}
}
