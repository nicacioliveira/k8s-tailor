package policy

import "testing"

// seasonGate.minCorrelation / minPairs tune the seasonality gate per policy.
// Unset (or out of the CRD bounds) = the engine defaults (0 here: the
// recommender applies 0.5 / 24).
func TestResolveSeasonGate(t *testing.T) {
	cases := []struct {
		name     string
		spec     string
		corr     float64
		minPairs int
	}{
		{"unset", `{"mode":"InPlace"}`, 0, 0},
		{"set", `{"mode":"InPlace","seasonGate":{"minCorrelation":0.7,"minPairs":48}}`, 0.7, 48},
		{"correlation out of range ignored", `{"seasonGate":{"minCorrelation":1.5,"minPairs":48}}`, 0, 48},
		{"negative correlation ignored", `{"seasonGate":{"minCorrelation":-0.2}}`, 0, 0},
		{"pairs below 2 ignored", `{"seasonGate":{"minPairs":1}}`, 0, 0},
		{"pairs above the bound ignored", `{"seasonGate":{"minPairs":100000}}`, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newStore(mkPolicy("p", c.spec)).Resolve("ns", nil, nil)
			if r.SeasonMinCorrelation != c.corr || r.SeasonMinPairs != c.minPairs {
				t.Fatalf("gate = (%v, %d), want (%v, %d)", r.SeasonMinCorrelation, r.SeasonMinPairs, c.corr, c.minPairs)
			}
		})
	}
}

// memory.enforceLimit (opt-in, default false) lets the engine give a memory
// limit to a container that runs without one. A profile, the policy inline
// model, a container rule and a per-container profile may set it; the last
// explicit value wins (false switches it back off).
func TestResolveMemoryEnforceLimit(t *testing.T) {
	s := newStore(mkPolicy("p", `{"mode":"InPlace","profileRef":"strict","memory":{"enforceLimit":false},
		"containers":[{"match":{"names":["db"]},"memory":{"enforceLimit":true}}]}`))
	s.profiles = map[string]scaling{"strict": {Memory: resSpec{EnforceLimit: boolPtr(true)}}}
	r := s.Resolve("ns", nil, nil)
	if r.Mem.EnforceLimit {
		t.Fatal("policy inline enforceLimit=false must override the profile's true")
	}
	if c := s.ResolveContainer(r, nil, "db", ClassApp); !c.Mem.EnforceLimit {
		t.Fatal("container rule enforceLimit=true not applied")
	}
	if c := s.ResolveContainer(r, nil, "app", ClassApp); c.Mem.EnforceLimit {
		t.Fatal("rule leaked to a non-matching container")
	}
	if d := newStore().Resolve("ns", nil, nil); d.Mem.EnforceLimit || d.CPU.EnforceLimit {
		t.Fatal("enforceLimit must default to false")
	}
	// CPU has no such switch: a cpu.enforceLimit in the spec is ignored.
	cpu := newStore(mkPolicy("c", `{"cpu":{"enforceLimit":true}}`)).Resolve("ns", nil, nil)
	if cpu.CPU.EnforceLimit {
		t.Fatal("cpu.enforceLimit must be ignored")
	}
}

func boolPtr(b bool) *bool { return &b }
