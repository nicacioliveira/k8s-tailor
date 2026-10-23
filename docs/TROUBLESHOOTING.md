# Troubleshooting

Start here when something does not behave. Each entry: symptom, how to confirm, what it usually means. Everything below is derived from the code and chart
(`internal/reconciler`, `internal/recommender`, `chart/templates`) and the suites in [E2E.md](E2E.md), [dev/calico](../dev/calico/README.md) and [dev/istio](../dev/istio/README.md). Limitations and design decisions live in [KNOWN-ISSUES.md](KNOWN-ISSUES.md).

Handy first commands:

```bash
kubectl -n k8s-tailor get pods
kubectl -n k8s-tailor logs deploy/k8s-tailor --tail=200
kubectl get tailorconfig cluster -o yaml
kubectl get rsp                                                     # TailorPolicies
kubectl get events -A --field-selector reason=Rightsized            # applied changes
kubectl get events -A --field-selector reason=ResizeSkipped         # held-back changes (deduplicated, 1/h per object and reason)
kubectl port-forward -n k8s-tailor svc/k8s-tailor 8080:8080 &       # then: curl -s localhost:8080/metrics | grep '^tailor_'
```

## Install and chart

| Symptom | Cause and fix |
|---|---|
| `helm lint` / `template` / `install`: missing dependency `victoria-metrics-single` | `chart/charts/` is git-ignored. Run `helm dependency build chart` once on a clean clone. |
| Render fails: "needs a live API server to REUSE its CA" | `tailor.webhook.certMode=helm` under `helm template`/ArgoCD/Flux. Use `certMode: certManager` or `existingSecret` (`allowEphemeralCA: true` is for CI only). |
| Render fails: "no Kubernetes API available to reuse the generated secret" | `secretGeneration.failWithoutLookup=true` and a Secret has no `existingSecret`. Provide `tailor.apiAuth.existingSecret`, `mcp.auth.existingSecret`, `clickhouse.auth.existingSecret`. |
| Render fails: "values.namespace=... no longer moves resources" | Remove `namespace:` from your values; install with `-n <ns>`. |
| Render fails: `tailor.replicas > 1 requires tailor.leaderElection.enabled=true` | Re-enable leader election. |
| `helm upgrade` fails: "CustomResourceDefinition ... invalid ownership metadata" | The CRDs exist but were not created by this release (applied by hand or by another release). Label and annotate them for the release (`app.kubernetes.io/managed-by=Helm`, `meta.helm.sh/release-name`, `meta.helm.sh/release-namespace`), or set `crds.install=false` and manage them yourself ([UPGRADE.md](UPGRADE.md#crds)). |
| `ImagePullBackOff` / `ErrImagePull` | The default `ghcr.io/nicacioliveira/k8s-tailor{,-mcp}:v0.1.0` images **do not exist yet**: the repository is private and no `v0.1.0` tag was pushed, so nothing was published. Either the owner publishes them (and makes the packages public or you set `global.imagePullSecrets`), or you build locally and `k3d image import` / `kind load` them. Both paths: [UPGRADE.md#images](UPGRADE.md#images). |
| Render fails: "mcp.auth.token may only contain A-Z a-z 0-9 . _ ~ + / = -" | The token is embedded in the nginx config, so its alphabet is restricted. Use `openssl rand -hex 32`. |
| MCP pod `CrashLoopBackOff`, log "refusing to start without MCP_API_TOKEN" (or the sidecar refuses an `existingSecret` token) | The Bun server never starts without a token, not even on loopback. With `mcp.auth.enabled=false` set `mcp.auth.existingSecret`/`token`, or (dev only) `mcp.auth.allowUnauthenticated=true`. With an `existingSecret`, check its key and that the value only uses `A-Z a-z 0-9 . _ ~ + / = -`. |
| vmsingle cannot write (permission denied) after an upgrade | A PVC written as root on a provisioner that ignores `fsGroup`. See the note in [UPGRADE.md](UPGRADE.md): the overlay `chart/values-vmsingle-chown.yaml` adds an opt-in `chown` init container, or set `victoria-metrics-single.server.podSecurityContext`. |
| ClickHouse engine log: "clickhouse schema not ready" repeating | ClickHouse still starting, wrong `CH_URL`, or wrong password (a GitOps render that minted a new password while ClickHouse kept the first one). The engine retries every 5 s and buffers decisions in memory until it attaches. |

## Nothing is being resized

Work down this list; each item is a gate in the order the code evaluates them.

1. **Is actuation on at all?** `curl localhost:8080/healthz` (port-forward) shows `"actuation":"dry-run"` when no workload resolves to a mode that actuates, `"paused"` when the kill switch is on.
   `kubectl get tailorconfig cluster -o jsonpath='{.spec.paused}'` must be `false`. `tailor_workloads{mode=...}` in `/metrics` counts workloads by effective mode.
2. **Mode.** The chart default is `Recommend`. Create a `TailorPolicy` with `mode: InPlace` (or `Both`) whose `selector` matches the namespace (globs allowed) and labels, or annotate the workload
   `tailor.k8s-tailor.dev/mode=InPlace`. The highest `priority` matching policy wins; an annotation beats it; `tailor.k8s-tailor.dev/ignore: "true"` beats everything. A policy with `schedule.windows` is
   `outsideMode` (default `Off`) outside its windows.
3. **Namespace excluded?** The release namespace and `tailor.excludeNamespaces` (default list includes `kube-system`, `istio-system`, `cert-manager`, `argocd`, `monitoring`, ...) are never touched.
   `kubectl get tailorconfig cluster -o jsonpath='{.spec.excludeNamespaces}'`. Remember the CR is seeded once; later chart changes do not update it ([UPGRADE.md](UPGRADE.md)).
4. **Still learning?** Each resource needs `MIN_SAMPLES` distinct 300 s scrape points. The chart default 288 means 24 h; `tailor_recommendations{status="learning"}` counts them. Lower `MIN_SAMPLES` only for demos
   (`dev/k3s` uses 2, about 10 minutes). Confidence is per resource: a container with memory data but no CPU series never has its CPU moved.
5. **Inside tolerance?** A resize needs a drift of at least 15% **and** 20m CPU / 16Mi memory against the live pod request. Direction `Down` ignores recommended increases, `Up` ignores decreases.
6. **Cooldown.** Scale-down 1 h and scale-up 30 s per workload and container by default (policy `scaleDown.cooldown`, `scaleUp.cooldown`); persisted on the pod annotation `tailor.k8s-tailor.dev/last-applied`.
   `tailor_resize_skipped_total{reason="cooldown"}` rises. A **scale-down stabilization** window (default 15 m) additionally holds the published request at the recent maximum.
7. **HPA.** If an HPA has a `Resource` metric on CPU or memory, that resource is held on purpose (the UI shows "HPA cpu"); the other one is still sized.
8. **QoS.** Guaranteed pods (requests equal to limits for every container) are skipped in place (`reason="guaranteed_qos"`) unless the policy sets `guaranteed: resizeBoth` (memory still never shrinks: `guaranteed_mem_shrink`); `apply: InPlacePreferred` rolls the template instead. BestEffort pods are skipped too
   (`besteffort_qos`). A resize that would turn a Burstable pod into Guaranteed is skipped (`qos_change`).
9. **Outstanding resize.** `tailor_pods_resize_state` and `kubectl get pod <p> -o jsonpath='{.status.conditions}'` show `PodResizePending` (`Deferred` or `Infeasible`) or `PodResizeInProgress`. While one exists the engine does not stack another patch
   (`reason="resize_pending"`); `Infeasible` also produces a `ResizeInfeasible` Warning Event: the node cannot fit the request.
10. **PDB and disruptive resizes.** If the container's `resizePolicy` is `RestartContainer` for the resource that moves and a PDB allows no disruption, that part is held (`pdb_blocked`); with `InPlacePreferred` it
    falls back to a rollout. Disruptive resizes advance one pod per cycle and wait for the others to be Ready (`disruptive_wait_ready`, `disruptive_budget`). A replica that stays NotReady holds the rolling for `tailor.disruptive.maxWait` (default 15m); after that it proceeds only if the live PDB and replica counts allow it (`tailor_disruptive_wait_expired_total`). The PDB budget and replica counts are read live before each restart; if that read fails the pod is skipped (`disruptive_live_unknown`) and retried on the next pass.
11. **Rejected resizes.** `kubectl get events -A --field-selector reason=ResizeFailed` shows the API server's reason; the pod/container is then left alone for 5 minutes (`failure_backoff`).
12. **No `pods/resize` on the cluster.** `kubectl get --raw /api/v1 | grep pods/resize`. If absent, managed workloads fall back to rollouts; if discovery itself fails, the engine holds (`reason="inplace_support_unknown"`, Warning Event) and retries in 30 s.
13. **Not the leader.** With `tailor.replicas > 1` only the Lease holder reconciles: `tailor_leader` is 1 on one replica; `kubectl -n k8s-tailor get lease k8s-tailor-actuator`. The Lease needs the namespaced Role the chart creates.
14. **Container class.** Injected sidecars (`istio-proxy`, `linkerd-proxy`, `vault-agent`, ...) are recommend-only until opted in; init and ephemeral containers are never sized. See [CONTAINERS.md](CONTAINERS.md).
    A native Istio sidecar is reported as `injected-sidecar` with `nativeSidecar=true`, not as a separate class.
15. **`ignore` or a per-container pin.** `tailor.k8s-tailor.dev/ignore: "true"` is absolute and also wins over a mode or exclusion written from the UI or `POST /api/workload`. A container with `container.<name>.mode=Off` or `.exclude=true` (set in the UI's per-container controls or by annotation) is never actuated.

## No recommendations or wrong numbers

| Symptom | Check |
|---|---|
| UI empty, `tailor_recommendations` absent, log "refresh error" | `curl localhost:8080/healthz`: `vmHealthy` must be `true`. Then query the store for the two series: `count(container_cpu_usage_seconds_total)` and `count(container_memory_working_set_bytes)`. In reuse mode confirm `VM_URL`, labels (`namespace`, `pod`, `container`) and that the store holds only this cluster. |
| Engine up, `vmHealthy` true, but no recommendations ever appear (inert) with your own Prometheus/VictoriaMetrics | Every `container_*` query carries `job="<VM_JOB>"` (default `kubelet-resource`, the bundled vmsingle's job). If your kubelet scrape job has another name nothing matches. Check `count(container_cpu_usage_seconds_total{job="kubelet-resource"})` in your store, then set `tailor.vmJob` (env `VM_JOB`) to the real job name; empty disables the filter. |
| Log: `refresh: skipping ns <x>: ...` | A query for one namespace failed (for example a VictoriaMetrics 422 on a very large namespace). The rest of the fleet is still sized; `tailor_query_errors_total{backend="vm"}`. |
| Workload missing from the list | Its kind is not in `TailorConfig.spec.observedKinds`, its namespace is excluded, its labels do not match `observeSelector`, or no usage series carries its pod names. Owner-less Pods are included; Jobs and CronJobs are not observed kinds. |
| Memory recommendation is "high" | Intended: memory uses the 5 min maximum and is floored at the observed peak of the current regime, and an OOM raises it further ([TECHNICAL.md](TECHNICAL.md#2-recommendation-model)). Raise `memory.quantile` or lower margins per policy, or use `memory.max`. |
| Recommendation does not drop after a deploy that reduced usage | A change-point is only detected after a sustained shift (median-filtered, at least about 30 min and a quarter of the series); decay half-life is 24 h; scale-down stabilization holds 15 m. See [KNOWN-ISSUES.md](KNOWN-ISSUES.md). |
| The CPU request sits at the CPU limit and the pod is throttled | Usage pinned at the limit is censored (the demand is higher than the data shows). The engine grows an **existing** CPU limit by `CPU_LIMIT_GROWTH_FACTOR` (1.5) per step when at least a quarter of the last hour's samples are at >= 90% of the limit (or, if the cAdvisor series exist, a quarter of CFS periods are throttled), bounded by the policy `cpu.max` or the template limit x `CPU_CENSOR_MAX_FACTOR` (8), and parked after `CPU_CENSOR_MAX_STEPS` (3) raises that the usage keeps eating (`cpuGreedy`, `tailor_cpu_greedy`). It does **not** act under a shrink-only policy, on an HPA-held CPU, without CPU confidence, or on a container with no CPU limit; the bundled scrape has no throttling series, so bursts that stay under 90% on a 5-minute average are not detected. Raise `cpu.max`/the limit by hand if needed. See [TECHNICAL.md](TECHNICAL.md#censored-cpu). |
| A pod was just replaced (rollout, eviction) and its workload shows "learning" again | The history of replaced pods is attributed to their owning Deployment (by ReplicaSet name) or DaemonSet when that is unambiguous. StatefulSets, bare pods and label-grouped units are not guessed, so a replaced pod there starts without its predecessor's history. See [TECHNICAL.md](TECHNICAL.md#1-metrics-sources-and-queries). |
| Seasonal forecast shows "off" | The gate needs at least a day of hourly pairs and a correlation of at least 0.5 with the previous day; irregular load is deliberately not forecast. |
| Cost numbers look off | Prices are static (`PRICE_CPU_CORE_HR`, `PRICE_GB_HR`) unless `PRICING_MODE=opencost` finds `node_cpu_hourly_cost`; the UI labels them estimates. |
| `Spec pending` flag | An in-place resize changed the running pods but not the controller template; a restart or rollout would revert it. Persist it by updating the manifest in Git (the durable source), or use `apply: Rollout` where a restart is acceptable. |

## UI and API

| Symptom | Cause |
|---|---|
| MCP answers `400 bad request: non-canonical path` | The raw request-target contains `\`, a `%`-escape, `//`, a `.`/`..` segment or an absolute URI. This is deliberate (path-confusion guard); send canonical paths (`/mcp`). |
| MCP answers `403 host not allowed` / `cross-origin request refused` | Behind an Ingress set `mcp.allowedHosts: [tailor.example.com]`; for a UI hosted elsewhere add it to `mcp.allowedOrigins`. The engine's browser writes need `tailor.allowedHosts` the same way. |
| A `?token=` link does not sign in | Removed: links must carry the token in the fragment (`#token=...`), which never reaches a server. |
| Sign-in prompt in the UI | `/mcp` is Bearer-gated (always, by nginx and again by Bun): use `kubectl -n k8s-tailor get secret k8s-tailor-mcp-auth -o jsonpath='{.data.token}' \| base64 -d` (or your `existingSecret`). |
| "Change not applied: Making changes needs the engine's API token" | You are on the engine's UI (`:8080`): paste the engine write token (`k8s-tailor-api-auth`). Through the MCP the token is forwarded automatically. |
| Write API answers 401 / 503 / 415 / 403 / 413 | 401: wrong or missing `Authorization: Bearer`. 503: `TAILOR_API_AUTH_REQUIRED=true` but no token configured. 415: body must be `application/json`. 403: cross-site browser request refused. 413: body over 1 MiB. |
| `POST /api/policies` answers 400 | Validation message in `error` (bounds mirror the CRD schema: for example `limitFactor` must be 0 or >= 1, `quantile` in (0, 1]). |
| UI works by port-forward but not through the Ingress | `networkPolicy.mcpFrom` does not list the ingress controller namespace; or no TLS/auth proxy. |
| Metrics pods cannot be scraped by an external Prometheus | Add the Prometheus peer to `networkPolicy.metricsFrom`; the engine port `:8080` is otherwise restricted. |
| Grafana or another reader cannot query the bundled VictoriaMetrics | `:8428` is restricted to the engine; add the reader to `networkPolicy.metricsFrom`. |
| `kubectl get --raw .../services/k8s-tailor:8080/proxy/healthz` times out | Some CNIs enforce the engine NetworkPolicy against the API-server proxy; use a port-forward. |
| Nothing is blocked on a local cluster with NetworkPolicies on | The CNI may not enforce them, or the Docker runtime's kernel cannot run the CNI. Calico, Cilium and k3s's kube-router need netfilter pieces (`xt_CT`, `xt_NFLOG`, `rpfilter`, nft expressions) that some runtimes lack: on the "Dory" runtime (kernel 6.12.30-dory) Calico v3.33.0 never becomes Ready, Cilium 1.20.3 dies with `protocol not supported`, and plain k3d lets pod traffic through. A rootful Docker in a Lima VM (Ubuntu 24.04, kernel 6.8) works; rootless Docker does not (kubelet cannot open `/dev/kmsg`). `dev/calico/netpol-test.sh` aborts with exit 3 when its enforcement canary (a deny-all policy) does not produce a connect timeout. Details: [dev/calico/README.md](../dev/calico/README.md). |
| A policy blocks a client you expected to work | Check, in the release namespace, that the client's labels match: anything labelled `app=k8s-tailor` is treated as the engine by the ClickHouse and vmsingle policies, and the MCP policy admits the whole release namespace plus `networkPolicy.mcpFrom`. On a multi-node cluster the API server reaches the webhook from a node or tunnel address: if you set `networkPolicy.webhookFrom`, list it, otherwise the webhook silently fails open (`failurePolicy: Ignore`; watch `apiserver_admission_webhook_fail_open_count`). Untested on multi-node. |

## Webhook

| Symptom | Cause |
|---|---|
| No `MutatingWebhookConfiguration` appears | It is created only with `tailor.webhook.enabled=true` **and** a non-empty `tailor.actuateNamespaces`. |
| Pods are admitted unmodified | By design on any problem (`failurePolicy: Ignore`, 5 s timeout). Check the engine log for "webhook disabled: cert ... unusable" or "webhook: waiting for cert" (cert-manager has not issued yet), `webhook listening on :8443 (TLS)`, the workload's mode (`Webhook` or `Both`), confidence, and that a recommendation exists for the owner. |
| Sidecar added by Istio/Linkerd not sized on the first pass | `reinvocationPolicy: IfNeeded` (the default here) is what makes the webhook see a sidecar injected by another webhook; the patch is idempotent. The name `zz-k8s-tailor` does not avoid the second call: with real Istio 1.31.3 the API server called the webhook twice per pod whether it was named `aa-` or `zz-` (the Istio injector has `reinvocationPolicy: Never`). Correctness comes from `IfNeeded`, not from the name ([dev/istio/README.md](../dev/istio/README.md)). |
| Pods are admitted unmodified right after the engine started or the pods were replaced | Before the first refresh completes the webhook serves the warm cache (`k8s-tailor-warm`, written by the leader, `tailor.warmCache`); with none it admits pods unmodified and counts `tailor_webhook_skipped_total{reason="warming_up"}`; a container whose recommendation is not confident counts `reason="not_confident"`; a Guaranteed template is left alone and counts `reason="guaranteed_qos"`. The first refresh reads the whole window, so a restart over intact history recovers within seconds; a replaced pod keeps its predecessor's history for Deployments and DaemonSets ([TECHNICAL.md](TECHNICAL.md#8-webhook)). |
| TLS errors after rotation | The engine hot-reloads the mounted Secret (kubelet sync takes up to about a minute); admissions in that window are skipped, not blocked. With `certMode: helm` a re-rendered CA is only stable under `helm install/upgrade`. |
| Webhook-sized pod does not show in the decision log or `tailor_mutations_total` | By design the webhook records nothing at admission: the leader's reconciler records it on a later pass (delay up to `RECONCILE_EVERY`, 30 s), once per pod, and not while the engine is paused or no replica holds the Lease. `dryRun` admissions are never recorded. Look for the pod annotation `tailor.k8s-tailor.dev/admission-sized`. Only `CREATE` of `v1/Pod` without subresource is patched. |
| Pod from a Guaranteed template is not sized at creation | By design: the webhook leaves a Guaranteed template (requests equal to limits on every container) untouched so the pod stays Guaranteed; it counts `tailor_webhook_skipped_total{reason="guaranteed_qos"}` ([KNOWN-ISSUES.md](KNOWN-ISSUES.md)). |

## OOM kills

`kubectl get pods -A -o jsonpath` for `lastState.terminated.reason=OOMKilled`, then the workload's `tailor_recommendations{status="oom_risk"}`. For a container **with** a memory limit, the limit is protected at
`killed limit x 1.5` for 30 days and stored in the annotation `tailor.k8s-tailor.dev/oom-floor`. To forget a floor early, delete that annotation (engine memory is re-seeded from live OOMs and from annotations, so also let the OOM age out or restart
the engine). The same annotation is kept on Knative Services (object metadata) and the floor of a TailorGroup on the TailorGroup (`tailor.k8s-tailor.dev/group-oom-floor`); a bare Pod has no persisted floor. For containers without a limit no limit is invented unless the effective memory model sets `enforceLimit: true`: the request is raised after an OOM, capped at `peak x OOM_MEM_CAP_FACTOR` (default 4, chart `tailor.oomMemCapFactor`) and then by the policy `memory.max` ([KNOWN-ISSUES.md](KNOWN-ISSUES.md)). To exclude a container entirely: annotation `tailor.k8s-tailor.dev/container.<name>.exclude=true`.

## Observability quick checks

```promql
rate(tailor_resizes_total{result="failed"}[15m])        # failed actuations
topk(5, rate(tailor_resize_skipped_total[15m]))         # why things are held, by reason
tailor_pods_resize_state                                 # Pending/Infeasible, Pending/Deferred, InProgress
time() - tailor_last_refresh_success_timestamp_seconds  # staleness of recommendations
tailor_leader                                            # exactly one replica should be 1 (per replica series)
rate(tailor_query_errors_total[15m])                     # vm / clickhouse errors
rate(tailor_webhook_skipped_total[15m])                  # pods the webhook admitted unmodified, by reason
```

If you still cannot explain a behavior, open an issue with: Kubernetes version, the engine log excerpt, `kubectl get tailorconfig,rsp -o yaml`, and the relevant `/api/detail/...` JSON.
