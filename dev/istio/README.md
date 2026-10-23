# k8s-tailor with real Istio (k3d cluster `tailor-istio`)

`dev/k3s/smoke-sidecar.sh` only imitates the proxy (a container named `istio-proxy`). This directory runs the
same checks against a real Istio control plane and real injected Envoy sidecars.

- Istio **1.31.3** (latest stable on 2026-10-22; 1.32.0-beta.0 is the only newer tag), `istioctl install --set profile=minimal`
  (istiod only, no gateways), istiod requests 50m/128Mi.
- k3s `v1.34.12-k3s1` (in-place resize is on by default), images `k8s-tailor:dev` / `k8s-tailor-mcp:dev` built from HEAD and
  imported with `k3d image import`. MCP/dashboard disabled (not needed).
- Native sidecars are the **default since Istio 1.27** (upgrade notes: "Native sidecars are now enabled by default for eligible
  pods. This changes `istio-proxy` from a container to an init container."). The script sets `ENABLE_NATIVE_SIDECARS`
  explicitly (`false` = classic, `true` = native); istiod reports the value in its env.

## Reproduce

```sh
# classic sidecar (forced), then the scenarios (~15-20 min: the engine needs MIN_SAMPLES=2 x 5 min of history)
ISTIO_NATIVE=false dev/istio/mesh-test.sh all
# native sidecar: re-run only the Istio install + restart the workloads, or recreate the cluster
dev/istio/mesh-test.sh down && ISTIO_NATIVE=true dev/istio/mesh-test.sh all
# optional: webhook ordering experiment (renames the webhook to aa-k8s-tailor; restarts the engine)
ORDER_TEST=1 dev/istio/mesh-test.sh test
dev/istio/mesh-test.sh only s2_optin s5_webhook       # single scenarios
dev/istio/mesh-test.sh down
```

Manual equivalent of `up`:

```sh
k3d cluster create tailor-istio --image rancher/k3s:v1.34.12-k3s1 --servers 1 --agents 0 --no-lb \
  --k3s-arg "--disable=traefik@server:0" --k3s-arg "--disable=servicelb@server:0" --k3s-arg "--disable=metrics-server@server:0" \
  --kubeconfig-switch-context=false --wait
k3d image import k8s-tailor:dev k8s-tailor-mcp:dev -c tailor-istio
istioctl --context k3d-tailor-istio install -y --set profile=minimal --set values.pilot.env.ENABLE_NATIVE_SIDECARS=false \
  --set values.pilot.resources.requests.cpu=50m --set values.pilot.resources.requests.memory=128Mi
helm upgrade --install k8s-tailor chart --kube-context k3d-tailor-istio -n k8s-tailor --create-namespace \
  -f dev/k3s/values-dev.yaml -f dev/istio/values-istio.yaml --set tailor.image.repository=k8s-tailor --set tailor.image.tag=dev --wait
kubectl --context k3d-tailor-istio apply -f dev/istio/mesh-demo.yaml -f dev/istio/scenarios.yaml
```

Files: `values-istio.yaml` (webhook on, `actuateNamespaces: mesh-demo,istio-system,kube-system`), `mesh-demo.yaml` (namespace with
`istio-injection=enabled`, policy, `web` x2 + `client`), `scenarios.yaml` (`optin`, `optin-load`, `roll`, `wh` + their policies),
`adversarial-policy.yaml` (priority-1 policy on `*`), `mesh-test.sh`.

Timing observed: k3d cluster 25 s, Istio install 14 s, chart 45 s, first workload Ready 20 s after apply, first in-place
resize ~7 min after the pods started, first template/annotation rollout ~8 min.

## Results (Istio 1.31.3)

| # | Scenario | Classic | Native |
|---|---|---|---|
| 1 | proxy `injected-sidecar`/`istio`/Recommend; app resized in place; proxy untouched | PASS | PASS |
| 2 | annotation `container.istio-proxy.mode=InPlace` resizes the proxy in place, no restart | PASS | PASS |
| 3 | native sidecar: `spec.initContainers` restartPolicy Always, resized through `pods/resize` | n/a | PASS |
| 4 | template path writes the four `sidecar.istio.io/proxy*` annotations, no container patched, Istio applies them | PASS | PASS |
| 5 | injection + Tailor webhook on the same new pod; `reinvocationPolicy: IfNeeded`; `zz-` name | PASS | PASS |
| 6 | istiod / coredns never touched (also with a `*` policy and the webhook selector matching both namespaces) | PASS | PASS |
| 7 | `client -> web` through Envoy after the resizes | PASS | PASS |

Key outputs (real):

```
# (1) classic
web istio-proxy class=injected-sidecar native=False mesh=istio tmpl=False mode=Recommend path=none pol=default:injected-sidecar
web app         class=app              native=False mesh=      tmpl=True  mode=InPlace   path=in-place
in-place resize mesh-demo/web[app] -> cpu=500m mem=32Mi (was cpu=500m mem=64Mi)
web-...  app[req=500m/32Mi rc=0] istio-proxy[req=100m/128Mi lim=2/1Gi rc=0]     # proxy unchanged 40+ min later

# (2) classic, opt-in, proxy starved on purpose (template annotations proxyCPU 20m / proxyCPULimit 50m)
in-place resize mesh-demo/optin[istio-proxy] -> cpu=50m mem=40Mi (was cpu=20m mem=64Mi)     # restartCount 0, same UID
```

### (2) cgroup change versus real effect (measured)

The proxy of `optin` is hammered by 6 parallel clients. cgroup v2 `cpu.max` / `cpu.stat` read inside the `istio-proxy` container:

```
before tailor (limit 50m)     cpu.max=5000 100000   cpu_used=51m    throttled_periods=204/204 (100%)
after tailor (req 20m->50m)   cpu.max=5000 100000   cpu_used=55m    throttled_periods=167/167 (100%)   # no effect: only the REQUEST moved
manual pods/resize limit 500m cpu.max=50000 100000  cpu_used=276m   throttled_periods=6/154 (4%)       # limit raised: throttling gone
Envoy: server.concurrency: 1 before and after                                                          # worker threads fixed at start-up
```

Conclusions: (a) a request change only alters `cpu.weight`, it matters under contention and nothing else; the quota that
throttles Envoy is the **limit**. (b) Raising the limit in place works immediately (Envoy used 5x more CPU), (c) Envoy's
worker count (`--concurrency`, derived from the limit at start-up) is not recomputed, so a proxy started under a tiny limit
stays single-worker after a resize. (d) Memory: Envoy does not read its cgroup limit at runtime, a resize only moves the OOM line.
With `cpu.limitFactor: 4` on the `istio-proxy` rule tailor did raise request and limit once the unthrottled usage showed up
(`req=323m lim=1292m` on the native run, no restart).

### (4) template path (real)

```
proxy-annotations mesh-demo/roll[istio-proxy] -> cpu=100m mem=40Mi
template annotations: sidecar.istio.io/proxyCPU=100m  proxyCPULimit=2000m  proxyMemory=40Mi  proxyMemoryLimit=1024Mi
template containers: [app]            # no istio-proxy appended
new pod: istio-proxy requests 100m/40Mi, limits 2/1Gi
```

The injection-template claim was verified: a pod with only `sidecar.istio.io/proxyCPU: 15m` gets
`istio-proxy resources: {"requests":{"cpu":"15m"}}` (no memory request, no limits). That is why the limits are written together.

### (5) webhook order

`istio-sidecar-injector` has `reinvocationPolicy: Never` and four webhooks; `zz-k8s-tailor` has `IfNeeded`. New `wh` pods came out with
the proxy injected and both containers mutated (`app 500m/256Mi -> 150m/32Mi`, `istio-proxy 100m/128Mi -> 10m/46Mi`; native: the proxy patched under
`spec.initContainers`). The apiserver metric `apiserver_admission_webhook_request_total{name="tailor.k8s-tailor.dev"}` rose by **2 calls per pod** both with
`aa-k8s-tailor` (before Istio) and with `zz-k8s-tailor`, so the pod is reinvoked either way and the idempotent patch held. The name therefore buys
little: correctness comes from `IfNeeded`.

## Findings / caveats

This run changed no code; it surfaced the findings below. Items 1, 2 and 4 were fixed afterwards (commits `5af26dc`, `40a6bec`, `68fddfd`),
covered by unit tests and the `throttled-cap` backtest scenario only. **The fixes were not re-run against a real Istio.**

1. **A throttled proxy could not ask for more CPU (fixed).** The recommender saw usage capped by the limit (optin proxy: usage 50.8m with
   limit 50m, recommendation 66m but never applied, swallowed by the 20m absolute drift gate). The engine now detects censored CPU (a quarter
   of the last hour's samples at 90% or more of the live limit, or optional cAdvisor throttling series) and raises the limit in steps
   (`CPU_LIMIT_GROWTH_FACTOR`, ceiling `CPU_CENSOR_MAX_FACTOR`). See `docs/TECHNICAL.md`.
2. **The webhook stayed silent after pods were replaced (fixed; the cause was not the engine restart).** The usage history was only
   attributed to pods that still exist, so every pod replacement (rollouts, deletes, helm upgrades) dropped the workload's history and the
   `MIN_SAMPLES` gate had to be satisfied again. Pods that no longer exist are now attributed through their ReplicaSet or DaemonSet name.
   The webhook also counts skipped admissions in `tailor_webhook_skipped_total{reason}`.
3. The brief expected `native-sidecar` for Istio native proxies; the code reports `containerClass=injected-sidecar` with `nativeSidecar=true`
   (`internal/k8s/containers.go:130-136`, documented in `docs/CONTAINERS.md` 2.1). Not a bug.
4. `signals.memTrendBytes` showed 829 MB for a 44 MB container (fixed). The trend now needs at least 24 points and R^2 of 0.5 or more and
   is never projected beyond the history it was fitted on.
5. Re-running `kubectl apply` on a workload tailor already resized reverts the template (3-way merge) and rolls the Deployment; the test
   manifests are meant to be applied once. See the GitOps note in `docs/KNOWN-ISSUES.md`.

## Outside this suite

- `ORDER_TEST` through the script (done by hand with the same commands); the full `mesh-test.sh all` from an empty cluster was not run end to
  end (setup and scenarios were run separately; `test` was run against the live cluster in native mode).
- Ambient mode, Istio revisions/tags, `proxy.istio.io/config` concurrency, HPA interplay, multi-node scheduling, Istio CNI.
- The fall-back path on kubelets without in-place resize, combined with real Istio.
- The 10-minute rollout cooldown (`minRolloutInterval`) was not exercised beyond one rollout per container.
