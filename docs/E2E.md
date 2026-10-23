# E2E on k3s (k3d)

Run date: 2026-10-22. **Code tested: `f96d636`** (images `k8s-tailor:dev` and `k8s-tailor-mcp:dev` built with
`SRC_REF=HEAD` when HEAD was `f96d636`; it contains the `e32ed45` hardening, the `09b7526..247a215` regressions, `2362fe2`,
`db589bd` and `ffe6da1`). Commit `20d1cc6` landed during the build (documentation only, 9 `.md`/`.txt` files, no
code): `git log -1` at the end = `20d1cc6`; the binary tested is the one from `f96d636`. k3s `v1.34.12-k3s1` (1 node), chart `0.2.0`
with `dev/k3s/values-dev.yaml`. Cluster torn down and recreated clean (`down.sh` + `SRC_REF=HEAD up.sh`, 229 s with cache);
old port-forwards killed. Nothing mocked, except the LLM in scenario 9 (fake OpenAI-compatible server, on purpose) and the
Istio in scenario 2 (imitation proxy). Clocks: `kubectl`/cluster logs in UTC (node time); the host's local time is UTC-3.

After `up.sh` the cluster was updated with **one** `helm upgrade --reuse-values` that turns on everything the scenarios
need: `tailor.replicas=2`, `tailor.webhook.enabled=true` (`certMode=helm`, `actuateNamespaces=e2e-webhook`),
`tailor.advisor.*` (fake LLM), `tailor.admissionPolicy.enabled=true`, **`mcp.auth.enabled=true`,
`mcp.auth.allowUnauthenticated=false`** (values-dev turns the nginx sidecar off; it was turned on for scenario 11a and the UI
in scenario 10 was used with the real login screen).

| # | Scenario | Result |
|---|---|---|
| 1 | `smoke.sh` | PASS |
| 2 | `smoke-sidecar.sh` | PASS |
| 3 | API auth (401/200/415/403/413/400) | PASS |
| 4 | Disruptive restart: 1 pod per cycle | PASS |
| 5 | Knative | SKIPPED (no CRDs; not simulated) |
| 6 | Webhook (annotation, later registration by the leader, excluded ns, engine at 0, forged AdmissionReview) | PASS |
| 7 | Leader election with 2 replicas (leader kill, helm upgrade) | PASS (helm upgrade: **2.75 s** without a leader, before ~40 s) |
| 8 | Backtest `-synthetic all` (6 scenarios at the time; the command now runs 7, see section 8) | PASS |
| 9 | Advisor with fake LLM | PASS |
| 10 | UI against the real cluster | PASS |
| 11a | MCP nginx gate in the cluster | PASS |
| 11b | ValidatingAdmissionPolicy as the engine SA | PASS |
| 11c | OOM without a limit stabilizes (no compounded ×1.5) | PASS |
| 11d | events / `/metrics` | PASS (with observations) |

Scope of this run: one k3s node. Blocking by NetworkPolicy is covered by the Calico suite below and real Istio by the Istio suite below; scenario 5 (Knative) is skipped because the cluster has no Knative CRDs.

## Dedicated suites on their own k3d clusters (2026-10-22)

The 11 scenarios above are the base run at `f96d636`; they were **not** repeated on the current tree (`434e777` at the time of
writing). Two dedicated suites were added afterwards, each with its own cluster (never `tailor-dev`), script and README. Their
images were built from the working tree of the day; the READMEs do not record the exact engine commit, so treat them as
"tree at or just before `69dbc1e` (Istio) and `434e777` (Calico)".

| Suite | What it proves | Result | Details |
|---|---|---|---|
| `dev/calico/netpol-test.sh` | The chart's NetworkPolicies block and allow real traffic (curl probes, a block counts only as a connect timeout), with an enforcement canary and a negative control (`networkPolicy.enabled=false`). Calico v3.33.0, k3s v1.34.12, one node, inside a Lima VM (rootful Docker, Ubuntu 24.04, kernel 6.8). | 8 scenarios, 52 PASS / 0 FAIL in the recorded run, about 5.5 min with warm caches | [dev/calico/README.md](../dev/calico/README.md) |
| `dev/istio/mesh-test.sh` | Real Istio 1.31.3 (profile `minimal`) with classic and native sidecars: classification, opt-in resize without restart, template path with the four `sidecar.istio.io/proxy*` annotations, webhook coexisting with the injector, system namespaces untouched, traffic through Envoy after resizes. | 7 scenarios PASS in each of the two modes; setup and scenarios were run in separate steps, not as one `all` from an empty cluster | [dev/istio/README.md](../dev/istio/README.md) |

Scope notes. The Calico suite does not work on every Docker runtime (the "Dory" runtime lacks the kernel netfilter modules, see its README and [TROUBLESHOOTING.md](TROUBLESHOOTING.md)). Calico does not filter ingress that originates on the node itself, so on one node the suite cannot exercise `webhookFrom` against the API server; it shows that the API server's calls reach the webhook with the policies on. Istio findings from the Istio run (throttled proxy, history of replaced pods, memory trend display) were fixed in the engine (`5af26dc`, `40a6bec`, `68fddfd`) and are covered by unit tests and the `throttled-cap` backtest scenario.

Reproduce (requirements and variables in each README):

```bash
dev/calico/netpol-test.sh        # needs docker, k3d, kubectl, helm, python3; a Docker whose kernel has xt_CT/xt_NFLOG/rpfilter
ISTIO_NATIVE=false dev/istio/mesh-test.sh all   # needs docker, k3d, kubectl, helm, curl, python3; istioctl 1.31.3 is downloaded if absent
```

## Known-issues round (2026-10-23)

What was run for the changes after `7c129c1` (branch commits `e335b35..4c4b9b3`), and what was not. Everything below
ran against code built from the working tree or from a clean clone of the branch; clusters were throw-away
(`tailor-fix` for the proofs, `tailor-dev` for the suite) and are deleted.

| Check | Result |
|---|---|
| `go build`, `go vet`, `gofmt -l` | clean |
| `go test -race -count=1 -timeout 40m ./...` (15 packages), run twice (before and after the refresh fix) | PASS |
| `helm lint --strict`, `make chart-test` (33 render checks, 29 before) | PASS |
| `make lint`; `mcp`: `bun test` (196), `tsc`, build; `mcp/server/e2e/nginx-gate.sh` | PASS |
| Clean clone of the branch: `make k3s-up`, `make k3s-smoke`, `dev/k3s/smoke-sidecar.sh` | PASS (`SMOKE: PASS`, `SMOKE-SIDECAR: PASS`) |
| Markdown: relative links and anchors (28 files), 15 Mermaid blocks rendered with `@mermaid-js/mermaid-cli` | 0 broken, 15 of 15 |
| Backtest `-synthetic all`, seeds 42 and 1-5, before and after | the seven earlier scenarios identical for all four strategies; the new scenarios and opt-ins are in [BACKTEST.md](BACKTEST.md) |

Cluster proofs on k3s v1.34.12 (`tailor-fix`, chart installed from the working tree with the webhook, `tailor.rbac.clusterLookups`
and the warm cache on):

- **`guaranteed: resizeBoth`.** Two Guaranteed pods (500m / 256Mi, requests == limits): the engine resized CPU to 10m
  **together** with the limit (`requests {cpu 10m, memory 256Mi}`, `limits` the same), QoS stayed `Guaranteed`, the pod UIDs
  were unchanged, `restartCount` 0, and the memory shrink was refused with a `ResizeSkipped` Event and
  `tailor_resize_skipped_total{reason="guaranteed_mem_shrink"}`. Upstream behaviour checked directly with `kubectl patch --subresource resize`:
  unequal values are rejected ("Pod QOS Class may not change as a result of resizing"), equal ones are applied in place.
- **`tailor.rbac.clusterLookups`.** `kubectl auth can-i` as the engine ServiceAccount: `list nodes` and `list limitranges` yes,
  `delete nodes` no; `signals.memCeiling` appears on the recommendations (8.3 GB, the node's allocatable) and the engine log has no lookup error.
  The cap itself is covered by unit tests; no scenario in the cluster had an OOM-raised request near the ceiling.
- **Warm cache.** The leader wrote `k8s-tailor-warm` (annotations `warm-entries`, `warm-saved-at`, `warm-version`). With the metrics backend **down**
  and the engine restarted, the new replica logged `warm cache: loaded 1 recommendation(s)`; pods created by scaling the Deployment right
  after were sized by the webhook (`500m / 256Mi` to `10m / 32Mi`, with the `admission-sized` annotation) and `tailor_webhook_skipped_total{reason="warming_up"}` did not
  appear; the first refresh failed (`all 3 namespace queries failed; keeping the previous recommendations`) and the loaded entries stayed. The same outage with
  `WARM_CACHE=false` left the new pods at `500m / 256Mi` and `warming_up` = 2. RBAC: `update` is allowed on `configmaps/k8s-tailor-warm`, not on another ConfigMap, and `create configmaps` is not allowed in other namespaces.
  A first run of this scenario exposed a real bug (a refresh in which every namespace failed replaced the state with nothing); it is fixed and has tests.

**Not verified in a cluster:** `disruptive.maxWait` (unit tests with fakes only); the ksvc desired count (no Knative CRDs in the cluster);
the grouped-pod history cache; the CPU-greedy rule and `autoQuantile` (backtest and unit tests; real greedy detection needs hours of samples);
weekly seasonality on a 14-day window (unit and fake-VM tests); the `values-vmsingle-chown.yaml` overlay (rendered and checked, not run on a provisioner
that ignores `fsGroup` with root-owned data); the Calico and Istio suites (not re-run: nothing under `dev/calico` or `dev/istio` changed, and the Docker runtime
used here cannot run Calico).

## 1. smoke.sh — PASS

```
== 5. In-place resize without restart (mode=InPlace)
  before: pod=smoke-burner-6ddd7dc87f-6ljzf uid=4c768a74-e5f8-4ba3-928d-f46f61e0c408 restarts=0 requests={"cpu":"500m","memory":"64Mi"}
  after:  pod=smoke-burner-6ddd7dc87f-6ljzf uid=4c768a74-e5f8-4ba3-928d-f46f61e0c408 restarts=0 requests={"cpu":"500m","memory":"32Mi"}
  PASS requests changed in-place / pod UID unchanged / restartCount unchanged (0)
SMOKE: PASS
```
(Checks 1–4 also PASS: pods Ready, 4 CRDs + TailorConfig, `/healthz`, series in VM.) Only memory changed in the smoke
(CPU becomes "confident" one scrape point later; see scenarios 4 and 6, where CPU also changed).

## 2. smoke-sidecar.sh — PASS

Run in parallel with `smoke.sh` (distinct labels since the fix from the previous round):

```
  PASS istio-proxy classified: injected-sidecar istio Recommend none
  PASS app resized in-place: {"cpu":"500m","memory":"64Mi"} -> {"cpu":"500m","memory":"32Mi"}
  PASS istio-proxy untouched in Recommend ({"cpu":"300m","memory":"64Mi"})
  PASS istio-proxy resized in-place after opt-in: {"cpu":"300m","memory":"64Mi"} -> {"cpu":"300m","memory":"32Mi"}
  PASS pod not recreated / PASS istio-proxy without restart
SMOKE-SIDECAR: PASS
```

## 3. API auth — PASS

Token from Secret `k8s-tailor-api-auth` (32 chars), `port-forward svc/k8s-tailor 8080`:

```
no token             POST /api/pause  -> 401 {"error":"unauthorized"}
with Bearer          POST /api/pause  -> 200 {"paused":false}
Content-Type text/plain + Bearer      -> 415 {"error":"Content-Type must be application/json"}
Origin: http://evil.example + Bearer  -> 403 {"error":"cross-site request refused"}
wrong Bearer                          -> 401 {"error":"unauthorized"}
body > 1 MiB                          -> 413 {"error":"body larger than 1048576 bytes"}
policy with invalid mode              -> 400 {"error":"invalid policy: mode \"Bogus\" not in Off|Recommend|Webhook|InPlace|Both"}
GET /api/overview, /api/policies, /api/recommendations (no token) -> 200
```

## 4. Disruptive restart — PASS

Deployment `e2e-restart/memrestart`, 3 replicas, `resizePolicy: memory=RestartContainer`, policy `InPlace`, memory
512Mi (usage ~1Mi). Samples every 20 s (`rc` = restartCount, `mem`/`cpu` = request from the spec; UTC time):

```
18:15:49 6tdjp rc=0 mem=512Mi cpu=100m | bdrbf rc=0 mem=512Mi cpu=100m | gpcvm rc=0 mem=512Mi cpu=100m
18:16:09 6tdjp rc=0 mem=32Mi  cpu=201m | bdrbf rc=0 mem=512Mi cpu=100m | gpcvm rc=0 mem=512Mi cpu=100m
18:16:49 6tdjp rc=1 mem=32Mi  cpu=201m | bdrbf rc=0 mem=32Mi  cpu=201m | gpcvm rc=0 mem=512Mi cpu=100m
18:17:09 6tdjp rc=1 mem=32Mi  cpu=201m | bdrbf rc=1 mem=32Mi  cpu=201m | gpcvm rc=0 mem=32Mi  cpu=201m
18:17:49 6tdjp rc=1 mem=32Mi  cpu=201m | bdrbf rc=1 mem=32Mi  cpu=201m | gpcvm rc=1 mem=32Mi  cpu=201m   (stable)
```

Engine log: one pod per 30 s cycle, the others wait:

```
18:16:08 in-place resize e2e-restart/memrestart[app] -> cpu=201m mem=32Mi (was cpu=100m mem=512Mi)
18:16:37 skip in-place resize e2e-restart/memrestart-5b5bc6d68d-6tdjp[app]: previous resize InProgress
18:16:37 in-place resize ... (next pod)
18:17:07 skip in-place resize e2e-restart/memrestart-5b5bc6d68d-bdrbf[app]: previous resize InProgress
18:17:07 in-place resize ... (third pod)
```

Each pod restarted exactly once; the specs changed at 18:16:08, 18:16:37 and 18:17:07 (1 per cycle).

## 5. Knative — SKIPPED

`kubectl get crd | grep -c serving.knative` = 0. Installing Knative Serving + net-kourier on a 1-node k3d without a registry
is outside the budget; it was not simulated. The `ksvc` path only has unit-test coverage.

## 6. Webhook — PASS

Two namespaces with a `Webhook` policy and the same `web` workload (400m/256Mi): `e2e-webhook` (in `actuateNamespaces`) and
`e2e-excluded` (outside it). Both scaled 1 -> 2 at 18:26:45:

```
== e2e-webhook
web-85bdb88776-d52cn  cpu=400m mem=256Mi  created=18:19:22Z  admission-sized=
web-85bdb88776-nrlr5  cpu=137m mem=32Mi   created=18:26:45Z  admission-sized={"app":{"fromCpu":0.4,"toCpu":0.137,"fromMem":268435456,"toMem":33554432}}
== e2e-excluded (NOT mutated)
web-85bdb88776-hk9rw  cpu=400m mem=256Mi  created=18:19:22Z
web-85bdb88776-c2chh  cpu=400m mem=256Mi  created=18:26:45Z  admission-sized=
```

**Decision recorded later by the leader's reconciler** (the webhook writes nothing): the mutation was logged by the
**standby** replica (`k8s-tailor-79dc85555-l9s4s: webhook: mutating e2e-webhook/web container app -> cpu=137m mem=32Mi`), and
~45 s later the counter `tailor_mutations_total{namespace="e2e-webhook",workload="web",container="app"} 1` existed
**only on the leader** (`...7255x`; the standby does not have the series) and `GET /api/actuations` returned
`{"mode":"webhook","namespace":"e2e-webhook","workload":"web","fromCpu":0.4,"toCpu":0.137,"fromMem":268435456,"toMem":33554432}`.

**Engine at 0 replicas** (`scale deploy k8s-tailor --replicas=0`, 18:34:30): standalone pod `solo` created and Running
(`50m/64Mi`, unchanged) and `web` 2 -> 4 created `6zssr`/`d6brf` with `400m/256Mi` with no admission error (`failurePolicy: Ignore`).
Engine turned back on at 2 replicas.

**Forged AdmissionReview** (`curl -k` against `https://localhost:18443/mutate`, port-forward to the pod; local 8443 belongs to Dory):

```
kind=Deployment operation=CREATE  -> allowed=True  NO PATCH
kind=Pod        operation=UPDATE  -> allowed=True  NO PATCH
kind=Pod        operation=CREATE  (control, well-formed Pod) -> allowed=True JSONPatch [... "tailor.k8s-tailor.dev/admission-sized" ...]
before/after (70 s): /api/actuations events=10 -> 10, counts unchanged; tailor_mutations_total on the leader = 1 -> 1
```
The two invalid forged reviews produce no patch and **none** of the three (not even the control Pod, which does not exist in the apiserver)
produces a record.

## 7. Leader election — PASS

`tailor.replicas=2`; Lease `k8s-tailor-actuator` (`leaseDuration` 15 s, `renewDeadline` 10 s, `retry` 2 s). A
`kubectl get lease -w` with local timestamps recorded every write (UTC time; empty `holder` = lease released):

```
(a) kubectl delete pod <leader>   [SIGTERM, 18:35:19]
18:35:19.753  holder=""            renew=18:35:19.267  trans=2     <- shutdown: lease released
18:35:20.223  holder=...-dptq4     renew=18:35:19.735  trans=3     <- gap 0.47 s

(b) kubectl delete pod <leader> --force --grace-period=0   [18:36:08]
18:36:08.341  holder=""            renew=18:36:07.856  trans=3
18:36:08.797  holder=...-fjhbx     renew=18:36:08.313  trans=4     <- gap 0.46 s

(c) kill -9 of the leader process on the node [18:37:06]: the container restarts in <1 s and the SAME pod retakes its own
    lease (renew 18:37:06.607 / .616, trans=4 unchanged). There was no leader change.

(d) frozen leader (kill -STOP 18:37:51 -> -CONT 18:38:13; lease NOT released, hang case)
18:37:50.721  holder=...-fjhbx  (last renew)
18:38:11.382  holder=...-25msl  trans=5           <- standby takeover, 20.7 s after the last renew
18:38:23.310  [fjhbx, unfrozen] "failed to renew lease ... context deadline exceeded" ->
              "lost/released lease — actuation stopped"  (stops acting ~10 s after CONT)
```

**During `helm upgrade` (rolling update, 2 replicas)** — target "a few seconds" (before ~40 s):

```
18:38:44.847 helm upgrade START  (--set-string tailor.podAnnotations.e2e-roll=1)
18:39:09.903 old leader (25msl): "shutdown: stopping actuation and releasing the lease"
18:39:09.905                     "lost/released lease — actuation stopped" / "shutdown complete"
18:39:09.902 lease released       holder="" trans=5
18:39:12.653 new leader (dlqt7)   holder=...-dlqt7 trans=6   -> 2.75 s without a leader
18:39:10.773 helm upgrade END
```
The 2.75 s gap is the new standby waiting for the next `RetryPeriod` (2 s) after the release. Post-upgrade:
`dlqt7: tailor_leader 1`, `kg9pj: tailor_leader 0`.

Caveat (not a chart bug, it is the client-go model): with a **hung** leader that does not release the lease (case d) the takeover takes
~15–21 s and the old leader still acts for up to ~`RenewDeadline` after waking up. Actuations are idempotent and
resizes have gates/cooldown, but a short window with two "leaders" exists.

## 8. Backtest — PASS (output identical to the previous round; deterministic, seed 42)

`go run ./cmd/k8s-tailor backtest -synthetic all` (seed 42, warmup 48 h, 12 days evaluated). `k8s-tailor` rows
(other strategies are in the full report; reproducible with the same command):

| scenario | CPU>request % | OOMs | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---|
| diurnal-weekly | 0.41 | 0 | 0.92 | 0.593 | 423m / 562Mi |
| bursty | 7.20 | 0 | 2.08 | 0.577 | 176m / 574Mi |
| step-change | 6.02 | 0 | 0.50 | 1.081 | 758m / 788Mi |
| memory-leak | 0.41 | 0 | 0.33 | 0.619 | 240m / 937Mi |
| memory-spikes | 0.94 | 0 | 0.42 | 0.651 | 290m / 640Mi |
| heterogeneous | 1.26 | 0 | 4.42 | 0.679 | 515m / 490Mi |
| throttled-cap (added later) | 4.99 | 0 | 0.75 | 11.339 | 321m / 53Mi (lim 383m) |

The first six rows are from the base run and were reproduced identically on `434e777`; `throttled-cap` (usage pinned at a 50m CPU limit)
was added afterwards and re-run on `434e777`: `static` stays throttled for 576 of 576 pod-hours, `k8s-tailor` for 5.7 as it grows the limit
50 -> 383m by steps. Its relative cost is above 1 because the baseline request (20m) is far below the demand; it is the cost of not being throttled
([BACKTEST.md](BACKTEST.md)). The `memory-spikes` and `throttled-cap` scenarios were not part of the cluster run. Notes on the numbers (KRR, VPA and `static` are reference baselines for the methodology): `k8s-tailor` had 0 OOMs in the 6 base scenarios (KRR had 3 and 26 OOMs in
step-change/memory-leak, `static` had 12 in memory-spikes). In `bursty` the VPA baseline has equivalent CPU waste
(0.143 vs 0.141 cores) and slightly lower CPU>request (7.12% vs 7.20%); in `heterogeneous` the VPA baseline has a lower cost (0.583 vs 0.679)
and `k8s-tailor` makes more changes/day (4.42 vs 1.92). In `step-change` the relative cost is > 1 (1.081): the extra is
paid in request to avoid throttling after the step (VPA 1.139, `static` 1.000 with 37% of the time above the request).

### Full tables

(Captured in the base run with the report format of the time: 6 scenarios and no `CPU>limit` column. The current report has 7 scenarios and that column; regenerate it with the command above.)

## backtest/diurnal-weekly/app

3 replicas; CPU and memory with a daily cycle (peak 16h UTC) and weekends at 55%; AR(1) noise. Initial request oversized (1 core / 1Gi).

3 pods · step 300s · evaluated window 12.0 days · initial 1000m / 1024Mi (lim 1024Mi)

| strategy | CPU>request (pod·h) | CPU>request (%) | OOMs | mem>ceiling (pod·h) | CPU waste (cores) | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 0.0 | 0.00 | 0 | 0.0 | 0.741 | 0.579 | 0.00 | 1.000 | 1000m / 1024Mi (lim 1024Mi) |
| krr | 31.7 | 3.67 | 0 | 0.0 | 0.263 | 0.173 | 1.00 | 0.529 | 517m / 609Mi (lim 609Mi) |
| vpa | 20.7 | 2.39 | 0 | 0.0 | 0.301 | 0.187 | 1.00 | 0.565 | 476m / 600Mi (lim 600Mi) |
| k8s-tailor | 3.5 | 0.41 | 0 | 0.0 | 0.337 | 0.147 | 0.92 | 0.593 | 423m / 562Mi (lim 1024Mi) |

## backtest/bursty/app

3 replicas; low CPU base with ~6 bursts/day (5–30 min, 3–8× the base) correlated across replicas; memory rises +80Mi during the burst.

3 pods · step 300s · evaluated window 12.0 days · initial 500m / 768Mi (lim 1024Mi)

| strategy | CPU>request (pod·h) | CPU>request (%) | OOMs | mem>ceiling (pod·h) | CPU waste (cores) | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 58.3 | 6.75 | 0 | 0.0 | 0.353 | 0.334 | 0.00 | 1.000 | 500m / 768Mi (lim 1024Mi) |
| krr | 29.5 | 3.41 | 0 | 0.0 | 0.655 | 0.176 | 1.00 | 1.499 | 825m / 607Mi (lim 607Mi) |
| vpa | 61.5 | 7.12 | 0 | 0.0 | 0.143 | 0.185 | 1.08 | 0.589 | 163m / 641Mi (lim 854Mi) |
| k8s-tailor | 62.3 | 7.20 | 0 | 0.0 | 0.141 | 0.147 | 2.08 | 0.577 | 176m / 574Mi (lim 1024Mi) |

## backtest/step-change/app

3 replicas; a deploy halfway through the period changes the level: CPU 0.20→0.55 core, memory 300Mi→650Mi.

3 pods · step 300s · evaluated window 12.0 days · initial 500m / 512Mi (lim 1024Mi)

| strategy | CPU>request (pod·h) | CPU>request (%) | OOMs | mem>ceiling (pod·h) | CPU waste (cores) | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 321.5 | 37.21 | 0 | 0.0 | 0.136 | 0.087 | 0.00 | 1.000 | 500m / 512Mi (lim 1024Mi) |
| krr | 168.3 | 19.48 | 3 | 72.0 | 0.079 | 0.118 | 1.00 | 0.937 | 671m / 826Mi (lim 826Mi) |
| vpa | 50.3 | 5.83 | 0 | 0.0 | 0.170 | 0.137 | 0.42 | 1.139 | 813m / 826Mi (lim 1652Mi) |
| k8s-tailor | 52.0 | 6.02 | 0 | 0.0 | 0.136 | 0.101 | 0.50 | 1.081 | 758m / 788Mi (lim 1024Mi) |

## backtest/memory-leak/app

2 replicas; memory leak of 110Mi/day starting at 280Mi, reset by a deploy every 5 days; limit 1Gi.

2 pods · step 300s · evaluated window 12.0 days · initial 500m / 512Mi (lim 1024Mi)

| strategy | CPU>request (pod·h) | CPU>request (%) | OOMs | mem>ceiling (pod·h) | CPU waste (cores) | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 0.0 | 0.00 | 0 | 0.0 | 0.350 | 0.040 | 0.00 | 1.000 | 500m / 512Mi (lim 1024Mi) |
| krr | 27.7 | 4.82 | 26 | 15.6 | 0.065 | 0.340 | 0.92 | 0.584 | 216m / 981Mi (lim 981Mi) |
| vpa | 0.8 | 0.14 | 0 | 0.0 | 0.097 | 0.289 | 1.58 | 0.629 | 247m / 878Mi (lim 1756Mi) |
| k8s-tailor | 2.3 | 0.41 | 0 | 0.0 | 0.089 | 0.307 | 0.33 | 0.619 | 240m / 937Mi (lim 1024Mi) |

## backtest/memory-spikes/app

3 replicas; stable memory ~300Mi with short, rare spikes (~1 every 2 days, 10–20 min, 680–820Mi) above the initial limit of 640Mi: real OOMKill. Measures whether the learned OOM floor raises the limit without inflating the request.

3 pods · step 300s · evaluated window 12.0 days · initial 500m / 512Mi (lim 640Mi)

| strategy | CPU>request (pod·h) | CPU>request (%) | OOMs | mem>ceiling (pod·h) | CPU waste (cores) | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 0.0 | 0.00 | 12 | 3.8 | 0.325 | 0.206 | 0.00 | 1.000 | 500m / 512Mi (lim 640Mi) |
| krr | 38.2 | 4.43 | 0 | 0.0 | 0.092 | 0.563 | 0.75 | 0.667 | 264m / 916Mi (lim 916Mi) |
| vpa | 4.8 | 0.56 | 3 | 0.2 | 0.121 | 0.405 | 0.33 | 0.684 | 296m / 933Mi (lim 1166Mi) |
| k8s-tailor | 8.1 | 0.94 | 0 | 0.0 | 0.118 | 0.288 | 0.42 | 0.651 | 290m / 640Mi (lim 960Mi) |

## backtest/heterogeneous/app

4 heterogeneous replicas (hot shard 2.2×, cold 0.4×) over the same daily cycle; the recommendation is single per workload.

4 pods · step 300s · evaluated window 12.0 days · initial 1000m / 1024Mi (lim 1024Mi)

| strategy | CPU>request (pod·h) | CPU>request (%) | OOMs | mem>ceiling (pod·h) | CPU waste (cores) | mem waste (GiB) | changes/day | rel. cost | final |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| static | 0.1 | 0.01 | 0 | 0.0 | 0.789 | 0.727 | 0.00 | 1.000 | 1000m / 1024Mi (lim 1024Mi) |
| krr | 13.1 | 1.14 | 0 | 0.0 | 0.616 | 0.272 | 1.00 | 0.794 | 823m / 563Mi (lim 563Mi) |
| vpa | 81.2 | 7.05 | 0 | 0.0 | 0.389 | 0.267 | 1.92 | 0.583 | 511m / 523Mi (lim 523Mi) |
| k8s-tailor | 14.5 | 1.26 | 0 | 0.0 | 0.492 | 0.230 | 4.42 | 0.679 | 515m / 490Mi (lim 1024Mi) |



## 9. Advisor — PASS (fake LLM, no external calls)

`kubectl apply -f dev/k3s/fake-llm.yaml` (busybox httpd + CGI in the cluster, ns `e2e-advisor`) and
`tailor.advisor.baseURL=http://fake-llm.e2e-advisor.svc/cgi-bin`, `model=fake-model`:

```
POST /api/advisor/review  (no token)   -> 401 {"error":"unauthorized"}
POST /api/advisor/review  (Bearer)     -> 200 {"container":"app","kind":"Deployment","namespace":"e2e-restart","review":{"verdict":"ok","confidence":"medium","rationale":"FAKE e2e response: ...","risks":[...],"suggested_action":"Keep the current recommendation.","cited_figures":[],"model":"fake-model"},"workload":"memrestart"}
POST /api/advisor/summary (no token)   -> 401
POST /api/advisor/summary (Bearer)     -> 200 {"considered":6,"summary":{"overview":"FAKE e2e response: ...","verdict":"ok","confidence":"medium",...},"total":6}
GET  /api/advisor/status               -> enabled=true, llm_calls=2, failures=0, breaker_state=closed
```

## 10. UI — PASS

`port-forward svc/k8s-tailor-mcp 8020:8020` with the **nginx gate on**: the page shows "Sign in to K8s Tailor"
(`00-signin.jpg`); with the token from Secret `k8s-tailor-mcp-auth` it enters the dashboard. Real screenshots in
`.context/shots/e2e2/` (gitignored): `01-overview` (6 workloads, "Actuating", real CPU/memory charts),
`02-workloads` (row `e2e-webhook/web 400m -> 154m, 256Mi -> 32Mi, Applied 15m ago, Acts via webhook (at admission)`),
`03-decisions` (real decisions: `oomtest 219Mi -> 329Mi +50%`, `webhook 400m -> 137m`, the 3 `memrestart` resizes),
`04-health` (VictoriaMetrics healthy, decision log "Durable"), `05-policies` (4 e2e policies),
`06-ai-summary` (fake LLM summary rendered). Console (`read_console_messages`, all levels): 6 messages,
**all `DEBUG`** (the MCP app SDK ignoring non-JSON-RPC `postMessage`); no `error`/`warn`.

## 11a. MCP nginx gate in the cluster — PASS

MCP with nginx sidecar (`mcp.auth.enabled=true`), Service `k8s-tailor-mcp:8020` -> nginx 8080. `curl --path-as-is`, POST
JSON-RPC `tools/list`:

```
NO token
/mcp                 401      /..\mcp        401      /x\..\mcp   401      //mcp       401
/%2e%2e/mcp          400      /mcp/../mcp    401      /MCP        401      /mcp%2f     401
Wrong Bearer on /mcp -> 401
WITH token
/mcp                 200  {"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"TAILOR_OVERVIEW",...
/..\mcp 400 "bad request: non-canonical path"   /x\..\mcp 400 (same)   //mcp 400 (same)   /%2e%2e/mcp 400 (nginx)
allowlist without token: GET /healthz 200, /readyz 200, / 200, /index.html 200; GET /mcp 401; POST / 403
```
No path variant without a token got past 4xx.

## 11b. ValidatingAdmissionPolicy — PASS

3 VAPs + bindings created (`k8s-tailor-patch-scope-{pods,replicas,workloads}`). Impersonating
`system:serviceaccount:k8s-tailor:k8s-tailor` via `kubectl proxy` + `Impersonate-User` header (`kubectl --as ... patch`
does not work: it does a GET first and the SA has no `get` on `pods/resize`):

```
deploy: change image           422 ... denied request: k8s-tailor may only change containers' resources (not image, command ...
deploy: scale replicas         422 ... k8s-tailor may not change the workload spec outside the pod template
deploy: pod-template label     422 ... k8s-tailor may not change pod-template labels or metadata
deploy: container command      422 ... k8s-tailor may only change containers' resources (not image, command ...
deploy: change resources       200 Deployment/web
pod/resize: resources          200 Pod/oomtest-...
pod: change image              422 ... k8s-tailor may not change a pod's spec (in-place resizes use the pods/resize ...
pod: annotation tailor.k8s-tailor.dev/*  200
```

## 11c. OOM without a limit stabilizes — PASS

`polinux/stress` does not run on k3d's arm64 node (`FEXServer ... Expect errors!`, CrashLoop from x86 emulation: it is the same
reason the demo's `mem-underprovisioned` ends up in CrashLoop, `demo/workloads.yaml:77`), so I used my own pod:
`e2e-oom/oomtest` (`python:3.12-alpine` holding 80 MiB, **no memory limit**, request 64Mi, policy `InPlace`).
A real OOMKill without a limit was provoked by lowering the pod cgroup's `memory.max` on the node (simulates node
memory pressure; the spec never had a limit: `lim=` empty throughout the series). Series (`rc` = restartCount, memory request from the spec):

```
18:11:48 rc=0 mem=64Mi
18:17:11 rc=0 mem=97Mi     <- 1st resize (usage 84 MiB, MIN_SAMPLES=2)
18:18:16 OOMKilled (exitCode 137, spec without limit)   [my cap let the 1st restart suffer a 2nd kill at 18:18:40]
18:18:41 rc=1 mem=146Mi    <- 97 x 1.5
18:19:42 rc=2 mem=219Mi    <- 146 x 1.5 (2nd kill); then STABLE from 18:19:42 to 18:40:30 (21 min, ~125 refreshes of 10 s),
                              across 4 engine restarts (helm upgrade, scale 0, delete, kill -STOP)
18:40:38 clean OOMKilled (a single kill, rc 2 -> 3)
18:41:21 rc=3 mem=329Mi    <- 219 x 1.5 (ONE step per kill)
18:42:02 ... 18:46:03: mem=329Mi in all 12 samples at 30 s (≥ 30 refreshes of 10 s with no new OOM)  -> fixed point
```
Each kill produces exactly one ×1.5 step; with no new kill the request does not grow (before `09b7526`: 256Mi -> 17Gi in 6
refreshes). Ceiling: peak × 4 (~350Mi) was not reached.

## 11d. events and /metrics — PASS (with observations)

`tailor_leader` 1/0 on the two replicas; `tailor_recommendations{status=oom_risk|overprovisioned|underprovisioned}`,
`tailor_workloads{mode=InPlace|Recommend|Webhook}`, `tailor_resize_skipped_total{reason="cooldown"} 12`,
`tailor_mutations_total{...}` (only on the leader that observed the pod). Counters are in-memory per process: after each leader
change the new leader's `tailor_resizes_total` series starts empty (the durable history is in ClickHouse,
`/api/actuations` shows `durable:true`). Warnings from `kubectl get events -A`: `ErrImagePull`/`ImagePullBackOff` due to a Docker
DNS failure on the host (`lookup registry-1.docker.io: Try again`) and the engine probe hanging only during my `kill -STOP`
(scenario 7d). No warning generated by k8s-tailor itself; `tailor-demo` was left at 0 replicas (scaled down by `smoke.sh`).

## Changes in dev/k3s

Documentation only: `dev/k3s/README.md` gained the full e2e section (`helm upgrade` flags, MCP auth, gotchas).
No code outside `dev/k3s/` and `docs/E2E.md` was edited.

## Findings (reported only)

1. `demo/workloads.yaml:77`: `polinux/stress` is amd64; on an arm64 node (k3d on Apple Silicon) `mem-underprovisioned` never
   comes up and is not usable for the OOM scenario. Replace it with a multi-arch image.
2. A hung leader (SIGSTOP) takes 20.7 s to be replaced (lease 15 s + retry) and the old one acts up to ~10 s after waking
   (`internal/leader/leader.go:34-41`, `RenewDeadline` 10 s). Acceptable with gates/cooldown; only document it.
3. The `tailor_resizes_total`/`tailor_mutations_total` counters are per process and disappear on leader change (alerts should
   use `sum`/`increase` across the replicas, not a single series).
4. Local port 8443 is occupied by `doryd` on this host (it answers "Dory: no backend"); use another port to talk to the
   webhook via port-forward.
5. No new functional bugs in the commits tested.

## Access (cluster left running)

- Dashboard (MCP, **gated**): http://localhost:8020 (login screen; token:
  `kubectl --context k3d-tailor-dev -n k8s-tailor get secret k8s-tailor-mcp-auth -o jsonpath='{.data.token}' | base64 -d`)
- Engine: http://localhost:8080 (reads open; writes with the token from
  `kubectl --context k3d-tailor-dev -n k8s-tailor get secret k8s-tailor-api-auth -o jsonpath='{.data.token}' | base64 -d`)
