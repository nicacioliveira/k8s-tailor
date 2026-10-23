import { shq } from "./kubectl";
import type { ContainerClass, Rec } from "./types";

// Per-container reading of /api/recommendations (docs/CONTAINERS.md): class, effective
// mode, where the policy came from, how a change would reach the container, and the
// annotation that opts a sidecar in. Pure functions, no React.

/** Annotation prefix of the per-container overrides (internal/policy/containers.go). */
export const ANN_CONTAINER_PREFIX = "tailor.k8s-tailor.dev/container.";
export const containerAnnotation = (container: string, field: string) =>
  `${ANN_CONTAINER_PREFIX}${container}.${field}`;

/** An engine from before container classification sends no class: that is a plain app. */
export const classOf = (r: Pick<Rec, "containerClass">): ContainerClass =>
  r.containerClass || "app";

export const isInjectedSidecar = (r: Rec) => classOf(r) === "injected-sidecar";
/** init / ephemeral containers are never sized, whatever the mode says. */
export const isNeverSized = (r: Rec) => classOf(r) === "init" || classOf(r) === "ephemeral";

export const CLASS_OPTIONS: { value: ContainerClass; label: string }[] = [
  { value: "app", label: "App" },
  { value: "injected-sidecar", label: "Injected sidecar" },
  { value: "native-sidecar", label: "Native sidecar" },
  { value: "init", label: "Init" },
  { value: "ephemeral", label: "Ephemeral" },
];

/** Short badge text: "App", "Sidecar · istio", "Native sidecar", "Init". */
export function classLabel(r: Pick<Rec, "containerClass" | "mesh">): string {
  switch (classOf(r)) {
    case "injected-sidecar":
      return r.mesh ? `Sidecar · ${r.mesh}` : "Sidecar";
    case "native-sidecar":
      return "Native sidecar";
    case "init":
      return "Init";
    case "ephemeral":
      return "Ephemeral";
    default:
      return "App";
  }
}

/** One sentence on what the class means for sizing and actuation. */
export function classHint(r: Rec): string {
  switch (classOf(r)) {
    case "injected-sidecar":
      return `Injected${r.mesh ? ` by ${r.mesh}` : ""}${r.nativeSidecar ? " as a native sidecar (initContainers, restartPolicy Always)" : ""}: sized with the sidecar profile and held at Recommend until you opt in.`;
    case "native-sidecar":
      return "Native sidecar (initContainers with restartPolicy Always) declared by the workload: follows the workload policy and can be resized in place.";
    case "init":
      return "Init container: runs to completion, never sized or resized.";
    case "ephemeral":
      return "Ephemeral (debug) container: never sized or resized.";
    default:
      return "Regular app container: follows the workload policy.";
  }
}

const CLASS_RANK: Record<ContainerClass, number> = {
  app: 0,
  "native-sidecar": 1,
  "injected-sidecar": 2,
  init: 3,
  ephemeral: 4,
};

/**
 * Keeps the containers of one workload adjacent (first appearance wins the position,
 * app first, then sidecars) without disturbing the engine's ranking between workloads.
 */
export function groupSiblings(recs: Rec[]): Rec[] {
  const order: string[] = [];
  const groups = new Map<string, Rec[]>();
  for (const r of recs) {
    const k = `${r.namespace}/${r.kind}/${r.workload}`;
    const g = groups.get(k);
    if (g) g.push(r);
    else {
      groups.set(k, [r]);
      order.push(k);
    }
  }
  return order.flatMap((k) =>
    (groups.get(k) as Rec[])
      .map((r, i) => ({ r, i }))
      .sort((a, b) => CLASS_RANK[classOf(a.r)] - CLASS_RANK[classOf(b.r)] || a.i - b.i)
      .map((x) => x.r),
  );
}

/** Effective mode of this container; an empty mode means recommend-only. */
export const effectiveMode = (r: Pick<Rec, "mode">) => r.mode || "Recommend";

/** Where the effective container policy came from, in plain words. */
export function originText(r: Pick<Rec, "containerPolicy">): string {
  const o = r.containerPolicy ?? "";
  if (o === "" || o === "policy") return "from policy";
  if (o === "default:injected-sidecar") return "default for injected sidecars";
  if (o === "annotation") return "container annotation";
  const m = /^(policy|profile):(.+) containers\[(\d+)\]$/.exec(o);
  if (m) return `rule ${Number(m[3]) + 1} of ${m[1]} ${m[2]}`;
  return o;
}

const PATH_PART: Record<string, string> = {
  none: "none",
  webhook: "webhook (at admission)",
  "in-place": "in-place",
  template: "template rollout",
  ksvc: "Knative revision",
  "none:not-in-template": "not in template",
  "none:native-sidecar-in-ksvc": "no durable path (native sidecar in a Knative service)",
};

function pathPart(p: string): string {
  if (PATH_PART[p]) return PATH_PART[p];
  const px = /^proxy-annotations:(.+)$/.exec(p);
  if (px) return `proxy annotations (${px[1]})`;
  const fb = /^fallback:(.+)$/.exec(p);
  if (fb) return `fallback to ${pathPart(fb[1])}`;
  return p;
}

/** How a resize reaches this container: "in-place + proxy annotations (istio)". */
export function pathText(r: Pick<Rec, "actuationPath">): string {
  const p = r.actuationPath ?? "";
  if (!p) return "not reported";
  return p.split("+").map(pathPart).join(" + ");
}

/** True when no path can change this container (none, or only "none:<why>" parts). */
export const pathIsNone = (r: Pick<Rec, "actuationPath">) =>
  (r.actuationPath ?? "") !== "" &&
  (r.actuationPath ?? "").split("+").every((p) => p === "none" || p.startsWith("none:"));

/**
 * An injected sidecar sitting at Recommend: the engine caps it there until a rule or a
 * per-container annotation sets a mode explicitly (the workload toggle never does).
 */
export const needsSidecarOptIn = (r: Rec) =>
  isInjectedSidecar(r) && r.containerPolicy !== "annotation" && effectiveMode(r) === "Recommend";

const KUBECTL_KIND: Record<string, string> = {
  Deployment: "deployment",
  StatefulSet: "statefulset",
  DaemonSet: "daemonset",
  Pod: "pod",
  ksvc: "ksvc",
};

/**
 * The annotation(s) as a `kubectl annotate` command, or YAML when the kind has no kubectl
 * resource (a TailorGroup puts them on its pods). Values are the engine's documented ones.
 */
export function annotateSnippet(
  ref: { namespace: string; kind: string; workload: string },
  container: string,
  entries: Record<string, string>,
): string {
  const res = KUBECTL_KIND[ref.kind];
  const pairs = Object.entries(entries).map(([f, v]) => [containerAnnotation(container, f), v]);
  if (res) {
    return `kubectl -n ${shq(ref.namespace)} annotate ${res}/${shq(ref.workload)} ${pairs
      .map(([k, v]) => shq(`${k}=${v}`))
      .join(" ")} --overwrite`;
  }
  return `# on the pods of ${ref.kind} ${ref.workload}\nmetadata:\n  annotations:\n${pairs
    .map(([k, v]) => `    ${k}: ${JSON.stringify(v)}`)
    .join("\n")}`;
}

/** Opt-in for a sidecar: enable in-place actuation (plus the durable proxy path when it applies). */
export function enableSnippet(r: Rec): string {
  const entries: Record<string, string> = { mode: "InPlace" };
  if (r.mesh && !r.inTemplate && !r.proxyAnnotations) entries["proxy-annotations"] = "true";
  return annotateSnippet(r, r.container, entries);
}

export const excludeSnippet = (r: Rec) => annotateSnippet(r, r.container, { exclude: "true" });
