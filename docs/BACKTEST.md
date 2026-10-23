# Backtest: k8s-tailor, KRR-style and VPA-style reference baselines, static request

`k8s-tailor backtest` does an **offline, deterministic** replay of a usage time
series (CPU and memory per pod of one container) through four right-sizing
strategies and scores all of them on the same simulation.

The goal is to put numbers on a direct question: *how do k8s-tailor's
recommendations compare with those of a VPA-style and a KRR-style baseline?* The
short answer, on the current synthetic scenarios, is **it depends on the load
pattern**: the engine has lower CPU risk on periodic load, is comparable to the
VPA-style baseline on step and leak patterns, and on bursty load it stopped
oscillating but accepts more risk than the KRR-style baseline (see
[Results](#results-seed-42) and
[Flapping fix on bursts](#flapping-fix-on-bursts)).

> This is a **simulation**, not production. Read
> [Limitations](#honest-limitations) before using any number from here as
> evidence.

## Usage

```sh
# All synthetic scenarios (fixed seed → byte-for-byte identical output)
k8s-tailor backtest -synthetic all

# One scenario; JSON report to a file, markdown on stdout
k8s-tailor backtest -synthetic bursty,memory-leak -seed 7 -out-json bt.json

# Export the synthetic scenario as CSV (to inspect/edit and reuse)
k8s-tailor backtest -synthetic step-change -export-csv step.csv -format none

# Real data from a file (CSV or JSONL)
k8s-tailor backtest -input usage.csv -cpu-request 1 -mem-request-mi 1024 -mem-limit-mi 1024

# Straight from VictoriaMetrics/Prometheus (reuses internal/vm)
k8s-tailor backtest -vm-url http://vmsingle:8428 -vm-namespace portal \
  -vm-pod-regex 'gateway-.*' -vm-container app -vm-range 14d \
  -cpu-request 1 -mem-request-mi 1024 -mem-limit-mi 1024
```

Main flags (all in `k8s-tailor backtest -h`):

| flag | default | meaning |
|---|---|---|
| `-synthetic` / `-input` / `-vm-url` | — | data source (exactly one) |
| `-seed` | 42 | synthetic generator seed |
| `-days` / `-step` | 14 / 5m | duration and step of the synthetic grid |
| `-warmup` | 2d | history seen before any strategy acts (not scored) |
| `-strategies` | all | `static,krr,vpa,k8s-tailor` (static always runs: it is the cost reference) |
| `-engine-refresh` | 1h | replay interval of the real `Engine.Refresh` (production: 60s) |
| `-krr-every` | 24h | how often the KRR report is (assumed to be) reapplied |
| `-price-cpu` / `-price-gb` | `PRICE_CPU_CORE_HR` / `PRICE_GB_HR` | price per core·h and GiB·h **of request** |
| `-format` | md | `md`, `json` or `none` on stdout; `-out-json`/`-out-md` write files |

The engine configuration (window, quantiles, margin, half-life, `MIN_SAMPLES`,
OOM learning…) comes from `config.Load()` — that is, the **same environment
variables** as the product. Policies (`TailorPolicy`) are not loaded: the default
fallback of `policy.Store.Resolve` applies (season `daily`, direction `Both`,
cooldowns 30s/1h, `scaleDown.stabilization` 15 min).

### Input format

CSV with a header (any column order) or JSONL with the same keys:

```
timestamp,namespace,workload,pod,container,cpu_cores,mem_bytes[,cpu_request,cpu_limit,mem_request,mem_limit]
```

- `timestamp`: unix seconds or RFC3339. `cpu_cores`: usage in cores (the rate, not
  the counter). `mem_bytes`: working set.
- The optional request/limit columns (cores/bytes) say what the workload ran
  with **before** any strategy; the first non-empty value is used. Anything
  missing comes from the `-cpu-request` etc. flags.
- Each `namespace/workload/container` becomes an independent backtest. Samples
  are snapped to the `-step` grid; two samples from the same pod in the same
  bucket keep the **maximum**; missing points stay empty (pod absent).

## What is simulated

At each grid point *i*: (1) each pod's usage is measured against the resources in
effect; (2) the strategy observes point *i* and returns the resources it wants
**from *i+1* onward** (no look-ahead — there is a property test for that); (3)
changes during warmup are discarded.

The trace is the **demand**. What a strategy **observes** (its `Tick` and, for the
engine, the fake VictoriaMetrics) is the demand **capped at the CPU limit it
applied** — the CFS quota: a container cannot use more CPU than its limit, so its
measured usage reads ~the limit while the real demand is unknown (censored data).
Each strategy gets its own observed copy of the trace (`Trace.observedCopy`, filled
tick by tick in `replay`); the metrics are scored on the demand. Without a CPU
limit nothing is capped: the six scenarios that existed before `throttled-cap` set
none (`cpu-greedy`, added later, has one), and their output is byte-for-byte identical with and without the cap.

### Strategies

**`static`** — the current request, never changes. Cost reference (rel. cost = 1).

**`k8s-tailor`** — the **real engine, not a reimplementation**:
`recommender.Engine.Refresh` → `fetchAndCollapse` → `recommender.Recommend` →
learned OOM floor → scale-down stabilization, unmodified. The adapter
(`internal/backtest/engine.go`) assembles:
- a fake cluster (client-go `fake`) with a Deployment whose *template* carries the
  initial resources (static, as in production with in-place resize) and pods with
  the applied resources and, when the simulation produces an OOM,
  `lastState.terminated.reason=OOMKilled`;
- a fake VictoriaMetrics (`httptest`) that answers the engine's `query_range`
  calls from the series, **never** returning a sample later than the current tick.

The engine runs on a **simulated clock** (`Engine.SetClock`): at tick *i* the
engine's "now" is `T(i)` — end of the query window, seasonal forecast target,
OOM-floor TTL and stabilization window are all evaluated in trace time. There is
no longer any time shift and the engine sees the current tick.

The **actuation decision** is the **real reconciler code**: `reconciler.Decide`
(confidence gate, `Direction.Gate`, drift tolerance 15%/20m and 15%/16Mi,
30s cooldown to scale up / 1h to scale down, limit never reduced in place, limit
raise by OOM floor, CPU limit raise on censored CPU) evaluated in simulated time, with the `cpuStr`/`memStr`
quantization (`QuantizeCPU`/`QuantizeMem`). Guaranteed pods are not resized (the synthetic scenarios are Burstable).
`reconcileNS` uses the same pieces (`PlanInPlace`, `Plan.Limits`, `Cooled`) and
adds the pod-dependent guards (pending resize, QoS, failure backoff, PDB).
Replacing the old port with `Decide` gave **byte-for-byte identical** output on
seeds 1–5.

Two more paths of the reconciler are replayed, for traces that declare a `Disruption`
(today the `restart-rolling` scenario; every other scenario is Burstable with resizes
that restart nothing, and its output is unchanged):

- **The disruptive rolling** (resizePolicy `RestartContainer`). The replay keeps each
  replica's own resources and readiness. The in-place engine decides per replica with
  the reconciler's own `PlanInPlace`, `Cooled` and `RollingGate` (the gate `reconcileNS`
  calls: every other replica Ready, the restart budget of the cycle, the PDB's
  `disruptionsAllowed` = Ready pods above `minAvailable`); a NotReady replica goes at
  once, a rolling in progress continues without waiting the cooldown, and a restarted
  replica is NotReady for `readySteps` ticks.
- **The rollout path** (`k8s-tailor-rollout`, runs only on such traces):
  `reconciler.PlanRollout` against the template (which follows the target) with the
  10 minute `MinRolloutInterval` floor; the replay then rolls the replicas to the new
  template behind the same PDB budget, like a Deployment's native rolling update.
- **The reference strategies** (KRR, VPA) only name a target; the replay rolls the
  replicas to it with the same PDB-bound pacing (the updater's evictions and a rollout
  both honor the PDB). One restart per replica per change, for every strategy.

**`krr`** — a port of `krr simple` from [robusta-dev/krr](https://github.com/robusta-dev/krr)
(`robusta_krr/strategies/simple.py` and `core/runner.py`, branch `main` read on
2026‑10‑21; latest release v1.31.1), used as a KRR-style reference baseline:
- CPU = **maximum across pods** of each pod's `quantile_over_time(0.95, rate)`
  (Prometheus linear interpolation — uses `recommender.Quantile`, same semantics);
  **CPU limit removed**;
- memory = maximum across pods of `max_over_time` × 1.15; **request = limit**;
- 336h history, `points_required=100`, rounding up to 1m / 1MiB,
  minimums 10m / 100MiB, `use_oomkill_data=false` (default).
- KRR is a report, not a controller. The backtest assumes someone reapplies the
  report **every day** (`-krr-every 24h`) — a generous assumption for KRR.

**`vpa`** — a faithful port of the recommender + updater from
[kubernetes/autoscaler, tag `vertical-pod-autoscaler-1.4.1`](https://github.com/kubernetes/autoscaler/tree/vertical-pod-autoscaler-1.4.1/vertical-pod-autoscaler),
used as a VPA-style reference baseline:
- decaying exponential histogram (`util/histogram*.go`, `decaying_histogram.go`):
  CPU from 0.01 to 1000 cores, memory from 1e7 to 1e12 bytes, buckets 5% larger,
  **24h half-life**, renormalization after 2^100, percentile = bucket **end**;
- CPU: constant weight `minSampleWeight=0.1` per sample (in 1.4.x it is no longer
  weighted by request); memory: **one peak per container per 24h window**
  (`addMemorySample`, with subtract/add of the peak);
- estimators (`logic/recommender.go`, `estimator.go`): target P90, lower P50,
  upper P95 (CPU and memory), **15% margin** on all, confidence multiplier
  `(1+1/conf)` on upper and `(1+0.001/conf)^-2` on lower, minimums 25m /
  250MiB per pod, `ResourceAmount` truncation;
- OOM: `RecordOOM` with `max(request+100MiB, request×1.2)`;
- updater (`updater/priority/*.go`): evicts if the request is outside
  `[lower, upper]`; inside the interval, only if the pod has lived ≥12h **and** the
  summed relative difference is ≥ 0.1; *quick OOM* (<10 min) forces it; does not
  evict with fewer than 2 replicas (`--min-replicas`). Admission: request = target,
  limit proportional to the template's limit/request ratio (`RequestsAndLimits`).
- The 5 min trace is "resampled" to the metrics-server's 1 min cadence (weight
  and count ×5), so confidence evolves as it would in a real cluster.

### Metrics (post-warmup window only)

| metric | definition |
|---|---|
| CPU>request (pod·h, %) | pod-hours / fraction of samples with CPU usage > request — throttling/contention risk (the excess is not guaranteed by the scheduler) |
| CPU>limit (pod·h) | pod-hours where the CPU **demand** exceeds the CPU limit in effect (CFS throttling; the measured usage is capped there). 0 without a CPU limit, so it is omitted from the tables of the scenarios that have none |
| OOMs | per-pod episodes where memory **crosses** the ceiling: the limit if there is one, otherwise the request |
| mem>ceiling (pod·h) | time above that ceiling |
| waste | per-sample mean of `max(0, request − usage)`, CPU in cores and memory in GiB |
| changes/day | number of times the applied resources changed ÷ evaluated days. For the VPA each change is an **eviction + restart of all replicas**; for k8s-tailor it is an in-place resize |
| rel. cost | Σ (CPU request × price + mem request × price) × time, divided by that of `static` |

## Results (seed 42)

`k8s-tailor backtest -synthetic all` (14 days, 5 min step, 2 day warmup, 12
evaluated days, engine refresh 1h, default config). Real output, unedited
(k8s-tailor rows **after** the flapping fix; static/KRR/VPA did not change — the
"before" is in [Flapping fix on bursts](#flapping-fix-on-bursts)):

**diurnal-weekly** — 3 replicas, daily cycle + weekend, initial 1000m / 1024Mi (lim 1024Mi)

| strategy | CPU>req (pod·h) | CPU>req (%) | OOMs | mem>ceiling (pod·h) | CPU waste | mem waste (GiB) | changes/day | rel. cost |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| static | 0.0 | 0.00 | 0 | 0.0 | 0.741 | 0.579 | 0.00 | 1.000 |
| krr | 31.7 | 3.67 | 0 | 0.0 | 0.263 | 0.173 | 1.00 | 0.529 |
| vpa | 20.7 | 2.39 | 0 | 0.0 | 0.301 | 0.187 | 1.00 | 0.565 |
| k8s-tailor | 3.5 | 0.41 | 0 | 0.0 | 0.337 | 0.147 | 0.92 | 0.593 |

**bursty** — ~6 bursts/day of 5–30 min, 3–8× the base, initial 500m / 768Mi (lim 1024Mi)

| strategy | CPU>req (pod·h) | CPU>req (%) | OOMs | mem>ceiling (pod·h) | CPU waste | mem waste (GiB) | changes/day | rel. cost |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| static | 58.3 | 6.75 | 0 | 0.0 | 0.353 | 0.334 | 0.00 | 1.000 |
| krr | 29.5 | 3.41 | 0 | 0.0 | 0.655 | 0.176 | 1.00 | 1.499 |
| vpa | 61.5 | 7.12 | 0 | 0.0 | 0.143 | 0.185 | 1.08 | 0.589 |
| k8s-tailor | 62.3 | 7.20 | 0 | 0.0 | 0.141 | 0.147 | 2.08 | 0.577 |

**step-change** — a mid-run deploy changes CPU 0.20→0.55 and memory 300Mi→650Mi, initial 500m / 512Mi (lim 1024Mi)

| strategy | CPU>req (pod·h) | CPU>req (%) | OOMs | mem>ceiling (pod·h) | CPU waste | mem waste (GiB) | changes/day | rel. cost |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| static | 321.5 | 37.21 | 0 | 0.0 | 0.136 | 0.087 | 0.00 | 1.000 |
| krr | 168.3 | 19.48 | 3 | 72.0 | 0.079 | 0.118 | 1.00 | 0.937 |
| vpa | 50.3 | 5.83 | 0 | 0.0 | 0.170 | 0.137 | 0.42 | 1.139 |
| k8s-tailor | 52.0 | 6.02 | 0 | 0.0 | 0.136 | 0.101 | 0.50 | 1.081 |

**memory-leak** — +110Mi/day, reset by a deploy every 5 days, initial 500m / 512Mi (lim 1024Mi)

| strategy | CPU>req (pod·h) | CPU>req (%) | OOMs | mem>ceiling (pod·h) | CPU waste | mem waste (GiB) | changes/day | rel. cost |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| static | 0.0 | 0.00 | 0 | 0.0 | 0.350 | 0.040 | 0.00 | 1.000 |
| krr | 27.7 | 4.82 | **26** | 15.6 | 0.065 | 0.340 | 0.92 | 0.584 |
| vpa | 0.8 | 0.14 | 0 | 0.0 | 0.097 | 0.289 | 1.58 | 0.629 |
| k8s-tailor | 2.3 | 0.41 | 0 | 0.0 | 0.089 | 0.307 | 0.33 | 0.619 |

**heterogeneous** — 4 replicas, hot shard 2.2×, cold 0.4×, initial 1000m / 1024Mi (lim 1024Mi)

| strategy | CPU>req (pod·h) | CPU>req (%) | OOMs | mem>ceiling (pod·h) | CPU waste | mem waste (GiB) | changes/day | rel. cost |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| static | 0.1 | 0.01 | 0 | 0.0 | 0.789 | 0.727 | 0.00 | 1.000 |
| krr | 13.1 | 1.14 | 0 | 0.0 | 0.616 | 0.272 | 1.00 | 0.794 |
| vpa | 81.2 | 7.05 | 0 | 0.0 | 0.389 | 0.267 | 1.92 | 0.583 |
| k8s-tailor | 14.5 | 1.26 | 0 | 0.0 | 0.492 | 0.230 | 4.42 | 0.679 |

**memory-spikes** — 3 replicas, ~300Mi with rare, short spikes (10–20 min, 680–820Mi) above the
initial limit; initial 500m / 512Mi (lim 640Mi). A real-OOM scenario (added after the
tables above; numbers from the same command, seed 42):

| strategy | CPU>req (pod·h) | CPU>req (%) | OOMs | mem>ceiling (pod·h) | CPU waste | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 0.0 | 0.00 | 12 | 3.8 | 0.325 | 0.206 | 0.00 | 1.000 | 500m / 512Mi (lim 640Mi) |
| krr | 38.2 | 4.43 | 0 | 0.0 | 0.092 | 0.563 | 0.75 | 0.667 | 264m / 916Mi (lim 916Mi) |
| vpa | 4.8 | 0.56 | 3 | 0.2 | 0.121 | 0.405 | 0.33 | 0.684 | 296m / 933Mi (lim 1166Mi) |
| k8s-tailor | 8.1 | 0.94 | 0 | 0.0 | 0.118 | 0.288 | 0.42 | 0.651 | 290m / 640Mi (lim 960Mi) |

The OOM floor raises only the limit (640 → 960Mi = 640 × 1.5). OOMs during warmup do not
count in the evaluated window. While writing this scenario the backtest exposed a real bug:
the same OOM, still in the pod's `lastState`, was relearned on every refresh with the limit
**already raised**, and the floor grew ×1.5 per refresh without bound. Fixed: each kill
(`finishedAt`) is learned once. The regression test is `TestEngineOnMemorySpikes`.

**memory-spikes without a limit** (`TestEngineNoLimitOOMConverges`): same trace with the
container having no memory limit (512Mi request) — the OOM is counted against the request
and the engine reacts by raising the **request** (it never introduces a limit). It runs in
two modes: pure in-place (fixed template) and `TemplateFollowsApplied` (the template follows
what was applied, like `apply: Rollout`/GitOps — the engine reads its own output as
"current"). Post-OOM convergence assertion: after the last simulated OOM the request rises
at most once and stays ≤ 4× the peak. Before the fix the template-follows mode ended at
~3×10¹¹ Mi (×1.5 per refresh, 54 changes); after it, 536Mi in both modes (floor = 357Mi ×
1.5), 3 OOMs against 15 for static. The synthetic tables above (`-synthetic all -seed 42`)
were identical after the fix.

**throttled-cap** — 2 replicas of a mesh proxy started at request 20m / CPU limit 50m
(the Istio test's `optin` proxy, [dev/istio/README.md](../dev/istio/README.md)), demand
100–280m over a daily cycle (280m: what the real proxy used once its limit was lifted).
The measured usage is capped at the applied CPU limit (see [What is simulated](#what-is-simulated)).
Same command, seed 42; "before" is the engine at the commit preceding the censored-CPU
change, replayed with the same (capped) simulation:

| strategy | CPU>req (pod·h) | CPU>req (%) | CPU>limit (pod·h) | OOMs | CPU waste | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 576.0 | 100.00 | 576.0 | 0 | 0.000 | 0.020 | 0.00 | 1.000 | 20m / 64Mi (lim 50m / 256Mi) |
| krr | 86.2 | 14.97 | 0.0 | 0 | 0.093 | 0.055 | 1.00 | 10.076 | 296m / 100Mi (lim 100Mi) |
| vpa | 49.2 | 8.54 | 6.1 | 0 | 0.122 | 0.201 | 0.25 | 12.036 | 323m / 250Mi (lim 807m / 1000Mi) |
| k8s-tailor **before** | 576.0 | 100.00 | 576.0 | 0 | 0.000 | 0.009 | 0.08 | 2.020 | 50m / 53Mi (lim 50m / 256Mi) |
| k8s-tailor **after** | 28.7 | 4.99 | 5.7 | 0 | 0.123 | 0.009 | 0.75 | 11.339 | 321m / 53Mi (lim 383m / 256Mi) |

Before, the engine measured ~50m forever, recommended the limit itself and never
touched the limit: stuck, throttled 100% of the time. After, the censored-CPU rule
([TECHNICAL.md](TECHNICAL.md#censored-cpu)) raises the limit 50 → 75 → 113 → 170 → 255 → 383m
(×1.5 per step) with the request following, and ends at the throttling level of the
VPA-style baseline at a slightly lower cost. *Notes:* KRR-style escapes by **removing**
the CPU limit; VPA-style escapes because it scales the limit with the request (template
ratio 2.5). With `CPU_CENSOR_MAX_FACTOR=4` (the brake when the policy sets no `cpu.max`)
the engine stops at 200m (4 × 50m) and CPU>limit is 263.8 pod·h: the default 8 was chosen
because the only measured real case needed 5.5× its starved limit. `TestEngineEscapesThrottledCap`
guards it.

### autoQuantile on bursty

`autoQuantile` (policy field, or `AUTO_QUANTILE=true` for the backtest, which loads the engine
configuration from the environment) raises the CPU quantile to 0.95 when the burstiness index
(p99 / p90) is at or above 3. It is off by default, so every table above is the default. With the
opt-in, `bursty` (14 days, refresh 1h, seeds 42 and 1-5), CPU>request % and relative cost:

| seed | k8s-tailor default | k8s-tailor autoQuantile | KRR-style | VPA-style |
|---:|---|---|---|---|
| 42 | 7.20% / 0.577 | 4.78% / 1.262 | 3.41% / 1.499 | 7.12% / 0.589 |
| 1 | 7.66% / 0.548 | 3.38% / 1.408 | 3.44% / 1.411 | 7.58% / 0.565 |
| 2 | 7.80% / 0.700 | 4.78% / 1.305 | 4.03% / 1.525 | 7.92% / 0.678 |
| 3 | 7.66% / 0.450 | 4.09% / 1.459 | 5.51% / 1.281 | 7.59% / 0.489 |
| 4 | 8.46% / 0.852 | 6.41% / 1.149 | 4.20% / 1.526 | 8.24% / 0.870 |
| 5 | 7.48% / 0.503 | 3.82% / 1.520 | 4.87% / 1.412 | 7.53% / 0.564 |

The risk falls to within 2 percentage points of the KRR-style baseline on five of the six seeds (seed 4:
+2.21pp, the exception), at a cost that is about 2x the default's and close to KRR's (between 25% below
and 14% above it, seed 3 being the highest). The other seven scenarios' k8s-tailor rows are identical with and without
the opt-in on all six seeds (the index stays below 3 for them). `TestEngineAutoQuantileOnBursty`
guards seed 42.

**restart-rolling** — 3 replicas with the diurnal-weekly load whose containers restart on
every resize (resizePolicy `RestartContainer` for CPU and memory), a PodDisruptionBudget
keeping 2 Ready (budget 1) and `readySteps` 1. The table adds **restarts/day** (container
restarts the applied changes cause, over all replicas) and **max not Ready** (the most
replicas not Ready at the same tick; the PDB allows 1). Restarts per day, 14 days, refresh 1h:

| seed | krr | vpa | k8s-tailor (in place) | k8s-tailor-rollout |
|---:|---:|---:|---:|---:|
| 42 | 2.25 | 2.75 | 1.00 | 1.00 |
| 1 | 2.75 | 2.00 | 1.75 | 1.75 |
| 2 | 2.50 | 2.75 | 2.25 | 2.25 |
| 3 | 3.00 | 3.25 | 2.50 | 2.50 |
| 4 | 2.75 | 3.25 | 3.50 | 3.50 |
| 5 | 3.00 | 1.75 | 1.75 | 1.75 |

`max not Ready` is 1 for every strategy on every seed (static: 0 restarts). The engine restarts
less than both reference strategies on seeds 42, 1, 2 and 3, as much as VPA-style on seed 5 and
**more** on seed 4 (3.50 against 2.75 and 3.25): it changes the request when its recommendation
drifts, and each change costs one restart per replica when the container restarts on resize, so
with `RestartContainer` the number of changes is what is paid for. The in-place and the rollout
paths take the same decisions here (the same plan; the 10 minute floor never binds at hourly
refreshes). On seed 42 the in-place engine's CPU>request is 0.38% against 3.12% (KRR) and 1.76%
(VPA) at a relative cost of 0.561 (0.531 and 0.560). A resize that restarts nothing (the default
`NotRequired`) costs the engine 0 restarts per day, which is the reason the in-place path exists.
`TestRestartRollingScenario` and `TestRollingPacesRestartsBehindThePDB` guard the PDB bound and
the one-restart-per-replica-per-change accounting.

**cpu-greedy** — 2 replicas of a busy loop (demand 10 cores, unreachable) started at request
100m / CPU limit 200m; the measured usage is always the applied limit, so every raise is eaten at
once. Seed 42; "before" is the same engine with `CPU_CENSOR_MAX_STEPS=1000` (no convergence rule):

| strategy | CPU>req (pod·h) | CPU>req (%) | CPU>limit (pod·h) | OOMs | CPU waste | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 576.0 | 100.00 | 576.0 | 0 | 0.000 | 0.031 | 0.00 | 1.000 | 100m / 128Mi (lim 200m / 256Mi) |
| krr | 91.9 | 15.96 | 0.0 | 0 | 0.684 | 0.023 | 1.00 | 84.928 | 10810m / 119Mi (lim 119Mi) |
| vpa | 120.0 | 20.83 | 96.0 | 0 | 1.839 | 0.151 | 0.58 | 89.068 | 12965m / 250Mi (lim 25930m / 500Mi) |
| k8s-tailor **before** | 576.0 | 100.00 | 576.0 | 0 | 0.000 | 0.019 | 0.58 | 13.740 | 1600m / 115Mi (lim 1600m / 256Mi) |
| k8s-tailor **after** | 576.0 | 100.00 | 576.0 | 0 | 0.000 | 0.019 | 0.33 | 5.905 | 675m / 115Mi (lim 675m / 256Mi) |

Before, the censored-CPU rule grew the limit to its brake (200m x `CPU_CENSOR_MAX_FACTOR` 8 = 1600m)
for a demand that never ends. After, it parks at the third step (200 → 300 → 450 → 675m) and marks
the container `cpuGreedy`; the cost falls from 13.7x to 5.9x static. Seeds 1–5 give the same
two final values (1600m / 675m). KRR-style and VPA-style follow the unbounded demand (the usage they see
is not capped by the limit they remove, or scales with the template ratio). The other seven scenarios'
k8s-tailor rows are identical to the run before the rule, seeds 42 and 1–5; `throttled-cap` still
reaches 383m because its demand dips below 80% of the limit between daily peaks, which resets the run.
`TestEngineParksCPUGreedy` guards it.

**Robustness**: see the before/after table with seed 1–5 averages below; the
conclusions hold on all seeds.

### Reading the results

- **Where the engine does well.** Regular diurnal/weekly load: CPU risk 6–9×
  lower than the VPA-style/KRR-style baselines (0.41% vs 2.4%/3.7%) at a cost 5%
  higher than the VPA-style baseline and 12% higher than the KRR-style one (0.593
  vs 0.565/0.529) — it is the seasonal forecast anticipating the peak. *Bias:*
  the generator produces clean periodicity, which is exactly what that forecast
  exploits; real load is less regular (and the forecast is now only used if the
  periodicity is consistent — see below). On `memory-leak` and `step-change` it
  stays close to the VPA-style baseline on risk (0.41% vs 0.14%; 6.0% vs 5.8% —
  the VPA-style baseline is slightly lower), with no OOM, slightly cheaper, and
  with far fewer changes than the VPA-style baseline on the leak (0.33 vs 1.58
  per day — and the VPA's are evictions).
- **`bursty`: stopped oscillating, but accepts more risk than the KRR-style
  baseline.** Before, the engine oscillated the CPU request between ~160m and ~1
  core almost hourly (11–14 changes/day; 18/day with a 15 min refresh). Now it
  makes 1.5/day on average (max 2.75 on one seed; 2.0/day with a 15 min refresh)
  and stays stable at ~P90×1.15 ≈ 170m. Honest consequence: risk rose from 6.5%
  to 7.8% (seeds 1–5), above the earlier working target of KRR+2pp (KRR 4.4%) and
  at the VPA-style level (7.8%), at a cost of 0.61 (before 0.94; KRR 1.43). The
  earlier "lower" risk was luck: the engine happened to be at 1 core at random
  hours. See [why the risk target is not reachable with the
  default](#why-the-risk-target-on-bursty-is-not-reachable-with-the-default).
- **`heterogeneous`.** The engine is at the KRR-style level on risk (1.3% vs 1.0%)
  at lower cost (0.66 vs 0.79), and the VPA-style baseline has the highest risk
  (7%) because it aggregates all replicas into a single histogram and the hot
  shard lives above the P90 of the set. The 4.5 changes/day are **not flapping**:
  the daily correlation is high, the forecast is active and the request follows
  the cycle (rises before the peak, falls in steps of ≥15% with a 1h cooldown). A
  larger stabilization window cuts this (6h → 3.2/day, cost 0.73) — it is a
  policy decision, not a bug.
- **KRR-style** with `request = limit = max+15%` is risky for growing memory: 26
  OOMs on the leak and 3 on step-change (the limit sits at the past maximum until
  the next report). On `bursty` it is the one that reduces risk well, because the
  per-pod P95 captures the bursts — at 1.5× the cost of static.
- **VPA-style** is the cheapest on `bursty`/`heterogeneous` precisely because it
  accepts more risk, and each of its changes restarts the pods.

Conclusion: the backtest does **not** support a general claim that k8s-tailor is
better or worse than either baseline. It supports: lower or comparable risk on
periodic, step and leak load; on bursts, stable and cheap like the VPA-style
baseline, with higher risk than the KRR-style baseline unless the policy asks for
a higher quantile.

## Flapping fix on bursts

Diagnosis (verified in the `Signals`): (1) the seasonal forecast (P90 of the same
time of day ±30 min) treated random bursts as a daily pattern; (2) the
change-point detector (50% threshold on the mean) trimmed the window right after
a burst and the quantile jumped from ~0.15 to ~0.57; (3) `ScaleDownStabilization`
was resolved in the policy and not used; (4) the engine read `time.Now`.

What changed (`internal/recommender/stability.go`, `recommender.go`):

1. **Injectable clock** (`Engine.SetClock`) and the pure function
   `recommender.Recommend(RecommendInput)`; the backtest uses the simulated clock
   (no time shift) and `reconciler.Decide` (no port).
2. **Seasonality gate**: the forecast only applies if the series is periodic — the
   Pearson correlation between the hourly mean and itself one season earlier
   (lag 24h; lag 168h for `weekly`, falling back to 24h when the window does not
   have a week of pairs) must be ≥ 0.5, with ≥ 24 hourly pairs. Random bursts
   give ~0; the synthetic daily cycle gives > 0.9. The result goes in
   `Signals.cpuSeason`/`memSeason` (`off` when rejected).
3. **Burst-robust change-point**: the detector runs on the rolling median
   (±6 samples ≈ 65 min) of the per-timestamp mean series. The median removes
   pulses of up to ~30 min and preserves steps; a cut requires a change that
   persists for more than half the filter window and covers ¼ of the window (the
   previous `minSeg`).
4. **Real scale-down stabilization** (HPA semantics): the engine keeps, per
   container, the *raw* recommendations of the `ScaleDownStabilization` window and
   publishes `max(current raw, raws in the window)`; increases pass immediately.
   `Signals.rawCpuRequest/rawMemRequest/stabilized` expose this. The default is
   still 15 min: with a 1h (or 15 min) refresh in the backtest it is inert — its
   real effect is in production (60s refresh).

Contribution of each piece on `bursty` (seed 1–5 averages, refresh 1h):

| variant | CPU>req (%) | changes/day | rel. cost |
|---|---:|---:|---:|
| before (with time shift) | 6.53 | 12.63 | 0.937 |
| simulated clock only | 6.54 | 11.85 | 0.918 |
| + median change-point | 6.57 | 11.75 | 0.904 |
| + seasonality gate (no filter) | 7.78 | 1.65 | 0.624 |
| **everything (final)** | **7.81** | **1.52** | **0.611** |

The seasonality gate is what solves it; the median filter is a small gain (and
avoids the spurious window cut on series with rare bursts).

### Before/after, all scenarios (k8s-tailor, seed 1–5 averages, refresh 1h)

| scenario | CPU>req % before → after (range) | OOMs | changes/day before → after (max) | rel. cost before → after | KRR / VPA reference (CPU>req %, cost) |
|---|---|---:|---|---|---|
| diurnal-weekly | 0.54 → 0.58 (0.10–1.08) | 0 → 0 | 0.85 → 0.90 (1.25) | 0.572 → 0.572 | 3.31% 0.531 / 2.32% 0.561 |
| bursty | 6.53 → **7.81** (7.48–8.46) | 0 → 0 | 12.63 → **1.52** (2.75) | 0.937 → **0.611** | 4.41% 1.431 / 7.77% 0.633 |
| step-change | 5.76 → 5.69 (5.43–6.29) | 0 → 0 | 0.50 → 0.50 | 1.101 → 1.105 | 18.51% 0.945 / 5.69% 1.140 |
| memory-leak | 0.71 → 0.71 | 0 → 0 | 0.37 → 0.37 | 0.611 → 0.611 | 4.96% 0.578 / 0.56% 0.625 |
| heterogeneous | 1.31 → 1.31 (0.97–1.63) | 0 → 0 | 4.43 → 4.50 (4.67) | 0.664 → 0.664 | 0.95% 0.791 / 7.30% 0.573 |

With `-engine-refresh 15m` (seeds 1–5): `bursty` 18.42 → 1.98 changes/day
(max 3.17 on one seed), risk 6.28% → 7.74%, cost 1.003 → 0.617;
`heterogeneous` 5.57 → 5.35/day; the other scenarios practically the same.

Regressions, stated plainly: CPU risk on `bursty` worsened by 1.3pp (see below);
`diurnal-weekly` +0.04pp and +0.05 changes/day and `heterogeneous` +0.07
changes/day are simulated-clock noise (the engine now sees the current tick, not
2.5 min late — the effect already shows in the "simulated clock only" variant).
No new OOMs.

### Why the risk target on `bursty` is not reachable with the default

The targets set were ≤ 3 changes/day (**met**: 1.52, max 2.75) and risk ≤ KRR+2pp
(**not met**: 7.81% vs 6.41%; missed on all 5 seeds, by 1.6–4.3pp). Reason:
~7% of the time is in a burst (6/day × ~17 min) and the bursts are 4–9× the base.
A **stable** request at the default CPU quantile (P90) sits at the base and, by
construction, leaves every burst above it — risk ≈ fraction of time in a burst. To
cover part of the bursts a quantile inside the burst mass is needed, that is,
~P93+:

| `CPU_QUANTILE` (bursty, seeds 1–5) | CPU>req (%) | changes/day (max) | rel. cost |
|---|---:|---:|---:|
| 0.90 (default) | 7.81 | 1.52 (2.75) | 0.611 |
| 0.93 | 5.77 | 2.47 (3.42) | 1.141 |
| 0.95 | 3.68 | 1.20 (1.42) | 1.507 |
| KRR (per-pod P95) | 4.41 | 0.97 | 1.431 |

With P95 the engine reaches lower risk than the KRR-style baseline (3.7% vs
4.4%) at a similar cost. This is a policy choice (per-workload quantile), not
something the engine should infer by chasing bursts — that is exactly what caused
the flapping. The synthetic generator and the default quantile were not changed.

## Honest limitations

1. **Simulation ≠ production.** Usage is exogenous: changing the request does not
   change the usage that comes after. There is no real throttling (CPU above the
   request is not throttled; the CPU limit is not modeled), no restart after an
   OOM (in a real leak, the OOM zeroes the memory; here the series stays above
   the ceiling and counts a single episode), no scheduling, preemption, node
   pressure, HPA, or application latency.
2. **Synthetic scenarios are the author's choices.** Shapes, amplitudes and
   initial resources were defined a priori and not calibrated against real
   clusters; a different generator could reverse conclusions. Run with real data
   (`-input`, `-vm-url`) before concluding anything.
3. **"CPU > request" is a risk proxy**, not a throttling measure: without a CPU
   limit, the excess runs if the node has headroom.
4. **"OOM" when there is no limit** is counted against the request (a proxy for
   eviction under memory pressure), which is pessimistic.
5. **Cost** uses only requests (what reserves node capacity). Limits cost
   nothing here; k8s-tailor never reduces the limit in place and that does not
   show up in the cost.
6. **k8s-tailor actuation** uses the real reconciler code (`Decide`; `PlanInPlace`, `Cooled` and `RollingGate` for the disruptive rolling, `PlanRollout` for the rollout path) but assumes
   `InPlace` mode with Burstable pods without HPA/PDB/pending resize; in
   production the default mode is `Recommend` (does not act). The refresh is
   hourly by default (production: 60s) — because of that the default scale-down
   stabilization (15 min) has no effect in the backtest.
7. **VPA**: Recreate/Auto mode; *InPlaceOrRecreate* is not modeled; eviction
   rate limiting (`eviction-tolerance`), PDBs and restart time are ignored (the
   change applies to all replicas on the next tick). The VPA checkpoint and the
   initial 8 days of Prometheus history are not modeled — it starts from zero with
   the same warmup as the others.
8. **KRR** reapplied every day is an optimistic assumption for KRR; KRR's 1.25 min
   step was replaced by the series step.
9. **One recommendation per workload/container** for all replicas, as in the
   three tools; there is no modeling of load redistribution between replicas.

## Status of the proposal to the owner of internal/recommender and internal/reconciler

Done: injectable clock (`Engine.SetClock`), pure entry point
`recommender.Recommend(RecommendInput)`, `reconciler.Decide` (plus `PlanInPlace`,
`Plan.Limits`, `Cooled`, `QuantizeCPU/Mem`) and `ScaleDownStabilization` consumed
by the engine. The backtest no longer uses a time shift or a reconciler port.

Implemented since: TailorPolicy `seasonGate.minCorrelation` / `seasonGate.minPairs`
(seasonality gate, defaults 0.5 / 24) and the per-workload CPU quantile for bursty
load (`cpu.quantile: 0.95` inline, in a TailorProfile, the chart's `bursty` preset
or a container rule; see [CONFIGURATION.md](CONFIGURATION.md)). The stabilization
history is now seeded on the first refresh after a start (replaying `Recommend` at
the past steps of the window): with the backtest's 1h refresh and the 15 min
window it only touches the first refresh (seeds 42 and 1–5: within ±0.1 pod·h of
CPU>request and ±0.001 core of waste, no scenario worse).

Still suggested (not implemented):

- `scaleDown.stabilization` with a larger default for CPU (e.g. 1–6h). Measured in
  the backtest (refresh 1h, seeds 1–5): 3h → `heterogeneous` 4.50 → 4.02
  changes/day (cost 0.664 → 0.691), `bursty` 1.52 → 1.22; 6h → 3.18/day
  (cost 0.726), `memory-leak` risk 0.71% → 0.29%;
- `cpu.quantile` per "bursty" profile (e.g. 0.95) — it is what actually lowers
  risk on bursty load (table above);
- `changePoint.minPersistence` (currently fixed: ~1h rolling median + ¼ of the window).

## Change point: best split (not adopted)

`changePointIndex` returns the **most recent** split whose relative mean shift
passes 0.5, not the split with the largest shift. The best-split variant (max
`|mean(after) − mean(before)|` among qualifying splits, same median filter and
segment bounds) was replayed on `-synthetic all`, seeds 1–5 and 42, against the
current engine. Rows that changed (CPU>request pod·h; waste and cost moved by
≤ 0.005):

| scenario | seed | current | best split |
|---|---:|---:|---:|
| diurnal-weekly | 1 | 9.3 (0.92 changes/day) | 9.7 (0.67) |
| diurnal-weekly | 2 | 4.9 | 5.2 |
| diurnal-weekly | 42 | 3.5 | 3.6 |
| heterogeneous | 3 | 14.3 | 15.9 |
| heterogeneous | 4 | 11.2 | 11.0 |
| heterogeneous | 5 | 14.5 | 13.0 |
| memory-leak | 4 | 3.8 | 3.6 |
| memory-leak | 5 | 4.1 | 4.4 |
| step-change | 1–5, 42 | unchanged | unchanged |

It does not improve `step-change` (the scenario it was meant for: the shift is
large enough that both rules cut at the same place) and regresses
`diurnal-weekly` (seeds 1, 2) and `heterogeneous` (seed 3), so the rule was
kept. With a 50% threshold on a median-filtered series the most recent
qualifying split is already at (or within a filter width of) the real shift;
the cost is only that up to a quarter of the window of the current regime may
be discarded, which makes the quantile slightly more reactive, not less safe.

## Tests

`go test -race ./internal/backtest ./cmd/k8s-tailor -run Backtest` (and
`TestEngineDoesNotFlapOnBursts`, a regression guard: `bursty` ≤ 3 changes/day
and `diurnal-weekly` with risk ≤ 1.5%, seed 1) covers:
determinism of the whole report (including the real engine); absence of
look-ahead for the four strategies (perturbing the future does not change past
decisions); `static` with waste = request − mean usage and zero changes;
manual count of hours/OOM/cost; histogram parity with the upstream tests
(`TestExponentialHistogramOptions`), half-life, minimums, OOM bump and the VPA
updater rules; exact KRR values; the fake VM never serving the future; engine
convergence on constant usage; OOM floor learned via a simulated `OOMKilled` pod;
the adapter over `reconciler.Decide`; CSV/JSONL round trip; format of the
synthetic scenarios.
