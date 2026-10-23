import type { UseQueryResult } from "@tanstack/react-query";
import { RangePicker } from "../components/range-picker";
import { ResourceChart } from "../components/resource-chart";
import { MI, usd } from "../lib/fmt";
import { type FitPoint, addRollingFit } from "../lib/series";
import type { OverviewResp } from "../lib/types";
import { Badge } from "../ui/badge";
import { Banner } from "../ui/banner";
import { Button } from "../ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../ui/card";
import type { ChartConfig } from "../ui/chart";
import { KpiCard } from "../ui/kpi-card";
import { formatNumber } from "../ui/lib/format";
import { SectionHeader } from "../ui/section-header";
import { useTool } from "../use-tool.ts";

const CLUSTER_CONFIG: ChartConfig = {
  used: { label: "Usage", color: 1 },
  waste: { label: "Wasted headroom", color: 4 },
  fit: { label: "Recommended request", color: 2 },
};

export function OverviewView({
  ov,
  range,
  onRange,
}: {
  ov: UseQueryResult<OverviewResp | null>;
  range: string;
  onRange: (id: string) => void;
}) {
  const eco = useTool<{ series: { T: number; V: number }[] | null }>(
    "TAILOR_ECONOMY",
    {},
    { refetchInterval: 15_000 },
  );
  const o = ov.data;
  const loading = ov.isLoading;
  // A failed overview is "unavailable" (—), never a misleading 0.
  const unavailable = !o && !loading;
  const v = o?.overview;
  const spark = (eco.data?.series ?? []).map((p) => p.V);
  const wasted = v?.wastedPct ?? 0;

  // Usage/series can be null on a fresh install or an empty window.
  const usage = v?.usage ?? [];
  const cpuReserved = v ? Math.round(v.cpuReserved * 1000) : 0;
  const memReserved = v ? Math.round(v.memReserved / MI) : 0;
  const cpu: FitPoint[] = addRollingFit(
    usage.map((p) => {
      const used = Math.round(p.cpu * 1000);
      return { t: p.t, used, waste: Math.max(cpuReserved - used, 0) };
    }),
  );
  const mem: FitPoint[] = addRollingFit(
    usage.map((p) => {
      const used = Math.round(p.mem / MI);
      return { t: p.t, used, waste: Math.max(memReserved - used, 0) };
    }),
  );
  const layers = [
    { key: "used", kind: "stack" as const },
    { key: "waste", kind: "stack" as const },
    { key: "fit", kind: "line" as const },
  ];

  return (
    <>
      <h1 className="sr-only">Overview</h1>
      {ov.isError && (
        <Banner
          variant="danger"
          role="alert"
          title="Overview unavailable"
          description={ov.error instanceof Error ? ov.error.message : String(ov.error)}
          action={
            <Button size="sm" variant="outline" onClick={() => ov.refetch()}>
              Try again
            </Button>
          }
        />
      )}
      <section
        aria-label="Key figures"
        className="grid grid-cols-2 gap-4 md:grid-cols-3 xl:grid-cols-6"
      >
        <KpiCard
          label="Cluster cost / week"
          value={usd(v?.costUsdWeek ?? 0)}
          loading={loading}
          unavailable={unavailable}
          deltaLabel={o ? `${formatNumber(v?.workloads ?? 0)} workloads` : undefined}
        />
        <KpiCard
          label="Wasted spend"
          value={`${formatNumber(wasted)}%`}
          loading={loading}
          unavailable={unavailable}
          deltaLabel="of reserved capacity"
          badge={
            o ? (
              <Badge
                variant={
                  wasted >= 40 ? "destructive-soft" : wasted >= 20 ? "warning-soft" : "success-soft"
                }
              >
                {wasted >= 40 ? "High" : wasted >= 20 ? "Moderate" : "Low"}
              </Badge>
            ) : undefined
          }
        />
        <KpiCard
          label="Reclaimable / week"
          value={usd(v?.savingsUsdWeek ?? 0)}
          loading={loading}
          unavailable={unavailable}
          trend={spark}
          tint="violet"
          deltaLabel="if all applied"
        />
        <KpiCard
          label="Automated"
          value={`${formatNumber(o?.automatedPct ?? 0)}%`}
          loading={loading}
          unavailable={unavailable}
          deltaLabel={
            o
              ? `${formatNumber(o.automated)} of ${formatNumber(v?.workloads ?? 0)} workloads`
              : undefined
          }
        />
        <KpiCard
          label="Over-provisioned"
          value={formatNumber(v?.over ?? 0)}
          unit="workloads"
          loading={loading}
          unavailable={unavailable}
          deltaLabel="requests above need"
        />
        <KpiCard
          label="Under-provisioned"
          value={formatNumber(v?.under ?? 0)}
          unit="workloads"
          loading={loading}
          unavailable={unavailable}
          deltaLabel="at risk of throttling or OOM"
        />
      </section>

      {!ov.isError && (
        <section aria-labelledby="cluster-usage" className="flex flex-col gap-4">
          <SectionHeader
            id="cluster-usage"
            title="Cluster usage vs reserved"
            description="Stacked: what pods use, and the reserved headroom nobody uses. The line is the request the tailor would set."
            action={<RangePicker value={range} onChange={onRange} />}
          />
          <div className="grid gap-4 lg:grid-cols-2">
            <Card className="gap-4">
              <CardHeader>
                <CardTitle as="h3">CPU</CardTitle>
                <CardDescription>Millicores across the cluster</CardDescription>
              </CardHeader>
              <CardContent>
                <ResourceChart
                  data={cpu}
                  unit="m"
                  config={CLUSTER_CONFIG}
                  layers={layers}
                  loading={loading}
                  refs={
                    cpuReserved > 0
                      ? [{ value: cpuReserved, label: `Reserved ${formatNumber(cpuReserved)}m` }]
                      : []
                  }
                  summary={`Cluster CPU usage against ${formatNumber(cpuReserved)} millicores reserved.`}
                />
              </CardContent>
            </Card>
            <Card className="gap-4">
              <CardHeader>
                <CardTitle as="h3">Memory</CardTitle>
                <CardDescription>MiB across the cluster</CardDescription>
              </CardHeader>
              <CardContent>
                <ResourceChart
                  data={mem}
                  unit="Mi"
                  config={CLUSTER_CONFIG}
                  layers={layers}
                  loading={loading}
                  refs={
                    memReserved > 0
                      ? [{ value: memReserved, label: `Reserved ${formatNumber(memReserved)}Mi` }]
                      : []
                  }
                  summary={`Cluster memory usage against ${formatNumber(memReserved)} MiB reserved.`}
                />
              </CardContent>
            </Card>
          </div>
        </section>
      )}
    </>
  );
}
