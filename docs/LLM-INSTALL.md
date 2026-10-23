# Runbook: installing K8s Tailor on a real cluster (for an LLM agent)

Audience: an LLM agent (or a human following it literally) with `kubectl` and `helm` access to a cluster and a checkout of this repository. The runbook is deterministic: every step has a command, an expected result and a
stop condition. If an expected result does not match, **stop and report**; do not improvise.

Scope: Kubernetes 1.33 or later; the project suites run on k3s v1.34 (other conformant distributions use the same APIs; report differences instead of working around them). The product ships inert: installing it changes no workload.
Human-oriented version with more context: [INSTALL.md](INSTALL.md). Variables used below: `NS` (release namespace, default `k8s-tailor`), `REL` (release name, default `k8s-tailor`).

## 0. Hard rules

NEVER, without an explicit, current confirmation from the human in this conversation:

1. Create or edit a `TailorPolicy`, `TailorConfig.defaultMode`, or a workload annotation whose mode is `InPlace`, `Both` or `Webhook` (this is what turns actuation on).
2. Enable `tailor.webhook.enabled`, set `tailor.actuateNamespaces`, or change `failurePolicy`/webhook configuration.
3. Set `tailor.apiAuth.enabled=false`, `mcp.auth.enabled=false`, `mcp.auth.allowUnauthenticated=true`, or `networkPolicy.enabled=false` on anything but a throwaway local cluster.
4. Target `kube-system`, `kube-public`, `kube-node-lease`, mesh, GitOps, cert-manager, monitoring or any other system namespace, or remove entries from `tailor.excludeNamespaces`.
5. Print, log or paste a token or password into chat or files. Show only where it is stored.
6. Run `helm uninstall`, delete CRDs, PVCs or namespaces, or `kubectl delete` anything you did not create in this run.
7. Install on a production cluster without being told it is acceptable and which kube-context to use. Always pass `--kube-context` explicitly.
8. Edit Go code, `chart/` or `docs/` of this repository as part of an install.

Always allowed: read-only `kubectl get/describe/logs`, `helm template`, `helm lint`, `helm status`, `helm get values`.

## 1. Ask the human first (do not assume)

Ask these and wait for the answers. Record them as variables.

| # | Question | Default if they say "you choose" | Variable |
|---|---|---|---|
| 1 | Which kube-context and cluster? Is it production? | none: must be given | `CTX` |
| 2 | Release namespace? | `k8s-tailor` | `NS` |
| 3 | Images: use `ghcr.io/nicacioliveira/*:v0.1.0` or your own registry (repo, tag, pull secret)? | verify GHCR first (step 3), else ask | `IMG_*` |
| 4 | Metrics: bundled VictoriaMetrics, or an existing Prometheus-compatible URL that holds only this cluster's series? | bundled | `VM_URL` |
| 5 | Decision history: bundled ClickHouse, existing ClickHouse URL, or none? | bundled | `CH_*` |
| 6 | Secrets: let the chart generate them, or use existing Secrets (names and keys)? GitOps tool (Argo CD, Flux)? | chart-generated, Helm CLI | |
| 7 | How will people reach the UI: port-forward only, or an Ingress (which controller namespace)? | port-forward | `INGRESS_NS` |
| 8 | Actuation: install **inert (recommend-only)**? Which namespaces, if any, may later be set to `InPlace`, and with which `direction`? | install inert; no actuation namespaces | |
| 9 | Webhook (admission-time sizing)? Only for ephemeral-pod namespaces. | no | |
| 10 | Is cert-manager present and should it be used for the webhook certificate? Only asked if question 9 is yes. | | |

If the answer to 8 is anything other than "inert", still install inert first, complete section 6, and only then ask for explicit confirmation before section 7.

## 2. Preflight (read-only)

Run each command; compare with "Expect". Stop on any mismatch.

```bash
CTX=<kube-context>; NS=k8s-tailor; REL=k8s-tailor
K="kubectl --context $CTX"
```

| # | Command | Expect | If not |
|---|---|---|---|
| P1 | `$K cluster-info` | prints the control plane URL | wrong context or no access: stop |
| P2 | `$K version -o json` | `serverVersion.minor` is `33` or higher (gitVersion `v1.33.x`+) | below 1.33: STOP and ask the human (the resize feature gate would have to be enabled on apiserver and kubelets, unsupported here) |
| P3 | `$K get --raw /api/v1 \| grep -c '"name":"pods/resize"'` | `1` | `0`: in-place resize is unavailable. STOP and ask (the product would only be able to do rollouts) |
| P4 | `helm version --short` | `v3.x` | install Helm 3: stop and ask |
| P5 | `$K get storageclass` | at least one line with `(default)`, or the human gave a storage class | none: ask for `storageClassName` for both `clickhouse` and `victoria-metrics-single.server.persistentVolume` |
| P6 | `$K get nodes -o wide` | all `Ready`; note arch (`amd64`/`arm64`) | NotReady nodes: report |
| P7 | `$K get crd \| grep tailor.k8s-tailor.dev` | empty for a first install | non-empty: an earlier install exists; read [UPGRADE.md](UPGRADE.md) and ask the human before continuing |
| P8 | `helm --kube-context $CTX list -A \| grep -i tailor` | empty | non-empty: this is an upgrade, not an install: ask |
| P9 | `$K get ns $NS` | `NotFound` (or an empty namespace you are allowed to use) | exists with other content: ask |
| P10 | **CNI with NetworkPolicy enforcement.** `$K -n kube-system get pods -o name` and identify the CNI (names such as `calico-node`, `cilium`, `antrea`, `kube-router`; on managed clusters also check the provider's network-policy add-on setting). Also `$K get networkpolicy -A` is not proof of anything: a policy object is accepted even when nothing enforces it | a CNI that enforces NetworkPolicy. The chart's Calico suite (`dev/calico/README.md`) checks block and allow behaviour; for any other CNI confirm enforcement with a deny-all test policy (it must produce a connect timeout) | none identified, or only a CNI that does not enforce (for example plain flannel): **STOP and ask the human.** Explain that the chart's NetworkPolicies would be no-ops, so the engine's unauthenticated read API (`:8080`), vmsingle (`:8428`, accepts writes) and ClickHouse would be reachable from any pod. Options for the human: enable policy enforcement first, or accept the risk explicitly. Do NOT set `networkPolicy.enabled=false` yourself (hard rule 3), and do not claim the install is isolated. Multi-node: if `networkPolicy.webhookFrom` is used, it must list the node/tunnel addresses the API server calls from, otherwise the webhook silently fails open |
| P11 | `$K auth can-i '*' '*' --all-namespaces` | `yes` | `no`: the install needs cluster-admin (CRDs, ClusterRoles); stop and ask |

## 3. Decision tree

```text
cert-manager installed?   $K get crd certificates.cert-manager.io          (exists -> yes)
  webhook not requested ................ ignore cert-manager
  webhook requested + cert-manager yes . tailor.webhook.certMode=certManager
  webhook requested + cert-manager no .. tailor.webhook.certMode=existingSecret (human supplies tls Secret + caBundle) or ask to install cert-manager. NEVER certMode=helm under GitOps.

Existing Prometheus / VictoriaMetrics to reuse?
  human gave VM_URL .................... --set victoria-metrics-single.enabled=false --set tailor.env.VM_URL=<url>
                                         then check the two series exist (step 5.4) and that the store holds only this cluster
  no ................................... bundled VictoriaMetrics (default)

Istio / Linkerd present?   $K get ns istio-system linkerd 2>/dev/null
  yes .................................. nothing to install. Sidecars are recommend-only until opted in. Tell the human; read docs/CONTAINERS.md.
  (If the webhook is requested, keep the chart's `reinvocationPolicy: IfNeeded`: it is what lets the webhook see a sidecar injected by the mesh. With real Istio 1.31.3 the API server called the webhook twice per pod regardless of its name. Istio classic and native sidecars were tested; Linkerd, Istio ambient and Knative were not.)

GitOps (Argo CD / Flux) in use?
  yes .................................. render uses `helm template`: every secret MUST be an existingSecret, set secretGeneration.failWithoutLookup=true; see deploy/argocd/
  no ................................... Helm CLI install (below)

Ingress requested?
  yes .................................. networkPolicy.mcpFrom must list the ingress controller namespace; TLS + SSO must be in front (the chart does not provide them)
  no ................................... port-forward only

GHCR images pullable?   docker manifest inspect ghcr.io/nicacioliveira/k8s-tailor:v0.1.0   (exit 0 -> yes)
  no / unknown ......................... the default images do not exist yet (private repo, no v0.1.0 tag). Do NOT publish them yourself (tag/push/workflow run is the owner's action).
                                         Ask the human which: (a) the owner publishes + packages made public or `global.imagePullSecrets`; (b) local build + `k3d image import` / `kind load docker-image`
                                         (see docs/UPGRADE.md#images and INSTALL.md section 4)
```

## 4. Install steps (idempotent)

Run from the repository root. Each step can be repeated safely.

1. **Dependencies** (required on a clean clone; `chart/charts/` is git-ignored):
   ```bash
   helm dependency build chart
   ```
   Expect: `Saving 1 charts` / `Downloading victoria-metrics-single` and the file `chart/charts/victoria-metrics-single-0.43.0.tgz`.
2. **Namespace** (idempotent):
   ```bash
   $K create namespace $NS --dry-run=client -o yaml | $K apply -f -
   ```
3. **Secrets** only if the human chose existing Secrets and asked you to create them. Create them without echoing values:
   ```bash
   $K -n $NS get secret k8s-tailor >/dev/null 2>&1 || $K -n $NS create secret generic k8s-tailor \
     --from-literal=api-token="$(openssl rand -hex 32)" \
     --from-literal=mcp-token="$(openssl rand -hex 32)" \
     --from-literal=clickhouse-password="$(openssl rand -hex 32)"
   ```
   Otherwise skip: the chart generates `k8s-tailor-api-auth`, `k8s-tailor-mcp-auth`, `k8s-tailor-clickhouse-auth` once and reuses them on upgrade.
4. **Values file** `values-install.yaml` (outside the repo, never committed). Start from this and apply the decisions of section 1 and 3:
   ```yaml
   tailor:
     image: { repository: <repo>, tag: <tag> }
     webhook: { enabled: false }
   mcp:
     image: { repository: <repo>, tag: <tag> }
   networkPolicy:
     enabled: true
     mcpFrom: []                 # add the ingress controller namespace selector only if an Ingress was requested
   ```
   Add `existingSecret` entries (`tailor.apiAuth`, `mcp.auth`, `clickhouse.auth`) when Secrets were created in step 3; add `storageClassName` when needed. Do **not** change `MIN_SAMPLES` (default `288`).
5. **Validate the render** (no cluster changes):
   ```bash
   helm lint chart -f values-install.yaml
   helm template $REL chart -n $NS -f values-install.yaml > /tmp/rendered.yaml
   ```
   Expect: `1 chart(s) linted, 0 chart(s) failed`; `/tmp/rendered.yaml` non-empty. Under `helm template` with the default `certMode: helm` and the webhook disabled, no failure is expected.
6. **Server-side dry run** (optional but recommended): `helm upgrade --install $REL chart -n $NS -f values-install.yaml --dry-run=server --kube-context $CTX`. Expect a rendered manifest and no error.
7. **Install** (only after the human confirmed context and values):
   ```bash
   helm upgrade --install $REL chart --kube-context $CTX -n $NS -f values-install.yaml --wait --timeout 10m
   ```
   Expect: `STATUS: deployed` and the chart NOTES ("installed in namespace ... inert until a TailorPolicy enables actuation"). On timeout: run section 7 diagnostics, report, do not delete anything.

## 5. Success criteria (all must hold)

| # | Check | Command | Expected |
|---|---|---|---|
| S1 | Release deployed | `helm --kube-context $CTX -n $NS status $REL -o json \| grep -o '"status":"deployed"'` | `"status":"deployed"` |
| S2 | Pods Ready | `$K -n $NS get pods` | engine `1/1 Running`; `k8s-tailor-mcp-...` `2/2 Running` (`1/1` if `mcp.auth.enabled=false`); `k8s-tailor-clickhouse-0` `1/1`; a `k8s-tailor-vmsingle...` pod `1/1` (when bundled) |
| S3 | CRDs | `$K get crd \| grep -c tailor.k8s-tailor.dev` | `4` |
| S4 | Config singleton | `$K get tailorconfig cluster -o jsonpath='{.spec.paused}{" "}{.spec.defaultMode}'` | `false Recommend` |
| S5 | Engine health | `$K get --raw /api/v1/namespaces/$NS/services/k8s-tailor:8080/proxy/healthz` | JSON with `"ok":true`, `"vmHealthy":true`, `"actuation":"dry-run"` (if the proxy is blocked by NetworkPolicy, use `$K -n $NS port-forward svc/k8s-tailor 8080:8080` and `curl localhost:8080/healthz`) |
| S6 | Metrics flowing | `$K get --raw "/api/v1/namespaces/$NS/services/k8s-tailor-vmsingle:8428/proxy/api/v1/query?query=count(container_cpu_usage_seconds_total)"` | `"status":"success"` with a non-empty `result` (may take up to a few minutes after install; retry every 30 s for 10 min) |
| S7 | Engine refreshing | `$K -n $NS logs deploy/$REL --tail=100 \| grep -c 'refresh ok'` (deployment is named `k8s-tailor`) | `>= 1` |
| S8 | Leader elected | `$K -n $NS get lease k8s-tailor-actuator -o jsonpath='{.spec.holderIdentity}'` | an engine pod name (when `tailor.leaderElection.enabled`, the default) |
| S9 | Inert | `$K get rsp` | `No resources found` (no TailorPolicy), and the S5 JSON shows `"actuation":"dry-run"` |
| S10 | Recommendations exist | `$K -n $NS port-forward svc/k8s-tailor 8080:8080 &` then `curl -s localhost:8080/api/recommendations \| grep -o '"count":[0-9]*'` | `"count":N` with `N > 0` after a couple of refresh cycles; workloads read as "learning" until `MIN_SAMPLES` (24 h by default) |

Stop port-forwards you started (`kill %1`). Report each S1-S10 result to the human.

## 6. Hand-off (still inert)

Tell the human, in plain words: how to open the UI (`kubectl --context $CTX -n $NS port-forward svc/k8s-tailor-mcp 8020:8020`, then `http://localhost:8020`), **where** the tokens are
(`Secret k8s-tailor-mcp-auth` key `token` and `Secret k8s-tailor-api-auth` key `token`, or their own Secrets; do not print them), that nothing is actuated yet, and that new workloads need about 24 hours of data before any recommendation
may be applied. Suggest reviewing Workloads and Savings for a day.

## 7. Enabling actuation (only on explicit confirmation)

Preconditions: sections 5 and 6 done, the human reviewed recommendations, and they named the namespaces. Then, **one namespace first**, shrink-only:

```bash
$K apply -f - <<'EOF'
apiVersion: tailor.k8s-tailor.dev/v1alpha1
kind: TailorPolicy
metadata: { name: pilot }
spec:
  priority: 100
  selector: { namespaces: ["<ONE-NAMESPACE-NAMED-BY-HUMAN>"] }
  mode: InPlace
  apply: InPlace
  direction: Down
EOF
```

Verify: `$K get events -n <ns> --field-selector reason=Rightsized` shows entries; `$K -n <ns> get pods -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}'` is unchanged; `/healthz` shows `"actuation":"active"`.
Widening (more namespaces, `direction: Both`, the webhook) needs a new confirmation each time.

## 8. Rollback and stop

| Goal | Command | Notes |
|---|---|---|
| Stop all actuation now | `$K patch tailorconfig cluster --type=merge -p '{"spec":{"paused":true}}'` | Stops reconciler and webhook. Already-applied sizes stay. Reverse with `false`. |
| Remove one policy | `$K delete tailorpolicy <name>` | Back to default mode (`Recommend`). |
| Revert the Helm release | `helm --kube-context $CTX -n $NS history $REL` then `helm --kube-context $CTX -n $NS rollback $REL <rev>` | Needs human confirmation. |
| Uninstall | see [INSTALL.md section 11](INSTALL.md#11-uninstall-completely) | Needs explicit human confirmation: it deletes data if PVCs/CRDs are removed. |

## 9. Failure matrix

| Symptom | Likely cause | Next action |
|---|---|---|
| `helm dependency build` fails | no network to `victoriametrics.github.io` | report; ask for a chart mirror |
| `helm template` fails with `lookup` or `certMode=helm` text | GitOps-style render | use `existingSecret`s and `certManager`; read section 3 |
| `ImagePullBackOff` | the default GHCR images are not published, or are private | docs/UPGRADE.md#images: ask for registry coordinates or use a local build + `k3d image import`/`kind load`; do not tag, push or publish without the owner's confirmation |
| MCP `CrashLoopBackOff`, "refusing to start ... MCP_API_TOKEN" | no token reached Bun (`mcp.auth.enabled=false` without a token), or an `existingSecret` token with characters outside `A-Z a-z 0-9 . _ ~ + / = -` | restore `mcp.auth.enabled=true`; for an `existingSecret`, regenerate the token with `openssl rand -hex 32` |
| PVC `Pending` | no default StorageClass | ask for `storageClassName` |
| S6 stays empty | kubelet metrics not scraped (RBAC `nodes/proxy`, CNI, or a custom store) | `$K -n $NS logs` the vmsingle pod; report; see [TROUBLESHOOTING.md](TROUBLESHOOTING.md) |
| Everything else | | [TROUBLESHOOTING.md](TROUBLESHOOTING.md) |
