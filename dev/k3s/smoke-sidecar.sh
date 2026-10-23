#!/usr/bin/env bash
# Sidecar smoke on k3d, WITHOUT installing Istio: a Deployment with 2 containers where
# the 2nd imitates the Istio proxy (name istio-proxy + the sidecar.istio.io/status
# annotation on the pod). Validates that:
#   1. the app is resized in place (policy mode=InPlace);
#   2. istio-proxy is classified as injected-sidecar/mesh istio and stays in
#      Recommend (safe default): its requests do NOT change;
#   3. with the annotation tailor.k8s-tailor.dev/container.istio-proxy.mode=InPlace on the
#      Deployment, the proxy is resized in place, without a restart.
# Variables: INPLACE_TIMEOUT (s, default 900), KEEP_SMOKE=1 (skip cleanup).
set -uo pipefail
. "$(dirname "$0")/common.sh"
INPLACE_TIMEOUT="${INPLACE_TIMEOUT:-900}"
FAILS=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS+1)); }
poll() { local t=$1; shift; local end=$(( $(date +%s) + t )); while [ "$(date +%s)" -lt "$end" ]; do "$@" >/dev/null 2>&1 && return 0; sleep 5; done; return 1; }
svc() { k get --raw "/api/v1/namespaces/$NS/services/$1/proxy/$2" 2>/dev/null; }

if [ "${KEEP_DEMO:-0}" != 1 ] && k get ns tailor-demo >/dev/null 2>&1; then
  echo "  (scaling tailor-demo to 0 to free CPU; KEEP_DEMO=1 disables this)"
  k -n tailor-demo scale deploy --all --replicas=0 >/dev/null
fi
SN="tailor-sidecar-$(date +%s)"
PN="smoke-sidecar-$SN"
for old in $(k get ns -o name 2>/dev/null | grep '/tailor-sidecar-'); do k delete "$old" --wait=false >/dev/null 2>&1; done
cleanup() { [ "${KEEP_SMOKE:-0}" = 1 ] || { k delete tailorpolicy "$PN" --ignore-not-found >/dev/null 2>&1; k delete ns "$SN" --ignore-not-found --wait=false >/dev/null 2>&1; }; }
trap cleanup EXIT

echo "== Sidecar: app + istio-proxy (imitation, no Istio) in $SN"
k apply -f <(sed -e "s/__SN__/$SN/g" -e "s/__PN__/$PN/g" <<'YAML'
apiVersion: v1
kind: Namespace
metadata: { name: __SN__ }
---
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorPolicy
metadata: { name: __PN__, labels: { app.kubernetes.io/managed-by: tailor-smoke-sidecar } }
spec:
  priority: 200
  mode: InPlace
  selector: { namespaces: ["__SN__"] }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: meshy, namespace: __SN__ }
spec:
  replicas: 1
  selector: { matchLabels: { app: meshy } }
  template:
    metadata:
      labels: { app: meshy }
      # The marker the Istio injector writes on the pod: with it Tailor treats
      # istio-proxy as the mesh proxy (mesh=istio).
      annotations: { sidecar.istio.io/status: '{"containers":["istio-proxy"]}' }
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "while true; do timeout -s KILL 1 yes > /dev/null; sleep 8; done"]
          resources:
            requests: { cpu: "500m", memory: 64Mi }
        - name: istio-proxy
          image: busybox:1.36
          command: ["sh", "-c", "while true; do sleep 3600; done"]
          resources:
            requests: { cpu: "300m", memory: 64Mi }
YAML
) >/dev/null
k -n "$SN" rollout status deploy/meshy --timeout=240s >/dev/null 2>&1 || { fail "meshy did not become ready"; k -n "$SN" get pod; }
POD=$(k -n "$SN" get pod -l app=meshy -o jsonpath='{.items[0].metadata.name}')
UID0=$(k -n "$SN" get pod "$POD" -o jsonpath='{.metadata.uid}')
req() { k -n "$SN" get pod "$POD" -o jsonpath="{.spec.containers[?(@.name==\"$1\")].resources.requests}"; }
rc() { k -n "$SN" get pod "$POD" -o jsonpath="{.status.containerStatuses[?(@.name==\"$1\")].restartCount}"; }
APP0=$(req app); PX0=$(req istio-proxy); PXRC0=$(rc istio-proxy)
echo "  before: app=$APP0 istio-proxy=$PX0"

recs() { svc k8s-tailor:8080 api/recommendations; }
px_rec() { recs | python3 -c '
import json,sys
d=json.load(sys.stdin).get("recommendations") or []
for r in d:
    if r.get("namespace")==sys.argv[1] and r.get("container")=="istio-proxy":
        print(r.get("containerClass"), r.get("mesh"), r.get("mode"), r.get("actuationPath")); break
' "$SN"; }
px_classified() { px_rec | grep -q '^injected-sidecar istio Recommend'; }
if poll 300 px_classified; then
  pass "istio-proxy classified: $(px_rec)"
else
  fail "istio-proxy not classified as injected-sidecar/istio/Recommend (got: $(px_rec))"
fi

app_changed() { [ "$(req app)" != "$APP0" ]; }
if poll "$INPLACE_TIMEOUT" app_changed; then
  pass "app resized in place: $APP0 -> $(req app)"
else
  fail "app did not change within ${INPLACE_TIMEOUT}s"; k -n "$NS" logs deploy/k8s-tailor --tail=30
fi
# Several reconciler cycles (10s) later: the proxy in Recommend must not change.
sleep 40
[ "$(req istio-proxy)" = "$PX0" ] && pass "istio-proxy untouched in Recommend ($PX0)" || fail "istio-proxy changed without opt-in: $PX0 -> $(req istio-proxy)"

echo "== Opt-in via annotation: container.istio-proxy.mode=InPlace"
k -n "$SN" annotate deploy meshy tailor.k8s-tailor.dev/container.istio-proxy.mode=InPlace --overwrite >/dev/null
px_changed() { [ "$(req istio-proxy)" != "$PX0" ]; }
if poll "$INPLACE_TIMEOUT" px_changed; then
  pass "istio-proxy resized in place after opt-in: $PX0 -> $(req istio-proxy)"
  [ "$(k -n "$SN" get pod "$POD" -o jsonpath='{.metadata.uid}')" = "$UID0" ] && pass "pod not recreated" || fail "pod recreated"
  [ "$(rc istio-proxy)" = "$PXRC0" ] && pass "istio-proxy without restart" || fail "istio-proxy restarted"
else
  fail "istio-proxy did not change after opt-in within ${INPLACE_TIMEOUT}s (rec: $(px_rec))"
fi

echo
if [ "$FAILS" -eq 0 ]; then printf '\033[1;32mSMOKE-SIDECAR: PASS\033[0m\n'; else printf '\033[1;31mSMOKE-SIDECAR: FAIL (%d)\033[0m\n' "$FAILS"; exit 1; fi
