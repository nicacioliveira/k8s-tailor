package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
)

// readPaths are GET /api/* routes that expose cluster data (names, sizes, costs,
// decisions). With TAILOR_API_PROTECT_READS=true they need the same Bearer token
// as the writes.
var readPaths = []string{
	"/api/recommendations",
	"/api/overview",
	"/api/policies",
	"/api/groups",
	"/api/groups/g1/members",
	"/api/profiles",
	"/api/economy",
	"/api/actuations",
	"/api/detail/ns/Deployment/w/c",
	"/api/patch/ns/Deployment/w/c",
	"/api/advisor/status",
}

// TestAPIReadAuth: optional read protection (TAILOR_API_PROTECT_READS).
func TestAPIReadAuth(t *testing.T) {
	t.Run("off by default: reads stay open", func(t *testing.T) {
		s, _ := newTestServer(t, config.Config{APIToken: "s3cret"})
		h := s.Handler()
		for _, p := range readPaths {
			if rr := do(h, "GET", p, "", ""); rr.Code == http.StatusUnauthorized || rr.Code == http.StatusServiceUnavailable {
				t.Errorf("GET %s without token = %d, want open", p, rr.Code)
			}
		}
	})

	t.Run("on: every /api read needs the token", func(t *testing.T) {
		s, _ := newTestServer(t, config.Config{APIToken: "s3cret", APIProtectReads: true})
		h := s.Handler()
		for _, p := range readPaths {
			for _, auth := range []string{"", "Bearer wrong", "Bearer s3cre", "Bearer s3cret2", "s3cret", "Basic s3cret", "bearer s3cret"} {
				rr := do(h, "GET", p, "", auth)
				if rr.Code != http.StatusUnauthorized {
					t.Errorf("GET %s auth=%q = %d, want 401", p, auth, rr.Code)
				}
				if got := rr.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
					t.Errorf("GET %s: WWW-Authenticate = %q", p, got)
				}
			}
			if rr := do(h, "HEAD", p, "", ""); rr.Code != http.StatusUnauthorized {
				t.Errorf("HEAD %s without token = %d, want 401", p, rr.Code)
			}
			if rr := do(h, "GET", p, "", "Bearer s3cret"); rr.Code == http.StatusUnauthorized || rr.Code == http.StatusServiceUnavailable {
				t.Errorf("GET %s with token = %d", p, rr.Code)
			}
		}
		// Writes keep their own gate.
		if rr := do(h, "POST", "/api/pause", `{"paused":true}`, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("POST without token = %d, want 401", rr.Code)
		}
		if rr := do(h, "POST", "/api/pause", `{"paused":true}`, "Bearer s3cret"); rr.Code != http.StatusOK {
			t.Errorf("POST with token = %d %s", rr.Code, rr.Body)
		}
	})

	t.Run("on: probes, metrics scrape and the UI shell stay open", func(t *testing.T) {
		s, _ := newTestServer(t, config.Config{APIToken: "s3cret", APIProtectReads: true})
		h := s.Handler()
		for _, p := range []string{"/healthz", "/livez", "/metrics", "/"} {
			rr := do(h, "GET", p, "", "")
			if rr.Code == http.StatusUnauthorized {
				t.Errorf("GET %s = 401; probes/metrics/UI shell must not need the token", p)
			}
		}
	})

	t.Run("on without a token: reads fail closed with an explicit 503", func(t *testing.T) {
		s, _ := newTestServer(t, config.Config{APIProtectReads: true})
		h := s.Handler()
		for _, p := range readPaths {
			rr := do(h, "GET", p, "", "Bearer anything")
			if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "TAILOR_API_PROTECT_READS") {
				t.Errorf("GET %s = %d %s, want 503 naming TAILOR_API_PROTECT_READS", p, rr.Code, rr.Body)
			}
		}
		if rr := do(h, "GET", "/healthz", "", ""); rr.Code != http.StatusOK {
			t.Errorf("/healthz = %d", rr.Code)
		}
	})
}
