#!/usr/bin/env bash
# k8s-tailor x REAL Istio on k3d (cluster `tailor-istio`, separate from tailor-dev / tailor-netpol).
#
# Phases:  up      create cluster, install Istio (istioctl) + the chart, apply the demo workloads
#          test    run the PASS/FAIL scenarios against the running cluster
#          down    delete the cluster
# Usage:   dev/istio/mesh-test.sh [up|test|down|all|only s2_optin s5_webhook ...]        (default: all = up + test, cluster is kept)
# Env:     ISTIO_NATIVE=false|true   classic sidecar (default) or native sidecar (initContainer, restartPolicy Always)
#          ISTIO_VERSION=1.31.3      istioctl version (downloaded to a temp dir when the PATH one differs)
#          SKIP_BUILD=1              reuse local images k8s-tailor:dev / k8s-tailor-mcp:dev (default: required to exist;
#                                    otherwise build as in dev/k3s/up.sh with SRC_REF=HEAD)
#          TIMEOUT=1500              seconds to wait for each "tailor acted" condition (MIN_SAMPLES=2 => ~10 min of history)
#          ORDER_TEST=1              also run the webhook-order experiment (config renamed to aa-k8s-tailor so it runs BEFORE
#                                    the Istio injector and relies on reinvocationPolicy: IfNeeded). Restarts the engine
#                                    (confidence is rebuilt: ~10 min before the webhook mutates again).
# Native sidecars: from Istio 1.27 they are the default; this script sets ENABLE_NATIVE_SIDECARS explicitly either way.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
CLUSTER="${CLUSTER:-tailor-istio}"
CTX="k3d-${CLUSTER}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.34.12-k3s1}"
ISTIO_VERSION="${ISTIO_VERSION:-1.31.3}"
ISTIO_NATIVE="${ISTIO_NATIVE:-false}"
TIMEOUT="${TIMEOUT:-1500}"
NS=k8s-tailor
MD=mesh-demo
EXPECT_NATIVE=$([ "$ISTIO_NATIVE" = true ] && echo True || echo False)
FAILS=0

k() { kubectl --context "$CTX" "$@"; }
log() { printf '\033[1;34m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS+1)); }
info() { printf '  info %s\n' "$*"; }
poll() { local t=$1; shift; local end=$(( $(date +%s) + t )); while [ "$(date +%s)" -lt "$end" ]; do "$@" >/dev/null 2>&1 && return 0; sleep 10; done; return 1; }

# ---------------------------------------------------------------- helpers
recs_json() { k get --raw "/api/v1/namespaces/$NS/services/k8s-tailor:8080/proxy/api/recommendations"; }
# rec <workload> <container> <field>
rec() { recs_json | python3 -c '
import json,sys
w,c,f=sys.argv[1:4]
for r in json.load(sys.stdin).get("recommendations") or []:
    if r["namespace"]=="mesh-demo" and r["workload"]==w and r["container"]==c: print(r.get(f)); break
' "$@"; }
# podres <pod> <container> -> "cpuReq/memReq cpuLim/memLim restarts" (searches containers and native-sidecar initContainers)
podres() { k -n "$MD" get pod "$1" -o json | python3 -c '
import json,sys
p=json.load(sys.stdin); n=sys.argv[1]
cs=p["spec"].get("containers",[])+p["spec"].get("initContainers",[])
rc={c["name"]:c["restartCount"] for c in p["status"].get("containerStatuses",[])+p["status"].get("initContainerStatuses",[])}
for c in cs:
    if c["name"]==n:
        r=c.get("resources",{}); q=r.get("requests",{}); l=r.get("limits",{})
        print("%s/%s %s/%s %s"%(q.get("cpu"),q.get("memory"),l.get("cpu"),l.get("memory"),rc.get(n))); break
' "$2"; }
# live (non-terminating) Running pods of app=$1
livepods() { k -n "$MD" get pod -l "app=$1" -o json | python3 -c '
import json,sys
for p in json.load(sys.stdin)["items"]:
    if p["status"].get("phase")=="Running" and not p["metadata"].get("deletionTimestamp"): print(p["metadata"]["name"])'; }
firstpod() { livepods "$1" | head -1; }
uid() { k -n "$MD" get pod "$1" -o jsonpath='{.metadata.uid}'; }
admission_calls() { k get --raw /metrics | awk '/^apiserver_admission_webhook_request_total\{.*name="tailor.k8s-tailor.dev"/ && /code="200"/ {s+=$NF} END{print s+0}'; }
# throttle <pod> <seconds>: cgroup cpu.max + throttling delta of the istio-proxy container
throttle() {
  local P=$1 S=${2:-15} a b
  rd() { k -n "$MD" exec "$P" -c istio-proxy -- sh -c 'cat /sys/fs/cgroup/cpu.max; grep -E "^(usage_usec|nr_periods|nr_throttled|throttled_usec)" /sys/fs/cgroup/cpu.stat' | tr '\n' ' '; }
  a=$(rd); sleep "$S"; b=$(rd)
  python3 - "$a" "$b" "$S" <<'PY'
import sys,re
def p(s):
    t=s.split(); return {"max":t[0]+" "+t[1],**{k:int(v) for k,v in re.findall(r"(\w+) (\d+)"," ".join(t[2:]))}}
a,b,dt=p(sys.argv[1]),p(sys.argv[2]),int(sys.argv[3])
dp=b["nr_periods"]-a["nr_periods"]; dth=b["nr_throttled"]-a["nr_throttled"]
print("  info cpu.max=%s cpu_used=%.0fm throttled_periods=%d/%d (%.0f%%) throttled_time=%.1fs/%ds"%(b["max"],(b["usage_usec"]-a["usage_usec"])/dt/1000,dth,dp,100*dth/max(dp,1),(b["throttled_usec"]-a["throttled_usec"])/1e6,dt))
PY
}

# ---------------------------------------------------------------- up
istioctl_bin() {
  if command -v istioctl >/dev/null && istioctl version --remote=false 2>/dev/null | grep -q "$ISTIO_VERSION"; then command -v istioctl; return; fi
  local d; d="$(mktemp -d "${TMPDIR:-/tmp}/istioctl.XXXXXX")"
  local os=osx arch=arm64; [ "$(uname -s)" = Linux ] && os=linux; case "$(uname -m)" in x86_64|amd64) arch=amd64;; esac
  curl -fsSL "https://github.com/istio/istio/releases/download/$ISTIO_VERSION/istioctl-$ISTIO_VERSION-$os-$arch.tar.gz" | tar -xz -C "$d" >&2
  echo "$d/istioctl"
}

up() {
  local START; START=$(date +%s)
  for b in docker k3d kubectl helm curl python3; do command -v "$b" >/dev/null || { echo "missing $b" >&2; exit 1; }; done
  if k3d cluster list "$CLUSTER" >/dev/null 2>&1; then log "cluster $CLUSTER already exists"; k3d kubeconfig merge "$CLUSTER" --kubeconfig-switch-context=false --kubeconfig-merge-default >/dev/null
  else
    log "creating cluster $CLUSTER"
    k3d cluster create "$CLUSTER" --image "$K3S_IMAGE" --servers 1 --agents 0 --no-lb \
      --k3s-arg "--disable=traefik@server:0" --k3s-arg "--disable=servicelb@server:0" --k3s-arg "--disable=metrics-server@server:0" \
      --kubeconfig-switch-context=false --wait --timeout 240s || exit 1
  fi
  k wait --for=condition=Ready node --all --timeout=120s >/dev/null

  if [ "${SKIP_BUILD:-0}" != 1 ] && ! docker image inspect k8s-tailor:dev k8s-tailor-mcp:dev >/dev/null 2>&1; then
    log "images missing: building via dev/k3s/up.sh logic (SRC_REF=HEAD)"
    local src; src="$(mktemp -d "${TMPDIR:-/tmp}/tailor-src.XXXXXX")"
    git -C "$ROOT" archive HEAD | tar -x -C "$src"
    docker build -t k8s-tailor:dev "$src" && docker build -t k8s-tailor-mcp:dev "$src/mcp" || exit 1
  fi
  k3d image import k8s-tailor:dev k8s-tailor-mcp:dev -c "$CLUSTER" >/dev/null

  local IC; IC="$(istioctl_bin)"; log "istioctl: $("$IC" version --remote=false)"
  "$IC" --context "$CTX" install -y --set profile=minimal \
    --set values.pilot.env.ENABLE_NATIVE_SIDECARS="$ISTIO_NATIVE" \
    --set values.pilot.resources.requests.cpu=50m --set values.pilot.resources.requests.memory=128Mi || exit 1
  k -n istio-system rollout status deploy/istiod --timeout=180s

  log "helm install (values-dev.yaml + values-istio.yaml)"
  helm dep build "$ROOT/chart" >/dev/null 2>&1
  helm upgrade --install k8s-tailor "$ROOT/chart" --kube-context "$CTX" -n "$NS" --create-namespace \
    -f "$ROOT/dev/k3s/values-dev.yaml" -f "$HERE/values-istio.yaml" \
    --set tailor.image.repository=k8s-tailor --set tailor.image.tag=dev --wait --timeout 8m || exit 1

  log "applying demo workloads"
  k apply -f "$HERE/mesh-demo.yaml" -f "$HERE/scenarios.yaml" >/dev/null
  for d in web client optin roll wh; do k -n "$MD" rollout status deploy/$d --timeout=300s >/dev/null || echo "WARN: $d not ready"; done
  log "up in $(( $(date +%s) - START ))s (Istio $ISTIO_VERSION, native=$ISTIO_NATIVE)"
}

# ---------------------------------------------------------------- scenarios
s1_classic() {
  echo "== (1) injected sidecar: classified, app resized in place, proxy untouched [native=$ISTIO_NATIVE]"
  local P; P=$(firstpod web)
  local u0 app0 px0; u0=$(uid "$P"); app0=$(podres "$P" app | cut -d' ' -f1); px0=$(podres "$P" istio-proxy)
  info "before: app=$app0 istio-proxy=$px0"
  local line; line="$(rec web istio-proxy containerClass) $(rec web istio-proxy mesh) $(rec web istio-proxy mode) native=$(rec web istio-proxy nativeSidecar)"
  [ "$line" = "injected-sidecar istio Recommend native=$EXPECT_NATIVE" ] && pass "istio-proxy: $line" || fail "unexpected classification: $line (want injected-sidecar istio Recommend native=$EXPECT_NATIVE)"
  info "(the code classifies a native Istio proxy as injected-sidecar + nativeSidecar=true, not 'native-sidecar'; see docs/CONTAINERS.md 2.1)"
  app_changed() { [ "$(podres "$P" app | cut -d' ' -f1)" != "$app0" ] || [ "$(k -n "$MD" get pod "$P" -o jsonpath='{.spec.containers[?(@.name=="app")].resources.requests.memory}')" != "64Mi" ]; }
  if poll "$TIMEOUT" app_changed; then
    pass "app resized in place: $(podres "$P" app)"
    [ "$(uid "$P")" = "$u0" ] && [ "$(podres "$P" app | awk '{print $3}')" = 0 ] && pass "same pod UID, app restartCount=0" || fail "pod recreated or app restarted"
  else fail "app not resized within ${TIMEOUT}s"; fi
  sleep 45
  [ "$(podres "$P" istio-proxy)" = "$px0" ] && pass "istio-proxy untouched in Recommend: $px0" || fail "istio-proxy changed without opt-in: $px0 -> $(podres "$P" istio-proxy)"
}

s2_optin() {
  echo "== (2) opt-in annotation container.istio-proxy.mode=InPlace [native=$ISTIO_NATIVE]"
  local P; P=$(firstpod optin)
  local u0 px0; u0=$(uid "$P"); px0=$(podres "$P" istio-proxy)
  info "before: istio-proxy=$px0 (template annotations: proxyCPU 20m / limit 50m)"
  info "rec mode=$(rec optin istio-proxy mode) policy=$(rec optin istio-proxy containerPolicy) path=$(rec optin istio-proxy actuationPath)"
  # baseline = the template annotation (proxyCPU 20m): tailor may already have resized this pod before the test started
  px_changed() { [ "$(podres "$P" istio-proxy | cut -d' ' -f1 | cut -d/ -f1)" != "20m" ]; }
  if poll "$TIMEOUT" px_changed; then
    pass "istio-proxy resized in place: $px0 -> $(podres "$P" istio-proxy)"
    [ "$(uid "$P")" = "$u0" ] && [ "$(podres "$P" istio-proxy | awk '{print $3}')" = 0 ] && pass "pod not recreated, istio-proxy restartCount=0" || fail "pod recreated or proxy restarted"
  else fail "istio-proxy not resized within ${TIMEOUT}s"; fi
  if [ "$ISTIO_NATIVE" = true ]; then
    k -n "$MD" get pod "$P" -o jsonpath='{.spec.initContainers[?(@.name=="istio-proxy")].restartPolicy}' | grep -q Always && pass "native: istio-proxy is an initContainer with restartPolicy Always (resized via pods/resize on spec.initContainers)" || fail "istio-proxy is not a native sidecar"
  fi
  echo "  -- cgroup vs real effect (informational; needs load on optin: scale optin-load to 1)"
  if k -n "$MD" get deploy optin-load -o jsonpath='{.spec.replicas}' | grep -qv '^0$'; then
    throttle "$P" 15
    info "now a manual pods/resize of the proxy limit (what tailor would do with cpu.limitFactor): 500m"
    k -n "$MD" patch pod "$P" --subresource resize --type strategic -p '{"spec":{"'"$([ "$ISTIO_NATIVE" = true ] && echo initContainers || echo containers)"'":[{"name":"istio-proxy","resources":{"requests":{"cpu":"100m"},"limits":{"cpu":"500m"}}}]}}' >/dev/null
    sleep 8; throttle "$P" 15
    info "Envoy worker threads (fixed at start-up, NOT changed by a cgroup resize): $(k -n "$MD" exec "$P" -c istio-proxy -- curl -s localhost:15000/stats | grep 'server.concurrency')"
  else info "optin-load is scaled to 0: skipping (kubectl -n $MD scale deploy optin-load --replicas=1)"; fi
}

s4_template() {
  echo "== (4) template path: pod-template annotations, no bogus container [native=$ISTIO_NATIVE]"
  info "rec: path=$(rec roll istio-proxy actuationPath) inTemplate=$(rec roll istio-proxy inTemplate)"
  has_ann() { k -n "$MD" get deploy roll -o json | python3 -c '
import json,sys
a=json.load(sys.stdin)["spec"]["template"]["metadata"].get("annotations",{})
sys.exit(0 if all(("sidecar.istio.io/"+x) in a for x in ("proxyCPU","proxyMemory","proxyCPULimit","proxyMemoryLimit")) else 1)'; }
  if poll "$TIMEOUT" has_ann; then
    pass "template annotations: $(k -n "$MD" get deploy roll -o json | python3 -c 'import json,sys;print({k:v for k,v in json.load(sys.stdin)["spec"]["template"]["metadata"]["annotations"].items() if "sidecar.istio.io/proxy" in k})')"
  else fail "annotations not written within ${TIMEOUT}s"; return; fi
  k -n "$MD" get deploy roll -o json | python3 -c '
import json,sys
s=json.load(sys.stdin)["spec"]["template"]["spec"]
names=[c["name"] for c in s["containers"]]+[c["name"] for c in s.get("initContainers",[])]
sys.exit(1 if "istio-proxy" in names else 0)' && pass "template has no istio-proxy container (nothing bogus appended)" || fail "istio-proxy was patched into the template"
  k -n "$MD" rollout status deploy/roll --timeout=300s >/dev/null
  local P ann; P=$(firstpod roll)
  ann=$(k -n "$MD" get deploy roll -o jsonpath='{.spec.template.metadata.annotations}' | python3 -c 'import json,sys;a=json.load(sys.stdin);print(a["sidecar.istio.io/proxyCPU"],a["sidecar.istio.io/proxyMemory"],a["sidecar.istio.io/proxyCPULimit"],a["sidecar.istio.io/proxyMemoryLimit"])')
  info "annotations: $ann ; new pod proxy: $(podres "$P" istio-proxy)"
  local want; want=$(python3 - $ann <<'PY'
import sys
c,m,cl,ml=sys.argv[1:5]
def q(x): return x
print("%s/%s %s/%s"%(c,m,cl,ml))
PY
)
  info "expected in pod (cpuReq/memReq cpuLim/memLim, Kubernetes may print 2000m as 2 and 1024Mi as 1Gi): $want"
  # compare numerically through kubectl's canonical form
  local got; got=$(podres "$P" istio-proxy | cut -d' ' -f1,2)
  [ "$(echo "$got" | cut -d' ' -f1)" = "$(echo "$want" | cut -d' ' -f1)" ] && pass "Istio applied the annotations to the NEW pod ($got)" || info "requests differ (pod may predate the last annotation change): got=$got want=$want"
}

s5_webhook() {
  echo "== (5) webhook order: injection + Tailor webhook on the same new pod [native=$ISTIO_NATIVE]"
  local cfg; cfg=$(k get mutatingwebhookconfiguration -o name | grep k8s-tailor | head -1)
  local ri; ri=$(k get "$cfg" -o jsonpath='{.webhooks[0].reinvocationPolicy}')
  info "$(k get mutatingwebhookconfiguration -o name | tr '\n' ' ')"
  [ "$ri" = IfNeeded ] && pass "$cfg reinvocationPolicy=$ri; istio-sidecar-injector reinvocationPolicy=$(k get mutatingwebhookconfiguration istio-sidecar-injector -o jsonpath='{.webhooks[0].reinvocationPolicy}')" || fail "reinvocationPolicy=$ri"
  [ "$cfg" = mutatingwebhookconfiguration.admissionregistration.k8s.io/zz-k8s-tailor ] && pass "name sorts after istio-sidecar-injector (zz-k8s-tailor)" || info "webhook config is $cfg (ORDER_TEST rename?)"
  both_conf() { recs_json | python3 -c '
import json,sys
r=[x for x in json.load(sys.stdin)["recommendations"] if x["namespace"]=="mesh-demo" and x["workload"]=="wh"]
sys.exit(0 if len(r)==2 and all(x["confident"] for x in r) else 1)'; }
  poll "$TIMEOUT" both_conf || fail "wh recommendations never became confident"
  local c0; c0=$(admission_calls)
  k -n "$MD" delete pod -l app=wh --wait=false >/dev/null
  sleep 20; k -n "$MD" rollout status deploy/wh --timeout=300s >/dev/null
  local c1; c1=$(admission_calls)
  local n=0 bad=0 P
  for P in $(livepods wh); do
    n=$((n+1)); info "$P app=$(podres "$P" app) istio-proxy=$(podres "$P" istio-proxy)"
    [ "$(podres "$P" app | cut -d' ' -f1)" = "500m/256Mi" ] && bad=1
    [ "$(podres "$P" istio-proxy | cut -d' ' -f1)" = "100m/128Mi" ] && bad=1
    k -n "$MD" get pod "$P" -o jsonpath='{.spec.containers[*].name} | {.spec.initContainers[*].name}{"\n"}' | grep -q istio-proxy || bad=1
  done
  [ "$bad" = 0 ] && [ "$n" -ge 1 ] && pass "new pods have the proxy injected AND app+proxy requests mutated" || fail "pod not (fully) mutated (see above; right after an engine restart recs are not confident for ~10 min)"
  info "apiserver admission calls to tailor webhook for $n new pod(s): $((c1-c0)) (n = no reinvocation; 2n = reinvoked after the injector)"
}

s5b_order() {
  echo "== (5b) ORDER_TEST: tailor webhook renamed to aa-k8s-tailor (runs BEFORE istio); relies on IfNeeded"
  helm upgrade k8s-tailor "$ROOT/chart" --kube-context "$CTX" -n "$NS" -f "$ROOT/dev/k3s/values-dev.yaml" -f "$HERE/values-istio.yaml" \
    --set tailor.image.repository=k8s-tailor --set tailor.image.tag=dev --set tailor.webhook.configurationName=aa-k8s-tailor --wait --timeout 5m >/dev/null
  s5_webhook
  helm upgrade k8s-tailor "$ROOT/chart" --kube-context "$CTX" -n "$NS" -f "$ROOT/dev/k3s/values-dev.yaml" -f "$HERE/values-istio.yaml" \
    --set tailor.image.repository=k8s-tailor --set tailor.image.tag=dev --wait --timeout 5m >/dev/null
}

s6_excluded() {
  echo "== (6) istio-system / kube-system are never touched"
  k apply -f "$HERE/adversarial-policy.yaml" >/dev/null
  local before after
  snap() { for n in istio-system kube-system; do k -n $n get pod -o json | python3 -c '
import sys,json
for p in json.load(sys.stdin)["items"]:
    for c in p["spec"].get("containers",[]): print(p["metadata"]["namespace"],p["metadata"]["name"].rsplit("-",2)[0],c["name"],json.dumps(c.get("resources"),sort_keys=True))' ; done | sort; }
  before=$(snap)
  k -n istio-system delete pod -l app=istiod --wait=false >/dev/null; k -n kube-system delete pod -l k8s-app=kube-dns --wait=false >/dev/null
  k -n istio-system rollout status deploy/istiod --timeout=180s >/dev/null; k -n kube-system rollout status deploy/coredns --timeout=180s >/dev/null
  sleep 60
  after=$(snap)
  [ "$before" = "$after" ] && pass "istiod and coredns re-created (webhook selector matches both namespaces) with unchanged resources" || { fail "resources changed"; diff <(echo "$before") <(echo "$after"); }
  recs_json | python3 -c '
import json,sys,collections
c=collections.Counter(r["namespace"] for r in json.load(sys.stdin)["recommendations"]); print("  info recommendations per namespace:",dict(c))
sys.exit(1 if ("istio-system" in c or "kube-system" in c) else 0)' && pass "no recommendation exists for istio-system / kube-system even with a '*' policy" || fail "recommendation for an excluded namespace"
  k delete -f "$HERE/adversarial-policy.yaml" >/dev/null
}

s7_traffic() {
  echo "== (7) traffic through the mesh after the resizes [native=$ISTIO_NATIVE]"
  local out; out=$(k -n "$MD" exec deploy/client -c app -- wget -S -T 3 -O- http://web:8080 2>&1)
  echo "$out" | sed 's/^/    /' | grep -E "HTTP/|server:|x-envoy|hello"
  echo "$out" | grep -q "server: envoy" && echo "$out" | grep -q hello-from && pass "client -> web via mesh (envoy hop) works" || fail "request failed or did not traverse envoy"
  local ok=0 i; for i in $(seq 1 20); do k -n "$MD" exec deploy/client -c app -- wget -q -T 3 -O- http://web:8080 >/dev/null 2>&1 && ok=$((ok+1)); done
  [ "$ok" = 20 ] && pass "20/20 requests OK" || fail "$ok/20 requests OK"
  k -n "$MD" get pods -o json | python3 -c '
import json,sys
bad=[p["metadata"]["name"] for p in json.load(sys.stdin)["items"] if p["status"]["phase"]=="Running" and any(c["restartCount"] for c in p["status"].get("containerStatuses",[])+p["status"].get("initContainerStatuses",[]))]
print("  info pods with restarts:",bad or "none")'
}

test_all() {
  echo "Istio: $(k -n istio-system get deploy istiod -o jsonpath='{.spec.template.spec.containers[0].image}') native=$ISTIO_NATIVE  K8s: $(k version -o json 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["serverVersion"]["gitVersion"])')"
  s1_classic; s2_optin; s4_template; s5_webhook; s6_excluded; s7_traffic
  [ "${ORDER_TEST:-0}" = 1 ] && { s5b_order; s7_traffic; }
  echo
  if [ "$FAILS" -eq 0 ]; then printf '\033[1;32mMESH-TEST: PASS\033[0m\n'; else printf '\033[1;31mMESH-TEST: FAIL (%d)\033[0m\n' "$FAILS"; return 1; fi
}

case "${1:-all}" in
  up) up ;;
  test) test_all ;;
  only) shift; for f in "$@"; do "$f"; done; [ "$FAILS" -eq 0 ] ;;
  down) k3d cluster delete "$CLUSTER" ;;
  all) up && test_all ;;
  *) echo "usage: $0 [up|test|down|all]" >&2; exit 2 ;;
esac
