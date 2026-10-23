// Package server exposes the API, Prometheus metrics and the embedded React UI.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/advisor"
	"github.com/nicacioliveira/k8s-tailor/internal/ch"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
	webui "github.com/nicacioliveira/k8s-tailor/web"
)

type Server struct {
	cfg   config.Config
	eng   *recommender.Engine
	vm    *vm.Client
	store *actuation.Store
	ch    *ch.Client

	adv      *advisor.Advisor // optional; nil/disabled → advisor routes answer 404
	advModel string

	// allowedHosts: Host names (lower-case, no port) a BROWSER write may target
	// (DNS-rebinding guard, see hostAllowed). From TAILOR_ALLOWED_HOSTS.
	allowedHosts []string
}

func New(cfg config.Config, eng *recommender.Engine, vmc *vm.Client, store *actuation.Store, chc *ch.Client) *Server {
	s := &Server{cfg: cfg, eng: eng, vm: vmc, store: store, ch: chc}
	s.SetAllowedHosts(strings.Split(os.Getenv("TAILOR_ALLOWED_HOSTS"), ","))
	return s
}

// SetAllowedHosts replaces the Host allowlist for browser writes (empty entries
// are ignored; an empty list restores the default — see hostAllowed).
func (s *Server) SetAllowedHosts(hosts []string) {
	s.allowedHosts = nil
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			s.allowedHosts = append(s.allowedHosts, h)
		}
	}
}

// actuationLabel derives the cluster's live actuation posture from real state —
// paused, actively actuating (≥1 workload on an actuating policy), or dry-run.
// There is no static actuation flag: scope lives in the TailorPolicy CRs.
func (s *Server) actuationLabel() string {
	if s.eng.Paused() {
		return "paused"
	}
	for _, r := range s.eng.List() {
		if r.Automated {
			return "active"
		}
	}
	return "dry-run"
}

// parseRange resolves the chart time range from the request. Absolute
// ?from=&to= (RFC3339 or unix seconds) win; otherwise ?window=<dur> back from now;
// otherwise zero → the engine falls back to its trend-window defaults. Returns
// start, end and step (seconds).
func parseRange(r *http.Request) (time.Time, time.Time, int) {
	q := r.URL.Query()
	step := 0
	if v := q.Get("step"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			step = n
		}
	}
	from, to := parseTime(q.Get("from")), parseTime(q.Get("to"))
	if !from.IsZero() && !to.IsZero() {
		return from, to, step
	}
	if v := q.Get("window"); v != "" {
		if d, err := config.ParseDuration(v); err == nil {
			end := time.Now()
			return end.Add(-d), end, step
		}
	}
	return time.Time{}, time.Time{}, step
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok": true, "source": s.cfg.Source, "actuation": s.actuationLabel(),
			"vmUrl": s.cfg.VMURL, "vmHealthy": s.vm.Healthy(r.Context()),
		})
	})

	// Liveness: the process serves HTTP. Deliberately touches no dependency (VM,
	// ClickHouse, API server) so a store outage can't restart-loop the engine.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /api/recommendations", func(w http.ResponseWriter, r *http.Request) {
		recs := s.eng.List()
		// Per-workload "applied N ago" from the DURABLE store (ClickHouse) so it
		// survives engine restarts; fall back to the in-memory ring buffer.
		var counts any = s.store.Counts()
		if s.ch != nil {
			if ds, err := s.ch.DecisionStats(r.Context()); err == nil && len(ds) > 0 {
				counts = ds
			}
		}
		total := 0.0
		for _, rc := range recs {
			if rc.WasteUSDWeek > 0 {
				total += rc.WasteUSDWeek
			}
		}
		writeJSON(w, map[string]any{
			"generatedAt":         time.Now().UTC().Format(time.RFC3339),
			"actuation":           s.actuationLabel(),
			"totalSavingsUsdWeek": round2(total),
			"count":               len(recs),
			"recommendations":     recs,
			"actuationCounts":     counts, // key -> {count,last}
		})
	})

	// Cluster overview: totals + aggregate usage-vs-reserved trend.
	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		ov := s.eng.Overview()
		// Replace the coarse per-workload aggregate with a dense, fine-grained
		// cluster usage series so the Overview charts actually show movement.
		start, end, step := parseRange(r)
		if usage, err := s.eng.ClusterUsageSeries(r.Context(), start, end, step); err == nil && len(usage) > 0 {
			ov.Usage = usage
		}
		// Automated = workloads whose resolved policy mode actually actuates.
		automated := 0
		for _, r := range s.eng.List() {
			if r.Automated {
				automated++
			}
		}
		automatedPct := 0.0
		if ov.Workloads > 0 {
			automatedPct = round2(float64(automated) / float64(ov.Workloads) * 100)
		}
		writeJSON(w, map[string]any{
			"overview":         ov,
			"actuation":        s.actuationLabel(),
			"inPlaceSupported": s.eng.InPlaceSupported(r.Context()),
			"paused":           s.eng.Paused(),
			"automatedPct":     automatedPct,
			"automated":        automated,
			"pricingSource":    s.eng.PricingSource(),
			"priceCpuCoreHr":   s.eng.PriceCPUCoreHr(),
			"priceGbHr":        s.eng.PriceGBHr(),
		})
	})

	// Global pause / resume — writes TailorConfig.spec.paused (etcd, no DB).
	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Paused bool `json:"paused"`
		}
		if !decodeJSON(w, r, &body, "bad body") {
			return
		}
		if err := s.eng.SetPaused(r.Context(), body.Paused); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"paused": body.Paused})
	})

	// Groups (TailorPolicy CRs) — list / upsert / delete from the UI.
	mux.HandleFunc("GET /api/policies", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"policies": s.eng.ListPolicies()})
	})
	mux.HandleFunc("POST /api/policies", func(w http.ResponseWriter, r *http.Request) {
		raw, ok := readBody(w, r)
		if !ok {
			return
		}
		var in policy.PolicyView
		var keys map[string]json.RawMessage
		if json.Unmarshal(raw, &in) != nil || json.Unmarshal(raw, &keys) != nil || in.Name == "" {
			jsonError(w, "bad body (name required)", http.StatusBadRequest)
			return
		}
		// Merge semantics: only the keys the caller sent are written; the rest of
		// the CR spec (GitOps/kubectl fields the view lacks) is preserved.
		in.Present = make(map[string]bool, len(keys))
		for k := range keys {
			in.Present[k] = true
		}
		if err := s.eng.UpsertPolicy(r.Context(), in); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, policy.ErrInvalid) {
				status = http.StatusBadRequest
			}
			jsonError(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "name": in.Name})
	})
	mux.HandleFunc("DELETE /api/policies/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.eng.DeletePolicy(r.Context(), r.PathValue("name")); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// Collapse rules (TailorGroup CRs) — app-managed grouping, list/upsert/delete.
	// A group only collapses workloads into a unit; it carries no Mode, so this
	// never turns on actuation (that's a TailorPolicy).
	mux.HandleFunc("GET /api/groups", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"groups": s.eng.ListGroups(r.Context())})
	})
	mux.HandleFunc("POST /api/groups", func(w http.ResponseWriter, r *http.Request) {
		var in policy.GroupView
		if !decodeJSON(w, r, &in, "bad body (name and groupBy required)") {
			return
		}
		if in.Name == "" || in.GroupBy == "" {
			jsonError(w, "bad body (name and groupBy required)", http.StatusBadRequest)
			return
		}
		if err := s.eng.UpsertGroup(r.Context(), in); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "name": in.Name})
	})
	mux.HandleFunc("DELETE /api/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.eng.DeleteGroup(r.Context(), r.PathValue("name")); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	// Drill-down: the raw workloads a group rule folds into its unit(s).
	mux.HandleFunc("GET /api/groups/{name}/members", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"members": s.eng.GroupMembers(r.Context(), r.PathValue("name"))})
	})

	// Reusable profiles (policy dropdown options).
	mux.HandleFunc("GET /api/profiles", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"profiles": s.eng.ProfileNames()})
	})
	// Per-workload override (Automated toggle + Policy dropdown → annotations),
	// or, with "container", the per-container mode/exclude annotations.
	mux.HandleFunc("POST /api/workload", s.postWorkload)

	// Cluster savings ($/week) trend + active pricing basis.
	mux.HandleFunc("GET /api/economy", func(w http.ResponseWriter, r *http.Request) {
		end := time.Now()
		start := end.Add(-6 * time.Hour)
		series := s.eng.EconomySeries(r.Context(), start, end, 300)
		writeJSON(w, map[string]any{
			"series":         series,
			"pricingSource":  s.eng.PricingSource(),
			"priceCpuCoreHr": s.eng.PriceCPUCoreHr(),
			"priceGbHr":      s.eng.PriceGBHr(),
		})
	})

	// Applied decisions: recent feed + per-key stats. Reads the durable decision
	// store (ClickHouse) when configured so history survives restarts; falls back
	// to the in-memory ring buffer otherwise.
	mux.HandleFunc("GET /api/actuations", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 50
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}
		events := s.store.Recent(limit)
		durable := false
		if s.ch != nil {
			start, end, _ := parseRange(r)
			if rows, err := s.ch.Decisions(r.Context(), start, end, q.Get("q"), limit); err == nil {
				events = eventsFromRows(rows)
				durable = true
			}
		}
		writeJSON(w, map[string]any{
			"events":  events,
			"counts":  s.store.Counts(),
			"durable": durable,
		})
	})

	// Drill-down: recommendation + trend + the mutations applied to it.
	mux.HandleFunc("GET /api/detail/{ns}/{kind}/{workload}/{container}", func(w http.ResponseWriter, r *http.Request) {
		ns, kind, wl, c := r.PathValue("ns"), r.PathValue("kind"), r.PathValue("workload"), r.PathValue("container")
		entry, ok := s.eng.Get(ns, kind, wl, c)
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		// Dense, fine-grained series (short window) for the charts; measured usage
		// + our own recommended/current, all from vmsingle.
		start, end, step := parseRange(r)
		ser := s.eng.DetailSeries(r.Context(), ns, kind, wl, c, start, end, step)
		// Actuation markers: prefer the DURABLE log (ClickHouse) so the chart shows
		// past resizes after an engine restart; fall back to the in-memory ring.
		events := s.store.EventsFor(ns, kind, wl, c)
		if s.ch != nil {
			if rows, err := s.ch.Decisions(r.Context(), start, end, wl, 500); err == nil {
				var f []actuation.Event
				for _, e := range eventsFromRows(rows) {
					if e.Namespace == ns && e.Kind == kind && e.Workload == wl && e.Container == c {
						f = append(f, e)
					}
				}
				if len(f) > 0 {
					events = f
				}
			}
		}
		writeJSON(w, map[string]any{
			"rec":      entry.Rec,
			"cpuTrend": ser["cpuTrend"],
			"memTrend": ser["memTrend"],
			"cpuRec":   ser["cpuRec"],
			"cpuCur":   ser["cpuCur"],
			"memRec":   ser["memRec"],
			"memCur":   ser["memCur"],
			"events":   events,
		})
	})

	// Dry-run patch: the exact change the tailor would apply for one container,
	// for both actuation methods. Read-only — never mutates.
	mux.HandleFunc("GET /api/patch/{ns}/{kind}/{workload}/{container}", func(w http.ResponseWriter, r *http.Request) {
		ns, kind, wl, c := r.PathValue("ns"), r.PathValue("kind"), r.PathValue("workload"), r.PathValue("container")
		entry, ok := s.eng.Get(ns, kind, wl, c)
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		rec := entry.Rec.Recommended
		reqs := map[string]any{"cpu": milliStr(rec.CPURequest), "memory": miStr(rec.MemRequest)}
		lims := map[string]any{}
		if rec.CPULimit > 0 {
			lims["cpu"] = milliStr(rec.CPULimit)
		}
		if rec.MemLimit > 0 {
			lims["memory"] = miStr(rec.MemLimit)
		}
		resources := map[string]any{"requests": reqs}
		if len(lims) > 0 {
			resources["limits"] = lims
		}
		container := map[string]any{"name": c, "resources": resources}
		writeJSON(w, map[string]any{
			"method": string(entry.Apply),
			"inPlace": map[string]any{ // pods/resize subresource — no restart
				"subresource": "resize",
				"patchType":   "strategic-merge",
				"body":        map[string]any{"spec": map[string]any{"containers": []any{container}}},
			},
			"rollout": map[string]any{ // controller template patch — native updateStrategy
				"target":    kind,
				"patchType": "strategic-merge",
				"body":      map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{container}}}}},
			},
		})
	})

	s.registerAdvisor(mux)

	// Prometheus metrics — scraped by vmsingle, so decisions live in Victoria too.
	mux.HandleFunc("GET /metrics", s.metrics)

	// Serve the SAME MCP dashboard (single self-contained HTML, embedded) as the
	// MCP server — so :8080 is identical to the product UI. Inject the REST flag
	// so its data layer talks to this engine's /api instead of an MCP host.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		b, err := webui.Files.ReadFile("dist/index.html")
		if err != nil {
			// Clean clone / `go build` without `make ui-build`: only the
			// placeholder is embedded. The API keeps working.
			http.Error(w, "UI not built into this binary — run `make ui-build` (bun run build:web) and rebuild; the /api is unaffected", http.StatusServiceUnavailable)
			return
		}
		html := strings.Replace(string(b), "<head>",
			"<head>\n<script>window.__TAILOR_REST__=true</script>", 1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	})

	return s.guardWrites(s.requireWriteAuth(mux))
}

// maxWriteBody caps every mutating /api request body (a policy with many
// container rules is a few KiB; 1 MiB is generous and bounds memory per request).
const maxWriteBody = 1 << 20

func isWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/api/")
}

// guardWrites is the CSRF + resource guard in front of every mutating /api call:
//   - cross-site browser requests are refused (Origin not this host, Origin
//     "null", or Sec-Fetch-Site: cross-site) — 403;
//   - a request with a body (or any Content-Type) must be application/json —
//     415. HTML forms always send urlencoded/multipart/text-plain, so a
//     cross-site form POST (a "simple request", no preflight) never reaches a
//     handler. Body-less requests (MCP DELETE, empty advisor summary) carry no
//     payload to forge and, from a browser, still carry an Origin (checked above);
//   - bodies are capped at maxWriteBody — 413.
//
// Non-browser clients (MCP server, curl) send no Origin and pass.
func (s *Server) guardWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWrite(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r) {
			jsonError(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if isBrowser(r) && !s.hostAllowed(r.Host) {
			jsonError(w, "host not allowed", http.StatusForbidden)
			return
		}
		hasBody := r.ContentLength > 0 || r.ContentLength < 0 && len(r.TransferEncoding) > 0 || r.Header.Get("Content-Type") != ""
		if hasBody {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				jsonError(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxWriteBody)
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether a browser request comes from the page this server
// served. No Origin and no Sec-Fetch-Site = not a browser (or a same-origin GET
// navigation) — allowed; auth is the control for those.
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false // includes the opaque "null" origin (sandboxed iframes, file://)
	}
	return strings.EqualFold(u.Host, r.Host)
}

// isBrowser: a browser always sends Origin on a cross-origin or non-GET fetch,
// and Sec-Fetch-Site on any request. Neither = the MCP server, curl, a script.
func isBrowser(r *http.Request) bool {
	return r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != ""
}

// hostAllowed is the DNS-rebinding guard for browser writes: a page at
// evil.example whose name resolves to the engine is "same origin" (Origin host ==
// Host), so the Host itself must be a name we serve. TAILOR_ALLOWED_HOSTS lists
// them; without it, an OPEN write API (no token) only accepts loopback names
// (kubectl port-forward), and a token-gated one any (the token, which a rebound
// page cannot read, is the control).
func (s *Server) hostAllowed(host string) bool {
	name := strings.ToLower(host)
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	if len(s.allowedHosts) > 0 {
		for _, a := range s.allowedHosts {
			if strings.TrimSuffix(strings.TrimPrefix(a, "["), "]") == name {
				return true
			}
		}
		return false
	}
	if s.cfg.APIToken == "" {
		return name == "localhost" || name == "127.0.0.1" || name == "::1"
	}
	return true
}

// readBody reads a (MaxBytesReader-capped) request body, answering 413/400.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(w, fmt.Sprintf("body larger than %d bytes", tooBig.Limit), http.StatusRequestEntityTooLarge)
			return nil, false
		}
		jsonError(w, "unreadable body", http.StatusBadRequest)
		return nil, false
	}
	return b, true
}

// decodeJSON decodes the body into v, answering 413 (too large) or 400 (msg).
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, msg string) bool {
	b, ok := readBody(w, r)
	if !ok {
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		jsonError(w, msg, http.StatusBadRequest)
		return false
	}
	return true
}

// jsonError writes {"error": msg} with proper escaping (error strings may
// contain quotes from the API server).
func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// requireWriteAuth gates every MUTATING /api/ call (anything but GET/HEAD/
// OPTIONS) behind `Authorization: Bearer <TAILOR_API_TOKEN>`. With no token
// configured the API stays open unless TAILOR_API_AUTH_REQUIRED=true, which
// fails closed.
//
// Reads (GET/HEAD /api/*) are open by default; TAILOR_API_PROTECT_READS=true puts
// them behind the same token (and fails closed, 503, when no token is
// configured). Outside /api/ nothing is gated: /healthz and /livez (kubelet
// probes), /metrics (the vmsingle scrape carries no token) and the UI shell
// (it must load to ask for the token).
func (s *Server) requireWriteAuth(next http.Handler) http.Handler {
	token := s.cfg.APIToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		case http.MethodGet, http.MethodHead:
			if !s.cfg.APIProtectReads {
				next.ServeHTTP(w, r)
				return
			}
			if token == "" {
				http.Error(w, `{"error":"read API disabled: TAILOR_API_PROTECT_READS is set but TAILOR_API_TOKEN is empty"}`, http.StatusServiceUnavailable)
				return
			}
			if !bearerOK(r, token) {
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if token == "" {
			if s.cfg.APIAuthRequired {
				http.Error(w, `{"error":"write API disabled: TAILOR_API_AUTH_REQUIRED is set but TAILOR_API_TOKEN is empty"}`, http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !bearerOK(r, token) {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerOK compares the request's Bearer token with the configured one in
// constant time (no early exit on the first differing byte).
func bearerOK(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="k8s-tailor"`)
	http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
}

// workloadWrite is the POST /api/workload body. Without "container" it is the
// legacy per-workload write (mode/profile). With "container" it writes
// tailor.k8s-tailor.dev/container.<name>.mode and/or .exclude instead.
type workloadWrite struct {
	Namespace string  `json:"namespace"`
	Kind      string  `json:"kind"`
	Workload  string  `json:"workload"`
	Mode      *string `json:"mode"`    // Off|Recommend|Webhook|InPlace|Both; "" or "inherit" removes
	Profile   *string `json:"profile"` // workload-level only; "" removes
	Container string  `json:"container"`
	Exclude   *bool   `json:"exclude"` // container-level only; false removes
}

func (s *Server) postWorkload(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var b workloadWrite
	if err := json.Unmarshal(body, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if b.Namespace == "" || b.Kind == "" || b.Workload == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "namespace, kind, workload required")
		return
	}
	if b.Container == "" {
		if b.Exclude != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "exclude requires container")
			return
		}
		if err := s.eng.SetWorkload(r.Context(), b.Namespace, b.Kind, b.Workload, b.Mode, b.Profile); err != nil {
			writeWorkloadErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if b.Profile != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "profile is workload-level; omit it when container is set")
		return
	}
	if err := s.eng.SetContainer(r.Context(), b.Namespace, b.Kind, b.Workload, b.Container, b.Mode, b.Exclude); err != nil {
		writeWorkloadErr(w, err)
		return
	}
	out := map[string]any{"ok": true, "namespace": b.Namespace, "kind": b.Kind, "workload": b.Workload, "container": b.Container}
	if b.Mode != nil {
		m := *b.Mode
		if m == "" {
			m = policy.ModeInherit
		}
		out["mode"] = m
	}
	if b.Exclude != nil {
		out["exclude"] = *b.Exclude
	}
	writeJSON(w, out)
}

// writeWorkloadErr maps engine write errors to 400/404 with a stable code.
func writeWorkloadErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policy.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid_mode", err.Error())
	case errors.Is(err, recommender.ErrNothingToChange):
		writeErr(w, http.StatusBadRequest, "nothing_to_change", err.Error())
	case errors.Is(err, k8s.ErrUnsupportedKind):
		writeErr(w, http.StatusBadRequest, "unsupported_kind", err.Error())
	case errors.Is(err, recommender.ErrContainerNotResizable):
		writeErr(w, http.StatusBadRequest, "container_not_resizable", err.Error())
	case errors.Is(err, recommender.ErrWorkloadNotFound):
		writeErr(w, http.StatusNotFound, "workload_not_found", err.Error())
	case errors.Is(err, recommender.ErrContainerNotFound):
		writeErr(w, http.StatusNotFound, "container_not_found", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	lbl := func(r recommender.Recommendation) string {
		return fmt.Sprintf(`{namespace=%q,workload=%q,container=%q}`, r.Namespace, r.Workload, r.Container)
	}
	list := s.eng.List() // one consistent copy for every per-rec gauge
	gauge := func(name, help string, val func(recommender.Recommendation) float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, r := range list {
			fmt.Fprintf(&b, "%s%s %g\n", name, lbl(r), val(r))
		}
	}
	gauge("tailor_recommended_cpu_cores", "recommended CPU request in cores", func(r recommender.Recommendation) float64 { return r.Recommended.CPURequest })
	gauge("tailor_recommended_mem_bytes", "recommended memory request in bytes", func(r recommender.Recommendation) float64 { return r.Recommended.MemRequest })
	// "current" = what the LIVE pods actually reserve (in-place resizes land here);
	// the template may lag (spec drift). This is what the node reserves and what
	// the charts should draw the waste band against.
	gauge("tailor_current_cpu_cores", "live CPU request in cores", func(r recommender.Recommendation) float64 { return r.Applied.CPURequest })
	gauge("tailor_current_mem_bytes", "live memory request in bytes", func(r recommender.Recommendation) float64 { return r.Applied.MemRequest })
	gauge("tailor_waste_usd_week", "weekly $ recoverable (positive) or needed (negative)", func(r recommender.Recommendation) float64 { return r.WasteUSDWeek })
	// Prediction basis — so dashboards can render the statistical estimate itself.
	gauge("tailor_cpu_p90_cores", "decayed p90 CPU usage in cores", func(r recommender.Recommendation) float64 { return r.Signals.CPUQuantile })
	gauge("tailor_mem_p95_bytes", "decayed p95 memory working set in bytes", func(r recommender.Recommendation) float64 { return r.Signals.MemQuantile })
	// CPU-greedy: the container keeps using every CPU limit it is given, so its
	// limit was parked after CPU_CENSOR_MAX_STEPS raises (1 = parked).
	gauge("tailor_cpu_greedy", "1 when the container is CPU-greedy: its CPU limit is parked after repeated raises it consumed", func(r recommender.Recommendation) float64 {
		if r.Signals.CPUGreedy {
			return 1
		}
		return 0
	})
	// Headroom breach (SLI): fraction of the window usage exceeded the recommended
	// request. High => the rec is too tight. A durable, query-cheap validation loop.
	gauge("tailor_headroom_breach_cpu_share", "fraction of window CPU samples above the recommended request", func(r recommender.Recommendation) float64 { return r.Signals.HeadroomBreachCPU })
	gauge("tailor_headroom_breach_mem_share", "fraction of window memory samples above the recommended request", func(r recommender.Recommendation) float64 { return r.Signals.HeadroomBreachMem })
	// predict_linear 24h projection (nullable — only emitted when computable).
	fmt.Fprintf(&b, "# HELP tailor_predicted_mem_bytes projected memory working set 24h ahead (predict_linear)\n# TYPE tailor_predicted_mem_bytes gauge\n")
	for _, r := range s.eng.List() {
		if r.Signals.MemTrendBytes != nil {
			fmt.Fprintf(&b, "tailor_predicted_mem_bytes%s %g\n", lbl(r), *r.Signals.MemTrendBytes)
		}
	}

	// Applied-mutation counters — the durable record of decisions.
	_, perKey := s.store.Snapshot()
	fmt.Fprintf(&b, "# HELP tailor_mutations_total mutations applied by the webhook\n# TYPE tailor_mutations_total counter\n")
	keys := make([]string, 0, len(perKey))
	for k := range perKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := strings.SplitN(k, "/", 4) // ns/kind/workload/container
		if len(p) == 4 {
			fmt.Fprintf(&b, `tailor_mutations_total{namespace=%q,workload=%q,container=%q} %d`+"\n", p[0], p[2], p[3], perKey[k])
		}
	}

	// Fleet-level gauges computed from the engine state at scrape time.
	recs := list
	byStatus := map[string]int{}
	modeOf := map[string]string{} // ns/kind/workload -> effective mode
	for _, r := range recs {
		byStatus[recStatus(r)]++
		modeOf[r.Namespace+"/"+r.Kind+"/"+r.Workload] = r.Mode
	}
	byMode := map[string]int{}
	for _, m := range modeOf {
		if m == "" {
			m = "unknown"
		}
		byMode[m]++
	}
	writeCounts := func(name, help, label string, m map[string]int) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s{%s=%q} %d\n", name, label, k, m[k])
		}
	}
	writeCounts("tailor_recommendations", "container recommendations by status (learning|oom_risk|underprovisioned|overprovisioned|rightsized)", "status", byStatus)
	writeCounts("tailor_workloads", "observed workloads by effective mode (Off|Recommend|Webhook|InPlace|Both)", "mode", byMode)
	paused := 0
	if s.eng.Paused() {
		paused = 1
	}
	fmt.Fprintf(&b, "# HELP tailor_paused 1 when the global kill switch (TailorConfig.spec.paused) is on\n# TYPE tailor_paused gauge\ntailor_paused %d\n", paused)

	// Operational counters/histograms (resizes, skips, refresh latency, backend
	// errors, leadership) from the shared registry.
	metrics.Default.Write(&b)
	s.writeAdvisorMetrics(&b)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

// statusTolerance is the relative gap below which live and recommended requests
// count as equal (same 15% the reconciler uses as its drift threshold).
const statusTolerance = 0.15

// recStatus buckets a recommendation for the fleet gauge. Precedence: thin data
// first (no claim either way), then OOM (a health problem), then a SHORTFALL on
// any resource (under beats over: a starved resource is the urgent one).
func recStatus(r recommender.Recommendation) string {
	switch {
	case !r.Confident:
		return "learning"
	case r.OOMRisk:
		return "oom_risk"
	}
	under := func(live, rec float64) bool { return rec > live*(1+statusTolerance) && rec-live > 1e-9 }
	if under(r.Applied.CPURequest, r.Recommended.CPURequest) || under(r.Applied.MemRequest, r.Recommended.MemRequest) {
		return "underprovisioned"
	}
	if under(r.Recommended.CPURequest, r.Applied.CPURequest) || under(r.Recommended.MemRequest, r.Applied.MemRequest) {
		return "overprovisioned"
	}
	return "rightsized"
}

// Timeouts for every http.Server the engine runs (API/UI and webhook). Without
// them a client trickling headers (slowloris) pins a conn + goroutine forever.
// WriteTimeout covers the slowest handler: an advisor call (ADVISOR_TIMEOUT per
// attempt, default 30s, with a retry) — keep it above that.
var Timeouts = struct {
	ReadHeader, Read, Write, Idle time.Duration
}{ReadHeader: 10 * time.Second, Read: 30 * time.Second, Write: 3 * time.Minute, Idle: 2 * time.Minute}

// NewHTTPServer builds an http.Server with the hardened Timeouts.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr: addr, Handler: h,
		ReadHeaderTimeout: Timeouts.ReadHeader,
		ReadTimeout:       Timeouts.Read,
		WriteTimeout:      Timeouts.Write,
		IdleTimeout:       Timeouts.Idle,
		MaxHeaderBytes:    64 << 10,
	}
}

// ListenAndServe serves until ctx is done, then shuts down gracefully (in-flight
// requests get up to 5s). It returns only AFTER the shutdown finished — not as
// soon as the listener closes — so the caller can sequence what comes next
// (flush the decision store, exit) without cutting responses mid-write.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := NewHTTPServer(fmt.Sprintf(":%d", s.cfg.Port), s.Handler())
	log.Print(s.cfg.AuthPosture())
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		<-done
	}
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func round2(f float64) float64 { return float64(int64(f*100)) / 100 }

// eventsFromRows reconstructs decision events from ClickHouse JSONEachRow rows
// (numeric fields may arrive as JSON numbers or strings — tolerate both).
func eventsFromRows(rows []map[string]any) []actuation.Event {
	out := make([]actuation.Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, actuation.Event{
			Time: int64(logF(r["time"])), Mode: logStr(r["mode"]), Namespace: logStr(r["namespace"]),
			Kind: logStr(r["kind"]), Workload: logStr(r["workload"]), Container: logStr(r["container"]),
			FromCPU: logF(r["fromCpu"]), ToCPU: logF(r["toCpu"]),
			FromMem: logF(r["fromMem"]), ToMem: logF(r["toMem"]),
		})
	}
	return out
}

func logStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func logF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func milliStr(cores float64) string { return fmt.Sprintf("%dm", int64(cores*1000+0.5)) }
func miStr(bytes float64) string    { return fmt.Sprintf("%dMi", int64(bytes/(1024*1024)+0.5)) }
