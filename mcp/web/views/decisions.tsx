import { SummaryPanel } from "../components/advisor";
import { Activity, ChevronDown, ChevronRight, RotateCw, Search, Zap } from "lucide-react";
import { Fragment, useId, useState } from "react";
import { RangePicker, rangeArgs } from "../components/range-picker";
import { Hint } from "../components/workload-badges";
import { ago, change, fullDate, mib, milli } from "../lib/fmt";
import { FILTER_ATTR } from "../lib/shortcuts";
import type { Decision } from "../lib/types";
import { Badge } from "../ui/badge";
import { Banner } from "../ui/banner";
import { Button } from "../ui/button";
import { Card } from "../ui/card";
import { DataField, DataGrid } from "../ui/data-field";
import { EmptyState } from "../ui/empty-state";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { SectionHeader } from "../ui/section-header";
import { Skeleton } from "../ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { useTool } from "../use-tool.ts";

const MODE_ICON: Record<string, typeof Activity> = {
  inplace: Activity,
  rollout: RotateCw,
  ksvc: RotateCw,
  webhook: Zap,
};
const MODE_DESC: Record<string, string> = {
  inplace: "In-place resize — no restart, no eviction (pods/resize subresource)",
  rollout: "Controller rollout — native updateStrategy, respects PDB and ordering",
  ksvc: "Knative ksvc — a durable resize mints a new revision",
  webhook: "Admission webhook — applied to the pod on (re)create",
};

export function DecisionsView({
  range,
  onRange,
}: { range: string; onRange: (id: string) => void }) {
  const filterId = useId();
  const [q, setQ] = useState("");
  const [limit, setLimit] = useState(50);
  const [open, setOpen] = useState<number | null>(null);
  const acts = useTool<{ events: Decision[] | null; durable?: boolean }>(
    "TAILOR_LIST_ACTUATIONS",
    { ...rangeArgs(range), q: q || undefined, limit },
    { refetchInterval: 10_000 },
  );
  const events = acts.data?.events ?? [];

  return (
    <>
      <h1 className="sr-only">Decisions</h1>
      <SummaryPanel />
      <section aria-labelledby="decisions-title" className="flex flex-col gap-4">
        <SectionHeader
          id="decisions-title"
          title="Decisions applied"
          description="Every resize the tailor applied, newest first. Expand a row for the exact change."
          action={
            acts.data?.durable ? (
              <Hint text="History is stored in ClickHouse and survives restarts">
                <Badge variant="success-soft">Durable history</Badge>
              </Hint>
            ) : undefined
          }
        />
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-wrap items-center gap-3">
            <Label htmlFor={filterId} className="text-muted-foreground">
              Filter
            </Label>
            <div className="relative w-72 max-w-full">
              <Search
                aria-hidden
                className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
              />
              <Input
                id={filterId}
                className="h-control-sm pl-9"
                placeholder="workload, namespace or method"
                {...{ [FILTER_ATTR]: "" }}
                value={q}
                onChange={(e) => {
                  setQ(e.target.value);
                  setLimit(50);
                  setOpen(null);
                }}
              />
            </div>
          </div>
          <RangePicker value={range} onChange={onRange} />
        </div>
        {acts.isError && (
          <Banner
            variant="danger"
            role="alert"
            title="Could not load decisions"
            description={acts.error instanceof Error ? acts.error.message : String(acts.error)}
          />
        )}
        <Card className="gap-0 py-0">
          {acts.isLoading ? (
            <div role="status" aria-label="Loading decisions" className="flex flex-col gap-3 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : events.length === 0 ? (
            <EmptyState
              illustration="inbox"
              title={q ? `No decisions match “${q}”` : "No changes applied in this range"}
              description="An automated workload's request has to drift from the recommendation before the tailor applies a resize. Try a wider range."
            />
          ) : (
            <Table aria-label="Decisions applied">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-10 pl-4">
                    <span className="sr-only">Details</span>
                  </TableHead>
                  <TableHead>When</TableHead>
                  <TableHead>Workload</TableHead>
                  <TableHead>Method</TableHead>
                  <TableHead>CPU</TableHead>
                  <TableHead className="pr-4">Memory</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {events.map((e, i) => {
                  const isOpen = open === i;
                  const Icon = MODE_ICON[e.mode] ?? Activity;
                  const detailId = `decision-${i}`;
                  return (
                    <Fragment key={`${e.time}-${e.namespace}-${e.workload}-${e.container}-${i}`}>
                      <TableRow interactive onClick={() => setOpen(isOpen ? null : i)}>
                        <TableCell className="pl-4">
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-expanded={isOpen}
                            aria-controls={isOpen ? detailId : undefined}
                            aria-label={`${isOpen ? "Hide" : "Show"} details for ${e.namespace}/${e.workload}`}
                            onClick={(ev) => {
                              ev.stopPropagation();
                              setOpen(isOpen ? null : i);
                            }}
                          >
                            {isOpen ? <ChevronDown aria-hidden /> : <ChevronRight aria-hidden />}
                          </Button>
                        </TableCell>
                        <TableCell className="text-muted-foreground">
                          <Hint text={fullDate(e.time)}>
                            <span>{ago(e.time)}</span>
                          </Hint>
                        </TableCell>
                        <TableCell className="whitespace-normal">
                          <span className="font-medium">
                            <span className="text-muted-foreground">{e.namespace}/</span>
                            {e.workload}
                          </span>
                          <span className="text-muted-foreground"> · {e.container}</span>
                        </TableCell>
                        <TableCell>
                          <Hint text={MODE_DESC[e.mode] ?? e.mode}>
                            <Badge variant="secondary">
                              <Icon aria-hidden />
                              {e.mode}
                            </Badge>
                          </Hint>
                        </TableCell>
                        <TableCell>
                          <Change
                            from={milli(e.fromCpu)}
                            to={milli(e.toCpu)}
                            pct={change(e.fromCpu, e.toCpu)}
                            down={e.toCpu <= e.fromCpu}
                          />
                        </TableCell>
                        <TableCell className="pr-4">
                          <Change
                            from={mib(e.fromMem)}
                            to={mib(e.toMem)}
                            pct={change(e.fromMem, e.toMem)}
                            down={e.toMem <= e.fromMem}
                          />
                        </TableCell>
                      </TableRow>
                      {isOpen && (
                        <TableRow className="bg-muted hover:bg-muted">
                          <TableCell
                            colSpan={6}
                            className="px-4 py-4 whitespace-normal"
                            id={detailId}
                          >
                            <DataGrid className="sm:grid-cols-3">
                              <DataField label="Method" value={MODE_DESC[e.mode] ?? e.mode} />
                              <DataField
                                label="Workload"
                                value={`${e.kind} ${e.namespace}/${e.workload} · container ${e.container}`}
                              />
                              <DataField numeric label="Applied at" value={fullDate(e.time)} />
                              <DataField
                                numeric
                                label="CPU request"
                                value={`${milli(e.fromCpu)} → ${milli(e.toCpu)} (${change(e.fromCpu, e.toCpu)}, ${e.toCpu <= e.fromCpu ? "reduced" : "raised"})`}
                              />
                              <DataField
                                numeric
                                label="Memory request"
                                value={`${mib(e.fromMem)} → ${mib(e.toMem)} (${change(e.fromMem, e.toMem)}, ${e.toMem <= e.fromMem ? "reduced" : "raised"})`}
                              />
                            </DataGrid>
                          </TableCell>
                        </TableRow>
                      )}
                    </Fragment>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </Card>
        {events.length >= limit && (
          <div className="flex justify-center">
            <Button variant="outline" size="sm" onClick={() => setLimit((n) => n + 50)}>
              Show 50 more
            </Button>
          </div>
        )}
      </section>
    </>
  );
}

/** "500m → 250m −50%": direction by arrow word + sign, colour only reinforces. */
function Change({ from, to, pct, down }: { from: string; to: string; pct: string; down: boolean }) {
  return (
    <span className="inline-flex items-center gap-1.5 tabular-nums">
      <span className="text-muted-foreground">{from}</span>
      <span aria-hidden className="text-muted-foreground">
        →
      </span>
      <span className="sr-only">to</span>
      <span className="font-medium">{to}</span>
      <span
        className={`text-xs font-medium ${from === to ? "text-muted-foreground" : down ? "text-success-text" : "text-warning-text"}`}
      >
        {pct}
      </span>
    </span>
  );
}
