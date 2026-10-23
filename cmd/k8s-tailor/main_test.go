package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nicacioliveira/k8s-tailor/internal/advisor"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
	"github.com/nicacioliveira/k8s-tailor/internal/recommender"
)

var tailorGVRs = map[schema.GroupVersionResource]string{
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorpolicies"}: "TailorPolicyList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorconfigs"}:  "TailorConfigList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorprofiles"}: "TailorProfileList",
	{Group: "tailor.k8s-tailor.dev", Version: "v1alpha1", Resource: "tailorgroups"}:   "TailorGroupList",
	{Group: "serving.knative.dev", Version: "v1", Resource: "services"}:               "ServiceList",
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// writeCert writes a self-signed key pair for localhost into dir.
func writeCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var insecureClient = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
}}

func get(url string) int {
	resp, err := insecureClient.Get(url)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

type fakeCluster struct {
	cfg    config.Config
	cs     *fake.Clientset
	kc     *k8s.Client
	pol    *policy.Store
	chHits atomic.Int32
}

// newFakeCluster: a Deployment and its pod in client-go fakes, a VictoriaMetrics
// stub answering every query empty, and a ClickHouse stub that fails the first
// two schema attempts (the engine must retry, then attach the sink).
func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	fc := &fakeCluster{}
	vmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	t.Cleanup(vmSrv.Close)
	chSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fc.chHits.Add(1) <= 2 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(chSrv.Close)

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}}}
	fc.cs = fake.NewSimpleClientset(dep)
	fc.kc = k8s.NewFromClients(fc.cs, dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs), nil)
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir)
	fc.cfg = config.Config{
		Source: "in-cluster", VMURL: vmSrv.URL, Port: freePort(t), TLSPort: freePort(t),
		CHURL: chSrv.URL, CHDatabase: "tailor", CHRetentionDays: 1,
		Window: time.Hour, CPUQuant: 0.9, MemQuant: 0.95, Margin: 0.15, HalfLife: time.Hour,
		RefreshEvery: 200 * time.Millisecond, ReconcileEvery: 200 * time.Millisecond,
		TrendWindow: time.Hour, TrendStepSec: 60, MinSamples: 1, PricingMode: "static",
		OOMGrowthFactor: 1.5, OOMMemCapFactor: 4, ExcludeNamespaces: map[string]bool{},
		LeaderElect: true, LeaseName: "k8s-tailor-actuator", LeaseNS: "k8s-tailor", LeaseIdentity: "replica-a",
		TLSCertFile: certFile, TLSKeyFile: keyFile,
	}
	fc.pol = policy.NewStoreWithClient(dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), tailorGVRs), fc.cfg)
	return fc
}

func noAdvisor() (advisor.Config, error) { return advisor.DefaultConfig(), nil }

// The full wiring runs against fakes: the API and the TLS webhook serve, the
// refresh loop sizes the fake cluster, the replica takes the Lease, ClickHouse
// is retried until it answers. On cancel, run returns only after the Lease was
// released and the servers stopped (ordered shutdown).
func TestRunServesAndShutsDownInOrder(t *testing.T) {
	old := chSchemaRetry
	chSchemaRetry = 20 * time.Millisecond
	t.Cleanup(func() { chSchemaRetry = old })

	fc := newFakeCluster(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, fc.cfg, fc.kc, fc.pol, noAdvisor) }()

	api := fmt.Sprintf("http://127.0.0.1:%d", fc.cfg.Port)
	waitFor(t, "API /healthz", 10*time.Second, func() bool { return get(api+"/healthz") == 200 })
	waitFor(t, "webhook /healthz over TLS", 10*time.Second, func() bool {
		return get(fmt.Sprintf("https://127.0.0.1:%d/healthz", fc.cfg.TLSPort)) == 200
	})
	lease := func() string {
		l, err := fc.cs.CoordinationV1().Leases("k8s-tailor").Get(context.Background(), "k8s-tailor-actuator", metav1.GetOptions{})
		if err != nil || l.Spec.HolderIdentity == nil {
			return ""
		}
		return *l.Spec.HolderIdentity
	}
	waitFor(t, "lease acquired", 10*time.Second, func() bool { return lease() == "replica-a" })
	waitFor(t, "ClickHouse schema retried until ready", 10*time.Second, func() bool { return fc.chHits.Load() >= 3 })
	// The write API is unauthenticated here (no token): a POST reaches the
	// handler, and the per-container write sees the refreshed snapshot.
	waitFor(t, "per-container write accepted", 10*time.Second, func() bool {
		req, _ := http.NewRequest("POST", api+"/api/workload",
			strings.NewReader(`{"namespace":"apps","kind":"Deployment","workload":"web","container":"app","mode":"Recommend"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := insecureClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if h := lease(); h != "" {
		t.Fatalf("lease still held by %q after shutdown", h)
	}
	if code := get(api + "/healthz"); code != 0 {
		t.Fatalf("API still serving after run returned: %d", code)
	}
}

func TestRunFailsOnAdvisorConfig(t *testing.T) {
	fc := newFakeCluster(t)
	err := run(context.Background(), fc.cfg, fc.kc, fc.pol, func() (advisor.Config, error) {
		return advisor.Config{}, errors.New("ADVISOR_MODEL required")
	})
	if err == nil || !strings.Contains(err.Error(), "advisor config") {
		t.Fatalf("err = %v", err)
	}
}

// A port already in use is a fatal server error (not a silent exit 0).
func TestRunFailsWhenPortBusy(t *testing.T) {
	fc := newFakeCluster(t)
	fc.cfg.LeaderElect = false
	fc.cfg.TLSCertFile = filepath.Join(t.TempDir(), "missing", "tls.crt") // webhook disabled
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", fc.cfg.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := run(ctx, fc.cfg, fc.kc, fc.pol, noAdvisor); err == nil || !strings.Contains(err.Error(), "server:") {
		t.Fatalf("err = %v, want a server error", err)
	}
}

// The servers are stopped only once the actuator finished (Lease released), or
// after maxWait when it hangs — never on ctx alone.
func TestStopServersAfterActuator(t *testing.T) {
	t.Run("waits for the actuator", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		actuatorDone := make(chan struct{})
		var stopped atomic.Bool
		finished := make(chan struct{})
		go func() {
			stopServersAfterActuator(ctx, actuatorDone, time.Minute, func() { stopped.Store(true) })
			close(finished)
		}()
		cancel()
		time.Sleep(100 * time.Millisecond)
		if stopped.Load() {
			t.Fatal("servers stopped while the actuator was still running")
		}
		close(actuatorDone)
		<-finished
		if !stopped.Load() {
			t.Fatal("servers not stopped after the actuator finished")
		}
	})
	t.Run("bounded by maxWait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		var stopped atomic.Bool
		stopServersAfterActuator(ctx, make(chan struct{}), 50*time.Millisecond, func() { stopped.Store(true) })
		if !stopped.Load() || time.Since(start) < 50*time.Millisecond {
			t.Fatalf("stopped=%v after %v", stopped.Load(), time.Since(start))
		}
	})
}

type fakeRefresher struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (f *fakeRefresher) Refresh(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("refresh without a timeout")
	}
	if f.fail {
		return errors.New("vm down")
	}
	return nil
}
func (f *fakeRefresher) RefreshTimeout() time.Duration      { return time.Second }
func (f *fakeRefresher) List() []recommender.Recommendation { return nil }
func (f *fakeRefresher) n() int                             { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// refreshLoop refreshes at once, then on every tick, each pass with its own
// deadline; a failing pass never stops the loop; ctx ends it.
func TestRefreshLoop(t *testing.T) {
	f := &fakeRefresher{fail: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { refreshLoop(ctx, 10*time.Millisecond, f, nil); close(done) }()
	waitFor(t, "3 refreshes despite errors", 5*time.Second, func() bool { return f.n() >= 3 })
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	n := f.n()
	waitFor(t, "refresh after recovery", 5*time.Second, func() bool { return f.n() > n })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refreshLoop ignored ctx")
	}
}

func TestRetryUntil(t *testing.T) {
	var attempts []int
	okCalled := false
	n := 0
	retryUntil(context.Background(), time.Millisecond, func() error {
		n++
		if n < 3 {
			return errors.New("not ready")
		}
		return nil
	}, func(a int, _ error) { attempts = append(attempts, a) }, func() { okCalled = true })
	if !okCalled || n != 3 || fmt.Sprint(attempts) != "[1 2]" {
		t.Fatalf("ok=%v tries=%d attempts=%v", okCalled, n, attempts)
	}
	// ctx done: gives up without calling ok.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	okCalled = false
	retryUntil(ctx, time.Hour, func() error { return errors.New("down") }, func(int, error) {}, func() { okCalled = true })
	if okCalled {
		t.Fatal("ok called although every attempt failed")
	}
}

func TestWebhookMux(t *testing.T) {
	var hits atomic.Int32
	mux := webhookMux(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) })
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/mutate", 200},
		{"GET", "/mutate", http.StatusMethodNotAllowed},
		{"GET", "/healthz", 200},
		{"GET", "/api/recommendations", 404}, // the API is not on the webhook port
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, nil))
		if rr.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rr.Code, c.want)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("mutate handler hits = %d, want 1", hits.Load())
	}
}

func TestWriteAuthBanner(t *testing.T) {
	for _, c := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{APIToken: "x"}, "Bearer token required"},
		{config.Config{APIToken: "x", APIAuthRequired: true}, "Bearer token required"},
		{config.Config{APIAuthRequired: true}, "DISABLED"},
		{config.Config{}, "UNAUTHENTICATED"},
	} {
		if got := writeAuthBanner(c.cfg); !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestAddrAndFileExists(t *testing.T) {
	for port, want := range map[int]string{0: ":0", 8: ":8", 8080: ":8080", 65535: ":65535"} {
		if got := addr(port); got != want {
			t.Errorf("addr(%d) = %q, want %q", port, got, want)
		}
	}
	dir := t.TempDir()
	if !fileExists(dir) || fileExists(filepath.Join(dir, "nope")) {
		t.Fatal("fileExists")
	}
}

// The cert directory is mounted but cert-manager has not issued the pair yet:
// the webhook waits for it and starts once both files exist (instead of
// staying disabled for the life of the process).
func TestWebhookWaitsForCert(t *testing.T) {
	old := certPollEvery
	certPollEvery = 20 * time.Millisecond
	t.Cleanup(func() { certPollEvery = old })

	fc := newFakeCluster(t)
	dir := t.TempDir()
	cfg := fc.cfg
	cfg.TLSCertFile, cfg.TLSKeyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvCtx, stopSrv := context.WithCancel(context.Background())
	defer stopSrv()
	startWebhookWhenReady(ctx, srvCtx, cfg, nil, fc.kc)
	url := fmt.Sprintf("https://127.0.0.1:%d/healthz", cfg.TLSPort)
	time.Sleep(100 * time.Millisecond)
	if code := get(url); code != 0 {
		t.Fatalf("webhook serving before the cert exists: %d", code)
	}
	writeCert(t, dir)
	waitFor(t, "webhook after the cert appeared", 10*time.Second, func() bool { return get(url) == 200 })
	stopSrv()
	waitFor(t, "webhook drained on srvCtx", 10*time.Second, func() bool { return get(url) == 0 })
}

// An unusable pair (garbage PEM) disables the webhook instead of crashing.
func TestWebhookBadCertDisables(t *testing.T) {
	fc := newFakeCluster(t)
	dir := t.TempDir()
	cfg := fc.cfg
	cfg.TLSCertFile, cfg.TLSKeyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for _, f := range []string{cfg.TLSCertFile, cfg.TLSKeyFile} {
		if err := os.WriteFile(f, []byte("not a pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	serveWebhook(context.Background(), cfg, nil, fc.kc) // returns at once
	if code := get(fmt.Sprintf("https://127.0.0.1:%d/healthz", cfg.TLSPort)); code != 0 {
		t.Fatalf("webhook serving with an unusable cert: %d", code)
	}
}

type fakeWarm struct {
	mu          sync.Mutex
	loaded      int
	saves       int
	loadErr     error
	saveErr     error
	loadedCalls int
}

func (f *fakeWarm) LoadWarm(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadedCalls++
	return f.loaded, f.loadErr
}
func (f *fakeWarm) SaveWarm(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	return f.saveErr
}
func (f *fakeWarm) saved() int { f.mu.Lock(); defer f.mu.Unlock(); return f.saves }

// Only the leader writes the warm cache, and only after a refresh that succeeded.
func TestWarmCacheWriterOnlyForTheLeader(t *testing.T) {
	f := &fakeWarm{}
	hook := warmCacheWriter(f)
	metrics.Leader.Set(0)
	hook(context.Background())
	if f.saved() != 0 {
		t.Fatal("a follower wrote the warm cache")
	}
	metrics.Leader.Set(1)
	t.Cleanup(func() { metrics.Leader.Set(0) })
	hook(context.Background())
	if f.saved() != 1 {
		t.Fatalf("the leader did not write (%d)", f.saved())
	}
	f.saveErr = errors.New("forbidden")
	hook(context.Background()) // logged once, never fatal
	hook(context.Background())
	if f.saved() != 3 {
		t.Fatalf("a failing write stopped being retried (%d)", f.saved())
	}

	r := &fakeRefresher{fail: true}
	g := &fakeWarm{}
	metrics.Leader.Set(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { refreshLoop(ctx, 10*time.Millisecond, r, warmCacheWriter(g)); close(done) }()
	waitFor(t, "failing refreshes", 5*time.Second, func() bool { return r.n() >= 3 })
	if g.saved() != 0 {
		t.Fatalf("the cache was written after a FAILED refresh (%d)", g.saved())
	}
	r.mu.Lock()
	r.fail = false
	r.mu.Unlock()
	waitFor(t, "a write after a successful refresh", 5*time.Second, func() bool { return g.saved() > 0 })
	cancel()
	<-done
}

func TestLoadWarmCacheNeverBlocksTheBoot(t *testing.T) {
	loadWarmCache(context.Background(), &fakeWarm{loaded: 3})
	loadWarmCache(context.Background(), &fakeWarm{loadErr: errors.New("configmaps is forbidden")})
	loadWarmCache(context.Background(), &fakeWarm{})
}
