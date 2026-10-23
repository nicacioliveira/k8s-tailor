package reconciler

import (
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// Webhook decisions are recorded when the reconciler OBSERVES the sized pod in
// the API server — once per pod, only when the marker matches what the pod
// really runs with, and not for pods admitted long before this leader started
// (a previous leader recorded those).
func TestRecordAdmissions(t *testing.T) {
	store := actuation.NewStore(50)
	r := New(config.Config{}, nil, nil, store)
	now := time.Now()
	r.now = func() time.Time { return now }
	r.started = now
	const mi = 1024 * 1024
	owner := k8s.Owner{Kind: "Deployment", Name: "web"}
	ap := func(uid string, created time.Time, d map[string]k8s.AdmissionDecision, cs ...k8s.Container) k8s.AdmittedPod {
		return k8s.AdmittedPod{Namespace: "apps", Name: "web-" + uid, UID: uid, Owner: owner, Created: created, Decisions: d, Containers: cs}
	}
	dec := map[string]k8s.AdmissionDecision{"app": {FromCPU: 1, ToCPU: 0.2, FromMem: 512 * mi, ToMem: 128 * mi}}
	snap := &k8s.ClusterSnapshot{Admitted: []k8s.AdmittedPod{
		// genuine: spec matches the marker (rounded like the patch: 200m / 128Mi)
		ap("ok", now.Add(-10*time.Second), dec, k8s.Container{Name: "app", CPURequest: 0.2, MemRequest: 128 * mi}),
		// forged marker: the pod does not run with the claimed values
		ap("forged", now.Add(-10*time.Second), dec, k8s.Container{Name: "app", CPURequest: 1, MemRequest: 512 * mi}),
		// marker for a container the pod doesn't have
		ap("ghost", now.Add(-10*time.Second), map[string]k8s.AdmissionDecision{"nope": dec["app"]}, k8s.Container{Name: "app", CPURequest: 0.2, MemRequest: 128 * mi}),
		// admitted long before this leader: a previous leader recorded it
		ap("old", now.Add(-time.Hour), dec, k8s.Container{Name: "app", CPURequest: 0.2, MemRequest: 128 * mi}),
	}}
	r.recordAdmissions(snap)
	r.recordAdmissions(snap) // observed again next cycle: no duplicate
	ev := store.Recent(10)
	if len(ev) != 1 {
		t.Fatalf("recorded %d events, want 1: %+v", len(ev), ev)
	}
	e := ev[0]
	if e.Mode != "webhook" || e.Namespace != "apps" || e.Kind != "Deployment" || e.Workload != "web" || e.Container != "app" ||
		e.FromCPU != 1 || e.ToCPU != 0.2 || e.ToMem != 128*mi || e.Time != now.Add(-10*time.Second).Unix() {
		t.Fatalf("event = %+v", e)
	}
	if _, perKey := store.Snapshot(); perKey["apps/Deployment/web/app"] != 1 {
		t.Fatalf("tailor_mutations_total source = %v, want exactly 1", perKey)
	}
}
