# Upgrading the chart

This is the first release line (chart `0.2.0`, app `0.1.0`), so there are no version-to-version migration steps yet. Release-specific notes will be added here under a heading per version. Until then this page covers how upgrades work, which names and defaults to know, and how to get the images.

Contents: [Procedure](#procedure) · [CRDs](#crds) · [Names in the cluster](#names-in-the-cluster) · [Defaults to know](#defaults-to-know) · [Images](#images) · [GitOps](#gitops) · [Clean clone builds](#clean-clone-builds)

## Procedure

```bash
helm dependency build chart        # chart/charts/ is git-ignored
helm upgrade k8s-tailor ./chart -n k8s-tailor -f values-prod.yaml --wait
helm history k8s-tailor -n k8s-tailor           # rollback: helm rollback k8s-tailor <revision> -n k8s-tailor
```

Read the diff of `chart/values.yaml` between your old and new checkout first, and `helm template` with your values before upgrading. The engine, MCP and dashboard are stateless; VictoriaMetrics and ClickHouse keep their PVCs. Generated Secrets are reused through `lookup` (and kept on uninstall); under `helm template` (Argo CD, Flux) use `existingSecret`, see [GitOps](#gitops).

## CRDs

The four CRDs (`TailorPolicy`, `TailorProfile`, `TailorGroup`, `TailorConfig`) are regular chart templates, so they are upgraded with the release and annotated `helm.sh/resource-policy: keep`: an uninstall does not delete them, nor your policies with them. Rolling back re-renders the older CRD schema, which can prune newer fields on future writes; review before rolling back across a CRD change.

- `crds.install=false`: CRDs managed out of band (a CRD-only Argo CD app, etc.).
- `crds.keep=false`: drop the keep annotation (an uninstall then deletes every Tailor CR).
- Argo CD: keep `ServerSideApply=true` (the CRDs are large).
- Numeric fields are validated by the schema (`limitFactor` 0 or >= 1, `quantile` in (0, 1], `margin` in [0, 10], `headroomThreshold` in (0, 0.5], `priority` in [-10000, 10000], quantity/duration/HH:MM/date patterns). An existing CR that violates a rule keeps working (validation ratcheting, default-on since Kubernetes 1.30) but must be fixed on its next edit.
- `TailorConfig/cluster` is seeded from `tailor.excludeNamespaces` on first install only; afterwards the CR is the source of truth, so new default namespaces must be added to it by hand:

```bash
kubectl patch tailorconfig cluster --type=json -p '[{"op":"add","path":"/spec/excludeNamespaces/-","value":"istio-system"}]'
```

## Names in the cluster

Object names are fixed (`k8s-tailor`, `k8s-tailor-mcp`, `k8s-tailor-clickhouse`, `k8s-tailor-clickhouse-auth`, `k8s-tailor-mcp-auth`, `k8s-tailor-mcp-nginx`) and namespaced objects follow the release namespace (`helm upgrade -n <ns>`; the old `namespace:` value is gone and fails the render if it differs). Cluster-scoped objects have fixed names too, so there is one release per cluster:

| Object | Name | Notes |
|---|---|---|
| Engine ClusterRole and ClusterRoleBinding | `k8s-tailor-engine` | Reads **and writes** (`pods/resize`, workload patches, CRs, Events). Reference this name in your own policies (OPA, Kyverno, audit filters). |
| vmsingle ClusterRole and ClusterRoleBinding | `k8s-tailor-vmsingle` | `nodes` get/list/watch and `nodes/proxy` get, for the kubelet scrape. |
| vmsingle Role (release namespace) | `k8s-tailor-vmsingle-pods` | `pods` get/list/watch, for the per-pod scrape of the engine. |

## Defaults to know

- **Write API authentication is on** (`tailor.apiAuth.enabled=true`): every mutating `/api/*` call needs `Authorization: Bearer <token>` and the engine fails closed without a token. The token is `tailor.apiAuth.existingSecret`, the inline `token`, or generated once into Secret `k8s-tailor-api-auth`. Writes must be `application/json` (415 otherwise), must not come from a cross-site browser origin (403) and are capped at 1 MiB (413). `tailor.apiAuth.protectReads=true` (off by default) also requires the token on `GET/HEAD /api/*`; `/healthz`, `/livez`, `/metrics` and `/` stay open.
- **The MCP is gated twice.** Bun always requires `MCP_API_TOKEN` (the chart injects it) and refuses to start without it; the nginx sidecar (`mcp.auth.enabled=true`) is deny-by-default: only `GET /`, `/index.html`, `/healthz`, `/readyz` are open. Tokens may only contain `A-Z a-z 0-9 . _ ~ + / = -` (for example `openssl rand -hex 32`). A link may carry the token only in the URL fragment (`#token=...`); `?token=` is ignored. With an Ingress set `mcp.allowedHosts`, and list the ingress controller's namespace under `networkPolicy.mcpFrom`. For a throwaway dev cluster only: `mcp.auth.enabled=false` plus `mcp.auth.allowUnauthenticated=true`.
- **NetworkPolicies** (`networkPolicy.enabled=true`) restrict the engine `:8080`, vmsingle `:8428`, ClickHouse and the MCP; a Grafana or other reader of the bundled VictoriaMetrics must be listed in `networkPolicy.metricsFrom`. `networkPolicy.webhookFrom` optionally restricts `:8443` to the kube-apiserver's addresses.
- **`MIN_SAMPLES` is `288`** (24 h of 300 s steps, one full daily cycle): new workloads stay in "learning" for a day. Lower it only for demos.
- **`tailor.admissionPolicy`** (opt-in, Kubernetes 1.30 or later) lets the engine's ServiceAccount change only containers'/initContainers' `resources`, `tailor.k8s-tailor.dev/*` annotations and the exact mesh proxy-sizing annotations. Roll it out with `validationActions: [Warn, Audit]` first.
- **`tailor.webhook.certMode=helm` fails the render** when no API server is reachable (`helm template`, Argo CD, client `--dry-run`). Use `certManager` or `existingSecret` with GitOps (`allowEphemeralCA=true` for CI only).
- **`tailor.k8s-tailor.dev/ignore: "true"` is absolute**: it resolves to `Off` even when a mode annotation or container rule says otherwise.
- **Policy edits from the UI/MCP merge** into the live spec: only the fields sent change; a field sent empty is cleared.
- **Scrape layout of the bundled vmsingle**: job `kubelet-resource` (`/metrics/resource`), job `kubelet-cadvisor` (only the two CFS counters, for the throttling signal, `VM_THROTTLE_JOB`) and job `tailor` (the engine pods through pod service discovery, so with `tailor.replicas > 1` counters and `tailor_leader` are one series per replica). The gauges derived from recommendations keep a single `instance` label. Aggregate per-replica metrics in your own queries (`sum`, `max by (pod)`); `deploy/grafana/k8s-tailor.json` does.
- **Volume ownership.** The pods run as non-root with `fsGroup`. A PVC written as root on a provisioner that ignores `fsGroup` (hostPath-type PVs) keeps root-owned files and vmsingle (uid 65534) cannot write: install with the opt-in overlay `-f chart/values-vmsingle-chown.yaml` (an init container that runs `chown -R 65534:65534 /storage` as root with only `CAP_CHOWN`; the vmsingle container itself stays non-root; rendered and checked by the chart tests, not exercised on a provisioner that ignores `fsGroup`), run a one-time `chown` on the volume by hand, or set `victoria-metrics-single.server.{podSecurityContext,securityContext}` to `{enabled: true}` on that installation.

## Images

The chart's default images (`ghcr.io/nicacioliveira/k8s-tailor{,-mcp}:v<appVersion>`) are
published by `.github/workflows/build-images.yml` and **do not exist until the owner publishes a
release**; a pull before that answers `denied`/`not found`. Two ways to get running images:

**A. Publish a release (owner).** This is an action only the repository owner should take: it
creates public-facing artifacts (tag, GHCR packages, an OCI chart). The workflow builds both images (amd64 + arm64) with
`GITHUB_TOKEN` (`packages: write`) and pushes the chart to `oci://ghcr.io/nicacioliveira/k8s-tailor/charts`.
The release tag must equal `v<appVersion>` from `chart/Chart.yaml` (the workflow refuses
anything else):

```bash
# the workflow file must be on the default branch for the manual trigger to exist
git tag v0.1.0 && git push origin v0.1.0
# or, without a tag:
gh workflow run build-images.yml -f version=v0.1.0
```

The repository is private, so the packages are created **private**: either make each one public
(GitHub -> profile -> Packages -> `k8s-tailor` / `k8s-tailor-mcp` -> Package settings -> Change
visibility -> Public), or give the cluster a pull secret:

```bash
kubectl -n k8s-tailor create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io --docker-username=<github-user> --docker-password=<PAT with read:packages>
helm upgrade --install k8s-tailor ./chart -n k8s-tailor --set 'global.imagePullSecrets[0].name=ghcr-pull'
```

`tailor.image` / `mcp.image` are maps (`repository`, `tag`, `digest`); `global.imagePullSecrets` adds pull secrets to every pod, including the VictoriaMetrics subchart.

**B. Build locally (dev / air-gapped).** Build both images and load them into the cluster's
container runtime, then point the chart at them:

```bash
helm dependency build chart                            # clean clone: chart/charts/ is git-ignored
make docker TAG=dev                                    # k8s-tailor:dev + k8s-tailor-mcp:dev
k3d image import k8s-tailor:dev k8s-tailor-mcp:dev -c <cluster>     # k3d
# kind load docker-image k8s-tailor:dev k8s-tailor-mcp:dev --name <cluster>   # kind
helm upgrade --install k8s-tailor ./chart -n k8s-tailor --create-namespace \
  --set tailor.image.repository=k8s-tailor --set tailor.image.tag=dev \
  --set mcp.image.repository=k8s-tailor-mcp --set mcp.image.tag=dev
```

For a private registry: `docker tag`/`docker push` to it, set `*.image.repository`/`tag` (or
`digest`) and `global.imagePullSecrets`.

## GitOps

Argo CD and Flux render with `helm template`: no `lookup`, so chart-generated secrets (engine API token,
MCP token, ClickHouse password) would be re-minted on every render and the running pods would
disagree with the Secret (ClickHouse keeps the password it bootstrapped on first boot). Set an
`existingSecret` for `tailor.apiAuth`, `mcp.auth` and `clickhouse.auth`, and
`secretGeneration.failWithoutLookup: true` so a forgotten one fails the render. Use
`tailor.webhook.certMode=certManager` for the webhook. See `deploy/argocd/` and `deploy/flux/`.

For the **workloads** the engine resizes, add the ignore rules of [deploy/README.md](../deploy/README.md) (`deploy/argocd/workload-application.yaml`, `deploy/flux/`) so a sync does not revert the template patches written under `apply: Rollout` or `InPlacePreferred`.

## Clean clone builds

`web/dist/.gitkeep` is versioned, so `go build ./...` works without building the UI first. Such a
binary answers `503 UI not built` on `/`; `make build` / the Dockerfile embed the real UI.
