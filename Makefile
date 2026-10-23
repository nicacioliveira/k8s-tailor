.PHONY: build test vet lint ui-build docker chart-lint chart-test

IMAGE      ?= k8s-tailor
MCP_IMAGE  ?= k8s-tailor-mcp
TAG        ?= dev

# The engine embeds the dashboard (web/dist), so build the UI first.
build: ui-build
	CGO_ENABLED=0 go build -o bin/k8s-tailor ./cmd/k8s-tailor

test: ui-build
	go test ./...

vet: ui-build
	go vet ./...

# Biome is the one pinned by mcp/bun.lock (1.9.4, matching biome.json's schema;
# it finds the root biome.json walking up from mcp/). A bare `bunx @biomejs/biome`
# fetches the latest (v2), which rejects the v1 config.
lint:
	@test -z "$$(gofmt -l cmd internal web)" || (echo "gofmt needed:"; gofmt -l cmd internal web; exit 1)
	cd mcp && bun install --frozen-lockfile && bun run check
	cd mcp && bun x --no-install biome format server web

ui-build:
	bun run build:web

docker:
	docker build -t $(IMAGE):$(TAG) .
	docker build -t $(MCP_IMAGE):$(TAG) ./mcp

chart-lint:
	helm dep build chart
	helm lint --strict chart
	helm template k8s-tailor chart >/dev/null

# chart/charts/ is gitignored: render_test runs `helm dependency build` by itself
# (or fails with a clear message). Needs helm, kubeconform and python3+pyyaml.
chart-test: chart-lint
	python3 chart/tests/render_test.py

# --- local test infra (k3d) — see dev/k3s/README.md ---
.PHONY: k3s-up k3s-down k3s-smoke
k3s-up:
	dev/k3s/up.sh
k3s-down:
	dev/k3s/down.sh
k3s-smoke:
	dev/k3s/smoke.sh
