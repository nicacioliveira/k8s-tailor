package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/actuation"
	"github.com/nicacioliveira/k8s-tailor/internal/advisor"
	"github.com/nicacioliveira/k8s-tailor/internal/ch"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/leader"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/pricing"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
	"github.com/nicacioliveira/k8s-tailor/internal/reconciler"
	"github.com/nicacioliveira/k8s-tailor/internal/server"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
	"github.com/nicacioliveira/k8s-tailor/internal/webhook"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "backtest" {
		os.Exit(RunBacktest(os.Args[2:]))
	}
	cfg := config.Load()
	// Fail fast on a value the operator did not intend (e.g. OOM_MEM_CAP_FACTOR
	// below OOM_GROWTH_FACTOR) instead of running with a silent fallback.
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("k8s-tailor starting (source=%s vm=%s) — inert until a policy enables actuation", cfg.Source, cfg.VMURL)

	kc, err := k8s.New(cfg)
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}
	pol, err := policy.NewStore(cfg)
	if err != nil {
		log.Fatalf("policy store: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, kc, pol, advisor.ConfigFromEnv); err != nil {
		log.Fatalf("%v", err)
	}
}

// run wires and runs the engine until ctx is done, then shuts down in order.
// It takes the cluster clients (and the advisor config source) as arguments so
// the whole wiring runs in tests against client-go fakes.
//
// Shutdown order on SIGTERM (helm upgrade, drain): ctx is cancelled → the
// actuator stops and the Lease is released (leader.Run returns only after both)
// → the API and webhook servers drain (srvCtx) → pending decisions are flushed
// to ClickHouse → run returns. Must fit in terminationGracePeriodSeconds.
func run(ctx context.Context, cfg config.Config, kc *k8s.Client, pol *policy.Store, advisorConfig func() (advisor.Config, error)) error {
	// Optional read-only LLM advisor (ADVISOR_*; off by default). A bad config
	// aborts the boot instead of silently running without the requested guards.
	advCfg, err := advisorConfig()
	if err != nil {
		return fmt.Errorf("advisor config: %w", err)
	}
	adv, err := advisor.New(advCfg, advisor.WithLogger(slog.Default()))
	if err != nil {
		return fmt.Errorf("advisor: %w", err)
	}

	vmc := vm.New(cfg.VMURL)
	price := pricing.New(cfg, vmc)
	eng := recommender.NewEngine(cfg, vmc, kc, pol, price)
	store := actuation.NewStore(200)

	// Durable decision store (ClickHouse). nil when CH_URL is unset → in-memory only.
	chc := ch.New(cfg.CHURL, cfg.CHDatabase, cfg.CHUser, cfg.CHPassword)

	srvCtx, stopServers := context.WithCancel(context.Background())
	defer stopServers()

	if chc != nil {
		// Retry until ClickHouse is reachable (it may still be starting when the
		// engine boots). Decisions buffer in-memory until the sink attaches, then
		// persist durably — no permanent "in-memory this boot" gap.
		go retryUntil(ctx, chSchemaRetry, func() error { return chc.EnsureSchema(ctx, cfg.CHRetentionDays) }, func(attempt int, err error) {
			log.Printf("clickhouse schema not ready (attempt %d): %v", attempt, err)
		}, func() {
			store.SetSink(chc)
			log.Printf("decision store -> ClickHouse %s db=%s", cfg.CHURL, cfg.CHDatabase)
		})
	}

	// Watch TailorPolicy / TailorConfig CRs (config lives in etcd, no DB).
	go pol.Run(ctx, 30*time.Second)
	// Refresh node pricing (opencost/aws/static).
	go price.Run(ctx, 5*time.Minute)
	// Warm cache: load what the leader wrote before the first refresh, so pods
	// created right after this start are sized by the webhook instead of admitted
	// unmodified (reason="warming_up"). Bounded; a failure only means a cold start.
	loadWarmCache(ctx, eng)

	// Recommender refresh loop (every replica: the API/UI/webhook read it). The
	// leader also writes the warm cache after each successful refresh.
	go refreshLoop(ctx, cfg.RefreshEvery, eng, warmCacheWriter(eng))

	// In-place resize reconciler — live actuation on running pods (no restart).
	// With LEADER_ELECT=true only the holder of the Lease actuates, so replicas>1
	// never double-resize; the cooldown is rebuilt from pod annotations on
	// failover (see reconciler.cooled).
	rec := reconciler.New(cfg, eng, kc, store)
	actuatorDone := make(chan struct{})
	if cfg.LeaderElect {
		go func() {
			defer close(actuatorDone)
			err := leader.Run(ctx, kc.Clientset(), leader.Options{
				Namespace: cfg.LeaseNS, Name: cfg.LeaseName, Identity: cfg.LeaseIdentity,
				StopTimeout: actuatorStopTimeout,
			}, rec.Run)
			if err != nil {
				log.Fatalf("leader election: %v", err)
			}
		}()
	} else {
		metrics.Leader.Set(1)
		go func() { defer close(actuatorDone); rec.Run(ctx) }()
	}
	// Servers keep answering (webhook admissions, UI) until actuation stopped
	// and the Lease is released; only then do they drain.
	go stopServersAfterActuator(ctx, actuatorDone, actuatorStopTimeout+5*time.Second, stopServers)

	startWebhookWhenReady(ctx, srvCtx, cfg, eng, kc)

	// API + UI (HTTP).
	log.Print(writeAuthBanner(cfg))
	srv := server.New(cfg, eng, vmc, store, chc)
	srv.SetAdvisor(adv, advCfg.Model)
	if adv.Enabled() {
		log.Printf("advisor: enabled (%s)", advCfg) // Config.String masks credentials
	}
	log.Printf("api+ui listening on :%d", cfg.Port)
	if err := srv.ListenAndServe(srvCtx); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	// Decisions recorded during the last pass would otherwise be lost: the
	// background flusher only runs every few seconds.
	fctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := store.Flush(fctx); err != nil {
		log.Printf("shutdown: decision store flush: %v", err)
	}
	log.Printf("shutdown complete")
	return nil
}

// refresher is the part of the engine the refresh loop drives.
type refresher interface {
	Refresh(ctx context.Context) error
	RefreshTimeout() time.Duration
	List() []recommender.Recommendation
}

// refreshLoop refreshes immediately, then every `every`, each pass bounded by
// RefreshTimeout, until ctx is done. A failed pass is logged, never fatal.
func refreshLoop(ctx context.Context, every time.Duration, eng refresher, afterRefresh func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, eng.RefreshTimeout())
		err := eng.Refresh(rctx)
		cancel()
		if err != nil {
			log.Printf("refresh error: %v", err)
		} else {
			log.Printf("refresh ok: %d recommendations", len(eng.List()))
			if afterRefresh != nil {
				afterRefresh(ctx)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// warmCache is the part of the engine the warm cache needs.
type warmCache interface {
	LoadWarm(ctx context.Context) (int, error)
	SaveWarm(ctx context.Context) error
}

// loadWarmCache loads the warm cache (see recommender.LoadWarm), bounded to a few
// seconds so a slow API server cannot delay the boot.
func loadWarmCache(ctx context.Context, eng warmCache) {
	lctx, cancel := context.WithTimeout(ctx, warmLoadTimeout)
	defer cancel()
	n, err := eng.LoadWarm(lctx)
	switch {
	case err != nil:
		log.Printf("warm cache: not loaded (%v); pods admitted before the first refresh are left unmodified", err)
	case n > 0:
		log.Printf("warm cache: loaded %d recommendation(s) for the webhook; the first refresh replaces them", n)
	}
}

// warmCacheWriter returns the after-refresh hook of the leader: it writes the warm
// cache, and logs a failure once per outage (a missing RBAC rule would otherwise
// repeat every refresh).
func warmCacheWriter(eng warmCache) func(context.Context) {
	failing := false
	return func(ctx context.Context) {
		if metrics.Leader.Value() != 1 {
			return
		}
		wctx, cancel := context.WithTimeout(ctx, warmLoadTimeout)
		defer cancel()
		if err := eng.SaveWarm(wctx); err != nil {
			if !failing {
				log.Printf("warm cache: write failed (will keep trying quietly): %v", err)
			}
			failing = true
			return
		}
		failing = false
	}
}

// retryUntil calls try every `every` until it succeeds (then calls ok) or ctx
// is done. onErr sees each failed attempt (1-based).
func retryUntil(ctx context.Context, every time.Duration, try func() error, onErr func(int, error), ok func()) {
	for attempt := 1; ; attempt++ {
		err := try()
		if err == nil {
			ok()
			return
		}
		onErr(attempt, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// stopServersAfterActuator is the shutdown gate between actuation and the
// servers: once ctx is done it waits for the actuator (and the Lease release)
// to finish, bounded by maxWait, and only then stops the servers — so webhook
// admissions and the UI keep answering while the last pass completes.
func stopServersAfterActuator(ctx context.Context, actuatorDone <-chan struct{}, maxWait time.Duration, stopServers func()) {
	<-ctx.Done()
	log.Printf("shutdown: stopping actuation and releasing the lease")
	select {
	case <-actuatorDone:
	case <-time.After(maxWait):
		log.Printf("shutdown: actuator did not stop in time — closing servers anyway")
	}
	stopServers()
}

// startWebhookWhenReady starts the mutating webhook (HTTPS) only when cert
// material is present. When the cert directory is mounted but still empty
// (cert-manager hasn't issued the Certificate yet; the Secret volume is
// optional), it waits for it instead of disabling the webhook for the life of
// the process. The webhook server drains when srvCtx is done.
func startWebhookWhenReady(ctx, srvCtx context.Context, cfg config.Config, eng *recommender.Engine, kc *k8s.Client) {
	start := func() { serveWebhook(srvCtx, cfg, eng, kc) }
	switch {
	case fileExists(cfg.TLSCertFile):
		go start()
	case fileExists(filepath.Dir(cfg.TLSCertFile)):
		log.Printf("webhook: waiting for cert at %s", cfg.TLSCertFile)
		go func() {
			t := time.NewTicker(certPollEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if fileExists(cfg.TLSCertFile) && fileExists(cfg.TLSKeyFile) {
						start()
						return
					}
				}
			}
		}()
	default:
		log.Printf("webhook disabled (no cert at %s)", cfg.TLSCertFile)
	}
}

// serveWebhook runs the webhook HTTPS server until srvCtx is done.
func serveWebhook(srvCtx context.Context, cfg config.Config, eng *recommender.Engine, kc *k8s.Client) {
	// Hot-reload the pair: cert-manager rotates the Secret under the running pod.
	certs, err := webhook.NewCertReloader(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		log.Printf("webhook disabled: cert at %s unusable: %v", cfg.TLSCertFile, err)
		return
	}
	// Same hardened timeouts as the API server (slowloris on :8443 would
	// otherwise pin conns that the kube-apiserver also needs).
	whSrv := server.NewHTTPServer(addr(cfg.TLSPort), webhookMux(webhook.New(cfg, eng, kc).Handle))
	whSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.GetCertificate}
	go func() {
		<-srvCtx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = whSrv.Shutdown(sctx)
	}()
	log.Printf("webhook listening on :%d (TLS)", cfg.TLSPort)
	if err := whSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		log.Printf("webhook server: %v", err)
	}
}

// webhookMux routes the webhook port: POST /mutate and a /healthz.
func webhookMux(mutate http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mutate", mutate)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	return mux
}

// writeAuthBanner describes the write-API posture for the boot log.
func writeAuthBanner(cfg config.Config) string {
	switch {
	case cfg.APIToken != "":
		return "write API: Bearer token required (TAILOR_API_TOKEN)"
	case cfg.APIAuthRequired:
		return "write API: DISABLED — TAILOR_API_AUTH_REQUIRED=true but TAILOR_API_TOKEN is empty"
	default:
		return "WARNING: write API (/api/pause, /api/policies, /api/groups, /api/workload) is UNAUTHENTICATED — set TAILOR_API_TOKEN"
	}
}

// Poll intervals (vars so tests can shorten them).
var (
	chSchemaRetry = 5 * time.Second
	certPollEvery = 10 * time.Second
)

// Shutdown budget: actuatorStopTimeout + lease release (~1 API call) + 5s
// server drain + flushTimeout ≈ 25s, inside the chart's default
// terminationGracePeriodSeconds (30).
const (
	warmLoadTimeout     = 5 * time.Second
	actuatorStopTimeout = 10 * time.Second
	flushTimeout        = 5 * time.Second
)

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func addr(port int) string {
	b := []byte{':'}
	if port == 0 {
		return ":0"
	}
	digits := []byte{}
	for port > 0 {
		digits = append([]byte{byte('0' + port%10)}, digits...)
		port /= 10
	}
	return string(append(b, digits...))
}
