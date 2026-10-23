package advisor

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config configures the advisor. The zero value is "disabled".
type Config struct {
	Enabled bool

	// Provider (OpenAI-compatible). Endpoint = BaseURL + "/chat/completions".
	BaseURL string
	APIKey  string // optional (Ollama needs none); never logged or put in errors
	Model   string
	Headers map[string]string // extra headers (e.g. LiteLLM virtual-key routing)

	Timeout          time.Duration // per HTTP attempt
	MaxTokens        int           // completion cap
	ResponseFormat   string        // json_schema (default) | json_object | none
	MaxRetries       int           // retries on 429/5xx only
	RetryBackoff     time.Duration // base backoff (doubles per retry)
	MaxResponseBytes int64         // provider response body cap

	// Privacy.
	Anonymize bool   // replace ns/workload/container names by stable tokens
	Language  string // pt-BR | en

	// Operation.
	MaxCallsPerHour    int // 0 = unlimited
	TokenBudgetPerHour int // 0 = unlimited
	CacheTTL           time.Duration
	CacheSize          int
	BreakerThreshold   int
	BreakerCooldown    time.Duration
	MaxSummaryItems    int

	// Validation.
	FigureTolerance float64 // relative tolerance for cited_figures (0.01 = 1%)
}

// DefaultConfig returns safe defaults (still disabled).
func DefaultConfig() Config {
	return Config{
		Timeout:          30 * time.Second,
		MaxTokens:        800,
		ResponseFormat:   "json_schema",
		MaxRetries:       2,
		RetryBackoff:     500 * time.Millisecond,
		MaxResponseBytes: 1 << 20,
		Language:         "en",
		MaxCallsPerHour:  60,
		CacheTTL:         15 * time.Minute,
		CacheSize:        256,
		BreakerThreshold: 5,
		BreakerCooldown:  60 * time.Second,
		MaxSummaryItems:  30,
		FigureTolerance:  0.01,
	}
}

// String redacts the API key so a stray %v / %+v / log never leaks it.
func (c Config) String() string {
	key := ""
	if c.APIKey != "" {
		key = "***"
	}
	hdr := make([]string, 0, len(c.Headers))
	for k := range c.Headers {
		hdr = append(hdr, k) // names only, values may be secrets
	}
	return fmt.Sprintf("advisor.Config{Enabled:%t BaseURL:%s APIKey:%q Model:%s Headers:%v Timeout:%s Anonymize:%t Language:%s MaxCallsPerHour:%d CacheTTL:%s}",
		c.Enabled, redactURL(c.BaseURL), key, c.Model, hdr, c.Timeout, c.Anonymize, c.Language, c.MaxCallsPerHour, c.CacheTTL)
}

// GoString keeps %#v safe too.
func (c Config) GoString() string { return c.String() }

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	return u.Redacted()
}

// normalize fills defaults for unset numeric fields and validates the config.
func (c Config) normalize() (Config, error) {
	d := DefaultConfig()
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = d.MaxTokens
	}
	if c.ResponseFormat == "" {
		c.ResponseFormat = d.ResponseFormat
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = d.RetryBackoff
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = d.MaxResponseBytes
	}
	if c.Language == "" {
		c.Language = d.Language
	}
	if c.CacheSize <= 0 {
		c.CacheSize = d.CacheSize
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = d.BreakerThreshold
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = d.BreakerCooldown
	}
	if c.MaxSummaryItems <= 0 {
		c.MaxSummaryItems = d.MaxSummaryItems
	}
	if c.FigureTolerance <= 0 {
		c.FigureTolerance = d.FigureTolerance
	}
	if !c.Enabled {
		return c, nil
	}
	switch c.ResponseFormat {
	case "json_schema", "json_object", "none":
	default:
		return c, fmt.Errorf("advisor: invalid response format %q (json_schema|json_object|none)", c.ResponseFormat)
	}
	switch strings.ToLower(c.Language) {
	case "pt-br", "pt":
		c.Language = "pt-BR"
	case "en", "en-us":
		c.Language = "en"
	default:
		return c, fmt.Errorf("advisor: unsupported language %q (pt-BR|en)", c.Language)
	}
	if c.Model == "" {
		return c, fmt.Errorf("advisor: ADVISOR_MODEL is required when enabled")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("advisor: ADVISOR_BASE_URL must be an http(s) URL")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c, nil
}

// ConfigFromEnv reads ADVISOR_* variables:
//
//	ADVISOR_ENABLED, ADVISOR_BASE_URL, ADVISOR_API_KEY, ADVISOR_MODEL,
//	ADVISOR_TIMEOUT, ADVISOR_ANONYMIZE, ADVISOR_MAX_CALLS_PER_HOUR,
//	ADVISOR_CACHE_TTL, ADVISOR_LANGUAGE (pt-BR|en)
//
// plus optional ADVISOR_MAX_TOKENS, ADVISOR_RESPONSE_FORMAT,
// ADVISOR_TOKEN_BUDGET_PER_HOUR and ADVISOR_EXTRA_HEADERS ("K=V,K2=V2").
func ConfigFromEnv() (Config, error) { return ConfigFromLookup(os.LookupEnv) }

// ConfigFromLookup is ConfigFromEnv with an injectable lookup (tests).
func ConfigFromLookup(lookup func(string) (string, bool)) (Config, error) {
	c := DefaultConfig()
	get := func(k string) string { v, _ := lookup(k); return strings.TrimSpace(v) }
	var errs []string
	boolV := func(k string, dst *bool) {
		if v := get(k); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, k+": not a boolean")
				return
			}
			*dst = b
		}
	}
	intV := func(k string, dst *int) {
		if v := get(k); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				errs = append(errs, k+": not a non-negative integer")
				return
			}
			*dst = n
		}
	}
	durV := func(k string, dst *time.Duration) {
		if v := get(k); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				errs = append(errs, k+": not a valid duration")
				return
			}
			*dst = d
		}
	}
	boolV("ADVISOR_ENABLED", &c.Enabled)
	boolV("ADVISOR_ANONYMIZE", &c.Anonymize)
	intV("ADVISOR_MAX_CALLS_PER_HOUR", &c.MaxCallsPerHour)
	intV("ADVISOR_MAX_TOKENS", &c.MaxTokens)
	intV("ADVISOR_TOKEN_BUDGET_PER_HOUR", &c.TokenBudgetPerHour)
	durV("ADVISOR_TIMEOUT", &c.Timeout)
	durV("ADVISOR_CACHE_TTL", &c.CacheTTL)
	c.BaseURL = get("ADVISOR_BASE_URL")
	c.APIKey = get("ADVISOR_API_KEY")
	c.Model = get("ADVISOR_MODEL")
	if v := get("ADVISOR_LANGUAGE"); v != "" {
		c.Language = v
	}
	if v := get("ADVISOR_RESPONSE_FORMAT"); v != "" {
		c.ResponseFormat = v
	}
	if v := get("ADVISOR_EXTRA_HEADERS"); v != "" {
		c.Headers = map[string]string{}
		for _, kv := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(kv, "=")
			k = strings.TrimSpace(k)
			if !ok || k == "" {
				errs = append(errs, "ADVISOR_EXTRA_HEADERS: expected K=V pairs")
				break
			}
			c.Headers[k] = strings.TrimSpace(val)
		}
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("advisor: invalid env: %s", strings.Join(errs, "; "))
	}
	if _, err := c.normalize(); err != nil {
		return c, err
	}
	return c, nil
}
