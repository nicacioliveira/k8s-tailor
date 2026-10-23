package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func req(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// CSRF: with auth off (or a stolen-session-free browser), a cross-site
// <form enctype="text/plain"> POST is a "simple request" — no preflight. Writes
// must demand application/json, and reject cross-site Origins outright.
func TestWriteRequiresJSONAndSameOrigin(t *testing.T) {
	s, _ := newTestServer(t, config.Config{})
	s.SetAllowedHosts([]string{"example.com"}) // httptest's default Host
	h := s.Handler()
	body := `{"paused":true}`
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		hdr := map[string]string{}
		if ct != "" {
			hdr["Content-Type"] = ct
		}
		if rr := req(h, "POST", "/api/pause", body, hdr); rr.Code != http.StatusUnsupportedMediaType {
			t.Errorf("POST Content-Type %q = %d, want 415", ct, rr.Code)
		}
	}
	if rr := req(h, "POST", "/api/pause", body, map[string]string{"Content-Type": "application/json; charset=utf-8"}); rr.Code != 200 {
		t.Errorf("json with charset = %d", rr.Code)
	}
	// Cross-site browser requests are refused even with the right content type.
	for _, hdr := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Origin": "null"},
		{"Sec-Fetch-Site": "cross-site"},
	} {
		hdr["Content-Type"] = "application/json"
		if rr := req(h, "POST", "/api/pause", body, hdr); rr.Code != http.StatusForbidden {
			t.Errorf("POST %v = %d, want 403", hdr, rr.Code)
		}
	}
	// Same-origin UI (served by this engine at example.com) passes.
	if rr := req(h, "POST", "/api/pause", body, map[string]string{"Content-Type": "application/json", "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}); rr.Code != 200 {
		t.Errorf("same-origin = %d %s", rr.Code, rr.Body)
	}
	// DELETE without a body (the MCP client sends no Content-Type) is allowed —
	// browsers can't send a cross-site DELETE without a CORS preflight.
	if rr := req(h, "DELETE", "/api/groups/nope", "", nil); rr.Code == http.StatusUnsupportedMediaType {
		t.Errorf("bodyless DELETE rejected: %d", rr.Code)
	}
	if rr := req(h, "DELETE", "/api/groups/nope", "x=1", map[string]string{"Content-Type": "text/plain"}); rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("DELETE with text body = %d, want 415", rr.Code)
	}
	// Reads are untouched.
	if rr := req(h, "GET", "/api/policies", "", map[string]string{"Origin": "https://evil.example"}); rr.Code != 200 {
		t.Errorf("GET = %d", rr.Code)
	}
}

func TestWriteBodyIsCapped(t *testing.T) {
	s, _ := newTestServer(t, config.Config{})
	h := s.Handler()
	big := `{"name":"p","namespaces":["` + strings.Repeat("a", 2<<20) + `"]}`
	for _, p := range []string{"/api/policies", "/api/groups", "/api/workload", "/api/pause"} {
		if rr := do(h, "POST", p, big, ""); rr.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("POST %s 2MiB = %d, want 413", p, rr.Code)
		}
	}
}

func TestPolicyValidationIs400(t *testing.T) {
	s, _ := newTestServer(t, config.Config{})
	h := s.Handler()
	for _, b := range []string{
		`{"name":"p","cpuLimitFactor":0.5}`,
		`{"name":"p","mode":"Turbo"}`,
		`{"name":"p","memMax":"lots"}`,
		`{"name":"Not A Name"}`,
	} {
		if rr := do(h, "POST", "/api/policies", b, ""); rr.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", b, rr.Code, rr.Body)
		}
	}
	// Error bodies are valid JSON even when the message has quotes.
	rr := do(h, "POST", "/api/policies", `{"name":"p","mode":"Tu\"rbo"}`, "")
	var e map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil || e["error"] == "" {
		t.Errorf("error body not JSON: %q", rr.Body)
	}
}

var tpGVR = schema.GroupVersionResource{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}

// Round-trip through the HTTP API: GET a GitOps-authored policy, POST it back
// (what the UI does on save), and a partial MCP-style POST — neither may drop
// headroomGuard / quantiles / cooldowns that the view does not carry.
func TestPolicyUpsertRoundTripHTTP(t *testing.T) {
	spec := map[string]any{
		"priority": int64(3), "mode": "Recommend",
		"headroomGuard": true, "headroomThreshold": 0.05,
		"cpu":       map[string]any{"quantile": 0.8, "margin": 0.2},
		"scaleDown": map[string]any{"cooldown": "6h"},
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tailor.k8s-tailor.dev/v1alpha1", "kind": "TailorPolicy",
		"metadata": map[string]any{"name": "p"}, "spec": spec,
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs, obj)
	cfg := config.Config{}
	vmc := vm.New(fakeVM(t).URL)
	kc := k8s.NewFromClients(fake.NewSimpleClientset(), dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	pol := policy.NewStoreWithClient(dyn, cfg)
	pol.Refresh(context.Background())
	eng := recommender.NewEngine(cfg, vmc, kc, pol, pricing.New(cfg, vmc))
	h := New(cfg, eng, vmc, actuation.NewStore(10), nil).Handler()

	live := func() map[string]any {
		o, err := dyn.Resource(tpGVR).Get(context.Background(), "p", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		m, _, _ := unstructured.NestedMap(o.Object, "spec")
		return m
	}
	before := live()

	var list struct {
		Policies []json.RawMessage `json:"policies"`
	}
	_ = json.Unmarshal(do(h, "GET", "/api/policies", "", "").Body.Bytes(), &list)
	if len(list.Policies) != 1 {
		t.Fatalf("policies = %d", len(list.Policies))
	}
	if rr := do(h, "POST", "/api/policies", string(list.Policies[0]), ""); rr.Code != 200 {
		t.Fatalf("round-trip POST = %d %s", rr.Code, rr.Body)
	}
	if after := live(); !reflect.DeepEqual(before, after) {
		t.Fatalf("UI round-trip changed the spec:\nbefore=%v\nafter =%v", before, after)
	}

	if rr := do(h, "POST", "/api/policies", `{"name":"p","mode":"InPlace"}`, ""); rr.Code != 200 {
		t.Fatalf("partial POST = %d %s", rr.Code, rr.Body)
	}
	after := live()
	if after["mode"] != "InPlace" || after["headroomGuard"] != true || after["headroomThreshold"] != 0.05 ||
		after["priority"] != int64(3) || after["cpu"].(map[string]any)["quantile"] != 0.8 {
		t.Fatalf("partial POST lost fields: %v", after)
	}
}

// Slowloris: a client that never finishes its headers must be cut off by
// ReadHeaderTimeout instead of pinning a goroutine/conn forever.
func TestHTTPServerTimeouts(t *testing.T) {
	srv := NewHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Fatalf("timeouts unset: %+v", srv)
	}
	if srv.MaxHeaderBytes == 0 || srv.MaxHeaderBytes > 1<<20 {
		t.Fatalf("MaxHeaderBytes = %d", srv.MaxHeaderBytes)
	}

	old := Timeouts
	Timeouts.ReadHeader = 200 * time.Millisecond
	t.Cleanup(func() { Timeouts = old })
	srv = NewHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nX-Slow: "))
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = io.ReadAll(bufio.NewReader(c))
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("server kept a half-sent request open for >3s (slowloris)")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("closed after %v, want ~ReadHeaderTimeout", d)
	}
}

func TestLivez(t *testing.T) {
	s, _ := newTestServer(t, config.Config{})
	if rr := do(s.Handler(), "GET", "/livez", "", ""); rr.Code != 200 {
		t.Fatalf("livez = %d", rr.Code)
	}
}

// DNS rebinding: evil.example resolves to the engine, so the attacker page is
// "same origin" (Origin host == Host). The Host must also be an expected name:
// TAILOR_ALLOWED_HOSTS, or — with an OPEN write API (no token) — loopback only.
func TestWriteHostAllowlist(t *testing.T) {
	body := `{"paused":true}`
	post := func(h http.Handler, host, origin string) int {
		r := httptest.NewRequest("POST", "/api/pause", strings.NewReader(body))
		r.Host = host
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Code
	}

	// Open write API (no token): only loopback Host names for browser writes.
	s, _ := newTestServer(t, config.Config{})
	h := s.Handler()
	if c := post(h, "evil.example:8080", "http://evil.example:8080"); c != http.StatusForbidden {
		t.Errorf("rebound same-origin write on open API = %d, want 403", c)
	}
	if c := post(h, "localhost:8080", "http://localhost:8080"); c != 200 {
		t.Errorf("port-forwarded UI write = %d, want 200", c)
	}
	if c := post(h, "[::1]:8080", "http://[::1]:8080"); c != 200 {
		t.Errorf("ipv6 loopback write = %d, want 200", c)
	}
	// Non-browser clients (MCP server: in-cluster Host, no Origin) are unaffected.
	if c := post(h, "k8s-tailor.k8s-tailor.svc:8080", ""); c != 200 {
		t.Errorf("non-browser write = %d, want 200", c)
	}

	// Explicit allowlist wins (and applies with or without a token).
	s.SetAllowedHosts([]string{"tailor.example.com"})
	if c := post(h, "tailor.example.com", "https://tailor.example.com"); c != 200 {
		t.Errorf("allowlisted host = %d, want 200", c)
	}
	if c := post(h, "localhost:8080", "http://localhost:8080"); c != http.StatusForbidden {
		t.Errorf("host outside the allowlist = %d, want 403", c)
	}
}
