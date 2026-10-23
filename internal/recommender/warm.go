package recommender

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
	"github.com/nicacioliveira/k8s-tailor/internal/policy"
)

// The warm cache. Right after a start the engine has no recommendation until its
// first refresh completes, so pods created in that gap are admitted unmodified
// (tailor_webhook_skipped_total{reason="warming_up"}). The leader therefore writes the
// recommendations the webhook can serve into a ConfigMap on every refresh, and a
// starting replica loads them before it accepts admissions. Loaded entries are for
// the webhook ONLY: they are invisible to Get (the reconciler, the API), so nothing
// is ever resized in place from them, and the first real refresh replaces them.

// WarmConfigMapName is the ConfigMap, in the release namespace, holding the cache.
const WarmConfigMapName = "k8s-tailor-warm"

const (
	warmDataKey  = "warm.json.gz"
	warmVersion  = 1
	warmMaxBytes = 900 * 1024 // ConfigMaps are limited to 1 MiB
)

type warmFile struct {
	Version  int                  `json:"version"`
	SavedAt  int64                `json:"savedAt"`  // unix seconds
	Checksum string               `json:"checksum"` // sha256 (hex) of the JSON of Entries
	Entries  map[string]warmEntry `json:"entries"`
}

// warmEntry is the subset of Entry that PlanInPlace and the webhook read.
type warmEntry struct {
	Mode                  string  `json:"mode"`
	Apply                 string  `json:"apply"`
	Direction             string  `json:"direction"`
	OOMOverridesDirection bool    `json:"oomOverridesDirection,omitempty"`
	EnforceMemLimit       bool    `json:"enforceMemLimit,omitempty"`
	GuaranteedResize      bool    `json:"guaranteedResize,omitempty"`
	Class                 string  `json:"class"`
	Native                bool    `json:"native,omitempty"`
	NotInTemplate         bool    `json:"notInTemplate,omitempty"`
	Confident             bool    `json:"confident,omitempty"`
	CPUConfident          bool    `json:"cpuConfident,omitempty"`
	MemConfident          bool    `json:"memConfident,omitempty"`
	OOMRisk               bool    `json:"oomRisk,omitempty"`
	CPUHeldByHPA          bool    `json:"cpuHeld,omitempty"`
	MemHeldByHPA          bool    `json:"memHeld,omitempty"`
	CPUCensored           bool    `json:"cpuCensored,omitempty"`
	CPUReq                float64 `json:"cpuReq"`
	CPULim                float64 `json:"cpuLim,omitempty"`
	MemReq                float64 `json:"memReq"`
	MemLim                float64 `json:"memLim,omitempty"`
}

func warmOf(en Entry) warmEntry {
	r := en.Rec
	return warmEntry{
		Mode: string(en.Mode), Apply: string(en.Apply), Direction: string(en.Direction),
		OOMOverridesDirection: en.OOMOverridesDirection, EnforceMemLimit: en.EnforceMemLimit, GuaranteedResize: en.GuaranteedResize,
		Class: string(en.Class), Native: en.Native, NotInTemplate: en.NotInTemplate,
		Confident: r.Confident, CPUConfident: r.CPUConfident, MemConfident: r.MemConfident, OOMRisk: r.OOMRisk,
		CPUHeldByHPA: r.CPUHeldByHPA, MemHeldByHPA: r.MemHeldByHPA, CPUCensored: r.Signals.CPUCensored,
		CPUReq: r.Recommended.CPURequest, CPULim: r.Recommended.CPULimit, MemReq: r.Recommended.MemRequest, MemLim: r.Recommended.MemLimit,
	}
}

func (w warmEntry) entry(key string) Entry {
	ns, kind, workload, container := splitKey(key)
	return Entry{
		Rec: Recommendation{
			Namespace: ns, Kind: kind, Workload: workload, Container: container,
			Recommended: Resources{CPURequest: w.CPUReq, CPULimit: w.CPULim, MemRequest: w.MemReq, MemLimit: w.MemLim},
			Confident:   w.Confident, CPUConfident: w.CPUConfident, MemConfident: w.MemConfident, OOMRisk: w.OOMRisk,
			CPUHeldByHPA: w.CPUHeldByHPA, MemHeldByHPA: w.MemHeldByHPA, Signals: Signals{CPUCensored: w.CPUCensored},
			Mode: w.Mode, Apply: w.Apply, Direction: w.Direction,
		},
		Mode: policy.Mode(w.Mode), Apply: policy.Apply(w.Apply), Direction: policy.Direction(w.Direction),
		OOMOverridesDirection: w.OOMOverridesDirection, EnforceMemLimit: w.EnforceMemLimit, GuaranteedResize: w.GuaranteedResize,
		Class: k8s.ContainerClass(w.Class), Native: w.Native, NotInTemplate: w.NotInTemplate,
	}
}

func sumOf(entries map[string]warmEntry) (string, error) {
	b, err := json.Marshal(entries) // map keys are sorted: deterministic
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

// warmPayload serializes the recommendations the webhook may serve: effective mode
// includes Webhook and the recommendation is confident (or an OOM recovery), the
// same gate the webhook applies. It returns the JSON and the entry count.
func (e *Engine) warmPayload() ([]byte, int, error) {
	e.mu.RLock()
	entries := make(map[string]warmEntry)
	for k, en := range e.byKey {
		if en.Mode.Webhook() && en.Class.Resizable() && (en.Rec.Confident || en.Rec.OOMRisk) {
			entries[k] = warmOf(en)
		}
	}
	e.mu.RUnlock()
	sum, err := sumOf(entries)
	if err != nil {
		return nil, 0, err
	}
	b, err := json.Marshal(warmFile{Version: warmVersion, SavedAt: e.clock().Unix(), Checksum: sum, Entries: entries})
	return b, len(entries), err
}

// loadWarmPayload installs a payload as the webhook-only warm entries. It refuses a
// payload of another version, with a bad checksum, or older than WARM_TTL, and does
// nothing once a real refresh has completed.
func (e *Engine) loadWarmPayload(b []byte) (int, error) {
	var f warmFile
	if err := json.Unmarshal(b, &f); err != nil {
		return 0, fmt.Errorf("warm cache: %w", err)
	}
	if f.Version != warmVersion {
		return 0, fmt.Errorf("warm cache: version %d, this engine reads %d", f.Version, warmVersion)
	}
	if sum, err := sumOf(f.Entries); err != nil || sum != f.Checksum {
		return 0, errors.New("warm cache: checksum mismatch")
	}
	until := time.Unix(f.SavedAt, 0).Add(e.cfg.WarmTTL)
	if e.cfg.WarmTTL <= 0 || !e.clock().Before(until) {
		return 0, fmt.Errorf("warm cache: saved at %s, older than WARM_TTL %s", time.Unix(f.SavedAt, 0).UTC().Format(time.RFC3339), e.cfg.WarmTTL)
	}
	loaded := make(map[string]Entry, len(f.Entries))
	for k, w := range f.Entries {
		loaded[k] = w.entry(k)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.warm.Load() {
		return 0, nil // a real refresh already happened
	}
	e.warmEntries, e.warmUntil = loaded, until
	return len(loaded), nil
}

// warmLive reports whether loaded entries are still inside their TTL. Caller holds e.mu.
func (e *Engine) warmLive() bool {
	return len(e.warmEntries) > 0 && e.clock().Before(e.warmUntil)
}

// Ready reports whether the webhook can serve: a refresh completed, or a warm cache
// was loaded and has not expired.
func (e *Engine) Ready() bool {
	if e.warm.Load() {
		return true
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.warmLive()
}

// Admission is Get for the webhook: the live entry, else (until the first refresh,
// and within the TTL) the one loaded from the warm cache.
func (e *Engine) Admission(ns, kind, workload, container string) (Entry, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	k := Key(ns, kind, workload, container)
	if en, ok := e.byKey[k]; ok {
		return en, true
	}
	if !e.warm.Load() && e.warmLive() {
		en, ok := e.warmEntries[k]
		return en, ok
	}
	return Entry{}, false
}

// LoadWarm reads the ConfigMap at start. A missing ConfigMap, WARM_CACHE=false or an
// unknown namespace is not an error; a damaged or expired payload is, and is ignored.
func (e *Engine) LoadWarm(ctx context.Context) (int, error) {
	if !e.cfg.WarmCache || e.cfg.SelfNamespace == "" || e.k8s == nil {
		return 0, nil
	}
	raw, found, err := e.k8s.ReadConfigMapData(ctx, e.cfg.SelfNamespace, WarmConfigMapName, warmDataKey)
	if err != nil || !found {
		return 0, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return 0, fmt.Errorf("warm cache: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(zr, 16<<20))
	if err != nil {
		return 0, fmt.Errorf("warm cache: %w", err)
	}
	n, err := e.loadWarmPayload(b)
	if n > 0 {
		metrics.WarmCacheEntries.Set(float64(n))
	}
	return n, err
}

// SaveWarm writes the cache. Only the leader calls it, only after a real refresh,
// and an unchanged payload is rewritten at most every WARM_TTL/3 (keeping it inside
// the TTL without an etcd write per refresh).
func (e *Engine) SaveWarm(ctx context.Context) error {
	if !e.cfg.WarmCache || e.cfg.SelfNamespace == "" || e.k8s == nil || !e.warm.Load() {
		return nil
	}
	b, n, err := e.warmPayload()
	if err != nil {
		return err
	}
	var f warmFile
	_ = json.Unmarshal(b, &f)
	now := e.clock()
	e.warmMu.Lock()
	defer e.warmMu.Unlock()
	if f.Checksum == e.warmSum && now.Sub(e.warmSavedAt) < e.cfg.WarmTTL/3 {
		return nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	if err := zw.Close(); err != nil {
		return err
	}
	if buf.Len() > warmMaxBytes {
		metrics.WarmCacheWrites.Inc("too_large")
		return fmt.Errorf("warm cache: %d entries compress to %d bytes, over the %d byte ConfigMap budget; not written", n, buf.Len(), warmMaxBytes)
	}
	ann := map[string]string{
		"tailor.k8s-tailor.dev/warm-saved-at": now.UTC().Format(time.RFC3339),
		"tailor.k8s-tailor.dev/warm-entries":  fmt.Sprint(n),
		"tailor.k8s-tailor.dev/warm-version":  fmt.Sprint(warmVersion),
	}
	if err := e.k8s.WriteConfigMapData(ctx, e.cfg.SelfNamespace, WarmConfigMapName, warmDataKey, buf.Bytes(), ann); err != nil {
		metrics.WarmCacheWrites.Inc("error")
		return err
	}
	metrics.WarmCacheWrites.Inc("ok")
	e.warmSum, e.warmSavedAt = f.Checksum, now
	log.Printf("warm cache: wrote %d entries (%d bytes) to %s/%s", n, buf.Len(), e.cfg.SelfNamespace, WarmConfigMapName)
	return nil
}
