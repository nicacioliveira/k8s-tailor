import type { ContainerResSpec, ContainerRuleView } from "./types";

// Pure helpers behind the policy editor's "Containers" section (TailorPolicy
// spec.containers[]; first matching rule wins, docs/CONTAINERS.md).

export const INHERIT = "__inherit__";

export const parseList = (s: string) =>
  s
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);

export const blankRule = (): ContainerRuleView => ({ match: { names: [], classes: [] } });

/** An empty match never applies on the engine side (it would capture every container). */
export const ruleHasMatch = (r: ContainerRuleView) =>
  (r.match?.names?.length ?? 0) > 0 || (r.match?.classes?.length ?? 0) > 0;

/** Moves rule `i` by `by` positions (order matters: the first match wins). */
export function moveRule(rules: ContainerRuleView[], i: number, by: -1 | 1): ContainerRuleView[] {
  const j = i + by;
  if (j < 0 || j >= rules.length) return rules;
  const out = [...rules];
  [out[i], out[j]] = [out[j], out[i]];
  return out;
}

/** Sets one field of a cpu/memory block; an emptied block disappears from the rule. */
export function setRes(
  rule: ContainerRuleView,
  which: "cpu" | "memory",
  field: "min" | "max",
  value: string,
): ContainerRuleView {
  const next: ContainerResSpec = { ...rule[which] };
  if (value.trim()) next[field] = value.trim();
  else delete next[field];
  const out = { ...rule };
  if (Object.keys(next).length) out[which] = next;
  else delete out[which];
  return out;
}

/** Drops keys the user left on "inherit" so the saved CR stays minimal. */
export function setOpt<K extends "mode" | "apply" | "direction">(
  rule: ContainerRuleView,
  key: K,
  value: string,
): ContainerRuleView {
  const out = { ...rule };
  if (value === INHERIT) delete out[key];
  else out[key] = value;
  return out;
}

export function setProxyAnnotations(rule: ContainerRuleView, value: string): ContainerRuleView {
  const out = { ...rule };
  if (value === INHERIT) delete out.proxyAnnotations;
  else out.proxyAnnotations = value === "true";
  return out;
}

/** One-line description of a rule for the list header and the policies table. */
export function describeRule(r: ContainerRuleView): string {
  const who = [...(r.match?.names ?? []), ...(r.match?.classes ?? [])].join(", ") || "no match";
  if (r.exclude) return `${who} → excluded`;
  const what = [
    r.mode && `mode ${r.mode}`,
    r.apply && `apply ${r.apply}`,
    r.direction && `direction ${r.direction}`,
    r.profileRef && `profile ${r.profileRef}`,
    r.proxyAnnotations != null && `proxy annotations ${r.proxyAnnotations ? "on" : "off"}`,
    (r.cpu?.min || r.cpu?.max) && "cpu bounds",
    (r.memory?.min || r.memory?.max) && "memory bounds",
  ].filter(Boolean);
  return `${who} → ${what.length ? what.join(", ") : "no overrides"}`;
}
