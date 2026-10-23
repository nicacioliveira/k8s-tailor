# Multi-container pods, init containers and sidecars

This document has two parts:

1. **Audit** — how Tailor handled multi-container pods, init containers and
   sidecars (injected and native) **before** sidecar support. The `file:line`
   references are from commit `e130093`.
2. **Current behavior** — container classification, per-container policy,
   actuation rules and the webhook ordering relative to mesh injectors (Istio, Linkerd).

External sources were checked on 2026-10-21 and only what was verified is cited here
(see [References](#references)).

---

## 1. Audit (state at `e130093`)

### 1.1 What the engine could see

The unit of recommendation is `(namespace, kind, workload, container)`
(`internal/recommender/recommender.go:425`). The container list of each workload
came from different places depending on the kind:

| Kind | Source of `Workload.Containers` | Reference |
|---|---|---|
| Deployment / StatefulSet / DaemonSet | `spec.template.spec.containers` (template only) | `internal/k8s/client.go:576`, `:595`, `:614` |
| Pod without an owner | the pod's own `spec.containers` | `internal/k8s/client.go:664` |
| `group` (TailorGroup, e.g. agent-sandbox) | `spec.containers` of the **first** pod seen | `internal/k8s/client.go:648-653` |
| `ksvc` (Knative) | `spec.template.spec.containers` of the Service | `internal/k8s/client.go:683-690`, `:722-741` |

`containersOf` (`internal/k8s/resources.go:8-36`) reads **only** `spec.containers`.
`initContainers` and `ephemeralContainers` were never read anywhere.

Consequences:

- **Sidecar injected into a Deployment/StatefulSet/DaemonSet** (istio-proxy,
  linkerd-proxy, vault-agent…): absent from the template, therefore absent from
  `Workload.Containers`. The recommendation loop (`recommender.go:587`) never
  visited it: **no recommendation, no actuation, no visibility**. The
  reconciler walks the live pod's containers (`reconciler.go:217`), but
  `eng.Get` fails (`reconciler.go:218`) and it skips. Safe by accident, but the
  cost of the proxy (which in large meshes can be significant) stayed invisible.
- **Sidecar injected into a `group` and into a Pod without an owner**: here the injected
  container IS in the pod's `spec.containers`, so it received a recommendation and was
  acted on **with the app's policy** (same mode, same minimums/maximums, same
  quantiles). A `cpu.min: 500m` meant for the app inflated istio-proxy; a
  `mode: InPlace` resized the proxy without the user having asked for it.
  **Real gap.**
- **Knative `queue-proxy`**: the comment at `recommender.go:588-591` says the
  queue-proxy recommendation is "informational", but since `ksvc` uses the containers of the
  Service template, queue-proxy never even entered `Workload.Containers`. The only
  effective handling is the *skip* in `PatchKnativeService` when the container does
  not exist in the template (`client.go:1286-1301`).
- **Native sidecar** (`initContainers[]` with `restartPolicy: Always`; Istio ≥ 1.27
  injects `istio-proxy` this way by default — see References): ignored everywhere —
  no recommendation, no resize, no OOM reading. Its usage did reach VM
  (the query at `recommender.go:654` only excludes `container=""` and `"POD"`), but
  was discarded because there was no matching container in the workload.
- **Regular init containers** (`istio-init`, `linkerd-init`…): ignored. Correct
  for actuation (they cannot be resized in-place, see 1.4), but also
  ignored in the QoS calculation (see 1.3).
- **Ephemeral containers**: ignored. Correct (they cannot be resized and have no
  `resources`).

### 1.2 Policy

`policy.Store.Resolve` (`internal/policy/policy.go:720-803`) resolves **one**
policy per workload (namespace + labels + workload annotations) and the refresh
applies it equally to all containers (`recommender.go:585`). There was no
per-container dimension: no exclusion, no mode, no minimums/maximums, no profile.

### 1.3 Pod sum, QoS and partial limits

- **QoS Guaranteed**: the reconciler skips in-place (`reconciler.go:267-275`). OK.
- **QoS BestEffort**: there was no check. A resize that adds a request to a
  BestEffort pod is rejected by the API server (the QoS class is immutable), went into
  `failBackoff` and was retried every 5 min forever. **Gap.**
- **Burstable → Guaranteed**: there was no check. If a container's resize
  left requests == limits for CPU **and** memory in **all** containers, the
  pod would become Guaranteed — also rejected. Rare, but possible with
  `limitFactor: 1`. **Gap.**
- **Partial limit**: `Plan.Limits` (`internal/reconciler/decide.go:90-98`) only
  raises the CPU limit when the recommendation carries one (`rec.CPULimit > 0`, i.e.
  `cpu.limitFactor > 0`). With `allowExceedLimit: true` and `limitFactor: 0`, a
  recommended CPU request above the existing limit produced a patch with
  `requests > limits` — rejected. **Gap.**
- **Pod-level resources** (`spec.resources`, PodLevelResources): ignored.
  With pod-level resources, the pod request must be ≥ the sum of the container requests
  and each container limit ≤ the pod limit; a container resize that
  violates this is rejected. **Gap.**
- **Pod sum / scheduling**: each container is sized in isolation. In-place
  growth that does not fit on the node becomes `PodResizePending/Infeasible|Deferred`,
  which was already handled (`reconciler.go:252-263`, `client.go:1045-1061`). OK.
- **Native sidecar OOM**: the OOM reading (`client.go:893-934`) only walks
  `status.containerStatuses`; `initContainerStatuses` (where native sidecars
  live) was not read. **Gap.**

### 1.4 Actuation

- **In-place** (`client.go:1193-1214`): strategic patch on
  `spec.containers[name=…]` in the `resize` subresource. There was no path for
  `spec.initContainers`, so native sidecars could not be resized.
- **Rollout** (`client.go:1224-1257`): strategic patch on
  `spec.template.spec.containers[name=…]`. For a container that **does not exist** in the
  template, the merge strategy by `name` **appends** a new container with only
  `name` and `resources` — invalid (no `image`) and rejected by validation.
  Today this did not happen for Deployment/STS/DS only because the injected sidecar never
  had a recommendation (1.1). There was no explicit protection.
- **Container order**: irrelevant for the reconciler (merge by name) and
  correct in the webhook (the index `i` in `/spec/containers/<i>/resources` is computed
  on the object received in the same invocation, `webhook.go:103`, `:181`).

### 1.5 Webhook

- Only walks `spec.containers` (`internal/webhook/webhook.go:103`); native sidecars
  in `initContainers` were not sized at admission.
- `MutatingWebhookConfiguration` `k8s-tailor` (`chart/templates/webhook.yaml:92-125`)
  has no `reinvocationPolicy` — the default is `Never`. If the Istio injector runs
  **after** Tailor, istio-proxy is never seen by Tailor at admission.
- The patch is idempotent (the direction *gate* compares the pod's request with the
  recommendation and skips if nothing changes, `webhook.go:137`), so reinvoking would be safe.

### 1.6 Summary of gaps

| # | Gap | Severity |
|---|---|---|
| L1 | Injected sidecar invisible in Deployment/STS/DS/ksvc (no rec) | medium (invisible cost) |
| L2 | Sidecar injected into `group`/Pod acted on with the app's policy and profile | **high** |
| L3 | Native sidecar ignored (rec, resize, webhook, OOM) | medium |
| L4 | No per-container policy (exclusion, mode, min/max, profile) | medium |
| L5 | BestEffort and Burstable→Guaranteed not checked in in-place | low (useless retry) |
| L6 | Partial CPU limit: request > limit without `limitFactor` | low |
| L7 | Pod-level resources ignored | low (new feature) |
| L8 | No explicit protection against template patch for an absent container | medium (latent) |
| L9 | Webhook without `reinvocationPolicy: IfNeeded` | medium |

---

## 2. Current behavior

### 2.1 Container classes

Every pod/template read goes through the classifier (`internal/k8s/containers.go`):

| Class | How it is detected | Recommendation | Actuation |
|---|---|---|---|
| `app` | regular container declared in the template | yes, workload policy | in-place, template, webhook |
| `injected-sidecar` | name matches the built-in/configured list **or** a mesh injection marker on the pod (`sidecar.istio.io/status`, `linkerd.io/proxy-version`, label `linkerd.io/control-plane-ns`) **or** the container exists in the live pods but **not** in the workload template | yes, **sidecar profile**, **Recommend** mode until opt-in | in-place (including if native); template only if it is in the template; otherwise proxy annotations (opt-in) |
| `native-sidecar` | `initContainers[]` with `restartPolicy: Always` declared by the owner and not a known sidecar | yes, workload policy | in-place on `spec.initContainers`; template on `spec.template.spec.initContainers` |
| `init` | regular `initContainers[]` (run-to-completion), e.g. `istio-init` | **never** | **never** (only enters the QoS calculation) |
| `ephemeral` | `ephemeralContainers[]` | **never** | **never** |

An injected sidecar that is itself native (the `istio-proxy` of Istio ≥ 1.27) is
`injected-sidecar` with `native=true`: the class dictates the policy; `native` dictates the
list (`containers` vs `initContainers`) of the patch.

Built-in list of names (globs): `istio-proxy`, `istio-init`, `istio-validation`,
`linkerd-proxy`, `linkerd-init`, `linkerd-network-validator`, `envoy`,
`envoy-sidecar`, `consul-dataplane`, `vault-agent`, `vault-agent-init`,
`cloud-sql-proxy`, `cloudsql-proxy`, `queue-proxy`, `datadog-agent`,
`otc-container`, `otel-collector`, `opentelemetry-collector`. To add more:

```yaml
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorConfig
metadata: { name: cluster }
spec:
  sidecarNames: ["acme-agent-*", "my-envoy"]
```

A container with a known name **declared in the template** (e.g. your own
`cloud-sql-proxy`) is also `injected-sidecar` — it gets the safe default
(Recommend + sidecar profile) — but, since it is in the template, the rollout path
can patch it normally.

### 2.2 Per-container policy

Resolution, from lowest to highest precedence:

1. **Workload policy** (`TailorPolicy` + `profileRef` + workload annotations).
2. **Class default**: `injected-sidecar` gets the built-in sidecar profile
   (CPU q0.95 + 25%, min 10m; memory q0.99 + 20%, min 32Mi; the app's `min/max` are
   **not** inherited — a 500m floor from the app would inflate every proxy) and the mode is
   **capped at Recommend** (Off stays Off).
3. **First matching rule** in `spec.containers` (in order: rules from the
   profile pinned by annotation on the workload, then from the policy, then from the
   policy's `profileRef`). Only an explicit `mode` in the rule takes the sidecar out of Recommend.
4. **Per-container annotations** on the workload (on the pod, for Pods without an owner/groups):

| Annotation | Value |
|---|---|
| `tailor.k8s-tailor.dev/container.<name>.mode` | `Off`\|`Recommend`\|`Webhook`\|`InPlace`\|`Both` |
| `tailor.k8s-tailor.dev/container.<name>.exclude` | `"true"` — no recommendation and no actuation |
| `tailor.k8s-tailor.dev/container.<name>.profile` | `TailorProfile` name |
| `tailor.k8s-tailor.dev/container.<name>.apply` | `InPlace`\|`Rollout`\|`InPlacePreferred` |
| `tailor.k8s-tailor.dev/container.<name>.direction` | `Both`\|`Down`\|`Up` |
| `tailor.k8s-tailor.dev/container.<name>.min-cpu` / `max-cpu` | quantity (`50m`) |
| `tailor.k8s-tailor.dev/container.<name>.min-mem` / `max-mem` | quantity (`64Mi`) |
| `tailor.k8s-tailor.dev/container.<name>.proxy-annotations` | `"true"`\|`"false"` |

The UI's per-workload toggle (`tailor.k8s-tailor.dev/mode`) does **not** enable injected
sidecars — only the per-container annotation or a rule does.

`tailor.k8s-tailor.dev/ignore: "true"` on the workload is **absolute**: it is applied
last, after the mode annotation and after every per-container rule/annotation, so no
`container.<name>.mode` re-enables a container of an ignored workload. `exclude`
(rule or annotation) always resolves to Off as well. Both hold on every path: the
refresh (no recommendation for an excluded container, Off for an ignored workload),
the reconciler and the webhook (they act only on an entry whose effective mode
actuates) and the UI writes (`POST /api/workload`): a write is reflected in memory at
once, but re-resolved through the same policy resolution as the refresh, so a mode
written on an ignored workload, or a workload mode written over a container pinned by
its own `mode`/`exclude` annotation, never actuates in the window before the next
refresh.

Example — Istio in production, proxy with its own profile, rollout via Istio
annotations, vault-agent excluded:

```yaml
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorProfile
metadata: { name: envoy }
spec:
  cpu: { quantile: 0.99, margin: 0.3, min: 20m }
  memory: { quantile: 0.99, margin: 0.25, min: 96Mi }
---
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorPolicy
metadata: { name: prod-mesh }
spec:
  priority: 10
  mode: InPlace
  apply: InPlacePreferred
  selector: { namespaces: ["prod-*"] }
  containers:
    - match: { names: ["istio-proxy"] }
      mode: InPlace
      profileRef: envoy
      proxyAnnotations: true
      memory: { max: 1Gi }
    - match: { names: ["vault-agent*"] }
      exclude: true
```

### 2.3 Actuation

**In-place** (`internal/reconciler/reconciler.go`):

- `init`/`ephemeral` are never resized.
- Native sidecar: patch on `spec.initContainers[name=…]` in the `resize` subresource
  (the Kubernetes docs list sidecars as resizable and non-restartable
  init/ephemeral containers as not resizable — see References).
- **QoS**: Guaranteed is skipped (as before; `guaranteed: resizeBoth` opts in to resizing request and limit together, see TECHNICAL.md); **BestEffort** is skipped (requests
  cannot be added); a resize that would leave **all** containers (init
  included) with requests == limits for CPU and memory — Burstable → Guaranteed — is
  skipped. Reason `qos_change`/`besteffort_qos` in `tailor_resize_skipped_total` and a
  deduplicated `ResizeSkipped` Event. The webhook applies the same rule at
  admission: a pod whose template is Guaranteed (every container, init included,
  or the pod-level resources, with requests == limits for CPU and memory) is admitted
  unmodified (`tailor_webhook_skipped_total{reason="guaranteed_qos"}`), so it is not
  born Burstable.
- **Memory limit**: never introduced on a container (app or sidecar) that has
  none, unless its effective memory model sets `enforceLimit: true` (policy,
  profile or `containers[].memory` rule).
- **Pod-level resources** (`spec.resources`): the resize is skipped if the sum of
  app + sidecar requests exceeds the pod request or if the container limit
  exceeds the pod limit (`pod_level_resources`).
- **Partial limit**: an existing limit is always raised to at least the new
  request, even without `limitFactor` (a `request > limit` is never generated).

**Template / rollout** (`apply: Rollout`, `InPlacePreferred` as a fallback, or
a cluster without `pods/resize`):

- Container in the template: patch on `containers` or `initContainers` (native).
- Container **absent from the template** (injected): there is **never** a container patch on the
  template. If it is a supported mesh proxy (`mesh` detected) **and**
  `proxyAnnotations` is on, Tailor merge-patches **only the pod template
  annotations**:
  - Istio: `sidecar.istio.io/proxyCPU`, `proxyMemory`, `proxyCPULimit`,
    `proxyMemoryLimit`;
  - Linkerd: `config.linkerd.io/proxy-cpu-request`, `proxy-memory-request`,
    `proxy-cpu-limit`, `proxy-memory-limit`.

  The Istio injection template replaces **all** of the proxy's default resources
  with those from the annotations as soon as **any one** of them exists; for that reason Tailor
  always writes the limits too (the live ones, floored at the new request) and removes the
  limit annotation only when the live proxy had no limit. Without
  `proxyAnnotations`/a supported mesh: skip `not_in_template` + Event.
- Knative: `queue-proxy` (injected) is never patched on the ksvc; a native sidecar on a
  ksvc is not either.

**Webhook** (`internal/webhook/webhook.go`): sizes `containers` and native sidecars
(`initContainers` with `restartPolicy: Always`); regular init never; raises an
existing limit that is below the new request (a patch with `request > limit` invalidates the
pod and the CREATE fails even with `failurePolicy: Ignore`); if the pod has pod-level
resources and the patch would violate them, it admits the pod **without** modification.

**Recommendation**: each container uses its **own** usage series
(`container=<name>` in VM). The proxy scales with requests/connections, not with the
app's work — hence its own profile and no app floor. The HPA by
resource utilization divides by the sum of the pod's requests, so the CPU/memory
*hold* for HPA also applies to sidecars. Native sidecar OOM now
feeds the memory floor (it reads `initContainerStatuses`).

### 2.4 Webhook × injectors (Istio, Linkerd, Vault)

- `reinvocationPolicy: IfNeeded` (chart default). The official docs say that, with
  IfNeeded, the webhook "may be called again … if the object being admitted is
  modified by other admission plugins after the initial webhook call", that the
  number of reinvocations is not guaranteed, and that reinvocable webhooks must be
  idempotent. Tailor's patch is idempotent: a container already at the
  recommended value generates no op; the indices (`/spec/containers/<i>`,
  `/spec/initContainers/<i>`) are computed on the object of **that** invocation,
  so the proxy appearing or disappearing between invocations does not produce a stale patch.
- **Order**: the official docs ("Admission Webhooks Good Practices") say that mutating
  webhooks "don't run in a consistent order" — do not depend on it. In the current
  API server implementation (`k8s.io/apiserver`,
  `pkg/admission/configuration/mutating_webhook_manager.go`), the
  `MutatingWebhookConfiguration`s are sorted **by name** (`sort.SliceStable`
  by `Name`) before running serially. For that reason the chart uses the name
  **`zz-k8s-tailor`** (`tailor.webhook.configurationName`): it sorts after
  `istio-sidecar-injector`, `istio-revision-tag-*`,
  `linkerd-proxy-injector-webhook-config` and `vault-agent-injector-cfg`, so on the
  first pass the proxy is normally already in the pod. The old name
  `k8s-tailor` already came after `istio-*`, but **before** `linkerd-*` and
  `vault-*`. Upgrading the release swaps the configuration (Helm deletes the old one).
- The Istio injector uses `reinvocationPolicy: Never` by default
  (`sidecarInjectorWebhook.reinvocationPolicy` in the istiod chart) — that is, it
  does not react to our mutation, which is what we want: Tailor only changes `resources`.

### 2.5 New API fields (`GET /api/recommendations`)

| Field | Meaning |
|---|---|
| `containerClass` | `app` \| `injected-sidecar` \| `native-sidecar` |
| `nativeSidecar` | lives in `initContainers` with `restartPolicy: Always` |
| `inTemplate` | exists in the workload template (rollout can patch it) |
| `mesh` | `istio` \| `linkerd` when the mesh injected it |
| `containerPolicy` | origin of the effective policy: `policy`, `default:injected-sidecar`, `policy:<name> containers[i]`, `profile:<name> containers[i]`, `annotation` |
| `proxyAnnotations` | durable path via proxy annotations |
| `actuationPath` | `none`, `webhook`, `in-place`, `template`, `ksvc`, `proxy-annotations:<mesh>`, `none:not-in-template`, `fallback:<…>` (joined by `+`) |

`mode` is now the **container's effective mode** (an `istio-proxy` in an
`InPlace` workload shows up as `Recommend`). `TailorPolicy` in the policies API gained
`containers` (list of rules); an upsert without `containers` preserves the rules that
already exist in the CR.

### 2.6 Remaining risks

- **Proxy resources defined outside the pod**: in Istio, a different `ProxyConfig`/
  `meshConfig.defaultConfig` or `global.proxy.resources` affects new pods;
  Tailor only knows what it sees on the live pod. With `proxyAnnotations`, the pod
  annotations take precedence over the global defaults of the injection template
  (verified in istiod's `injection-template.yaml`), but other forms
  of customization (custom injection templates) were not audited.
- **Ordering by name is an implementation detail**: if it changes, `IfNeeded` still
  covers the case, but the number of reinvocations is not guaranteed by the API.
- **In-place proxy resize × Envoy threads**: the number of Envoy worker threads is
  fixed at proxy start; Istio derives it from the CPU limit, and an in-place resize
  does not change it until the pod restarts (observed with Istio 1.31.3:
  `server.concurrency: 1` before and after a limit raise, [dev/istio/README.md](../dev/istio/README.md)).
  For large proxy CPU changes, prefer `proxyAnnotations` + rollout.
- **Throttled proxy**: a proxy pinned at a small CPU limit is the typical case of the
  [censored-CPU rule](TECHNICAL.md#censored-cpu) — its measured usage is capped at the
  limit, so the engine raises the limit by steps (only once the proxy is opted in to an
  actuating mode). The step ceiling without a policy `cpu.max` is anchored on the proxy's
  live values (it has no template), so it moves with each raise: set `cpu.max` on the
  sidecar rule.
- **Classification by absence from the template** also catches containers from an old
  template revision during a rollout (renamed ones): they fall into the safe
  default (Recommend) and are not acted on.
- **Cardinality**: each sidecar becomes one more `tailor_*{container=…}` series per
  workload.
- **QoS with init containers**: the Guaranteed calculation considers all
  containers of the live pod, like the kubelet. Pod-level resources combined with QoS
  are an edge case: if the guard disagrees with the kubelet, open an issue with the pod spec.

## References

Checked on 2026-10-21.

- Kubernetes — *Resize CPU and Memory Resources assigned to Containers*
  (<https://kubernetes.io/docs/tasks/configure-pod-container/resize-container-resources/>):
  feature *stable* since v1.35 (beta, enabled by default, in v1.33 —
  <https://v1-33.docs.kubernetes.io/docs/tasks/configure-pod-container/resize-container-resources/>);
  limitations: "Non-restartable init containers and ephemeral containers cannot be
  resized. Sidecar containers can be resized."; the original QoS class "cannot be
  changed by a resize" (Burstable cannot become Guaranteed; BestEffort cannot
  gain requests/limits); conditions `PodResizePending` (Infeasible/Deferred) and
  `PodResizeInProgress`.
- Kubernetes — *Resize CPU and Memory Resources assigned to Pods*
  (<https://kubernetes.io/docs/tasks/configure-pod-container/resize-pod-resources/>):
  pod requests ≥ sum of container requests; each container's limit ≤
  the pod's limit. (The page shows version states that are inconsistent with each other; Tailor
  simply respects the constraints.)
- Kubernetes — *Sidecar Containers*
  (<https://v1-35.docs.kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/>):
  sidecar = init container with `restartPolicy: Always`; stable since v1.33; effective
  pod request = overhead + max(sum of app + sidecars, effective init request).
- Kubernetes — *Dynamic Admission Control*, section *Reinvocation policy*
  (<https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/>)
  and *Admission Webhooks Good Practices*
  (<https://kubernetes.io/docs/concepts/cluster-administration/admission-webhooks-good-practices/>).
- `k8s.io/apiserver` `pkg/admission/configuration/mutating_webhook_manager.go`
  (master branch): `sort.SliceStable(configurations, …ByName)`.
- Istio — *Resource Annotations* (<https://istio.io/latest/docs/reference/config/annotations/>,
  version 1.31.2 page): `sidecar.istio.io/proxyCPU`, `proxyCPULimit`,
  `proxyMemory`, `proxyMemoryLimit`, `sidecar.istio.io/status`.
- Istio — `manifests/charts/istio-control/istio-discovery/files/injection-template.yaml`
  (master): the `resources` block uses only the annotations when any exists;
  `restartPolicy: Always` on istio-proxy when `nativeSidecar`
  (annotation `sidecar.istio.io/nativeSidecar`); `values.yaml`:
  `sidecarInjectorWebhook.reinvocationPolicy: Never`; configuration name
  `istio-sidecar-injector[-<rev>]`.
- Istio 1.27 change notes
  (<https://istio.io/latest/news/releases/1.27.x/announcing-1.27/change-notes/>):
  "Promoted the environment variable `ENABLE_NATIVE_SIDECARS` to default to `true`".
- Linkerd — *Proxy Configuration* (<https://linkerd.io/2-edge/reference/proxy-configuration/>):
  `config.linkerd.io/proxy-cpu-request`, `proxy-cpu-limit`, `proxy-memory-request`,
  `proxy-memory-limit`. The `linkerd.io/proxy-version` marker on the pod is **not** listed
  on that page (moderate confidence, based on the injector's known
  behavior); for that reason detection also accepts the `linkerd.io/control-plane-ns` label
  and, as a last resort, the name `linkerd-proxy` already classifies it as a sidecar (only
  writing annotations requires the marker).
- The names `linkerd-proxy-injector-webhook-config` and `vault-agent-injector-cfg`
  of the injectors' configurations were **not** verified in this review
  (moderate confidence); the ordering argument holds for any name < `zz`.
