package advisor

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Resources mirrors recommender.Resources (cores / bytes; 0 = unset).
type Resources struct {
	CPURequest float64 `json:"cpuRequest"`
	CPULimit   float64 `json:"cpuLimit,omitempty"`
	MemRequest float64 `json:"memRequest"`
	MemLimit   float64 `json:"memLimit"`
}

// Signals mirrors recommender.Signals.
type Signals struct {
	CPUQuantile       float64  `json:"cpuQuantile"`
	CPUPeak           float64  `json:"cpuPeak"`
	CPUForecast       *float64 `json:"cpuForecast"`
	MemQuantile       float64  `json:"memQuantile"`
	MemPeak           float64  `json:"memPeak"`
	MemForecastBytes  *float64 `json:"memForecastBytes"`
	MemTrendBytes     *float64 `json:"memTrendBytes"`
	Samples           int      `json:"samples"`
	HeadroomBreachCPU float64  `json:"headroomBreachCpu"` // fraction 0..1
	HeadroomBreachMem float64  `json:"headroomBreachMem"` // fraction 0..1
}

// DecisionInput is the advisor's own view of one recommendation/decision. The
// JSON tags intentionally match recommender.Recommendation so DecisionFromJSON
// can ingest it without importing the recommender (and k8s) packages.
type DecisionInput struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	Container string `json:"container"`
	Replicas  int    `json:"replicas"`

	Current     Resources `json:"current"`
	Applied     Resources `json:"applied"`
	Recommended Resources `json:"recommended"`
	Signals     Signals   `json:"signals"`

	Mode      string `json:"mode"`
	Apply     string `json:"apply"`
	Direction string `json:"direction"`
	Policy    string `json:"policy"`

	Automated    bool `json:"automated"`
	Confident    bool `json:"confident"`
	OOMRisk      bool `json:"oomRisk"`
	LimitCapped  bool `json:"limitCapped"`
	SpecDrift    bool `json:"specDrift"`
	PDBProtected bool `json:"pdbProtected"`
	PDBAllowed   int  `json:"pdbAllowed"`
	CPUHeldByHPA bool `json:"cpuHeldByHpa"`
	MemHeldByHPA bool `json:"memHeldByHpa"`

	WasteUSDWeek float64 `json:"wasteUsdWeek"`

	// Labels are optional workload labels. They are untrusted data: sanitized,
	// truncated, capped, and dropped entirely when anonymization is on.
	Labels map[string]string `json:"labels,omitempty"`
}

type Verdict string

const (
	VerdictOK     Verdict = "ok"
	VerdictReview Verdict = "review"
	VerdictRisky  Verdict = "risky"
)

type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// Figure is a number the model cites, in the units of the prompt payload
// (CPU in millicores, memory in MiB, percentages, USD, counts).
type Figure struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

// Review is the advisor's opinion on one decision. It is advisory only.
type Review struct {
	Verdict         Verdict    `json:"verdict"`
	Confidence      Confidence `json:"confidence"`
	Rationale       string     `json:"rationale"`
	Risks           []string   `json:"risks"`
	SuggestedAction string     `json:"suggested_action"`
	CitedFigures    []Figure   `json:"cited_figures"`

	Model  string `json:"model,omitempty"`
	Cached bool   `json:"cached,omitempty"`
}

// AttentionItem points at a decision the summary wants a human to look at.
type AttentionItem struct {
	Workload string `json:"workload"` // "namespace/workload"
	Reason   string `json:"reason"`
}

// Summary is the advisor's opinion on a batch of decisions.
type Summary struct {
	Overview     string          `json:"overview"`
	Verdict      Verdict         `json:"verdict"`
	Confidence   Confidence      `json:"confidence"`
	Highlights   []string        `json:"highlights"`
	Attention    []AttentionItem `json:"attention"`
	CitedFigures []Figure        `json:"cited_figures"`

	Omitted int    `json:"omitted,omitempty"` // decisions not sent (over MaxSummaryItems)
	Model   string `json:"model,omitempty"`
	Cached  bool   `json:"cached,omitempty"`
}

// Typed errors. Use errors.Is.
var (
	ErrDisabled           = errors.New("advisor: disabled")
	ErrInvalidInput       = errors.New("advisor: invalid input")
	ErrRateLimited        = errors.New("advisor: hourly call limit reached")
	ErrBudgetExceeded     = errors.New("advisor: hourly token budget exhausted")
	ErrCircuitOpen        = errors.New("advisor: circuit breaker open")
	ErrResponseTooLarge   = errors.New("advisor: provider response exceeds size limit")
	ErrNoContent          = errors.New("advisor: provider returned no content")
	ErrInvalidResponse    = errors.New("advisor: invalid model response") // parent of the two below
	ErrMalformedJSON      = errors.New("advisor: model response is not valid JSON")
	ErrHallucinatedFigure = errors.New("advisor: cited figure not present in input")
	ErrSchemaViolation    = errors.New("advisor: model response violates schema")
)

// ValidationError is returned (and the model output discarded) when the model
// response fails validation. errors.Is matches both ErrInvalidResponse and the
// specific Kind.
type ValidationError struct {
	Kind   error
	Detail string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%v: %s", e.Kind, e.Detail)
}

func (e *ValidationError) Unwrap() []error { return []error{e.Kind, ErrInvalidResponse} }

// HTTPError is a non-2xx answer from the provider (body is truncated and
// scrubbed of the API key).
type HTTPError struct {
	Status int
	Body   string

	retryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("advisor: provider returned HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}
