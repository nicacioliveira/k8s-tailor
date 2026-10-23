# Product

## Register

product

## Platform

web

## Users

Platform / SRE engineers who run Kubernetes fleets (Knative services, stateful services like
Postgres/Kafka). They use this in-task, from inside an MCP host (embedded) or standalone, while tuning how tightly workloads are sized and
deciding what may be actuated automatically vs. left recommend-only. They are fluent in
kubectl and expect the UI to agree with the cluster, never diverge from it.

## Product Purpose

A self-hosted pod rightsizer built around in-place resize. It collects only what it needs
(kubelet resource metrics via a single-node VictoriaMetrics, no full Prometheus), recommends
right-sized requests (decay-weighted percentile, memory peak floor, seasonal forecast,
change-point trimming) and can actuate by in-place resize of running pods, a controller rollout,
a Knative Service patch, or an opt-in mutating webhook at pod creation. All configuration
(policies, profiles, groups, schedules, pause, per-workload overrides) lives in Kubernetes CRs and
annotations, never a database. Success is: a platform team trusts it enough to let it actuate,
sees exactly what it decided and why, and recovers real cluster cost without OOMs or surprises.

## Positioning

A companion for services that dislike restarts, Knative and other latency-sensitive workloads
above all. Its focus, in order: in-place actuation with guardrails (OOM-learned memory
floor, never shrinking limits, direction gate, HPA and PDB awareness, growing a CPU limit a container is pinned at), Knative revisions collapsed
into one unit, and an auditable decision trail. It is not a general FinOps suite. It is an
independent project inspired by ideas from VPA, KRR, Goldilocks, OpenCost and similar projects; see
"Related projects and inspirations" in the README.

## Brand Personality

Precise, calm, trustworthy — a platform tool that disappears into
the task. Honest over hype: it names percentile+decay+forecast for what it is and never dresses
plumbing up as "AI". Confident, dense where the operator needs data, quiet everywhere else.

## Anti-references

SaaS-cream / warm-neutral dashboards; marketing "AI/ML" mysticism; over-styled buttons and
identical icon-card grids; gradient text; hero-metric templates; anything that looks generated.
Never a UI that disagrees with `kubectl`.

## Design Principles

Earned familiarity — mirror the best K8s/observability tools so it's trustworthy on sight.
The tool disappears into the task — density and clarity over ornament.
Honesty over hype — surface the real method and real numbers; no fake intelligence.
Every decision is auditable — recommendation, reasoning, and each applied change are visible
(feed, charts, metrics in the same VM) and reconcilable with the cluster.
Safety is a first-class surface — guardrails, pause, dry-run, and per-workload and per-container
opt-out (an `ignore` annotation is absolute) are always one glance and one click away.

## Learning Loop (planned, not implemented)

Nothing in this section ships today: the engine is deterministic statistics only. This is the
intended direction, kept subordinate to the deterministic core.

The invariant: **learning proposes and prioritizes; the deterministic statistics decide the number;
a CR records it; a human can veto.** Nothing here trains a model that emits a request value
directly. When the Tailor says "455m", the samples that produced it stay traceable.

Training data accrues on its own: the usage series (VictoriaMetrics) plus the decision log
(ClickHouse). Learned per-component parameters (halfLife, margin, season, shape class) would be
written back as `TailorProfile` CRs so they stay auditable and overridable.

Already shipped from the original list: **change-point detection** and the **OOM-learned memory
floor**; also deterministic (not learned) safeguards such as the censored-CPU rule, which treats usage
pinned at a CPU limit as a lower bound of the demand ([TECHNICAL.md](TECHNICAL.md#censored-cpu)). Remaining, in order of value-to-risk:

1. **Shape classification** (steady, spiky, cyclical, bursty): drives suggested policies and a UI
   badge. Interpretable and cheap.
2. **Feedback loop on margin**: treat each resize as an experiment, watch OOM, throttle and waste
   afterwards, and nudge that component's margin. A simple controller, not a model that emits the
   number.
3. **Better forecasting**: Holt-Winters or STL for combined daily and weekly patterns.
4. **Cold-start transfer**: a new component borrows a profile from shape-neighbors.

Out of scope: deep learning per component and any black-box model choosing the final
request or limit; it would break the "explain why the pod OOM'd" contract.

## Accessibility & Inclusion

WCAG AA contrast on the dark theme (body text verified against surfaces, not muted-gray on
tint). `focus-visible` rings on every interactive element. `prefers-reduced-motion` honored
(transitions/animations disabled). Status is never encoded by color alone — icons + text labels
accompany the green/amber/red semantics.
