# Contributing

Thanks for looking. This is a small, pre-1.0 project; issues and focused PRs are welcome.
Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) first and [docs/KNOWN-ISSUES.md](docs/KNOWN-ISSUES.md)
before reporting a bug (it may be known).

## Prerequisites

Go (version in `go.mod`), [Bun](https://bun.sh) (CI pins 1.3.10), Helm, and for the local cluster
`docker`, `k3d`, `kubectl`.

## Dev loop

```bash
make ui-build    # dashboard bundle -> web/dist (go:embed requires it before Go builds/tests)
make test        # go test ./... (builds the UI first)
make vet
make lint        # gofmt + tsc + biome
make chart-lint  # helm lint + template
make chart-test  # chart-lint + chart/tests/render_test.py (helm, kubeconform, python3 + pyyaml)
mcp/server/e2e/nginx-gate.sh   # MCP auth gate: chart's nginx image + Bun (docker, helm, python3 + pyyaml)

make k3s-up      # local k3d cluster + both images + chart + demo workloads (dev/k3s)
make k3s-smoke   # end-to-end: an in-place resize without restart
make k3s-down
```

Optional real-cluster suites (slow, throw-away k3d clusters; details in [AGENTS.md](AGENTS.md) and each README):

```bash
dev/calico/netpol-test.sh                      # NetworkPolicy proof on Calico; needs a Docker kernel with xt_CT/xt_NFLOG/rpfilter
                                               # (a Lima docker-rootful VM works; the "Dory" runtime does not), python3, ~6 min
ISTIO_NATIVE=false dev/istio/mesh-test.sh all  # real Istio 1.31.3 (classic; ISTIO_NATIVE=true for native), needs curl, python3; istioctl is
                                               # downloaded if absent; 15-20 min
```

Run them when you change the chart's NetworkPolicies, the webhook, or sidecar handling. See [dev/calico/README.md](dev/calico/README.md) and [dev/istio/README.md](dev/istio/README.md).

Running the engine against a cluster without Docker: `SOURCE=local VM_URL=... go run ./cmd/k8s-tailor`.
For UI work: `cd mcp && MCP_API_TOKEN=dev-token bun run dev` (server; it refuses to start without a token, dev-only escape: `MCP_ALLOW_UNAUTHENTICATED=true`) and `bun run dev:web` (Vite on :5179).

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, `go test -race`, Helm lint/template with
a few value combinations, the MCP `tsc`, biome format, `bun test` and build, and the `mcp-gate` job (`mcp/server/e2e/nginx-gate.sh`). Run `go test -race ./...`
locally before pushing changes to `internal/`.

## Tests

- Engine logic is table-tested in `internal/*/..._test.go`; the reconciler has fake-client tests.
  A change to recommendation math, gates or actuation needs a test that fails without it.
- Do not weaken a safety rule (limit shrink, OOM floor, direction gate, PDB fallback) without
  updating [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) section 8 and explaining why in the PR.
- If you change `chart/` or a CRD, run `make chart-test` (which includes `make chart-lint`). A change to the MCP routing or the nginx sidecar also needs `mcp/server/e2e/nginx-gate.sh`. CRDs live in `chart/templates/crds/` (templated so upgrades apply schema changes).

## Conventions

- **Commits** follow Conventional Commits: `feat(scope): ...`, `fix: ...`, `docs: ...`, `chore: ...`,
  `test: ...`, `refactor: ...`. Write commit messages in English.
  Keep one logical change per commit, and explain the "why" in the body.
- **Go**: `gofmt`, no new dependencies without a reason, errors wrapped with context, comments
  explain why. The binary stays a single static executable.
- **TypeScript**: formatted by Biome (`bun run format` in `mcp/`).
- **Docs**: if behavior changes, update the doc that states it in the same PR. Docs are checked
  against code; do not describe features that are not implemented (mark them "planned").
- Do not commit secrets, `.env`, or cluster-specific values; examples use `*.example.com`.

## Pull requests

Describe what changed and how you verified it (test output, `make k3s-smoke`, or one of the suites above). Keep PRs small.
For anything touching actuation, state the failure mode you considered.

## Security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).
