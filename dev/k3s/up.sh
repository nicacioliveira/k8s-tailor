#!/usr/bin/env bash
# Brings up the k3d cluster `tailor-dev` and installs k8s-tailor + the demo.
# Variables: SRC_REF=<git ref> (build from a clean ref), SKIP_BUILD=1 (reuse local images), SKIP_DEMO=1, DEMO_FULL=1 (scenario.yaml + cpu-wave.yaml), K3S_IMAGE, CLUSTER.
set -euo pipefail
. "$(dirname "$0")/common.sh"
START=$(date +%s)

for b in docker k3d kubectl helm; do command -v "$b" >/dev/null || { echo "missing $b in PATH" >&2; exit 1; }; done
docker info >/dev/null 2>&1 || { echo "docker daemon unreachable (docker context: $(docker context show))" >&2; exit 1; }

if k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
  log "cluster $CLUSTER already exists (use down.sh to recreate it)"
  k3d kubeconfig merge "$CLUSTER" --kubeconfig-merge-default >/dev/null
else
  log "creating cluster $CLUSTER ($K3S_IMAGE)"
  # K8s >=1.33 already enables InPlacePodVerticalScaling by default; the explicit
  # feature gates below are idempotent and protect the case where K3S_IMAGE is <1.33 (>=1.27).
  k3d cluster create "$CLUSTER" \
    --image "$K3S_IMAGE" \
    --servers 1 --agents 0 \
    --no-lb \
    --k3s-arg "--disable=traefik@server:0" \
    --k3s-arg "--disable=servicelb@server:0" \
    --k3s-arg "--disable=metrics-server@server:0" \
    --k3s-arg "--kube-apiserver-arg=feature-gates=InPlacePodVerticalScaling=true@server:0" \
    --k3s-arg "--kubelet-arg=feature-gates=InPlacePodVerticalScaling=true@server:0" \
    --wait --timeout 180s
fi
k3d kubeconfig merge "$CLUSTER" --kubeconfig-switch-context=false --kubeconfig-merge-default >/dev/null
k wait --for=condition=Ready node --all --timeout=120s

if [ "${SKIP_BUILD:-0}" != 1 ]; then
  log "building images ($IMAGE:$TAG, $MCP_IMAGE:$TAG)"
  BSRC="$ROOT"
  if [ -n "${SRC_REF:-}" ]; then
    # Build from a clean commit/ref (e.g. SRC_REF=HEAD) instead of the working tree.
    BSRC="$(mktemp -d "${TMPDIR:-/tmp}/tailor-src.XXXXXX")"
    git -C "$ROOT" archive "$SRC_REF" | tar -x -C "$BSRC"
    log "source: git archive $SRC_REF -> $BSRC"
  fi
  if docker buildx version >/dev/null 2>&1; then
    docker build -t "$IMAGE:$TAG" "$BSRC"
  else
    # Without buildx/BuildKit the legacy builder does not define BUILDPLATFORM/TARGETARCH and
    # the Dockerfile (FROM --platform=$BUILDPLATFORM) fails. Fallback: a temporary Dockerfile
    # without --platform, with the daemon arch passed as a build-arg.
    log "buildx missing: using the legacy builder with a temporary Dockerfile"
    arch="$(docker version --format '{{.Server.Arch}}')"
    tmpdf="$(mktemp "${TMPDIR:-/tmp}/Dockerfile.k3s.XXXXXX")"
    sed 's/--platform=\$BUILDPLATFORM //' "$BSRC/Dockerfile" > "$tmpdf"
    docker build -f "$tmpdf" --build-arg TARGETOS=linux --build-arg "TARGETARCH=$arch" \
      -t "$IMAGE:$TAG" "$BSRC"
    rm -f "$tmpdf"
  fi
  docker build -t "$MCP_IMAGE:$TAG" "$BSRC/mcp"
  [ "$BSRC" != "$ROOT" ] && rm -rf "$BSRC" || true
fi
log "importing images into the cluster"
k3d image import "$IMAGE:$TAG" "$MCP_IMAGE:$TAG" -c "$CLUSTER"

log "helm install"
helm dep build "$ROOT/chart" >/dev/null
helm upgrade --install "$RELEASE" "$ROOT/chart" \
  --kube-context "$CTX" --namespace "$NS" --create-namespace \
  -f "$HERE/values-dev.yaml" \
  --set tailor.image.repository="$IMAGE" --set tailor.image.tag="$TAG" \
  --set mcp.image.repository="$MCP_IMAGE" --set mcp.image.tag="$TAG" \
  --wait --timeout 8m

if [ "${SKIP_DEMO:-0}" != 1 ]; then
  log "applying demo/"
  k apply -f "$ROOT/demo/workloads.yaml" -f "$ROOT/demo/cpu-burner.yaml"
  [ "${DEMO_FULL:-0}" = 1 ] && k apply -f "$ROOT/demo/scenario.yaml" -f "$ROOT/demo/cpu-wave.yaml" || true
fi

log "ready in $(( $(date +%s) - START ))s. Next: dev/k3s/smoke.sh  (kubectl config use-context $CTX)"
