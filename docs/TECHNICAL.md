# Technical reference

How K8s Tailor computes and applies a recommendation, in enough detail to audit it. Every statement here was
checked against the code; references are `path:function`. Overview and diagrams: [ARCHITECTURE.md](ARCHITECTURE.md).
Offline evaluation against KRR and VPA: [BACKTEST.md](BACKTEST.md).

Contents: [1 Metrics](#1-metrics-sources-and-queries) · [2 Recommendation model](#2-recommendation-model) ·
[3 Anti-flapping](#3-anti-flapping-change-point-seasonality-stabilization) · [4 Limits](#4-limits) ·
[5 OOM floor](#5-oom-learned-floor-and-limit-rules) · [6 HPA and confidence](#6-hpa-hold-and-confidence) ·
[7 Actuation](#7-actuation) · [8 Webhook](#8-webhook) · [9 Groups](#9-groups-tailorgroup) ·
[10 CRDs](#10-crds-real-fields) · [11 Configuration](#11-engine-configuration-env) · [12 API](#12-http-api) ·
[13 Observability](#13-observability) · [14 Security](#14-security-model) · [15 Persistence](#15-state-and-persistence)

---

## 1. Metrics: sources and queries

The engine reads only two cAdvisor/kubelet series (`internal/recommender/recommender.go:fetchAndCollapse`), always on
a **fixed 300 s step** (`const stepSec = 300`), per namespace, over the window `WINDOW` (default `7d`):

```promql
# CPU (CPU_RATE_MODE=avg, default): average rate over each 5 min step
rate(container_cpu_usage_seconds_total{job="kubelet-resource",namespace="NS",container!="",container!="POD"}[5m])

# CPU (CPU_RATE_MODE=max, VictoriaMetrics only): sub-window micro-bursts
rollup_rate(container_cpu_usage_seconds_total{job="kubelet-resource",namespace="NS",container!="",container!="POD"}[5m], "max")

# Memory: the MAX of each 5 min step, not the instantaneous value at the boundary
max_over_time(container_memory_working_set_bytes{job="kubelet-resource",namespace="NS",container!="",container!="POD"}[5m])
```

Every `container_*` query (these, the dashboard charts, the cluster usage chart and the backtest's `-vm-url` mode with `-vm-job`) carries `job="<VM_JOB>"`
(chart `tailor.vmJob`, default `kubelet-resource`, the job the bundled vmsingle scrapes the kubelet with). Without the pin, any series with the same name and
`namespace/pod/container` labels from another scrape job would feed the recommendation. `VM_JOB` set but empty removes the filter. With your own Prometheus set the
name of the job that scrapes the kubelet, otherwise nothing matches and the engine is inert. The pin does not stop someone with write access to the TSDB from
pushing a series labelled with that job (`/api/v1/import`); the vmsingle NetworkPolicy covers that.

Rationale for the memory choice (code comment): with a 300 s step an instant query samples one scrape in
10 to 20 and misses short peaks, which are exactly the ones that OOMKill. Memory is deliberately conservative; for
CPU a miss is throttling.

Series are keyed by `pod` and `container` labels, mapped to the owning workload through a cluster-wide pod and
ReplicaSet snapshot (`internal/k8s/client.go:Snapshot`, `ClusterSnapshot.OwnerOf`), and **concatenated across pods** (not summed): the quantile
is therefore a per-pod request. Every container of a pod, including sidecars, is sized from its own series.

**Pods that no longer exist.** Their series stay in the TSDB for the whole window under their old pod names. `OwnerOf` attributes such a pod
by its generated name when that is unambiguous: `<ReplicaSet>-<5-char suffix>` for a ReplicaSet that still exists (current or one of the
`revisionHistoryLimit` old ones) and belongs to a Deployment, or `<DaemonSet>-<suffix>` for a DaemonSet owning a live pod in the namespace (the
suffix must use the API server's `generateName` alphabet). StatefulSet pods keep their names. Grouped units (`ksvc`, `group`) are attributed by labels a deleted pod no longer has, so the engine
remembers, while it observes a grouped pod alive, which unit it belonged to (`Engine.rememberUnits`, namespace/pod to unit, in memory) and
keeps that mapping for one `WINDOW` after the pod was last seen; a pod dropped as an old generation of a latest-cohort group is forgotten at once
so it cannot dilute the group. What stays outside: pods that vanished before this engine process ever saw them (the cache is not persisted, so after an
engine restart the history of grouped pods that are already gone is not recovered), a ReplicaSet already garbage-collected, and bare pods (a
bare pod is its own unit; a recreated one with the same name is live again, any other name is a new pod). Before this, a pod replacement (rollout, `kubectl delete pod`, eviction) threw the workload's
history away: the fresh pods were unconfident for `MIN_SAMPLES x 5 min` (24 h with chart defaults) with the data right there, and the webhook
stayed mute for that long (Istio test finding 2, [section 8](#8-webhook)).

**Optional throttling series.** One more query per namespace, pinned to its own job, `VM_THROTTLE_JOB` (chart `tailor.vmThrottleJob`,
default `kubelet-cadvisor`; set but empty = no job filter):

```promql
sum by (namespace, pod, container) (increase(container_cpu_cfs_throttled_periods_total{job="<VM_THROTTLE_JOB>",namespace="NS",...}[5m]))
  / sum by (namespace, pod, container) (increase(container_cpu_cfs_periods_total{job="<VM_THROTTLE_JOB>",namespace="NS",...}[5m]))
```

These are **cAdvisor** series (`/metrics/cadvisor`), scraped by a different job than the kubelet resource metrics: the chart's bundled
vmsingle collects them in job `kubelet-cadvisor` (only the two CFS counters are kept, see [CONFIGURATION.md](CONFIGURATION.md)). With your own
Prometheus/VM set `VM_THROTTLE_JOB` to its cAdvisor job; a wrong value only removes the throttling signal (the pinned-at-limit check still
works). They feed the [censored-CPU rule](#censored-cpu). An empty result or a failed query is **no signal**, never "zero throttling", and
never fails the refresh.

Other queries (all through `internal/vm`, Prometheus `/api/v1/query` and `/api/v1/query_range`):

| Purpose | Expression |
|---|---|
| Dashboard charts, per container | `sum(rate(container_cpu_usage_seconds_total{job=..,namespace=..,container=..,pod=~"p1\|p2"}[2m]))`, `sum(container_memory_working_set_bytes{...})` (`recommender.go:DetailSeries`) |
| Cluster usage on Overview | `sum(rate(container_cpu_usage_seconds_total{job=..,namespace=~"a\|b",container!="",container!="POD"}[2m]))` and the memory equivalent (`ClusterUsageSeries`) |
| Engine's own series | `tailor_recommended_cpu_cores{...}`, `tailor_current_cpu_cores{...}`, `tailor_recommended_mem_bytes{...}`, `tailor_current_mem_bytes{...}` (read back for the charts); `sum(clamp_min(tailor_waste_usd_week, 0))` (Savings trend) |
| Opencost pricing (`PRICING_MODE=opencost`) | `avg(node_cpu_hourly_cost)`, `avg(node_ram_hourly_cost)` (`internal/pricing`); falls back to static prices when absent. `aws` mode is a stub that also falls back. |

Source of truth for "what the pod runs with" is `status.containerStatuses[].resources` when the kubelet reports it
(k8s >= 1.33 with in-place resize), else the pod spec. A workload reads *right-sized* once the live pods carry the
new request even though the controller template still shows the old one; the `SpecDrift` flag marks that.

## 2. Recommendation model

`internal/recommender/recommender.go:Recommend` is a **pure function**: same input, same output (no cluster, no
backend, no wall clock). For one container, with `q_cpu`, `q_mem` the policy quantiles (defaults 0.90 and 0.95),
`m_cpu`, `m_mem` the margins (default 0.15) and `H` the half-life (default `24h`, policy `halfLife` overrides):

1. **Regime trimming.** Keep only samples at or after the latest sustained level shift ([section 3](#3-anti-flapping-change-point-seasonality-stabilization)).
2. **Decay-weighted quantile** (VPA-style, `math.go:WeightedQuantile`). Each sample has weight
   `w = 2^(-(t_now - t)/H)`; sort by value; the estimate is the first value whose cumulative weight reaches `q * sum(w)`.
3. **Peak** of the trimmed regime (`math.go:Peak`).
4. **Seasonal forecast** ([section 3](#3-anti-flapping-change-point-seasonality-stabilization)): the same quantile over samples in the matching hour bucket, evaluated one hour ahead (`now + 1h`), only when the season gate passes. 0 when not enough history.
5. **Request**:

```text
cpu_req = clamp( max( cpuQ * (1 + m_cpu),  cpuForecast * (1 + m_cpu) ),             cpu.min, cpu.max )
mem_req = clamp( max( memQ * (1 + m_mem),  memPeak,  memForecast * (1 + m_mem) ),   mem.min, mem.max )
```

   `clamp` raises to `min` then caps at `max` (`max <= 0` means unbounded). CPU has **no peak floor**; memory is floored
   at the observed peak of the current regime with **no margin on the peak**. Default floors: `MIN_CPU_MILLI=10`,
   `MIN_MEM_MI=32` (engine env; policy `cpu.min`/`memory.min` override).
6. **Headroom guard** (opt-in `headroomGuard`, threshold default 0.02): additionally floors each request at the
   `1 - threshold` plain quantile of the full window (`quantileOf`), so at most `threshold` of samples exceed it by
   construction (still bounded by `max`).
7. **No series, no change.** A resource with no samples keeps its current request (never drops to the policy minimum blind).
8. **Scale-down stabilization** (HPA semantics, [section 3](#3-anti-flapping-change-point-seasonality-stabilization)): the published request is never below the
   highest raw recommendation of the last `scaleDown.stabilization` window.
9. **Request cap.** Unless the policy sets `allowExceedLimit`, a request never exceeds the container's current limit
   (`limitCapped` flags a memory request that wanted more).
10. **Quantization**: CPU rounds **up** to 1m, memory **up** to 1 MiB (`RoundCPUCores`, `RoundMemBytes`). The values the API finally
    receives are `"%dm"` (millicores, half-up) and `"%dMi"` (MiB, half-up) in `reconciler.go:cpuStr/memStr`.

Cost estimate per container: `waste_usd_week = ((cpuCur - cpuRec) * price_cpu + (memCur - memRec)/GiB * price_gb) * 168 * replicas`,
with `price_cpu = PRICE_CPU_CORE_HR` (0.031) and `price_gb = PRICE_GB_HR` (0.004) unless `opencost` pricing answers.
Positive means reclaimable, negative means needed. These are estimates and the UI labels them so.

**Headroom breach** (reported per resource): the share of full-window samples strictly above the recommended request
(`shareGt`), a whole-window SLI computed locally from the samples already fetched. Also `Signals.MemTrendBytes` (and the
`tailor_predicted_mem_bytes` series), a 24 h linear projection of the per-timestamp mean memory, used for display only. It is validated
(`math.go:TrendForecast`): at least 24 distinct timestamps (2 h), a least-squares fit with R² >= 0.5, the horizon capped at the span of
the data it was fitted on (never projects further ahead than the history), result floored at 0; otherwise it is `null`. Before, an hour of
almost-flat, noisy proxy memory projected 24 h ahead read 829 MB for a 44 MB container (Istio test finding 4).

### Burstiness and autoQuantile

`recommender.go:burstiness` measures how far the tail of the CPU series sits above the level the request is sized from: `p99 / p90` over the window,
all replicas pooled, with the denominator floored at 5m so an idle container's blips do not read as bursts, and only once 12 hours of distinct
timestamps exist (`Signals.Burstiness` is `null` before). The values measured on the synthetic scenarios (last 7 of 14 days, seeds 42, 1 and 2) are
`bursty` 6.5-9.0, `heterogeneous` 1.7-1.8, `diurnal-weekly` 1.16-1.19, `step-change`, `memory-leak`, `memory-spikes` at most 1.2; the alternatives from the
design (`p99 / p50`, share of time above 2x the median) do not separate `bursty` from `diurnal-weekly` (about 8% of the time above 2x the median in
both) or from `heterogeneous` (`p99 / p50` 5.4 against 7.8-9.0). A burst share above 10% moves `p90` itself into the burst and the index falls back to
about 1: that case is already sized at the burst level.

With the policy opt-in `autoQuantile: true` (or `AUTO_QUANTILE=true`), an index at or above `burstThreshold` (default 3) raises the CPU quantile used for the
request **and** the seasonal forecast to 0.95; a configured quantile of 0.95 or more is kept. `Signals.CPUQuantileUsed` is the quantile applied,
`Signals.BurstQuantileRaised` says whether it was lifted, and `Signals.BurstReason` states why in words (including "autoQuantile is off" and "below the
threshold"), so the signal is visible before anyone opts in. Backtest `bursty` with the opt-in: [BACKTEST.md](BACKTEST.md#autoquantile-on-bursty).

### Sidecar profile

An injected sidecar defaults to its own model (`internal/policy/containers.go:sidecarProfile`): CPU `q=0.95`, margin `0.25`, min `10m`;
memory `q=0.99`, margin `0.20`, min `32Mi`. The app policy's min/max are deliberately not inherited. Rationale from the code
comments: a proxy's CPU tracks requests, not the app's work, and its memory must never be squeezed into an OOM. Details:
[CONTAINERS.md](CONTAINERS.md).

## 3. Anti-flapping: change-point, seasonality, stabilization

Implemented in `recommender.go` (`currentRegime`, `changePointIndex`) and `stability.go`.

**Per-timestamp mean.** Workload samples arrive concatenated per pod, not time-ordered. `meanByTime` collapses samples that
share a timestamp (one per replica) into their mean and sorts, so replicas running at different steady levels are never mistaken for
a level shift and "before/after" has a meaning.

**Running median.** `medianFilter` replaces each point by the median of `+-3` samples (6 samples at 300 s = 65 min window).
A burst shorter than roughly half that window is removed; a sustained shift survives.

**Change point** (`changePointIndex`, relative threshold `0.5`): needs at least 24 distinct timestamps; each side at least
`max(n/4, 8)` samples. Scanning splits from the most recent side, the first split where
`|mean(after) - mean(before)| / mean(before) >= 0.5` is the change point; samples before it are dropped from the quantile and the
peak. It returns the **most recent qualifying split, not the best one** (the split with the largest `|Δ|`): the best-split variant was
measured with the backtest (seeds 1-5) and kept out because it regressed `heterogeneous` and `diurnal-weekly` without improving
`step-change` ([BACKTEST.md](BACKTEST.md#change-point-best-split-not-adopted)). The seasonal forecast keeps the full series.

**Seasonal forecast** (`math.go:seasonForecast`, `SeasonalForecast`): hour-of-day (`daily`, default), hour-of-week (`weekly`) or
the higher of both (`both`); `off` disables it. Bucket width is 1 h, centered: samples within +-30 min of the target phase. It needs at
least 8 samples overall and 4 in the bucket, else it contributes 0.

**Seasonality gate** (`stability.go:effectiveSeasonGate`): the forecast is only trusted when the data is actually periodic. The hourly-mean
series is correlated with itself one season earlier (Pearson): `daily` needs the lag 24 h correlation `>= minCorrelation`, `weekly` the lag
168 h one, each over at least `minPairs` hourly pairs. Defaults `0.5` and `24` (one day of pairs); TailorPolicy `seasonGate.minCorrelation`
(0 < x <= 1) and `seasonGate.minPairs` (2..10000) override them per policy (out-of-range values in an old CR fall back to the defaults).
**Weekly seasonality needs more than 7 days of history**: week-lag pairs exist only past one week (with the default `minPairs` 24, a window of
at least 8 days; 14 days cover every hour of the week). With the default 7 d `WINDOW` alone, `weekly` would fall back to the daily correlation as a
proxy and a weekly-only pattern (Monday peak, weekdays without a daily cycle) would be rejected (`TestWeeklySeasonNeedsMoreThanAWeek`). So for a
namespace with a `weekly` or `both` policy the refresh also queries `SEASON_WINDOW` (default `14d`, at most `28d`) of CPU and memory
(`Engine.seasonSeries`, cached per namespace for `SEASON_REFRESH`, default `1h`) and passes it as `RecommendInput.SeasonCPU/SeasonMem`: the gate and
the seasonal forecast of those policies read the long series, everything else (quantile, peak, change-point, confidence) keeps `WINDOW`
(`TestRecommendWeeklyUsesSeasonSeries`, `TestSeasonWindowFetchIsBounded`). Cost bound: one extra usage query pair per weekly namespace per
`SEASON_REFRESH`, none for daily policies, none when `SEASON_WINDOW` is `0` or not longer than `WINDOW`; the cached series (up to 28 days of 5-minute
samples per pod of those workloads) lives in memory. If the query fails the refresh logs it and keeps judging on `WINDOW`, as before. The effective season is returned as `cpuSeason`/`memSeason` (`off` when gated out).

**Scale-down stabilization** (`stability.go:stabilizer`): per container, the raw (pre-stabilization) recommendations of the last
`scaleDown.stabilization` window (default `15m`) are kept in memory; the published request is `max(raw, floor)`. Scale-ups pass through
at once. When the engine holds no history for a container (first refresh after a restart or failover, or a new container), it seeds it by
replaying the pure `Recommend` at the past 5-minute query steps inside the window on the series truncated to each instant (at most 12 extra
calls per container, none when the window is 0; `recommendStabilized`), so a restarted engine publishes what one that kept running would
have (`TestStabilizerSeededAfterRestart`).

## 4. Limits

- A **memory limit** is re-derived only for a container that **already has one**: `limit = max(req * memory.limitFactor, memPeak * (1 + m_mem), req)`
  (default `limitFactor` 1.0), rounded up to 1 MiB. A limit factor never imposes an OOM ceiling where the owner chose none.
- A **CPU limit** is set only when `cpu.limitFactor > 0`: `max(req * factor, req)`. Default 0 (no CPU limit managed).
- Unless `allowExceedLimit`, the recommended limit never exceeds the current one either. For CPU "current" is the **live** limit (max across the
  running pods) when known: an in-place raise moves the pod's limit, not the template's.
- Exception: the [censored-CPU rule](#censored-cpu) raises an existing CPU limit.

### Censored CPU

A container's CPU usage cannot exceed its CPU limit (CFS quota): when it is pinned there, the measured series is **censored** — a lower bound
of the demand. The quantile of censored data estimates the limit, not the demand, so the recommendation used to sit at ~the limit forever
(capped at it by the rule above), and the small difference it did want was swallowed by the 20m absolute drift gate. The Istio test showed it:
proxy limit 50m, usage 50.8m, recommendation ~64m never applied, throttled 100% of CFS periods; once the limit was lifted by hand it used 276m.

Detection (`recommender.go:cpuCensoring`), over the **last hour** of samples (all replicas), against the live CPU limit:

- `cpuPinnedShare` = share of samples `>= 0.9 x limit`; censored when `>= 0.25` (at least 6 samples); or
- `cpuThrottledShare` = mean throttled-period ratio from the [optional cAdvisor series](#1-metrics-sources-and-queries); censored when `>= 0.25`
  (at least 6 samples; absent series = `null` = no signal).
- A container without a CPU limit is never censored.

When censored (`Signals.CPUCensored`), in `Recommend` after the caps:

```text
limit   = max(roundUp(liveLimit x CPU_LIMIT_GROWTH_FACTOR), liveLimit)        # default 1.5, a step per actuation
          capped at policy cpu.max, or (no cpu.max) at templateLimit x CPU_CENSOR_MAX_FACTOR (default 8); never lowered
request = clamp(max(request, liveLimit), cpu.min, cpu.max), limit >= request  # the demand is provably >= the live limit
```

At actuation (`PlanInPlace`, the rollout path and therefore the webhook): the request growth passes the drift gate (any rise >= 1m), and
`Plan.CPULimitRaise` patches the CPU limit even when the request does not move. Never under a shrink-only (`Down`) policy, never for an
HPA-held or unconfident CPU, only on an **existing** limit, and only up (the scale-up cooldown paces the steps). Each step lets the next hour
measure past the old limit; the rule stops when less than a quarter of the last hour is at the new limit. Backtest `throttled-cap`
([BACKTEST.md](BACKTEST.md)): stuck at 50m before (throttled 576 of 576 pod-hours), 50 → 75 → 113 → 170 → 255 → 383m after (5.7 pod-hours).

**CPU-greedy containers (convergence).** A busy loop eats every raise at once, so it would be raised to the brake forever. The engine counts the
raises of a run: when it first sees a container censored it remembers the live limit as the run's *anchor* (`Engine.trackCensor`, in memory), and
`censorSteps = floor(log_growth(liveLimit / anchor))` (`Signals.CPUCensorSteps`). At `CPU_CENSOR_MAX_STEPS` (default 3) steps with the usage still pinned
(`CPUCensored`), the limit is **parked**: the recommendation keeps the live limit (no further raise; the request still follows it), the container is marked
`Signals.CPUGreedy` (`cpuGreedy` in the API, `tailor_cpu_greedy` gauge, shown in the workload sheet) and the Event/skip path stays quiet. The run ends, and
the anchor is forgotten, when fewer than a quarter of the last hour's samples are at or above 80% of the limit (`Signals.CPUGreedyRelease`): a later episode
is counted from the limit then in force. A container whose demand really ends is not parked: its usage dips below 80% of the limit between episodes
(the `throttled-cap` scenario, whose demand follows a daily cycle, still reaches 383m). Backtest `cpu-greedy` ([BACKTEST.md](BACKTEST.md)): 200m → 1600m
(the brake) before, parked at 675m (200m x 1.5^3) after. The anchor is not persisted: after an engine restart a run is counted again from the live limit,
so a parked container may be raised up to `CPU_CENSOR_MAX_STEPS` more times before it parks again.

Limits of the rule: `cpu.max` still bounds the growth and is the right tool when the workload's ceiling is known. The `templateLimit x factor` brake is anchored on the controller template, which an in-place resize does not
touch, but **moves** for an injected sidecar (its "template" values come from the running pods) and with `apply: Rollout` (the template
follows each raise): there only `cpu.max` bounds the growth. `CPU_LIMIT_GROWTH_FACTOR=1` disables the limit raise (the request is still lifted
to the live limit). 5-minute averages hide sub-period throttling of bursty containers; only the cAdvisor ratio sees it.
- At actuation (`reconciler/decide.go:Plan.Limits`): limits ride along only with a request that drifted; a limit is **never shrunk** in place
  (`limitFloor`) and never below the new request; an existing limit is lifted to at least the new request even with no `limitFactor`
  (a request above its limit is invalid). A memory limit is never introduced on a container that has none, unless the policy opts in
  with `memory.enforceLimit: true` (below).

<a id="enforce-limit"></a>**Opt-in memory limit (`memory.enforceLimit`).** Default `false`. On a TailorPolicy or TailorProfile memory model (and on
`containers[].memory` rules; a later layer may set `false` to switch it back off), `true` makes the recommender derive a memory limit for a container
that has none, the same way it re-derives an existing one: `request * limitFactor` (1 when unset), never below `observedPeak * (1 + margin)`
and never below the request. The webhook adds it at admission, the in-place resize adds it even when the request has not drifted
(`Plan.MemLimitAdd`; Kubernetes allows adding a limit in place as long as the QoS class does not change, which the `qos_change` guard checks),
and the rollout adds it to the template. HPA memory holds, the PDB/disruptive guards (a `RestartContainer` memory policy makes it disruptive)
and the confidence gate apply as to any memory change. Use it only where an OOM ceiling is wanted: a limit derived from the observed window
can still be below a future peak.

## 5. OOM-learned floor and limit rules

When a container with a memory **limit** is OOMKilled, the limit that proved insufficient is recorded and protected for `OOM_FLOOR_TTL`
(default `30d`) at `observedLimit * OOM_GROWTH_FACTOR` (default 1.5) (`recommender.go:learnOOM`, `Recommend`):

- The floor is applied to the **limit** after all caps, regardless of `allowExceedLimit` or a `Down` policy, so a later shrink cannot
  push the limit back to the value that killed the container. The request stays tight.
- An active floor only rises within its TTL. The **same kill** (same `finishedAt`) is learned once, so the raised limit seen in the pod's
  `lastState` is not re-folded on every refresh.
- An OOM bypasses the confidence gate for memory and, with `oomOverridesDirection`, a shrink-only policy.
- The floor is persisted when the workload's mode actuates, so it survives restarts and failover (`persistOOM`, `seedOOM`): on Deployments,
  StatefulSets and DaemonSets as annotation `tailor.k8s-tailor.dev/oom-floor`; on a Knative Service (`ksvc`) as the same annotation on the
  Service **metadata** (get + update; never the template, so no revision is minted); for a `group` unit, which has no object, on the
  TailorGroup CR whose rule folded it, annotation `tailor.k8s-tailor.dev/group-oom-floor` keyed `namespace/unit/container` (other units'
  entries kept, expired ones pruned). The verbs are the ones the ClusterRole already grants (`services.serving.knative.dev` get/update,
  `tailorgroups` get/update). Bare Pods keep it only in memory, by design: a floor protects the next pod of a controller, and a bare Pod has none.
- The reconciler forces a resize to raise an existing memory limit up to the floor even when the request has not drifted (`Plan.MemLimitRaise`),
  because a restarted pod comes back at its template limit, the very value that killed it.

For a container **without** a memory limit the behavior differs (the kill comes from node pressure, and the engine never introduces a limit unless `memory.enforceLimit` is on).
The **request** is raised instead, to `max(floor, observedPeak * OOM_GROWTH_FACTOR)`, where the floor is `request-at-kill * OOM_GROWTH_FACTOR`
learned once per kill; it is never derived from the live request (that is the engine's own output and would compound on every refresh).
The raise is capped at `observedPeak * max(OOM_MEM_CAP_FACTOR, OOM_GROWTH_FACTOR)` (default factor 4, chart `tailor.oomMemCapFactor`) and then
at the policy `memory.max` (or a per-container `max-mem` annotation); the lower wins. With no memory series (peak 0) only `memory.max` applies.
`config.Validate` rejects a non-finite factor or one below `OOM_GROWTH_FACTOR` at boot.

**Schedulable ceiling (opt-in).** With chart `tailor.rbac.clusterLookups=true` (`CLUSTER_LOOKUPS=true`) the engine also lists `nodes` and
`limitranges` (`k8s.Client.MemoryCeilings`, cached 5 minutes) and `Recommend` caps the memory **request** at
`min(largest allocatable memory among Ready, uncordoned nodes, namespace LimitRange max of type Container)` (`RecommendInput.MemCeiling`): in the OOM raise
above and as the last step for every request, so a request no node or admission rule can accept is never recommended. The limit may stay higher, but never
below the request. Not evaluated: taints, tolerations, affinities, node selectors, `Pod`-type LimitRange max, ResourceQuota. If the capped value is below
the observed peak the container can still OOM; the ceiling reports reality, it does not create memory. Without the flag the engine has no access to either
resource and nothing changes.

## 6. HPA hold and confidence

**HPA.** `internal/k8s/client.go:loadHPAs` finds HorizontalPodAutoscalers per target. If one has a `Resource` metric on CPU (or memory), that
resource's request is the denominator of the HPA's math, so the recommendation is still computed but **never actuated** (`CPUHeldByHPA`,
`MemHeldByHPA`); the other resource is sized. This applies to in-place, rollout and webhook. Knative's KPA scales on concurrency and is vertical-safe.

**Confidence.** Per resource, `confident = distinct scrape timestamps in the window >= MIN_SAMPLES` (`Recommend`, `meanByTime`): 40 replicas seen in one
scrape is one point in time. `Confident = cpuConfident || memConfident`. In `PlanInPlace` a resource without its own coverage is held at the live
value like an HPA-held one (memory still moves on an OOM). Because the step is fixed at 300 s, `MIN_SAMPLES` is time:

| `MIN_SAMPLES` | Coverage required |
|---|---|
| 288 (chart default) | 24 h (one daily cycle; `season: daily` is the default basis) |
| 30 (engine default when the env var is unset) | 2.5 h |
| 2 (`dev/k3s`) | about 10 min |

So with chart defaults a new workload is not actuated for roughly a day, on purpose: 288 samples cover one full daily cycle, and a smaller value would let the engine shrink requests after observing only part of that cycle (for example just the overnight trough, then be caught short by the daytime peak).

## 7. Actuation

### Mode versus method

`mode` decides **whether** to act (`Off`, `Recommend`, `Webhook`, `InPlace`, `Both`); `apply` decides **how** (`InPlace`, `Rollout`, `InPlacePreferred`).
Defaults: mode `Recommend`, apply `InPlace`, direction `Both`, season `daily`, cooldowns up `30s` / down `1h`, stabilization `15m`
(`policy.go:Resolve`). Precedence: `ignore=true` annotation (absolute, forces `Off`) > per-workload `tailor.k8s-tailor.dev/mode` annotation >
highest-`priority` matching TailorPolicy > `TailorConfig.defaultMode`. Per container, a class default, then the first matching `spec.containers` rule,
then `container.<name>.*` annotations refine it (`ResolveContainer`). A policy with `schedule.windows` falls back to `schedule.outsideMode`
(default `Off`) outside its windows.

### In-place (`reconciler.go`, `decide.go`)

`PlanInPlace(entry, container)` produces a `Plan`:

```text
gCPU = direction.Gate(liveCPU, recCPU)      # Down: never raise; Up: never lower
gMem = direction.Gate(liveMem, recMem)      # OOM + oomOverridesDirection may raise under Down
held  -> target = live value                # HPA hold, or no per-resource confidence (memory moves on OOM)
cpuDrift = |live - target| >= 20m  AND  |live - target| / max(live, target) >= 15%
memDrift = |live - target| >= 16Mi AND  ... >= 15%
memLimitRaise = OOM floor active AND the container has a memory limit AND recLimit > liveLimit * 1.01
censored      = Signals.CPUCensored AND CPU not held AND direction != Down
cpuDrift     |= censored AND target >= live + 1m             # censored growth bypasses the drift gate
cpuLimitRaise = censored AND the container has a CPU limit AND recCPULimit > liveCPULimit * 1.01
```

`Decide` adds the confidence gate and the cooldown; `reconcileNS` adds the pod-level guards. The patch is a strategic merge on the
`resize` subresource of the pod (`k8s/client.go:ResizePodContainer`), targeting `spec.initContainers` for native sidecars. Guards, in order:

| Guard | Behavior | `tailor_resize_skipped_total{reason}` |
|---|---|---|
| In-place support unknown (discovery error) | Hold, Warning Event, retry in 30 s. A definite "no `pods/resize`" falls back to rollout for managed workloads. Answers are cached 10 min. | `inplace_support_unknown` |
| Previous resize outstanding | Do not stack another patch. | `resize_pending` |
| QoS Guaranteed pod (`guaranteed: skip`, default) | Skip (class is immutable); under `InPlacePreferred`, roll the template instead. | `guaranteed_qos` |
| QoS Guaranteed pod, `guaranteed: resizeBoth` | Request and limit are patched to the same value ([Guaranteed pods](#guaranteed-pods)); a memory shrink is dropped. | `guaranteed_mem_shrink` |
| QoS BestEffort pod | Skip (cannot add requests in place). | `besteffort_qos` |
| Failure backoff | 5 min per pod and container after a rejected resize, persisted on the pod (`tailor.k8s-tailor.dev/resize-failed`: time and reason; cleared by a later success) so it survives a restart. | `failure_backoff` |
| Live disruption state unreadable (disruptive restart of a Ready pod) | Hold; the PDBs of the namespace and the owning controller are listed right before the restart, and a failed read or a deleted controller never turns into a restart. | `disruptive_live_unknown` |
| PDB with 0 disruptions allowed (read live) and a resize that restarts the container | `InPlacePreferred`: rollout; else hold the restarting part and apply the rest. | `pdb_blocked` |
| Cooldown (up 30 s, down 1 h by default) | Persisted via the `last-applied` pod annotation. | `cooldown` |
| Resize would make the pod Guaranteed | Skip. | `qos_change` |
| Pod-level resources would be violated | Skip. | `pod_level_resources` |
| Disruptive and another replica is not Ready, is Pending, or is missing (fewer Ready pods than the live `spec.replicas`, or the controller's `status.readyReplicas` below it) | Wait, for at most `DISRUPTIVE_MAX_WAIT` (default 15m, `0` = forever) on replicas that stay NotReady; see below. | `disruptive_wait_ready` |
| The wait expired but a restart would leave fewer than 2 Ready replicas | Held; Warning Event `RollingHeld` on the workload. | `disruptive_wait_expired_held` |
| Disruptive and per-cycle budget used | Wait (budget = the live PDB `disruptionsAllowed`, else 1 pod). | `disruptive_budget` |

After the first non-disruptive resize of a workload container, its sibling replicas follow in the same cycle. A resize is **disruptive** when the
container's `resizePolicy` is `RestartContainer` for the resource that moves; those roll one pod per cycle and advance only when the others are Ready,
without waiting the cooldown. Right before a **Ready** pod is restarted, the PDB budget and the replica counts are read **live**
(`k8s.LiveDisruption`, once per workload per cycle, with the `list` verb the ClusterRole already has): the PDBs of the namespace matched on the
pod's own labels (the smallest budget when several match) and the owning Deployment/StatefulSet (`spec.replicas`, `status.readyReplicas`) or
DaemonSet (`desiredNumberScheduled`, `numberReady`). An HPA scale-up or an eviction since the last refresh is therefore seen at once. For a `ksvc`
the counts are the sums over the Deployments Knative creates per revision (`<revision>-deployment`, selected by the label
`serving.knative.dev/service`; the autoscaler writes the desired count into their `spec.replicas`, old revisions sit at 0). `group` and bare Pods
only get the PDB check (there is no desired count). Each restarted pod is marked `tailor.k8s-tailor.dev/rolling`
(same time as its `last-applied`): a restarted engine resumes the rolling when a pod's last actuation was a rolling step and that pod is
already at its target, still behind the readiness gate and the budget; a marker from a finished rolling never skips the cooldown of a new
one (then every pod wants the change).

### Rollout and Knative

`Apply: Rollout` (or `InPlacePreferred` falling back, or a cluster without `pods/resize`) patches the **controller template**
(`PatchWorkloadResources`), floored at 10 min between rollouts, letting the native update strategy (and PDB, `maxUnavailable`, StatefulSet ordering) sequence
pods. A container absent from the template is never patched there; its only durable path is the mesh's pod-template annotations when
`proxyAnnotations` is on (Istio `sidecar.istio.io/proxyCPU|proxyMemory|proxyCPULimit|proxyMemoryLimit`; Linkerd `config.linkerd.io/proxy-cpu-request|proxy-memory-request|proxy-cpu-limit|proxy-memory-limit`).
For a Knative Service the revision Deployment is immutable, so the **ksvc template** is merged and `update`d (`PatchKnativeService`), minting a new revision that is
born right-sized. With `apply: InPlace` on a ksvc, running pods are resized and `SpecDrift` flags that the spec is not yet persisted.

Every applied action: increments `tailor_resizes_total{mode,result}` (`mode` = `inplace`, `rollout`, `ksvc`, `proxy-annotations`), records a decision
(ring buffer plus ClickHouse), and emits a Kubernetes Event `Rightsized` on the pod and the workload (ksvc Events go on the Service). Failures emit
`ResizeFailed` or `RolloutFailed`. Skips emit deduplicated (1 per hour per object and reason) `ResizeSkipped` Events.

### Disruptive rolling and a stuck sibling

The disruptive rolling waits until every other replica is Ready. A sibling that never becomes Ready (a crash loop unrelated to the resize) would
hold it forever, so `DISRUPTIVE_MAX_WAIT` (chart `tailor.disruptive.maxWait`, default `15m`, `0` = wait forever) bounds the wait
(`reconciler.go:maxWaitProceeds`). The engine records when it first saw each Running pod NotReady; a Ready pod may be restarted despite
a NotReady sibling only when all of this holds, on the same live read the other guards use:

- every non-Ready replica has been NotReady for at least `DISRUPTIVE_MAX_WAIT` (a replica the rolling itself just restarted is back long before,
  so the one-pod-at-a-time pacing is unchanged) and no replica is Pending;
- the controller's `readyReplicas` is short of `spec.replicas` by exactly those stuck replicas (nothing unexplained is missing);
- a PodDisruptionBudget covering the pod still allows a disruption (the earlier check is unchanged; a PDB with `disruptionsAllowed: 0` is never
  bypassed) and the per-cycle budget is respected;
- at least two replicas are Ready, so the restart leaves one.

The outcome is counted in `tailor_disruptive_wait_expired_total{outcome="proceeded|held"}`, with a `RollingProceeded` / `RollingHeld` Event on the
workload. The NotReady timestamps live in memory: an engine restart or a leader change starts the wait over.

### Guaranteed pods

Kubernetes fixes the QoS class at creation, and a resize must keep it: a Guaranteed pod needs `requests == limits` for CPU and memory after
the resize (otherwise the API server answers "Pod QOS Class may not change as a result of resizing"; checked on Kubernetes 1.34 with a k3d
cluster, and stated in the upstream "Resize CPU and Memory Resources assigned to Containers" page, section Limitations). By default
(`guaranteed: skip`) such pods are left alone. Opt in with `guaranteed: resizeBoth` on a `TailorPolicy`, on a `containers[]` rule or with the
annotation `tailor.k8s-tailor.dev/container.<name>.guaranteed`:

- The rule applies to a container whose request equals its limit for CPU and memory (`k8s.Container.GuaranteedShape`). For it the current limit is
  no ceiling (the limit moves with the request) and `Recommend` returns one value per resource: memory is `max(request, limit)` (the limit already
  clears the observed peak with the margin and carries the OOM floor), bounded by `memory.max` unless an OOM floor is active; CPU is the request,
  or the grown limit for a censored container.
- `PlanInPlace` marks the plan `Guaranteed` and `Plan.Limits` returns the target as the limit, so request and limit are patched together
  (`reconciler/decide.go:guaranteedAdjust`). The webhook uses the same functions, so admission and live resize agree.
- CPU grows and shrinks (lowering a CFS quota in place is safe). Memory only grows: the kubelet applies a lowered memory limit best effort
  (skipped, the resize left in progress, when usage is above the new limit, and still racy right after the check), and a limit equal to a request
  sized near the observed peak turns a small error into an OOM. A held shrink is counted as `guaranteed_mem_shrink` (reconciler and webhook) and,
  in the reconciler, emits a deduplicated `ResizeSkipped` Event.
- Not covered: pods with pod-level resources (`spec.resources`) keep the `guaranteed_qos` skip, and nodes using the static CPU or memory manager
  policy cannot resize in place at all (upstream limitation). `apply: InPlacePreferred` no longer rolls such a pod for QoS reasons when the opt-in is on.

## 8. Webhook

`internal/webhook/webhook.go`. `POST /mutate` on `:8443` (TLS, min TLS 1.2, certificate hot-reloaded). For every container of a created pod (app containers and native
sidecars; regular init containers never), it takes the pod's own resources as baseline, runs `reconciler.PlanInPlace` and `Plan.Limits`, copies the existing
`resources` object and overrides only cpu and memory (so ephemeral-storage, hugepages, GPUs and DRA claims survive), and returns an RFC 6902 `add` on
`/spec/<list>/<i>/resources`. Idempotent for `reinvocationPolicy: IfNeeded`. All-or-nothing against pod-level `spec.resources`. Review bodies are capped at
8 MiB; the owner lookup has a 3 s budget. Global pause disables it.

Only a `CREATE` of a `v1/Pod` with no subresource gets a patch; any other kind, resource, operation or subresource is admitted unchanged. The webhook
**writes nothing to the store at admission** (the pod does not exist yet, and `:8443` has no caller authentication). The patch adds the annotation
`tailor.k8s-tailor.dev/admission-sized` (container to from/to, merged on reinvocation). The leader's reconciler (`recordAdmissions`) sees the pod on its next pass,
so the decision appears in the decision log, ClickHouse and `tailor_mutations_total` with a delay of at most `RECONCILE_EVERY` (30 s), once per pod, and only when
the mark agrees with the requests the pod really has. Nothing is recorded while the engine is paused or no replica holds the Lease. Dry-run (`dryRun`) admissions
receive the patch but are never recorded, since the pod is not persisted. Cert modes: `helm` (self-signed CA reused
through `lookup`; fails the render without an API server), `certManager`, `existingSecret` (`chart/templates/webhook.yaml`).

**After an engine restart.** The webhook reads the in-memory recommendations (`Engine.Get`); they are rebuilt by the first refresh, which runs
at start-up and reads the **whole window** from VictoriaMetrics, so a restarted engine is as confident after that refresh (seconds) as before
it. Until the first refresh completes the webhook serves the **warm cache** when there is one ([below](#warm-cache)); with none (first install,
cache expired or unreadable) there is no recommendation at all: the webhook admits the pod unmodified, logs it and counts
`tailor_webhook_skipped_total{reason="warming_up"}`. A container whose recommendation is not confident is admitted unmodified and counted as
`reason="not_confident"`. A pod whose template is QoS **Guaranteed** (requests == limits for CPU and memory on every container, or pod-level
resources) is admitted unmodified and counted as `reason="guaranteed_qos"` unless its policy says `guaranteed: resizeBoth` ([Guaranteed pods](#guaranteed-pods)): lowering only its requests would make it born Burstable, a
different eviction class, and the reconciler does not resize a Guaranteed pod in place by default either. No Event is emitted: at admission the pod does not exist yet. The ~10 minute mute window seen in the Istio test was
not the restart itself (an engine restarted over full history is confident on its first refresh, `TestWebhookMutatesAfterFirstRefresh`): the
`wh` pods had just been replaced, and the history of the replaced pods was dropped because their names were no longer in the cluster; now
it is attributed ([section 1](#1-metrics-sources-and-queries)).

#### Warm cache

`recommender/warm.go`. After each successful refresh the **leader** writes the recommendations the webhook can serve (effective mode includes Webhook,
resizable container, confident or an OOM recovery: the webhook's own gate) to the ConfigMap `k8s-tailor-warm` in the release namespace
(`binaryData["warm.json.gz"]`: version, save time, a sha256 of the entries, and per container only the fields `PlanInPlace` and the webhook read; no trend
series). An unchanged payload is rewritten at most every `WARM_TTL / 3`; a payload over ~900 KiB compressed is not written
(`tailor_warm_cache_writes_total{result="too_large"}`). A starting replica, leader or not, reads it before it starts the refresh loop
(`Engine.LoadWarm`, bounded to 5 s) and refuses a payload of another version, with a bad checksum, or older than `WARM_TTL` (default `15m`).

A refresh in which **every** namespace query fails (the metrics backend is down) is an error and leaves the state untouched: the previous recommendations,
and any loaded warm entries, stay. Before, it reported success with zero recommendations and replaced everything, so a metrics outage emptied the engine at the next
refresh. One failing namespace among others is still just skipped. This was found by the k3d test of the warm cache (the first refresh after a restart ran with
vmsingle down and erased the loaded entries).

What a loaded entry may do is deliberately narrow. It is returned by `Engine.Admission`, which only the webhook calls; `Engine.Get`, the recommendation
list, the API and the reconciler never see it, so **nothing is resized in place from it**. The first real refresh replaces the whole set, and the entries
also stop being served `WARM_TTL` after they were saved if no refresh has succeeded by then. `tailor_warm_cache_loaded_entries` shows how many a replica
loaded. The checksum guards against a truncated or half-written object, not against someone allowed to edit the ConfigMap: that person can already
patch workloads, and the effect of a poisoned cache is bounded by the `TTL` and by the same bounds, direction and OOM rules the webhook applies to any recommendation.
RBAC: `create` on `configmaps`, and `get`/`update` on that one name, in the release namespace (chart `tailor.warmCache`); without it the write fails once in the log per outage and
the engine keeps working cold. Backtest and decision log are unaffected.

## 9. Groups (TailorGroup)

A TailorGroup folds many workloads into one logical unit keyed by a label (or `namespace`) so usage is pooled and one recommendation applies to all of them
(`k8s/client.go:SetGroupKeys`, `matchGroupKey`). Fields: `groupBy`, `namespaces` (globs), `actuation` (`ksvc` default, `direct`, `webhook`) and
`cohort{by, select}` with `select` of `all` (default), `latest` (only the newest generation by pod creation time) or `activeByTraffic` (Knative revisions serving traffic).
In code `actuation: ksvc` yields a `ksvc` unit; any other value yields a `group` unit sized at admission. With no TailorGroup a built-in rule groups by
`serving.knative.dev/service` with `ksvc` actuation. Usage is pooled per pod (concatenated, not summed), so the quantile is a per-pod request and the peak is the hottest replica's.

## 10. CRDs (real fields)

Source: `chart/templates/crds/*.yaml`. Cluster-scoped, group `tailor.k8s-tailor.dev/v1alpha1`. Validation bounds are mirrored in `internal/policy/validate.go`
(the API answers 400 with the reason for UI/MCP writes).

**TailorConfig** (`cluster`): `paused` (bool), `defaultMode` (enum, default `Recommend`), `observedKinds` (`Deployment|StatefulSet|DaemonSet|Pod`),
`excludeNamespaces`, `observeSelector` (map), `sidecarNames` (globs).

**TailorPolicy**: `priority` (-10000..10000), `mode`, `profileRef`, `apply`, `halfLife` (duration), `season` (`daily|weekly|both|off`), `allowExceedLimit`, `direction`,
`oomOverridesDirection`, `headroomGuard`, `headroomThreshold` (0 < x <= 0.5, default 0.02), `schedule{timezone, outsideMode, windows[{days,start,end,from,until}]}`,
`selector{namespaces, matchLabels}`, `cpu` and `memory` each `{quantile (0,1], margin [0,10], min, max, limitFactor}`, `scaleUp.cooldown`, `scaleDown.{cooldown, stabilization}`,
`containers[]` rules `{match{names, classes}, exclude, mode, apply, direction, profileRef, proxyAnnotations, cpu, memory}`, `seasonGate{minCorrelation (0,1], minPairs 2..10000}`.
`memory` (policy, profile and rules) also takes `enforceLimit` (bool, no default = off). CRD schema defaults: quantile 0.9/0.95, margin 0.15, memory
`limitFactor` 1.0, cpu `limitFactor` 0.

**TailorProfile**: `cpu`, `memory`, `scaleUp`, `scaleDown`, `containers[]` (same shapes; no defaults).

**TailorGroup**: `groupBy` (required), `namespaces`, `actuation`, `cohort{by, select}`.

Annotations on workloads (or pods): `tailor.k8s-tailor.dev/mode`, `/ignore`, `/profile`, `/container.<name>.<field>`; written by the engine: `/last-applied` (per container
timestamps, JSON), `/oom-floor` (also on a Knative Service), on pods `/resize-failed` and `/rolling`; on a TailorGroup CR `/group-oom-floor`.

## 11. Engine configuration (env)

Loaded by `internal/config/config.go:Load`. In the chart most are under `tailor.env`; the full table with chart defaults is in [CONFIGURATION.md](CONFIGURATION.md).

## 12. HTTP API

Served by `internal/server/server.go` on `:8080`. Reads are unauthenticated; every non-GET `/api/*` call needs `Authorization: Bearer <token>` (constant-time compare) when
`TAILOR_API_TOKEN` is set, and fails closed with 503 when `TAILOR_API_AUTH_REQUIRED=true` and no token. Writes must be `application/json` (415), same-origin
for browsers (403) and at most 1 MiB (413). Details and the full route table: [CONFIGURATION.md](CONFIGURATION.md#engine-http-api).

## 13. Observability

`GET /metrics` (Prometheus text 0.0.4), assembled in `internal/server/server.go:metrics`, `internal/metrics/metrics.go` and `internal/server/advisor.go`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `tailor_recommended_cpu_cores`, `tailor_recommended_mem_bytes` | gauge | namespace, workload, container | recommendation |
| `tailor_current_cpu_cores`, `tailor_current_mem_bytes` | gauge | same | live request |
| `tailor_waste_usd_week` | gauge | same | weekly dollars recoverable (+) or needed (-) |
| `tailor_cpu_p90_cores`, `tailor_mem_p95_bytes` | gauge | same | decayed quantile (names are fixed even if quantiles are changed) |
| `tailor_headroom_breach_cpu_share`, `tailor_headroom_breach_mem_share` | gauge | same | share of window above the recommendation |
| `tailor_predicted_mem_bytes` | gauge | same | 24 h linear projection (only when computable) |
| `tailor_mutations_total` | counter | namespace, workload, container | decisions recorded in the engine store (in-memory counter; the HELP text says webhook but in-place decisions count as well) |
| `tailor_recommendations` | gauge | status | `learning`, `oom_risk`, `underprovisioned`, `overprovisioned`, `rightsized` |
| `tailor_workloads` | gauge | mode | workloads by effective mode |
| `tailor_paused` | gauge | | 1 when the global pause is on |
| `tailor_resizes_total` | counter | mode, result | actuation attempts (`inplace`, `rollout`, `ksvc`, `proxy-annotations`; `applied`, `failed`) |
| `tailor_resize_skipped_total` | counter | reason | evaluations held back, per tick per pod and container (read as a rate) |
| `tailor_webhook_skipped_total` | counter | reason (`warming_up`, `not_confident`, `guaranteed_qos`, `guaranteed_mem_shrink`) | pod containers the webhook admitted unmodified although their mode includes Webhook ([section 8](#8-webhook)) |
| `tailor_pods_resize_state` | gauge | state | pods with an outstanding resize (`Pending/Deferred`, `Pending/Infeasible`, `InProgress`) |
| `tailor_refresh_duration_seconds` | histogram | result | recommender refresh latency (buckets 0.5 to 300 s) |
| `tailor_last_refresh_success_timestamp_seconds` | gauge | | last successful refresh |
| `tailor_query_errors_total` | counter | backend (`vm`, `clickhouse`) | failed backend calls |
| `tailor_leader` | gauge | | 1 on the replica that runs the reconciler |
| `tailor_advisor_*` | counters, gauges | | requests, cache, LLM calls, failures, invalid and hallucinated responses, rate/budget/circuit rejections, retries, tokens, latency, breaker state, enabled ([ADVISOR.md](ADVISOR.md)) |

The bundled vmsingle scrapes every engine pod (`job_name: tailor`, pod service discovery in the release namespace, port 8080) and the kubelet (`kubelet-resource`, plus `kubelet-cadvisor` with only the two CFS counters); a Grafana dashboard is in `deploy/grafana/k8s-tailor.json`; `serviceMonitor.enabled`
renders a Prometheus Operator `ServiceMonitor`. With more than one replica, counters and `tailor_leader` are one series per replica (`pod` label); aggregate them in your queries.
Operational Events: `Rightsized`, `ResizeFailed`, `ResizeSkipped`, `ResizeInfeasible`, `RolloutFailed`.

## 14. Security model

Short form (full table with controls and residual gaps: [../SECURITY.md](../SECURITY.md)):

- The engine is a privileged, cluster-scoped controller: it can resize pods and patch workloads in every namespace it observes. The main risk is someone driving
  that power through the write API or the MCP, so both are Bearer-gated by default (the MCP twice: deny-by-default nginx sidecar plus the Bun server itself, which refuses non-canonical paths and refuses to start without a token) and writes are CSRF- and DNS-rebinding-hardened.
- RBAC is least privilege per verb actually used (section "RBAC summary" in [ARCHITECTURE.md](ARCHITECTURE.md#3-deployment-topology-in-the-cluster)); the ClusterRole
  `k8s-tailor-engine` **reads and writes**. Optional `tailor.admissionPolicy` (k8s >= 1.30) restricts what the engine's ServiceAccount
  may change to containers' `resources`, `tailor.*` annotations and 8 exact mesh proxy-sizing annotations (everything else in the spec is frozen).
- The engine never resizes itself, its ClickHouse or vmsingle: the release namespace is always excluded at runtime (`POD_NAMESPACE`), on top of the configured list.
- Reads (`/api/recommendations`, `/metrics`, ...) are unauthenticated by default and protected by NetworkPolicy; `tailor.apiAuth.protectReads=true` puts `GET /api/*` behind the same token (`/healthz`, `/livez`, `/metrics` stay open); the MCP UI shell at `/` is public but carries no data.
- Containers run non-root with read-only root filesystems, dropped capabilities and `RuntimeDefault` seccomp.
- The LLM advisor sees only aggregated metrics (optionally anonymized names), never env vars, logs, secrets, annotations or specs.

## 15. State and persistence

| State | Where | Survives engine restart |
|---|---|---|
| Configuration | CRs and annotations (etcd) | yes |
| Cooldown | `tailor.k8s-tailor.dev/last-applied` on pods (and the template after a rollout) | yes |
| Learned OOM floor | `tailor.k8s-tailor.dev/oom-floor` on Deployments, StatefulSets, DaemonSets and Knative Services that actuate; `tailor.k8s-tailor.dev/group-oom-floor` on the TailorGroup CR for `group` units | yes; memory-only for bare Pods (by design) |
| Decision history | ClickHouse `decisions` table (TTL `CH_RETENTION_DAYS`), else in-memory ring of 200 | yes with ClickHouse |
| Failure backoff | `tailor.k8s-tailor.dev/resize-failed` on the pod (time, reason) | yes |
| In-progress disruptive rolling | `tailor.k8s-tailor.dev/rolling` on each restarted pod | yes (resumed behind the readiness gate) |
| Scale-down stabilization history | engine memory | rebuilt by the first refresh (replayed from the usage history) |
| Usage history | VictoriaMetrics PVC | yes (including pods that were replaced since, see [section 1](#1-metrics-sources-and-queries)) |
| Recommendations and confidence | engine memory, rebuilt from the usage history | rebuilt by the first refresh after start-up |
| Webhook-servable recommendations | ConfigMap `k8s-tailor-warm` (release namespace), written by the leader | yes, for `WARM_TTL`; webhook only, until the first refresh ([warm cache](#warm-cache)) |

ClickHouse batching: up to 500 rows or every 2 s from a bounded queue of 10,000 (oldest dropped); on SIGTERM the engine flushes the buffer as the last step of the ordered shutdown (stop actuating, release the Lease, drain the servers, flush; see ARCHITECTURE section 10); a hard kill can still lose up to about 2 s of decisions
(the Kubernetes Event is still emitted).
