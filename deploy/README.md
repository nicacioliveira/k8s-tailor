# Deploying K8s Tailor

K8s Tailor is a **platform component**: one Helm chart (`../chart`) installs the engine, the
MCP server + dashboard, VictoriaMetrics and a single-node ClickHouse. The files here are
GitOps examples; replace every `*.example.com` and org-specific value with your own.

## What's here

| File | Purpose |
|------|---------|
| `argocd/application.yaml` | ArgoCD `Application` pointing at this repo's `chart/`, with prod values inline. |
| `argocd/values-prod.yaml` | Standalone prod values overlay (mirror of the Application's inline block). |
| `argocd/externalsecret.yaml` | Example ExternalSecret mapping a secret-manager entry (`mcp-token`) into a k8s Secret. |
| `argocd/workload-application.yaml` | Example `Application` for **your** workloads: `ignoreDifferences` + `RespectIgnoreDifferences=true` so a sync does not revert what the engine writes to controller templates. |
| `flux/kustomization.yaml` | Flux equivalent for workloads applied by a `Kustomization` (`.spec.ignore`). |
| `flux/helmrelease.yaml` | Flux equivalent for workloads installed by a `HelmRelease` (`driftDetection.ignore`; Helm upgrades still write the chart's values). |
| `grafana/k8s-tailor.json` | Grafana dashboard for the engine's `/metrics` (replica-safe: recommendation gauges with `max by`, per-replica counters summed). |

## Wiring it up

1. Pin `targetRevision` to a tag or commit sha (never a moving branch in prod).
2. Add the `Application` (or an `ApplicationSet` entry) for the cluster you are enabling.
3. Provide credentials: create one secret `k8s-tailor` containing
   `{ "mcp-token": "<random>", "api-token": "<random>", "clickhouse-password": "<random>" }`
   in your secret manager, then apply `externalsecret.yaml` (adjust the `ClusterSecretStore`).
   ArgoCD renders with `helm template` (no `lookup`), so chart-generated secrets would rotate on
   every sync; the examples set `secretGeneration.failWithoutLookup: true` so a missing
   `existingSecret` fails the render instead. Upgrading an existing install: `../docs/UPGRADE.md`.
4. Sync. The chart installs everything in one release.

## GitOps for the workloads the engine resizes

In-place resizes (`apply: InPlace`, the default path) change running pods only; re-applying
the workload manifests does not undo them. With `apply: Rollout`, `InPlacePreferred` falling
back to a rollout, Knative Services or `proxyAnnotations`, the engine patches the controller
**template** (container `resources`, the mesh proxy resource annotations, the
`tailor.k8s-tailor.dev/last-applied` stamp). A GitOps sync then writes the Git values back,
which rolls the pods, and the engine resizes again after its cooldown: a loop of rollouts.
Tell the tool to leave those fields alone, in the Application/Kustomization **of the
workloads** (not of K8s Tailor itself):

- Argo CD: `argocd/workload-application.yaml` (`ignoreDifferences` with `jqPathExpressions`,
  plus `RespectIgnoreDifferences=true`, without which a sync still writes them).
- Flux: `flux/kustomization.yaml` (`.spec.ignore`; the controller adopts the live value) or
  `flux/helmrelease.yaml` (`driftDetection.ignore`; a Helm upgrade still writes the chart's
  values, so prefer `apply: InPlace` for Helm-managed workloads).

Flux paths are JSON Pointers without wildcards: list one `containers/<index>/resources` per
container. The trade-off in every case: the tool no longer reports or reverts manual edits of
those fields.

## Images (GHCR)

CI publishes both images to GHCR on push to `main` and on tags:

- `ghcr.io/nicacioliveira/k8s-tailor` (engine + embedded UI)
- `ghcr.io/nicacioliveira/k8s-tailor-mcp` (MCP server + dashboard)

New GHCR packages inherit the repo's visibility. To pull anonymously, make each package public
once (GitHub UI: Packages -> package -> Package settings -> Change visibility), or provide an
image pull secret.

Pin an immutable `:sha` or release tag in prod, not `:latest`.

## Safe by default

- Comes up **inert**: recommendations are computed (read-only); nothing is actuated until a
  `TailorPolicy` enables a group (mode `InPlace`/`Both`).
- The mutating **webhook is opt-in** (`tailor.webhook.enabled=false`).
- The release namespace and system/mesh namespaces are excluded; the tailor never touches
  control planes or itself.
- The engine write API requires a Bearer token by default (`tailor.apiAuth`).
- The bundled stores are **node-safe**: single-node, memory-capped, and off every request path.

## Reuse mode

To reuse existing stores instead of bundling: set `clickhouse.enabled=false` /
`victoria-metrics-single.enabled=false` and point `tailor.env.CH_URL` / `tailor.env.VM_URL`
(and CH credentials) at the existing services. Same tables, same engine.
