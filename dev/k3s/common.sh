# Shared config for the dev/k3s scripts (source it, do not execute it).
CLUSTER="${CLUSTER:-tailor-dev}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.34.12-k3s1}"  # >=1.33: InPlacePodVerticalScaling beta, default-on
NS="${NS:-k8s-tailor}"
RELEASE="${RELEASE:-k8s-tailor}"
IMAGE="${IMAGE:-k8s-tailor}"
MCP_IMAGE="${MCP_IMAGE:-k8s-tailor-mcp}"
TAG="${TAG:-dev}"
CTX="k3d-${CLUSTER}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
k() { kubectl --context "$CTX" "$@"; }
log() { printf '\033[1;34m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*"; }
