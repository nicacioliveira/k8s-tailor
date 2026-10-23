package config

import (
	"os"
	"strings"
	"testing"
)

// The engine's own namespace is always excluded, even when EXCLUDE_NAMESPACES
// (or a chart rendered into another namespace) forgets it.
func TestLoadAlwaysExcludesSelfNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "other")
	t.Setenv("EXCLUDE_NAMESPACES", "kube-system")
	c := Load()
	if !c.ExcludeNamespaces["other"] || !c.ExcludeNamespaces["kube-system"] {
		t.Fatalf("exclude = %v, want other + kube-system", c.ExcludeNamespaces)
	}
	if c.SelfNamespace != "other" {
		t.Fatalf("SelfNamespace = %q", c.SelfNamespace)
	}
}

func TestDefaultExcludesMeshAndSystem(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("EXCLUDE_NAMESPACES", "")
	c := Load()
	for _, ns := range []string{"kube-system", "istio-system", "linkerd", "cert-manager", "k8s-tailor"} {
		if !c.ExcludeNamespaces[ns] {
			t.Errorf("default exclude list misses %s", ns)
		}
	}
}

// VM_JOB pins the usage queries to the scrape job the chart configures; set but
// EMPTY must disable the filter (own Prometheus with other job names) instead of
// falling back to the default.
func TestVMJob(t *testing.T) {
	t.Setenv("VM_JOB", "x")
	if got := Load().VMJob; got != "x" {
		t.Fatalf("VMJob = %q, want x", got)
	}
	t.Setenv("VM_JOB", "")
	if got := Load().VMJob; got != "" {
		t.Fatalf("VM_JOB=\"\" must disable the filter, got %q", got)
	}
}

func TestVMJobDefault(t *testing.T) {
	// t.Setenv registers the restore; Unsetenv then simulates "not set".
	t.Setenv("VM_JOB", "")
	os.Unsetenv("VM_JOB")
	if got := Load().VMJob; got != "kubelet-resource" {
		t.Fatalf("default VMJob = %q, want kubelet-resource", got)
	}
}

// VM_THROTTLE_JOB pins the optional CFS throttling query to the cAdvisor scrape
// job (default kubelet-cadvisor); set but EMPTY disables the filter.
func TestVMThrottleJob(t *testing.T) {
	t.Setenv("VM_THROTTLE_JOB", "")
	os.Unsetenv("VM_THROTTLE_JOB")
	if got := Load().VMThrottleJob; got != "kubelet-cadvisor" {
		t.Fatalf("default VMThrottleJob = %q, want kubelet-cadvisor", got)
	}
	t.Setenv("VM_THROTTLE_JOB", "cadvisor")
	if got := Load().VMThrottleJob; got != "cadvisor" {
		t.Fatalf("VMThrottleJob = %q, want cadvisor", got)
	}
	t.Setenv("VM_THROTTLE_JOB", "")
	if got := Load().VMThrottleJob; got != "" {
		t.Fatalf("VM_THROTTLE_JOB=\"\" must disable the filter, got %q", got)
	}
}

// OOM_MEM_CAP_FACTOR bounds the OOM-driven request raise of a container without
// a memory limit (observed peak × factor). Default 4; any value must be finite
// and >= OOM_GROWTH_FACTOR (a cap below the growth factor would cut the raise
// the OOM rule itself asks for).
func TestOOMMemCapFactor(t *testing.T) {
	t.Setenv("OOM_GROWTH_FACTOR", "")
	t.Setenv("OOM_MEM_CAP_FACTOR", "")
	c := Load()
	if c.OOMMemCapFactor != DefaultOOMMemCapFactor || DefaultOOMMemCapFactor != 4 {
		t.Fatalf("default OOMMemCapFactor = %v, want 4", c.OOMMemCapFactor)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	cases := []struct {
		growth, capf string
		ok           bool
	}{
		{"1.5", "8", true},
		{"1.5", "1.5", true}, // equal to the growth factor: allowed
		{"2", "1.5", false},  // below the growth factor
		{"1.5", "0", false},
		{"1.5", "-3", false},
		{"1.5", "NaN", false},
		{"1.5", "+Inf", false},
		{"1.5", "four", false}, // unparsable must not silently fall back to the default
	}
	for _, tc := range cases {
		t.Setenv("OOM_GROWTH_FACTOR", tc.growth)
		t.Setenv("OOM_MEM_CAP_FACTOR", tc.capf)
		err := Load().Validate()
		if (err == nil) != tc.ok {
			t.Errorf("growth=%s cap=%s: Validate() = %v, want ok=%v", tc.growth, tc.capf, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "OOM_MEM_CAP_FACTOR") {
			t.Errorf("error must name the variable: %v", err)
		}
	}
	t.Setenv("OOM_GROWTH_FACTOR", "1.5")
	t.Setenv("OOM_MEM_CAP_FACTOR", "6")
	if got := Load().OOMMemCapFactor; got != 6 {
		t.Fatalf("OOMMemCapFactor = %v, want 6", got)
	}
}

// CPU_LIMIT_GROWTH_FACTOR / CPU_CENSOR_MAX_FACTOR (censored CPU): defaults 1.5
// and 8; finite and >= 1 (growth 1 disables the limit raise); an unparsable
// value is an error.
func TestCPUCensorFactors(t *testing.T) {
	t.Setenv("CPU_LIMIT_GROWTH_FACTOR", "")
	t.Setenv("CPU_CENSOR_MAX_FACTOR", "")
	c := Load()
	if c.CPULimitGrowth != 1.5 || c.CPUCensorMaxFactor != 8 {
		t.Fatalf("defaults = %v, %v; want 1.5, 8", c.CPULimitGrowth, c.CPUCensorMaxFactor)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	cases := []struct {
		growth, maxf string
		ok           bool
	}{
		{"1", "1", true},
		{"2", "8", true},
		{"0.9", "4", false},
		{"1.5", "0.5", false},
		{"NaN", "4", false},
		{"1.5", "+Inf", false},
		{"fast", "4", false},
	}
	for _, tc := range cases {
		t.Setenv("CPU_LIMIT_GROWTH_FACTOR", tc.growth)
		t.Setenv("CPU_CENSOR_MAX_FACTOR", tc.maxf)
		if err := Load().Validate(); (err == nil) != tc.ok {
			t.Errorf("growth=%s max=%s: Validate() = %v, want ok=%v", tc.growth, tc.maxf, err, tc.ok)
		}
	}
}
