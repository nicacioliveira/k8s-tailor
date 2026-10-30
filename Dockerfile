# Stage 1: build the SAME MCP dashboard (single self-contained HTML) that the MCP
# server serves — the engine embeds it too, so :8080 and the MCP UI are identical.
# Pinned to the BUILD platform: the output is a static JS bundle (arch-agnostic),
# so there's no need to emulate this stage per target arch.
FROM --platform=$BUILDPLATFORM oven/bun:1.4.4 AS web
WORKDIR /app
COPY mcp/package.json mcp/bun.lock ./
RUN bun install --frozen-lockfile
COPY mcp ./
RUN bun run build:web \
 && mkdir -p /out/dist \
 && cp dist/client/web/entries/dashboard-ui.html /out/dist/index.html

# Stage 2: build the Go binary with the embedded UI. Runs natively on the build
# platform and CROSS-COMPILES to the target arch (CGO off → trivial), so multi-arch
# builds don't pay the QEMU tax.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# bring in the freshly built dashboard so go:embed picks it up
COPY --from=web /out/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/k8s-tailor ./cmd/k8s-tailor

# Stage 3: minimal runtime.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/k8s-tailor /k8s-tailor
EXPOSE 8080 8443
ENTRYPOINT ["/k8s-tailor"]
