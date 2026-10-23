#!/usr/bin/env bash
# k8s-tailor smoke test on k3d. PASS/FAIL output per check; exits 1 if any fails.
# Variables: INPLACE_TIMEOUT (s, default 720), VM_TIMEOUT (default 120).
set -uo pipefail
. "$(dirname "$0")/common.sh"
INPLACE_TIMEOUT="${INPLACE_TIMEOUT:-720}"
VM_TIMEOUT="${VM_TIMEOUT:-120}"
FAILS=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS+1)); }
# poll <timeout> <cmd...>: repeats until the command returns 0
poll() { local t=$1; shift; local end=$(( $(date +%s) + t )); while [ "$(date +%s)" -lt "$end" ]; do "$@" >/dev/null 2>&1 && return 0; sleep 3; done; return 1; }
svc() { k get --raw "/api/v1/namespaces/$NS/services/$1/proxy/$2" 2>/dev/null; }

echo "== 1. Pods Ready in $NS"
if k -n "$NS" rollout status deploy --timeout=180s >/dev/null 2>&1 && k -n "$NS" rollout status sts --timeout=180s >/dev/null 2>&1; then
  pass "all pods Ready: $(k -n "$NS" get pods --no-headers -o custom-columns=N:.metadata.name | tr '\n' ' ')"
else
  fail "pods not Ready"; k -n "$NS" get pods
fi

echo "== 2. CRDs and TailorConfig"
crds=$(k get crd -o name 2>/dev/null | grep -c 'tailor.k8s-tailor.dev')
[ "$crds" -ge 4 ] && pass "$crds CRDs tailor.k8s-tailor.dev" || fail "CRDs found: $crds (expected >=4)"
cfg_ok() { k get tailorconfig cluster >/dev/null 2>&1; }
poll 60 cfg_ok && pass "TailorConfig/cluster present" || fail "TailorConfig/cluster missing"

echo "== 3. Engine /healthz"
hz_ok() { svc k8s-tailor:8080 healthz | grep -q '"ok":true'; }
poll 60 hz_ok && pass "engine /healthz ok: $(svc k8s-tailor:8080 healthz)" || fail "engine /healthz not ok"

echo "== 4. VictoriaMetrics receiving series"
vm_q() { svc k8s-tailor-vmsingle:8428 "api/v1/query?query=$1"; }
vm_has() { vm_q "$1" | grep -q '"result":\[{'; }
for m in container_cpu_usage_seconds_total container_memory_working_set_bytes; do
  poll "$VM_TIMEOUT" vm_has "count($m)" && pass "series $m present" || fail "series $m missing in VM"
done
poll "$VM_TIMEOUT" vm_has "tailor_current_cpu_cores" && pass "series tailor_current_cpu_cores present" || fail "series tailor_* missing (has the engine not published yet?)"

echo "== 5. In-place resize without restart (mode=InPlace)"
# demo/ is deliberately oversized and takes ~99% of the node's requested CPU; without
# headroom the smoke pod stays Pending. Scales the demo to 0 (KEEP_DEMO=1 to leave it alone).
if [ "${KEEP_DEMO:-0}" != 1 ] && k get ns tailor-demo >/dev/null 2>&1; then
  echo "  (scaling tailor-demo deployments to 0 to free CPU; KEEP_DEMO=1 disables this)"
  k -n tailor-demo scale deploy --all --replicas=0 >/dev/null
fi
# UNIQUE namespace per run: the engine keeps the scale-down cooldown (1h) in
# memory per ns/workload; reusing the same name would make the 2nd run never act.
SN="tailor-smoke-$(date +%s)"
PN="smoke-inplace-$SN"
k delete tailorpolicy -l app.kubernetes.io/managed-by=tailor-smoke --ignore-not-found >/dev/null 2>&1
for old in $(k get ns -o name 2>/dev/null | grep '/tailor-smoke-'); do k delete "$old" --wait=false >/dev/null 2>&1; done
cleanup() { [ "${KEEP_SMOKE:-0}" = 1 ] || { k delete tailorpolicy "$PN" --ignore-not-found >/dev/null 2>&1; k delete ns "$SN" --ignore-not-found --wait=false >/dev/null 2>&1; }; }
trap cleanup EXIT
k apply -f <(sed -e "s/__SN__/$SN/g" -e "s/__PN__/$PN/g" <<'YAML'
apiVersion: v1
kind: Namespace
metadata: { name: __SN__ }
---
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorPolicy
metadata: { name: __PN__, labels: { app.kubernetes.io/managed-by: tailor-smoke } }
spec:
  priority: 200
  mode: InPlace
  selector: { namespaces: ["__SN__"] }
---
# Heavily oversized: requests 500m, uses ~110m (1s on / 8s off).
apiVersion: apps/v1
kind: Deployment
metadata: { name: smoke-burner, namespace: __SN__ }
spec:
  replicas: 1
  selector: { matchLabels: { app: smoke-burner } }
  template:
    metadata: { labels: { app: smoke-burner } }
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "while true; do timeout -s KILL 1 yes > /dev/null; sleep 8; done"]
          resources:
            requests: { cpu: "500m", memory: 64Mi }
YAML
) >/dev/null
k -n "$SN" rollout status deploy/smoke-burner --timeout=240s >/dev/null 2>&1 || { fail "smoke-burner did not become ready"; k -n "$SN" get pod; k -n "$SN" get events --sort-by=.lastTimestamp | tail -5; }
POD=$(k -n "$SN" get pod -l app=smoke-burner -o jsonpath='{.items[0].metadata.name}')
UID0=$(k -n "$SN" get pod "$POD" -o jsonpath='{.metadata.uid}')
RC0=$(k -n "$SN" get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')
REQ0=$(k -n "$SN" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests}')
echo "  before: pod=$POD uid=$UID0 restarts=$RC0 requests=$REQ0"
changed() { [ "$(k -n "$SN" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests}')" != "$REQ0" ]; }
if poll "$INPLACE_TIMEOUT" changed; then
  UID1=$(k -n "$SN" get pod "$POD" -o jsonpath='{.metadata.uid}')
  RC1=$(k -n "$SN" get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')
  REQ1=$(k -n "$SN" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests}')
  echo "  after:  pod=$POD uid=$UID1 restarts=$RC1 requests=$REQ1"
  pass "requests changed in place: $REQ0 -> $REQ1"
  [ "$UID1" = "$UID0" ] && pass "pod UID unchanged" || fail "pod was recreated (UID changed)"
  [ "$RC1" = "$RC0" ] && pass "restartCount unchanged ($RC1)" || fail "container restarted ($RC0 -> $RC1)"
else
  fail "requests did not change within ${INPLACE_TIMEOUT}s"
  echo "  --- engine logs (last 30 lines) ---"; k -n "$NS" logs deploy/k8s-tailor --tail=30
fi

echo
if [ "$FAILS" -eq 0 ]; then printf '\033[1;32mSMOKE: PASS\033[0m\n'; else printf '\033[1;31mSMOKE: FAIL (%d)\033[0m\n' "$FAILS"; exit 1; fi
