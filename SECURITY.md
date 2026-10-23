# Security

## Reporting a vulnerability

Please report privately through GitHub's "Report a vulnerability" (Security tab of
`nicacioliveira/k8s-tailor`) or by email to the maintainer listed in the git history. Do not open a
public issue. Expect an acknowledgement within a few days; there is no SLA. Only the latest release / `master` is supported.

## Threat model (chart 0.2.0)

K8s Tailor is a **privileged, cluster-scoped controller**: it can resize pods and patch workloads
in every namespace it observes. The main risks are someone driving that power (write API, MCP)
and the engine being a high-value target. Upgrade notes: [docs/UPGRADE.md](docs/UPGRADE.md).

| Asset / surface | Risk | Control (default unless noted) | Residual gap |
|---|---|---|---|
| Engine **write API** (`POST/PUT/PATCH/DELETE /api/*`: pause, policies, groups, workload mode, advisor) | Anyone with network reach enables actuation (e.g. a policy with `InPlace` and `memory.max: 4Mi`) | **Bearer token required** (`tailor.apiAuth.enabled=true`): generated once into `k8s-tailor-api-auth` (lookup + `resource-policy: keep`) or `existingSecret`; constant-time compare; `TAILOR_API_AUTH_REQUIRED` fails closed (503) without a token. **CSRF**: bodies must be `application/json` (415), cross-site `Origin`/`Sec-Fetch-Site` refused (403). **DNS rebinding**: a browser write must target an expected `Host` (`tailor.allowedHosts`; with the write API open, loopback names only) — 403. Bodies capped at 1 MiB (413). Input validated (400) with the same bounds as the CRD schema. | Single shared static token, no per-user identity or audit of *who* (the Kubernetes audit log only sees the engine SA). No TLS on `:8080` (cluster-internal). |
| Engine **read API** (`GET/HEAD /api/*`), `/metrics` | Workload/namespace inventory and sizing disclosure | NetworkPolicy: `:8080` only from the MCP pods, vmsingle and `networkPolicy.metricsFrom` (blocking checked by the Calico suite; see below). **Optional read auth** (`tailor.apiAuth.protectReads=true` → `TAILOR_API_PROTECT_READS=true`, off by default for compatibility; needs `apiAuth.enabled`): every `GET/HEAD /api/*` needs the same Bearer token as the writes (constant-time compare, 401 + `WWW-Authenticate`; 503 fail-closed when the engine has no token). The MCP server already forwards the token on reads; the embedded UI on `:8080` asks for it in its token dialog on the first 401. | Off by default. Never gated, by design: `/healthz` and `/livez` (kubelet probes carry no token), `/metrics` (the bundled vmsingle scrape carries no token; it exposes namespace/workload/container names and sizes, so keep the NetworkPolicy) and the UI shell `/` (static, no data). Without an enforcing CNI the NetworkPolicy does nothing; `kubectl port-forward` bypasses it (with `protectReads` the API still needs the token there). |
| **HTTP servers** (API `:8080`, webhook `:8443`) | Slowloris / resource exhaustion | `ReadHeaderTimeout` 10s, `ReadTimeout` 30s, `WriteTimeout` 3m (advisor calls), `IdleTimeout` 2m, 64 KiB headers | No per-client rate limit except the advisor's call budget. |
| **MCP / dashboard** (`k8s-tailor-mcp:8020`) | `/mcp` forwards the engine token: an open `/mcp` = an open write API. Path confusion between a path-based proxy and the server | **Two independent Bearer checks.** (1) nginx sidecar (`mcp.auth.enabled`), **deny-by-default**: only `= /`, `= /index.html`, `= /healthz`, `= /readyz` (GET) are open, every other path and method needs `Bearer <mcp token>` (exact-match `map`, no string interpolation in `if`). (2) Bun **always** requires `MCP_API_TOKEN` (the chart injects the same token; constant-time, hashed compare) and refuses to start without it; it routes on the **raw** request-target and answers **400** to any non-canonical path (`\`, any `%`-escape, `//`, `.`/`..` segments, absolute-form). Bun binds 127.0.0.1 behind the sidecar. `/mcp`: POST + `application/json` only (415), foreign/`null` `Origin` refused (403), `Host` allowlist (`mcp.allowedHosts`; loopback-only when open), body ≤ 1 MiB, JSON-RPC batch ≤ 20, body deadline 30 s. The token charset is restricted to RFC 6750 token68 (`A-Z a-z 0-9 . _ ~ + / = -`): the render fails otherwise, and the sidecar and Bun refuse to start on an `existingSecret` with other characters. nginx access log = method, path **without query**, status (JSON-escaped; never headers); error log at `error` level. NetworkPolicy: release namespace + `networkPolicy.mcpFrom`. No ServiceAccount token in the pod. Regression test: `mcp/server/e2e/nginx-gate.sh` (CI job `mcp-gate`). | Static shared token, no per-user identity. The UI shell at `/` is public (no data). nginx's `error`-level lines still include the request line. Put TLS + SSO (oauth2-proxy / ingress auth) in front for anything beyond port-forward. `mcp.auth.allowUnauthenticated` = open `/mcp` (dev only). |
| **Token handling in the browser** | Token leaking via URLs, logs, framing | Stored in `sessionStorage` (localStorage only on "remember"). A link may carry it **only in the fragment** (`#token=` / `#/<view>?token=`), which never reaches a server or log; `?token=` is ignored and stripped. The shell is served with `X-Frame-Options: DENY`, `frame-ancestors 'none'`, `no-store`, `Referrer-Policy: no-referrer`. | Any XSS in the dashboard could read the stored token. |
| **RBAC** of the engine SA (ClusterRole/Binding `k8s-tailor-engine`) | Compromise = cluster-wide resize/patch | Least privilege, every verb used by the code: pods list/patch, `pods/resize` patch, events create, apps workloads list/patch, replicasets get/list, PDB/HPA list, ksvc get/list/update, Tailor CRs. No nodes or limitranges unless the opt-in `tailor.rbac.clusterLookups` adds `list` on both (read only), no watch, no `update` on pods/workloads, no secrets. Leases, and the warm-cache ConfigMap `k8s-tailor-warm` (create; get/update on that name), only in the release namespace. | A `patch` on workloads can in principle change anything in the object — see the next row. |
| **Admission scope** of the engine SA (`tailor.admissionPolicy.enabled=true`, k8s >= 1.30, opt-in) | A compromised engine / stolen API token re-deploys workloads (image, command, env, hostPath, privileged, serviceAccountName...) | ValidatingAdmissionPolicies on UPDATEs **by the Tailor SA**: on Deployments/StatefulSets/DaemonSets the whole spec is frozen except each container's / initContainer's `resources` (field-by-field, so fields added by future Kubernetes versions are frozen too); pod-template metadata frozen except annotations, which may change only for `tailor.k8s-tailor.dev/*` and the **8 exact** mesh proxy-sizing keys the engine writes (`sidecar.istio.io/proxy{CPU,Memory,CPULimit,MemoryLimit}`, `config.linkerd.io/proxy-{cpu,memory}-{request,limit}` — not `proxyImage`/`proxy-image`/`proxy-version`); workload labels, ownerReferences, finalizers frozen; workload annotations only `tailor.*`. Pods: `spec` frozen entirely (in-place resizes use the `pods/resize` subresource, which is not matched), only `tailor.*` annotations. Verified against a real API server (CEL) with an allow/deny matrix. | Knative Services (whole-object `update`) are not covered. Raising `resources` itself is allowed by design (a resize can still starve a node or OOM a pod — the policy bounds what the engine can *change*, not how well it sizes). |
| **vmsingle** (bundled VictoriaMetrics) | No authentication; holds the CPU/memory usage of every pod in the cluster, and accepts writes (`/api/v1/import`): a pod able to write could inject fake usage and drive recommendations down (OOM) | NetworkPolicy `k8s-tailor-vmsingle`: `:8428` only from the engine pods and `networkPolicy.metricsFrom`. Own ServiceAccount `k8s-tailor-vmsingle` with only nodes get/list/watch + `nodes/proxy` get (kubelet `/metrics/resource` and `/metrics/cadvisor`, the latter reduced to the two CFS counters at scrape time) and, for the per-pod scrape of the engine, pods get/list/watch in the release namespace only (Role `k8s-tailor-vmsingle-pods`); subchart's broader ClusterRole disabled. | Peers in `metricsFrom` can also write. The engine's queries are pinned to `job="kubelet-resource"` (`tailor.vmJob` / `VM_JOB`), so a series pushed under another scrape job is ignored; the pin does **not** stop a direct write to `/api/v1/import` that labels itself `job="kubelet-resource"`, which is why the NetworkPolicy is the real control. `nodes/proxy` get is still powerful (kubelet read endpoints). |
| **Self-actuation** | The Tailor resizes itself, its ClickHouse or vmsingle, or a mesh control plane | Release namespace always excluded (chart + engine runtime `POD_NAMESPACE`, even if the TailorConfig CR omits it); default list covers kube-*, istio-*, linkerd*, cert-manager, knative, gatekeeper, kyverno, external-secrets, argocd, flux-system, monitoring. `ignore=true` annotation is absolute (also against mode or exclusion writes from the UI or `POST /api/workload`). | The TailorConfig CR is editable by anyone with RBAC on it. |
| **Mutating webhook** | Broken/malicious webhook affects pod creation | Opt-in, namespace allow-list, `failurePolicy: Ignore`, 5s timeout, only cpu/memory. `/mutate` patches only a CREATE of `v1/Pod` without subresource (anything else is admitted unchanged) and **records nothing at admission**: the patch carries the `tailor.k8s-tailor.dev/admission-sized` mark and the leader's reconciler records the decision later, once per pod, only if the mark matches the requests the pod really has. Cert hot-reloaded. `certMode=helm` **fails the render** without a live API (no silent CA rotation under GitOps); `certManager` recommended. | `/mutate` has no caller authentication: anyone reaching `:8443` can POST an AdmissionReview and read the computed sizing for a workload (plus owner lookups). A forged review no longer writes the decision log or `tailor_mutations_total`; what remains is a hand-made pod carrying a forged `admission-sized` mark, which is recorded only if the mark agrees with the pod's real requests (and that needs permission to create pods). Restrict the port with `networkPolicy.webhookFrom` (control-plane CIDR); without it `:8443` is open to all sources. |
| **Secrets in GitOps** | `helm template` can't `lookup`: generated tokens/passwords change on every render | `existingSecret` for apiAuth / mcp.auth / clickhouse.auth; `secretGeneration.failWithoutLookup=true` fails the render when one is missing (set in `deploy/argocd`) | Default is off so `helm template`/lint work out of the box. |
| **ClickHouse** | Decision history readable or tamperable | Random password (stable via lookup/keep or `existingSecret`), NetworkPolicy from the engine only. PVC. | — |
| **CRDs** | Schema drift, invalid values | CRDs are templates (upgraded with the release, kept on uninstall); OpenAPI bounds + CEL (`limitFactor` 0 or >= 1, quantiles, margins, durations, HH:MM) | — |
| **Supply chain** | Tampered or missing images | Distroless nonroot engine, lockfiles, Dependabot; chart defaults to GHCR `v<appVersion>` (published by `build-images.yml` for the tag `v<appVersion>` only — the workflow refuses a tag that disagrees with the chart), supports `digest` pinning and `global.imagePullSecrets` for private registries / private GHCR packages. | Images are not signed; no SBOM/provenance attestation yet. Until a release is published the default images do not exist (see docs/UPGRADE.md#images). |

### What the NetworkPolicy suite checks, and how the policies behave

All NetworkPolicy controls need a CNI that enforces NetworkPolicy (otherwise they are accepted by the API
server and do nothing); `kubectl port-forward` bypasses them.

**Checked** by `dev/calico/netpol-test.sh` ([dev/calico/README.md](dev/calico/README.md)): Calico v3.33.0 on a
single-node k3d cluster in a Lima VM (rootful Docker, kernel 6.8), 8 scenarios, 52 checks PASS and 0 FAIL in the
recorded run, with real `curl` probes (a block counts only as a connect timeout), an enforcement canary before the
test, and a negative control with `networkPolicy.enabled=false` showing the same probes then succeed. Under the
chart's default values plus one allowed peer namespace per peer list:

- the engine `:8080` is reachable from the MCP pods, the bundled vmsingle and `metricsFrom` namespaces, and blocked
  from other namespaces and from unlabelled pods of the release namespace;
- ClickHouse `:8123`/`:9000` is reachable only from engine-labelled pods;
- vmsingle `:8428` is reachable from the engine and `metricsFrom`; a write to `/api/v1/import/prometheus` from
  another namespace is blocked;
- the MCP port is reachable from `mcpFrom` and the release namespace, blocked elsewhere;
- all chart pods stay Ready with 0 restarts under the policies, recover after being deleted, and the bundled
  scrape keeps working (its traffic is egress, which the policies do not restrict).

**Configuration notes:**

- The suite uses one node. Calico does not apply ingress policy to traffic that originates on the node itself
  (kubelet probes, the API server calling a local pod), so `networkPolicy.webhookFrom` is not exercised by it. On a
  multi-node cluster the API server's source is a node or tunnel address that `webhookFrom` must list, or the webhook
  silently fails open (`failurePolicy: Ignore`). The suite shows that the API server's calls reach the webhook under
  the policies (apiserver admission counters `code="200"` rising, fail-open count flat).
- The suite uses Calico. Other CNIs (Cilium, Antrea, kube-router) and managed-cluster network-policy add-ons enforce
  the same `networking.k8s.io/v1` policies; verify enforcement with a deny-all test policy (it must produce a connect
  timeout). The suite does not run on every Docker runtime: where the kernel lacks the netfilter modules (the
  "Dory" runtime), Calico, Cilium and kube-router cannot enforce and the test aborts at its canary.
- By design the policies are coarse: the MCP policy admits the whole release namespace; anything labelled
  `app=k8s-tailor` in the release namespace is treated as the engine by the ClickHouse and vmsingle policies; the
  webhook port is open to every source unless `webhookFrom` is set; only ingress is restricted.

## Hardening checklist

1. Keep `tailor.apiAuth.enabled=true` and `mcp.auth.enabled=true`; in production source both
   tokens and the ClickHouse password from External Secrets (`existingSecret`; tokens limited to
   `A-Z a-z 0-9 . _ ~ + / = -`, e.g. `openssl rand -hex 32`) and set
   `secretGeneration.failWithoutLookup=true` under GitOps.
2. Use a CNI that enforces NetworkPolicy (check: a deny-all test policy must produce a connect timeout;
   `dev/calico/netpol-test.sh` does this on a throwaway cluster); list your ingress controller in
   `networkPolicy.mcpFrom` and your Prometheus/Grafana in `networkPolicy.metricsFrom`. On multi-node clusters,
   if you set `networkPolicy.webhookFrom`, include the node or tunnel addresses the API server calls from.
3. Front the MCP with TLS + SSO if it leaves port-forward; set `mcp.allowedHosts` (and
   `tailor.allowedHosts` if the engine UI is exposed) to the public names.
4. Enable `tailor.admissionPolicy` (k8s >= 1.30), first with `validationActions: [Warn, Audit]`,
   then `[Deny]`.
5. Webhook: `certMode: certManager`.
6. Pin images by digest.
7. With GitOps, give the engine-owned fields to the engine: [deploy/argocd/workload-application.yaml](deploy/argocd/workload-application.yaml) (`ignoreDifferences` for `resources` and the proxy annotations) and [deploy/flux/](deploy/flux/) keep a sync from reverting its template patches.
8. Consider `tailor.apiAuth.protectReads=true` when the engine API is reachable from more than the MCP and your metrics scraper (`/healthz`, `/livez`, `/metrics` and the embedded UI shell stay open; the UI asks for the token).
9. Start in `Recommend`, review the Decisions view, then enable actuation on a narrow selector
   with `direction: Down`.
