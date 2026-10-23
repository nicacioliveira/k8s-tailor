#!/usr/bin/env bash
# Removes the k3d cluster `tailor-dev` (and everything inside it).
set -euo pipefail
. "$(dirname "$0")/common.sh"
if k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
  k3d cluster delete "$CLUSTER"
else
  log "cluster $CLUSTER does not exist"
fi
