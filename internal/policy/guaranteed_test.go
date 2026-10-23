package policy

import "testing"

// guaranteed (skip | resizeBoth): policy field, container rule and per-container
// annotation, in that precedence order; the default is skip.
func TestResolveGuaranteed(t *testing.T) {
	s := newStore(mkPolicy("p", `{"priority":10,"mode":"InPlace","guaranteed":"resizeBoth","selector":{"namespaces":["prod"]},
		"containers":[{"match":{"names":["worker"]},"guaranteed":"skip"}]}`))
	if r := s.Resolve("other", nil, nil); r.Guaranteed != GuaranteedSkip || r.Guaranteed.ResizeBoth() {
		t.Fatalf("default = %q, want skip", r.Guaranteed)
	}
	base := s.Resolve("prod", nil, nil)
	if !base.Guaranteed.ResizeBoth() {
		t.Fatalf("policy guaranteed = %q, want resizeBoth", base.Guaranteed)
	}
	if r := s.ResolveContainer(base, nil, "app", "app"); !r.Guaranteed.ResizeBoth() {
		t.Fatalf("container without a rule = %q, want the workload value", r.Guaranteed)
	}
	if r := s.ResolveContainer(base, nil, "worker", "app"); r.Guaranteed != GuaranteedSkip {
		t.Fatalf("rule guaranteed:skip = %q, want skip", r.Guaranteed)
	}
	ann := map[string]string{AnnContainer("worker", "guaranteed"): "resizeBoth"}
	if r := s.ResolveContainer(base, ann, "worker", "app"); !r.Guaranteed.ResizeBoth() {
		t.Fatalf("annotation must win over the rule: %q", r.Guaranteed)
	}
}

func TestValidateGuaranteedRule(t *testing.T) {
	ok := ContainerRule{Guaranteed: "resizeBoth"}
	ok.Match.Names = []string{"app"}
	if err := validateRule(ok); err != nil {
		t.Fatalf("resizeBoth rejected: %v", err)
	}
	bad := ContainerRule{Guaranteed: "both"}
	bad.Match.Names = []string{"app"}
	if err := validateRule(bad); err == nil {
		t.Fatal("unknown guaranteed value accepted")
	}
}

func TestResolveAutoQuantile(t *testing.T) {
	s := newStore(mkPolicy("on", `{"priority":10,"selector":{"namespaces":["bursty"]},"autoQuantile":true,"burstThreshold":4.5}`),
		mkPolicy("off", `{"priority":5,"selector":{"namespaces":["calm"]},"autoQuantile":false,"burstThreshold":0.5}`))
	s.fallback.AutoQuantile, s.fallback.BurstThreshold = true, 3
	if r := s.Resolve("bursty", nil, nil); !r.AutoQuantile || r.BurstThreshold != 4.5 {
		t.Fatalf("policy values lost: %+v", r)
	}
	// An explicit false beats the engine-wide default; an invalid threshold (<= 1) is ignored.
	if r := s.Resolve("calm", nil, nil); r.AutoQuantile || r.BurstThreshold != 3 {
		t.Fatalf("calm: autoQuantile=%v threshold=%v, want false / the engine default 3", r.AutoQuantile, r.BurstThreshold)
	}
	// No policy: the engine-wide default applies.
	if r := s.Resolve("other", nil, nil); !r.AutoQuantile || r.BurstThreshold != 3 {
		t.Fatalf("fallback not applied: %+v", r)
	}
}
