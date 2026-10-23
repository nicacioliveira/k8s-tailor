package advisor

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// sanitizeName turns an untrusted cluster-derived string into an inert token:
// ASCII letters/digits and ._-/:=@+ only (everything else, including spaces,
// quotes, newlines and angle brackets, becomes "_"), runs of "_" collapsed,
// truncated to max bytes. Real Kubernetes names already satisfy this, so
// legitimate data is unchanged while "ignore previous instructions\n..." is
// reduced to a harmless identifier.
func sanitizeName(s string, max int) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		ok := r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-/:=@+", r))
		if !ok {
			r = '_'
		}
		if r == '_' {
			if lastUnderscore {
				continue
			}
			lastUnderscore = true
		} else {
			lastUnderscore = false
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}

// cleanText sanitizes model output meant for display: control characters
// become spaces, whitespace is trimmed, length is capped (runes).
// Consumers must still render it as TEXT, never as HTML.
func cleanText(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxRunes {
		r := []rune(s)
		s = string(r[:maxRunes]) + "…"
	}
	return s
}

// ---- anonymization -------------------------------------------------------

var tokenRe = regexp.MustCompile(`\b(?:ns|workload|container)-\d+\b`)

// anonymizer maps real names to stable tokens (ns-1, workload-1, container-1)
// and back. Tokens are assigned in the order names are first requested; the
// callers feed sorted input so the mapping is deterministic for equal input.
type anonymizer struct {
	on  bool
	fwd map[string]string // "ns:prod" -> "ns-1"
	rev map[string]string // "ns-1" -> "prod"
	n   map[string]int
}

func newAnonymizer(on bool) *anonymizer {
	return &anonymizer{on: on, fwd: map[string]string{}, rev: map[string]string{}, n: map[string]int{}}
}

func (a *anonymizer) token(kind, name string) string {
	if !a.on || name == "" {
		return name
	}
	k := kind + ":" + name
	if t, ok := a.fwd[k]; ok {
		return t
	}
	a.n[kind]++
	t := kind + "-" + strconv.Itoa(a.n[kind])
	a.fwd[k] = t
	a.rev[t] = name
	return t
}

// restore replaces known tokens in s by the real names. Unknown tokens are
// left untouched.
func (a *anonymizer) restore(s string) string {
	if !a.on || len(a.rev) == 0 {
		return s
	}
	return tokenRe.ReplaceAllStringFunc(s, func(t string) string {
		if r, ok := a.rev[t]; ok {
			return r
		}
		return t
	})
}

// ---- JSON extraction -----------------------------------------------------

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// extractJSON returns the first balanced top-level JSON object found in text,
// tolerating ``` fences, reasoning <think> blocks and chatty prefixes/suffixes.
func extractJSON(text string) (string, bool) {
	text = thinkRe.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1], true
			}
		}
	}
	return "", false
}

// ---- anti-hallucination --------------------------------------------------

// collectNumbers returns every numeric value present in v (as the model sees
// it, after JSON encoding): this is the universe cited_figures must come from.
func collectNumbers(v any) []float64 {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if dec.Decode(&root) != nil {
		return nil
	}
	var out []float64
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case json.Number:
			if f, err := t.Float64(); err == nil {
				out = append(out, f)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(root)
	return out
}

// figureKnown reports whether v matches one of the known numbers within the
// relative tolerance (sign-insensitive: "cut by 40%" may cite 40 or -40).
func figureKnown(v float64, known []float64, tol float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	av := math.Abs(v)
	for _, k := range known {
		ak := math.Abs(k)
		d := math.Abs(av - ak)
		if d < 1e-9 || d <= tol*math.Max(av, ak) {
			return true
		}
	}
	return false
}
