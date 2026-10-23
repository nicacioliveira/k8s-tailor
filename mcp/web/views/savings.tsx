import { SummaryPanel } from "../components/advisor";
import type { UseQueryResult } from "@tanstack/react-query";
import { ResourceChart } from "../components/resource-chart";
import { usd } from "../lib/fmt";
import { buildHash } from "../lib/hash-route";
import {
  type Prices,
  appliedUsdWeek,
  neededUsdWeek,
  savingsTimeline,
  topWaste,
} from "../lib/savings";
import type { ActuationsResp, EconomyResp, ListResp, OverviewResp } from "../lib/types";
import { Banner } from "../ui/banner";
import { Button } from "../ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../ui/card";
import type { ChartConfig } from "../ui/chart";
import { EmptyState } from "../ui/empty-state";
import { InfoBox } from "../ui/info-box";
import { KpiCard } from "../ui/kpi-card";
import { formatNumber } from "../ui/lib/format";
import { SectionHeader } from "../ui/section-header";
import { Skeleton } from "../ui/skeleton";
import { useTool } from "../use-tool.ts";

// Same vocabulary as the fit charts: reclaimable = chart-4, recommended/applied = chart-2.
const CONFIG: ChartConfig = {
  projected: { label: "Still reclaimable (projected)", color: 4 },
  applied: { label: "Captured by applied resizes (est.)", color: 2 },
};
const LAYERS = [
  { key: "projected", kind: "area" as const },
  { key: "applied", kind: "line" as const },
];

export function SavingsView({
  list,
  ov,
}: {
  list: UseQueryResult<ListResp | null>;
  ov: UseQueryResult<OverviewResp | null>;
}) {
  const eco = useTool<EconomyResp>("TAILOR_ECONOMY", {}, { refetchInterval: 15_000 });
  const series = eco.data?.series ?? [];
  const from = series[0]?.T;
  const to = series[series.length - 1]?.T;
  // The economy window is fixed by the engine (last 6h); fetch the decisions in it.
  const acts = useTool<ActuationsResp>(
    "TAILOR_LIST_ACTUATIONS",
    { from: from ? Math.floor(from) : undefined, to: to ? Math.ceil(to) : undefined, limit: 1000 },
    { enabled: from != null, refetchInterval: 30_000 },
  );

  const recs = list.data?.recommendations ?? [];
  const cpu = ov.data?.priceCpuCoreHr ?? eco.data?.priceCpuCoreHr;
  const gb = ov.data?.priceGbHr ?? eco.data?.priceGbHr;
  const prices: Prices | null = cpu != null && gb != null ? { cpuCoreHr: cpu, gbHr: gb } : null;
  const events = acts.data?.events ?? [];
  const timeline = prices ? savingsTimeline(series, events, prices, recs) : [];
  const chartData = prices
    ? timeline
    : series.map((p) => ({ t: p.T, projected: Math.round(p.V * 100) / 100 }));
  const applied = prices && from != null ? appliedUsdWeek(events, prices, recs, from) : null;
  const top = topWaste(recs, 10);
  const maxWaste = top[0]?.wasteUsdWeek ?? 0;
  const loadingList = list.isLoading;
  const pricing = ov.data?.pricingSource ?? eco.data?.pricingSource;

  return (
    <>
      <h1 className="sr-only">Savings</h1>
      {eco.isError && (
        <Banner
          variant="danger"
          role="alert"
          title="Savings trend unavailable"
          description={eco.error instanceof Error ? eco.error.message : String(eco.error)}
          action={
            <Button size="sm" variant="outline" onClick={() => eco.refetch()}>
              Try again
            </Button>
          }
        />
      )}
      <section aria-label="Savings figures" className="grid grid-cols-2 gap-4 xl:grid-cols-4">
        <KpiCard
          label="Reclaimable / week"
          value={usd(list.data?.totalSavingsUsdWeek ?? 0)}
          loading={loadingList}
          unavailable={!list.data && !loadingList}
          tint="violet"
          trend={series.map((p) => p.V)}
          deltaLabel="if all applied"
        />
        <KpiCard
          label="Captured, last 6 hours"
          value={applied == null ? "—" : usd(applied)}
          unit="/ week"
          loading={eco.isLoading || acts.isLoading}
          unavailable={applied == null && !eco.isLoading && !acts.isLoading}
          tint="emerald"
          deltaLabel={`${formatNumber(events.length)} resize${events.length === 1 ? "" : "s"} applied · estimate`}
        />
        <KpiCard
          label="Needed by under-provisioned"
          value={usd(neededUsdWeek(recs))}
          unit="/ week"
          loading={loadingList}
          unavailable={!list.data && !loadingList}
          deltaLabel="extra spend to stop throttling / OOM"
        />
        <KpiCard
          label="Cluster cost / week"
          value={usd(ov.data?.overview.costUsdWeek ?? 0)}
          loading={ov.isLoading}
          unavailable={!ov.data && !ov.isLoading}
          deltaLabel={
            ov.data ? `${formatNumber(ov.data.overview.wastedPct)}% of it reclaimable` : undefined
          }
        />
      </section>

      <SummaryPanel />

      <section aria-labelledby="savings-timeline" className="flex flex-col gap-4">
        <SectionHeader
          id="savings-timeline"
          title="Savings timeline"
          description="Last 6 hours (the engine's fixed economy window), in $ per week. The area is what is still reclaimable; the line is what resizes applied in the window have already captured."
        />
        <Card className="gap-4">
          <CardContent>
            {!eco.isLoading && series.length === 0 && !eco.isError ? (
              <EmptyState
                illustration="chart"
                title="No savings trend yet"
                description="The trend needs a few scrapes of tailor_waste_usd_week in VictoriaMetrics — usually 5–10 minutes after the engine starts."
              />
            ) : (
              <ResourceChart
                data={chartData}
                unit="$"
                config={prices ? CONFIG : { projected: CONFIG.projected }}
                layers={prices ? LAYERS : LAYERS.slice(0, 1)}
                loading={eco.isLoading}
                summary={`Reclaimable spend over the last 6 hours, now ${usd(series[series.length - 1]?.V ?? 0)} per week${applied != null ? `; resizes in the window captured about ${usd(applied)} per week` : ""}.`}
              />
            )}
          </CardContent>
        </Card>
        <InfoBox tone="muted" title="How these numbers are computed">
          Reclaimable is the engine's own figure (sum of positive weekly waste across workloads).
          “Captured” is estimated here from each applied decision: (CPU cut × $/core-hr + memory cut
          × $/GiB-hr) × 168 h × the workload's current replicas
          {pricing === "static" ? ", using the default list prices — not your bill" : ""}.
        </InfoBox>
      </section>

      <section aria-labelledby="top-waste" className="flex flex-col gap-4">
        <SectionHeader
          id="top-waste"
          title="Top 10 by reclaimable spend"
          description="Containers whose requests sit furthest above what they use. Open one to see its fit."
        />
        <Card className="gap-0">
          <CardHeader className="sr-only">
            <CardTitle as="h3">Top 10 by reclaimable spend</CardTitle>
            <CardDescription>Weekly dollars per container</CardDescription>
          </CardHeader>
          <CardContent>
            {loadingList ? (
              <div role="status" aria-label="Loading" className="flex flex-col gap-4">
                {[0, 1, 2, 3].map((i) => (
                  <Skeleton key={i} className="h-9 w-full" />
                ))}
              </div>
            ) : top.length === 0 ? (
              <EmptyState
                illustration="list"
                title="Nothing to reclaim"
                description="No container is over-provisioned right now. Under-provisioned ones show on the Workloads page."
              />
            ) : (
              <ol className="flex flex-col gap-4">
                {top.map((r, i) => (
                  <li key={`${r.namespace}/${r.kind}/${r.workload}/${r.container}`}>
                    <div className="flex items-baseline justify-between gap-3 text-sm">
                      <a
                        href={buildHash(
                          "workloads",
                          new URLSearchParams({ q: `${r.namespace}/${r.workload}` }),
                        )}
                        className="min-w-0 truncate rounded-sm font-medium text-foreground outline-none hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
                      >
                        <span className="text-muted-foreground tabular-nums">{i + 1}. </span>
                        <span className="text-muted-foreground">{r.namespace}/</span>
                        {r.workload}
                        <span className="font-normal text-muted-foreground"> · {r.container}</span>
                      </a>
                      <span className="shrink-0 font-medium tabular-nums">
                        {usd(r.wasteUsdWeek)}
                        <span className="font-normal text-muted-foreground"> / week</span>
                      </span>
                    </div>
                    <div aria-hidden className="mt-1.5 h-2 overflow-hidden rounded-full bg-muted">
                      <div
                        className="h-full rounded-full bg-chart-4"
                        style={{
                          width: `${maxWaste > 0 ? (r.wasteUsdWeek / maxWaste) * 100 : 0}%`,
                        }}
                      />
                    </div>
                  </li>
                ))}
              </ol>
            )}
          </CardContent>
        </Card>
      </section>
    </>
  );
}
