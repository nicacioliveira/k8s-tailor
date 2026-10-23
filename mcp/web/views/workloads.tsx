import {
  type UseQueryResult,
  useMutation,
  useQueries,
  useQueryClient,
} from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowRight,
  ArrowUp,
  ArrowUpDown,
  Hourglass,
  Loader2,
  RotateCcw,
  Search,
  TrendingDown,
  TrendingUp,
  X,
} from "lucide-react";
import { useId, useState } from "react";
import { ContainerControls } from "../components/container-controls";
import { ModeOrigin, PathLine, SidecarOptIn } from "../components/container-info";
import { ExportMenu } from "../components/export-menu";
import { RangePicker, rangeArgs } from "../components/range-picker";
import { FIT_CONFIG, FIT_LAYERS, ResourceChart } from "../components/resource-chart";
import {
  ActuationBadges,
  ClassBadge,
  Hint,
  KindBadges,
  StatusBadge,
} from "../components/workload-badges";
import { useMcpApp } from "../context.tsx";
import { heldHint } from "../lib/confidence";
import {
  CLASS_OPTIONS,
  classOf,
  groupSiblings,
  isInjectedSidecar,
  isNeverSized,
  originText,
} from "../lib/containers";
import { MI, keyOf, mib, milli, usd, workloadKey } from "../lib/fmt";
import { isPendingFor, useSetWorkload } from "../lib/mutations";
import { type FitPoint, addRollingFit, addWasteBand, withUsage } from "../lib/series";
import { FILTER_ATTR } from "../lib/shortcuts";
import { STATUSES } from "../lib/status";
import {
  type Detail,
  GROUP_DEFAULT,
  LIST_KEY,
  type ListResp,
  type OverviewResp,
  type Rec,
} from "../lib/types";
import {
  EMPTY_FILTERS,
  type SortKey,
  type WorkloadFilters,
  distinct,
  filterRecs,
  isFiltered,
  modeOf,
  parseFilters,
  serializeFilters,
  sortRecs,
} from "../lib/workload-filter";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "../ui/card";
import type { ChartConfig } from "../ui/chart";
import { Checkbox } from "../ui/checkbox";
import { ConfirmDialog } from "../ui/confirm-dialog";
import { EmptyState } from "../ui/empty-state";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { formatNumber } from "../ui/lib/format";
import { SectionHeader } from "../ui/section-header";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Skeleton } from "../ui/skeleton";
import { Switch } from "../ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { ToggleGroup, ToggleGroupItem } from "../ui/toggle-group";
import { callTool, useTool } from "../use-tool.ts";
import { WorkloadSheet } from "./workload-sheet";

/** Charts carry at most 4 series; comparing more becomes unreadable. */
const MAX_COMPARE = 4;

export function WorkloadsView({
  list,
  ov,
  params,
  onParams,
  range,
  onRange,
  onError,
}: {
  list: UseQueryResult<ListResp | null>;
  ov: UseQueryResult<OverviewResp | null>;
  params: URLSearchParams;
  onParams: (p: URLSearchParams) => void;
  range: string;
  onRange: (id: string) => void;
  onError: (e: unknown) => void;
}) {
  const app = useMcpApp();
  const qc = useQueryClient();
  const filterId = useId();
  const profiles = useTool<{ profiles: string[] | null }>(
    "TAILOR_LIST_PROFILES",
    {},
    { refetchInterval: 30_000 },
  );
  const [compare, setCompare] = useState<string[]>([]);
  const filters = parseFilters(params);
  const setFilters = (patch: Partial<WorkloadFilters>) => {
    onParams(serializeFilters({ ...filters, ...patch }));
    setLimit(100);
  };
  const [limit, setLimit] = useState(100);
  const [selKey, setSelKey] = useState<string | null>(null);
  const [bulkMode, setBulkMode] = useState<null | "InPlace" | "Recommend">(null);
  const setWl = useSetWorkload(onError);
  const bulk = useMutation({
    mutationFn: async (mode: string) => {
      const recs = qc.getQueryData<ListResp>(LIST_KEY)?.recommendations ?? [];
      const seen = new Set<string>();
      const targets = recs.filter((r) => {
        const k = workloadKey(r);
        if (seen.has(k)) return false;
        seen.add(k);
        return true;
      });
      await Promise.all(
        targets.map((r) =>
          callTool(app, "TAILOR_SET_WORKLOAD", {
            namespace: r.namespace,
            kind: r.kind,
            workload: r.workload,
            mode,
          }),
        ),
      );
    },
    onSettled: () => qc.invalidateQueries({ queryKey: LIST_KEY }),
  });

  const data = list.data;
  const recs = data?.recommendations ?? [];
  const profileNames = profiles.data?.profiles ?? [];
  const matched = sortRecs(filterRecs(recs, filters), filters.sort, filters.dir);
  const filtered = isFiltered(filters);
  const sortBy = (key: SortKey) =>
    setFilters(
      filters.sort !== key
        ? { sort: key, dir: key === "workload" ? "asc" : "desc" }
        : filters.dir === (key === "workload" ? "asc" : "desc")
          ? { dir: key === "workload" ? "desc" : "asc" }
          : { sort: "", dir: "desc" },
    );
  const shown = matched.slice(0, limit);
  const sel = selKey ? recs.find((r) => keyOf(r) === selKey) : undefined;
  const togglePick = (k: string) =>
    setCompare((cur) =>
      cur.includes(k) ? cur.filter((x) => x !== k) : cur.length >= MAX_COMPARE ? cur : [...cur, k],
    );

  return (
    <>
      <h1 className="sr-only">Workloads</h1>
      <CompareChart
        refs={recs.filter((r) => compare.includes(keyOf(r)))}
        clusterUsage={ov.data?.overview.usage ?? []}
        range={range}
        onRange={onRange}
        onClear={() => setCompare([])}
      />

      <section aria-labelledby="workloads-title" className="flex flex-col gap-4">
        <SectionHeader
          id="workloads-title"
          title="Workloads"
          description={`Requests now → recommended. Tick up to ${MAX_COMPARE} rows to overlay them on the chart; click a row for the full analysis.`}
          actions={[
            <ExportMenu key="export" recs={matched} filtered={filtered} />,
            <Button
              key="rec"
              variant="outline"
              size="sm"
              disabled={bulk.isPending || recs.length === 0}
              onClick={() => setBulkMode("Recommend")}
            >
              Set all to recommend-only
            </Button>,
            <Button
              key="auto"
              size="sm"
              disabled={bulk.isPending || recs.length === 0}
              onClick={() => setBulkMode("InPlace")}
            >
              Automate all
            </Button>,
          ]}
        />
        {recs.length > 0 && (
          <FilterBar
            filterId={filterId}
            recs={recs}
            filters={filters}
            onChange={setFilters}
            onClear={() => {
              onParams(
                serializeFilters({ ...EMPTY_FILTERS, sort: filters.sort, dir: filters.dir }),
              );
              setLimit(100);
            }}
            note={
              compare.length > 0 ? `${compare.length} of ${MAX_COMPARE} on the chart` : undefined
            }
          />
        )}

        <Card className="gap-0 py-0">
          {!data && list.isLoading ? (
            <TableSkeleton />
          ) : recs.length === 0 ? (
            <EmptyState
              illustration="list"
              title="No workloads with enough metrics yet"
              description="Recommendations appear once usage accumulates — usually a few minutes after the tailor starts watching a namespace."
            />
          ) : matched.length === 0 ? (
            <EmptyState
              illustration="search"
              title={
                filters.q.trim()
                  ? `Nothing matches “${filters.q.trim()}”`
                  : "No workloads match these filters"
              }
              description="Loosen a filter, or search by namespace, workload name or kind (Deployment, StatefulSet…)."
              action={
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() =>
                    onParams(
                      serializeFilters({ ...EMPTY_FILTERS, sort: filters.sort, dir: filters.dir }),
                    )
                  }
                >
                  Clear filters
                </Button>
              }
            />
          ) : (
            <Table aria-label="Workloads">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-10 pl-4">
                    <span className="sr-only">Compare on chart</span>
                  </TableHead>
                  <SortHead k="workload" label="Workload" f={filters} onSort={sortBy} />
                  <SortHead k="cpu" label="CPU request" f={filters} onSort={sortBy} />
                  <SortHead k="mem" label="Memory request" f={filters} onSort={sortBy} />
                  <TableHead>Limits</TableHead>
                  <TableHead>Profile</TableHead>
                  <TableHead>Automated</TableHead>
                  <SortHead
                    k="waste"
                    label="$ / week"
                    f={filters}
                    onSort={sortBy}
                    numeric
                    className="pr-4"
                  />
                </TableRow>
              </TableHeader>
              <TableBody>
                {shown.map((r) => {
                  const k = keyOf(r);
                  const picked = compare.includes(k);
                  const pending = isPendingFor(setWl, r);
                  return (
                    <TableRow
                      key={k}
                      interactive
                      data-state={picked ? "selected" : undefined}
                      onClick={() => setSelKey(k)}
                      className="align-top"
                    >
                      <TableCell className="pl-4" onClick={(e) => e.stopPropagation()}>
                        <Checkbox
                          checked={picked}
                          disabled={!picked && compare.length >= MAX_COMPARE}
                          onCheckedChange={() => togglePick(k)}
                          aria-label={`Compare ${r.namespace}/${r.workload} on the chart`}
                        />
                      </TableCell>
                      <TableCell className="min-w-56 py-2.5 whitespace-normal">
                        <button
                          type="button"
                          className="text-left font-medium text-foreground outline-none hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
                          onClick={(e) => {
                            e.stopPropagation();
                            setSelKey(k);
                          }}
                        >
                          <span className="text-muted-foreground">{r.namespace}/</span>
                          {r.workload}
                          <span className="font-normal text-muted-foreground">
                            {" "}
                            · {r.container}
                          </span>
                        </button>
                        <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
                          <ClassBadge r={r} />
                          {!isNeverSized(r) && <StatusBadge r={r} />}
                          <KindBadges r={r} />
                          <ActuationBadges r={r} stat={data?.actuationCounts?.[k]} />
                        </div>
                        <div className="mt-1.5 flex flex-col items-start gap-1">
                          <PathLine r={r} />
                          <SidecarOptIn r={r} compact />
                          {heldHint(r) && !isNeverSized(r) && (
                            <Hint text="The other resource is recommended and actuated normally.">
                              <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                                <Hourglass aria-hidden className="size-3.5 shrink-0" />
                                {heldHint(r)}
                              </span>
                            </Hint>
                          )}
                        </div>
                      </TableCell>
                      {isNeverSized(r) ? (
                        <TableCell colSpan={3} className="text-sm text-muted-foreground">
                          Not sized: {classOf(r) === "init" ? "init" : "ephemeral"} containers
                          cannot be resized.
                        </TableCell>
                      ) : (
                        <>
                          <TableCell>
                            <Delta
                              drift={r.specDrift ? milli(r.current.cpuRequest) : undefined}
                              from={milli(r.applied.cpuRequest)}
                              to={milli(r.recommended.cpuRequest)}
                            />
                          </TableCell>
                          <TableCell>
                            <Delta
                              drift={r.specDrift ? mib(r.current.memRequest) : undefined}
                              from={mib(r.applied.memRequest)}
                              to={mib(r.recommended.memRequest)}
                            />
                          </TableCell>
                          <TableCell>
                            <LimitCell r={r} />
                          </TableCell>
                        </>
                      )}
                      <TableCell onClick={(e) => e.stopPropagation()}>
                        {isInjectedSidecar(r) || isNeverSized(r) ? (
                          <Hint text="The workload profile does not apply to this container: injected sidecars use the built-in sidecar profile unless a container rule or annotation sets another.">
                            <span className="text-xs text-muted-foreground">
                              {isInjectedSidecar(r) ? "Sidecar profile" : "—"}
                            </span>
                          </Hint>
                        ) : (
                          <Select
                            value={r.profile || GROUP_DEFAULT}
                            disabled={pending}
                            onValueChange={(v) =>
                              setWl.mutate({
                                namespace: r.namespace,
                                kind: r.kind,
                                workload: r.workload,
                                profile: v === GROUP_DEFAULT ? "" : v,
                              })
                            }
                          >
                            <SelectTrigger
                              size="sm"
                              className="w-36"
                              aria-label={`Profile for ${r.workload}`}
                            >
                              <SelectValue />
                            </SelectTrigger>
                            <SelectContent>
                              <SelectItem value={GROUP_DEFAULT}>Policy default</SelectItem>
                              {profileNames.map((p) => (
                                <SelectItem key={p} value={p}>
                                  {p}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                        )}
                      </TableCell>
                      <TableCell onClick={(e) => e.stopPropagation()}>
                        {isInjectedSidecar(r) || isNeverSized(r) ? (
                          <div className="flex flex-col items-start gap-1.5">
                            <ModeOrigin r={r} />
                            <ContainerControls
                              r={r}
                              compact
                              onError={(m) => onError(new Error(m))}
                            />
                          </div>
                        ) : (
                          <div className="flex flex-col items-start gap-1">
                            <AutomateControl
                              r={r}
                              pending={pending}
                              onSet={(mode) =>
                                setWl.mutate({
                                  namespace: r.namespace,
                                  kind: r.kind,
                                  workload: r.workload,
                                  mode,
                                })
                              }
                            />
                            {r.containerPolicy && r.containerPolicy !== "policy" && (
                              <span className="text-xs text-muted-foreground">
                                This container: {originText(r)}
                              </span>
                            )}
                          </div>
                        )}
                      </TableCell>
                      <TableCell numeric className="pr-4">
                        <WeeklyCost value={r.wasteUsdWeek} />
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </Card>
        {recs.length > 0 && matched.length > 0 && (
          <div className="flex flex-wrap items-center gap-3 text-sm text-muted-foreground tabular-nums">
            <span>
              Showing {formatNumber(shown.length)} of {formatNumber(matched.length)}
              {matched.length !== recs.length
                ? ` (filtered from ${formatNumber(recs.length)})`
                : ""}
            </span>
            {shown.length < matched.length && (
              <Button variant="outline" size="sm" onClick={() => setLimit((n) => n + 100)}>
                Show 100 more
              </Button>
            )}
          </div>
        )}
      </section>

      <ConfirmDialog
        open={bulkMode != null}
        onOpenChange={(o) => !o && setBulkMode(null)}
        title={
          bulkMode === "InPlace"
            ? "Automate every workload?"
            : "Set every workload to recommend-only?"
        }
        description={
          bulkMode === "InPlace"
            ? "Each workload gets an annotation enabling automated in-place resizing, bounded by its policy and guardrails. Injected sidecars (istio-proxy…) stay at Recommend until opted in per container. The global pause switch still overrides everything."
            : "Each workload is set to recommend-only — the tailor keeps computing recommendations but applies no changes."
        }
        confirmLabel={bulkMode === "InPlace" ? "Automate all" : "Set all to recommend-only"}
        onConfirm={() => bulk.mutateAsync(bulkMode ?? "Recommend")}
      />

      {sel && (
        <WorkloadSheet
          rec={sel}
          range={range}
          onRange={onRange}
          profiles={profileNames}
          siblings={groupSiblings(
            recs.filter(
              (r) =>
                r.namespace === sel.namespace && r.kind === sel.kind && r.workload === sel.workload,
            ),
          )}
          onSelect={(r) => setSelKey(keyOf(r))}
          onClose={() => setSelKey(null)}
          onError={onError}
        />
      )}
    </>
  );
}

const ALL = "__all__";

function FilterSelect({
  label,
  value,
  options,
  onChange,
  allLabel,
}: {
  label: string;
  value: string;
  options: { value: string; label: string }[];
  onChange: (v: string) => void;
  allLabel: string;
}) {
  const id = useId();
  return (
    <div className="flex items-center gap-2 max-sm:w-full">
      <Label htmlFor={id} className="text-muted-foreground max-sm:w-24">
        {label}
      </Label>
      <Select value={value || ALL} onValueChange={(v) => onChange(v === ALL ? "" : v)}>
        <SelectTrigger id={id} size="sm" className="w-40 max-sm:w-auto max-sm:flex-1">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={ALL}>{allLabel}</SelectItem>
          {options.map((o) => (
            <SelectItem key={o.value} value={o.value}>
              {o.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

function FilterBar({
  filterId,
  recs,
  filters,
  onChange,
  onClear,
  note,
}: {
  filterId: string;
  recs: Rec[];
  filters: WorkloadFilters;
  onChange: (patch: Partial<WorkloadFilters>) => void;
  onClear: () => void;
  note?: string;
}) {
  const oomId = useId();
  const pinId = useId();
  const opts = (vals: string[]) => vals.map((v) => ({ value: v, label: v }));
  const classes = new Set(recs.map(classOf));
  const meshes = distinct(recs, (r) => r.mesh ?? "");
  return (
    <div role="search" aria-label="Filter workloads" className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-3">
        <Label htmlFor={filterId} className="text-muted-foreground">
          Search
        </Label>
        <div className="relative w-72 max-w-full max-sm:w-full">
          <Search
            aria-hidden
            className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
          />
          <Input
            id={filterId}
            type="search"
            className="h-control-sm pl-9"
            placeholder="namespace, workload or kind"
            aria-keyshortcuts="/"
            value={filters.q}
            onChange={(e) => onChange({ q: e.target.value })}
            {...{ [FILTER_ATTR]: "" }}
          />
        </div>
        {note && (
          <span className="text-sm text-muted-foreground tabular-nums" role="status">
            {note}
          </span>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-3">
        <FilterSelect
          label="Namespace"
          allLabel="All namespaces"
          value={filters.ns}
          options={opts(distinct(recs, (r) => r.namespace))}
          onChange={(ns) => onChange({ ns })}
        />
        <FilterSelect
          label="Status"
          allLabel="Any status"
          value={filters.status}
          options={STATUSES.map((s) => ({ value: s.id, label: s.label }))}
          onChange={(v) => onChange({ status: v as WorkloadFilters["status"] })}
        />
        <FilterSelect
          label="Kind"
          allLabel="All kinds"
          value={filters.kind}
          options={opts(distinct(recs, (r) => r.kind))}
          onChange={(kind) => onChange({ kind })}
        />
        <FilterSelect
          label="Mode"
          allLabel="Any mode"
          value={filters.mode}
          options={opts(distinct(recs, modeOf))}
          onChange={(mode) => onChange({ mode })}
        />
        <FilterSelect
          label="Container"
          allLabel="Any container"
          value={filters.cls}
          options={CLASS_OPTIONS.filter((o) => classes.has(o.value))}
          onChange={(cls) => onChange({ cls })}
        />
        {meshes.length > 0 && (
          <FilterSelect
            label="Mesh"
            allLabel="Any mesh"
            value={filters.mesh}
            options={opts(meshes)}
            onChange={(mesh) => onChange({ mesh })}
          />
        )}
        <div className="flex items-center gap-2">
          <Checkbox
            id={oomId}
            checked={filters.oom}
            onCheckedChange={(v) => onChange({ oom: v === true })}
          />
          <Label htmlFor={oomId} className="font-normal">
            Only OOM risk
          </Label>
        </div>
        <div className="flex items-center gap-2">
          <Checkbox
            id={pinId}
            checked={filters.pinned}
            onCheckedChange={(v) => onChange({ pinned: v === true })}
          />
          <Label htmlFor={pinId} className="font-normal">
            Only pinned
          </Label>
        </div>
        {isFiltered(filters) && (
          <Button variant="ghost" size="sm" onClick={onClear}>
            <X aria-hidden />
            Clear filters
          </Button>
        )}
      </div>
    </div>
  );
}

/** Column header that sorts; announces its state through aria-sort. */
function SortHead({
  k,
  label,
  f,
  onSort,
  numeric,
  className,
}: {
  k: SortKey;
  label: string;
  f: WorkloadFilters;
  onSort: (k: SortKey) => void;
  numeric?: boolean;
  className?: string;
}) {
  const active = f.sort === k;
  const Icon = !active ? ArrowUpDown : f.dir === "asc" ? ArrowUp : ArrowDown;
  return (
    <TableHead
      numeric={numeric}
      className={className}
      aria-sort={active ? (f.dir === "asc" ? "ascending" : "descending") : "none"}
    >
      <button
        type="button"
        onClick={() => onSort(k)}
        className={`-mx-1 inline-flex items-center gap-1 rounded-md px-1 py-0.5 outline-none hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring ${active ? "text-foreground" : ""}`}
      >
        {label}
        <Icon aria-hidden className="size-3.5" />
      </button>
    </TableHead>
  );
}

function Delta({ drift, from, to }: { drift?: string; from: string; to: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap tabular-nums">
      {drift && drift !== from && (
        <Hint text="Workload spec (template) — the in-place change is not persisted yet">
          <s className="text-muted-foreground">{drift}</s>
        </Hint>
      )}
      <Hint text="Running now (live pod)">
        <span className="text-muted-foreground">{from}</span>
      </Hint>
      <ArrowRight aria-label="to" className="size-3.5 text-muted-foreground" />
      <span className="font-medium text-foreground">{to}</span>
    </span>
  );
}

function LimitCell({ r }: { r: Rec }) {
  const cpuCur = r.current.cpuLimit ?? 0;
  const cpuRec = r.recommended.cpuLimit ?? 0;
  const memCur = r.current.memLimit ?? 0;
  const memRec = r.recommended.memLimit ?? 0;
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-2 gap-y-1 text-xs tabular-nums">
      <dt className="text-muted-foreground">CPU</dt>
      <dd>
        {cpuCur === 0 && cpuRec === 0 ? (
          <Hint text="No CPU limit — avoids throttling (best practice)">
            <span className="text-muted-foreground">none</span>
          </Hint>
        ) : (
          `${milli(cpuCur || undefined)} → ${milli(cpuRec || undefined)}`
        )}
      </dd>
      <dt className="text-muted-foreground">Mem</dt>
      <dd>
        {mib(memCur || undefined)} → {mib(memRec || undefined)}
      </dd>
    </dl>
  );
}

/** Positive = reclaimable (saving); negative = more needed. Sign + icon + word, not colour alone. */
export function WeeklyCost({ value }: { value: number }) {
  const save = value >= 0;
  return (
    <Hint
      text={
        save
          ? "Reclaimable per week if the recommendation is applied"
          : "Extra cost per week to give it what it needs"
      }
    >
      <span
        className={`inline-flex items-center justify-end gap-1 font-medium tabular-nums ${save ? "text-success-text" : "text-destructive-text"}`}
      >
        {save ? (
          <TrendingDown aria-hidden className="size-3.5" />
        ) : (
          <TrendingUp aria-hidden className="size-3.5" />
        )}
        {save ? usd(value) : `+${usd(-value)}`}
        <span className="sr-only">{save ? " reclaimable" : " more needed"}</span>
      </span>
    </Hint>
  );
}

export function AutomateControl({
  r,
  pending,
  onSet,
}: {
  r: Rec;
  pending: boolean;
  onSet: (mode: string) => void;
}) {
  return (
    <span className="inline-flex items-center gap-2">
      <Hint
        text={
          r.overridden
            ? `Pinned on this workload: ${r.automated ? `automated (${r.mode})` : r.mode}. Overrides the policy.`
            : `Inherited from policy ${r.policy || "default"}: ${r.automated ? "automated" : "recommend-only"}. Flip to pin an override.`
        }
      >
        <span>
          <Switch
            checked={r.automated}
            disabled={pending}
            onCheckedChange={() => onSet(r.automated ? "Recommend" : "Both")}
            aria-label={`Automate ${r.workload}`}
          />
        </span>
      </Hint>
      {r.overridden ? (
        <>
          <Badge variant="info-soft">Pinned</Badge>
          <Hint text="Follow the policy again (clear the per-workload override)">
            <Button
              variant="ghost"
              size="icon-sm"
              disabled={pending}
              aria-label={`Follow policy for ${r.workload}`}
              onClick={() => onSet("")}
            >
              <RotateCcw aria-hidden />
            </Button>
          </Hint>
        </>
      ) : (
        <span className="text-xs text-muted-foreground">from policy</span>
      )}
      {pending && (
        <Loader2
          aria-label="Saving"
          role="status"
          className="size-4 animate-spin text-muted-foreground"
        />
      )}
    </span>
  );
}

function TableSkeleton() {
  return (
    <div role="status" aria-label="Loading workloads" className="flex flex-col gap-3 p-4">
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className="flex items-center gap-4">
          <Skeleton className="size-4" />
          <Skeleton className="h-9 flex-1" />
          <Skeleton className="h-9 w-24" />
          <Skeleton className="h-9 w-24" />
          <Skeleton className="h-9 w-40" />
          <Skeleton className="h-9 w-16" />
        </div>
      ))}
    </div>
  );
}

function CompareChart({
  refs,
  clusterUsage,
  range,
  onRange,
  onClear,
}: {
  refs: Rec[];
  clusterUsage: { t: number; cpu: number; mem: number }[];
  range: string;
  onRange: (id: string) => void;
  onClear: () => void;
}) {
  const app = useMcpApp();
  const [metric, setMetric] = useState<"cpu" | "mem">("cpu");
  const results = useQueries({
    queries: refs.map((r) => ({
      queryKey: ["cmp", keyOf(r), range],
      queryFn: () =>
        callTool<Detail>(app, "TAILOR_GET_DETAIL", {
          namespace: r.namespace,
          kind: r.kind,
          workload: r.workload,
          container: r.container,
          ...rangeArgs(range),
        }),
      enabled: !!app,
      refetchInterval: 15_000,
    })),
  });
  const loading = results.some((q) => q.isLoading);
  const scale = (v: number) => (metric === "cpu" ? v * 1000 : v / MI);
  const unit = metric === "cpu" ? "m" : "Mi";
  const mode = refs.length === 0 ? "cluster" : refs.length === 1 ? "single" : "multi";

  const byT = new Map<number, FitPoint>();
  const put = (arr: { T: number; V: number }[] | null | undefined, key: string) => {
    for (const p of arr ?? []) {
      const e = byT.get(p.T) ?? { t: p.T };
      e[key] = +scale(p.V).toFixed(1);
      byT.set(p.T, e);
    }
  };
  let config: ChartConfig = { used: { label: "Cluster usage", color: 1 } };
  let layers = [{ key: "used", kind: "area" as "stack" | "area" | "line" }];
  let events: number[] = [];

  if (mode === "cluster") {
    for (const p of clusterUsage) {
      const e = byT.get(p.t) ?? { t: p.t };
      e.used = +scale(metric === "cpu" ? p.cpu : p.mem).toFixed(1);
      byT.set(p.t, e);
    }
  } else if (mode === "single") {
    const d = results[0]?.data;
    put(metric === "cpu" ? d?.cpuTrend : d?.memTrend, "used");
    put(metric === "cpu" ? d?.cpuRec : d?.memRec, "recommended");
    put(metric === "cpu" ? d?.cpuCur : d?.memCur, "current");
    config = FIT_CONFIG;
    layers = FIT_LAYERS;
    events = (d?.events ?? []).map((e) => e.time);
  } else {
    config = {};
    layers = [];
    refs.forEach((r, i) => {
      const key = `s${i}`;
      config[key] = { label: `${r.namespace}/${r.workload}`, color: (i + 1) as 1 | 2 | 3 | 4 };
      layers.push({ key, kind: "area" });
      put(metric === "cpu" ? results[i]?.data?.cpuTrend : results[i]?.data?.memTrend, key);
    });
  }
  let data = [...byT.values()].sort((a, b) => a.t - b.t);
  if (mode === "single") data = withUsage(addWasteBand(addRollingFit(data)));

  const title =
    mode === "cluster"
      ? "Cluster usage"
      : mode === "single"
        ? `${refs[0].namespace}/${refs[0].workload}`
        : `Comparing ${refs.length} workloads`;

  return (
    <Card className="gap-4">
      <CardHeader>
        <CardTitle as="h2">{title}</CardTitle>
        <CardDescription>
          {mode === "cluster"
            ? "Tick workloads in the table below to compare them here."
            : mode === "single"
              ? "Usage, safety margin and reclaimable waste up to the current request; the line is the recommendation."
              : "Usage of each selected workload."}
        </CardDescription>
        <CardAction className="flex flex-wrap items-center justify-end gap-2">
          <ToggleGroup
            type="single"
            value={metric}
            onValueChange={(v) => v && setMetric(v as "cpu" | "mem")}
            aria-label="Metric"
          >
            <ToggleGroupItem value="cpu">CPU</ToggleGroupItem>
            <ToggleGroupItem value="mem">Memory</ToggleGroupItem>
          </ToggleGroup>
          {refs.length > 0 && (
            <Button variant="ghost" size="sm" onClick={onClear}>
              <X aria-hidden />
              Clear selection
            </Button>
          )}
        </CardAction>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <RangePicker value={range} onChange={onRange} />
        <ResourceChart
          data={data}
          unit={unit}
          config={config}
          layers={layers}
          events={events}
          loading={loading && data.length === 0}
          summary={`${title}: ${metric === "cpu" ? "CPU in millicores" : "memory in MiB"} over the selected range.`}
        />
      </CardContent>
    </Card>
  );
}
