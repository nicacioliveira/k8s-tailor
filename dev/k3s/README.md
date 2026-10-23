# Local test infra (k3s via k3d)

Brings up a 1-node k3d cluster (`tailor-dev`) with the full chart (engine, MCP/dashboard,
VictoriaMetrics, ClickHouse), applies `demo/` and runs a smoke test that proves in-place
resize without restart.

## Usage

```sh
make k3s-up       # creates the cluster, builds/imports the 2 images, helm install, applies demo/
make k3s-smoke    # PASS/FAIL asserts (exit 1 if anything fails)
make k3s-down     # removes the cluster
```

Equivalents: `dev/k3s/up.sh`, `dev/k3s/smoke.sh`, `dev/k3s/down.sh`.

Sidecars (without installing Istio): `dev/k3s/smoke-sidecar.sh` brings up a Deployment with
`app` + an imitation `istio-proxy` (annotation `sidecar.istio.io/status` on the pod) and
verifies that the app is resized in-place, that the proxy is classified
`injected-sidecar`/`istio` and stays in `Recommend` (requests unchanged), and that after the
annotation `tailor.k8s-tailor.dev/container.istio-proxy.mode=InPlace` the proxy is also
resized without a restart. See `docs/CONTAINERS.md`.
Requirements on PATH: `docker`, `k3d`, `kubectl`, `helm`.

## What gets created

- Cluster `tailor-dev`, image `rancher/k3s:v1.34.12-k3s1` (K8s 1.34: `InPlacePodVerticalScaling`
  is beta/default-on since 1.33; `pods/resize` exists). `up.sh` also passes the feature gate
  explicitly to the apiserver/kubelet (idempotent), so images >=1.27 work.
  Traefik, servicelb and metrics-server disabled.
- Images `k8s-tailor:dev` and `k8s-tailor-mcp:dev` built locally and imported with
  `k3d image import` (no registry).
- Helm release `k8s-tailor` in namespace `k8s-tailor` with `values-dev.yaml`
  (VM/ClickHouse at minimum size, MCP without Bearer auth, `MIN_SAMPLES=2`, `REFRESH_EVERY=10s`).
  `MIN_SAMPLES=2` is a dev-only shortcut so the first resize happens in minutes; the production
  chart default is `288` (about 24h of 5-minute samples, a full daily cycle).
- `demo/workloads.yaml` + `demo/cpu-burner.yaml` (`DEMO_FULL=1` also includes `scenario.yaml` and `cpu-wave.yaml`).

## Metrics

The engine only queries `container_cpu_usage_seconds_total` and `container_memory_working_set_bytes`
(+ its own `tailor_*`). The chart's scrape (`kubelet-resource`, via the apiserver proxy at
`/api/v1/nodes/<node>/proxy/metrics/resource`) already collects them on k3s: no need for kube-state-metrics
or an extra cAdvisor. The scrape interval in dev is 10s.

## Variables

| Var | Script | Effect |
|---|---|---|
| `SKIP_BUILD=1` | up | reuses local images |
| `SRC_REF=HEAD` | up | builds from a `git archive` of the ref (ignores a dirty working tree) |
| `SKIP_DEMO=1` / `DEMO_FULL=1` | up | does not apply / applies the full demo |
| `K3S_IMAGE`, `CLUSTER` | all | changes the k3s version / cluster name |
| `INPLACE_TIMEOUT` (720s), `VM_TIMEOUT` (120s) | smoke | timeouts |
| `KEEP_DEMO=1` / `KEEP_SMOKE=1` | smoke | does not scale the demo to 0 / does not delete the smoke ns |

## What the smoke verifies

1. Pods in namespace `k8s-tailor` Ready (rollout of deployments + statefulsets).
2. 4 CRDs `tailor.k8s-tailor.dev` and `TailorConfig/cluster`.
3. Engine `/healthz` (via the apiserver service proxy; does not use port-forward).
4. VictoriaMetrics has `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, `tailor_current_cpu_cores`.
5. Creates a unique namespace `tailor-smoke-<ts>` + `TailorPolicy` `mode: InPlace` + Deployment with
   500m requested / ~110m used; waits for the pod's requests to change and checks that the **UID** and
   **restartCount** of the same pod did not change.

## Notes and gotchas

- **Time to the first resize**: the engine samples at a fixed step of 300s (`stepSec` in
  `internal/recommender`), so `MIN_SAMPLES=N` needs ~N*5min of history. With `2`, the smoke takes ~10 min.
  This is why dev uses `MIN_SAMPLES=2`; the production chart default of `288` (about 24h of
  5-minute samples) is intentional: it waits for a full daily cycle before shrinking requests.
- `demo/` asks for ~6 CPUs (on purpose, oversized) and fills the node; so the smoke scales
  the `tailor-demo` deployments to 0 (use `KEEP_DEMO=1` to leave them alone). `mem-underprovisioned`
  stays in CrashLoop by design.
- The engine keeps a scale-down cooldown (1h) in memory per `ns/workload`; that is why the smoke uses a
  new namespace on every run.
- Without `docker buildx` (BuildKit), `up.sh` falls back to a legacy builder with a temporary Dockerfile without
  `--platform=$BUILDPLATFORM` (the legacy builder does not define that ARG). With buildx, it uses the Dockerfile directly.
- The first build is slow (downloads bun/go/modules); later builds use the cache.
- UI access: `kubectl --context k3d-tailor-dev -n k8s-tailor port-forward svc/k8s-tailor-mcp 8020`
  (or `svc/k8s-tailor 8080` for the engine's embedded UI).

## Full E2E (docs/E2E.md)

`values-dev.yaml` turns off the MCP nginx sidecar. For the full e2e, after `up.sh`, a single upgrade turns on the rest:

```sh
helm upgrade k8s-tailor chart --kube-context k3d-tailor-dev -n k8s-tailor --reuse-values \
  --set tailor.replicas=2 --set tailor.webhook.enabled=true --set tailor.webhook.certMode=helm \
  --set tailor.actuateNamespaces=e2e-webhook \
  --set tailor.advisor.enabled=true --set tailor.advisor.baseURL=http://fake-llm.e2e-advisor.svc/cgi-bin \
  --set tailor.advisor.model=fake-model --set tailor.admissionPolicy.enabled=true \
  --set mcp.auth.enabled=true --set mcp.auth.allowUnauthenticated=false --wait
```

With the gate on, the UI at `:8020` asks for the token (`secret/k8s-tailor-mcp-auth`). Gotchas: `port-forward svc/...` stays pinned
to the pod from before the upgrade (redo it after each rollout); `polinux/stress` (demo `mem-underprovisioned`) is amd64 and does not
run on an arm64 node — for the OOM scenario use a multi-arch image (e.g. `python:3.12-alpine`); local port 8443 may
be taken (Dory), use another one to talk to the webhook.
