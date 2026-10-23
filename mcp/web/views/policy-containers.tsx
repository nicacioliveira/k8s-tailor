import { ArrowDown, ArrowUp, Plus, Trash2 } from "lucide-react";
import { useId, useState } from "react";
import { Field, OptionSelect } from "../components/field";
import { Hint } from "../components/workload-badges";
import { CLASS_OPTIONS } from "../lib/containers";
import {
  INHERIT,
  blankRule,
  describeRule,
  moveRule,
  parseList,
  ruleHasMatch,
  setOpt,
  setProxyAnnotations,
  setRes,
} from "../lib/policy-rules";
import {
  APPLY_METHODS,
  CONTAINER_CLASSES,
  type ContainerRuleView,
  DIRECTIONS,
  MODES,
} from "../lib/types";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { InfoBox } from "../ui/info-box";
import { Input } from "../ui/input";
import { Label } from "../ui/label";

const withInherit = (opts: string[]) => [
  { value: INHERIT, label: "Inherit" },
  ...opts.map((o) => ({ value: o, label: o })),
];

/** Comma-separated list that keeps what you type (the parsed list alone would eat a trailing comma). */
export function ListInput({
  value,
  onChange,
  ...props
}: {
  value: string[];
  onChange: (v: string[]) => void;
} & Omit<React.ComponentProps<typeof Input>, "value" | "onChange">) {
  const [draft, setDraft] = useState(value.join(", "));
  const synced = parseList(draft).join("\u0000") === value.join("\u0000");
  return (
    <Input
      {...props}
      value={synced ? draft : value.join(", ")}
      onChange={(e) => {
        setDraft(e.target.value);
        onChange(parseList(e.target.value));
      }}
    />
  );
}

/**
 * Rules of spec.containers. Any field of an existing rule this form does not show
 * (quantile, margin, limitFactor…) is kept: every edit spreads the rule it started from.
 */
export function ContainerRules({
  rules,
  onChange,
}: {
  rules: ContainerRuleView[];
  onChange: (rules: ContainerRuleView[]) => void;
}) {
  const patch = (i: number, rule: ContainerRuleView) =>
    onChange(rules.map((r, k) => (k === i ? rule : r)));
  return (
    <section aria-labelledby="container-rules-title" className="flex flex-col gap-4">
      <div className="flex flex-wrap items-baseline gap-2">
        <h3 id="container-rules-title" className="text-base font-semibold tracking-tight">
          Containers
        </h3>
        <span className="text-sm text-muted-foreground">
          Per-container rules; the first matching rule wins
        </span>
      </div>
      {rules.length === 0 && (
        <InfoBox tone="muted" icon={false}>
          No container rules. Every container follows this policy; injected sidecars (istio-proxy…)
          are held at Recommend with the sidecar sizing profile until a rule or annotation sets
          their mode.
        </InfoBox>
      )}
      <ol className="flex flex-col gap-4">
        {rules.map((rule, i) => (
          <RuleCard
            // biome-ignore lint/suspicious/noArrayIndexKey: rules have no identity; order is the identity
            key={i}
            index={i}
            count={rules.length}
            rule={rule}
            onChange={(r) => patch(i, r)}
            onMove={(by) => onChange(moveRule(rules, i, by))}
            onRemove={() => onChange(rules.filter((_, k) => k !== i))}
          />
        ))}
      </ol>
      <Button
        variant="outline"
        size="sm"
        className="w-fit"
        onClick={() => onChange([...rules, blankRule()])}
      >
        <Plus aria-hidden />
        Add container rule
      </Button>
    </section>
  );
}

function RuleCard({
  index,
  count,
  rule,
  onChange,
  onMove,
  onRemove,
}: {
  index: number;
  count: number;
  rule: ContainerRuleView;
  onChange: (r: ContainerRuleView) => void;
  onMove: (by: -1 | 1) => void;
  onRemove: () => void;
}) {
  const ids = { ex: useId() };
  const classes = rule.match?.classes ?? [];
  const toggleClass = (c: string, on: boolean) =>
    onChange({
      ...rule,
      match: {
        ...rule.match,
        classes: on ? [...classes, c] : classes.filter((x) => x !== c),
      },
    });
  return (
    <li className="flex flex-col gap-4 rounded-xl border border-border p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h4 className="text-sm font-semibold">
          Rule {index + 1}
          <span className="ml-2 font-normal text-muted-foreground">{describeRule(rule)}</span>
        </h4>
        <span className="inline-flex items-center gap-1">
          <Hint text="Move up (earlier rules win)">
            <Button
              variant="ghost"
              size="icon-sm"
              disabled={index === 0}
              aria-label={`Move rule ${index + 1} up`}
              onClick={() => onMove(-1)}
            >
              <ArrowUp aria-hidden />
            </Button>
          </Hint>
          <Hint text="Move down">
            <Button
              variant="ghost"
              size="icon-sm"
              disabled={index === count - 1}
              aria-label={`Move rule ${index + 1} down`}
              onClick={() => onMove(1)}
            >
              <ArrowDown aria-hidden />
            </Button>
          </Hint>
          <Hint text="Remove rule">
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={`Remove rule ${index + 1}`}
              onClick={onRemove}
            >
              <Trash2 aria-hidden />
            </Button>
          </Hint>
        </span>
      </div>

      {!ruleHasMatch(rule) && (
        <InfoBox tone="warning" title="This rule matches nothing">
          Add a container name or pick a class. A rule with an empty match never applies.
        </InfoBox>
      )}

      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Container names" hint="Globs, comma-separated: istio-proxy, vault-agent*">
          <ListInput
            placeholder="istio-proxy"
            value={rule.match?.names ?? []}
            onChange={(names) => onChange({ ...rule, match: { ...rule.match, names } })}
          />
        </Field>
        <fieldset className="grid content-start gap-2">
          <legend className="text-sm font-medium">Container classes</legend>
          <div className="flex flex-wrap gap-x-4 gap-y-2">
            {CONTAINER_CLASSES.map((c) => (
              <ClassCheck
                key={c}
                label={CLASS_OPTIONS.find((o) => o.value === c)?.label ?? c}
                checked={classes.includes(c)}
                onChange={(on) => toggleClass(c, on)}
              />
            ))}
          </div>
          <p className="text-xs text-muted-foreground">
            Names and classes both given: both must match.
          </p>
        </fieldset>
      </div>

      <div className="flex items-start gap-3">
        <Checkbox
          id={ids.ex}
          className="mt-0.5"
          checked={!!rule.exclude}
          onCheckedChange={(c) => onChange({ ...rule, exclude: c === true || undefined })}
        />
        <div className="grid gap-1">
          <Label htmlFor={ids.ex}>Exclude</Label>
          <p className="text-xs text-muted-foreground">
            No recommendation and no actuation for matching containers.
          </p>
        </div>
      </div>

      {!rule.exclude && (
        <>
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label="Mode" hint="Only an explicit mode lifts a sidecar from Recommend">
              <OptionSelect
                value={rule.mode ?? INHERIT}
                options={withInherit(MODES)}
                onChange={(v) => onChange(setOpt(rule, "mode", v))}
              />
            </Field>
            <Field label="Apply" hint="How to act">
              <OptionSelect
                value={rule.apply ?? INHERIT}
                options={withInherit(APPLY_METHODS)}
                onChange={(v) => onChange(setOpt(rule, "apply", v))}
              />
            </Field>
            <Field label="Direction" hint="Shrink-only / grow-only">
              <OptionSelect
                value={rule.direction ?? INHERIT}
                options={withInherit(DIRECTIONS)}
                onChange={(v) => onChange(setOpt(rule, "direction", v))}
              />
            </Field>
            <Field label="Profile" hint="TailorProfile sizing this container">
              <Input
                placeholder="envoy"
                value={rule.profileRef ?? ""}
                onChange={(e) => onChange({ ...rule, profileRef: e.target.value || undefined })}
              />
            </Field>
            <Field
              label="Proxy annotations"
              hint="Mesh proxy outside the template: durable path via the mesh's resource annotations"
              className="sm:col-span-2"
            >
              <OptionSelect
                value={rule.proxyAnnotations == null ? INHERIT : String(rule.proxyAnnotations)}
                options={[
                  { value: INHERIT, label: "Inherit" },
                  { value: "true", label: "On" },
                  { value: "false", label: "Off" },
                ]}
                onChange={(v) => onChange(setProxyAnnotations(rule, v))}
              />
            </Field>
          </div>
          <div className="grid gap-4 sm:grid-cols-4">
            <Field label="CPU min">
              <Input
                placeholder="20m"
                value={rule.cpu?.min ?? ""}
                onChange={(e) => onChange(setRes(rule, "cpu", "min", e.target.value))}
              />
            </Field>
            <Field label="CPU max">
              <Input
                placeholder="none"
                value={rule.cpu?.max ?? ""}
                onChange={(e) => onChange(setRes(rule, "cpu", "max", e.target.value))}
              />
            </Field>
            <Field label="Memory min">
              <Input
                placeholder="96Mi"
                value={rule.memory?.min ?? ""}
                onChange={(e) => onChange(setRes(rule, "memory", "min", e.target.value))}
              />
            </Field>
            <Field label="Memory max">
              <Input
                placeholder="1Gi"
                value={rule.memory?.max ?? ""}
                onChange={(e) => onChange(setRes(rule, "memory", "max", e.target.value))}
              />
            </Field>
          </div>
        </>
      )}
    </li>
  );
}

function ClassCheck({
  label,
  checked,
  onChange,
}: { label: string; checked: boolean; onChange: (on: boolean) => void }) {
  const id = useId();
  return (
    <span className="inline-flex items-center gap-2">
      <Checkbox id={id} checked={checked} onCheckedChange={(c) => onChange(c === true)} />
      <Label htmlFor={id} className="font-normal">
        {label}
      </Label>
    </span>
  );
}
