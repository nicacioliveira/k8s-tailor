package policy

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Container classes (mirrors k8s.ContainerClass; kept as strings here so the
// policy package stays independent of the classifier).
const (
	ClassApp             = "app"
	ClassInjectedSidecar = "injected-sidecar"
	ClassNativeSidecar   = "native-sidecar"
)

// ContainerRule is one entry of TailorPolicy/TailorProfile spec.containers: a
// per-container override matched by name glob and/or container class. Rules are
// evaluated in order; the FIRST match wins.
type ContainerRule struct {
	Match struct {
		Names   []string `json:"names,omitempty"`   // container-name globs (empty = any)
		Classes []string `json:"classes,omitempty"` // app | injected-sidecar | native-sidecar (empty = any)
	} `json:"match"`
	Exclude bool   `json:"exclude,omitempty"` // no recommendation, no actuation
	Mode    string `json:"mode,omitempty"`    // overrides the workload mode for this container
	Apply   string `json:"apply,omitempty"`
	// Direction overrides the workload direction for this container.
	Direction string `json:"direction,omitempty"`
	// Guaranteed overrides the workload's skip | resizeBoth for this container.
	Guaranteed string `json:"guaranteed,omitempty"`
	ProfileRef string `json:"profileRef,omitempty"` // TailorProfile whose cpu/memory model sizes this container
	// ProxyAnnotations: for a mesh proxy absent from the workload template, the
	// durable (rollout) path writes the mesh's resource annotations on the pod
	// template (sidecar.istio.io/proxyCPU…, config.linkerd.io/proxy-cpu-request…)
	// instead of patching a container that does not exist there.
	ProxyAnnotations *bool   `json:"proxyAnnotations,omitempty"`
	CPU              resSpec `json:"cpu,omitempty"`
	Memory           resSpec `json:"memory,omitempty"`

	origin string // "policy:<name> containers[i]" | "profile:<name> containers[i]"
}

func (r ContainerRule) matches(name, class string) bool {
	if len(r.Match.Names) == 0 && len(r.Match.Classes) == 0 {
		return false // an empty match would silently capture every container
	}
	if len(r.Match.Names) > 0 && !globAny(r.Match.Names, name) {
		return false
	}
	if len(r.Match.Classes) > 0 && !containsStr(r.Match.Classes, class) {
		return false
	}
	return true
}

func globAny(globs []string, s string) bool {
	for _, g := range globs {
		if g == s {
			return true
		}
		if strings.ContainsAny(g, "*?[") {
			if ok, err := filepath.Match(g, s); err == nil && ok {
				return true
			}
		}
	}
	return false
}

func withOrigin(rules []ContainerRule, kind, name string) []ContainerRule {
	out := make([]ContainerRule, len(rules))
	for i, r := range rules {
		r.origin = fmt.Sprintf("%s:%s containers[%d]", kind, name, i)
		out[i] = r
	}
	return out
}

// Per-container annotation overrides on the workload (pod for bare pods):
//
//	tailor.k8s-tailor.dev/container.<name>.mode               Off|Recommend|Webhook|InPlace|Both
//	tailor.k8s-tailor.dev/container.<name>.exclude            "true"
//	tailor.k8s-tailor.dev/container.<name>.profile            TailorProfile name
//	tailor.k8s-tailor.dev/container.<name>.apply              InPlace|Rollout|InPlacePreferred
//	tailor.k8s-tailor.dev/container.<name>.direction          Both|Down|Up
//	tailor.k8s-tailor.dev/container.<name>.guaranteed         skip|resizeBoth
//	tailor.k8s-tailor.dev/container.<name>.min-cpu|max-cpu    quantity (e.g. 50m)
//	tailor.k8s-tailor.dev/container.<name>.min-mem|max-mem    quantity (e.g. 64Mi)
//	tailor.k8s-tailor.dev/container.<name>.proxy-annotations  "true"|"false"
const AnnContainerPrefix = "tailor.k8s-tailor.dev/container."

// AnnContainer returns the per-container annotation key for field.
func AnnContainer(container, field string) string {
	return AnnContainerPrefix + container + "." + field
}

// sidecarProfile is the built-in sizing model for infrastructure sidecars. A
// proxy's CPU tracks requests/connections, not the app's work, and is bursty —
// hence a higher quantile and margin; its memory is dominated by config/xDS size
// and connection count and must never be squeezed into an OOM. The app policy's
// min/max are deliberately NOT inherited (an app floor of 500m would inflate
// every proxy). Limit factors are kept (they only re-derive existing limits).
func sidecarProfile(base Resolved) (ResourceProfile, ResourceProfile) {
	return ResourceProfile{Quantile: 0.95, Margin: 0.25, Min: 0.01, LimitFactor: base.CPU.LimitFactor},
		ResourceProfile{Quantile: 0.99, Margin: 0.2, Min: 32 * 1024 * 1024, LimitFactor: base.Mem.LimitFactor}
}

// actuates reports whether a mode changes anything in the cluster.
func actuates(m Mode) bool { return m.Webhook() || m.InPlace() }

// ResolveContainer refines the workload-level policy for one container:
//
//  1. class default — an injected sidecar gets the built-in sidecar profile and
//     is capped at Recommend (never actuated until the user opts in);
//  2. first matching spec.containers rule (annotation-pinned profile's rules,
//     then the policy's, then the policy profile's);
//  3. per-container annotations (highest precedence).
//
// Only an explicit mode on a rule or annotation lifts the sidecar cap.
func (s *Store) ResolveContainer(base Resolved, annotations map[string]string, name, class string) Resolved {
	r := base
	r.ContainerClass = class
	r.ContainerPolicy = "policy"
	if class == ClassInjectedSidecar {
		r.CPU, r.Mem = sidecarProfile(base)
		if actuates(r.Mode) {
			r.Mode = ModeRecommend
		}
		r.ContainerPolicy = "default:injected-sidecar"
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rule := range base.ContainerRules {
		if !rule.matches(name, class) {
			continue
		}
		r.ContainerPolicy = rule.origin
		if rule.ProfileRef != "" {
			if pr, ok := s.profiles[rule.ProfileRef]; ok {
				applyRes(&r.CPU, pr.CPU, false)
				applyRes(&r.Mem, pr.Memory, true)
			}
		}
		applyRes(&r.CPU, rule.CPU, false)
		applyRes(&r.Mem, rule.Memory, true)
		if rule.Mode != "" {
			r.Mode = Mode(rule.Mode)
		}
		if rule.Apply != "" {
			r.Apply = Apply(rule.Apply)
		}
		if rule.Direction != "" {
			r.Direction = Direction(rule.Direction)
		}
		if rule.Guaranteed != "" {
			r.Guaranteed = Guaranteed(rule.Guaranteed)
		}
		if rule.ProxyAnnotations != nil {
			r.ProxyAnnotations = *rule.ProxyAnnotations
		}
		if rule.Exclude {
			r.Excluded = true
		}
		break
	}

	ann := func(f string) string { return annotations[AnnContainer(name, f)] }
	touched := false
	if p := ann("profile"); p != "" {
		if pr, ok := s.profiles[p]; ok {
			applyRes(&r.CPU, pr.CPU, false)
			applyRes(&r.Mem, pr.Memory, true)
			touched = true
		}
	}
	for f, dst := range map[string]*float64{"min-cpu": &r.CPU.Min, "max-cpu": &r.CPU.Max} {
		if q := parseQty(ann(f), false); q > 0 {
			*dst, touched = q, true
		}
	}
	for f, dst := range map[string]*float64{"min-mem": &r.Mem.Min, "max-mem": &r.Mem.Max} {
		if q := parseQty(ann(f), true); q > 0 {
			*dst, touched = q, true
		}
	}
	if v := ann("apply"); v != "" {
		r.Apply, touched = Apply(v), true
	}
	if v := ann("direction"); v != "" {
		r.Direction, touched = Direction(v), true
	}
	if v := ann("guaranteed"); v != "" {
		r.Guaranteed, touched = Guaranteed(v), true
	}
	if v := ann("proxy-annotations"); v != "" {
		r.ProxyAnnotations, touched = v == "true", true
	}
	if v := ann("mode"); v != "" {
		r.Mode, touched = Mode(v), true
		r.Overridden = true
	}
	if ann("exclude") == "true" {
		r.Excluded, touched = true, true
	}
	if touched {
		r.ContainerPolicy = "annotation"
	}
	if r.Excluded || r.Ignored {
		r.Mode = ModeOff
	}
	return r
}
