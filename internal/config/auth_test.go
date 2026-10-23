package config

import (
	"strings"
	"testing"
)

func TestAPIProtectReadsEnv(t *testing.T) {
	for _, tc := range []struct {
		val  string
		set  bool
		want bool
	}{
		{set: false, want: false},
		{val: "false", set: true, want: false},
		{val: "", set: true, want: false},
		{val: "TRUE", set: true, want: false}, // same strictness as TAILOR_API_AUTH_REQUIRED
		{val: "true", set: true, want: true},
	} {
		if tc.set {
			t.Setenv(EnvAPIProtectReads, tc.val)
		} else {
			t.Setenv(EnvAPIProtectReads, "")
		}
		if got := Load().APIProtectReads; got != tc.want {
			t.Errorf("%s=%q: APIProtectReads = %v, want %v", EnvAPIProtectReads, tc.val, got, tc.want)
		}
	}
}

func TestAuthPostureNeverLeaksToken(t *testing.T) {
	c := Config{APIToken: "sup3r-s3cret", APIProtectReads: true}
	p := c.AuthPosture()
	if strings.Contains(p, "sup3r-s3cret") {
		t.Fatalf("posture leaks the token: %q", p)
	}
	if !strings.Contains(p, "reads: Bearer token required") || !strings.Contains(p, "writes: Bearer token required") {
		t.Errorf("posture = %q", p)
	}
	if p := (Config{APIProtectReads: true}).AuthPosture(); !strings.Contains(p, "reads: disabled") {
		t.Errorf("protect reads without token: %q", p)
	}
	if p := (Config{}).AuthPosture(); !strings.Contains(p, "reads: open") || !strings.Contains(p, "writes: open") {
		t.Errorf("open posture: %q", p)
	}
}
