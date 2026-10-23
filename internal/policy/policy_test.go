package policy

import (
	"encoding/json"
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
)

func TestDirectionGate(t *testing.T) {
	cases := []struct {
		d             Direction
		cur, tgt, out float64
	}{
		{DirBoth, 1, 2, 2}, {DirBoth, 2, 1, 1}, // unrestricted
		{DirDown, 1, 2, 1}, {DirDown, 2, 1, 1}, // never raise
		{DirUp, 1, 2, 2}, {DirUp, 2, 1, 2}, // never lower
	}
	for _, c := range cases {
		if got := c.d.Gate(c.cur, c.tgt); got != c.out {
			t.Errorf("%s.Gate(%v,%v)=%v want %v", c.d, c.cur, c.tgt, got, c.out)
		}
	}
}

func mkPolicy(name, spec string) policySpec {
	var ps policySpec
	if err := json.Unmarshal([]byte(spec), &ps); err != nil {
		panic(err)
	}
	ps.name = name
	return ps
}

func newStore(policies ...policySpec) *Store {
	return &Store{fallback: config.Config{}, defMode: ModeRecommend, policies: policies}
}

func TestResolvePolicyMatch(t *testing.T) {
	s := newStore(mkPolicy("p", `{"priority":10,"mode":"InPlace","direction":"Down","headroomGuard":true,"headroomThreshold":0.03,"selector":{"namespaces":["prod","apps-*"]}}`))

	// Namespace match (exact) applies the policy.
	r := s.Resolve("prod", nil, nil)
	if r.Mode != ModeInPlace {
		t.Fatalf("mode=%v want InPlace", r.Mode)
	}
	if r.Direction != DirDown {
		t.Fatalf("direction=%v want Down", r.Direction)
	}
	if !r.HeadroomGuard || r.HeadroomThreshold != 0.03 {
		t.Fatalf("guard=%v thr=%v want true/0.03", r.HeadroomGuard, r.HeadroomThreshold)
	}

	// Glob namespace match.
	if r := s.Resolve("apps-acme", nil, nil); r.Mode != ModeInPlace {
		t.Fatalf("glob match mode=%v want InPlace", r.Mode)
	}

	// Non-matching namespace falls back to the default mode, direction Both.
	r2 := s.Resolve("other", nil, nil)
	if r2.Mode != ModeRecommend {
		t.Fatalf("default mode=%v want Recommend", r2.Mode)
	}
	if r2.Direction != DirBoth {
		t.Fatalf("default direction=%v want Both", r2.Direction)
	}
}

func TestResolveAnnotationOverride(t *testing.T) {
	s := newStore(mkPolicy("p", `{"priority":10,"mode":"Both","selector":{"namespaces":["prod"]}}`))

	// A per-workload annotation pins the mode over the policy and marks Overridden.
	r := s.Resolve("prod", nil, map[string]string{AnnMode: "Off"})
	if r.Mode != ModeOff || !r.Overridden {
		t.Fatalf("override mode=%v overridden=%v want Off/true", r.Mode, r.Overridden)
	}
	// ignore=true also forces Off.
	if r := s.Resolve("prod", nil, map[string]string{AnnIgnore: "true"}); r.Mode != ModeOff || !r.Overridden {
		t.Fatalf("ignore mode=%v overridden=%v", r.Mode, r.Overridden)
	}
}

func TestResolvePriorityAndLabels(t *testing.T) {
	// Higher priority wins; matchLabels must all match.
	s := newStore(
		mkPolicy("low", `{"priority":1,"mode":"Recommend","selector":{"namespaces":["prod"]}}`),
		mkPolicy("high", `{"priority":100,"mode":"InPlace","selector":{"namespaces":["prod"],"matchLabels":{"tier":"web"}}}`),
	)
	// refresh sorts by priority desc; emulate that ordering for the test.
	s.policies = []policySpec{s.policies[1], s.policies[0]}

	// Label matches the high-priority policy.
	if r := s.Resolve("prod", map[string]string{"tier": "web"}, nil); r.Mode != ModeInPlace {
		t.Fatalf("labelled mode=%v want InPlace (high prio)", r.Mode)
	}
	// Label does not match high; falls to low.
	if r := s.Resolve("prod", map[string]string{"tier": "db"}, nil); r.Mode != ModeRecommend {
		t.Fatalf("unlabelled mode=%v want Recommend (low prio)", r.Mode)
	}
}
