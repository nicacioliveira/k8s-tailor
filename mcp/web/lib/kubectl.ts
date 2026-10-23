// Turns the engine's dry-run patch (GET /api/patch/...) into copy-pasteable kubectl.
// The patch bodies are the engine's, verbatim; we only wrap them in the command.

export interface PatchPlan {
  method?: string; // InPlace | Rollout | InPlacePreferred (from the policy)
  inPlace?: { subresource?: string; patchType?: string; body?: unknown };
  rollout?: { target?: string; patchType?: string; body?: unknown };
}

/** Built-in controllers accept a strategic-merge patch on their pod template. */
const NATIVE: Record<string, string> = {
  Deployment: "deployment",
  StatefulSet: "statefulset",
  DaemonSet: "daemonset",
  ReplicaSet: "replicaset",
};

/** POSIX single-quote a string for the shell. */
export const shq = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;

export interface KubectlCommands {
  /** Controller template patch (persists; rolls pods). Null for kinds without strategic merge. */
  rollout: string | null;
  /** In-place resize of ONE running pod (no restart). POD is a placeholder. */
  inPlace: string | null;
  /** Why `rollout` is null, when it is. */
  rolloutNote?: string;
}

export function kubectlCommands(
  ref: { namespace: string; kind: string; workload: string },
  plan: PatchPlan,
  dryRun: boolean,
): KubectlCommands {
  const dr = dryRun ? " --dry-run=server -o yaml" : "";
  const ns = shq(ref.namespace);
  const res = NATIVE[ref.kind];
  const rolloutBody = plan.rollout?.body;
  const rollout =
    res && rolloutBody
      ? `kubectl -n ${ns} patch ${res}/${shq(ref.workload)} --type strategic -p ${shq(JSON.stringify(rolloutBody))}${dr}`
      : null;
  const inPlaceBody = plan.inPlace?.body;
  const inPlace = inPlaceBody
    ? `# for each running pod of ${ref.kind}/${ref.workload} (kubectl ≥ 1.32, Kubernetes ≥ 1.33)\nkubectl -n ${ns} patch pod POD --subresource resize --type strategic -p ${shq(JSON.stringify(inPlaceBody))}${dr}`
    : null;
  return {
    rollout,
    inPlace,
    rolloutNote:
      rollout || !rolloutBody
        ? undefined
        : `${ref.kind} is not a built-in controller: kubectl has no strategic merge for it, and a JSON merge patch would replace the whole container list. Apply it through its owner (e.g. edit the Knative Service).`,
  };
}
