#!/usr/bin/env bash
# Regression test for the MCP auth gate, run against the REAL artifacts:
#   - the nginx sidecar config/command/env rendered by the chart (helm template),
#   - the nginx-unprivileged image the chart pins,
#   - the Bun server from this repo (mcp/server/main.ts) with the env the chart
#     gives it,
# wired like the pod (one network namespace: nginx + Bun on the chart's MCP_HOST),
# plus a fake engine on 127.0.0.1:9090 that answers every call with a marker.
# Everything runs in ONE container (the chart's nginx image + a bun binary), which
# also works on Docker setups that refuse --network container:<id>.
#
# Every path-confusion variant (/..\mcp, /x\..\mcp, //mcp, /%2e%2e/mcp,
# /mcp/../mcp, /MCP, HEAD/OPTIONS...) must answer 4xx WITHOUT reaching the engine
# when no/wrong token is sent; the canonical POST /mcp with the token must reach it.
# Runs twice: chart defaults (nginx sidecar) and mcp.auth.enabled=false (Bun alone).
#
# Needs: docker, helm, python3 + PyYAML, curl; mcp/node_modules installed (`bun install`)
# and, for the UI shell check, `bun run build:web`.
# Usage: mcp/server/e2e/nginx-gate.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
TOKEN=gateE2EToken0123456789abcdefABCD
API_TOKEN=engineApiToken0123456789abcdefAB
PORT=${GATE_PORT:-18088}
BUN_ALPINE=${BUN_ALPINE:-oven/bun:1-alpine}
FAKE_ENGINE='Bun.serve({hostname:"127.0.0.1",port:9090,fetch:(r)=>Response.json({engineReached:true,auth:r.headers.get("authorization")?"present":"absent"})})'
P=tailor-gate-$$
WORK=$(mktemp -d)
FAILS=0

cleanup() {
  docker rm -f "$P-pod" >/dev/null 2>&1 || true
  docker rmi -f "$P-img" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

if [ ! -d "$ROOT/chart/charts" ] || ! ls "$ROOT"/chart/charts/*.tgz >/dev/null 2>&1; then
  helm dependency build "$ROOT/chart" >/dev/null
fi

# Render the chart and extract what the pod would run: the nginx template, the
# nginx image/command/args/env and the Bun env (secretKeyRefs resolved to the
# tokens set above, TAILOR_API pointed at the fake engine).
render() {
  local dir=$1
  shift
  mkdir -p "$dir/tpl"
  helm template t "$ROOT/chart" -n tailor \
    --set "mcp.auth.token=$TOKEN" --set "tailor.apiAuth.token=$API_TOKEN" "$@" >"$dir/out.yaml"
  python3 - "$dir" "$TOKEN" "$API_TOKEN" <<'PY'
import json, sys, yaml
d, tok, api = sys.argv[1:4]
docs = [x for x in yaml.safe_load_all(open(f"{d}/out.yaml")) if x]
secrets = {"k8s-tailor-mcp-auth": tok, "k8s-tailor-api-auth": api}
def envfile(c, path, extra):
    lines = []
    for e in c.get("env", []):
        if "value" in e:
            v = e["value"]
        elif "secretKeyRef" in e.get("valueFrom", {}):
            v = secrets[e["valueFrom"]["secretKeyRef"]["name"]]
        else:
            continue
        lines.append((e["name"], v))
    lines = [(k, v) for k, v in lines if k not in extra] + list(extra.items())
    open(path, "w").write("".join(f"{k}={v}\n" for k, v in lines))
for x in docs:
    if x["kind"] == "ConfigMap" and x["metadata"]["name"] == "k8s-tailor-mcp-nginx":
        for k, v in x["data"].items():
            open(f"{d}/tpl/{k}", "w").write(v)
    if x["kind"] == "Deployment" and x["metadata"]["name"] == "k8s-tailor-mcp":
        for c in x["spec"]["template"]["spec"]["containers"]:
            if c["name"] == "mcp":
                envfile(c, f"{d}/bun.env", {"TAILOR_API": "http://127.0.0.1:9090", "HOME": "/tmp"})
            if c["name"] == "nginx-auth":
                envfile(c, f"{d}/nginx.env", {})
                json.dump({"image": c["image"], "command": c.get("command", []), "args": c.get("args", [])},
                          open(f"{d}/nginx.json", "w"))
PY
}

# code METHOD PATH [TOKEN] -> prints "<status> <engine-reached 0|1>"
code() {
  local m=$1 path=$2 tok=${3:-} body hdrs=(-H "Content-Type: application/json")
  [ -n "$tok" ] && hdrs+=(-H "Authorization: Bearer $tok")
  [ -n "${ORIGIN:-}" ] && hdrs+=(-H "Origin: $ORIGIN")
  body='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"TAILOR_HEALTH","arguments":{}}}'
  local args=(-s --path-as-is -o "$WORK/body" -w '%{http_code}' --max-time 10 "${hdrs[@]}")
  case $m in
    HEAD) args+=(-I) ;;
    GET) ;;
    *) args+=(-X "$m" --data "$body") ;;
  esac
  local st
  st=$(curl "${args[@]}" "http://127.0.0.1:$PORT$path" || echo 000)
  if grep -q engineReached "$WORK/body" 2>/dev/null; then echo "$st 1"; else echo "$st 0"; fi
}

expect_blocked() { # METHOD PATH [TOKEN]
  local r
  r=$(code "$@")
  local st=${r% *} reached=${r#* }
  if [ "$reached" = 1 ] || [ "${st:0:1}" != 4 ]; then
    echo "  FAIL  $1 $2 ${3:+(Bearer ${3:0:8}...)} -> HTTP $st engineReached=$reached (expected 4xx, engine untouched)"
    FAILS=$((FAILS + 1))
  else
    echo "  ok    $1 $2 ${3:+(Bearer ${3:0:8}...)} -> $st"
  fi
}

expect_open() { # METHOD PATH WANT
  local r
  r=$(code "$1" "$2")
  local st=${r% *}
  if [ "$st" != "$3" ]; then
    echo "  FAIL  $1 $2 -> HTTP $st (expected $3)"
    FAILS=$((FAILS + 1))
  else
    echo "  ok    $1 $2 -> $st"
  fi
}

expect_reached() { # METHOD PATH TOKEN
  local r
  r=$(code "$@")
  if [ "$r" != "200 1" ]; then
    echo "  FAIL  $1 $2 ${ORIGIN:+Origin=$ORIGIN }(token) -> $r (expected 200 and the engine reached)"
    FAILS=$((FAILS + 1))
  else
    echo "  ok    $1 $2 ${ORIGIN:+Origin=$ORIGIN }(token) -> 200, engine reached"
  fi
}

wait_up() {
  for _ in $(seq 1 60); do
    if curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$PORT/healthz"; then return 0; fi
    sleep 0.5
  done
  echo "server did not come up"; docker logs "$P-pod" 2>&1 | tail -30
  return 1
}

run_case() {
  local name=$1 sidecar=$2
  shift 2
  local dir="$WORK/$name"
  echo "== $name"
  render "$dir" "$@"

  # One container = the pod's network namespace: the chart's nginx image (sidecar)
  # with a bun binary added runs the fake engine, Bun on the chart's MCP_HOST and,
  # in the sidecar case, nginx with the chart's command and template.
  local img podport=8020 cmd=()
  img=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["image"])' "$WORK/sidecar/nginx.json")
  docker build -q -t "$P-img" - >/dev/null <<EOF_D
FROM $BUN_ALPINE AS bun
FROM $img
COPY --from=bun /usr/lib/libstdc++.so.6 /usr/lib/libgcc_s.so.1 /usr/lib/
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
EOF_D
  local main='exec bun run server/main.ts'
  : >"$dir/env"
  cat "$dir/bun.env" >>"$dir/env"
  if [ "$sidecar" = 1 ]; then
    podport=8080
    cat "$dir/nginx.env" >>"$dir/env"
    while IFS= read -r -d '' arg; do cmd+=("$arg"); done < <(python3 -c 'import json,sys;j=json.load(open(sys.argv[1]));[print(a, end="\0") for a in j["command"]+j["args"]]' "$dir/nginx.json")
    [ "${#cmd[@]}" -eq 0 ] && cmd=(/docker-entrypoint.sh nginx -g "daemon off;")
    main='bun run server/main.ts & exec "$@"'
  fi
  # Templates are copied in (not bind-mounted): $WORK may live outside the paths a
  # Docker VM shares (macOS /var/folders); the repo itself must be shared.
  docker create --name "$P-pod" -p "127.0.0.1:$PORT:$podport" --env-file "$dir/env" \
    -v "$ROOT/mcp:/app:ro" --entrypoint /bin/sh "$P-img" -c \
    "cd /app && bun -e '$FAKE_ENGINE' & cd /app && $main" sh "${cmd[@]}" >/dev/null
  if [ "$sidecar" = 1 ]; then docker cp "$dir/tpl/." "$P-pod:/etc/nginx/templates" >/dev/null; fi
  docker start "$P-pod" >/dev/null
  wait_up

  echo "-- without a token (must never reach the engine)"
  for path in '/mcp' '/..\mcp' '/x\..\mcp' '/x/..\mcp' '//mcp' '/%2e%2e/mcp' '/%2E%2E/mcp' \
    '/mcp/../mcp' '/./mcp' '/MCP' '/Mcp' '/mcp/' '/mcp?x=1' '/%2fmcp' '/%5cmcp' '/..%5cmcp' \
    '/index.html/../mcp' '/healthz/../mcp' '/' '/index.html' '/anything'; do
    expect_blocked POST "$path"
  done
  for m in HEAD OPTIONS GET PUT DELETE PATCH; do expect_blocked "$m" /mcp; done
  expect_blocked POST /mcp wrong-token-0123456789abcdefABCD
  expect_blocked POST '/..\mcp' wrong-token-0123456789abcdefABCD

  echo "-- with the token: only the canonical path reaches the engine"
  expect_reached POST /mcp "$TOKEN"
  for path in '/..\mcp' '/x\..\mcp' '/%2e%2e/mcp' '/mcp/../mcp' '/MCP' '/%5cmcp'; do
    expect_blocked POST "$path" "$TOKEN"
  done

  echo "-- browser origin (CSRF): same origin passes, foreign origin refused"
  ORIGIN="http://127.0.0.1:$PORT" expect_reached POST /mcp "$TOKEN"
  ORIGIN="https://evil.example" expect_blocked POST /mcp "$TOKEN"
  ORIGIN="null" expect_blocked POST /mcp "$TOKEN"

  echo "-- open surface"
  expect_open GET /healthz 200
  expect_open GET /readyz 200
  expect_open GET / 200

  echo "-- logs never carry the token or the query string"
  code GET "/?token=$TOKEN&x=leak" >/dev/null
  sleep 0.5
  if docker logs "$P-pod" 2>&1 | grep -q -e "$TOKEN" -e "x=leak"; then
    echo "  FAIL  token or query string found in the container log:"
    docker logs "$P-pod" 2>&1 | grep -e "$TOKEN" -e "x=leak" | head -5
    FAILS=$((FAILS + 1))
  else
    echo "  ok    no token / query string in the log"
  fi

  docker rm -f "$P-pod" >/dev/null 2>&1 || true
}

# The sidecar refuses to start on a token nginx can't embed (existingSecret
# values are not visible to the chart's render-time check).
unsafe_token() {
  local script
  script=$(python3 -c 'import json,sys;j=json.load(open(sys.argv[1]));print(j["args"][0] if j["args"] else "")' "$WORK/sidecar/nginx.json")
  echo "== sidecar refuses unsafe tokens"
  for t in 'a"b' 'a;b' 'a$b' 'a b' 'a}b'; do
    if docker run --rm -e "TOKEN=$t" --entrypoint /bin/sh "$1" -ec "$script" >/dev/null 2>&1; then
      echo "  FAIL  token [$t] accepted"
      FAILS=$((FAILS + 1))
    else
      echo "  ok    token [$t] refused"
    fi
  done
}

run_case sidecar 1
unsafe_token "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["image"])' "$WORK/sidecar/nginx.json")"
run_case bun-only 0 --set mcp.auth.enabled=false

if [ "$FAILS" -gt 0 ]; then
  echo "$FAILS FAILED"
  exit 1
fi
echo "all gate checks passed"
