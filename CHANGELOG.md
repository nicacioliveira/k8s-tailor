# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the project is pre-1.0,
minor versions may contain breaking changes; they are called out under "Changed" and in
[docs/UPGRADE.md](docs/UPGRADE.md).

## [Unreleased]

First public version (`0.1.0`, Helm chart `0.2.0`), not yet tagged or published. Nothing here has been
released: the container images and the chart are published only when a `v*` tag is pushed.

### Added

- **Recommendation engine** (Go): per-container CPU and memory recommendations from usage history
  (percentile-based, with change-point detection, a seasonality gate, anti-flapping stabilization and
  an OOM-kill memory floor).
- **In-place resize** through the Kubernetes `pods/resize` subresource (Kubernetes 1.33+), without
  restarting pods, plus a mutating admission webhook that sizes new pods at creation, and an opt-in
  disruptive rollout path for workloads that cannot be resized in place.
- **Safety model**: ships inert (no actuation until a policy enables it), per-workload modes, bounds,
  PodDisruptionBudget and HPA awareness, pause switch, leader election and Kubernetes Events for
  every applied or skipped change.
- **CRDs** (`v1alpha1`): `TailorPolicy` and `TailorProfile` for namespaced and cluster-wide settings.
- **Multi-container support**: sidecars, init containers and mesh proxy sizing annotations
  (Istio and Linkerd), see [docs/CONTAINERS.md](docs/CONTAINERS.md).
- **Dashboard and MCP server** (Bun, React): fleet overview, recommendations, decisions, policies and
  an MCP endpoint for AI agents, gated by two independent Bearer-token checks.
- **Offline backtest** CLI (`k8s-tailor backtest`) that replays usage series against KRR-style,
  VPA-style and static baselines, see [docs/BACKTEST.md](docs/BACKTEST.md).
- **Optional LLM advisor**: read-only second opinion over an OpenAI-compatible endpoint, off by
  default, see [docs/ADVISOR.md](docs/ADVISOR.md).
- **Observability**: Prometheus metrics, optional ServiceMonitor, bundled VictoriaMetrics, a Grafana
  dashboard.
- **Helm chart** with NetworkPolicies, an opt-in ValidatingAdmissionPolicy that confines the engine's
  writes to resource fields, and generated API and MCP tokens.
- **Local environment** based on k3d/k3s (`make k3s-up`) and demo workloads in `demo/`.
- **`guaranteed: resizeBoth`** (opt-in, on policies, `containers[]` rules and the container annotation): resizes a Guaranteed
  container's request and limit together so the pod keeps its QoS class; CPU grows and shrinks, memory only grows
  (`tailor_resize_skipped_total{reason="guaranteed_mem_shrink"}`, same reason on `tailor_webhook_skipped_total`).
- **`tailor.rbac.clusterLookups`** (opt-in, default off): adds `list` on `nodes` and `limitranges` to the engine role and caps a
  memory request at the largest eligible node's allocatable memory and the namespace LimitRange max (`CLUSTER_LOOKUPS`).
- **`disruptive.maxWait`** (chart `tailor.disruptive.maxWait`, env `DISRUPTIVE_MAX_WAIT`, default 15m, `0` = forever): a permanently
  NotReady sibling no longer holds a disruptive rolling indefinitely; after the wait it proceeds only when the live PDB and replica reads
  allow it (`tailor_disruptive_wait_expired_total`).
- **History of replaced grouped pods**: `ksvc` and `group` pods are remembered while alive, so their usage series stays attributed
  to the unit for one `WINDOW` after the pod is gone (in memory; not persisted).
- **Knative desired count**: the disruptive rolling of a `ksvc` compares Ready replicas with the sum of `spec.replicas` over the
  Deployments Knative creates for its revisions, so it holds while the autoscaler's desired count is not Ready yet.
- **CPU-greedy convergence** (`CPU_CENSOR_MAX_STEPS`, default 3): a container that keeps eating every CPU limit raise is parked at the
  last step and marked `cpuGreedy` (API, `tailor_cpu_greedy`) until its usage falls below 80% of the limit; new backtest scenario `cpu-greedy`.
- **Burstiness index and `autoQuantile`**: every recommendation exposes `signals.burstiness` (p99 / p90 of the CPU window),
  `cpuQuantileUsed` and a plain-language `burstReason`. The opt-in policy field `autoQuantile` (`AUTO_QUANTILE`) raises the CPU
  quantile to 0.95 when the index reaches `burstThreshold` (`BURST_THRESHOLD`, default 3).
- **Weekly seasonality on a 7-day window**: for namespaces with a `weekly` or `both` policy the engine fetches `SEASON_WINDOW`
  (default 14d, at most 28d, reused for `SEASON_REFRESH`, default 1h) for the seasonality gate and forecast only, so a weekly-only pattern is
  judged on real week-lag pairs without raising `WINDOW`.
- **Warm cache** (`tailor.warmCache`, `WARM_CACHE`/`WARM_TTL`): the leader writes the webhook-servable recommendations to the
  ConfigMap `k8s-tailor-warm` after each refresh and a starting replica loads them, so pods created right after a restart are sized
  instead of admitted unmodified. Loaded entries serve the webhook only (never an in-place resize) until the first real refresh.
  Adds a namespaced Role (configmaps create; get/update on that one name).
- **Backtest of the disruptive paths**: `reconciler.PlanRollout` and `reconciler.RollingGate` are extracted from the reconciler and
  replayed in simulated time; the new scenario `restart-rolling` (containers that restart on resize, 3 replicas, a PDB) reports
  restarts per day and the most replicas not Ready for KRR-style, VPA-style, the in-place engine and the rollout path (`k8s-tailor-rollout`).
- **`chart/values-vmsingle-chown.yaml`** (opt-in overlay): an init container that chowns the vmsingle volume to 65534 with only
  `CAP_CHOWN`, for provisioners that ignore `fsGroup`.
- **Refresh against a dead metrics backend**: a refresh in which every namespace query fails is now an error that keeps the previous
  recommendations; it used to report success with none and replace them.
- **Documentation**: install guide, LLM install runbook, architecture (Mermaid), technical reference,
  configuration reference, troubleshooting, known issues and roadmap.
- **CI** (`ci.yml`), multi-arch image and chart publishing on version tags (`build-images.yml`) and
  GitHub Release creation (`release.yml`).
- **Censored-CPU handling**: usage pinned at a CPU limit (at least a quarter of the last hour's samples at
  >= 90% of it, or, when cAdvisor series exist, a quarter of CFS periods throttled) grows the existing limit
  by `CPU_LIMIT_GROWTH_FACTOR` (1.5) per step, bounded by the policy `cpu.max` or the template limit x
  `CPU_CENSOR_MAX_FACTOR` (8). It passes the drift gate (`Plan.CPULimitRaise`) and never applies under a
  shrink-only policy, an HPA hold or without CPU confidence. See [docs/TECHNICAL.md](docs/TECHNICAL.md#censored-cpu).
- **Usage history of replaced pods** is attributed to the owning Deployment (by ReplicaSet name) or DaemonSet
  (`ClusterSnapshot.OwnerOf`), so a rollout no longer resets confidence.
- **Webhook visibility**: `tailor_webhook_skipped_total{reason}` (`warming_up`, `not_confident`, `guaranteed_qos`) counts pods
  the webhook admitted unmodified. The webhook leaves Guaranteed templates alone so the pod stays Guaranteed.
- **Validated memory trend**: `TrendForecast` publishes `memTrendBytes` / `tailor_predicted_mem_bytes` only
  with at least 24 points and R² >= 0.5 (display only).
- **`OOM_MEM_CAP_FACTOR`** (chart `tailor.oomMemCapFactor`, default 4): ceiling of the OOM-driven request raise
  for containers without a memory limit; config is validated at boot.
- **Per-container writes**: `POST /api/workload` accepts `container` with `mode` and/or `exclude`; the
  dashboard has per-container mode selects, Exclude switches and per-resource (CPU/memory) confidence badges;
  the MCP tool `TAILOR_SET_WORKLOAD` takes the same fields. `ignore` is absolute on every path, including
  these writes.
- **Backtest**: new scenarios `memory-spikes` and `throttled-cap` (7 in total); the report gains a
  `CPU>limit` column.
- **Advisor** default response language is English (`tailor.advisor.language`).
- **Events** for Knative-service units target the Knative Service; units with no real object (groups) emit none.
- **NetworkPolicy suite** (`dev/calico/`): `netpol-test.sh` builds a Calico v3.33.0 k3d cluster and checks
  8 scenarios with a negative control (52 PASS / 0 FAIL in the recorded run, single node, Lima VM with
  kernel 6.8). Requires a Docker kernel with the netfilter modules Calico needs; the "Dory" runtime lacks them.
- **Real Istio test** (`dev/istio/`): `mesh-test.sh` runs Istio 1.31.3 (profile `minimal`) on k3d with classic
  and native sidecars, 7 scenarios PASS in each mode.

- **Live disruption reads**: the PDB budget and replica counts are read from the API server right before a
  disruptive restart (`internal/k8s/live.go`); if the read fails the pod is held (`disruptive_live_unknown`).
- **Persisted reconciler state**: the failure backoff and the in-progress rolling are kept in pod annotations
  (`tailor.k8s-tailor.dev/resize-failed`, `/rolling`), and the scale-down stabilizer is re-seeded after a restart
  by replaying `Recommend` (at most 12 calls).
- **OOM floor persistence** also for Knative Services (object metadata) and TailorGroups (annotation
  `tailor.k8s-tailor.dev/group-oom-floor`); a bare Pod stays in memory.
- **`memory.enforceLimit`** (opt-in, on policies, profiles and `containers[].memory`): give a memory limit to
  containers that run without one.
- **`seasonGate.minCorrelation` / `seasonGate.minPairs`** on `TailorPolicy`: the seasonality gate thresholds are
  tunable (defaults 0.5 and 24). Weekly seasonality needs a window longer than 7 days.
- **`VM_THROTTLE_JOB`** (chart `tailor.vmThrottleJob`, default `kubelet-cadvisor`) pins the CFS throttling query;
  the bundled vmsingle gains the job `kubelet-cadvisor` (only the two CFS counters) and scrapes the engine
  per pod (pod service discovery, Role `k8s-tailor-vmsingle-pods`).
- **Optional read protection** (`tailor.apiAuth.protectReads`, `TAILOR_API_PROTECT_READS`): `GET/HEAD /api/*`
  need the same Bearer token as writes; `/healthz`, `/livez`, `/metrics` and `/` stay open. The MCP server and the
  embedded UI handle the protected reads.
- **Built-in presets** (`tailor.presets.enabled`): the `bursty` `TailorProfile` (CPU quantile 0.95).
- **GitOps examples for engine-owned fields**: `deploy/argocd/workload-application.yaml` (`ignoreDifferences`) and
  `deploy/flux/` (`Kustomization` ignore, `HelmRelease` drift detection).
- **MCP Apps**: the dashboard resource is exposed with `_meta.ui.resourceUri` (SEP-1865, the open MCP extension
  for interactive UI; `ui://` URI, `text/html;profile=mcp-app`) next to the legacy flat key.
- **`license: Apache-2.0`** declared in the `package.json` files.

### Changed

- The engine ClusterRole and ClusterRoleBinding are named `k8s-tailor-engine` (they read and write).
- The webhook no longer mutates Guaranteed templates.
- A refused advisor read (protected reads) shows its own message in the dashboard instead of the
  "advisor locked" write message.

### Known limitations

Authentication is a single shared static token per surface; images are not signed. The remaining limitations and
design decisions (Guaranteed pods, OOM ceiling and LimitRange, node versus cgroup OOM, history of replaced pods,
"Applied" on kubelets older than 1.33, GitOps interaction and more) are in
[docs/KNOWN-ISSUES.md](docs/KNOWN-ISSUES.md).

[Unreleased]: https://github.com/nicacioliveira/k8s-tailor/commits/master
