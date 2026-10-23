package k8s

import (
	"path/filepath"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// ContainerClass is what role a container plays in its pod. It drives the
// default policy (an injected sidecar is only recommended until the user opts
// in) and which actuation paths are legal (init/ephemeral containers are never
// resized; a container absent from the workload template is never patched into
// it). See docs/CONTAINERS.md.
type ContainerClass string

const (
	// ClassApp is a regular container declared by the workload owner.
	ClassApp ContainerClass = "app"
	// ClassInjectedSidecar is infrastructure next to the app: injected by a
	// mutating webhook (absent from the template, or carrying a mesh injection
	// annotation) or recognized by name (istio-proxy, linkerd-proxy, vault-agent…).
	// It may itself live in initContainers as a native sidecar (Native=true).
	ClassInjectedSidecar ContainerClass = "injected-sidecar"
	// ClassNativeSidecar is an initContainer with restartPolicy: Always declared
	// by the workload owner (Kubernetes native sidecar) and not a known
	// infrastructure sidecar.
	ClassNativeSidecar ContainerClass = "native-sidecar"
	// ClassInit is a regular (run-to-completion) init container.
	ClassInit ContainerClass = "init"
	// ClassEphemeral is a debug container added via the ephemeralcontainers
	// subresource.
	ClassEphemeral ContainerClass = "ephemeral"
)

// Resizable reports whether a container of this class may carry a
// recommendation and be resized. Non-restartable init containers and ephemeral
// containers cannot be resized in place (Kubernetes docs, in-place resize
// limitations); resizing an init container's template is pointless (it only
// runs at start-up). Empty = legacy/unknown → treated as app.
func (c ContainerClass) Resizable() bool {
	return c != ClassInit && c != ClassEphemeral
}

// Service-mesh identifiers (Container.Mesh). A mesh is only set when the
// injector's own marker is present on the pod — that is what makes it safe to
// size the proxy through the mesh's resource annotations.
const (
	MeshIstio   = "istio"
	MeshLinkerd = "linkerd"
)

// Injection markers stamped on the POD by the mesh injectors.
const (
	AnnIstioStatus         = "sidecar.istio.io/status"
	AnnLinkerdProxyVersion = "linkerd.io/proxy-version"
	lblLinkerdControlPlane = "linkerd.io/control-plane-ns"
)

// DefaultSidecarNames are container-name globs recognized as infrastructure
// sidecars out of the box. TailorConfig.spec.sidecarNames extends the list.
var DefaultSidecarNames = []string{
	// Istio (classic + native sidecar) and its init/validation containers.
	"istio-proxy", "istio-init", "istio-validation",
	// Linkerd.
	"linkerd-proxy", "linkerd-init", "linkerd-network-validator",
	// Generic Envoy sidecars (Consul, App Mesh, custom).
	"envoy", "envoy-sidecar", "consul-dataplane",
	// HashiCorp Vault agent injector.
	"vault-agent", "vault-agent-init",
	// Google Cloud SQL Auth Proxy.
	"cloud-sql-proxy", "cloudsql-proxy",
	// Knative serving.
	"queue-proxy",
	// Observability agents commonly run as sidecars.
	"datadog-agent", "otc-container", "otel-collector", "opentelemetry-collector",
}

// Classifier recognizes infrastructure sidecars by name glob.
type Classifier struct {
	names []string
}

// NewClassifier builds a classifier over the built-in list plus extra globs.
func NewClassifier(extra []string) *Classifier {
	names := append([]string{}, DefaultSidecarNames...)
	for _, n := range extra {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return &Classifier{names: names}
}

// IsSidecarName reports whether name matches a known sidecar glob.
func (k *Classifier) IsSidecarName(name string) bool {
	for _, g := range k.names {
		if g == name {
			return true
		}
		if strings.ContainsAny(g, "*?[") {
			if ok, err := filepath.Match(g, name); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// meshOf returns the mesh that injected container name into a pod with these
// annotations/labels, or "" when no injector marker is present.
func meshOf(name string, anns, lbls map[string]string) string {
	switch name {
	case "istio-proxy":
		if anns[AnnIstioStatus] != "" {
			return MeshIstio
		}
	case "linkerd-proxy":
		if anns[AnnLinkerdProxyVersion] != "" || lbls[lblLinkerdControlPlane] != "" {
			return MeshLinkerd
		}
	}
	return ""
}

// isNativeSidecar reports whether an init container is a Kubernetes native
// sidecar (restartPolicy: Always).
func isNativeSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// classify assigns class/native/mesh to a container read from a pod or template.
func (k *Classifier) classify(ct *Container, init, native bool, anns, lbls map[string]string) {
	ct.Native = native
	ct.Mesh = meshOf(ct.Name, anns, lbls)
	sidecar := ct.Mesh != "" || k.IsSidecarName(ct.Name)
	switch {
	case init && !native:
		ct.Class = ClassInit
	case sidecar:
		ct.Class = ClassInjectedSidecar
	case native:
		ct.Class = ClassNativeSidecar
	default:
		ct.Class = ClassApp
	}
}

// TemplateContainers returns the sizable containers of a workload template:
// spec.containers plus native sidecars declared in spec.initContainers. Regular
// init containers are omitted (never sized). All are InTemplate.
func (k *Classifier) TemplateContainers(spec *corev1.PodSpec, anns, lbls map[string]string) []Container {
	out := containersOf(spec.Containers)
	for i := range out {
		k.classify(&out[i], false, false, anns, lbls)
		out[i].InTemplate = true
	}
	for i := range spec.InitContainers {
		ic := &spec.InitContainers[i]
		if !isNativeSidecar(ic) {
			continue
		}
		ct := containersOf([]corev1.Container{*ic})[0]
		k.classify(&ct, true, true, anns, lbls)
		ct.InTemplate = true
		out = append(out, ct)
	}
	return out
}

// PodContainers returns EVERY container of a pod — app containers, init
// containers (native sidecars and regular), ephemeral — classified, with spec
// resources. InTemplate is false (a pod is not a template; the recommender sets
// it by comparing with the owner's template).
func (k *Classifier) PodContainers(p *corev1.Pod) []Container {
	out := containersOf(p.Spec.Containers)
	for i := range out {
		k.classify(&out[i], false, false, p.Annotations, p.Labels)
	}
	inits := containersOf(p.Spec.InitContainers)
	for i := range inits {
		k.classify(&inits[i], true, isNativeSidecar(&p.Spec.InitContainers[i]), p.Annotations, p.Labels)
	}
	out = append(out, inits...)
	for i := range p.Spec.EphemeralContainers {
		out = append(out, Container{Name: p.Spec.EphemeralContainers[i].Name, Class: ClassEphemeral})
	}
	return out
}

// sizable filters to the containers that may carry a recommendation.
func sizable(cs []Container) []Container {
	out := cs[:0:0]
	for _, c := range cs {
		if c.Class.Resizable() {
			out = append(out, c)
		}
	}
	return out
}

// classifier state on the Client (configurable via TailorConfig).
type classifierHolder struct {
	mu  sync.RWMutex
	cls *Classifier
}

func (h *classifierHolder) get() *Classifier {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cls == nil {
		return NewClassifier(nil)
	}
	return h.cls
}

// SetSidecarNames sets the extra sidecar name globs (TailorConfig.spec.sidecarNames).
func (c *Client) SetSidecarNames(extra []string) {
	c.cls.mu.Lock()
	c.cls.cls = NewClassifier(extra)
	c.cls.mu.Unlock()
}

// Classifier returns the active container classifier.
func (c *Client) Classifier() *Classifier { return c.cls.get() }
