# AGENTS.md

Instructions for AI coding agents (and humans in a hurry) working **on this repository**. To *install* K8s Tailor on a cluster, use [docs/LLM-INSTALL.md](docs/LLM-INSTALL.md) instead.

K8s Tailor is a Kubernetes pod rightsizer built around in-place resize (`pods/resize`). One Go binary (engine, API, webhook, backtest CLI), a Bun/React MCP server and dashboard, one Helm chart. Pre-1.0; a wrong change here can resize
live pods, so safety logic is treated as the most sensitive code.

## Repository map

| Path | What it is |
|---|---|
| `cmd/k8s-tailor/` | `main.go` (wiring: refresh loop, reconciler, leader election, webhook, API) and `backtest.go` (the `backtest` subcommand) |
| `internal/recommender/` | Recommendation math (`Recommend` is pure), change-point, seasonality gate, stabilizer, OOM floor, container merging |
| `internal/reconciler/` | In-place and rollout actuation; `decide.go` (`PlanInPlace`, `Decide`) is shared with the webhook and the backtest |
| `internal/webhook/` | Mutating admission webhook and certificate hot reload |
| `internal/policy/` | CRD store, mode/policy resolution (`Resolve`, `ResolveContainer`), validation (bounds mirror the CRD schema) |
| `internal/k8s/` | client-go reads, `pods/resize`, template and Knative patches, container classification, HPA/PDB/ownership snapshot |
| `internal/server/` | HTTP API, `/metrics`, embedded UI, write-auth and CSRF guards, advisor routes |
| `internal/advisor/` | Optional read-only LLM advisor (stdlib only; must not import actuation code, a test enforces it) |
| `internal/backtest/` | Offline replay engine with KRR-style, VPA-style and static reference baselines |
| `internal/{actuation,ch,vm,metrics,leader,pricing,config}/` | Decision store, ClickHouse sink, MetricsQL client, operational metrics, Lease election, pricing, env config |
| `web/` | `go:embed` of the dashboard bundle (`web/dist/index.html` is generated and ignored; only `.gitkeep` is versioned) |
| `mcp/` | Bun MCP server (`server/`) and React dashboard (`web/`: `views/`, `components/`, `lib/`, `dev-fixtures/`) |
| `chart/` | Helm chart: `templates/` (CRDs are in `templates/crds/`, not `crds/`), `values.yaml`, `tests/render_test.py` |
| `deploy/` | Argo CD and ExternalSecret examples, Grafana dashboard |
| `dev/k3s/`, `demo/` | Local k3d environment, smoke tests, demo workloads |
| `dev/calico/`, `dev/istio/` | Throw-away k3d suites: NetworkPolicy proof on Calico, real Istio 1.31.3 mesh test (each with its README) |
| `docs/` | Documentation (see the index in [README.md](README.md#documentation-index)) |

Read first: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/TECHNICAL.md](docs/TECHNICAL.md), [docs/KNOWN-ISSUES.md](docs/KNOWN-ISSUES.md).

## Commands

Toolchain: Go (version in `go.mod`, currently 1.26.1), Bun (CI pins 1.3.10), Helm 3; `docker`, `k3d`, `kubectl` for the local cluster.

```bash
make ui-build      # bun run build:web -> web/dist/index.html (go:embed needs it; `go build` works without it but serves 503 on /)
make test          # go test ./...   (builds the UI first)
go test -race -count=1 -timeout 40m ./...   # what CI runs (the backtest package alone takes ~10 min); run it before pushing changes under internal/
make vet           # go vet ./...
make lint          # gofmt check + mcp tsc + biome format (uses the Biome pinned in mcp/node_modules; run `bun install` in mcp/ first)
make chart-lint    # helm dep build chart; helm lint --strict chart; helm template
make chart-test    # chart-lint + python3 chart/tests/render_test.py (needs helm, kubeconform, python3 + pyyaml)
make build         # CGO_ENABLED=0 go build -o bin/k8s-tailor ./cmd/k8s-tailor
make docker        # builds k8s-tailor:dev and k8s-tailor-mcp:dev
cd mcp && bun install --frozen-lockfile && bun run check    # tsc
cd mcp && bun test server web                                # MCP/dashboard unit tests
cd mcp && bun run dev:web                                    # Vite on :5179; open /web/entries/dashboard-ui.html?fixtures
cd mcp && MCP_API_TOKEN=dev-token bun run dev               # MCP server with hot reload; it refuses to start without a token (dev only: MCP_ALLOW_UNAUTHENTICATED=true)
mcp/server/e2e/nginx-gate.sh                                 # MCP auth-gate regression (docker, helm, python3+pyyaml, bun install + build:web); CI job mcp-gate
```

`chart/charts/` is git-ignored: run `helm dependency build chart` on a clean clone before `helm lint/template/install`.

Run the engine locally against a cluster without Docker: `SOURCE=local VM_URL=... go run ./cmd/k8s-tailor`.

### Local cluster and smoke tests

```bash
make k3s-up        # k3d cluster tailor-dev (rancher/k3s:v1.34.12-k3s1), builds and imports both images, helm install with dev/k3s/values-dev.yaml, applies demo/
make k3s-smoke     # PASS/FAIL asserts: pods Ready, CRDs, /healthz, VM series, and a real in-place resize (same UID, restartCount unchanged)
dev/k3s/smoke-sidecar.sh    # sidecar behavior without installing Istio
make k3s-down
```

The smoke test needs ~10 minutes because the engine samples on a fixed 300 s step and dev uses `MIN_SAMPLES=2`. Details: [dev/k3s/README.md](dev/k3s/README.md). `dev/k3s` is the base environment; two more single-node suites exist (below). The e2e results are in [docs/E2E.md](docs/E2E.md).

### NetworkPolicy and Istio suites (slow, optional)

```bash
dev/calico/netpol-test.sh        # up + test + down; KEEP=1 keeps the cluster; also: up | test | down | mutate (slow, unverified)
ISTIO_NATIVE=false dev/istio/mesh-test.sh all   # classic sidecar; ISTIO_NATIVE=true for native; also: up | test | down | only <scenario...>
```

- `dev/calico/netpol-test.sh` creates cluster `tailor-netpol` (never `tailor-dev`) with flannel and k3s's network-policy controller off and Calico v3.33.0 on, installs the chart with `dev/k3s/values-dev.yaml` + `dev/calico/values-netpol.yaml`, runs an enforcement canary (exit 3 if the CNI does not enforce) and 8 scenarios, ends with a negative control.
  Needs `docker`, `k3d`, `kubectl`, `helm`, `python3`, internet access, images `k8s-tailor:dev` / `k8s-tailor-mcp:dev` in the same Docker daemon, and **a Docker whose kernel has `xt_CT`, `xt_NFLOG` and `rpfilter`** (verified: a Lima `template://docker-rootful` VM, Ubuntu 24.04, kernel 6.8; the "Dory" runtime cannot run Calico, Cilium or kube-router). About 6 min warm.
- `dev/istio/mesh-test.sh` creates cluster `tailor-istio`, installs Istio 1.31.3 (profile `minimal`) with `istioctl` (version from `ISTIO_VERSION`; downloaded to a temp dir when the one on PATH differs), the chart with `dev/k3s/values-dev.yaml` + `dev/istio/values-istio.yaml`, and the demo workloads. Needs `docker`, `k3d`, `kubectl`, `helm`, `curl`, `python3`
  and the local images (`SKIP_BUILD=1` reuses them). Takes 15-20 min because the engine needs history before it acts (`TIMEOUT` seconds per wait, default 1500). `ORDER_TEST=1` adds the webhook-order experiment, which restarts the engine.
- Re-running `kubectl apply` on a workload the engine already resized reverts the template and rolls the Deployment; the Istio scenario manifests are meant to be applied once.

Details and results: [dev/calico/README.md](dev/calico/README.md), [dev/istio/README.md](dev/istio/README.md).

### Backtest

```bash
go run ./cmd/k8s-tailor backtest -synthetic all                       # 9 scenarios, deterministic (fixed seed)
go run ./cmd/k8s-tailor backtest -synthetic bursty -seed 7 -format json
```

See [docs/BACKTEST.md](docs/BACKTEST.md). It replays the real `Engine.Refresh` and `reconciler.Decide` in simulated time; changes to recommendation math or gates should be checked against it.

## Conventions

- **Commits:** Conventional Commits (`feat(scope):`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`). Write commit messages in English. One logical change per commit, explain the why. Never commit secrets, `.env`, kubeconfigs or cluster-specific values.
  Stage explicit paths (`git add <paths>`), not `git add -A`, because several people or agents may be editing the tree.
- **Go:** `gofmt`; wrap errors with context; comments explain why; no new dependencies without a reason (client-go is the only large one; the metrics registry is hand-rolled on purpose); the binary stays one static executable.
- **TypeScript:** formatted by Biome (`bun run format` in `mcp/`); the dashboard must work in an MCP host iframe and standalone.
- **Tests:** logic is table-tested next to the code. A change to recommendation math, gates or actuation needs a test that fails without it. The reconciler has fake-client tests (`reconciler_*_test.go`).
- **Language:** Everything (docs, code comments, log and error messages, script output, tests) is in English.
- **Docs follow code:** if behavior changes, update the doc that states it in the same change. Do not document what is not implemented; mark it "planned". Every doc claim should be checkable in the code, chart or an upstream source.

## What not to touch (or touch only with care)

- **Safety rules** (limit never shrunk or invented unless `memory.enforceLimit`, OOM floor, direction gate, HPA hold, PDB/QoS guards, confidence gate, excluded namespaces, `ignore` annotation absolute). Do not weaken one without a test and an explanation in the change; update
  [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and [docs/TECHNICAL.md](docs/TECHNICAL.md).
- **`reconciler.PlanInPlace` / `Plan.Limits`:** shared by the reconciler, the webhook and the backtest. Change them once, test all three.
- **CRDs:** edit `chart/templates/crds/*.yaml` (templates, so upgrades apply schema changes) and keep `internal/policy/validate.go` bounds in sync. Never add a `chart/crds/` directory.
- **Chart defaults:** `MIN_SAMPLES` is `"288"` in the chart (24 h) while the engine default is 30; the engine ClusterRole is `k8s-tailor-engine` (it reads and writes); object names are fixed; one release per cluster.
- **Generated or ignored:** `web/dist/index.html`, `chart/charts/`, `mcp/dist/`, `node_modules/`, `.context/`.
- **Design system:** tokens in `mcp/web/ds/`, components in `mcp/web/ui/`; keep the `NOTICE` file (it carries the Inter font attribution).
- **Docs of other owners:** if you were assigned a specific area (for example only docs, or only the chart), stay inside it and link to files you do not own.

## Areas and owners

This repository is developed in parallel by people and agents who each own an area. Typical split: engine (`internal/recommender`, `internal/reconciler`, `internal/webhook`, `internal/policy`, `internal/k8s`), chart and security (`chart/`, `SECURITY.md`,
`docs/UPGRADE.md`), UI and MCP (`mcp/`), end-to-end and release docs (`docs/E2E.md`, `docs/KNOWN-ISSUES.md`), and product/user documentation (`README.md`, `docs/INSTALL.md`, `docs/CONFIGURATION.md`, `docs/ARCHITECTURE.md`, `docs/TECHNICAL.md`, `docs/SCREENS.md`,
`docs/TROUBLESHOOTING.md`, `docs/LLM-INSTALL.md`, `llms*.txt`, `AGENTS.md`). Do not rewrite another area's files; open a note or link instead. Do not rename or switch the working branch, and do not push, unless explicitly asked.

## Gotchas

- The engine samples on a fixed `stepSec = 300`; `MIN_SAMPLES=N` therefore means N x 5 minutes of history.
- The recommender is pure (`Recommend`) but the engine holds in-memory state (stabilization history, OOM floors, cooldowns, failure backoff, rolling state). After a restart it is rebuilt from annotations (`last-applied`, `oom-floor`, `group-oom-floor`, pod `resize-failed` and `rolling`) and the stabilizer is re-seeded by replaying `Recommend`; a bare Pod keeps nothing across recreation.
- Annotations the engine writes (`tailor.k8s-tailor.dev/last-applied`, `/oom-floor`, `/group-oom-floor`, `/resize-failed`, `/rolling`) are part of the state model; do not rename them casually.
- The webhook configuration is named `zz-k8s-tailor` and `failurePolicy` is `Ignore`. The name does not decide correctness: `reinvocationPolicy: IfNeeded` does (with real Istio the API server calls the webhook twice per pod whatever the name).
- Usage pinned at a CPU limit is censored: do not "fix" a recommendation sitting at the limit by capping harder; see `Signals.CPUCensored` and docs/TECHNICAL.md. `Plan.CPULimitRaise` is the only way an existing limit grows (plus the OOM memory-limit raise).
- `ignore` must stay absolute on every path, including `SetWorkload`/`SetContainer` (UI and `POST /api/workload` writes) and the webhook.
- `helm template` has no `lookup`: anything that relies on it (generated secrets, `certMode=helm`) fails or changes on every render under GitOps by design.
- Do not run `kubectl`/`helm` against a cluster you were not told to use; always pass `--context` explicitly.
