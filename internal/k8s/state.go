package k8s

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Reconciler state persisted on the pod (metadata, merge-patched separately from
// pods/resize, which only accepts resources), so a restarted or newly elected
// engine does not start from a clean slate. Like AnnLastApplied, these are part
// of the state model: do not rename them casually.
const (
	// AnnResizeFailed records the last REJECTED in-place resize per container:
	// {"<container>": {"at": "<RFC3339>", "reason": "<error, truncated>"}}. The
	// reconciler leaves the container alone for its failure backoff after "at"
	// (a deterministic rejection — validation, quota, QoS — would otherwise be
	// retried once per restart/failover).
	AnnResizeFailed = "tailor.k8s-tailor.dev/resize-failed"
	// AnnRolling marks a pod restarted by a disruptive (RestartContainer)
	// resize as one step of a rolling: {"<container>": "<RFC3339>"}, written in
	// the same patch as AnnLastApplied with the same time. It lets a restarted
	// engine resume the rolling (next pod without the cooldown, still one Ready
	// pod at a time behind the readiness gate) instead of waiting the cooldown.
	AnnRolling = "tailor.k8s-tailor.dev/rolling"
)

// ResizeFailure is one AnnResizeFailed entry.
type ResizeFailure struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// maxFailureReason bounds the reason stored in the annotation (annotations
// share a 256 KiB budget per object).
const maxFailureReason = 256

// ParseResizeFailed decodes AnnResizeFailed (malformed → empty).
func ParseResizeFailed(v string) map[string]ResizeFailure {
	out := map[string]ResizeFailure{}
	if v == "" {
		return out
	}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return map[string]ResizeFailure{}
	}
	return out
}

// FormatResizeFailed encodes AnnResizeFailed; reasons are cut on a rune boundary.
func FormatResizeFailed(m map[string]ResizeFailure) string {
	cp := make(map[string]ResizeFailure, len(m))
	for k, f := range m {
		f.At = f.At.UTC().Truncate(time.Second)
		if len(f.Reason) > maxFailureReason {
			r := f.Reason[:maxFailureReason]
			for len(r) > 0 && !utf8.ValidString(r) {
				r = r[:len(r)-1]
			}
			f.Reason = r
		}
		cp[k] = f
	}
	b, _ := json.Marshal(cp)
	return string(b)
}

// AnnGroupOOMFloor persists the learned OOM floors of "group" units (TailorGroup
// members folded into one synthetic unit, which has no object of its own) on the
// TailorGroup CR whose rule folded them:
// {"<namespace>/<unit>/<container>": {"mem": <bytes>, "at": "<RFC3339>"}}.
// One CR may fold many units (one per label value and namespace).
const AnnGroupOOMFloor = "tailor.k8s-tailor.dev/group-oom-floor"

// AnnotateKnativeService merge-writes annotations on a Knative Service's
// METADATA (nil value removes the key). Metadata only — never spec.template —
// so no new revision is minted. get + update: the engine's ClusterRole has
// get/list/update on services.serving.knative.dev (no patch).
func (c *Client) AnnotateKnativeService(ctx context.Context, namespace, name string, anns map[string]*string) error {
	res := c.dyn.Resource(ksvcGVR).Namespace(namespace)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cur := obj.GetAnnotations()
	if cur == nil {
		cur = map[string]string{}
	}
	for k, v := range anns {
		if v == nil {
			delete(cur, k)
		} else {
			cur[k] = *v
		}
	}
	obj.SetAnnotations(cur)
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}
