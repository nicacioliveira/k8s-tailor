package webhook

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

var restartAlways = corev1.ContainerRestartPolicyAlways

func seedMany(f *fixture, es map[string]*recommender.Entry) {
	m := map[string]recommender.Entry{}
	var list []recommender.Recommendation
	for c, e := range es {
		e.Rec.Container = c
		m[recommender.Key("apps", "Deployment", "web", c)] = *e
		list = append(list, e.Rec)
	}
	f.eng.Seed(list, m)
}

// meshPod is the pod at admission: app, optionally an injected istio-proxy
// (classic or native) and the istio-init init container.
func meshPod(proxy string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc-x", Namespace: "apps", OwnerReferences: rsOwner},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app",
			Resources: corev1.ResourceRequirements{Requests: rl("cpu", "1", "memory", "512Mi")}}}},
	}
	px := corev1.Container{Name: "istio-proxy", Resources: corev1.ResourceRequirements{
		Requests: rl("cpu", "100m", "memory", "128Mi"), Limits: rl("cpu", "2", "memory", "1Gi")}}
	switch proxy {
	case "classic":
		p.Annotations = map[string]string{k8s.AnnIstioStatus: "{}"}
		p.Spec.InitContainers = []corev1.Container{{Name: "istio-init", Resources: corev1.ResourceRequirements{Requests: rl("cpu", "10m")}}}
		p.Spec.Containers = append(p.Spec.Containers, px)
	case "native":
		p.Annotations = map[string]string{k8s.AnnIstioStatus: "{}"}
		px.RestartPolicy = &restartAlways
		p.Spec.InitContainers = []corev1.Container{{Name: "istio-init", Resources: corev1.ResourceRequirements{Requests: rl("cpu", "10m")}}, px}
	}
	return p
}

func podReview(t *testing.T, p *corev1.Pod) []byte {
	t.Helper()
	raw, _ := json.Marshal(p)
	return review(t, podSpec{rawJSON: raw})
}

// applyOps applies our JSON-patch ops (add .../<list>/<i>/resources) to the pod,
// as the API server does between webhook invocations.
func applyOps(t *testing.T, p *corev1.Pod, ops []map[string]any) {
	t.Helper()
	for _, op := range ops {
		parts := strings.Split(strings.TrimPrefix(op["path"].(string), "/"), "/")
		if len(parts) != 4 || parts[0] != "spec" || parts[3] != "resources" {
			t.Fatalf("unexpected path %v", op["path"])
		}
		i, _ := strconv.Atoi(parts[2])
		var r corev1.ResourceRequirements
		b, _ := json.Marshal(op["value"])
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		switch parts[1] {
		case "containers":
			p.Spec.Containers[i].Resources = r
		case "initContainers":
			p.Spec.InitContainers[i].Resources = r
		default:
			t.Fatalf("unexpected list %q", parts[1])
		}
	}
}

func paths(ops []map[string]any) []string {
	var out []string
	for _, op := range ops {
		out = append(out, fmt.Sprint(op["path"]))
	}
	return out
}

func proxyWebEntry(cpu, mem float64, mode policy.Mode) *recommender.Entry {
	e := webEntry(cpu, mem)
	e.Mode = mode
	e.Class, e.NotInTemplate, e.Mesh = k8s.ClassInjectedSidecar, true, k8s.MeshIstio
	return e
}

func TestWebhookMultiContainer(t *testing.T) {
	cases := []struct {
		name  string
		proxy string
		px    *recommender.Entry
		init  bool // seed an entry for istio-init too
		want  []string
	}{
		{"app only, proxy in Recommend", "classic", proxyWebEntry(0.02, 64*mib, policy.ModeRecommend), false,
			[]string{"/spec/containers/0/resources"}},
		{"proxy opted in (classic)", "classic", proxyWebEntry(0.02, 64*mib, policy.ModeWebhook), false,
			[]string{"/spec/containers/0/resources", "/spec/containers/1/resources"}},
		{"proxy opted in (native sidecar)", "native", proxyWebEntry(0.02, 64*mib, policy.ModeWebhook), false,
			[]string{"/spec/containers/0/resources", "/spec/initContainers/1/resources"}},
		{"plain init container never sized", "classic", nil, true,
			[]string{"/spec/containers/0/resources"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil)
			es := map[string]*recommender.Entry{"app": webEntry(0.2, 128*mib)}
			if c.px != nil {
				es["istio-proxy"] = c.px
			}
			if c.init {
				es["istio-init"] = webEntry(0.5, 256*mib)
			}
			seedMany(f, es)
			res := call(t, f.wh, podReview(t, meshPod(c.proxy)))
			got := paths(res.ops)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("patched %v, want %v", got, c.want)
			}
		})
	}
}

// reinvocationPolicy: IfNeeded — the Tailor webhook may run BEFORE the Istio
// injector (proxy absent), be reinvoked after it (proxy present), and again.
// Each invocation must patch only what still needs it (idempotent), with paths
// computed on the object as it is at that invocation, and never break when the
// proxy appears or disappears between invocations.
func TestWebhookReinvocation(t *testing.T) {
	f := newFixture(t, nil)
	seedMany(f, map[string]*recommender.Entry{
		"app":         webEntry(0.2, 128*mib),
		"istio-proxy": proxyWebEntry(0.02, 64*mib, policy.ModeWebhook),
	})
	// 1st invocation: before the injector — only the app exists.
	pod := meshPod("")
	r1 := call(t, f.wh, podReview(t, pod))
	if fmt.Sprint(paths(r1.ops)) != "[/spec/containers/0/resources]" {
		t.Fatalf("1st: %v", paths(r1.ops))
	}
	applyOps(t, pod, r1.ops)

	// The injector adds istio-proxy as a native sidecar (and istio-init).
	injected := meshPod("native")
	injected.Spec.Containers = pod.Spec.Containers // keep our app mutation
	r2 := call(t, f.wh, podReview(t, injected))
	if fmt.Sprint(paths(r2.ops)) != "[/spec/initContainers/1/resources]" {
		t.Fatalf("2nd (proxy appeared): %v — app must not be re-patched", paths(r2.ops))
	}
	applyOps(t, injected, r2.ops)
	if got := injected.Spec.InitContainers[1].Resources.Requests[corev1.ResourceCPU]; got.String() != "20m" {
		t.Fatalf("proxy cpu = %s", got.String())
	}
	if got := injected.Spec.InitContainers[1].Resources.Limits[corev1.ResourceMemory]; got.String() != "1Gi" {
		t.Fatalf("proxy mem limit = %s (must be preserved)", got.String())
	}

	// 3rd invocation on our own output: nothing left to do.
	if r3 := call(t, f.wh, podReview(t, injected)); len(r3.ops) != 0 {
		t.Fatalf("3rd (idempotence): %v", paths(r3.ops))
	}

	// The proxy disappears (another plugin removed it): no stale index, no op.
	gone := meshPod("")
	gone.Spec.Containers = injected.Spec.Containers
	if r4 := call(t, f.wh, podReview(t, gone)); len(r4.ops) != 0 {
		t.Fatalf("4th (proxy gone): %v", paths(r4.ops))
	}
}

// A request raised above an existing limit (recommendation without a limit)
// must lift that limit — an admission patch with request > limit makes the pod
// itself invalid and the CREATE fails.
func TestWebhookLiftsPartialLimit(t *testing.T) {
	f := newFixture(t, webEntry(2, 512*mib))
	res := call(t, f.wh, review(t, podSpec{owners: rsOwner, req: rl("cpu", "1", "memory", "512Mi"), lim: rl("cpu", "1500m")}))
	req, lim := resourcesOf(t, res.ops)
	if req["cpu"] != "2000m" || lim["cpu"] != "2000m" {
		t.Fatalf("req=%v lim=%v, want cpu limit lifted to the request", req, lim)
	}
}

// Pod-level resources: a patch that would push the sum of container requests
// above spec.resources.requests makes the pod invalid — admit it unmodified.
func TestWebhookPodLevelResources(t *testing.T) {
	cases := []struct {
		name   string
		podCPU string
		rec    float64
		want   int
	}{
		{"growth beyond pod-level request: no patch", "1200m", 1.5, 0},
		{"shrink within pod-level request: patched", "1200m", 0.5, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, webEntry(c.rec, 512*mib))
			pod := meshPod("")
			pod.Spec.Resources = &corev1.ResourceRequirements{Requests: rl("cpu", c.podCPU)}
			res := call(t, f.wh, podReview(t, pod))
			if len(res.ops) != c.want {
				t.Fatalf("ops = %v, want %d", paths(res.ops), c.want)
			}
			if len(res.annOps) != c.want { // decision marker only with a real patch
				t.Fatalf("annotation ops = %v, want %d", res.annOps, c.want)
			}
		})
	}
}
