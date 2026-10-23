# Roadmap

Where the project stands and what comes next. See [ARCHITECTURE.md](ARCHITECTURE.md) for the
design, [PRODUCT.md](PRODUCT.md) for intent, and [KNOWN-ISSUES.md](KNOWN-ISSUES.md) for bugs and
limitations that remain.

## Working today

- **In-place resize, no restart**: a running pod's requests are resized through `pods/resize`
  (`restartCount` stays 0; `make k3s-smoke` checks this on k3d). Rollout fallback for
  Deployments, StatefulSets and DaemonSets when apply is `Rollout`, the cluster lacks
  `pods/resize`, or `InPlacePreferred` needs a restart (PDB-blocked memory resize, Guaranteed QoS).
- **Decision trail**: every applied change is recorded in ClickHouse (when `CH_URL` is set), kept in
  an in-memory ring buffer otherwise, and emitted as a Kubernetes Event for in-place, rollout and
  ksvc actions.
- **Confidence gate**: workloads with fewer than `MIN_SAMPLES` distinct scrape points (engine
  default 30; the chart sets 288) are never actuated, except for OOM recovery.
- **OOM-learned memory limit floor**, durable for `OOM_FLOOR_TTL` (default 30d) and persisted on the Deployment, StatefulSet, DaemonSet, Knative Service or TailorGroup; for containers
  without a memory limit the request raise is capped at `peak x OOM_MEM_CAP_FACTOR` (default 4).
- **Censored-CPU handling**: usage pinned at a CPU limit (or throttled, when cAdvisor series exist)
  grows an existing limit by `CPU_LIMIT_GROWTH_FACTOR` per step, bounded by `cpu.max` or
  `CPU_CENSOR_MAX_FACTOR`; never under shrink-only, HPA hold or without CPU confidence.
- **History of replaced pods** is attributed to the owning Deployment or DaemonSet by pod name;
  the memory trend is published only when validated (>= 24 points, R² >= 0.5).
- **Change-point trimming** and **seasonal forecast** (daily, weekly, both) in the recommender.
- **Guardrails**: direction gate, HPA hold, PDB-aware fallback, limits never shrunk, drift
  thresholds, cooldowns, global pause, per-policy schedule windows.
- **Knative**: revisions collapse into one unit; template patch when a rollout is needed.
- **Optional webhook** that sizes pods at creation in an allow-listed set of namespaces.
- **Auth**: nginx sidecar Bearer-gates the MCP/UI; the engine's write API is Bearer-gated with
  `tailor.apiAuth` and the MCP forwards that token ([UPGRADE.md](UPGRADE.md)). Reads are open
  by default (NetworkPolicy) and can require the token with `tailor.apiAuth.protectReads`.
- **Per-container control**: `POST /api/workload` and the dashboard can set a mode or an exclusion for a
  single container; `ignore` is absolute on every path.
- **Real-cluster suites**: the chart's NetworkPolicies on Calico v3.33.0
  ([dev/calico/README.md](../dev/calico/README.md)) and a real Istio 1.31.3, classic and native sidecars
  ([dev/istio/README.md](../dev/istio/README.md)); the base run is in [E2E.md](E2E.md).
- **Reconciler state survives restarts**: cooldown, failure backoff and the in-progress rolling are kept in pod annotations; PDB budget and replica counts are read live before a disruptive restart; the scale-down stabilizer is re-seeded after a restart.
- **Opt-in memory limit** (`memory.enforceLimit`), tunable seasonality gate (`seasonGate`), `bursty` preset profile, optional read protection (`tailor.apiAuth.protectReads`), GitOps ignore rules for engine-owned fields (`deploy/argocd`, `deploy/flux`).
- **Honest cost**: static prices are labeled `est`; `opencost` reads measured node prices.
- **CI**: Go (`gofmt`, `vet`, `test -race`), Helm lint/template, MCP `tsc` + biome + build.
  Local k3d environment under `dev/k3s`.

## Next

Ordered roughly by value to risk. Items that touch safety come first.

1. **Per-user identity and audit** on top of the shared static tokens (the MCP forwards the token, the embedded UI accepts a pasted one, reads can be token-protected).
2. **Release hygiene**: tag, publish versioned images and the chart to GHCR (the workflow exists
   and runs on `v*` tags), pin versions in `deploy/`.
3. **Shared reconciler state across replicas** for the pieces that are still per replica (the in-memory decision ring buffer without ClickHouse).
4. **Guaranteed QoS support** (resize request and limit together), if the memory-limit decrease can be made safe ([KNOWN-ISSUES.md](KNOWN-ISSUES.md)).
5. **LimitRange and node allocatable in the OOM ceiling**, which needs extra RBAC (`limitranges`, `nodes`).
6. **Learning loop**: shape classification, then margin feedback (designed in
   [PRODUCT.md](PRODUCT.md), not implemented).
7. **Better forecasting** (Holt-Winters or STL) and **cold-start transfer**.
8. **Pricing**: the `aws` provider is a stub; `opencost` is the recommended source.
9. **Scale measurements**: load test with hundreds of namespaces and publish refresh latency and memory figures.
10. **More suites**: multi-node clusters and the API server reaching the webhook under `webhookFrom`; the policies on Cilium; a real Knative install; Linkerd; Istio ambient mode and Istio CNI.
11. **A webhook mutation asserted under the NetworkPolicies**, end to end (the `mutate` step of `dev/calico/netpol-test.sh`).

## Before production

- Keep `MIN_SAMPLES` at the chart default (288 samples at the fixed 300 s step = 24 h of coverage, so new workloads are not actuated for about a day by design). The engine's own default is 30; `dev/k3s` uses 2 for demos.
- Keep `mcp.auth.enabled=true` with a token from a managed Secret; set `tailor.apiAuth`; restrict
  access with a NetworkPolicy on a CNI that enforces it (engine reads are open unless
  `tailor.apiAuth.protectReads=true`).
- Start with `mode: Recommend`, then `direction: Down` on a small selector.
- Pin image tags (`:vX.Y.Z` or `:sha`), never `:latest`.

---

## Open decisions for the project owner

These are recorded, **not executed**. Each has a recommendation and the trade-offs.

### 1. Drop ClickHouse?

ClickHouse stores applied decisions (low volume: a handful of events per workload per day) and
backs the Decisions view and counts. It is a 2Gi-limit StatefulSet and the heaviest part of the
install for the least data.

- **Keep as is**: pro, durable and queryable history, future ML feature tables, no code change.
  Con, an extra stateful dependency for a small event log; a new user has to run or trust it.
- **Replace with a CR or ConfigMap-backed log, or an optional sink**: pro, one fewer database;
  Events plus the `tailor_*` metrics in VictoriaMetrics already carry much of the trail. Con,
  etcd is a poor event store; history is bounded and retention needs a bespoke pruner.
- **Make it opt-in** (default off, ring buffer plus Events): pro, lightest default install, same
  code path since `CH_URL` empty already works. Con, default install loses durable history until
  enabled.

**Recommendation:** make ClickHouse **opt-in** (`clickhouse.enabled=false` by default) and keep the
sink for teams that want it; do not delete the code until usage shows nobody enables it. Persist the
essentials (last N decisions) in a small CR status or ConfigMap if durability across restarts
matters for the default.

### 2. Make VictoriaMetrics optional?

The bundled single-node VictoriaMetrics is the metric source (two kubelet series at 5s). `VM_URL` can
already point to an existing VictoriaMetrics. The queries use `rate()` plus `rollup_rate` in `max`
mode (VictoriaMetrics-only); the default `avg` mode uses plain `rate()`, which Prometheus also provides; there is
no CI job against a Prometheus container yet.

- **Keep bundled VM, BYO supported** (today): pro, works out of the box, 5s resolution, small.
  Con, a second stateful component; duplicates what many clusters already have.
- **Support Prometheus and Thanos explicitly**: pro, fits existing stacks and KRR users. Con,
  needs a test matrix and gives up `rollup_rate` precision.
- **Read kubelet `/metrics/resource` directly with an in-engine ring buffer**: pro, no TSDB at
  all. Con, loses history across restarts, which the 7-day window depends on.

**Recommendation:** keep the bundled VM as the default and **document and test** Prometheus
compatibility in `avg` mode (add a CI job against a Prometheus container). Do not build an
in-engine store.

### 3. MCP as a thin adapter?

Today `mcp/` is both the product UI host (Hono, nginx sidecar, React) and the MCP tool surface,
proxying the engine. The engine embeds the same bundle on `:8080` but lacks the Bearer sidecar and
the MCP integrations, which creates two UIs with different auth behavior.

- **Status quo**: pro, nothing breaks. Con, two deployment paths for one UI, a Bun runtime image
  in addition to the Go image, auth logic split across nginx, MCP and engine.
- **Thin MCP adapter** (MCP server only translates tools to engine API calls; UI served by the engine,
  auth done once in the engine, optional ingress for TLS): pro, one UI, one auth model, one fewer
  runtime for users who never use MCP. Con, work to move the nginx gate and token flow into the
  engine; MCP hosts that render the UI resource need the bundle served from the adapter anyway.

**Recommendation:** move toward a **thin MCP adapter**: engine owns the UI and auth, `mcp/` becomes
an optional tool shim (and can still return the UI as an MCP resource). Do this after item 1 under
Next, since fixing token forwarding is a prerequisite either way.
