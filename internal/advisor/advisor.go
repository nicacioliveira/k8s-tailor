package advisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Stats is a snapshot of the internal counters. The owner of /metrics maps
// these to Prometheus series (all counters are monotonic except BreakerState
// and CacheEntries).
type Stats struct {
	Enabled           bool
	Requests          uint64 // Review/Summarize calls that reached the advisor
	CacheHits         uint64
	CacheMisses       uint64
	LLMCalls          uint64 // calls that went to the provider
	Failures          uint64 // provider/transport failures (after retries)
	InvalidResponses  uint64 // model output discarded by validation
	HallucinatedFigs  uint64 // subset of InvalidResponses: cited number not in input
	RateLimited       uint64
	BudgetRejected    uint64
	CircuitRejected   uint64
	Retries           uint64
	PromptTokens      uint64
	CompletionTokens  uint64
	LatencyCount      uint64
	LatencySecondsSum float64
	BreakerState      string // closed | open | half-open
	CacheEntries      int
}

type counters struct {
	requests, hits, misses, calls, failures, invalid, halluc atomic.Uint64
	rateLimited, budget, circuit, retries                    atomic.Uint64
	promptTok, complTok, latCount, latNanos                  atomic.Uint64
}

// Option customizes New (mainly for tests and custom transports).
type Option func(*Advisor)

func WithHTTPClient(hc *http.Client) Option { return func(a *Advisor) { a.hc = hc } }
func WithLogger(l *slog.Logger) Option      { return func(a *Advisor) { a.log = l } }
func WithClock(now func() time.Time) Option { return func(a *Advisor) { a.now = now } }

// Advisor is the read-only LLM reviewer. Safe for concurrent use.
type Advisor struct {
	cfg   Config
	hc    *http.Client
	log   *slog.Logger
	now   func() time.Time
	cl    *client
	cache *lruCache
	calls *slidingWindow
	toks  *slidingWindow
	brk   *breaker
	c     counters
}

// New builds an Advisor. A disabled config yields a usable Advisor whose
// methods return ErrDisabled, so callers need no nil checks.
func New(cfg Config, opts ...Option) (*Advisor, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	a := &Advisor{cfg: cfg, now: time.Now}
	for _, o := range opts {
		o(a)
	}
	if a.log == nil {
		a.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	a.cl = newClient(cfg, a.hc, a.log, &a.c.retries)
	a.cache = newLRU(cfg.CacheSize, cfg.CacheTTL, a.now)
	a.calls = newWindow(time.Hour, a.now)
	a.toks = newWindow(time.Hour, a.now)
	a.brk = newBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown, a.now)
	return a, nil
}

// Enabled reports whether the advisor will call a provider. Nil-safe.
func (a *Advisor) Enabled() bool { return a != nil && a.cfg.Enabled }

// Stats returns a counters snapshot. Nil-safe.
func (a *Advisor) Stats() Stats {
	if a == nil {
		return Stats{BreakerState: "closed"}
	}
	return Stats{
		Enabled: a.cfg.Enabled, Requests: a.c.requests.Load(), CacheHits: a.c.hits.Load(),
		CacheMisses: a.c.misses.Load(), LLMCalls: a.c.calls.Load(), Failures: a.c.failures.Load(),
		InvalidResponses: a.c.invalid.Load(), HallucinatedFigs: a.c.halluc.Load(),
		RateLimited: a.c.rateLimited.Load(), BudgetRejected: a.c.budget.Load(),
		CircuitRejected: a.c.circuit.Load(), Retries: a.c.retries.Load(),
		PromptTokens: a.c.promptTok.Load(), CompletionTokens: a.c.complTok.Load(),
		LatencyCount:      a.c.latCount.Load(),
		LatencySecondsSum: float64(a.c.latNanos.Load()) / 1e9,
		BreakerState:      a.brk.current().String(), CacheEntries: a.cache.len(),
	}
}

func validInput(in DecisionInput) error {
	if in.Workload == "" || in.Namespace == "" {
		return fmt.Errorf("%w: namespace and workload are required", ErrInvalidInput)
	}
	if _, err := json.Marshal(in); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err) // NaN/Inf
	}
	return nil
}

func (a *Advisor) cacheKey(kind string, v any) string {
	b, _ := json.Marshal(struct {
		K, M, L string
		A       bool
		V       any
	}{kind, a.cfg.Model, a.cfg.Language, a.cfg.Anonymize, v})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Review asks the model for an opinion on one decision. The result is
// advisory; the advisor cannot change anything.
func (a *Advisor) Review(ctx context.Context, in DecisionInput) (Review, error) {
	if !a.Enabled() {
		return Review{}, ErrDisabled
	}
	a.c.requests.Add(1)
	if err := validInput(in); err != nil {
		return Review{}, err
	}
	key := a.cacheKey("review", in)
	if v, ok := a.cache.get(key); ok {
		a.c.hits.Add(1)
		r := cloneReview(v.(Review))
		r.Cached = true
		return r, nil
	}
	a.c.misses.Add(1)

	an := newAnonymizer(a.cfg.Anonymize)
	p := buildDecision(in, an, true, 8)
	content, err := a.invoke(ctx, systemPrompt(a.cfg.Language, "review"), userMessage(p), reviewSchema)
	if err != nil {
		return Review{}, err
	}
	r, err := parseReview(content, collectNumbers(p), a.cfg.FigureTolerance)
	if err != nil {
		a.noteInvalid(err)
		return Review{}, err
	}
	r = restoreReview(r, an)
	r.Model = a.cfg.Model
	a.cache.put(key, cloneReview(r))
	return r, nil
}

// Summarize asks the model for an opinion on a batch of decisions (the top
// MaxSummaryItems by weekly waste are sent; the rest only count in totals).
func (a *Advisor) Summarize(ctx context.Context, ins []DecisionInput) (Summary, error) {
	if !a.Enabled() {
		return Summary{}, ErrDisabled
	}
	a.c.requests.Add(1)
	if len(ins) == 0 {
		return Summary{}, fmt.Errorf("%w: no decisions", ErrInvalidInput)
	}
	for _, in := range ins {
		if err := validInput(in); err != nil {
			return Summary{}, err
		}
	}
	key := a.cacheKey("summary", ins)
	if v, ok := a.cache.get(key); ok {
		a.c.hits.Add(1)
		s := cloneSummary(v.(Summary))
		s.Cached = true
		return s, nil
	}
	a.c.misses.Add(1)

	an := newAnonymizer(a.cfg.Anonymize)
	p := buildSummary(ins, an, a.cfg.MaxSummaryItems)
	refs := map[string]bool{}
	for _, d := range p.Decisions {
		refs[d.Ref] = true
		refs[d.Workload] = true
	}
	content, err := a.invoke(ctx, systemPrompt(a.cfg.Language, "summary"), userMessage(p), summarySchema)
	if err != nil {
		return Summary{}, err
	}
	s, err := parseSummary(content, collectNumbers(p), refs, a.cfg.FigureTolerance)
	if err != nil {
		a.noteInvalid(err)
		return Summary{}, err
	}
	s = restoreSummary(s, an)
	s.Omitted = p.Totals.Omitted
	s.Model = a.cfg.Model
	a.cache.put(key, cloneSummary(s))
	return s, nil
}

func (a *Advisor) noteInvalid(err error) {
	a.c.invalid.Add(1)
	if errors.Is(err, ErrHallucinatedFigure) {
		a.c.halluc.Add(1)
	}
}

// invoke runs the guarded provider call: breaker, rate limit, token budget,
// accounting.
func (a *Advisor) invoke(ctx context.Context, system, user string, s *schemaSpec) (string, error) {
	if err := a.brk.allow(); err != nil {
		a.c.circuit.Add(1)
		return "", err
	}
	if lim := a.cfg.TokenBudgetPerHour; lim > 0 && a.toks.sum() >= lim {
		a.brk.release()
		a.c.budget.Add(1)
		return "", ErrBudgetExceeded
	}
	if lim := a.cfg.MaxCallsPerHour; lim > 0 && !a.calls.tryAdd(1, lim) {
		a.brk.release()
		a.c.rateLimited.Add(1)
		return "", ErrRateLimited
	}
	a.c.calls.Add(1)
	start := a.now()
	res, err := a.cl.chat(ctx, system, user, s)
	a.c.latCount.Add(1)
	a.c.latNanos.Add(uint64(a.now().Sub(start)))
	if err != nil {
		if ctx.Err() != nil {
			a.brk.release() // caller gave up: not provider's fault
		} else {
			a.brk.failure()
		}
		a.c.failures.Add(1)
		return "", err
	}
	a.brk.success()
	used := res.Usage.TotalTokens
	if used <= 0 {
		used = (len(system) + len(user) + len(res.Content)) / 4 // rough estimate
	}
	a.toks.add(used)
	a.c.promptTok.Add(uint64(max(res.Usage.PromptTokens, 0)))
	a.c.complTok.Add(uint64(max(res.Usage.CompletionTokens, 0)))
	return res.Content, nil
}

// ---- parsing & validation ----------------------------------------------------

type reviewWire struct {
	Verdict         string   `json:"verdict"`
	Confidence      string   `json:"confidence"`
	Rationale       string   `json:"rationale"`
	Risks           []string `json:"risks"`
	SuggestedAction string   `json:"suggested_action"`
	CitedFigures    []Figure `json:"cited_figures"`
}

type summaryWire struct {
	Overview     string          `json:"overview"`
	Verdict      string          `json:"verdict"`
	Confidence   string          `json:"confidence"`
	Highlights   []string        `json:"highlights"`
	Attention    []AttentionItem `json:"attention"`
	CitedFigures []Figure        `json:"cited_figures"`
}

func decodeStrict(content string, dst any) error {
	js, ok := extractJSON(content)
	if !ok {
		return &ValidationError{Kind: ErrMalformedJSON, Detail: "no JSON object found"}
	}
	if err := json.Unmarshal([]byte(js), dst); err != nil {
		return &ValidationError{Kind: ErrMalformedJSON, Detail: err.Error()}
	}
	return nil
}

func parseEnums(v, c string) (Verdict, Confidence, error) {
	vv, cc := Verdict(strings.ToLower(strings.TrimSpace(v))), Confidence(strings.ToLower(strings.TrimSpace(c)))
	switch vv {
	case VerdictOK, VerdictReview, VerdictRisky:
	default:
		return "", "", &ValidationError{Kind: ErrSchemaViolation, Detail: fmt.Sprintf("verdict %q", cleanText(v, 20))}
	}
	switch cc {
	case ConfidenceLow, ConfidenceMedium, ConfidenceHigh:
	default:
		return "", "", &ValidationError{Kind: ErrSchemaViolation, Detail: fmt.Sprintf("confidence %q", cleanText(c, 20))}
	}
	return vv, cc, nil
}

func checkFigures(figs []Figure, known []float64, tol float64) ([]Figure, error) {
	if len(figs) > 20 {
		figs = figs[:20]
	}
	out := make([]Figure, 0, len(figs))
	for _, f := range figs {
		if !figureKnown(f.Value, known, tol) {
			return nil, &ValidationError{Kind: ErrHallucinatedFigure,
				Detail: fmt.Sprintf("%q = %v", cleanText(f.Label, 40), f.Value)}
		}
		out = append(out, Figure{Label: cleanText(f.Label, 80), Value: f.Value, Unit: cleanText(f.Unit, 16)})
	}
	return out, nil
}

func cleanList(in []string, n, maxRunes int) []string {
	out := []string{}
	for _, s := range in {
		if s = cleanText(s, maxRunes); s != "" {
			out = append(out, s)
		}
		if len(out) == n {
			break
		}
	}
	return out
}

func parseReview(content string, known []float64, tol float64) (Review, error) {
	var w reviewWire
	if err := decodeStrict(content, &w); err != nil {
		return Review{}, err
	}
	v, c, err := parseEnums(w.Verdict, w.Confidence)
	if err != nil {
		return Review{}, err
	}
	rat := cleanText(w.Rationale, 1200)
	if rat == "" {
		return Review{}, &ValidationError{Kind: ErrSchemaViolation, Detail: "empty rationale"}
	}
	figs, err := checkFigures(w.CitedFigures, known, tol)
	if err != nil {
		return Review{}, err
	}
	return Review{Verdict: v, Confidence: c, Rationale: rat, Risks: cleanList(w.Risks, 8, 300),
		SuggestedAction: cleanText(w.SuggestedAction, 400), CitedFigures: figs}, nil
}

func parseSummary(content string, known []float64, refs map[string]bool, tol float64) (Summary, error) {
	var w summaryWire
	if err := decodeStrict(content, &w); err != nil {
		return Summary{}, err
	}
	v, c, err := parseEnums(w.Verdict, w.Confidence)
	if err != nil {
		return Summary{}, err
	}
	ov := cleanText(w.Overview, 1200)
	if ov == "" {
		return Summary{}, &ValidationError{Kind: ErrSchemaViolation, Detail: "empty overview"}
	}
	figs, err := checkFigures(w.CitedFigures, known, tol)
	if err != nil {
		return Summary{}, err
	}
	att := []AttentionItem{}
	for _, it := range w.Attention {
		ref := strings.TrimSpace(it.Workload)
		if !refs[ref] {
			return Summary{}, &ValidationError{Kind: ErrSchemaViolation,
				Detail: fmt.Sprintf("attention references unknown workload %q", cleanText(ref, 80))}
		}
		att = append(att, AttentionItem{Workload: ref, Reason: cleanText(it.Reason, 300)})
		if len(att) == 8 {
			break
		}
	}
	return Summary{Overview: ov, Verdict: v, Confidence: c, Highlights: cleanList(w.Highlights, 6, 300),
		Attention: att, CitedFigures: figs}, nil
}

func restoreReview(r Review, a *anonymizer) Review {
	r.Rationale = a.restore(r.Rationale)
	r.SuggestedAction = a.restore(r.SuggestedAction)
	for i := range r.Risks {
		r.Risks[i] = a.restore(r.Risks[i])
	}
	for i := range r.CitedFigures {
		r.CitedFigures[i].Label = a.restore(r.CitedFigures[i].Label)
	}
	return r
}

func restoreSummary(s Summary, a *anonymizer) Summary {
	s.Overview = a.restore(s.Overview)
	for i := range s.Highlights {
		s.Highlights[i] = a.restore(s.Highlights[i])
	}
	for i := range s.Attention {
		s.Attention[i].Workload = a.restore(s.Attention[i].Workload)
		s.Attention[i].Reason = a.restore(s.Attention[i].Reason)
	}
	for i := range s.CitedFigures {
		s.CitedFigures[i].Label = a.restore(s.CitedFigures[i].Label)
	}
	return s
}

func cloneReview(r Review) Review {
	r.Risks = append([]string(nil), r.Risks...)
	r.CitedFigures = append([]Figure(nil), r.CitedFigures...)
	return r
}

func cloneSummary(s Summary) Summary {
	s.Highlights = append([]string(nil), s.Highlights...)
	s.Attention = append([]AttentionItem(nil), s.Attention...)
	s.CitedFigures = append([]Figure(nil), s.CitedFigures...)
	return s
}
