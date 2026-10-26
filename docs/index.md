# K8s Tailor documentation

K8s Tailor is a self-hosted Kubernetes pod rightsizer built around **in-place resize** (the `pods/resize`
subresource, Kubernetes 1.33 and later). It measures what your containers really use, computes a right-sized
request with plain statistics and applies it to running pods without restarting them, keeping an explainable record
of every decision.

!!! warning "Status: pre-1.0"
    The CRDs are `v1alpha1`. It ships **inert**: nothing is changed until you create a policy that turns actuation on.
    The dollar figures are estimates, the backtest is a simulation, and what the test suites cover (and what they do not)
    is listed in [End-to-end results](E2E.md) and [Known limitations](KNOWN-ISSUES.md).

## Install in two commands

```bash
helm install k8s-tailor oci://ghcr.io/nicacioliveira/k8s-tailor/charts/k8s-tailor \
  --version 0.2.0 -n k8s-tailor --create-namespace
kubectl -n k8s-tailor get pods
```

The images (`ghcr.io/nicacioliveira/k8s-tailor` and `k8s-tailor-mcp`, `v0.1.0`) are public and built for
linux/amd64 and linux/arm64. Prerequisites, values and the first policy are in [Install](INSTALL.md).

## Where to go

| If you want to... | Read |
|---|---|
| Install it on a cluster, or try it on a local k3d | [Install](INSTALL.md) |
| Let an LLM agent do the install with a deterministic runbook | [Install with an LLM agent](LLM-INSTALL.md) |
| Understand what it does and why | [Product](PRODUCT.md), [Architecture](ARCHITECTURE.md) |
| Write a policy, tune quantiles, margins, cooldowns | [Configuration](CONFIGURATION.md) |
| Audit how a recommendation is computed and applied | [Technical reference](TECHNICAL.md) |
| Handle sidecars (Istio, Linkerd) and init containers | [Containers and sidecars](CONTAINERS.md) |
| See what the engine deliberately does not do | [Known limitations](KNOWN-ISSUES.md) |
| See how it compares with KRR-style and VPA-style on synthetic load | [Backtest](BACKTEST.md) |
| See what was run against a real cluster | [End-to-end results](E2E.md) |
| Fix something that does not work | [Troubleshooting](TROUBLESHOOTING.md) |
| Upgrade the chart | [Upgrade](UPGRADE.md) |
| Use the optional, read-only LLM advisor | [Advisor](ADVISOR.md) |
| Tour the dashboard | [Screens](SCREENS.md) |
| Report a vulnerability | [Security](../SECURITY.md) |
| Contribute | [Contributing](../CONTRIBUTING.md) |

## The short version of the safety model

- **Inert by default.** A resize happens only for workloads that a `TailorPolicy` selects with an actuating mode.
- **Limits are never shrunk in place**, and a memory limit is never invented unless a policy opts in.
- **A learned OOM floor** keeps a limit that proved too small from being pushed back down.
- **Guards you can read:** per-resource confidence, direction gate, HPA hold, PodDisruptionBudget budget, QoS checks, cooldowns
  and a global pause switch.
- **Everything is a Kubernetes object.** Policies, profiles, groups and the pause switch are CRs and annotations; there is no database for configuration.

The full list, with the reason for each rule, is in [Architecture](ARCHITECTURE.md) and [Technical reference](TECHNICAL.md).

## Source

The code, the chart and these pages live in one repository:
[github.com/nicacioliveira/k8s-tailor](https://github.com/nicacioliveira/k8s-tailor). Licensed under Apache 2.0.
