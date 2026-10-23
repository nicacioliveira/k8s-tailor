#!/usr/bin/env bash
# Proves that the chart's NetworkPolicies really block / allow traffic, on a k3d
# cluster whose CNI enforces them (Calico). k3d's default flannel setup is NOT a
# reliable enforcer, hence the dedicated cluster `tailor-netpol`.
#
#   dev/calico/netpol-test.sh [up|test|down|all]      (default: all)
#
#   up    create the cluster + Calico + install the chart (values-dev + values-netpol)
#   test  run the scenarios (PASS/FAIL), needs `up` first
#   mutate (slow, up to ~1h, needs `up` and the policies on) shows a REAL webhook mutation: deploys a
#         workload in np-optin, waits for a recommendation, recreates its pod and checks the resize
#   down  delete the cluster
#   all   up, test, down (down also runs when the tests fail)
#
# Variables: CLUSTER (tailor-netpol), CALICO_VERSION (v3.33.0), K3S_IMAGE,
#   KEEP=1 (do not delete the cluster at the end of `all`),
#   FORCE=1 (carry on even if the enforcement canary fails: the blocked
#   scenarios then FAIL, which is the correct verdict for a non-enforcing CNI).
# Requires a Docker daemon whose kernel has the netfilter modules Calico needs
# (xt_CT, xt_NFLOG, rpfilter): Docker Desktop, OrbStack, Colima/Lima, Linux. See README.md.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
CLUSTER="${CLUSTER:-tailor-netpol}"
CTX="k3d-${CLUSTER}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.34.12-k3s1}"
CALICO_VERSION="${CALICO_VERSION:-v3.33.0}"
# k8s 1.34/1.35 need the v1beta1 CRD bundle (+ the MutatingAdmissionPolicy gate below); 1.36+: v3_projectcalico_org.yaml
CALICO_CRDS="${CALICO_CRDS:-v3_projectcalico_org-v1beta1.yaml}"
CALICO_BASE="https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests"
NS=k8s-tailor
REL=k8s-tailor
IMAGE="${IMAGE:-k8s-tailor}"
MCP_IMAGE="${MCP_IMAGE:-k8s-tailor-mcp}"
TAG="${TAG:-dev}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.10.1}"
WEBHOOK_NAME="tailor.k8s-tailor.dev"

T0=$(date +%s)
PASS=0; FAIL=0
FAILED=()

log()  { printf '\033[1;34m[%s +%ss]\033[0m %s\n' "$(date +%H:%M:%S)" "$(( $(date +%s) - T0 ))" "$*"; }
hdr()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
k()    { kubectl --context "$CTX" "$@"; }
pass() { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
fail() { FAIL=$((FAIL+1)); FAILED+=("$*"); printf '  \033[31mFAIL\033[0m  %s\n' "$*"; }

helm_install() { # extra helm args...
  helm upgrade --install "$REL" "$ROOT/chart" --kube-context "$CTX" --namespace "$NS" --create-namespace \
    -f "$ROOT/dev/k3s/values-dev.yaml" -f "$HERE/values-netpol.yaml" \
    --set tailor.image.repository="$IMAGE" --set tailor.image.tag="$TAG" \
    --set mcp.image.repository="$MCP_IMAGE" --set mcp.image.tag="$TAG" \
    --wait --timeout 8m "$@" >/dev/null
}

# ---------------------------------------------------------------- probing ---
# probe NS POD METHOD URL [BODY] -> "<curl rc> <http code>".
# rc 28 = connect timeout (what a NetworkPolicy DROP looks like), rc 7 = refused.
probe() {
  local ns=$1 pod=$2 method=$3 url=$4 body=${5:-}
  k -n "$ns" exec "$pod" -- sh -c \
    "c=\$(curl -sk -o /dev/null -m 6 --connect-timeout 4 -w '%{http_code}' -X '$method' ${body:+--data '$body'} '$url'); echo \"\$? \$c\"" 2>/dev/null | tail -1
}
# curl rcs that prove the TCP handshake completed (HTTP error / empty reply / reset after connect)
connected() { case "$1" in 0|22|35|52|56) return 0;; *) return 1;; esac; }

expect_reach() { # LABEL NS POD METHOD URL [BODY]
  local label=$1; shift; local r rc code; r=$(probe "$@"); rc=${r%% *}; code=${r##* }
  if connected "$rc"; then pass "$label  -> reachable (curl rc=$rc http=$code)"
  else fail "$label  -> expected reachable, got curl rc=$rc http=$code"; fi
}
expect_block() { # LABEL NS POD METHOD URL [BODY]
  local label=$1; shift; local r rc code; r=$(probe "$@"); rc=${r%% *}; code=${r##* }
  if [ "$rc" = 28 ]; then pass "$label  -> BLOCKED (connect timeout, curl rc=28)"
  elif connected "$rc"; then fail "$label  -> expected BLOCKED, but connected (curl rc=$rc http=$code)"
  else fail "$label  -> expected BLOCKED by timeout (28), got curl rc=$rc (refused/DNS = not a policy drop)"; fi
}

# Cases that MUST be blocked while the policies are on, and MUST pass in the negative control.
# Format: label|ns|pod|method|url|body
BLOCKED=()
add_blocked() { BLOCKED+=("$1|$2|$3|$4|$5|${6:-}"); }
ENGINE="http://k8s-tailor.$NS.svc:8080/healthz"
CH_HTTP="http://k8s-tailor-clickhouse.$NS.svc:8123/ping"
CH_NATIVE="http://k8s-tailor-clickhouse.$NS.svc:9000/"
VM="http://k8s-tailor-vmsingle.$NS.svc:8428/health"
VM_IMPORT="http://k8s-tailor-vmsingle.$NS.svc:8428/api/v1/import/prometheus"
MCP="http://k8s-tailor-mcp.$NS.svc:8020/healthz"
WH="https://k8s-tailor.$NS.svc:8443/mutate"
WH_BODY='{"request":{"uid":"np-test"}}'

define_blocked() {
  BLOCKED=()
  add_blocked "(1) engine :8080 from np-other/t"              np-other  t        GET  "$ENGINE"
  add_blocked "(1) engine :8080 from np-ingress/t"            np-ingress t       GET  "$ENGINE"
  add_blocked "(1) engine :8080 from release-ns pod w/o label" "$NS"    t-plain  GET  "$ENGINE"
  add_blocked "(2) clickhouse :8123 from MCP-labelled pod"     "$NS"    t-mcp    GET  "$CH_HTTP"
  add_blocked "(2) clickhouse :8123 from np-metrics/t"        np-metrics t       GET  "$CH_HTTP"
  add_blocked "(2) clickhouse :8123 from np-other/t"          np-other  t        GET  "$CH_HTTP"
  add_blocked "(2) clickhouse :9000 from np-other/t"          np-other  t        GET  "$CH_NATIVE"
  add_blocked "(2) clickhouse :8123 from release-ns pod"       "$NS"    t-plain  GET  "$CH_HTTP"
  add_blocked "(3) vmsingle :8428 from np-other/t"            np-other  t        GET  "$VM"
  add_blocked "(3) vmsingle POST import from np-other/t"      np-other  t        POST "$VM_IMPORT" 'np_test_metric 1'
  add_blocked "(3) vmsingle :8428 from MCP-labelled pod"       "$NS"    t-mcp    GET  "$VM"
  add_blocked "(3) vmsingle :8428 from release-ns pod"         "$NS"    t-plain  GET  "$VM"
  add_blocked "(4) MCP :8020 from np-other/t"                 np-other  t        GET  "$MCP"
  add_blocked "(4) MCP :8020 from np-metrics/t"               np-metrics t       GET  "$MCP"
}

wh_metric() { # sum of apiserver webhook counters: request_total{code=200} and fail_open_count
  local raw; raw=$(k get --raw /metrics 2>/dev/null | grep -F "name=\"$WEBHOOK_NAME\"" || true)
  local ok fo
  ok=$(printf '%s\n' "$raw" | awk '/^apiserver_admission_webhook_request_total/ && /code="200"/ {s+=$NF} END{printf "%d", s}')
  fo=$(printf '%s\n' "$raw" | awk '/^apiserver_admission_webhook_fail_open_count/ {s+=$NF} END{printf "%d", s}')
  echo "$ok $fo"
}

# ------------------------------------------------------------------- up -----
cmd_up() {
  for b in docker k3d kubectl helm; do command -v "$b" >/dev/null || { echo "missing $b" >&2; exit 1; }; done
  docker info >/dev/null 2>&1 || { echo "docker daemon unreachable" >&2; exit 1; }
  if k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
    log "cluster $CLUSTER already exists"
  else
    log "creating k3d cluster $CLUSTER (flannel + k3s netpol controller OFF -> Calico)"
    k3d cluster create "$CLUSTER" --image "$K3S_IMAGE" --servers 1 --agents 0 --no-lb \
      --k3s-arg "--flannel-backend=none@server:*" --k3s-arg "--disable-network-policy@server:*" \
      --k3s-arg "--disable=traefik@server:0" --k3s-arg "--disable=servicelb@server:0" --k3s-arg "--disable=metrics-server@server:0" \
      --k3s-arg "--kube-apiserver-arg=feature-gates=InPlacePodVerticalScaling=true,MutatingAdmissionPolicy=true@server:0" \
      --k3s-arg "--kube-apiserver-arg=runtime-config=admissionregistration.k8s.io/v1beta1=true@server:0" \
      --k3s-arg "--kubelet-arg=feature-gates=InPlacePodVerticalScaling=true@server:0" \
      --wait --timeout 240s
  fi
  k3d kubeconfig merge "$CLUSTER" --kubeconfig-switch-context=false --kubeconfig-merge-default >/dev/null

  log "installing Calico $CALICO_VERSION (tigera-operator)"
  k create -f "$CALICO_BASE/$CALICO_CRDS" >/dev/null
  k create -f "$CALICO_BASE/tigera-operator.yaml" >/dev/null
  k create -f "$CALICO_BASE/custom-resources.yaml" >/dev/null
  # Flow-log components (Goldmane/Whisker) are not needed and add NFLOG rules: drop them.
  k delete goldmane default --ignore-not-found >/dev/null 2>&1 || true
  k delete whisker default --ignore-not-found >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do k -n calico-system get ds calico-node >/dev/null 2>&1 && break; sleep 5; done
  k -n calico-system rollout status ds/calico-node --timeout=420s
  k wait --for=condition=Ready node --all --timeout=300s
  k -n kube-system rollout status deploy/coredns --timeout=300s >/dev/null

  canary

  for img in "$IMAGE:$TAG" "$MCP_IMAGE:$TAG"; do
    if ! docker image inspect "$img" >/dev/null 2>&1; then
      log "image $img missing in this Docker daemon: building (slow, see dev/k3s/up.sh)"
      if [ "$img" = "$IMAGE:$TAG" ]; then docker build -t "$img" "$ROOT"; else docker build -t "$img" "$ROOT/mcp"; fi
    fi
  done
  k3d image import "$IMAGE:$TAG" "$MCP_IMAGE:$TAG" -c "$CLUSTER"

  log "helm install (networkPolicy ON, mcp.auth ON, webhook ON certMode=helm)"
  helm dep build "$ROOT/chart" >/dev/null
  helm_install
  k -n "$NS" get pods
  k -n "$NS" get netpol
}

# Does this cluster actually enforce NetworkPolicy? deny-all => timeout; policy removed => reachable.
canary() {
  log "enforcement canary (deny-all NetworkPolicy must actually drop)"
  k create ns np-canary >/dev/null 2>&1 || true
  k -n np-canary run srv --image=nginxinc/nginx-unprivileged:1.27-alpine --port=8080 >/dev/null 2>&1 || true
  k -n np-canary run cli --image="$CURL_IMAGE" --command -- sleep 3600 >/dev/null 2>&1 || true
  k -n np-canary wait --for=condition=Ready pod --all --timeout=240s >/dev/null
  local ip; ip=$(k -n np-canary get pod srv -o jsonpath='{.status.podIP}')
  k -n np-canary apply -f - >/dev/null <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: deny-all, namespace: np-canary }
spec: { podSelector: {}, policyTypes: ["Ingress"] }
EOF
  sleep 8
  local r; r=$(probe np-canary cli GET "http://$ip:8080/"); r=${r%% *}
  k -n np-canary delete netpol deny-all >/dev/null
  if [ "$r" = 28 ]; then
    log "canary OK: deny-all dropped traffic (curl rc=28)"
  else
    echo "CANARY FAILED: deny-all did not drop traffic (curl rc=$r). This CNI/kernel does not enforce NetworkPolicy." >&2
    echo "Calico needs xt_CT/xt_NFLOG/rpfilter in the Docker VM kernel (not available on Dory); see dev/calico/README.md." >&2
    [ "${FORCE:-0}" = 1 ] || exit 3
  fi
  k delete ns np-canary --wait=false >/dev/null
}

# ------------------------------------------------------------------ test ----
setup_pods() {
  for n in np-ingress np-metrics np-other np-optin; do k create ns "$n" >/dev/null 2>&1 || true; done
  # Impersonators carry the labels the policies select on. Their readiness probe
  # always fails so they never join the Services' endpoints (a pod with
  # app=k8s-tailor that does not listen on 8080/8443 would poison k8s-tailor).
  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: t-engine, namespace: $NS, labels: { app: k8s-tailor } }
spec:
  terminationGracePeriodSeconds: 0
  containers:
    - { name: c, image: "$CURL_IMAGE", command: ["sleep","86400"], readinessProbe: { exec: { command: ["false"] } } }
---
apiVersion: v1
kind: Pod
metadata: { name: t-mcp, namespace: $NS, labels: { app: k8s-tailor-mcp } }
spec:
  terminationGracePeriodSeconds: 0
  containers:
    - { name: c, image: "$CURL_IMAGE", command: ["sleep","86400"], readinessProbe: { exec: { command: ["false"] } } }
---
apiVersion: v1
kind: Pod
metadata: { name: t-plain, namespace: $NS }
spec:
  terminationGracePeriodSeconds: 0
  containers: [ { name: c, image: "$CURL_IMAGE", command: ["sleep","86400"] } ]
EOF
  for n in np-ingress np-metrics np-other; do
    k -n "$n" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: t, namespace: $n }
spec:
  terminationGracePeriodSeconds: 0
  containers: [ { name: c, image: "$CURL_IMAGE", command: ["sleep","86400"] } ]
EOF
  done
  for spec in "$NS/t-engine" "$NS/t-mcp" "$NS/t-plain" np-ingress/t np-metrics/t np-other/t; do
    k -n "${spec%%/*}" wait --for=jsonpath='{.status.phase}'=Running "pod/${spec##*/}" --timeout=240s >/dev/null
  done
  sleep 5  # let the CNI program the new endpoints
}

scenario1() {
  hdr "(1) engine :8080 - only MCP + metricsFrom (np-metrics); blocked from arbitrary namespaces"
  expect_reach "engine from MCP-labelled pod (app=k8s-tailor-mcp)" "$NS" t-mcp GET "$ENGINE"
  expect_reach "engine from np-metrics/t (metricsFrom)" np-metrics t GET "$ENGINE"
  local out
  out=$(k -n "$NS" exec deploy/k8s-tailor-mcp -c mcp -- bun -e \
    "fetch('$ENGINE',{signal:AbortSignal.timeout(5000)}).then(r=>console.log('code',r.status)).catch(e=>console.log('ERR',e.message))" 2>&1 | tail -1)
  case "$out" in "code 200") pass "engine from the REAL MCP pod (bun fetch) -> $out";; *) fail "engine from the REAL MCP pod -> $out";; esac
  expect_block "engine from np-other/t (arbitrary ns)"  np-other  t GET "$ENGINE"
  expect_block "engine from np-ingress/t (mcpFrom ns, not metricsFrom)" np-ingress t GET "$ENGINE"
  expect_block "engine from unlabelled pod in the release ns" "$NS" t-plain GET "$ENGINE"
}
scenario2() {
  hdr "(2) ClickHouse :8123/:9000 - only the engine"
  expect_reach "clickhouse :8123 from engine-labelled pod" "$NS" t-engine GET "$CH_HTTP"
  expect_reach "clickhouse :9000 from engine-labelled pod" "$NS" t-engine GET "$CH_NATIVE"
  for c in "${BLOCKED[@]}"; do case "$c" in "(2)"*) run_case block "$c";; esac; done
}
scenario3() {
  hdr "(3) vmsingle :8428 - only the engine + metricsFrom"
  expect_reach "vmsingle GET /health from engine-labelled pod" "$NS" t-engine GET "$VM"
  expect_reach "vmsingle GET /health from np-metrics/t (metricsFrom)" np-metrics t GET "$VM"
  expect_reach "vmsingle POST /api/v1/import/prometheus from engine-labelled pod" "$NS" t-engine POST "$VM_IMPORT" 'np_test_metric 1'
  for c in "${BLOCKED[@]}"; do case "$c" in "(3)"*) run_case block "$c";; esac; done
}
scenario4() {
  hdr "(4) MCP :8020 - release namespace + mcpFrom (np-ingress)"
  expect_reach "MCP from np-ingress/t (mcpFrom)" np-ingress t GET "$MCP"
  expect_reach "MCP from unlabelled pod in the release ns" "$NS" t-plain GET "$MCP"
  for c in "${BLOCKED[@]}"; do case "$c" in "(4)"*) run_case block "$c";; esac; done
}
run_case() { # block|reach "label|ns|pod|method|url|body"
  local mode=$1 IFS='|'; read -r label ns pod method url body <<<"$2"
  if [ "$mode" = block ]; then expect_block "$label" "$ns" "$pod" "$method" "$url" "$body"
  else expect_reach "$label" "$ns" "$pod" "$method" "$url" "$body"; fi
}
scenario5() {
  hdr "(5) webhook :8443 - kube-apiserver reaches it; webhookFrom restricts everybody else"
  local b a ok0 fo0 ok1 fo1
  read -r ok0 fo0 <<<"$(wh_metric)"
  k -n np-optin run "w-$RANDOM" --image="$CURL_IMAGE" --restart=Never --command -- sleep 20 >/dev/null
  sleep 6
  read -r ok1 fo1 <<<"$(wh_metric)"
  if [ $((ok1-ok0)) -ge 1 ] && [ "$fo1" = "$fo0" ]; then
    pass "apiserver -> webhook on pod CREATE in opted-in ns: request_total{code=200} +$((ok1-ok0)), fail_open +0 (apiserver_admission_webhook_*)"
  else
    fail "apiserver -> webhook: code200 $ok0->$ok1, fail_open $fo0->$fo1 (webhook failurePolicy=Ignore hides a blocked call; fail_open must stay flat)"
  fi
  local ann; ann=$(k -n np-optin get pod -o jsonpath='{.items[-1].metadata.annotations}' 2>/dev/null || true)
  case "$ann" in *admission-sized*) log "info: pod was MUTATED (admission-sized annotation present)";;
    *) log "info: pod admitted unmutated (expected: the engine has no recommendation for a brand-new bare pod; reach is proven by the apiserver counters)";; esac
  expect_reach "webhook from np-other/t with webhookFrom=[] (open by design)" np-other t POST "$WH" "$WH_BODY"
}
scenario5b() {
  log "helm upgrade: networkPolicy.webhookFrom=[ipBlock 203.0.113.0/24] (a CIDR that is nobody)"
  helm_install --set 'networkPolicy.webhookFrom[0].ipBlock.cidr=203.0.113.0/24'
  sleep 8
  expect_block "webhook from np-other/t with webhookFrom set"   np-other  t GET "$WH"
  expect_block "webhook from np-ingress/t with webhookFrom set" np-ingress t GET "$WH"
  expect_block "webhook from release-ns pod with webhookFrom set" "$NS" t-plain GET "$WH"
  local ok0 fo0 ok1 fo1
  read -r ok0 fo0 <<<"$(wh_metric)"
  k -n np-optin run "w-$RANDOM" --image="$CURL_IMAGE" --restart=Never --command -- sleep 20 >/dev/null
  sleep 6
  read -r ok1 fo1 <<<"$(wh_metric)"
  if [ $((ok1-ok0)) -ge 1 ] && [ "$fo1" = "$fo0" ]; then
    pass "apiserver still reaches the webhook with webhookFrom set (single node: host->local-pod traffic is not subject to NP in Calico): code200 +$((ok1-ok0)), fail_open +0"
  else
    fail "apiserver -> webhook with webhookFrom set: code200 $ok0->$ok1, fail_open $fo0->$fo1"
  fi
}
scenario6() {
  hdr "(6) kubelet probes pass with the policies applied"
  local bad
  bad=$(k -n "$NS" get pods -o json 2>/dev/null | \
    python3 -I -c 'import json,sys
d=json.load(sys.stdin); bad=[]
for p in d["items"]:
    n=p["metadata"]["name"]
    if n.startswith("t-"): continue
    cs=p["status"].get("containerStatuses",[])
    if not cs or not all(c["ready"] for c in cs) or any(c["restartCount"]>0 for c in cs): bad.append(n)
print(",".join(bad))' || echo "parse-error")
  [ -z "$bad" ] && pass "engine / MCP(+nginx) / clickhouse pods: all containers Ready, restartCount=0 (readiness+liveness+startup run against the pods under NP)" || fail "pods not Ready or restarted: $bad"
  local vmready; vmready=$(k -n "$NS" get pod k8s-tailor-vmsingle-0 -o jsonpath='{.status.containerStatuses[0].ready}/{.status.containerStatuses[0].restartCount}')
  [ "$vmready" = "true/0" ] && pass "vmsingle pod Ready, restartCount=0" || fail "vmsingle pod ready/restarts = $vmready"
  # Startup/readiness failures while a process is still booting are "connection refused" / 5xx
  # (the process is not listening yet). A NetworkPolicy DROP shows up as a probe TIMEOUT.
  local ev; ev=$(k -n "$NS" get events --field-selector reason=Unhealthy -o jsonpath='{range .items[*]}{.involvedObject.name}: {.message}{"\n"}{end}' | grep -v '^t-' || true)
  local to; to=$(printf '%s\n' "$ev" | grep -iE 'timeout|deadline exceeded' || true)
  [ -z "$to" ] && pass "no probe TIMEOUT events in $NS (kubelet->pod traffic is never dropped)" || fail "probe timeout events: $to"
  [ -n "$ev" ] && log "info: boot-time probe failures (connection refused / 5xx while starting, not drops): $(printf '%s\n' "$ev" | wc -l | tr -d ' ') events"
  log "restarting every chart pod: fresh start must pass startup/readiness/liveness under the policies"
  # shellcheck disable=SC2046
  k -n "$NS" delete $(k -n "$NS" get pods -o name | grep -v '/t-') --wait=false >/dev/null
  sleep 3
  if k -n "$NS" rollout status deploy/k8s-tailor --timeout=240s >/dev/null && k -n "$NS" rollout status deploy/k8s-tailor-mcp --timeout=240s >/dev/null \
     && k -n "$NS" rollout status sts/k8s-tailor-clickhouse --timeout=300s >/dev/null && k -n "$NS" rollout status sts/k8s-tailor-vmsingle --timeout=240s >/dev/null; then
    pass "after deleting engine, MCP, clickhouse and vmsingle pods all became Ready again under the NetworkPolicies"
  else
    fail "some chart pod did not become Ready after restart with the policies on"
  fi
}
scenario7() {
  hdr "(7) vmsingle -> kubelet scrape still works (container_cpu_usage_seconds_total present)"
  local n=0 q='http://k8s-tailor-vmsingle.'$NS'.svc:8428/api/v1/query?query=count(container_cpu_usage_seconds_total)'
  for _ in $(seq 1 24); do
    n=$(k -n "$NS" exec t-engine -- curl -s -m 5 "$q" 2>/dev/null | python3 -I -c 'import json,sys
try:
    r=json.load(sys.stdin)["data"]["result"]; print(int(float(r[0]["value"][1])) if r else 0)
except Exception: print(0)' || echo 0)
    [ "${n:-0}" -gt 0 ] && break; sleep 5
  done
  [ "${n:-0}" -gt 0 ] && pass "count(container_cpu_usage_seconds_total) = $n series (queried from an engine-labelled pod)" || fail "no container_cpu_usage_seconds_total series after 120s"
  local up; up=$(k -n "$NS" exec t-engine -- curl -s -m 5 "http://k8s-tailor-vmsingle.$NS.svc:8428/api/v1/targets" 2>/dev/null | grep -o '"health":"up"' | wc -l | tr -d ' ')
  log "info: scrape targets up = $up"
}
scenario8() {
  hdr "(8) negative control: networkPolicy.enabled=false -> everything blocked above must now pass"
  helm_install --set networkPolicy.enabled=false
  local np; np=$(k -n "$NS" get netpol --no-headers 2>/dev/null | wc -l | tr -d ' ')
  [ "$np" = 0 ] && pass "no NetworkPolicy objects left in $NS" || fail "$np NetworkPolicy objects still present"
  sleep 10
  expect_reach "engine from np-other/t (control)" np-other t GET "$ENGINE"
  for c in "${BLOCKED[@]}"; do run_case reach "$c"; done
  expect_reach "webhook from np-other/t (control)" np-other t POST "$WH" "$WH_BODY"
}

cmd_test() {
  k get ns >/dev/null
  define_blocked
  log "creating test pods"
  setup_pods
  scenario1; scenario2; scenario3; scenario4
  scenario5
  scenario6
  scenario7
  scenario5b
  scenario8
  hdr "SUMMARY"
  printf 'PASS=%d FAIL=%d  (elapsed %ss)\n' "$PASS" "$FAIL" "$(( $(date +%s) - T0 ))"
  if [ "$FAIL" -gt 0 ]; then printf 'FAILED:\n'; printf '  - %s\n' "${FAILED[@]}"; return 1; fi
}

# Slow extra for scenario 5: needs usage history (MIN_SAMPLES=2 x 300s step) before the engine recommends.
cmd_mutate() {
  hdr "(5+) real webhook mutation with the NetworkPolicies on"
  helm_install   # policies ON, webhookFrom=[] (scenario 8 leaves them OFF)
  local np; np=$(k -n "$NS" get netpol --no-headers | wc -l | tr -d ' ')
  [ "$np" -ge 4 ] && pass "$np NetworkPolicy objects active" || { fail "NetworkPolicies not active ($np)"; return 1; }
  k create ns np-optin >/dev/null 2>&1 || true
  k apply -f - >/dev/null <<'EOF2'
apiVersion: apps/v1
kind: Deployment
metadata: { name: np-burner, namespace: np-optin }
spec:
  replicas: 1
  selector: { matchLabels: { app: np-burner } }
  template:
    metadata: { labels: { app: np-burner } }
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh","-c","while true; do sleep 30; done"]
          resources: { requests: { cpu: "500m", memory: 256Mi }, limits: { memory: 256Mi } }
---
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorPolicy
metadata: { name: np-optin }
spec:
  priority: 50
  mode: Webhook
  selector: { namespaces: ["np-optin"] }
EOF2
  local cpu="" _
  for _ in $(seq 1 90); do  # up to ~1h
    sleep 30
    k -n np-optin delete pod -l app=np-burner --wait=false >/dev/null 2>&1 || true
    sleep 10
    cpu=$(k -n np-optin get pod -l app=np-burner --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].spec.containers[0].resources.requests.cpu}' 2>/dev/null || true)
    [ -n "$cpu" ] && [ "$cpu" != 500m ] && break
  done
  if [ "$cpu" != 500m ] && [ -n "$cpu" ]; then
    pass "pod recreated by the apiserver was MUTATED by the webhook through the NetworkPolicies: cpu request 500m -> $cpu"
    k -n np-optin get pod -l app=np-burner --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.annotations}'; echo
  else
    fail "pod was never mutated within 1h (cpu request still ${cpu:-unknown})"; return 1
  fi
}

cmd_down() { log "deleting cluster $CLUSTER"; k3d cluster delete "$CLUSTER"; }

case "${1:-all}" in
  up) cmd_up ;;
  test) cmd_test ;;
  mutate) cmd_mutate ;;
  down) cmd_down ;;
  all)
    rc=0
    "$0" up || rc=$?
    [ "$rc" = 0 ] && { "$0" test || rc=$?; }
    [ "${KEEP:-0}" = 1 ] || cmd_down
    log "total $(( $(date +%s) - T0 ))s"
    exit "$rc" ;;
  *) echo "usage: $0 [up|test|mutate|down|all]" >&2; exit 2 ;;
esac
