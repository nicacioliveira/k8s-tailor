# Configuration reference

Three layers, from broadest to narrowest:

1. **Helm values** (`chart/values.yaml`): what is installed and how it is wired.
2. **Engine environment** (`tailor.env.*` in the chart, `internal/config/config.go`): the recommendation math defaults.
3. **Custom resources and annotations**: runtime behavior per workload (modes, margins, groups). These are the source of
   truth at runtime; there is no configuration database.

Install steps: [INSTALL.md](INSTALL.md). The math behind the knobs: [TECHNICAL.md](TECHNICAL.md).

Contents: [Helm values](#helm-values) · [Engine environment](#engine-environment) · [MCP environment](#mcp-environment) ·
[CRDs and annotations](#crds-and-annotations) · [Engine HTTP API](#engine-http-api) · [MCP tools](#mcp-tools)

---

## Helm values

Cluster-scoped objects have fixed names: the engine's ClusterRole and ClusterRoleBinding are `k8s-tailor-engine` (they read **and write**: `pods/resize`, workload patches, CRs, Events) and the bundled vmsingle's are `k8s-tailor-vmsingle`; there is one release per cluster.

Names below are the keys in `chart/values.yaml`; defaults are the chart defaults (verified against the file).
Everything namespaced goes into the **release namespace** (`helm install -n <ns>`);
Object names are fixed, so install **one release per cluster**.

| Key | Default | Purpose |
|---|---|---|
| `crds.install` | `true` | Render the four CRDs with the release (so upgrades apply schema changes). Set `false` when CRDs are managed out of band. |
| `crds.keep` | `true` | `helm.sh/resource-policy: keep`: `helm uninstall` leaves CRDs and every Tailor CR. |
| `secretGeneration.failWithoutLookup` | `false` | Fail the render when a generated secret cannot be reused (`helm template`, ArgoCD, Flux). Set `true` with GitOps. |
| `victoria-metrics-single.enabled` | `true` | Bundled VictoriaMetrics (official subchart). `false` = bring your own and set `tailor.env.VM_URL`. |
| `victoria-metrics-single.server.retentionPeriod` | `8d` | Must cover `WINDOW` (default 7d). |
| `victoria-metrics-single.server.persistentVolume` | `size: 10Gi`, `storageClassName` unset | TSDB volume. Never emptyDir. |
| `victoria-metrics-single.server.scrape.config` | jobs `kubelet-resource` (kubelet `/metrics/resource` via API-server proxy, every 5s), `kubelet-cadvisor` (`/metrics/cadvisor`, every 15s) and `tailor` (engine pods by pod discovery in the release namespace) | Only the two usage series, the two cAdvisor CFS counters (`container_cpu_cfs_throttled_periods_total`, `container_cpu_cfs_periods_total`, labels `namespace`/`pod`/`container`; present only for containers with a CPU limit) and the engine's own metrics. The engine job yields one series per replica (`pod` label) for `tailor_leader`, counters and refresh metrics; the recommendation gauges stay a single series (`instance="k8s-tailor:8080"`). |
| `clickhouse.enabled` | `true` | Bundled single-node ClickHouse. `false` = reuse mode via `tailor.env.CH_URL`. |
| `clickhouse.auth.{username,database}` | `tailor`, `tailor` | |
| `clickhouse.auth.{existingSecret,passwordKey,password}` | unset, `password`, unset | Precedence: `existingSecret` > inline `password` > random password generated once and reused (`lookup`), kept on uninstall. |
| `clickhouse.storage` | `10Gi` | |
| `clickhouse.resources` | requests 200m / 512Mi, limit memory 2Gi | |
| `tailor.enabled` | `true` | |
| `tailor.image.{repository,tag,digest}` | `ghcr.io/nicacioliveira/k8s-tailor`, `""` (= `v<appVersion>`), `""` | A plain `repo:tag` string is also accepted. |
| `tailor.replicas` | `1` | More than 1 requires `leaderElection.enabled` (the render fails otherwise). |
| `tailor.leaderElection.{enabled,leaseName}` | `true`, `k8s-tailor-actuator` | |
| `tailor.terminationGracePeriodSeconds` | `30` | Time for the ordered shutdown on SIGTERM: stop actuating, release the Lease, drain the API/webhook servers, flush the ClickHouse buffer. The leader waits up to 10 s for a running pass, so keep this above that. |
| `tailor.vmJob` | `kubelet-resource` | Value of the `job` label pinned on every `container_*` query (env `VM_JOB`). Must equal the scrape job that carries the kubelet series: the bundled vmsingle uses `kubelet-resource`; with your own Prometheus/VictoriaMetrics set your job name, otherwise no series matches and the engine stays **inert** (no recommendations, nothing actuated). Empty disables the filter (any job with the same labels can then feed recommendations). |
| `tailor.vmThrottleJob` | `kubelet-cadvisor` | Value of the `job` label pinned on the optional CFS throttling query (env `VM_THROTTLE_JOB`; cAdvisor `container_cpu_cfs_*` counters). The bundled vmsingle scrapes them in job `kubelet-cadvisor`; with your own Prometheus/VictoriaMetrics set its cAdvisor job. A wrong value only removes the throttling signal of the censored-CPU rule (the pinned-at-limit check still works). Empty disables the filter. |
| `tailor.rbac.clusterLookups` | `false` | Opt-in. Adds `list` on `nodes` and `limitranges` to the engine ClusterRole and sets `CLUSTER_LOOKUPS=true`: a memory request is then capped at `min(largest allocatable memory among Ready, uncordoned nodes, the namespace's LimitRange max for type Container)`. Taints, affinities and node selectors are not evaluated, a `Pod`-type LimitRange max is ignored, and the node list is re-read every 5 minutes. If the lists fail (RBAC), the engine logs it once and sizes as without the flag. The recommendation then carries `signals.memCeiling` / `signals.memCeilingCapped`. |
| `tailor.warmCache.enabled` / `.ttl` | `true` / `15m` | Env `WARM_CACHE` / `WARM_TTL`. The leader writes the recommendations the webhook can serve into the ConfigMap `k8s-tailor-warm` (release namespace) after every refresh; a starting replica loads them, so pods created right after a restart are sized instead of admitted unmodified (`reason="warming_up"`). Loaded entries serve the webhook only (never an in-place resize) and the first refresh replaces them; entries older than `ttl` are ignored. Adds a namespaced Role: `create` on `configmaps` and `get`/`update` on `k8s-tailor-warm`. See [TECHNICAL.md](TECHNICAL.md#warm-cache). |
| `tailor.disruptive.maxWait` | `15m` | Env `DISRUPTIVE_MAX_WAIT`: how long a permanently NotReady sibling holds a disruptive rolling. `0` = wait forever. After it, the rolling continues only if the live PDB allows a disruption and at least two replicas are Ready. See [TECHNICAL.md](TECHNICAL.md#disruptive-rolling-and-a-stuck-sibling). |
| `tailor.oomMemCapFactor` | `4` | Env `OOM_MEM_CAP_FACTOR`: ceiling (× observed peak) of the OOM-driven request raise for containers without a memory limit; the policy `memory.max` still applies on top. The render fails on a value `<= 0` or below `tailor.env.OOM_GROWTH_FACTOR`. See the engine table below. |
| `tailor.pdb.{enabled,minAvailable}` | `true`, `1` | Rendered only when `replicas > 1`. |
| `tailor.excludeNamespaces` | `kube-system, kube-public, kube-node-lease, istio-system, istio-ingress, istio-gateway, linkerd, linkerd-viz, cert-manager, knative-serving, knative-eventing, kourier-system, gatekeeper-system, kyverno, external-secrets, argocd, flux-system, monitoring` | The release namespace is always added. Seeds `TailorConfig` on first install only; afterwards the CR is the source of truth. |
| `tailor.admissionPolicy.{enabled,validationActions}` | `false`, `[Deny]` | Optional ValidatingAdmissionPolicies (Kubernetes >= 1.30) limiting what the engine ServiceAccount may change. Start with `[Warn, Audit]`. |
| `tailor.webhook.enabled` | `false` | Opt-in mutating webhook. |
| `tailor.webhook.configurationName` | `zz-k8s-tailor` | |
| `tailor.webhook.reinvocationPolicy` | `IfNeeded` | |
| `tailor.webhook.certMode` | `helm` | `helm` (self-signed, reused via `lookup`, **fails the render without an API server**), `certManager`, `existingSecret`. |
| `tailor.webhook.allowEphemeralCA` | `false` | CI/lint only. |
| `tailor.webhook.certManager.{issuerRef,duration,renewBefore}` | `{}`, `8760h`, `720h` | Empty `issuerRef` creates a self-signed Issuer. |
| `tailor.webhook.tls.{existingSecret,caBundle}` | unset | For `certMode: existingSecret` (`kubernetes.io/tls` Secret, base64 PEM CA). |
| `tailor.actuateNamespaces` | `""` | Comma-separated namespaces where the webhook sizes pods at creation. Empty = no `MutatingWebhookConfiguration` is created. |
| `tailor.allowedHosts` | `[]` | `Host` names the engine accepts for **browser writes** (DNS-rebinding guard on top of the same-origin check), e.g. `[tailor.example.com]` when the engine UI is exposed. `[]` = any Host with `apiAuth` on; loopback names only with it off. Sets `TAILOR_ALLOWED_HOSTS`. |
| `tailor.apiAuth.enabled` | `true` | Bearer required on mutating `/api/*`; fails closed without a token. |
| `tailor.apiAuth.{token,existingSecret,secretKey}` | unset, unset, `token` | Precedence: `existingSecret` > inline > generated once into `k8s-tailor-api-auth`. |
| `tailor.apiAuth.protectReads` | `false` | `true` = `GET/HEAD /api/*` need the same Bearer (`TAILOR_API_PROTECT_READS`). `/healthz`, `/livez`, `/metrics` and the UI shell stay open. Requires `apiAuth.enabled` (the render fails otherwise). |
| `tailor.presets.enabled` | `false` | Installs the built-in `TailorProfile`s (see [Presets](#presets)). |
| `tailor.advisor.*` | `enabled: false`, `language: en`, `timeout: 30s`, `maxCallsPerHour: 60`, `tokenBudgetPerHour: 0`, `cacheTTL: 15m`, `maxTokens: 800`, `responseFormat: json_schema` | `baseURL` and `model` are required when enabled; the API key comes only from `existingSecret`/`secretKey` (default `api-key`). `anonymize: true` sends `ns-1/workload-1` tokens. See [ADVISOR.md](ADVISOR.md). |
| `tailor.podAnnotations` | `prometheus.io/scrape: "true"`, `port: "8080"`, `path: /metrics` | For a fleet vmagent. `{}` keeps metrics cluster-local. |
| `tailor.resources` | requests 50m / 128Mi, limit memory 256Mi | |
| `mcp.enabled` | `true` | MCP server + dashboard. |
| `mcp.image.*` | `ghcr.io/nicacioliveira/k8s-tailor-mcp`, tag `v<appVersion>` | |
| `mcp.allowedHosts` | `[]` | `Host` names the MCP server accepts (DNS-rebinding guard), e.g. `[tailor.example.com]`. `[]` = any Host while a token is enforced; loopback names only when the MCP is open (dev). Sets `MCP_ALLOWED_HOSTS`. |
| `mcp.allowedOrigins` | `[]` | Extra browser `Origin`s accepted on `POST /mcp` besides the page's own origin. Sets `MCP_ALLOWED_ORIGINS`. |
| `mcp.auth.enabled` | `true` | Adds the deny-by-default nginx sidecar (only `/`, `/index.html`, `/healthz`, `/readyz` open; everything else, including `/mcp`, needs `Bearer`). The Bun server binds `127.0.0.1` **and also validates the same token** (it always does, with or without the sidecar). |
| `mcp.auth.{token,existingSecret,secretKey}` | unset, unset, `token` | Precedence: `existingSecret` > inline > generated once into `k8s-tailor-mcp-auth`. The token may only contain `A-Z a-z 0-9 . _ ~ + / = -` (RFC 6750 token68): the render fails otherwise, and the sidecar and the Bun server refuse to start on an `existingSecret` with other characters. `openssl rand -hex 32` is a safe generator. |
| `mcp.auth.allowUnauthenticated` | `false` | With `mcp.auth.enabled=false` only: no token at all (`MCP_ALLOW_UNAUTHENTICATED=true`). Local dev over port-forward only. |
| `mcp.auth.nginxImage` | `nginxinc/nginx-unprivileged:1.27-alpine` | |
| `networkPolicy.enabled` | `true` | Needs a CNI that enforces NetworkPolicy. |
| `networkPolicy.mcpFrom` | `[]` | Peers allowed to reach the MCP/UI besides the release namespace (your ingress controller). |
| `networkPolicy.metricsFrom` | `[]` | Extra peers allowed to reach the engine `:8080` **and** the bundled vmsingle `:8428` (a fleet Prometheus/vmagent, Grafana). vmsingle has no authentication and accepts writes: keep this narrow. |
| `networkPolicy.webhookFrom` | `[]` | Who may call the admission webhook `:8443` (only with `tailor.webhook.enabled`): the kube-apiserver's source addresses, e.g. `- ipBlock: { cidr: 10.0.0.0/24 }`. `[]` = any source, so a forged AdmissionReview is answered. |
| `global.imagePullSecrets` | `[]` | Pull secrets applied to every pod (engine, MCP, ClickHouse, VictoriaMetrics subchart). Needed when the GHCR packages are private; see [UPGRADE.md#images](UPGRADE.md#images). |
| `serviceMonitor.{enabled,interval,scrapeTimeout,labels}` | `false`, `30s`, `10s`, `{}` | Needs the `monitoring.coreos.com` CRDs. |

## Engine environment

Set through `tailor.env.<NAME>` in the chart (strings; quote numbers). Chart defaults are in the third column when they differ from the engine's own default.

| Variable | Engine default | Chart default | Meaning |
|---|---|---|---|
| `SOURCE` | `in-cluster` | `in-cluster` | `local` uses your kubeconfig (dev loop). |
| `VM_URL` | `http://127.0.0.1:8428` | bundled vmsingle in the release namespace | Prometheus-compatible query endpoint. Set only in reuse mode. |
| `VM_JOB` | `kubelet-resource` | `kubelet-resource` (`tailor.vmJob`) | `job` label added to every `container_*` query (recommendation refresh, charts, cluster usage, `backtest -vm-job`). Set but empty = no filter. Wrong value with a custom Prometheus = engine inert; see [TROUBLESHOOTING.md](TROUBLESHOOTING.md). The filter does not block direct writes to `/api/v1/import`. |
| `VM_THROTTLE_JOB` | `kubelet-cadvisor` | `kubelet-cadvisor` (`tailor.vmThrottleJob`) | `job` label added to both series of the optional CFS throttling query (`container_cpu_cfs_throttled_periods_total` / `container_cpu_cfs_periods_total`). Set but empty = no filter. A wrong value means no throttling signal, never a wrong action. |
| `PORT`, `TLS_PORT` | `8080`, `8443` | | API/UI and webhook ports. |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | `/etc/webhook/tls/tls.{crt,key}` | | Webhook certificate; hot-reloaded. |
| `CH_URL` | empty (in-memory ring of 200) | bundled ClickHouse `:8123` | |
| `CH_DATABASE`, `CH_USER` | `tailor`, empty | `tailor`, `tailor` | |
| `CH_PASSWORD` | empty | injected from the ClickHouse Secret | In reuse mode with no secret, set it in `tailor.env`. |
| `CH_RETENTION_DAYS` | `90` | `90` | TTL of the `decisions` table. |
| `WINDOW` | `7d` | `7d` | Usage history used for each recommendation. Accepts `d` and `w` suffixes. |
| `CPU_QUANTILE`, `MEM_QUANTILE` | `0.90`, `0.95` | same | Defaults when a policy does not set `quantile`. |
| `CPU_RATE_MODE` | `avg` | `avg` | `max` uses `rollup_rate(..., "max")` (VictoriaMetrics only). |
| `MARGIN` | `0.15` | `0.15` | Default headroom. |
| `HALF_LIFE` | `24h` | `24h` | Decay half-life. |
| `REFRESH_EVERY` | `60s` | `60s` | Recommendation refresh interval. |
| `RECONCILE_EVERY` | `30s` | | Reconcile interval (not set by the chart). |
| `TREND_WINDOW`, `TREND_STEP_SEC` | `3h`, `60` | | Dashboard chart window and step. |
| `MEM_LIMIT_FACTOR`, `CPU_LIMIT_FACTOR` | `1.0`, `0` | `1.0`, `0` | Defaults when a policy does not set `limitFactor`. |
| `MIN_CPU_MILLI`, `MIN_MEM_MI` | `10`, `32` | | Floors for any recommendation. |
| `MIN_SAMPLES` | `30` | **`288`** | Distinct 300 s scrape points required before a resource may be actuated. 288 = 24 h, one full daily cycle: with fewer samples the engine could shrink requests after seeing only part of the cycle (for example only the overnight trough). `dev/k3s` uses `2` (about 10 minutes) so demos converge quickly; do not use that in production. |
| `OOM_LEARN` | `true` | `"true"` | Disable with `false`. |
| `OOM_GROWTH_FACTOR` | `1.5` | `"1.5"` | |
| `OOM_FLOOR_TTL` | `30d` | `30d` | |
| `WARM_CACHE` | `true` | `true` (`tailor.warmCache.enabled`; `tailor.env.WARM_CACHE` wins) | `false` stops the leader writing and replicas loading the warm-cache ConfigMap. Needs `POD_NAMESPACE` (the chart sets it); without it the cache is off. |
| `WARM_TTL` | `15m` | `15m` (`tailor.warmCache.ttl`) | How long a saved cache may be loaded and served. `0` = never load it (a negative value stops the engine at boot). |
| `DISRUPTIVE_MAX_WAIT` | `15m` | `15m` (`tailor.disruptive.maxWait`; `tailor.env.DISRUPTIVE_MAX_WAIT` wins) | Bound on the wait for a NotReady sibling during a disruptive rolling; `0` waits forever, a negative value stops the engine at boot. |
| `OOM_MEM_CAP_FACTOR` | `4` | `4` (`tailor.oomMemCapFactor`; `tailor.env.OOM_MEM_CAP_FACTOR` wins) | Ceiling of the OOM-driven memory **request** raise for a container **without** a memory limit (node-pressure kill): the request never goes above `observed peak × max(OOM_MEM_CAP_FACTOR, OOM_GROWTH_FACTOR)`, then never above the policy `memory.max` (or a per-container `max-mem` annotation): the lower of the two wins. Without a memory series (peak 0) only `memory.max` applies. Does not affect containers that have a limit (the OOM floor raises their limit). Must be finite and `>= OOM_GROWTH_FACTOR`: the engine refuses to start otherwise (an unparsable value is an error, not a silent default) and the chart render fails on a value `<= 0` or below `tailor.env.OOM_GROWTH_FACTOR`. |
| `CPU_LIMIT_GROWTH_FACTOR` | `1.5` | | Censored CPU (usage pinned at the CPU limit, or CFS throttling when the cAdvisor series exist): the existing CPU **limit** is raised by this factor per step and the request lifted to at least the live limit ([TECHNICAL.md](TECHNICAL.md#censored-cpu)). `1` disables the limit raise. Must be finite and `>= 1` (the engine refuses to start otherwise; an unparsable value is an error). Not set by the chart. |
| `SEASON_WINDOW` | `14d` | | History fetched, only for namespaces with a `weekly`/`both` policy, for the seasonality gate and forecast, so a weekly-only pattern is judged on real week-lag pairs while `WINDOW` stays `7d`. `0` = off; at most `28d` (the engine refuses to start above it). Not set by the chart (use `tailor.env`). |
| `SEASON_REFRESH` | `1h` | | How long that extra history is reused per namespace before it is fetched again (one more query pair per namespace per interval). |
| `AUTO_QUANTILE` | `false` | | Engine-wide default of the policy field `autoQuantile` (a policy overrides it): raise the CPU quantile to 0.95 for a burstiness index at or above `BURST_THRESHOLD`. Not set by the chart (use `tailor.env`). |
| `BURST_THRESHOLD` | `3` | | Engine-wide default of `burstThreshold`: the `p99 / p90` ratio that triggers `autoQuantile`. Must be a finite number `> 1`; the engine refuses to start otherwise. |
| `CPU_CENSOR_MAX_STEPS` | `3` | | Convergence of the censored-CPU rule: after this many consecutive raises with the usage still pinned at the new limit the limit is parked and the container is marked `cpuGreedy` (until usage falls below 80% of the limit). Integer `>= 1`; the engine refuses to start otherwise. Not set by the chart (use `tailor.env`). |
| `CPU_CENSOR_MAX_FACTOR` | `8` | | Ceiling of that raise when the policy sets no `cpu.max`: the template CPU limit × this factor (a policy `cpu.max` replaces it). Must be finite and `>= 1`. Not set by the chart. |
| `PRICING_MODE` | `static` | | `static`, `opencost`, `aws` (stub; falls back to static). |
| `PRICE_CPU_CORE_HR`, `PRICE_GB_HR` | `0.031`, `0.004` | same | Static prices (USD). |
| `EXCLUDE_NAMESPACES` | built-in list | rendered from `tailor.excludeNamespaces` plus the release namespace | |
| `POD_NAMESPACE`, `POD_NAME` | | set by the chart | The engine's own namespace is always excluded. |
| `TAILOR_API_TOKEN` | | from the API-auth Secret | |
| `TAILOR_API_AUTH_REQUIRED` | `false` | `true` when `apiAuth.enabled` | |
| `TAILOR_API_PROTECT_READS` | `false` | `true` when `apiAuth.protectReads` | `GET/HEAD /api/*` need the token too; 503 when no `TAILOR_API_TOKEN` is set. |
| `LEADER_ELECT`, `LEADER_LEASE_NAME` | `false`, `k8s-tailor-actuator` | `true` when enabled | |
| `ADVISOR_*` | disabled | from `tailor.advisor` | See [ADVISOR.md](ADVISOR.md). |

## MCP environment

| Variable | Meaning |
|---|---|
| `PORT` | default `8020` |
| `MCP_HOST` | bind address. The chart sets `127.0.0.1` behind the nginx sidecar, otherwise `0.0.0.0`. |
| `MCP_API_TOKEN` | Bearer enforced by the Bun server **always**, with or without the nginx sidecar (defense in depth; constant-time compare of hashes). The server **refuses to start without it**, even on loopback, unless `MCP_ALLOW_UNAUTHENTICATED=true`. Allowed characters: `A-Z a-z 0-9 . _ ~ + / = -`. The chart injects the same token into the sidecar and Bun. |
| `MCP_ALLOW_UNAUTHENTICATED` | `true` = no token at all, accepted only with loopback-only `Host`s. Dev only. |
| `MCP_ALLOWED_HOSTS`, `MCP_ALLOWED_ORIGINS` | Comma-separated `Host` allowlist (from `mcp.allowedHosts`) and extra browser `Origin`s (from `mcp.allowedOrigins`). |
| `TAILOR_API`, `TAILOR_API_TOKEN` | engine base URL (`http://k8s-tailor.<ns>.svc:8080`) and the write token the MCP forwards. |

## CRDs and annotations

Cluster-scoped, `tailor.k8s-tailor.dev/v1alpha1`. Short names: `rscfg` (TailorConfig), `rsp` (TailorPolicy), `rsprof` (TailorProfile), `tgrp` (TailorGroup).

```yaml
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorPolicy
metadata: { name: apps }
spec:
  priority: 200                      # highest matching priority wins; no merging between policies
  selector:
    namespaces: ["portal", "apps-*"]   # exact names or shell globs
    matchLabels: { tier: web }       # optional
  mode: InPlace                      # Off | Recommend | Webhook | InPlace | Both
  apply: InPlacePreferred            # InPlace | Rollout | InPlacePreferred
  direction: Down                    # Both | Down | Up
  # guaranteed: resizeBoth           # opt-in: skip (default) | resizeBoth, for containers with request == limit
  season: daily                      # daily | weekly | both | off (weekly fetches SEASON_WINDOW of history, see below)
  seasonGate: { minCorrelation: 0.5, minPairs: 24 }   # optional; these are the engine defaults
  halfLife: 24h
  cpu:    { quantile: 0.90, margin: 0.15, min: 10m,  max: "4" }
  memory: { quantile: 0.95, margin: 0.15, min: 64Mi, max: 8Gi, limitFactor: 1.0 }
  # memory.enforceLimit: true        # opt-in: give a memory limit to containers running without one
  scaleUp:   { cooldown: 30s }
  scaleDown: { cooldown: 1h, stabilization: 15m }
  schedule:                          # optional; outside the windows, outsideMode applies (default Off)
    timezone: UTC
    outsideMode: Recommend
    windows: [{ days: [Mon, Tue, Wed, Thu, Fri], start: "09:00", end: "18:00" }]
  containers:                        # per-container rules, first match wins
    - match: { names: ["vault-agent*"] }
      exclude: true
    - match: { classes: ["injected-sidecar"] }
      mode: InPlace                  # only an explicit mode lifts the sidecar Recommend cap
      proxyAnnotations: true
```

Fields that change safety or detection behavior:

- `guaranteed` (`skip` | `resizeBoth`, default `skip`; on policies and `containers[]` rules, or the container annotation `.guaranteed`): what to do with a
  container whose request equals its limit (a Guaranteed pod). `resizeBoth` patches request and limit to the same value so the pod keeps its QoS
  class; CPU grows and shrinks, memory only grows. See [TECHNICAL.md](TECHNICAL.md#guaranteed-pods).
- `memory.enforceLimit` (bool, default `false`; on policies, profiles and `containers[].memory` rules): when `true`, a container running
  **without** a memory limit gets one (`request × limitFactor`, never below the observed peak × (1 + margin) nor the request) from the
  webhook, the in-place resize and the rollout. Off, the engine never introduces a memory limit (an OOMKill of an unbounded container raises
  the request instead). See [TECHNICAL.md](TECHNICAL.md#enforce-limit).
- `seasonGate.minCorrelation` (0 < x <= 1, default 0.5) and `seasonGate.minPairs` (2..10000, default 24): the seasonality gate. The seasonal
  forecast is used only when the hourly usage profile correlates with itself one season earlier at least this much, over at least this many
  hourly pairs. A week-lag correlation needs more than 7 days of history (14
  cover every hour of the week), so for namespaces with a `season: weekly` (or `both`) policy the engine fetches `SEASON_WINDOW` (default `14d`) of
  history for the seasonality gate and forecast only; the quantile, the peak and the change-point keep `WINDOW` (default `7d`). The extra query
  runs once per namespace per `SEASON_REFRESH` (default `1h`), only where such a policy exists, and `SEASON_WINDOW` is capped at 28 days; `0`
  switches it off, and a value not longer than `WINDOW` adds nothing. Without it (off, or the query failing) the 7 days have no week-lag pairs, the
  daily correlation is used as a proxy and a weekly-only pattern (a Monday peak on otherwise flat days) is rejected.

**Bursty workloads.** Short CPU bursts a few % of the time sit above a P90 request by construction. Size such a workload from a higher CPU
quantile, `cpu.quantile: 0.95`, without touching the rest of the fleet: per policy (inline `cpu: { quantile: 0.95 }` or `profileRef: bursty`
with the preset below), per workload (annotation `tailor.k8s-tailor.dev/profile: bursty`, any TailorProfile whose `cpu.quantile` is 0.95),
or per container (`containers[]` rule with `cpu: { quantile: 0.95 }` or `profileRef: bursty`, or the annotation
`tailor.k8s-tailor.dev/container.<name>.profile: bursty`). Backtest `bursty` (seeds 1-5): share of samples with CPU above the request
7.8% → 3.7% at about the KRR reference cost ([BACKTEST.md](BACKTEST.md)).

**Automatic detection (`autoQuantile`).** Instead of choosing the quantile by hand, set `autoQuantile: true` on a policy (default `false`; engine-wide
default `AUTO_QUANTILE`): a container whose **burstiness index** reaches `burstThreshold` (default 3, `BURST_THRESHOLD`) is sized from the 0.95 quantile
instead of the configured one (a quantile already at 0.95 or more is kept). The index is `p99 / p90` of the CPU series over the window, all replicas
pooled, defined once 12 hours of samples exist. It is **always** computed and shown, with or without the opt-in: `signals.burstiness`,
`signals.cpuQuantileUsed`, `signals.burstQuantileRaised` and the plain-language `signals.burstReason` in the API, and in the workload sheet.
On the backtest scenarios the index is 6.5-9.0 for `bursty`, about 1.8 for `heterogeneous`, about 1.2 for `diurnal-weekly` and at most 1.2 for the others, so
the default threshold separates them with margin ([TECHNICAL.md](TECHNICAL.md#burstiness-and-autoquantile)).

Annotations on a workload (Deployment, StatefulSet, DaemonSet; on the Pod for bare Pods) in the `tailor.k8s-tailor.dev/` namespace:

| Annotation | Effect |
|---|---|
| `mode` | `Off|Recommend|Webhook|InPlace|Both`. Beats any policy. |
| `ignore: "true"` | Absolute `Off`; no mode annotation or container rule can re-enable it. |
| `profile` | Name of a `TailorProfile` to apply. |
| `container.<name>.mode` / `.exclude` / `.profile` / `.apply` / `.direction` / `.guaranteed` / `.min-cpu` / `.max-cpu` / `.min-mem` / `.max-mem` / `.proxy-annotations` | Per-container overrides (highest precedence for that container). |
| `last-applied`, `oom-floor` (also on a Knative Service), `resize-failed` and `rolling` (pods), `group-oom-floor` (TailorGroup CR) | Written by the engine (state that survives a restart); do not edit. |

Useful commands:

```bash
kubectl get tailorconfig cluster -o yaml          # global pause, default mode, exclusions
kubectl get rsp,rsprof,tgrp                       # policies, profiles, groups
kubectl patch tailorconfig cluster --type=merge -p '{"spec":{"paused":true}}'   # master kill switch
kubectl -n portal annotate deploy/gateway tailor.k8s-tailor.dev/mode=Off --overwrite
```

### Presets

`tailor.presets.enabled=true` installs built-in `TailorProfile`s (cluster-scoped). A policy uses one
with `profileRef`:

| Preset | Spec | When |
|---|---|---|
| `bursty` | `cpu.quantile: 0.95` (everything else inherited) | Spiky CPU: short bursts a few % of the time. With the default P90 the request sits at the base load and the bursts land above it; P95 cuts the share of samples above the request (backtest `bursty`: 7.8% → 3.7%) for a slightly larger request. |

```yaml
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorPolicy
metadata: { name: spiky-apis }
spec:
  mode: Recommend
  profileRef: bursty
  selector: { namespaces: ["gateway"] }
```

Resolution order: the policy's own defaults, then the profile, then the policy's inline `cpu`/`memory`
fields (an inline `cpu.quantile` beats the preset's). A field the profile leaves out (or sets to 0)
keeps the inherited value. Per-container rules can use it too (`containers[].profileRef: bursty`).
The preset is applied as a Helm post-install/post-upgrade hook (its CRD is a template of the same
release), so setting `enabled=false` later does not delete it: `kubectl delete tailorprofile bursty`.

## Engine HTTP API

On `:8080` of Service `k8s-tailor` (the MCP forwards to it). Reads are open unless `tailor.apiAuth.protectReads=true` (`TAILOR_API_PROTECT_READS=true`): then every `GET /api/*` needs the same `Authorization: Bearer <token>` (401 without it; 503 if the engine has no token), while `/healthz`, `/livez`, `/metrics` and the UI shell stay open. Writes need `Authorization: Bearer <token>` and `Content-Type: application/json`.

| Route | Auth | Purpose |
|---|---|---|
| `GET /healthz` | open | JSON: `ok`, `source`, `actuation`, `vmUrl`, `vmHealthy`. Readiness probe. |
| `GET /livez` | open | `ok`; touches no dependency. Liveness probe. |
| `GET /api/recommendations` | open | All container recommendations, totals, per-workload applied counts. Each recommendation carries `confident` (`cpuConfident || memConfident`) and the per-resource `cpuConfident` / `memConfident`: a resource is only actuated on its own coverage (`MIN_SAMPLES` distinct scrape points). |
| `GET /api/overview?from&to&step` | open | Cluster totals and usage-versus-reserved series, `paused`, `inPlaceSupported`, pricing basis. |
| `GET /api/detail/{ns}/{kind}/{workload}/{container}` | open | Recommendation, usage and recommended/current series, applied events. |
| `GET /api/patch/{ns}/{kind}/{workload}/{container}` | open | Dry-run: the exact in-place and rollout patch bodies. Never mutates. |
| `GET /api/actuations?limit&q&from&to` | open | Applied decisions (ClickHouse when configured, else ring buffer). |
| `GET /api/economy` | open | Savings series (last 6 h) and pricing basis. |
| `GET /api/policies`, `GET /api/groups`, `GET /api/groups/{name}/members`, `GET /api/profiles` | open | |
| `POST /api/pause` `{"paused":bool}` | Bearer | Global kill switch (writes `TailorConfig`). |
| `POST /api/policies` | Bearer | Create/merge a TailorPolicy (only the sent keys change). 400 with the reason on invalid input. |
| `DELETE /api/policies/{name}` | Bearer | |
| `POST /api/groups`, `DELETE /api/groups/{name}` | Bearer | |
| `POST /api/workload` `{namespace,kind,workload,mode?,profile?}` or `{namespace,kind,workload,container,mode?,exclude?}` | Bearer | Writes the per-workload annotations, or with `container` the per-container ones. See [below](#post-apiworkload). |
| `GET /api/advisor/status`; `POST /api/advisor/review` `{namespace,kind,workload,container?}`; `POST /api/advisor/summary` `{namespace?,limit?}` | status open, POSTs Bearer | Optional advisor. 404 `disabled` when off; 429 on call/token limits; 503 on an open breaker; 400 on bad input; 502 on provider or invalid-response errors. |
| `GET /metrics` | open | Prometheus metrics ([TECHNICAL.md](TECHNICAL.md#13-observability)). |
| `GET /` | open | The embedded dashboard. |

### `POST /api/workload`

Writes annotations on the workload object (Deployment, StatefulSet, DaemonSet or bare Pod; `ksvc` and `group` rows answer `400 unsupported_kind`), exactly
the ones a `kubectl annotate` would set; the next refresh resolves them (a refresh is triggered at once). Request body limit: 1 MiB (`413`).

Workload level (no `container`; unchanged contract, plus mode validation):

```json
{"namespace": "portal", "kind": "Deployment", "workload": "gateway", "mode": "InPlace", "profile": "conservative"}
```

- `mode` writes `tailor.k8s-tailor.dev/mode`: `Off|Recommend|Webhook|InPlace|Both`; `""` or `"inherit"` removes it; anything else is `400 invalid_mode`.
- `profile` writes `tailor.k8s-tailor.dev/profile`; `""` removes it. Omitted fields are left unchanged.
- Response: `{"ok": true}`.

Container level (`container` set):

```json
{"namespace": "portal", "kind": "Deployment", "workload": "gateway", "container": "istio-proxy", "mode": "InPlace", "exclude": false}
```

- `mode` writes `tailor.k8s-tailor.dev/container.<container>.mode` with the same values and validation; `""` / `"inherit"` removes it (the container
  falls back to the policy, the policy `containers` rules and the class default).
- `exclude: true` writes `tailor.k8s-tailor.dev/container.<container>.exclude: "true"` (container never sized or actuated); `false` removes it.
- At least one of `mode` / `exclude` is required (`400 nothing_to_change`); `profile` is refused (`400 bad_request`).
- An injected sidecar stays capped at `Recommend` until opted in: an explicit container `mode` **is** that opt-in.
- The container must have been seen in the last refresh (template, or the running pods for injected sidecars) and must be resizable: init and
  ephemeral containers answer `400 container_not_resizable`. An excluded container stays addressable, so it can be re-included.
- Response: `{"ok": true, "namespace": "portal", "kind": "Deployment", "workload": "gateway", "container": "istio-proxy", "mode": "InPlace", "exclude": false}`
  (`mode` and `exclude` only when sent; a cleared mode is echoed as `"inherit"`).

Errors are `{"error": "<message>", "code": "<code>"}`:

| Status | `code` | When |
|---|---|---|
| 400 | `bad_request` | Invalid JSON, missing `namespace`/`kind`/`workload`, `exclude` without `container`, `profile` with `container` |
| 400 | `invalid_mode` | `mode` not in `Off\|Recommend\|Webhook\|InPlace\|Both\|inherit\|""` (nothing is written) |
| 400 | `nothing_to_change` | `container` with neither `mode` nor `exclude` |
| 400 | `unsupported_kind` | Kind other than Deployment, StatefulSet, DaemonSet, Pod |
| 400 | `container_not_resizable` | Init or ephemeral container |
| 404 | `workload_not_found` | Workload not in the last snapshot (container write) or not in the cluster |
| 404 | `container_not_found` | Container not seen on the workload in the last refresh |
| 401 / 503 / 403 / 415 / 413 | (body without `code`) | Shared write guards: token, auth required without token, cross-site, Content-Type, size |

```bash
TOKEN=$(kubectl -n k8s-tailor get secret k8s-tailor-api-auth -o jsonpath='{.data.token}' | base64 -d)
# through a port-forward to the engine: kubectl -n k8s-tailor port-forward svc/k8s-tailor 8080:8080
curl -s -X POST localhost:8080/api/pause -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"paused":true}'
```

## MCP tools

`POST /mcp` (JSON-RPC 2.0, protocol version `2025-06-18`), Bearer-gated. Tools (`mcp/server/tools.ts`):
read-only: `TAILOR_OVERVIEW` (also opens the dashboard UI), `TAILOR_LIST_RECOMMENDATIONS`, `TAILOR_GET_DETAIL`,
`TAILOR_LIST_ACTUATIONS`, `TAILOR_PREVIEW_PATCH`, `TAILOR_ECONOMY`, `TAILOR_HEALTH`, `TAILOR_ADVISOR_STATUS`, `TAILOR_ADVISOR_REVIEW`,
`TAILOR_ADVISOR_SUMMARY`, `TAILOR_LIST_POLICIES`, `TAILOR_LIST_GROUPS`, `TAILOR_GROUP_MEMBERS`, `TAILOR_LIST_PROFILES`;
writes (need the engine token, which the MCP forwards): `TAILOR_SET_PAUSED`, `TAILOR_SET_POLICY`, `TAILOR_DELETE_POLICY`,
`TAILOR_SET_GROUP`, `TAILOR_DELETE_GROUP`, `TAILOR_SET_WORKLOAD`. Each tool advertises `readOnlyHint` (true for the read-only list above, false for the writes).

**MCP Apps.** The dashboard is also published as an MCP App, the open MCP extension SEP-1865: a `ui://` resource served as `text/html;profile=mcp-app` (SDK `@modelcontextprotocol/ext-apps`, MIT). Each tool that opens it carries `_meta.ui.resourceUri` (the legacy flat `ui/resourceUri` key is kept for older hosts); hosts without MCP Apps support get the plain text result.
