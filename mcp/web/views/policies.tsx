import { useMutation } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { type ReactNode, useId, useState } from "react";
import { DateField, Field, OptionSelect } from "../components/field";
import { Hint } from "../components/workload-badges";
import { useMcpApp } from "../context.tsx";
import { describeRule, parseList } from "../lib/policy-rules";
import { APPLY_METHODS, MODES, type PolicyView, SEASONS, type WindowView } from "../lib/types";
import { Badge } from "../ui/badge";
import { Banner } from "../ui/banner";
import { Button } from "../ui/button";
import { Card } from "../ui/card";
import { Checkbox } from "../ui/checkbox";
import { ConfirmDialog } from "../ui/confirm-dialog";
import { EmptyState } from "../ui/empty-state";
import { InfoBox } from "../ui/info-box";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { formatNumber } from "../ui/lib/format";
import { SectionHeader } from "../ui/section-header";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "../ui/sheet";
import { Skeleton } from "../ui/skeleton";
import { Slider } from "../ui/slider";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { callTool, useTool } from "../use-tool.ts";
import { ContainerRules, ListInput } from "./policy-containers";

function blankPolicy(): PolicyView {
  return {
    name: "",
    priority: 100,
    mode: "Recommend",
    apply: "InPlace",
    profileRef: "",
    namespaces: [],
    matchLabels: {},
    cpuMargin: 0.15,
    memMargin: 0.15,
    cpuMin: "",
    cpuMax: "",
    memMin: "",
    memMax: "",
    cpuLimitFactor: 0,
    memLimitFactor: 1,
    halfLife: "",
    season: "daily",
    allowExceedLimit: false,
    scheduled: false,
    schedule: { timezone: "UTC", outsideMode: "Off", windows: [] },
  };
}

const scopeText = (p: PolicyView) => {
  const ns = (p.namespaces ?? []).length ? `ns: ${(p.namespaces ?? []).join(", ")}` : "";
  const lb =
    p.matchLabels && Object.keys(p.matchLabels).length
      ? Object.entries(p.matchLabels)
          .map(([k, v]) => `${k}=${v}`)
          .join(",")
      : "";
  return [ns, lb].filter(Boolean).join(" · ") || "Any workload";
};

const guardText = (p: PolicyView) =>
  [
    p.cpuMin && `cpu ≥ ${p.cpuMin}`,
    p.cpuMax && `cpu ≤ ${p.cpuMax}`,
    p.memMin && `mem ≥ ${p.memMin}`,
    p.memMax && `mem ≤ ${p.memMax}`,
  ]
    .filter(Boolean)
    .join(", ");

const normalize = (p: PolicyView): PolicyView => ({
  ...p,
  namespaces: p.namespaces ?? [],
  matchLabels: p.matchLabels ?? {},
});

export function PoliciesView() {
  const app = useMcpApp();
  const list = useTool<{ policies: PolicyView[] | null }>(
    "TAILOR_LIST_POLICIES",
    {},
    { refetchInterval: 15_000 },
  );
  const [editing, setEditing] = useState<PolicyView | null>(null);
  const [isNew, setIsNew] = useState(false);
  const [confirmDel, setConfirmDel] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: (p: PolicyView) => callTool(app, "TAILOR_SET_POLICY", { ...normalize(p) }),
    onSuccess: () => {
      setEditing(null);
      list.refetch();
    },
  });
  const del = useMutation({
    mutationFn: (name: string) => callTool(app, "TAILOR_DELETE_POLICY", { name }),
    onSuccess: () => list.refetch(),
  });
  const policies = list.data?.policies ?? [];
  const create = () => {
    save.reset();
    setIsNew(true);
    setEditing(blankPolicy());
  };

  return (
    <>
      <h1 className="sr-only">Policies</h1>
      <section aria-labelledby="policies-title" className="flex flex-col gap-4">
        <SectionHeader
          id="policies-title"
          title="Policies"
          description="Rightsizing rules: which workloads they select and how the tailor sizes and actuates them. Higher priority wins on overlap."
          action={
            <Button size="sm" onClick={create}>
              <Plus aria-hidden />
              New policy
            </Button>
          }
        />
        {list.isError && (
          <Banner
            variant="danger"
            role="alert"
            title="Could not load policies"
            description={list.error instanceof Error ? list.error.message : String(list.error)}
          />
        )}
        <Card className="gap-0 py-0">
          {list.isLoading ? (
            <div role="status" aria-label="Loading policies" className="flex flex-col gap-3 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : policies.length === 0 ? (
            <EmptyState
              illustration="list"
              title="No policies yet"
              description="Create one to protect critical systems (for example databases → mode Recommend with high headroom) or to tune headroom per scope."
              action={
                <Button size="sm" onClick={create}>
                  <Plus aria-hidden />
                  Create the first policy
                </Button>
              }
            />
          ) : (
            <Table aria-label="Policies">
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4">Policy</TableHead>
                  <TableHead>Scope</TableHead>
                  <TableHead>Mode</TableHead>
                  <TableHead>Method</TableHead>
                  <TableHead>Forecast</TableHead>
                  <TableHead numeric>Headroom</TableHead>
                  <TableHead>Guardrails</TableHead>
                  <TableHead className="pr-4">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {policies.map((p) => (
                  <TableRow
                    key={p.name}
                    interactive
                    onClick={() => {
                      save.reset();
                      setIsNew(false);
                      setEditing(normalize(p));
                    }}
                  >
                    <TableCell className="pl-4">
                      <span className="inline-flex flex-wrap items-center gap-2">
                        <button
                          type="button"
                          className="font-medium outline-none hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
                        >
                          {p.name}
                        </button>
                        {p.scheduled && <Badge variant="info-soft">Scheduled</Badge>}
                        {(p.containers?.length ?? 0) > 0 && (
                          <Hint text={(p.containers ?? []).map(describeRule).join("; ")}>
                            <Badge variant="outline">
                              {p.containers?.length} container rule
                              {p.containers?.length === 1 ? "" : "s"}
                            </Badge>
                          </Hint>
                        )}
                        <span className="text-xs text-muted-foreground tabular-nums">
                          priority {p.priority}
                        </span>
                      </span>
                    </TableCell>
                    <TableCell className="max-w-64 truncate text-muted-foreground">
                      {scopeText(p)}
                    </TableCell>
                    <TableCell>{p.mode}</TableCell>
                    <TableCell className="text-muted-foreground">{p.apply || "InPlace"}</TableCell>
                    <TableCell className="text-muted-foreground">{p.season || "daily"}</TableCell>
                    <TableCell numeric>
                      {formatNumber(Math.round((p.cpuMargin || 0) * 100))}%
                    </TableCell>
                    <TableCell className="text-muted-foreground">{guardText(p) || "—"}</TableCell>
                    <TableCell className="pr-4 text-right" onClick={(e) => e.stopPropagation()}>
                      <Hint text="Delete policy">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`Delete policy ${p.name}`}
                          onClick={() => setConfirmDel(p.name)}
                        >
                          <Trash2 aria-hidden />
                        </Button>
                      </Hint>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </Card>
      </section>

      <Sheet open={editing != null} onOpenChange={(o) => !o && setEditing(null)}>
        <SheetContent side="right" className="w-full gap-0 sm:max-w-2xl">
          <SheetHeader className="border-b border-border">
            <SheetTitle>{isNew ? "New policy" : `Edit “${editing?.name}”`}</SheetTitle>
            <SheetDescription>
              A policy selects workloads (namespaces / labels) and sets how they are sized: mode,
              method, headroom, guardrails and forecast.
            </SheetDescription>
          </SheetHeader>
          {editing && <PolicyForm value={editing} onChange={setEditing} />}
          <SheetFooter className="sticky bottom-0 flex-row justify-end border-t border-border bg-popover">
            {save.isError && (
              <InfoBox tone="danger" role="alert" className="mr-auto" title="Not saved">
                {save.error instanceof Error ? save.error.message : String(save.error)}
              </InfoBox>
            )}
            <Button variant="outline" onClick={() => setEditing(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => editing && save.mutate(editing)}
              disabled={!editing?.name}
              loading={save.isPending}
            >
              {isNew ? "Create policy" : "Save policy"}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      <ConfirmDialog
        open={confirmDel != null}
        onOpenChange={(o) => !o && setConfirmDel(null)}
        tone="destructive"
        title={`Delete policy “${confirmDel}”?`}
        description="Workloads in its scope fall back to lower-priority policies or the default. The TailorPolicy resource is removed from the cluster; this cannot be undone."
        confirmLabel="Delete policy"
        onConfirm={async () => {
          if (confirmDel) await del.mutateAsync(confirmDel);
        }}
      />
    </>
  );
}

function PolicyForm({
  value,
  onChange,
}: {
  value: PolicyView;
  onChange: (v: PolicyView) => void;
}) {
  const [exampleUsed, setExampleUsed] = useState(500); // example usage (millicores) for the preview
  const exampleId = useId();
  const allowId = useId();
  const headroom = Math.round((value.cpuMargin || 0) * 100);
  const rec = Math.round(exampleUsed * (1 + (value.cpuMargin || 0)));
  const set = (patch: Partial<PolicyView>) => onChange({ ...value, ...patch });
  const lblKey = Object.keys(value.matchLabels ?? {})[0] ?? "";
  const lblVal = lblKey ? (value.matchLabels ?? {})[lblKey] : "";
  const sched = value.schedule ?? { timezone: "UTC", outsideMode: "Off", windows: [] };
  const win: WindowView = sched.windows?.[0] ?? {};
  const hasContent = (w: WindowView) => !!(w.start || w.end || w.from || w.until || w.days?.length);
  const setWin = (patch: Partial<WindowView>) => {
    const w = { ...win, ...patch };
    set({ schedule: { ...sched, windows: hasContent(w) ? [w] : [] } });
  };
  const list = parseList;

  return (
    <div className="flex flex-col gap-8 p-6">
      <FormSection title="Identity and scope">
        <Field label="Name" hint="Saving under a new name creates another policy">
          <Input
            placeholder="e.g. apps"
            value={value.name}
            onChange={(e) => set({ name: e.target.value })}
          />
        </Field>
        <Field label="Priority" hint="Higher wins when policies overlap">
          <Input
            type="number"
            value={value.priority}
            onChange={(e) => set({ priority: Number(e.target.value) })}
          />
        </Field>
        <Field label="Profile" hint="Reusable base (optional)">
          <Input
            placeholder="profile name"
            value={value.profileRef}
            onChange={(e) => set({ profileRef: e.target.value })}
          />
        </Field>
        <Field label="Namespaces" hint="Comma-separated">
          <ListInput
            placeholder="apps, data"
            value={value.namespaces ?? []}
            onChange={(namespaces) => set({ namespaces })}
          />
        </Field>
        <Field label="Match label key">
          <Input
            placeholder="tier"
            value={lblKey}
            onChange={(e) =>
              set({ matchLabels: e.target.value ? { [e.target.value]: lblVal } : {} })
            }
          />
        </Field>
        <Field label="Match label value">
          <Input
            placeholder="database"
            value={lblVal}
            disabled={!lblKey}
            onChange={(e) => set({ matchLabels: lblKey ? { [lblKey]: e.target.value } : {} })}
          />
        </Field>
      </FormSection>

      <FormSection title="Actuation and intelligence">
        <Field label="Mode" hint="Whether to act">
          <OptionSelect value={value.mode} options={MODES} onChange={(v) => set({ mode: v })} />
        </Field>
        <Field label="Method" hint="How to act">
          <OptionSelect
            value={value.apply || "InPlace"}
            options={APPLY_METHODS}
            onChange={(v) => set({ apply: v })}
          />
        </Field>
        <Field label="Seasonal forecast">
          <OptionSelect
            value={value.season || "daily"}
            options={SEASONS}
            onChange={(v) => set({ season: v })}
          />
        </Field>
        <Field label="Decay half-life" hint="Shorter reacts faster">
          <Input
            placeholder="24h"
            value={value.halfLife}
            onChange={(e) => set({ halfLife: e.target.value })}
          />
        </Field>
      </FormSection>

      <section className="flex flex-col gap-4">
        <h3 className="text-base font-semibold tracking-tight">Sizing</h3>
        <Slider
          label="Headroom — how far the request sits above measured usage"
          min={0}
          max={80}
          step={1}
          value={[headroom]}
          formatValue={(v) => `${v}%`}
          marks={[
            { value: 0, label: "Tight 0%" },
            { value: 80, label: "Safe 80%" },
          ]}
          onValueChange={([v]) => set({ cpuMargin: v / 100, memMargin: v / 100 })}
        />
        <div className="flex flex-col gap-3 rounded-xl border border-border bg-muted p-4">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <Label htmlFor={exampleId} className="font-normal text-muted-foreground">
              Example usage
            </Label>
            <Input
              id={exampleId}
              type="number"
              className="h-control-sm w-24 tabular-nums"
              value={exampleUsed}
              onChange={(e) => setExampleUsed(Math.max(1, Number(e.target.value)))}
            />
            <span className="text-muted-foreground">millicores → request</span>
            <b className="font-semibold tabular-nums">{formatNumber(rec)}m</b>
          </div>
          <div aria-hidden className="flex h-3 overflow-hidden rounded-full bg-neutral-6">
            <span className="bg-chart-1" style={{ width: `${(exampleUsed / rec) * 100}%` }} />
            <span className="flex-1 bg-chart-2" />
          </div>
          <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground tabular-nums">
            <li className="flex items-center gap-1.5">
              <span aria-hidden className="size-2.5 rounded-[3px] bg-chart-1" />
              Usage {formatNumber(exampleUsed)}m
            </li>
            <li className="flex items-center gap-1.5">
              <span aria-hidden className="size-2.5 rounded-[3px] bg-chart-2" />
              Headroom {formatNumber(rec - exampleUsed)}m
            </li>
          </ul>
        </div>
        <div className="grid gap-4 sm:grid-cols-3">
          <Field label="CPU min">
            <Input
              placeholder="10m"
              value={value.cpuMin}
              onChange={(e) => set({ cpuMin: e.target.value })}
            />
          </Field>
          <Field label="CPU max">
            <Input
              placeholder="none"
              value={value.cpuMax}
              onChange={(e) => set({ cpuMax: e.target.value })}
            />
          </Field>
          <Field label="CPU limit ×" hint="0 = no limit">
            <Input
              type="number"
              step={0.1}
              value={value.cpuLimitFactor}
              onChange={(e) => set({ cpuLimitFactor: Number(e.target.value) })}
            />
          </Field>
          <Field label="Memory min">
            <Input
              placeholder="64Mi"
              value={value.memMin}
              onChange={(e) => set({ memMin: e.target.value })}
            />
          </Field>
          <Field label="Memory max">
            <Input
              placeholder="none"
              value={value.memMax}
              onChange={(e) => set({ memMax: e.target.value })}
            />
          </Field>
          <Field label="Memory limit ×" hint="× request">
            <Input
              type="number"
              step={0.1}
              value={value.memLimitFactor}
              onChange={(e) => set({ memLimitFactor: Number(e.target.value) })}
            />
          </Field>
        </div>
        <div className="flex items-start gap-3">
          <Checkbox
            id={allowId}
            className="mt-0.5"
            checked={value.allowExceedLimit}
            onCheckedChange={(c) => set({ allowExceedLimit: c === true })}
          />
          <div className="grid gap-1">
            <Label htmlFor={allowId}>Allow growing above the current limit</Label>
            <p className="text-xs text-muted-foreground">
              Off keeps workloads bounded; OOMKills then surface as “needs higher limit”.
            </p>
          </div>
        </div>
      </section>

      <ContainerRules
        rules={value.containers ?? []}
        onChange={(containers) => set({ containers })}
      />

      <FormSection title="Schedule and campaigns" description="Optional">
        <Field label="Outside the window" hint="Mode while inactive">
          <OptionSelect
            value={sched.outsideMode || "Off"}
            options={MODES}
            onChange={(v) => set({ schedule: { ...sched, outsideMode: v } })}
          />
        </Field>
        <Field label="Timezone">
          <Input
            placeholder="UTC"
            value={sched.timezone || ""}
            onChange={(e) => set({ schedule: { ...sched, timezone: e.target.value } })}
          />
        </Field>
        <Field label="Days" hint="Blank = every day">
          <Input
            placeholder="Mon, Tue, Wed"
            value={(win.days ?? []).join(", ")}
            onChange={(e) => setWin({ days: list(e.target.value) })}
          />
        </Field>
        <Field label="Daily start" hint="HH:MM, 24h">
          <Input
            placeholder="09:00"
            inputMode="numeric"
            pattern="[0-2][0-9]:[0-5][0-9]"
            value={win.start ?? ""}
            onChange={(e) => setWin({ start: e.target.value })}
          />
        </Field>
        <Field label="Daily end" hint="HH:MM, 24h">
          <Input
            placeholder="18:00"
            inputMode="numeric"
            pattern="[0-2][0-9]:[0-5][0-9]"
            value={win.end ?? ""}
            onChange={(e) => setWin({ end: e.target.value })}
          />
        </Field>
        <div />
        <Field label="Campaign from" hint="e.g. a seasonal event">
          <DateField value={win.from ?? ""} onChange={(v) => setWin({ from: v })} />
        </Field>
        <Field label="Campaign until">
          <DateField value={win.until ?? ""} onChange={(v) => setWin({ until: v })} />
        </Field>
      </FormSection>
      <InfoBox tone="info">
        Inside the window the policy's mode applies; outside it,{" "}
        <b className="font-semibold">{sched.outsideMode || "Off"}</b>. For a one-off event set only
        the campaign dates and a high headroom — the policy stays active for those dates, then
        reverts on its own.
      </InfoBox>
    </div>
  );
}

function FormSection({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children: ReactNode;
}) {
  return (
    <section className="flex flex-col gap-4">
      <div className="flex items-baseline gap-2">
        <h3 className="text-base font-semibold tracking-tight">{title}</h3>
        {description && <span className="text-sm text-muted-foreground">{description}</span>}
      </div>
      <div className="grid gap-4 sm:grid-cols-3">{children}</div>
    </section>
  );
}
