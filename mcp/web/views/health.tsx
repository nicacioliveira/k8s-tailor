import type { UseQueryResult } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { ago, fullDate } from "../lib/fmt";
import type { ActuationsResp, EconomyResp, HealthResp, ListResp, OverviewResp } from "../lib/types";
import { Button } from "../ui/button";
import { Card } from "../ui/card";
import { DataField, DataGrid } from "../ui/data-field";
import { InfoBox } from "../ui/info-box";
import { formatNumber } from "../ui/lib/format";
import { SectionHeader } from "../ui/section-header";
import { Skeleton } from "../ui/skeleton";
import { StatusIndicator, type SystemStatus } from "../ui/status-indicator";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { useTool } from "../use-tool.ts";

// Engine health, derived ONLY from what the API already returns. What the engine does
// not expose (last recompute, per-source error log…) is listed at the bottom instead of
// being guessed — see docs/DESIGN.md "Backend gaps".

type Check = {
  name: string;
  status: SystemStatus;
  label: string;
  detail: string;
  checkedAt?: number; // ms, when this browser last got an answer
};

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));
const seconds = (ms: number) => Math.floor(ms / 1000);

function fromQuery<T>(
  q: UseQueryResult<T | null>,
  name: string,
  ok: (d: T) => Omit<Check, "name" | "checkedAt">,
): Check {
  if (q.isLoading) return { name, status: "syncing", label: "Checking", detail: "" };
  if (q.isError)
    return {
      name,
      status: "error",
      label: "Failing",
      detail: errText(q.error),
      checkedAt: q.errorUpdatedAt || undefined,
    };
  if (!q.data) return { name, status: "offline", label: "No data", detail: "Empty answer" };
  return { name, ...ok(q.data), checkedAt: q.dataUpdatedAt || undefined };
}

export function HealthView({
  list,
  ov,
}: {
  list: UseQueryResult<ListResp | null>;
  ov: UseQueryResult<OverviewResp | null>;
}) {
  const health = useTool<HealthResp>("TAILOR_HEALTH", {}, { refetchInterval: 15_000 });
  const eco = useTool<EconomyResp>("TAILOR_ECONOMY", {}, { refetchInterval: 15_000 });
  const acts = useTool<ActuationsResp>(
    "TAILOR_LIST_ACTUATIONS",
    { limit: 1 },
    { refetchInterval: 30_000 },
  );

  const recs = list.data?.recommendations ?? [];
  const lowConfidence = recs.filter((r) => !r.confident).length;
  const o = ov.data;
  const h = health.data;

  const checks: Check[] = [
    fromQuery(health, "Engine API", (d) => ({
      status: d.ok ? "online" : "error",
      label: d.ok ? "Answering" : "Not ok",
      detail: `/healthz · metrics source “${d.source}”`,
    })),
    health.isError
      ? {
          name: "VictoriaMetrics",
          status: "offline" as SystemStatus,
          label: "Unknown",
          detail: "Reported by the engine, which isn't answering",
        }
      : fromQuery(health, "VictoriaMetrics", (d) => ({
          status: d.vmHealthy ? "online" : "error",
          label: d.vmHealthy ? "Healthy" : "Unreachable",
          detail: d.vmHealthy
            ? d.vmUrl
            : `${d.vmUrl} — usage, trends and savings series can't be read`,
        })),
    fromQuery(list, "Recommendations", (d) => ({
      status: d.count > 0 ? (lowConfidence > 0 ? "warning" : "online") : "warning",
      label: d.count > 0 ? (lowConfidence > 0 ? "Partly warming up" : "Current") : "None yet",
      detail:
        d.count > 0
          ? `${formatNumber(d.count)} containers${lowConfidence ? ` · ${formatNumber(lowConfidence)} still gathering samples` : ""}`
          : "No workload has enough metrics yet",
    })),
    fromQuery(ov, "Cluster usage series", (d) => {
      const n = d.overview.usage?.length ?? 0;
      return {
        status: n > 0 ? "online" : "warning",
        label: n > 0 ? "Flowing" : "Empty",
        detail:
          n > 0
            ? `${formatNumber(n)} points in the current range`
            : "No usage samples in the selected range",
      };
    }),
    fromQuery(eco, "Savings series", (d) => {
      const n = d.series?.length ?? 0;
      return {
        status: n > 0 ? "online" : "warning",
        label: n > 0 ? "Flowing" : "Empty",
        detail:
          n > 0
            ? `${formatNumber(n)} points over the last 6 hours`
            : "tailor_waste_usd_week has no samples in VictoriaMetrics yet",
      };
    }),
    fromQuery(acts, "Decision log", (d) => ({
      status: d.durable ? "online" : "warning",
      label: d.durable ? "Durable" : "In memory",
      detail: d.durable
        ? "ClickHouse — history survives restarts"
        : "ClickHouse not configured or failing — history is lost on restart",
    })),
    fromQuery(ov, "Pricing", (d) => ({
      status: d.pricingSource === "static" ? "warning" : "online",
      label: d.pricingSource === "static" ? "Estimate" : "Measured",
      detail:
        d.pricingSource === "static"
          ? "Fixed default prices — savings are estimates, not your bill"
          : `${d.pricingSource} node costs`,
    })),
  ];

  const anyLoading = health.isLoading || list.isLoading || ov.isLoading;

  return (
    <>
      <h1 className="sr-only">Health</h1>
      <section aria-labelledby="health-posture" className="flex flex-col gap-4">
        <SectionHeader
          id="health-posture"
          title="Engine"
          description="Actuation posture and cluster capabilities, as the engine reports them."
          action={
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                health.refetch();
                eco.refetch();
                acts.refetch();
                list.refetch();
                ov.refetch();
              }}
            >
              <RefreshCw aria-hidden className={health.isFetching ? "animate-spin" : undefined} />
              Check again
            </Button>
          }
        />
        <Card className="p-6">
          {anyLoading && !o && !h ? (
            <div role="status" aria-label="Loading" className="grid gap-4 sm:grid-cols-3">
              {[0, 1, 2, 3, 4, 5].map((i) => (
                <Skeleton key={i} className="h-10" />
              ))}
            </div>
          ) : (
            <DataGrid className="sm:grid-cols-3">
              <DataField
                label="Actuation"
                value={
                  o ? (
                    <StatusIndicator
                      status={
                        o.paused ? "warning" : o.actuation === "active" ? "online" : "offline"
                      }
                      label={
                        o.paused ? "Paused" : o.actuation === "active" ? "Actuating" : "Dry-run"
                      }
                      description={
                        o.paused
                          ? "global kill switch on"
                          : o.actuation === "active"
                            ? undefined
                            : "recommending only"
                      }
                    />
                  ) : null
                }
              />
              <DataField
                numeric
                label="Automated workloads"
                value={
                  o
                    ? `${formatNumber(o.automated)} of ${formatNumber(o.overview.workloads)} (${formatNumber(o.automatedPct)}%)`
                    : null
                }
              />
              <DataField
                label="In-place pod resize"
                value={
                  o
                    ? o.inPlaceSupported
                      ? "Supported"
                      : "Not available — falls back to rollout"
                    : null
                }
              />
              <DataField label="Metrics source" value={h?.source ?? null} />
              <DataField
                numeric
                label="Last answer from the engine"
                value={
                  list.dataUpdatedAt
                    ? `${ago(seconds(list.dataUpdatedAt))} · ${fullDate(seconds(list.dataUpdatedAt))}`
                    : null
                }
              />
              <DataField
                numeric
                label="Pricing basis"
                value={
                  o?.priceCpuCoreHr != null && o.priceGbHr != null
                    ? `$${formatNumber(o.priceCpuCoreHr, 4)}/core-hr · $${formatNumber(o.priceGbHr, 4)}/GiB-hr`
                    : (o?.pricingSource ?? null)
                }
              />
            </DataGrid>
          )}
        </Card>
      </section>

      <section aria-labelledby="health-sources" className="flex flex-col gap-4">
        <SectionHeader
          id="health-sources"
          title="Data sources"
          description="Each row is checked from this browser through the engine's API; “checked” is when it last answered."
        />
        <Card className="gap-0 py-0">
          <Table aria-label="Data sources">
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Source</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Detail</TableHead>
                <TableHead className="pr-4">Checked</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {checks.map((c) => (
                <TableRow key={c.name}>
                  <TableCell className="pl-4 font-medium">{c.name}</TableCell>
                  <TableCell>
                    <StatusIndicator status={c.status} label={c.label} />
                  </TableCell>
                  <TableCell className="min-w-64 whitespace-normal text-muted-foreground">
                    {c.detail}
                  </TableCell>
                  <TableCell className="pr-4 text-muted-foreground">
                    {c.checkedAt ? ago(seconds(c.checkedAt)) : "—"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
        <InfoBox tone="info" title="Not reported by the engine yet">
          The API has no field for: when recommendations were last recomputed, the collection loop's
          last error per source (Kubernetes, VictoriaMetrics, OpenCost, ClickHouse), webhook health,
          or engine version and uptime. The states above are derived from what the API does return,
          so a source can look healthy here while its last background run failed.
        </InfoBox>
      </section>
    </>
  );
}
