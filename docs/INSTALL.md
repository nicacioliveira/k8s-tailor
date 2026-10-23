# Installing on a real cluster

> **Compatibility.** Requires Kubernetes 1.33 or later (in-place pod resize; GA in 1.35). The project suites run on k3s v1.34; other conformant distributions use the same APIs, so open an issue if something differs. The sections below list what to check on each distribution and roll out in the order of [section 6](#6-progressive-rollout).

The chart is pre-1.0 (chart `0.2.0`, app `0.1.0`). It installs everything in one release: engine, MCP server plus dashboard, VictoriaMetrics and a
single-node ClickHouse. It ships **inert**: nothing is changed until a `TailorPolicy` (or annotation) enables actuation.

Automated installs by an LLM agent: [LLM-INSTALL.md](LLM-INSTALL.md). Settings reference: [CONFIGURATION.md](CONFIGURATION.md).

Contents: [1 Prerequisites](#1-prerequisites) · [2 Metrics source](#2-metrics-source) · [3 Storage and resources](#3-storage-and-resources) ·
[4 Images and chart](#4-images-and-chart) · [5 Install](#5-install-step-by-step) · [6 Rollout](#6-progressive-rollout) ·
[7 Verify](#7-verify-the-installation) · [8 UI access](#8-accessing-the-ui) · [9 GitOps](#9-argocd-and-flux) · [10 Upgrade and rollback](#10-upgrade-and-rollback) ·
[11 Uninstall](#11-uninstall-completely) · [12 Troubleshooting](#12-troubleshooting)

---

## 1. Prerequisites

### Kubernetes version and the resize feature

In-place pod resize (`pods/resize` subresource, `InPlacePodVerticalScaling`) history, per the Kubernetes documentation and release blog (checked 2026-10-21):

| Kubernetes | State of in-place resize |
|---|---|
| 1.27 to 1.32 | alpha, feature gate **off** by default |
| **1.33** | beta, feature gate **on** by default (the task page says "your Kubernetes server must be at or later than version 1.33") |
| 1.35 | stable, gate locked on |

**Minimum recommended: Kubernetes 1.33.** Also needed: `kubectl` >= 1.32 to use `--subresource=resize` by hand. On 1.27 to 1.32 you must enable
`InPlacePodVerticalScaling=true` on both the kube-apiserver and every kubelet; `dev/k3s/up.sh` passes that gate to k3s for older images, the project suites run on 1.34.
If the API server has no `pods/resize`, the engine does not fail: managed Deployments, StatefulSets and DaemonSets fall back to rollouts, and a discovery
*error* holds (no action, retries every 30 s) rather than turning into a rollout.

Limitations that come from Kubernetes itself (docs, "Limitations"): only CPU and memory; the QoS class cannot change (a Guaranteed pod stays Guaranteed, BestEffort cannot gain requests);
non-restartable init containers and ephemeral containers cannot be resized, sidecars can; requests and limits cannot be removed once set; no Windows pods; pods under the static
CPU or Memory manager policies cannot be resized in place; with swap, memory resizes need `RestartContainer`; if the memory `resizePolicy` is `NotRequired`, a memory decrease is best effort and
may stay `InProgress` while usage is above the new limit.

### Per distribution: what to check

| Distribution | Check before installing |
|---|---|
| k3s / k3d | `kubectl version`, server >= 1.33. |
| kubeadm | Server >= 1.33, or the gate on apiserver and kubelets for older versions. |
| EKS, GKE, AKS | Cluster minor version >= 1.33 (then the gate is on by default). Managed control planes generally do not let you set feature gates yourself; confirm the provider's current docs before relying on an older version. |
| OpenShift | Verify the OpenShift release's Kubernetes level and its SCC/PSA rules for the non-root pods; the chart sets explicit non-root `securityContext`s. |
| Any | **A CNI that enforces `NetworkPolicy`, if you want the chart's policies to have any effect.** Without one (for example plain flannel) they are accepted by the API server and silently do nothing: the chart still works, but the engine API, vmsingle and ClickHouse are then reachable from any pod. Calico, Cilium, Antrea and kube-router enforce them; the chart's Calico suite ([dev/calico](../dev/calico/README.md)) checks that the policies block and allow the chart's traffic as intended. Node kubelets must serve `/metrics/resource` (and `/metrics/cadvisor` for the throttling series) through the API-server node proxy for the bundled scrape. |

Preflight commands:

```bash
kubectl version -o json | grep -E 'gitVersion'           # serverVersion.gitVersion should be v1.33 or newer
kubectl get --raw /api/v1 | grep -o '"name":"pods/resize"' # expect: "name":"pods/resize"
helm version --short                                       # Helm 3
kubectl get storageclass                                   # one marked (default), or pick storageClassName
kubectl -n kube-system get pods -o name | grep -Ei 'calico|cilium|antrea|kube-router|weave'   # which CNI? none listed: check your provider's docs
```

NetworkPolicy notes from the Calico suite ([dev/calico/README.md](../dev/calico/README.md)): ingress traffic that originates on the node itself (kubelet probes, the API server calling a local pod) is not filtered by Calico, so on a single node `networkPolicy.webhookFrom` has no effect on the API server; on a multi-node cluster the API server's source is a node or tunnel address that `webhookFrom` must list, or the webhook silently fails open. Probes under a policy depend on the CNI. Local clusters on exotic Docker runtimes may not enforce at all, see [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

### Other requirements

- **Helm 3** and `kubectl` with cluster-admin (the release creates CRDs, ClusterRoles and, optionally, a webhook configuration).
- **A default StorageClass** (or set `storageClassName`): VictoriaMetrics and ClickHouse use PVCs (10Gi each by default).
- **cert-manager** only if you enable the webhook with `certMode: certManager`.
- **Network egress** only if you enable the LLM advisor (the chart restricts ingress only).
- One release per cluster (fixed object names and cluster-scoped RBAC).

## 2. Metrics source

The engine reads exactly two series, with the labels `namespace`, `pod`, `container`: `container_cpu_usage_seconds_total` and
`container_memory_working_set_bytes` (queries in [TECHNICAL.md](TECHNICAL.md#1-metrics-sources-and-queries)). It applies **no cluster selector**, so the store it queries must hold only this cluster's
series.

| Option | How | When |
|---|---|---|
| **Bundled VictoriaMetrics** (default) | `victoria-metrics-single` subchart scrapes each node's `/metrics/resource` through the API server (`/api/v1/nodes/<node>/proxy/metrics/resource`, 5 s) with its own ServiceAccount (`nodes` get/list/watch, `nodes/proxy` get) in the job `kubelet-resource`, plus a second job `kubelet-cadvisor` that keeps only the two CFS series (`container_cpu_cfs_throttled_periods_total`, `container_cpu_cfs_periods_total`) for the throttling signal. It also scrapes every engine pod's `/metrics` (pod service discovery, Role `k8s-tailor-vmsingle-pods`), so per-replica counters stay separate. | Recommended: nothing else to install. |
| **Existing VictoriaMetrics / Prometheus-compatible store** | `--set victoria-metrics-single.enabled=false --set tailor.env.VM_URL=http://<host>:8428`. The store must already ingest the two series (kubelet `/metrics/resource` or cAdvisor `/metrics/cadvisor` both expose them) with the standard labels **and `job="kubelet-resource"`** (set `tailor.vmJob` to your kubelet scrape job's name, or empty to drop the filter; a wrong value leaves the engine inert, see [TROUBLESHOOTING.md](TROUBLESHOOTING.md)), retain at least `WINDOW` (7 d), and be reachable from the engine pod. For the optional CFS throttling signal the store must also hold `container_cpu_cfs_throttled_periods_total` and `container_cpu_cfs_periods_total` from cAdvisor: set `tailor.vmThrottleJob` (`VM_THROTTLE_JOB`) to the job that scrapes them (default `kubelet-cadvisor`); with a wrong value only the throttling signal is lost. The engine also reads its own `tailor_*` series back for charts, so the store must scrape the engine (`prometheus.io/*` pod annotations are set by default, or enable `serviceMonitor`). | You already run a metrics stack. `CPU_RATE_MODE=max` needs VictoriaMetrics (`rollup_rate`). |

No kube-state-metrics, metrics-server or node-exporter is needed. A decision store is optional: `--set clickhouse.enabled=false` (history then lives in a 200-event in-memory ring) or point
`tailor.env.CH_URL`, `CH_USER`, `CH_PASSWORD` at an existing ClickHouse (same tables).

## 3. Storage and resources

Defaults from `chart/values.yaml`:

| Component | Requests | Limits | Storage |
|---|---|---|---|
| Engine | 50m CPU, 128Mi | memory 256Mi | none (writes nothing to disk) |
| MCP + nginx | 50m, 128Mi (MCP) | memory 256Mi | none |
| vmsingle | 100m, 256Mi | memory 512Mi | PVC 10Gi, retention 8d |
| ClickHouse | 200m, 512Mi | memory 2Gi | PVC 10Gi, TTL 90d |

These are sized for small clusters. On larger ones, watch `tailor_refresh_duration_seconds` and raise the engine and vmsingle
resources if refreshes approach `REFRESH_EVERY`. Use a storage class with `fsGroup` support for the non-root volumes, or see the vmsingle ownership note in [UPGRADE.md](UPGRADE.md) (opt-in overlay `chart/values-vmsingle-chown.yaml`).

## 4. Images and chart

- The chart is installed from a checkout of this repository (there is no published Helm repository that this documentation verified).
- The chart's default images are `ghcr.io/nicacioliveira/k8s-tailor` and `ghcr.io/nicacioliveira/k8s-tailor-mcp` at tag `v<appVersion>` = `v0.1.0`. **These images do not exist yet**: the repository is
  private, no `v0.1.0` tag was ever pushed, and `.github/workflows/build-images.yml` runs only on `v*` tags or a manual dispatch, so a default install ends in `ImagePullBackOff`. Pick one of the two real options
  (full detail in [UPGRADE.md#images](UPGRADE.md#images)):

  **(a) Publish a release.** This is an action for the repository owner (it creates a tag and GHCR packages); do not run it on a repository you do not own.

```bash
git tag v0.1.0 && git push origin v0.1.0         # the tag must equal v<appVersion> in chart/Chart.yaml
# or, without a tag: gh workflow run build-images.yml -f version=v0.1.0
```

  Packages pushed from a private repository are private: make `k8s-tailor` and `k8s-tailor-mcp` public in GitHub (Packages -> Package settings -> Change visibility), **or** give the cluster a pull secret
  (`kubectl -n k8s-tailor create secret docker-registry ghcr-pull --docker-server=ghcr.io --docker-username=<user> --docker-password=<PAT read:packages>`) and install with
  `--set 'global.imagePullSecrets[0].name=ghcr-pull'`.

  **(b) Build locally** (dev, air-gapped, or your own registry) and load the images into the cluster:

```bash
make docker TAG=dev                                                     # k8s-tailor:dev and k8s-tailor-mcp:dev
k3d image import k8s-tailor:dev k8s-tailor-mcp:dev -c <cluster>         # k3d
# kind load docker-image k8s-tailor:dev k8s-tailor-mcp:dev --name <cluster>   # kind
# own registry: make docker TAG=0.1.0 IMAGE=registry.example.com/platform/k8s-tailor MCP_IMAGE=registry.example.com/platform/k8s-tailor-mcp, docker push both
```

  then install with `--set tailor.image.repository=k8s-tailor --set tailor.image.tag=dev --set mcp.image.repository=k8s-tailor-mcp --set mcp.image.tag=dev` (or your registry's names, plus
  `global.imagePullSecrets` if it is private). `make k3s-up` does all of this for a throwaway k3d cluster. Pin by digest in production (`tailor.image.digest`, `mcp.image.digest`).
- `chart/charts/` is git-ignored, so on a clean clone fetch the VictoriaMetrics subchart **before** `helm lint`, `helm template` or `helm install`:

```bash
helm dependency build chart       # downloads victoria-metrics-single 0.43.0 into chart/charts/
```

## 5. Install, step by step

1. **Choose a namespace** (the release namespace). It is always excluded from actuation, together with the system namespaces in `tailor.excludeNamespaces`. Example: `k8s-tailor`.

2. **Create the secrets you manage yourself** (recommended for production; otherwise the chart generates random ones on first install and reuses them on upgrade through `lookup`):

```bash
kubectl create namespace k8s-tailor
kubectl -n k8s-tailor create secret generic k8s-tailor \
  --from-literal=api-token="$(openssl rand -hex 32)" \
  --from-literal=mcp-token="$(openssl rand -hex 32)" \
  --from-literal=clickhouse-password="$(openssl rand -hex 32)"
```

   (With GitOps, provision this Secret from your secret manager instead; see [section 9](#9-argocd-and-flux) and `deploy/argocd/externalsecret.yaml`.)

3. **Write a values file.** A production starting point, assuming a 1.33+ cluster, an ingress controller in `ingress-nginx`, and **no actuation yet**:

```yaml
# values-prod.yaml
secretGeneration:
  failWithoutLookup: true        # a forgotten existingSecret fails the render instead of rotating credentials

tailor:
  image: { repository: ghcr.io/nicacioliveira/k8s-tailor, tag: v0.1.0 }   # only exists after a release is published; else your own, ideally digest-pinned
  replicas: 2                    # optional HA: leader election is on by default, a PDB is added at replicas > 1
  apiAuth:
    existingSecret: k8s-tailor
    secretKey: api-token
  env:
    MIN_SAMPLES: "288"           # chart default, shown on purpose: 24 h of coverage before any actuation
  webhook:
    enabled: false               # keep the in-place reconciler as the only path until you need admission-time sizing
  admissionPolicy:
    enabled: true                # Kubernetes >= 1.30; limits what the engine ServiceAccount may change
    validationActions: [Warn, Audit]   # switch to [Deny] after you saw no warnings

clickhouse:
  auth:
    existingSecret: k8s-tailor
    passwordKey: clickhouse-password
  # storageClassName: fast-ssd

victoria-metrics-single:
  server:
    persistentVolume: { size: 20Gi }   # retention is 8d; size to your series count
    # persistentVolume.storageClassName: fast-ssd

mcp:
  image: { repository: ghcr.io/nicacioliveira/k8s-tailor-mcp, tag: v0.1.0 }
  auth:
    enabled: true
    existingSecret: k8s-tailor
    secretKey: mcp-token

networkPolicy:
  enabled: true
  mcpFrom:
    - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: ingress-nginx } }
  # metricsFrom:                 # allow a fleet Prometheus/vmagent to scrape the engine
  #   - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: monitoring } }
```

   The default `tailor.excludeNamespaces` list (system, mesh, GitOps and monitoring namespaces; see [CONFIGURATION.md](CONFIGURATION.md)) is already applied, and the release namespace is always added. If you set
   `tailor.excludeNamespaces` yourself it **replaces** the default list (Helm replaces lists), so copy the defaults from `chart/values.yaml` first and append, e.g. `keda`.

   The webhook with `certMode: helm` (the default) needs a live API server at render time. For the webhook use `tailor.webhook.certMode: certManager` (requires cert-manager) or `existingSecret`.

4. **Install:**

```bash
helm dependency build chart
helm upgrade --install k8s-tailor ./chart \
  --namespace k8s-tailor --create-namespace \
  -f values-prod.yaml --wait --timeout 10m
```

   `helm lint chart -f values-prod.yaml` and `helm template k8s-tailor chart -f values-prod.yaml > /dev/null` validate the values first (with `certMode: helm` and no API server the render fails by design).

5. Continue with [section 7](#7-verify-the-installation) before touching any policy.

## 6. Progressive rollout

Order: Off, then Recommend, then one namespace in place, then more. The mode names: `Off` (ignore), `Recommend` (compute only), `Webhook`, `InPlace`, `Both`. Do not skip stages.

1. **Recommend only (the chart default).** `TailorConfig.defaultMode` is `Recommend`. Leave it for at least the confidence window (with `MIN_SAMPLES=288`, 24 h) and review the Workloads, Savings and Health views.
   If you want some namespaces fully ignored, add a policy with `mode: Off` or the annotation `tailor.k8s-tailor.dev/ignore: "true"` on the workload.
2. **One namespace, shrink-only, in place.** Pick a low-risk, non-critical namespace with Burstable pods (requests below limits, or no limits) and apply:

```yaml
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorPolicy
metadata: { name: pilot }
spec:
  priority: 100
  selector: { namespaces: ["my-pilot-ns"] }
  mode: InPlace
  apply: InPlace
  direction: Down          # never raises anything; the most conservative savings posture
```

   Watch Decisions, `kubectl get events -n my-pilot-ns --field-selector reason=Rightsized`, and confirm `kubectl get pod <p> -o jsonpath='{.status.containerStatuses[*].restartCount}'` stays 0.
   Kill switch at any time: `kubectl patch tailorconfig cluster --type=merge -p '{"spec":{"paused":true}}'`.
3. **Allow growth** (`direction: Both`) once the shrink phase behaved, then widen the selector namespace by namespace. Keep databases and anything with a restart-on-resize `resizePolicy` in `Recommend` until you understand
   the disruptive-resize path ([TECHNICAL.md](TECHNICAL.md#7-actuation)).
4. **Sidecars** (Istio, Linkerd, Vault) stay recommend-only until a rule or annotation opts them in ([CONTAINERS.md](CONTAINERS.md)). **HPA** workloads keep their scaling resource held automatically.
5. Enable the **webhook** only for namespaces that mint ephemeral pods (`tailor.actuateNamespaces`); it is the only path that sizes pods at creation.

## 7. Verify the installation

```bash
kubectl -n k8s-tailor get pods
# NAME                              READY   STATUS    
# k8s-tailor-<hash>                 1/1     Running     engine (2 pods at replicas: 2)
# k8s-tailor-mcp-<hash>             2/2     Running     mcp + nginx-auth sidecar (1/1 with mcp.auth.enabled=false)
# k8s-tailor-clickhouse-0           1/1     Running
# k8s-tailor-vmsingle-...-0         1/1     Running     name starts with k8s-tailor-vmsingle

kubectl get crd | grep tailor.k8s-tailor.dev
# tailorconfigs.tailor.k8s-tailor.dev ... tailorgroups ... tailorpolicies ... tailorprofiles   (4 lines)

kubectl get tailorconfig cluster -o jsonpath='{.spec.paused} {.spec.defaultMode}{"\n"}'
# false Recommend

# Engine health through the API server's service proxy (no port-forward). A NetworkPolicy may block
# the proxy path on some CNIs; then use: kubectl -n k8s-tailor port-forward svc/k8s-tailor 8080:8080
kubectl get --raw /api/v1/namespaces/k8s-tailor/services/k8s-tailor:8080/proxy/healthz
# {"ok":true,"source":"in-cluster","actuation":"dry-run","vmUrl":"http://k8s-tailor-vmsingle.k8s-tailor.svc:8428","vmHealthy":true}

kubectl get --raw "/api/v1/namespaces/k8s-tailor/services/k8s-tailor-vmsingle:8428/proxy/api/v1/query?query=count(container_cpu_usage_seconds_total)"
# ..."status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[<ts>,"<n>"]}]}   (non-empty result)

kubectl -n k8s-tailor logs deploy/k8s-tailor | grep -E 'refresh ok|decision store|webhook|leader'
# refresh ok: <N> recommendations
# decision store -> ClickHouse http://k8s-tailor-clickhouse.k8s-tailor.svc:8123 db=tailor

kubectl -n k8s-tailor get lease k8s-tailor-actuator       # holder is one engine pod
```

`actuation` reads `dry-run` until some workload resolves to a mode that actuates, then `active`; `paused` after the kill switch. Recommendations appear after the first refresh (60 s); a workload shows
"learning" until it has `MIN_SAMPLES` scrape points.

## 8. Accessing the UI

- **Port-forward** (simplest, and it bypasses NetworkPolicy):

```bash
kubectl -n k8s-tailor port-forward svc/k8s-tailor-mcp 8020:8020      # then http://localhost:8020
kubectl -n k8s-tailor get secret k8s-tailor-mcp-auth -o jsonpath='{.data.token}' | base64 -d   # MCP/UI token
kubectl -n k8s-tailor get secret k8s-tailor-api-auth -o jsonpath='{.data.token}' | base64 -d   # engine write token
```

  With your own `existingSecret`, read the Secret you created instead. The page shell loads without a token; every data call goes through `/mcp`, which needs the **MCP token** (the UI shows a sign-in). The MCP forwards the engine
  write token by itself, so you do not paste it. Only the engine's own UI on `:8080` asks for the engine token on the first change ("Token needed to make changes", see [SCREENS.md](SCREENS.md)).
- **Ingress**: the Service is `k8s-tailor-mcp:8020`. Put TLS and SSO in front (oauth2-proxy, an auth-aware ingress, or a VPN gateway): the Bearer token is a static shared secret, the UI shell at `/` is public (no data), and `/mcp` is
  the path to the engine's write API. List your ingress controller's namespace under `networkPolicy.mcpFrom`, otherwise the NetworkPolicy blocks it. The MCP/UI does not do OIDC itself.
- The engine also serves the same dashboard on `svc/k8s-tailor:8080`; its reads are unauthenticated unless `tailor.apiAuth.protectReads=true` (then every `GET /api/*` needs the engine token; the embedded UI shows a token dialog), so keep it behind the NetworkPolicy.

## 9. ArgoCD and Flux

GitOps tools render with `helm template`, which has no `lookup`, so chart-generated secrets would change on every sync. Set an `existingSecret` for `tailor.apiAuth`, `mcp.auth` and `clickhouse.auth` and
`secretGeneration.failWithoutLookup: true`; for the webhook use `certManager` or `existingSecret`. Working examples (adjust the placeholders): `deploy/argocd/application.yaml`, `deploy/argocd/values-prod.yaml`,
`deploy/argocd/externalsecret.yaml`, and [deploy/README.md](../deploy/README.md). For the **workloads** the engine resizes, `deploy/argocd/workload-application.yaml` (`ignoreDifferences` + `RespectIgnoreDifferences=true`), `deploy/flux/kustomization.yaml` and `deploy/flux/helmrelease.yaml` stop a sync from reverting the template patches of `apply: Rollout` and `InPlacePreferred` (see the GitOps section of the deploy README). Keep `ServerSideApply=true` (large CRDs) and pin `targetRevision` to a tag or SHA. Because `chart/charts/` is not committed,
make sure your tool runs `helm dependency build` (Argo CD does for a Helm source with dependencies; verify on your version). For Flux, a `HelmRelease` against a Git source needs the same.

## 10. Upgrade and rollback

- **Upgrade:** read [UPGRADE.md](UPGRADE.md) first (images, the MCP gate, the engine ClusterRole name, auth defaults). Then `helm dependency build chart && helm upgrade k8s-tailor ./chart -n k8s-tailor -f values-prod.yaml --wait`.
- **Rollback:** `helm history k8s-tailor -n k8s-tailor`, then `helm rollback k8s-tailor <revision> -n k8s-tailor`. CRDs are templates annotated `keep`: a rollback re-renders the older CRD schema, which can prune newer fields on future writes; review before rolling back across a
  CRD change.
- **Immediate stop without uninstalling:** `kubectl patch tailorconfig cluster --type=merge -p '{"spec":{"paused":true}}'` stops both the reconciler and the webhook. Resizes already applied stay; the pods keep the sizes they have.
  To return workloads to their templates, restart or roll them (`kubectl rollout restart`) because an in-place resize only changed the running pods.

## 11. Uninstall completely

`helm uninstall` leaves the CRDs (and every policy), the generated Secrets (`resource-policy: keep`) and the PVCs behind on purpose. For a full removal:

```bash
helm uninstall k8s-tailor -n k8s-tailor
kubectl delete mutatingwebhookconfiguration zz-k8s-tailor --ignore-not-found      # if you enabled the webhook (Helm removes it too; this is a safety net)
for n in workloads replicas pods; do kubectl delete validatingadmissionpolicybinding,validatingadmissionpolicy k8s-tailor-patch-scope-$n --ignore-not-found; done   # safety net if tailor.admissionPolicy was enabled (Helm removes them too)
kubectl delete tailorpolicies,tailorprofiles,tailorgroups,tailorconfigs --all      # your configuration
kubectl delete crd tailorconfigs.tailor.k8s-tailor.dev tailorgroups.tailor.k8s-tailor.dev tailorpolicies.tailor.k8s-tailor.dev tailorprofiles.tailor.k8s-tailor.dev
kubectl -n k8s-tailor delete pvc --all                                              # usage history and decision log are lost
kubectl -n k8s-tailor delete secret k8s-tailor-api-auth k8s-tailor-mcp-auth k8s-tailor-clickhouse-auth --ignore-not-found
kubectl delete clusterrole,clusterrolebinding k8s-tailor-engine k8s-tailor-vmsingle --ignore-not-found   # normally removed by uninstall
kubectl delete namespace k8s-tailor
```

Workload annotations `tailor.k8s-tailor.dev/*` (`mode`, `oom-floor`, `last-applied`, ...) stay on your objects; remove them if you want a clean slate:
`kubectl annotate deploy/<name> tailor.k8s-tailor.dev/mode- tailor.k8s-tailor.dev/oom-floor-`. The webhook-related deletions are no-ops when the feature was never enabled. The cluster-scoped deletions above are a safety net after `helm uninstall`; check `kubectl get` first and only delete objects that belong to this release.

## 12. Troubleshooting

Quick pointers (the full list is in [TROUBLESHOOTING.md](TROUBLESHOOTING.md)):

| Symptom | Likely cause |
|---|---|
| `helm install` fails with "no matching ... charts/" or missing dependency | Run `helm dependency build chart` first. |
| Render fails mentioning `lookup` or `certMode=helm` | You are rendering with `helm template`/GitOps: use `existingSecret`s and `certMode: certManager`. |
| `ImagePullBackOff` on the engine | The default GHCR images do not exist until the owner publishes a release ([section 4](#4-images-and-chart), [UPGRADE.md#images](UPGRADE.md#images)): publish them or build locally and `k3d image import` / `kind load`. |
| Everything "learning", nothing resized | By design for `MIN_SAMPLES` (288 = 24 h). |
| `Pending/Infeasible` resizes | The node cannot fit the request; see `tailor_pods_resize_state`. |
| UI loads but data is empty | Wrong token, or the NetworkPolicy does not list your ingress under `networkPolicy.mcpFrom`. |
