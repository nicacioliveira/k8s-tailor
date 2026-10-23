// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Source string // "local" | "in-cluster"
	VMURL  string
	// VMJob pins every usage query (container_cpu_usage_seconds_total /
	// container_memory_working_set_bytes) to job="<VMJob>": series with the same
	// name and labels coming from any OTHER scrape job are ignored. Default is the
	// job the chart's bundled vmsingle scrapes the kubelet with; "" (VM_JOB set but
	// empty) disables the filter for a Prometheus with other job names.
	VMJob string
	// VMThrottleJob pins the OPTIONAL CFS throttling query
	// (container_cpu_cfs_throttled_periods_total / container_cpu_cfs_periods_total,
	// cAdvisor /metrics/cadvisor) to job="<VMThrottleJob>". Those series come from
	// a different scrape job than the kubelet resource metrics (VMJob). Default is
	// the cAdvisor job of the chart's bundled vmsingle; "" (VM_THROTTLE_JOB set but
	// empty) disables the filter.
	VMThrottleJob string
	Port          int // plain HTTP: API + UI
	TLSPort       int // HTTPS: mutating webhook

	// ClickHouse: durable store for applied decisions (and future telemetry/ML
	// features). Empty CHURL = in-memory ring buffer only.
	CHURL           string
	CHDatabase      string
	CHUser          string
	CHPassword      string
	CHRetentionDays int

	Window   time.Duration
	CPUQuant float64
	MemQuant float64
	// CPURateMode: "avg" (default) uses rate() — smooths sub-window bursts; "max"
	// uses VictoriaMetrics rollup_rate(...,"max") to capture micro-bursts (spike-
	// accurate, sizes higher). "max" requires a VictoriaMetrics datasource.
	CPURateMode    string
	Margin         float64
	HalfLife       time.Duration
	RefreshEvery   time.Duration
	ReconcileEvery time.Duration

	// Detail charts use a shorter, finer-grained window than the recommendation
	// math so movement is visible over minutes, not just the multi-day quantile window.
	TrendWindow  time.Duration
	TrendStepSec int

	MemLimitFactor float64
	CPULimitFactor float64

	// Sane floors so a recommendation never starves a container.
	MinCPUCores float64
	MinMemBytes float64

	// Don't actuate on thin data — require at least this many samples first.
	MinSamples int

	// OOM-learned memory floor. When a container is OOMKilled, its memory LIMIT
	// (the value that proved insufficient) is re-protected DURABLY at
	// (observedLimit × OOMGrowthFactor): a later Down/trim can never crush the
	// limit back below the value that killed it, so OOM-prone workloads protect
	// themselves instead of needing manual per-workload exclusion. The floor is
	// forgotten OOMFloorTTL after the last OOM, so a fixed workload resumes normal
	// sizing. Held in-engine across refreshes (survives pod replacement); an engine
	// restart re-seeds it from any live OOM.
	OOMLearn        bool
	OOMGrowthFactor float64
	OOMFloorTTL     time.Duration
	// OOMMemCapFactor (OOM_MEM_CAP_FACTOR, default 4) bounds the OOM-driven
	// REQUEST raise of a container WITHOUT a memory limit: never above
	// observedPeak × OOMMemCapFactor (and never above the policy memory.max). The
	// floor of an unbounded container is learned from its request at the kill, so
	// repeated node-pressure kills would otherwise ratchet it ×growth per kill
	// toward a request no node can schedule. Must be finite and >=
	// OOMGrowthFactor (see Validate).
	OOMMemCapFactor float64
	// ClusterLookups (CLUSTER_LOOKUPS=true, chart tailor.rbac.clusterLookups) lets
	// the engine list nodes and LimitRanges so a memory request never exceeds the
	// largest eligible node's allocatable memory or the namespace LimitRange max.
	// Off by default: the engine role then has no access to either.
	ClusterLookups bool

	// Censored CPU (usage pinned at the CPU limit, or CFS throttling when the
	// cAdvisor series exist): the measured usage is a lower bound of the demand,
	// so the CPU LIMIT is raised by CPULimitGrowth per step (1 = never raise it)
	// and the request lifted to at least the live limit. The raised limit never
	// exceeds the policy cpu.max, or, when the policy sets none, the template
	// limit × CPUCensorMaxFactor. See docs/TECHNICAL.md ("Censored CPU").
	CPULimitGrowth     float64
	CPUCensorMaxFactor float64
	// CPUCensorMaxSteps (CPU_CENSOR_MAX_STEPS, default 3): consecutive limit raises
	// after which a container whose usage keeps pinning at the new limit is parked
	// as CPU-greedy instead of being raised again.
	CPUCensorMaxSteps int

	// SeasonWindow (SEASON_WINDOW, default 14d; 0 = off) is the history fetched for
	// the seasonality gate and forecast of weekly and both policies when WINDOW is
	// shorter: a week-lag correlation needs more than 7 days (14 cover every hour
	// of the week). Only those namespaces pay for it, once per SeasonRefresh
	// (SEASON_REFRESH, default 1h), and at most MaxSeasonWindow. The recommendation's
	// quantile, peak and change-point still use WINDOW.
	SeasonWindow  time.Duration
	SeasonRefresh time.Duration

	// WarmCache (WARM_CACHE, default true) lets the leader write the recommendations
	// the webhook can serve into the ConfigMap k8s-tailor-warm (release namespace) on
	// every refresh, and a starting replica load them, so pods created right after a
	// start are sized instead of admitted unmodified. Entries older than WarmTTL
	// (WARM_TTL, default 15m; 0 = never load) are ignored. Loaded entries serve the
	// webhook only and never drive an in-place resize.
	WarmCache bool
	WarmTTL   time.Duration

	// AutoQuantile (AUTO_QUANTILE, default false) and BurstThreshold
	// (BURST_THRESHOLD, default 3): the engine-wide default of the policy fields
	// autoQuantile / burstThreshold (a policy overrides them). With AutoQuantile the
	// CPU quantile is raised to 0.95 for a container whose burstiness index (p99 / p90)
	// reaches the threshold.
	AutoQuantile   bool
	BurstThreshold float64

	// DisruptiveMaxWait (DISRUPTIVE_MAX_WAIT, default 15m; 0 = wait forever) bounds
	// how long a permanently NotReady sibling holds the disruptive rolling (resize
	// with resizePolicy RestartContainer). Once it expires the rolling proceeds
	// only if the live PDB and replica reads allow a restart.
	DisruptiveMaxWait time.Duration

	// Pricing: "static" | "opencost" (read node_*_hourly_cost from VM) | "aws" (stub).
	PricingMode    string
	PriceCPUCoreHr float64
	PriceGBHr      float64

	ExcludeNamespaces map[string]bool
	// SelfNamespace is the namespace the engine runs in (POD_NAMESPACE). It is
	// ALWAYS excluded — on top of EXCLUDE_NAMESPACES and of the TailorConfig CR
	// list — so the Tailor never resizes itself, its ClickHouse or its vmsingle,
	// whatever namespace the chart was installed into. Empty outside a pod.
	SelfNamespace string

	// API auth for the MUTATING endpoints (POST/PUT/PATCH/DELETE under /api/):
	// when APIToken is set they require `Authorization: Bearer <APIToken>`.
	// APIAuthRequired fails closed (503) when no token is configured. Reads stay
	// open unless APIProtectReads (TAILOR_API_PROTECT_READS, see auth.go). Empty
	// token + not required = open (backwards compatible).
	APIToken        string
	APIAuthRequired bool
	APIProtectReads bool

	// Leader election (coordination.k8s.io Lease) for the ACTUATING reconciler,
	// so the engine can run >1 replica: every replica recommends and serves the
	// API/UI/webhook; only the Lease holder resizes. Off = this process always
	// actuates (single replica / local dev).
	LeaderElect   bool
	LeaseName     string
	LeaseNS       string
	LeaseIdentity string

	// TLS material for the webhook server.
	TLSCertFile string
	TLSKeyFile  string
}

func str(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// DefaultVMJob is the job_name of the kubelet resource-metrics scrape in the
// chart's bundled vmsingle (values.yaml victoria-metrics-single.server.scrape).
const DefaultVMJob = "kubelet-resource"

// DefaultVMThrottleJob is the job_name of the cAdvisor CFS-counter scrape in the
// chart's bundled vmsingle (values.yaml victoria-metrics-single.server.scrape).
const DefaultVMThrottleJob = "kubelet-cadvisor"

// strOrEmpty is str, except that a variable SET to "" yields "" (an explicit
// opt-out) instead of the default.
func strOrEmpty(k, d string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return d
}

func num(k string, d float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return d
}

// strictNum is num, except that a value that does not parse yields NaN instead
// of the default, so Validate rejects it rather than silently ignoring a typo.
func strictNum(k string, d float64) float64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// strictInt is strictNum for an integer: an unparsable or fractional value
// becomes 0, which Validate rejects.
func strictInt(k string, d int) int {
	f := strictNum(k, float64(d))
	if math.IsNaN(f) || f != math.Trunc(f) || f > math.MaxInt32 || f < math.MinInt32 {
		return 0
	}
	return int(f)
}

func dur(k, d string) time.Duration {
	v := str(k, d)
	pd, err := ParseDuration(v)
	if err != nil {
		panic("invalid duration " + k + "=" + v)
	}
	return pd
}

func set(k, d string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(str(k, d), ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			out[s] = true
		}
	}
	return out
}

// ParseDuration extends time.ParseDuration with "d" (days) and "w" (weeks).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n := len(s); n > 1 {
		unit := s[n-1]
		if unit == 'd' || unit == 'w' {
			val, err := strconv.ParseFloat(s[:n-1], 64)
			if err != nil {
				return 0, err
			}
			mult := 24 * time.Hour
			if unit == 'w' {
				mult = 7 * 24 * time.Hour
			}
			return time.Duration(val * float64(mult)), nil
		}
	}
	return time.ParseDuration(s)
}

func Load() Config {
	c := Config{
		Source:        str("SOURCE", "in-cluster"),
		VMURL:         strings.TrimRight(str("VM_URL", "http://127.0.0.1:8428"), "/"),
		VMJob:         strOrEmpty("VM_JOB", DefaultVMJob),
		VMThrottleJob: strOrEmpty("VM_THROTTLE_JOB", DefaultVMThrottleJob),
		Port:          int(num("PORT", 8080)),
		TLSPort:       int(num("TLS_PORT", 8443)),

		CHURL:           strings.TrimRight(str("CH_URL", ""), "/"),
		CHDatabase:      str("CH_DATABASE", "tailor"),
		CHUser:          str("CH_USER", ""),
		CHPassword:      str("CH_PASSWORD", ""),
		CHRetentionDays: int(num("CH_RETENTION_DAYS", 90)),

		Window:         dur("WINDOW", "7d"),
		CPUQuant:       num("CPU_QUANTILE", 0.90),
		MemQuant:       num("MEM_QUANTILE", 0.95),
		CPURateMode:    str("CPU_RATE_MODE", "avg"),
		Margin:         num("MARGIN", 0.15),
		HalfLife:       dur("HALF_LIFE", "24h"),
		RefreshEvery:   dur("REFRESH_EVERY", "60s"),
		ReconcileEvery: dur("RECONCILE_EVERY", "30s"),

		TrendWindow:  dur("TREND_WINDOW", "3h"),
		TrendStepSec: int(num("TREND_STEP_SEC", 60)),

		MemLimitFactor: num("MEM_LIMIT_FACTOR", 1.0),
		CPULimitFactor: num("CPU_LIMIT_FACTOR", 0),

		MinCPUCores: num("MIN_CPU_MILLI", 10) / 1000,
		MinMemBytes: num("MIN_MEM_MI", 32) * 1024 * 1024,
		MinSamples:  int(num("MIN_SAMPLES", 30)),

		OOMLearn:        str("OOM_LEARN", "true") != "false",
		OOMGrowthFactor: num("OOM_GROWTH_FACTOR", 1.5),
		OOMFloorTTL:     dur("OOM_FLOOR_TTL", "30d"),
		OOMMemCapFactor: strictNum("OOM_MEM_CAP_FACTOR", DefaultOOMMemCapFactor),
		ClusterLookups:  str("CLUSTER_LOOKUPS", "false") == "true",

		CPULimitGrowth:     strictNum("CPU_LIMIT_GROWTH_FACTOR", DefaultCPULimitGrowth),
		CPUCensorMaxFactor: strictNum("CPU_CENSOR_MAX_FACTOR", DefaultCPUCensorMaxFactor),
		CPUCensorMaxSteps:  strictInt("CPU_CENSOR_MAX_STEPS", DefaultCPUCensorMaxSteps),
		SeasonWindow:       dur("SEASON_WINDOW", "14d"),
		SeasonRefresh:      dur("SEASON_REFRESH", "1h"),
		WarmCache:          str("WARM_CACHE", "true") != "false",
		WarmTTL:            dur("WARM_TTL", "15m"),
		AutoQuantile:       str("AUTO_QUANTILE", "false") == "true",
		BurstThreshold:     strictNum("BURST_THRESHOLD", DefaultBurstThreshold),

		DisruptiveMaxWait: dur("DISRUPTIVE_MAX_WAIT", "15m"),

		PricingMode:    str("PRICING_MODE", "static"),
		PriceCPUCoreHr: num("PRICE_CPU_CORE_HR", 0.031),
		PriceGBHr:      num("PRICE_GB_HR", 0.004),

		ExcludeNamespaces: set("EXCLUDE_NAMESPACES", DefaultExcludeNamespaces),
		SelfNamespace:     strings.TrimSpace(os.Getenv("POD_NAMESPACE")),

		APIToken:        os.Getenv("TAILOR_API_TOKEN"),
		APIAuthRequired: str("TAILOR_API_AUTH_REQUIRED", "false") == "true",
		APIProtectReads: apiProtectReads(),

		LeaderElect:   str("LEADER_ELECT", "false") == "true",
		LeaseName:     str("LEADER_LEASE_NAME", "k8s-tailor-actuator"),
		LeaseNS:       str("POD_NAMESPACE", "k8s-tailor"),
		LeaseIdentity: str("POD_NAME", hostname()),

		TLSCertFile: str("TLS_CERT_FILE", "/etc/webhook/tls/tls.crt"),
		TLSKeyFile:  str("TLS_KEY_FILE", "/etc/webhook/tls/tls.key"),
	}
	if c.SelfNamespace != "" {
		c.ExcludeNamespaces[c.SelfNamespace] = true
	}
	return c
}

// DefaultOOMMemCapFactor is the default OOM_MEM_CAP_FACTOR.
const DefaultOOMMemCapFactor = 4.0

// Defaults of CPU_LIMIT_GROWTH_FACTOR and CPU_CENSOR_MAX_FACTOR.
const (
	DefaultCPULimitGrowth     = 1.5
	DefaultCPUCensorMaxFactor = 8.0
	DefaultCPUCensorMaxSteps  = 3
	DefaultBurstThreshold     = 3.0
	// MaxSeasonWindow bounds SEASON_WINDOW: the cost of the extra query grows
	// linearly with it (28 days at the 300 s step is 8064 points per series).
	MaxSeasonWindow = 28 * 24 * time.Hour
)

// Validate rejects settings that would make the engine unsafe or meaningless.
// main aborts the boot on an error (fail fast instead of running with a value
// the operator did not intend).
func (c Config) Validate() error {
	if c.DisruptiveMaxWait < 0 {
		return fmt.Errorf("DISRUPTIVE_MAX_WAIT must be >= 0 (0 = wait forever), got %s", c.DisruptiveMaxWait)
	}
	f := c.OOMMemCapFactor
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return fmt.Errorf("OOM_MEM_CAP_FACTOR must be a finite number > 0, got %v", f)
	}
	if f < c.OOMGrowthFactor {
		return fmt.Errorf("OOM_MEM_CAP_FACTOR (%v) must be >= OOM_GROWTH_FACTOR (%v): a lower cap would undo the OOM raise itself", f, c.OOMGrowthFactor)
	}
	if g := c.CPULimitGrowth; math.IsNaN(g) || math.IsInf(g, 0) || g < 1 {
		return fmt.Errorf("CPU_LIMIT_GROWTH_FACTOR must be a finite number >= 1 (1 disables the censored-CPU limit raise), got %v", g)
	}
	if c.SeasonWindow < 0 || c.SeasonWindow > MaxSeasonWindow {
		return fmt.Errorf("SEASON_WINDOW must be between 0 (off) and %s, got %s", MaxSeasonWindow, c.SeasonWindow)
	}
	if c.WarmTTL < 0 {
		return fmt.Errorf("WARM_TTL must be >= 0 (0 = never load the warm cache), got %s", c.WarmTTL)
	}
	if c.SeasonRefresh < 0 {
		return fmt.Errorf("SEASON_REFRESH must be >= 0, got %s", c.SeasonRefresh)
	}
	if b := c.BurstThreshold; math.IsNaN(b) || math.IsInf(b, 0) || b <= 1 {
		return fmt.Errorf("BURST_THRESHOLD must be a finite number > 1, got %v", b)
	}
	if c.CPUCensorMaxSteps < 1 {
		return fmt.Errorf("CPU_CENSOR_MAX_STEPS must be an integer >= 1, got %d", c.CPUCensorMaxSteps)
	}
	if m := c.CPUCensorMaxFactor; math.IsNaN(m) || math.IsInf(m, 0) || m < 1 {
		return fmt.Errorf("CPU_CENSOR_MAX_FACTOR must be a finite number >= 1, got %v", m)
	}
	return nil
}

// DefaultExcludeNamespaces is the out-of-chart fallback (local runs). The chart
// renders its own list (tailor.excludeNamespaces + the release namespace).
const DefaultExcludeNamespaces = "kube-system,kube-public,kube-node-lease,k8s-tailor," +
	"istio-system,istio-ingress,istio-gateway,linkerd,linkerd-viz,cert-manager," +
	"knative-serving,knative-eventing,kourier-system,gatekeeper-system,kyverno"

const HoursPerWeek = 168.0

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "k8s-tailor"
	}
	return h
}
