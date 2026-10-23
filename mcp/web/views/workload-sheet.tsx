import { useMutation } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import { type ReactNode, useId, useState } from "react";
import { ReviewPanel } from "../components/advisor";
import { ConfidenceBadges } from "../components/confidence-badges";
import { ContainerList, SidecarOptIn } from "../components/container-info";
import { CopyButton } from "../components/copy-button";
import { RangePicker, rangeArgs } from "../components/range-picker";
import { FIT_CONFIG, FIT_LAYERS, type RefLine, ResourceChart } from "../components/resource-chart";
import { ClassBadge, KindBadges, StatusBadge, pdbText } from "../components/workload-badges";
import { useMcpApp } from "../context.tsx";
import {
  classHint,
  effectiveMode,
  isInjectedSidecar,
  isNeverSized,
  originText,
  pathText,
} from "../lib/containers";
import { heldHint, heldResource } from "../lib/confidence";
import { MI, mib, milli, pct1 } from "../lib/fmt";
import { type PatchPlan, kubectlCommands } from "../lib/kubectl";
import { isPendingFor, useSetWorkload } from "../lib/mutations";
import { addRollingFit, addWasteBand, mergeSeries, withUsage } from "../lib/series";
import { type Detail, GROUP_DEFAULT, MODES, type Rec } from "../lib/types";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { DataField, DataGrid } from "../ui/data-field";
import { InfoBox } from "../ui/info-box";
import { Label } from "../ui/label";
import { formatNumber } from "../ui/lib/format";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "../ui/sheet";
import { Skeleton } from "../ui/skeleton";
import { Switch } from "../ui/switch";
import { callTool, useTool } from "../use-tool.ts";

export function WorkloadSheet({
  rec,
  range,
  onRange,
  profiles,
  siblings,
  onSelect,
  onClose,
  onError,
}: {
  rec: Rec;
  range: string;
  onRange: (id: string) => void;
  profiles: string[];
  /** Every container of this workload, `rec` included (the AI review can target any of them). */
  siblings: Rec[];
  onSelect: (r: Rec) => void;
  onClose: () => void;
  onError: (e: unknown) => void;
}) {
  const app = useMcpApp();
  const ids = { profile: useId(), mode: useId(), auto: useId() };
  const [showPatch, setShowPatch] = useState(false);
  const [aiContainer, setAiContainer] = useState(rec.container);
  const detail = useTool<Detail>("TAILOR_GET_DETAIL", {
    namespace: rec.namespace,
    kind: rec.kind,
    workload: rec.workload,
    container: rec.container,
    ...rangeArgs(range),
  });
  const setWl = useSetWorkload(onError);
  const pending = isPendingFor(setWl, rec);
  const set = (b: { profile?: string; mode?: string }) =>
    setWl.mutate({ namespace: rec.namespace, kind: rec.kind, workload: rec.workload, ...b });
  const patch = useMutation({
    mutationFn: () =>
      callTool(app, "TAILOR_PREVIEW_PATCH", {
        namespace: rec.namespace,
        kind: rec.kind,
        workload: rec.workload,
        container: rec.container,
      }),
  });

  const s = rec.signals;
  const d = detail.data;
  const cpu = withUsage(
    addWasteBand(addRollingFit(mergeSeries(d?.cpuTrend, d?.cpuRec, d?.cpuCur, (v) => v * 1000))),
  );
  const mem = withUsage(
    addWasteBand(addRollingFit(mergeSeries(d?.memTrend, d?.memRec, d?.memCur, (v) => v / MI))),
  );
  const events = (d?.events ?? []).map((e) => e.time);

  const refs = (
    unit: string,
    request: number,
    limit: number | undefined,
    pLabel: string,
    p: number,
    forecast: number | undefined,
  ): RefLine[] => {
    const out: RefLine[] = [];
    const same = !!limit && limit === request;
    if (request > 0)
      out.push({
        value: request,
        label: `${same ? "Request = limit" : "Request"} ${formatNumber(request)}${unit}`,
      });
    if (limit && limit > 0 && !same)
      out.push({ value: limit, label: `Limit ${formatNumber(limit)}${unit}`, dash: "2 4" });
    out.push({
      value: p,
      label: `${pLabel} ${formatNumber(p)}${unit}`,
      dash: "1 3",
      position: "start",
    });
    if (forecast && forecast > 0)
      out.push({
        value: forecast,
        label: `Forecast ${formatNumber(forecast)}${unit}`,
        dash: "8 3",
        position: "start",
      });
    return out;
  };

  const heldBy = [rec.cpuHeldByHpa && "CPU", rec.memHeldByHpa && "memory"]
    .filter(Boolean)
    .join(" and ");

  return (
    <Sheet open onOpenChange={(o) => !o && onClose()}>
      <SheetContent side="right" className="w-full gap-0 sm:max-w-3xl">
        <SheetHeader className="border-b border-border">
          <SheetTitle className="break-all">
            <span className="text-muted-foreground">{rec.namespace}/</span>
            {rec.workload}
            <span className="font-normal text-muted-foreground"> · {rec.container}</span>
          </SheetTitle>
          <SheetDescription asChild>
            <div className="mt-1 flex flex-wrap items-center gap-1.5">
              <ClassBadge r={rec} />
              {!isNeverSized(rec) && <StatusBadge r={rec} />}
              <KindBadges r={rec} />
            </div>
          </SheetDescription>
        </SheetHeader>

        <div className="flex flex-col gap-6 p-6">
          <div className="flex flex-col gap-2">
            <InfoBox tone="muted" icon={false}>
              <b className="font-semibold">{rec.kind}</b> · {rec.strategy || "default strategy"} ·{" "}
              {pdbText(rec)}
              {rec.pdbProtected && rec.pdbAllowed <= 0 && " — disruptive memory resize is held"}
              {rec.knative?.service && (
                <>
                  <br />
                  Knative <b className="font-semibold">{rec.knative.service}</b> (revision{" "}
                  {rec.knative.revision}) — a durable resize edits the ksvc and mints a new
                  revision.
                </>
              )}
            </InfoBox>
            <SidecarOptIn r={rec} />
            {heldBy && (
              <InfoBox tone="warning" title={`HPA ${rec.autoscaler.name} scales on ${heldBy}`}>
                {rec.cpuHeldByHpa && rec.memHeldByHpa
                  ? "Both requests are held — no vertical actuation."
                  : `The ${rec.cpuHeldByHpa ? "CPU" : "memory"} request is held; only ${rec.cpuHeldByHpa ? "memory" : "CPU"} is managed.`}
              </InfoBox>
            )}
            {rec.oomRisk && (
              <InfoBox tone="danger" title="Recently OOMKilled">
                Memory was grown to recover.
              </InfoBox>
            )}
            {rec.limitCapped && (
              <InfoBox tone="warning" title="Capped at the current memory limit">
                Needs a higher limit — or enable “allow growing above the limit” on its policy.
              </InfoBox>
            )}
            {!isNeverSized(rec) && (
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
                <span className="text-sm font-medium">Confidence</span>
                <ConfidenceBadges r={rec} />
              </div>
            )}
            {heldResource(rec) && (
              <InfoBox tone="info" title={heldHint(rec) ?? ""}>
                {heldResource(rec) === "cpu" ? "Memory" : "CPU"} is recommended and actuated
                normally; the held resource keeps its current request until it has enough samples (
                {formatNumber(s.samples)} so far).
              </InfoBox>
            )}
            {!rec.confident && (
              <InfoBox tone="info" title="Low confidence">
                Only {formatNumber(s.samples)} samples so far — recommending, not actuating yet.
              </InfoBox>
            )}
          </div>

          <section aria-labelledby="containers-title" className="flex flex-col gap-3">
            <h3 id="containers-title" className="text-base font-semibold tracking-tight">
              Containers
            </h3>
            <ContainerList recs={siblings} current={rec} onSelect={onSelect} />
            <p className="text-xs text-muted-foreground">
              Mode and exclusion are written per container as the annotations{" "}
              <code className="break-all">tailor.k8s-tailor.dev/container.&lt;name&gt;.mode</code>{" "}
              and <code>.exclude</code> on the workload. Inherit removes the annotation, so the
              policy decides. The list comes from the engine&apos;s last refresh (up to a minute
              old). Prefer GitOps? Copy the annotation from each container instead.
            </p>
          </section>

          <section
            aria-label="Controls"
            className="grid gap-4 rounded-xl border border-border p-4 sm:grid-cols-4"
          >
            <div className="grid gap-2">
              <Label htmlFor={ids.profile}>Profile</Label>
              <Select
                value={rec.profile || GROUP_DEFAULT}
                disabled={pending}
                onValueChange={(v) => set({ profile: v === GROUP_DEFAULT ? "" : v })}
              >
                <SelectTrigger id={ids.profile} size="sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={GROUP_DEFAULT}>Policy default</SelectItem>
                  {profiles.map((p) => (
                    <SelectItem key={p} value={p}>
                      {p}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {isInjectedSidecar(rec) || isNeverSized(rec) ? (
              <>
                <DataField
                  label="Mode (this container)"
                  value={`${effectiveMode(rec)} · ${originText(rec)}`}
                />
                <DataField label="Method (from policy)" value={rec.apply || "InPlace"} />
                <DataField
                  label="Automated"
                  value={
                    <span className="inline-flex items-center gap-1.5">
                      <Badge variant={rec.automated ? "success-soft" : "outline"}>
                        {rec.automated ? "On" : "Off"}
                      </Badge>
                      <span className="text-xs font-normal text-muted-foreground">
                        workload toggle does not apply
                      </span>
                    </span>
                  }
                />
              </>
            ) : (
              <>
                <div className="grid gap-2">
                  <Label htmlFor={ids.mode}>Mode (workload)</Label>
                  <Select
                    value={rec.mode || "Recommend"}
                    disabled={pending}
                    onValueChange={(v) => set({ mode: v })}
                  >
                    <SelectTrigger id={ids.mode} size="sm">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {MODES.map((m) => (
                        <SelectItem key={m} value={m}>
                          {m}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <DataField label="Method (from policy)" value={rec.apply || "InPlace"} />
                <div className="grid gap-2">
                  <Label htmlFor={ids.auto}>Automated</Label>
                  <div className="flex h-control-sm items-center gap-2">
                    <Switch
                      id={ids.auto}
                      checked={rec.automated}
                      disabled={pending}
                      onCheckedChange={() => set({ mode: rec.automated ? "Recommend" : "Both" })}
                    />
                    <span className="text-sm text-muted-foreground">
                      {pending ? "Saving…" : rec.automated ? "On" : "Off"}
                    </span>
                    {rec.overridden && <Badge variant="info-soft">Pinned</Badge>}
                  </div>
                </div>
              </>
            )}
          </section>
          {(isInjectedSidecar(rec) || isNeverSized(rec)) && (
            <p className="-mt-3 text-xs text-muted-foreground">{classHint(rec)}</p>
          )}

          <ReviewPanel
            namespace={rec.namespace}
            kind={rec.kind}
            workload={rec.workload}
            container={aiContainer}
            containers={siblings.length ? siblings.map((x) => x.container) : [rec.container]}
            onContainer={setAiContainer}
          />

          <div className="flex flex-wrap items-center justify-between gap-3">
            <h3 className="text-base font-semibold tracking-tight">Usage and fit</h3>
            <RangePicker value={range} onChange={onRange} />
          </div>
          {detail.isError && (
            <InfoBox tone="danger" title="Could not load the series">
              {detail.error instanceof Error ? detail.error.message : String(detail.error)}
            </InfoBox>
          )}
          <FitSection title="CPU" description="Millicores">
            <ResourceChart
              data={cpu}
              unit="m"
              config={FIT_CONFIG}
              layers={FIT_LAYERS}
              loading={detail.isLoading}
              events={events}
              refs={refs(
                "m",
                Math.round(rec.current.cpuRequest * 1000),
                rec.current.cpuLimit ? Math.round(rec.current.cpuLimit * 1000) : undefined,
                "p90",
                Math.round(s.cpuQuantile * 1000),
                s.cpuForecast != null ? Math.round(s.cpuForecast * 1000) : undefined,
              )}
              summary={`CPU of ${rec.workload}: usage, safety margin and reclaimable waste up to the request of ${milli(rec.current.cpuRequest)}; recommended ${milli(rec.recommended.cpuRequest)}.`}
            />
          </FitSection>
          <FitSection title="Memory" description="MiB">
            <ResourceChart
              data={mem}
              unit="Mi"
              config={FIT_CONFIG}
              layers={FIT_LAYERS}
              loading={detail.isLoading}
              events={events}
              refs={refs(
                "Mi",
                Math.round(rec.current.memRequest / MI),
                rec.current.memLimit ? Math.round(rec.current.memLimit / MI) : undefined,
                "p95",
                Math.round(s.memQuantile / MI),
                s.memForecastBytes != null ? Math.round(s.memForecastBytes / MI) : undefined,
              )}
              summary={`Memory of ${rec.workload}: usage, safety margin and reclaimable waste up to the request of ${mib(rec.current.memRequest)}; recommended ${mib(rec.recommended.memRequest)}.`}
            />
          </FitSection>

          <section aria-labelledby="reasoning" className="flex flex-col gap-4">
            <h3 id="reasoning" className="text-base font-semibold tracking-tight">
              Why this recommendation
            </h3>
            <DataGrid className="sm:grid-cols-3">
              <DataField
                numeric
                label="CPU p90 (decay-weighted)"
                value={`${milli(s.cpuQuantile)} · peak ${milli(s.cpuPeak)}`}
              />
              <DataField
                numeric
                label="CPU request"
                value={`${milli(rec.applied.cpuRequest)} → ${milli(rec.recommended.cpuRequest)}`}
              />
              <DataField
                numeric
                label="CPU limit"
                value={rec.recommended.cpuLimit ? milli(rec.recommended.cpuLimit) : "none"}
              />
              <DataField
                numeric
                label="Memory p95 (decay-weighted)"
                value={`${mib(s.memQuantile)} · peak ${mib(s.memPeak)}`}
              />
              <DataField
                numeric
                label="Memory request (anti-OOM floor)"
                value={`${mib(rec.applied.memRequest)} → ${mib(rec.recommended.memRequest)}`}
              />
              <DataField numeric label="Memory limit" value={mib(rec.recommended.memLimit)} />
              <DataField
                numeric
                label="Seasonal forecast"
                value={
                  s.cpuForecast == null && s.memForecastBytes == null
                    ? null
                    : `CPU ${milli(s.cpuForecast)} · mem ${mib(s.memForecastBytes)}`
                }
              />
              <DataField
                numeric
                label="Memory 24h trend"
                value={s.memTrendBytes == null ? null : mib(s.memTrendBytes)}
              />
              <DataField
                numeric
                label="Headroom breach (share of window above rec.)"
                value={
                  <span className="inline-flex flex-wrap items-center gap-1.5">
                    CPU {pct1(s.headroomBreachCpu)} · mem {pct1(s.headroomBreachMem)}
                    {(s.headroomBreachCpu > 0.05 || s.headroomBreachMem > 0.05) && (
                      <Badge variant="destructive-soft">Above 5%</Badge>
                    )}
                  </span>
                }
              />
              {s.burstiness != null && (
                <DataField
                  numeric
                  label="CPU burstiness (p99 / p90)"
                  value={
                    <span
                      className="inline-flex flex-wrap items-center gap-1.5"
                      title={s.burstReason}
                    >
                      {s.burstiness.toFixed(1)} · sized at p
                      {Math.round((s.cpuQuantileUsed ?? 0) * 100)}
                      {s.burstQuantileRaised && <Badge variant="info-soft">Quantile raised</Badge>}
                    </span>
                  }
                />
              )}
              {s.burstReason && <DataField label="Quantile choice" value={s.burstReason} />}
              {(s.cpuCensored || s.cpuGreedy) && (
                <DataField
                  numeric
                  label="CPU demand"
                  value={
                    <span className="inline-flex flex-wrap items-center gap-1.5">
                      {s.cpuGreedy
                        ? `Parked after ${s.cpuCensorSteps ?? 0} limit raises`
                        : `Pinned at the CPU limit · ${s.cpuCensorSteps ?? 0} raises`}
                      {s.cpuGreedy && <Badge variant="warning-soft">CPU-greedy</Badge>}
                    </span>
                  }
                />
              )}
              <DataField
                label="Actuation"
                value={`${effectiveMode(rec)} · method ${rec.apply || "InPlace"}`}
              />
              <DataField label="Acts via" value={pathText(rec)} />
              <DataField label="Container policy" value={originText(rec)} />
              <DataField
                numeric
                label="Samples · replicas"
                value={`${formatNumber(s.samples)} · ${formatNumber(rec.replicas)}`}
              />
              <DataField label="Policy" value={rec.policy || "default"} />
            </DataGrid>
          </section>

          <section className="flex flex-col gap-3">
            <Button
              variant="ghost"
              size="sm"
              className="-ml-3 w-fit"
              aria-expanded={showPatch}
              onClick={() => {
                setShowPatch((v) => !v);
                if (!patch.data && !patch.isPending) patch.mutate();
              }}
            >
              {showPatch ? <ChevronDown aria-hidden /> : <ChevronRight aria-hidden />}
              Dry-run patch — the exact change as kubectl, not applied
            </Button>
            {showPatch &&
              (patch.isPending ? (
                <Skeleton className="h-40 w-full" />
              ) : patch.isError ? (
                <InfoBox tone="danger" title="Could not compute the patch">
                  {patch.error instanceof Error ? patch.error.message : String(patch.error)}
                </InfoBox>
              ) : patch.data ? (
                <PatchCommands rec={rec} plan={patch.data as PatchPlan} />
              ) : null)}
          </section>
        </div>
      </SheetContent>
    </Sheet>
  );
}

function PatchCommands({ rec, plan }: { rec: Rec; plan: PatchPlan }) {
  const dryId = useId();
  const [dryRun, setDryRun] = useState(true);
  const cmds = kubectlCommands(rec, plan, dryRun);
  const json = JSON.stringify(plan, null, 2);
  const preferInPlace = (plan.method || rec.apply || "InPlace") !== "Rollout";
  const blocks = [
    cmds.inPlace && {
      id: "inplace",
      title: "In-place resize (no restart)",
      hint: "Patches one running pod's resources through the resize subresource. Replace POD and repeat per pod; it does not survive a rollout.",
      text: cmds.inPlace,
    },
    cmds.rollout && {
      id: "rollout",
      title: `Template patch on ${rec.kind}`,
      hint: "Persists the change in the workload spec; the controller rolls pods with its update strategy.",
      text: cmds.rollout,
    },
  ].filter(Boolean) as { id: string; title: string; hint: string; text: string }[];
  if (!preferInPlace) blocks.reverse();
  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-2">
        <Switch id={dryId} checked={dryRun} onCheckedChange={setDryRun} />
        <Label htmlFor={dryId} className="font-normal">
          Server-side dry run — adds <code>--dry-run=server</code>, validates without changing
          anything
        </Label>
      </div>
      {blocks.map((b, i) => (
        <section key={b.id} aria-labelledby={`${dryId}-${b.id}`} className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h4 id={`${dryId}-${b.id}`} className="text-sm font-semibold">
              {b.title}
              {i === 0 && (
                <Badge variant="info-soft" className="ml-2 align-middle">
                  Policy method
                </Badge>
              )}
            </h4>
            <CopyButton text={b.text} label="Copy command" />
          </div>
          <p className="text-xs text-muted-foreground">{b.hint}</p>
          <pre className="overflow-x-auto rounded-xl border border-border bg-muted p-4 text-foreground whitespace-pre-wrap break-all">
            {b.text}
          </pre>
        </section>
      ))}
      {cmds.rolloutNote && (
        <InfoBox tone="warning" title={`No kubectl template patch for ${rec.kind}`}>
          {cmds.rolloutNote}
        </InfoBox>
      )}
      <details className="group">
        <summary className="cursor-pointer rounded-md text-sm text-muted-foreground outline-none hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring">
          Raw patch from the engine (JSON)
        </summary>
        <div className="mt-2 flex flex-col gap-2">
          <CopyButton text={json} label="Copy JSON" />
          <pre className="max-h-96 overflow-auto rounded-xl border border-border bg-muted p-4 text-foreground">
            {json}
          </pre>
        </div>
      </details>
    </div>
  );
}

function FitSection({
  title,
  description,
  children,
}: { title: string; description: string; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-2">
      <div className="flex items-baseline gap-2">
        <h4 className="text-sm font-semibold">{title}</h4>
        <span className="text-xs text-muted-foreground">{description}</span>
      </div>
      {children}
    </section>
  );
}
