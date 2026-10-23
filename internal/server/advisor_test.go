package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/advisor"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

const fakeReview = `{"verdict":"ok","confidence":"high","rationale":"dimensionamento coerente","risks":[],"suggested_action":"manter","cited_figures":[]}`
const fakeSummary = `{"overview":"tudo certo","verdict":"ok","confidence":"medium","highlights":["a"],"attention":[],"cited_figures":[]}`

// fakeLLM is an OpenAI-compatible stub; content is the assistant message.
func fakeLLM(t *testing.T, calls *atomic.Int32, content func(r *http.Request) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c := fakeReview
		if content != nil {
			c = content(r)
		}
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": c}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func advServer(t *testing.T, cfg config.Config, ac advisor.Config) (*Server, *recommender.Engine) {
	t.Helper()
	s, eng := newTestServer(t, cfg)
	a, err := advisor.New(ac)
	if err != nil {
		t.Fatal(err)
	}
	s.SetAdvisor(a, ac.Model)
	mib := 1024.0 * 1024
	recs := []recommender.Recommendation{
		{Namespace: "a", Kind: "Deployment", Workload: "w1", Container: "app", Replicas: 2, Mode: "InPlace", Confident: true, WasteUSDWeek: 4.2,
			Applied: recommender.Resources{CPURequest: 1, MemRequest: 512 * mib}, Current: recommender.Resources{CPURequest: 1, MemRequest: 512 * mib},
			Recommended: recommender.Resources{CPURequest: 0.2, MemRequest: 128 * mib}},
		{Namespace: "a", Kind: "Deployment", Workload: "w2", Container: "app", Replicas: 1, Mode: "Recommend", Confident: true, WasteUSDWeek: 1,
			Applied: recommender.Resources{CPURequest: 0.5, MemRequest: 256 * mib}, Recommended: recommender.Resources{CPURequest: 0.1, MemRequest: 128 * mib}},
		{Namespace: "b", Kind: "Deployment", Workload: "multi", Container: "c1", Mode: "Recommend", Confident: true},
		{Namespace: "b", Kind: "Deployment", Workload: "multi", Container: "c2", Mode: "Recommend", Confident: true},
	}
	entries := map[string]recommender.Entry{}
	for _, r := range recs {
		entries[recommender.Key(r.Namespace, r.Kind, r.Workload, r.Container)] = recommender.Entry{Rec: r}
	}
	eng.Seed(recs, entries)
	return s, eng
}

func advCfg(url string) advisor.Config {
	c := advisor.DefaultConfig()
	c.Enabled = true
	c.BaseURL = url
	c.Model = "test-model"
	c.APIKey = "sk-secret-do-not-leak"
	c.MaxRetries = 0
	return c
}

const reviewBody = `{"namespace":"a","kind":"Deployment","workload":"w1","container":"app"}`

func TestAdvisorReviewOK(t *testing.T) {
	var calls atomic.Int32
	llm := fakeLLM(t, &calls, nil)
	s, _ := advServer(t, config.Config{}, advCfg(llm.URL))
	rr := do(s.Handler(), "POST", "/api/advisor/review", reviewBody, "")
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var got struct {
		Workload string         `json:"workload"`
		Review   advisor.Review `json:"review"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Workload != "w1" || got.Review.Verdict != advisor.Verdict("ok") || got.Review.Model != "test-model" {
		t.Errorf("unexpected response: %s", rr.Body)
	}
	// Second identical call is served from cache.
	rr = do(s.Handler(), "POST", "/api/advisor/review", reviewBody, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"cached":true`) || calls.Load() != 1 {
		t.Errorf("want cache hit, calls=%d body=%s", calls.Load(), rr.Body)
	}
	// Omitted container resolves a single-container workload.
	rr = do(s.Handler(), "POST", "/api/advisor/review", `{"namespace":"a","kind":"Deployment","workload":"w1"}`, "")
	if rr.Code != 200 {
		t.Errorf("single-container lookup: %d %s", rr.Code, rr.Body)
	}
	// Status + metrics reflect usage and never expose the key.
	st := do(s.Handler(), "GET", "/api/advisor/status", "", "")
	if !strings.Contains(st.Body.String(), `"enabled":true`) || !strings.Contains(st.Body.String(), `"model":"test-model"`) || strings.Contains(st.Body.String(), "sk-secret") {
		t.Errorf("status: %s", st.Body)
	}
	m := do(s.Handler(), "GET", "/metrics", "", "").Body.String()
	for _, want := range []string{"tailor_advisor_enabled 1", "tailor_advisor_llm_calls_total 1", "tailor_advisor_cache_hits_total 2",
		`tailor_advisor_tokens_total{type="prompt"} 10`, `tailor_advisor_breaker_state{state="closed"} 1`, "tailor_advisor_llm_latency_seconds_count 1"} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestAdvisorReviewRejectsArbitraryNumbers(t *testing.T) {
	var calls atomic.Int32
	llm := fakeLLM(t, &calls, nil)
	s, _ := advServer(t, config.Config{}, advCfg(llm.URL))
	rr := do(s.Handler(), "POST", "/api/advisor/review", `{"namespace":"a","kind":"Deployment","workload":"w1","container":"app","recommended":{"cpuRequest":99}}`, "")
	if rr.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Errorf("want 400 and no LLM call, got %d calls=%d", rr.Code, calls.Load())
	}
}

func TestAdvisorDisabled(t *testing.T) {
	s, _ := newTestServer(t, config.Config{})
	s.SetAdvisor(mustAdvisor(t, advisor.DefaultConfig()), "")
	for _, p := range []string{"/api/advisor/review", "/api/advisor/summary"} {
		rr := do(s.Handler(), "POST", p, reviewBody, "")
		if rr.Code != 404 || !strings.Contains(rr.Body.String(), `"code":"disabled"`) {
			t.Errorf("%s: %d %s", p, rr.Code, rr.Body)
		}
	}
	st := do(s.Handler(), "GET", "/api/advisor/status", "", "")
	if st.Code != 200 || !strings.Contains(st.Body.String(), `"enabled":false`) {
		t.Errorf("status: %d %s", st.Code, st.Body)
	}
	// Server without SetAdvisor at all behaves the same.
	s2, _ := newTestServer(t, config.Config{})
	if rr := do(s2.Handler(), "POST", "/api/advisor/review", reviewBody, ""); rr.Code != 404 {
		t.Errorf("nil advisor: %d", rr.Code)
	}
}

func mustAdvisor(t *testing.T, c advisor.Config) *advisor.Advisor {
	t.Helper()
	a, err := advisor.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAdvisorRateLimited(t *testing.T) {
	var calls atomic.Int32
	llm := fakeLLM(t, &calls, nil)
	ac := advCfg(llm.URL)
	ac.MaxCallsPerHour = 1
	s, _ := advServer(t, config.Config{}, ac)
	if rr := do(s.Handler(), "POST", "/api/advisor/review", reviewBody, ""); rr.Code != 200 {
		t.Fatalf("first: %d %s", rr.Code, rr.Body)
	}
	rr := do(s.Handler(), "POST", "/api/advisor/review", `{"namespace":"a","kind":"Deployment","workload":"w2","container":"app"}`, "")
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), `"code":"rate_limited"`) {
		t.Errorf("want 429, got %d %s", rr.Code, rr.Body)
	}
}

func TestAdvisorNotFoundAndAmbiguous(t *testing.T) {
	var calls atomic.Int32
	llm := fakeLLM(t, &calls, nil)
	s, _ := advServer(t, config.Config{}, advCfg(llm.URL))
	rr := do(s.Handler(), "POST", "/api/advisor/review", `{"namespace":"a","kind":"Deployment","workload":"nope","container":"app"}`, "")
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), `"code":"not_found"`) {
		t.Errorf("not found: %d %s", rr.Code, rr.Body)
	}
	rr = do(s.Handler(), "POST", "/api/advisor/review", `{"namespace":"b","kind":"Deployment","workload":"multi"}`, "")
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "container_required") {
		t.Errorf("ambiguous: %d %s", rr.Code, rr.Body)
	}
	if rr := do(s.Handler(), "POST", "/api/advisor/review", `{"namespace":"a"}`, ""); rr.Code != 400 {
		t.Errorf("missing fields: %d", rr.Code)
	}
	if calls.Load() != 0 {
		t.Errorf("LLM must not be called, got %d", calls.Load())
	}
}

func TestAdvisorBadProviderIs502(t *testing.T) {
	var calls atomic.Int32
	llm := fakeLLM(t, &calls, func(*http.Request) string { return "this is not JSON" })
	s, _ := advServer(t, config.Config{}, advCfg(llm.URL))
	rr := do(s.Handler(), "POST", "/api/advisor/review", reviewBody, "")
	if rr.Code != http.StatusBadGateway || strings.Contains(rr.Body.String(), "sk-secret") {
		t.Errorf("want 502, got %d %s", rr.Code, rr.Body)
	}
}

func TestAdvisorSummary(t *testing.T) {
	var calls atomic.Int32
	llm := fakeLLM(t, &calls, func(*http.Request) string { return fakeSummary })
	s, _ := advServer(t, config.Config{}, advCfg(llm.URL))
	rr := do(s.Handler(), "POST", "/api/advisor/summary", `{"namespace":"a","limit":5}`, "")
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	var got struct {
		Considered int `json:"considered"`
		Total      int `json:"total"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Considered != 2 || got.Total != 2 {
		t.Errorf("considered/total: %s", rr.Body)
	}
	// Empty body = all namespaces, default limit.
	if rr := do(s.Handler(), "POST", "/api/advisor/summary", "", ""); rr.Code != 200 {
		t.Errorf("empty body: %d %s", rr.Code, rr.Body)
	}
	if rr := do(s.Handler(), "POST", "/api/advisor/summary", `{"namespace":"zzz"}`, ""); rr.Code != 404 {
		t.Errorf("empty selection: %d", rr.Code)
	}
}

func TestAdvisorAuth(t *testing.T) {
	var calls atomic.Int32
	llm := fakeLLM(t, &calls, nil)
	s, _ := advServer(t, config.Config{APIToken: "s3cret"}, advCfg(llm.URL))
	h := s.Handler()
	for _, p := range []string{"/api/advisor/review", "/api/advisor/summary"} {
		if rr := do(h, "POST", p, reviewBody, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s no bearer: %d", p, rr.Code)
		}
		if rr := do(h, "POST", p, reviewBody, "Bearer wrong"); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s wrong bearer: %d", p, rr.Code)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("unauthenticated requests reached the LLM (%d)", calls.Load())
	}
	if rr := do(h, "POST", "/api/advisor/review", reviewBody, "Bearer s3cret"); rr.Code != 200 {
		t.Errorf("with bearer: %d %s", rr.Code, rr.Body)
	}
	if rr := do(h, "GET", "/api/advisor/status", "", ""); rr.Code != 200 {
		t.Errorf("status must stay open: %d", rr.Code)
	}
}
