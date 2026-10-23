package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

var tailorGVRs = map[schema.GroupVersionResource]string{
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}: "TailorPolicyList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}:  "TailorConfigList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}: "TailorProfileList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}:   "TailorGroupList",
}

// fakeVM answers every query with an empty success (and /health OK).
func fakeVM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestServer(t *testing.T, cfg config.Config) (*Server, *recommender.Engine) {
	t.Helper()
	vmSrv := fakeVM(t)
	vmc := vm.New(vmSrv.URL)
	kc := k8s.NewFromClients(fake.NewSimpleClientset(), dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	pol := policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs), cfg)
	eng := recommender.NewEngine(cfg, vmc, kc, pol, pricing.New(cfg, vmc))
	return New(cfg, eng, vmc, actuation.NewStore(10), nil), eng
}

func do(h http.Handler, method, path, body, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// Bug 6: the write API (/api/pause, /api/policies, /api/groups, /api/workload)
// was reachable by anyone who could reach :8080. With TAILOR_API_TOKEN set, every
// mutating /api call must carry `Authorization: Bearer <token>`; reads stay open.
// TAILOR_API_AUTH_REQUIRED=true with no token configured fails CLOSED.
func TestAPIWriteAuth(t *testing.T) {
	writes := []struct{ method, path, body string }{
		{"POST", "/api/pause", `{"paused":true}`},
		{"POST", "/api/policies", `{"name":"p1"}`},
		{"DELETE", "/api/policies/p1", ""},
		{"POST", "/api/groups", `{"name":"g1","groupBy":"app"}`},
		{"DELETE", "/api/groups/g1", ""},
		{"POST", "/api/workload", `{"namespace":"a","kind":"Pod","workload":"x"}`},
	}
	cases := []struct {
		name     string
		token    string
		required bool
		auth     string
		allowed  bool
		status   int // expected status when NOT allowed
	}{
		{"no token configured: open (back-compat)", "", false, "", true, 0},
		{"token set, no header", "s3cret", false, "", false, http.StatusUnauthorized},
		{"token set, wrong token", "s3cret", false, "Bearer nope", false, http.StatusUnauthorized},
		{"token set, wrong scheme", "s3cret", false, "Basic s3cret", false, http.StatusUnauthorized},
		{"token set, correct", "s3cret", false, "Bearer s3cret", true, 0},
		{"required but no token configured: fail closed", "", true, "Bearer anything", false, http.StatusServiceUnavailable},
		{"required and correct", "s3cret", true, "Bearer s3cret", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newTestServer(t, config.Config{APIToken: c.token, APIAuthRequired: c.required})
			h := s.Handler()
			for _, w := range writes {
				rr := do(h, w.method, w.path, w.body, c.auth)
				denied := rr.Code == http.StatusUnauthorized || rr.Code == http.StatusServiceUnavailable
				if c.allowed && denied {
					t.Errorf("%s %s: denied (%d), want allowed", w.method, w.path, rr.Code)
				}
				if !c.allowed && rr.Code != c.status {
					t.Errorf("%s %s: status %d, want %d", w.method, w.path, rr.Code, c.status)
				}
			}
			// Reads are never gated (UI + scrapes).
			for _, p := range []string{"/healthz", "/api/recommendations", "/api/policies", "/metrics"} {
				if rr := do(h, "GET", p, "", ""); rr.Code != http.StatusOK {
					t.Errorf("GET %s: %d, want 200", p, rr.Code)
				}
			}
		})
	}
}

func TestReadEndpoints(t *testing.T) {
	s, eng := newTestServer(t, config.Config{})
	rec := recommender.Recommendation{
		Namespace: "apps", Kind: "Deployment", Workload: "web", Container: "app", Automated: true,
		Recommended:  recommender.Resources{CPURequest: 0.25, MemRequest: 256 * 1024 * 1024, MemLimit: 512 * 1024 * 1024},
		WasteUSDWeek: 1.5,
	}
	eng.Seed([]recommender.Recommendation{rec}, map[string]recommender.Entry{
		recommender.Key("apps", "Deployment", "web", "app"): {Rec: rec, Apply: policy.ApplyInPlace},
	})
	h := s.Handler()

	rr := do(h, "GET", "/api/recommendations", "", "")
	var recs struct {
		Actuation string  `json:"actuation"`
		Total     float64 `json:"totalSavingsUsdWeek"`
		Count     int     `json:"count"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &recs)
	if recs.Count != 1 || recs.Total != 1.5 || recs.Actuation != "active" {
		t.Fatalf("recommendations = %+v", recs)
	}

	rr = do(h, "GET", "/api/patch/apps/Deployment/web/app", "", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"cpu":"250m"`) || !strings.Contains(rr.Body.String(), `"memory":"512Mi"`) {
		t.Fatalf("patch preview = %d %s", rr.Code, rr.Body.String())
	}
	for _, p := range []string{"/api/patch/apps/Deployment/nope/app", "/api/detail/apps/Deployment/nope/app"} {
		if rr := do(h, "GET", p, "", ""); rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", p, rr.Code)
		}
	}

	rr = do(h, "GET", "/metrics", "", "")
	if !strings.Contains(rr.Body.String(), `tailor_recommended_cpu_cores{namespace="apps",workload="web",container="app"} 0.25`) {
		t.Fatalf("metrics missing gauge:\n%s", rr.Body.String())
	}

	rr = do(h, "GET", "/healthz", "", "")
	if !strings.Contains(rr.Body.String(), `"vmHealthy":true`) {
		t.Fatalf("healthz = %s", rr.Body.String())
	}
	if rr := do(h, "GET", "/api/actuations?limit=5", "", ""); rr.Code != 200 {
		t.Fatalf("actuations = %d", rr.Code)
	}
}

func TestWriteEndpointsValidate(t *testing.T) {
	s, _ := newTestServer(t, config.Config{})
	h := s.Handler()
	cases := []struct{ method, path, body string }{
		{"POST", "/api/pause", "{"},
		{"POST", "/api/policies", `{}`},
		{"POST", "/api/groups", `{"name":"g"}`},
		{"POST", "/api/workload", `{"namespace":"a"}`},
	}
	for _, c := range cases {
		if rr := do(h, c.method, c.path, c.body, ""); rr.Code != http.StatusBadRequest {
			t.Errorf("%s %s %s = %d, want 400", c.method, c.path, c.body, rr.Code)
		}
	}
	// A valid pause round-trips through the TailorConfig CR.
	if rr := do(h, "POST", "/api/pause", `{"paused":true}`, ""); rr.Code != 200 {
		t.Fatalf("pause = %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(h, "GET", "/healthz", "", ""); !strings.Contains(rr.Body.String(), `"actuation":"paused"`) {
		t.Fatalf("after pause healthz = %s", rr.Body.String())
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		name     string
		q        string
		wantZero bool
		span     time.Duration
		step     int
	}{
		{"empty", "", true, 0, 0},
		{"unix from/to", "from=1000&to=4600&step=30", false, time.Hour, 30},
		{"rfc3339", "from=2026-01-01T00:00:00Z&to=2026-01-01T02:00:00Z", false, 2 * time.Hour, 0},
		{"window days", "window=1d", false, 24 * time.Hour, 0},
		{"bad window", "window=zz", true, 0, 0},
		{"only from", "from=1000", true, 0, 0},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/x?"+c.q, nil)
		start, end, step := parseRange(r)
		if c.wantZero != start.IsZero() || step != c.step {
			t.Errorf("%s: start=%v step=%d", c.name, start, step)
			continue
		}
		if !c.wantZero && end.Sub(start) != c.span {
			t.Errorf("%s: span=%v want %v", c.name, end.Sub(start), c.span)
		}
	}
}

func TestEventsFromRows(t *testing.T) {
	ev := eventsFromRows([]map[string]any{{
		"time": "1700000000", "mode": "inplace", "namespace": "a", "kind": "Deployment",
		"workload": "w", "container": "c", "fromCpu": 1.0, "toCpu": "0.5", "fromMem": nil, "toMem": 3.0,
	}})
	if len(ev) != 1 || ev[0].Time != 1700000000 || ev[0].ToCPU != 0.5 || ev[0].FromMem != 0 || ev[0].ToMem != 3 || ev[0].Mode != "inplace" {
		t.Fatalf("eventsFromRows = %+v", ev)
	}
}
