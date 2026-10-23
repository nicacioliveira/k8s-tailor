# dev/calico - proving the chart's NetworkPolicies

`netpol-test.sh` builds a throw-away k3d cluster (`tailor-netpol`, never `tailor-dev`) whose
CNI really enforces NetworkPolicy (Calico), installs the chart with
[`dev/k3s/values-dev.yaml`](../k3s/values-dev.yaml) + [`values-netpol.yaml`](values-netpol.yaml)
(NetworkPolicy on, `mcp.auth` on, webhook on with `certMode: helm`) and checks, with real
`curl` probes, what is blocked and what is allowed. It ends with a negative control
(`networkPolicy.enabled=false`) so a PASS on a "blocked" case cannot be a broken test.

## Run it

```bash
dev/calico/netpol-test.sh            # up + test + down (KEEP=1 keeps the cluster)
dev/calico/netpol-test.sh up         # cluster + Calico + canary + chart
dev/calico/netpol-test.sh test       # scenarios only, prints PASS/FAIL, exit 1 on any FAIL
dev/calico/netpol-test.sh down
```

Needs `docker`, `k3d`, `kubectl`, `helm`, `python3`, internet access (Calico manifests, images) and the
local images `k8s-tailor:dev` / `k8s-tailor-mcp:dev` in the SAME Docker daemon k3d talks to (the
script builds them as `dev/k3s/up.sh` does if they are missing - slow). Variables: `CLUSTER`,
`CALICO_VERSION`, `K3S_IMAGE`, `KEEP=1`, `FORCE=1`.

Measured on an Apple-silicon Mac with a Lima VM (Ubuntu 24.04, kernel 6.8, 4 CPU / 6 GiB,
rootful Docker 29.9): about 5.5 minutes end to end with warm image caches (cluster + Calico ~2 min,
chart ~1 min, scenarios ~2.5 min), plus 1 to 3 minutes the first time for image pulls.

## Cluster and Calico

```bash
k3d cluster create tailor-netpol --image rancher/k3s:v1.34.12-k3s1 --servers 1 --agents 0 --no-lb \
  --k3s-arg "--flannel-backend=none@server:*" --k3s-arg "--disable-network-policy@server:*" \
  --k3s-arg "--disable=traefik@server:0" --k3s-arg "--disable=servicelb@server:0" --k3s-arg "--disable=metrics-server@server:0" \
  --k3s-arg "--kube-apiserver-arg=feature-gates=InPlacePodVerticalScaling=true,MutatingAdmissionPolicy=true@server:0" \
  --k3s-arg "--kube-apiserver-arg=runtime-config=admissionregistration.k8s.io/v1beta1=true@server:0" \
  --k3s-arg "--kubelet-arg=feature-gates=InPlacePodVerticalScaling=true@server:0"

B=https://raw.githubusercontent.com/projectcalico/calico/v3.33.0/manifests
kubectl create -f $B/v3_projectcalico_org-v1beta1.yaml   # k8s 1.34/1.35; 1.36+: v3_projectcalico_org.yaml
kubectl create -f $B/tigera-operator.yaml
kubectl create -f $B/custom-resources.yaml               # default IP pool 192.168.0.0/16
kubectl delete goldmane,whisker default                  # flow-log components, not needed
```

Version: **Calico v3.33.0** (current stable on the docs.tigera.io k3s quickstart on 2026-10-22,
operator install). The `MutatingAdmissionPolicy` gate / `v1beta1` runtime-config are required on
Kubernetes 1.34: without them the tigera-operator (v1.44) crash-loops with
`v3 CRD mode requires MutatingAdmissionPolicy support`.

The script first runs an **enforcement canary** (a deny-all policy must produce a connect timeout)
and aborts with exit 3 if the CNI does not enforce. `FORCE=1` skips the abort.

## Scenarios (what `test` checks)

A blocked connection is only accepted as "blocked" when curl ends with rc=28 (connect timeout, the
signature of a DROP); "connection refused" or a DNS error is a FAIL, never a pass.

Test pods are `curlimages/curl` pods: `np-ingress/t` (the `mcpFrom` namespace), `np-metrics/t`
(`metricsFrom`), `np-other/t` (arbitrary), `np-optin/*` (webhook opt-in namespace), and in the release
namespace `t-engine` (label `app=k8s-tailor`), `t-mcp` (label `app=k8s-tailor-mcp`), `t-plain` (no
labels). The two label impersonators have a never-ready readiness probe so they do not join the
Services' endpoints (a pod labelled `app=k8s-tailor` that does not listen on 8443 otherwise makes the
apiserver's webhook calls fail through the Service).

| # | Check |
|---|---|
| 1 | engine :8080 reachable from the MCP-labelled pod, from the real MCP pod and from `np-metrics`; blocked from `np-other`, `np-ingress`, an unlabelled release-ns pod |
| 2 | ClickHouse :8123/:9000 reachable from the engine-labelled pod only |
| 3 | vmsingle :8428 reachable from the engine and `np-metrics`; `POST /api/v1/import/prometheus` blocked from `np-other` |
| 4 | MCP :8020 reachable from `np-ingress` (`mcpFrom`) and the release namespace; blocked from `np-other`, `np-metrics` |
| 5 | kube-apiserver reaches the webhook (apiserver counters `apiserver_admission_webhook_request_total{code="200"}` up, `..._fail_open_count` flat; needed because `failurePolicy: Ignore` hides a blocked webhook); with `webhookFrom` set, pods in other namespaces are blocked |
| 6 | all chart pods Ready with 0 restarts under the policies, no probe TIMEOUT events, and every chart pod is deleted and must come back Ready |
| 7 | `count(container_cpu_usage_seconds_total)` > 0 in vmsingle (its scrape of the kubelet through the apiserver proxy is egress, unaffected) |
| 8 | `networkPolicy.enabled=false`: no NetworkPolicy left, and every case blocked above is now reachable |

Scenario 5 does not show a mutated pod: a brand-new bare pod has no recommendation, so the webhook
admits it unchanged (HTTP 200 from the webhook, no `admission-sized` annotation). Reach is proven by
the apiserver counters (code=200 up, fail_open flat; 43+ admissions answered with the policies on).
`netpol-test.sh mutate` is a slow optional extra (policies on, a `Webhook`-mode TailorPolicy and a
workload in `np-optin`, pod recreated until the webhook resizes it). Status of that extra: a
mutation (cpu 500m -> 10m, memory 256Mi -> 32Mi, `admission-sized` annotation) was observed once, but
in a cluster where scenario 8 had already switched the policies OFF, so it is not NetworkPolicy
evidence. With the policies ON the webhook kept answering 200 but did not resize within the ~20-40
minutes waited, because the engine's `Confident` gate (webhook.go) was not met yet (the same happened
with the policies OFF right after the recommendation appeared), i.e. a confidence/time effect, not a
drop. The `mutate` subcommand therefore stays UNVERIFIED end to end.

Single-node caveat: Calico never applies ingress policy to traffic that originates on the node itself
(kubelet probes, the apiserver reaching a local pod), so with one node `webhookFrom` cannot be
shown to admit or deny the apiserver; it only proves that other pods are blocked. On a multi-node
cluster the apiserver's source address is a node/tunnel address and `webhookFrom` must list it, or
the webhook silently fails open.

## Which Docker runtimes work (checked 2026-10-22)

| Runtime | Result |
|---|---|
| Dory (kernel 6.12.30-dory), the runtime in use here | Calico v3.33.0: **does not enforce.** The kernel has no `xt_CT`, `xt_NFLOG`, `rpfilter` (and no nft expression support): felix logs `Extension NFLOG revision 0 not supported`, `Extension CT revision 0 not supported`, `RULE_APPEND failed`, `Failed to create knftables client ... Operation not supported`, then panics (`calico-node` never Ready). Cilium 1.20.3: agent dies with `unable to run agent: failed to start: protocol not supported` (netlink family missing). Plain k3d (flannel + k3s kube-router controller): programs chains but pod traffic is not filtered (`RULE_APPEND failed ... KUBE-ROUTER-FORWARD`, no `xt_physdev`); a test policy let traffic through. |
| Lima VM `template://docker-rootful` (Ubuntu 24.04, kernel 6.8.0) | **Works**, all scenarios pass. (`template://docker`, the rootless one, fails: kubelet cannot open `/dev/kmsg`.) |
| Docker Desktop / OrbStack / Colima / native Linux | not exercised by this suite; standard kernels ship the netfilter modules Calico needs |

Lima recipe used (macOS): `limactl start --name=tailor-netpol-vm --cpus 4 --memory 6 --disk 30 --vm-type=vz --tty=false template://docker-rootful`,
then `export DOCKER_HOST=unix://$HOME/.lima/tailor-netpol-vm/sock/docker.sock`, copy the images
(`docker save k8s-tailor:dev k8s-tailor-mcp:dev | docker load` against the new socket) and run the script.
Clean up with `limactl delete -f tailor-netpol-vm`.

## Chart notes from this run

No policy blocked a probe, the scrape or the webhook. By design (documented in `values.yaml`):
the MCP policy admits the whole release namespace (`podSelector: {}`) plus `mcpFrom`; the webhook port
is open to every source unless `webhookFrom` is set; anything labelled `app=k8s-tailor` in the release
namespace is treated as the engine by the ClickHouse and vmsingle policies.
