package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/advisor"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

const (
	// advisorTimeout bounds one advisor call end to end (retries included).
	advisorTimeout = 90 * time.Second
	// advisorMaxBody caps request bodies: the payload is only an identifier.
	advisorMaxBody      = 4 << 10
	summaryDefaultLimit = 20
	summaryMaxLimit     = 100
)

// SetAdvisor attaches the optional LLM advisor. A nil or disabled advisor makes
// the /api/advisor/review and /summary routes answer 404 (code "disabled").
// model is the configured model name, shown by /api/advisor/status (never a key
// or URL).
func (s *Server) SetAdvisor(a *advisor.Advisor, model string) {
	s.adv = a
	s.advModel = model
}

// writeErr emits the API's standard error body ({"error": msg}) plus a stable
// machine-readable "code".
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

// advisorError maps typed advisor errors to HTTP. Provider details (bodies,
// URLs) never reach the client: only a generic message for 502.
func advisorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, advisor.ErrDisabled):
		writeErr(w, http.StatusNotFound, "disabled", "advisor is disabled")
	case errors.Is(err, advisor.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "advisor hourly call limit reached")
	case errors.Is(err, advisor.ErrBudgetExceeded):
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "budget_exceeded", "advisor hourly token budget exhausted")
	case errors.Is(err, advisor.ErrCircuitOpen):
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusServiceUnavailable, "circuit_open", "advisor provider temporarily unavailable (circuit breaker open)")
	case errors.Is(err, advisor.ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, advisor.ErrInvalidResponse):
		writeErr(w, http.StatusBadGateway, "invalid_response", "advisor model returned an invalid response")
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusBadGateway, "timeout", "advisor provider timed out")
	default:
		writeErr(w, http.StatusBadGateway, "upstream_error", "advisor provider error")
	}
}

// recToDecision converts through JSON (the advisor's documented integration
// point), so the advisor package never depends on the recommender.
func recToDecision(rec recommender.Recommendation) (advisor.DecisionInput, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return advisor.DecisionInput{}, fmt.Errorf("%w: %v", advisor.ErrInvalidInput, err)
	}
	return advisor.DecisionFromJSON(raw)
}

func (s *Server) registerAdvisor(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/advisor/status", func(w http.ResponseWriter, _ *http.Request) {
		st := s.adv.Stats()
		model := ""
		if s.adv.Enabled() {
			model = s.advModel
		}
		writeJSON(w, map[string]any{
			"enabled":       s.adv.Enabled(),
			"model":         model,
			"breaker_state": st.BreakerState,
			"stats": map[string]any{
				"requests":             st.Requests,
				"cache_hits":           st.CacheHits,
				"cache_misses":         st.CacheMisses,
				"cache_entries":        st.CacheEntries,
				"llm_calls":            st.LLMCalls,
				"failures":             st.Failures,
				"invalid_responses":    st.InvalidResponses,
				"hallucinated_figures": st.HallucinatedFigs,
				"rate_limited":         st.RateLimited,
				"budget_rejected":      st.BudgetRejected,
				"circuit_rejected":     st.CircuitRejected,
				"retries":              st.Retries,
				"prompt_tokens":        st.PromptTokens,
				"completion_tokens":    st.CompletionTokens,
				"latency_count":        st.LatencyCount,
				"latency_seconds_sum":  st.LatencySecondsSum,
			},
		})
	})

	// Review one workload container. The client sends ONLY an identifier; the
	// numbers come from the engine's current Recommendation, so this endpoint
	// cannot be used as a free LLM proxy. Gated by the write Bearer (POST).
	mux.HandleFunc("POST /api/advisor/review", func(w http.ResponseWriter, r *http.Request) {
		if !s.adv.Enabled() {
			advisorError(w, advisor.ErrDisabled)
			return
		}
		var b struct {
			Namespace string `json:"namespace"`
			Kind      string `json:"kind"`
			Workload  string `json:"workload"`
			Container string `json:"container"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, advisorMaxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil || b.Namespace == "" || b.Kind == "" || b.Workload == "" {
			writeErr(w, http.StatusBadRequest, "invalid_input", "namespace, kind, workload required (container optional when the workload has a single one)")
			return
		}
		rec, status, code, msg := s.findRec(b.Namespace, b.Kind, b.Workload, b.Container)
		if status != 0 {
			writeErr(w, status, code, msg)
			return
		}
		in, err := recToDecision(rec)
		if err != nil {
			advisorError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), advisorTimeout)
		defer cancel()
		rv, err := s.adv.Review(ctx, in)
		if err != nil {
			advisorError(w, err)
			return
		}
		writeJSON(w, map[string]any{
			"namespace": rec.Namespace, "kind": rec.Kind, "workload": rec.Workload, "container": rec.Container,
			"review": rv,
		})
	})

	// Summarize the engine's current decisions (top-N by |weekly waste|,
	// optionally one namespace). Same Bearer gate as review.
	mux.HandleFunc("POST /api/advisor/summary", func(w http.ResponseWriter, r *http.Request) {
		if !s.adv.Enabled() {
			advisorError(w, advisor.ErrDisabled)
			return
		}
		var b struct {
			Namespace string `json:"namespace"`
			Limit     int    `json:"limit"`
		}
		if r.ContentLength != 0 {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, advisorMaxBody))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&b); err != nil && !errors.Is(err, io.EOF) {
				writeErr(w, http.StatusBadRequest, "invalid_input", "bad body (namespace?, limit?)")
				return
			}
		}
		if b.Limit < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_input", "limit must be >= 0")
			return
		}
		limit := b.Limit
		if limit == 0 {
			limit = summaryDefaultLimit
		}
		if limit > summaryMaxLimit {
			limit = summaryMaxLimit
		}
		var recs []recommender.Recommendation
		for _, rc := range s.eng.List() {
			if b.Namespace == "" || rc.Namespace == b.Namespace {
				recs = append(recs, rc)
			}
		}
		if len(recs) == 0 {
			writeErr(w, http.StatusNotFound, "no_workloads", "no workloads match")
			return
		}
		abs := func(f float64) float64 {
			if f < 0 {
				return -f
			}
			return f
		}
		sort.SliceStable(recs, func(i, j int) bool { return abs(recs[i].WasteUSDWeek) > abs(recs[j].WasteUSDWeek) })
		total := len(recs)
		if total > limit {
			recs = recs[:limit]
		}
		raw, err := json.Marshal(recs)
		if err != nil {
			advisorError(w, fmt.Errorf("%w: %v", advisor.ErrInvalidInput, err))
			return
		}
		ins, err := advisor.DecisionsFromJSON(raw)
		if err != nil {
			advisorError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), advisorTimeout)
		defer cancel()
		sm, err := s.adv.Summarize(ctx, ins)
		if err != nil {
			advisorError(w, err)
			return
		}
		writeJSON(w, map[string]any{
			"namespace": b.Namespace, "considered": len(ins), "total": total, "summary": sm,
		})
	})
}

// findRec resolves the target Recommendation. A non-zero status means an error
// response. With an empty container, the workload must have exactly one.
func (s *Server) findRec(ns, kind, wl, container string) (rec recommender.Recommendation, status int, code, msg string) {
	if container != "" {
		en, ok := s.eng.Get(ns, kind, wl, container)
		if !ok {
			return rec, http.StatusNotFound, "not_found", "workload not found"
		}
		return en.Rec, 0, "", ""
	}
	var match []recommender.Recommendation
	for _, rc := range s.eng.List() {
		if rc.Namespace == ns && strings.EqualFold(rc.Kind, kind) && rc.Workload == wl {
			match = append(match, rc)
		}
	}
	switch len(match) {
	case 0:
		return rec, http.StatusNotFound, "not_found", "workload not found"
	case 1:
		return match[0], 0, "", ""
	}
	return rec, http.StatusBadRequest, "container_required", "workload has multiple containers: container is required"
}

// writeAdvisorMetrics maps advisor.Stats() to tailor_advisor_* series.
func (s *Server) writeAdvisorMetrics(b *strings.Builder) {
	st := s.adv.Stats()
	counter := func(name, help string, v uint64) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	enabled := 0
	if st.Enabled {
		enabled = 1
	}
	fmt.Fprintf(b, "# HELP tailor_advisor_enabled 1 when the LLM advisor is enabled\n# TYPE tailor_advisor_enabled gauge\ntailor_advisor_enabled %d\n", enabled)
	counter("tailor_advisor_requests_total", "advisor Review/Summarize calls", st.Requests)
	counter("tailor_advisor_cache_hits_total", "advisor cache hits", st.CacheHits)
	counter("tailor_advisor_cache_misses_total", "advisor cache misses", st.CacheMisses)
	counter("tailor_advisor_llm_calls_total", "calls sent to the LLM provider", st.LLMCalls)
	counter("tailor_advisor_failures_total", "provider/transport failures after retries", st.Failures)
	counter("tailor_advisor_invalid_responses_total", "model responses discarded by validation", st.InvalidResponses)
	counter("tailor_advisor_hallucinated_figures_total", "responses discarded for citing numbers absent from the input", st.HallucinatedFigs)
	counter("tailor_advisor_rate_limited_total", "calls rejected by the hourly call limit", st.RateLimited)
	counter("tailor_advisor_budget_rejected_total", "calls rejected by the hourly token budget", st.BudgetRejected)
	counter("tailor_advisor_circuit_rejected_total", "calls rejected by the open circuit breaker", st.CircuitRejected)
	counter("tailor_advisor_retries_total", "provider retries (429/5xx)", st.Retries)
	fmt.Fprintf(b, "# HELP tailor_advisor_tokens_total LLM tokens consumed\n# TYPE tailor_advisor_tokens_total counter\n")
	fmt.Fprintf(b, "tailor_advisor_tokens_total{type=\"prompt\"} %d\ntailor_advisor_tokens_total{type=\"completion\"} %d\n", st.PromptTokens, st.CompletionTokens)
	fmt.Fprintf(b, "# HELP tailor_advisor_llm_latency_seconds provider call latency\n# TYPE tailor_advisor_llm_latency_seconds summary\n")
	fmt.Fprintf(b, "tailor_advisor_llm_latency_seconds_sum %g\ntailor_advisor_llm_latency_seconds_count %d\n", st.LatencySecondsSum, st.LatencyCount)
	fmt.Fprintf(b, "# HELP tailor_advisor_breaker_state circuit breaker state (1 = current)\n# TYPE tailor_advisor_breaker_state gauge\n")
	for _, name := range []string{"closed", "open", "half-open"} {
		v := 0
		if st.BreakerState == name {
			v = 1
		}
		fmt.Fprintf(b, "tailor_advisor_breaker_state{state=%q} %d\n", name, v)
	}
	fmt.Fprintf(b, "# HELP tailor_advisor_cache_entries entries currently cached\n# TYPE tailor_advisor_cache_entries gauge\ntailor_advisor_cache_entries %d\n", st.CacheEntries)
}
