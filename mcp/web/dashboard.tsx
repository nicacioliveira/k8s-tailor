import { useMutation } from "@tanstack/react-query";
import {
  Activity,
  Boxes,
  HeartPulse,
  Keyboard,
  Layers,
  LayoutDashboard,
  List,
  Pause,
  PiggyBank,
  Play,
  RefreshCw,
} from "lucide-react";
import { useEffect, useState } from "react";
import { AccessTokenButton } from "./components/auth";
import { Wordmark } from "./components/brand";
import { DEFAULT_RANGE, rangeArgs } from "./components/range-picker";
import { ShortcutsHelp } from "./components/shortcuts-help";
import { Hint } from "./components/workload-badges";
import { ENGINE_AUTH_UI, useMcpApp } from "./context.tsx";
import { navigate, useHashRoute } from "./lib/hash-route";
import { FILTER_ATTR, useShortcuts } from "./lib/shortcuts";
import type { ListResp, OverviewResp } from "./lib/types";
import { Badge } from "./ui/badge";
import { Banner } from "./ui/banner";
import { Button } from "./ui/button";
import { formatNumber } from "./ui/lib/format";
import { NavTabs } from "./ui/nav-tabs";
import { Skeleton } from "./ui/skeleton";
import { StatusIndicator } from "./ui/status-indicator";
import { callTool, useTool } from "./use-tool.ts";
import { DecisionsView } from "./views/decisions";
import { GroupingView } from "./views/grouping";
import { HealthView } from "./views/health";
import { OverviewView } from "./views/overview";
import { PoliciesView } from "./views/policies";
import { SavingsView } from "./views/savings";
import { WorkloadsView } from "./views/workloads";

// Screens are hash routes (#/workloads) so they can be linked and survive a reload,
// both standalone and inside an MCP host iframe.
const NAV = [
  { id: "overview", label: "Overview", icon: LayoutDashboard },
  { id: "workloads", label: "Workloads", icon: List },
  { id: "savings", label: "Savings", icon: PiggyBank },
  { id: "policies", label: "Policies", icon: Layers },
  { id: "grouping", label: "Grouping", icon: Boxes },
  { id: "decisions", label: "Decisions", icon: Activity },
  { id: "health", label: "Health", icon: HeartPulse },
] as const;
type View = (typeof NAV)[number]["id"];

const asView = (v: string): View => (NAV.find((n) => n.id === v)?.id ?? "overview") as View;

/** `/`: focus the current page's filter; pages without one send you to Workloads'. */
function focusFilter() {
  const el = document.querySelector<HTMLInputElement>(`[${FILTER_ATTR}]`);
  if (el) {
    el.focus();
    el.select();
    return;
  }
  navigate("workloads");
  setTimeout(() => document.querySelector<HTMLInputElement>(`[${FILTER_ATTR}]`)?.focus(), 150);
}

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

export function Dashboard() {
  const app = useMcpApp();
  const route = useHashRoute();
  const view = asView(route.view);
  const [help, setHelp] = useState(false);
  useShortcuts({ go: navigate, help: () => setHelp(true), focusFilter });
  // A new screen starts at the top (filter edits only replace the query, same view).
  useEffect(() => {
    window.scrollTo(0, 0);
  }, [view]);
  const [range, setRange] = useState(DEFAULT_RANGE);
  const [actionError, setActionError] = useState<string | null>(null);
  const list = useTool<ListResp>("TAILOR_LIST_RECOMMENDATIONS", {}, { refetchInterval: 10_000 });
  const ov = useTool<OverviewResp>("TAILOR_OVERVIEW", rangeArgs(range), {
    refetchInterval: 10_000,
  });
  const pause = useMutation({
    mutationFn: (p: boolean) => callTool(app, "TAILOR_SET_PAUSED", { paused: p }),
    onSuccess: () => ov.refetch(),
    onError: (e) => setActionError(`Could not change the pause switch: ${errText(e)}`),
  });

  if (!app) {
    return (
      <div className="grid min-h-dvh place-items-center p-6">
        <StatusIndicator status="syncing" label="Connecting to the tailor…" role="status" />
      </div>
    );
  }

  const paused = ov.data?.paused ?? false;
  const actuating = !paused && ov.data?.actuation === "active";
  const pricing = ov.data?.pricingSource;

  return (
    <div className="min-h-dvh">
      <header className="sticky top-0 z-(--z-sticky) border-b border-border bg-background">
        <div className="mx-auto flex max-w-[1280px] flex-wrap items-center gap-x-4 gap-y-2 px-4 pt-3 sm:px-6">
          <Wordmark />
          <span className="text-sm text-muted-foreground tabular-nums">
            {list.data ? (
              `${formatNumber(list.data.count)} workloads`
            ) : list.isLoading ? (
              <Skeleton className="h-4 w-24" />
            ) : null}
          </span>
          <span className="flex-1" />
          {ov.data && (
            <StatusIndicator
              role="status"
              status={paused ? "warning" : actuating ? "online" : "offline"}
              label={paused ? "Paused" : actuating ? "Actuating" : "Dry-run"}
              description={
                paused ? "no changes applied" : actuating ? undefined : "recommending only"
              }
            />
          )}
          {pricing && (
            <Hint
              text={
                pricing === "static"
                  ? "Estimate: savings use fixed default prices ($/core-hr, $/GB-hr), not your billed cost. Switch to OpenCost for measured, spot-aware node costs."
                  : pricing === "opencost"
                    ? "Measured node hourly cost (OpenCost, spot-aware)."
                    : "Pricing basis for savings"
              }
            >
              <Badge variant={pricing === "static" ? "warning-soft" : "success-soft"}>
                {pricing === "static" ? "Cost: estimate" : `Cost: ${pricing} · measured`}
              </Badge>
            </Hint>
          )}
          <Hint text="Global kill switch — writes TailorConfig.spec.paused">
            <Button
              variant={paused ? "default" : "outline"}
              size="sm"
              onClick={() => pause.mutate(!paused)}
              disabled={!ov.data}
              loading={pause.isPending}
            >
              {paused ? <Play aria-hidden /> : <Pause aria-hidden />}
              {paused ? "Resume actuation" : "Pause actuation"}
            </Button>
          </Hint>
          {ENGINE_AUTH_UI && <AccessTokenButton />}
          <Hint text="Keyboard shortcuts (?)">
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label="Keyboard shortcuts"
              onClick={() => setHelp(true)}
            >
              <Keyboard aria-hidden />
            </Button>
          </Hint>
          <Hint text="Refresh now">
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label="Refresh"
              onClick={() => {
                list.refetch();
                ov.refetch();
              }}
            >
              <RefreshCw aria-hidden className={list.isFetching ? "animate-spin" : undefined} />
            </Button>
          </Hint>
        </div>
        <div className="mx-auto max-w-[1280px] px-4 sm:px-6">
          <NavTabs
            aria-label="Dashboard sections"
            className="border-b-0"
            activeId={view}
            items={NAV.map((n) => ({ id: n.id, label: n.label, href: `#/${n.id}`, icon: n.icon }))}
          />
        </div>
      </header>

      <main className="mx-auto flex max-w-[1280px] flex-col gap-6 px-4 py-6 sm:px-6">
        {actionError && (
          <Banner
            variant="danger"
            role="alert"
            title="Change not applied"
            description={actionError}
            onDismiss={() => setActionError(null)}
          />
        )}
        {list.isError && (
          <Banner
            variant="danger"
            role="alert"
            title="Could not load recommendations"
            description={errText(list.error)}
            action={
              <Button size="sm" variant="outline" onClick={() => list.refetch()}>
                Try again
              </Button>
            }
          />
        )}
        {ov.data && ov.data.inPlaceSupported === false && (
          <Banner
            variant="warning"
            title="No in-place pod resize on this cluster"
            description="Needs Kubernetes 1.33+ with InPlacePodVerticalScaling. In-place policies fall back to a controller rollout."
          />
        )}

        {view === "overview" && <OverviewView ov={ov} range={range} onRange={setRange} />}
        {view === "workloads" && (
          <WorkloadsView
            list={list}
            ov={ov}
            params={route.params}
            onParams={route.setParams}
            range={range}
            onRange={setRange}
            onError={(e) => setActionError(errText(e))}
          />
        )}
        {view === "savings" && <SavingsView list={list} ov={ov} />}
        {view === "policies" && <PoliciesView />}
        {view === "grouping" && <GroupingView />}
        {view === "decisions" && <DecisionsView range={range} onRange={setRange} />}
        {view === "health" && <HealthView list={list} ov={ov} />}
      </main>
      <ShortcutsHelp open={help} onOpenChange={setHelp} />
    </div>
  );
}
