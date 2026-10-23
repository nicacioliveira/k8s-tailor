import { useMutation, useQuery } from "@tanstack/react-query";
import { CheckCircle2, Eye, ShieldAlert, Sparkles } from "lucide-react";
import { type ReactNode, useEffect, useId, useState } from "react";
import { ENGINE_AUTH_UI, useMcpApp } from "../context.tsx";
import {
  ADVISOR_DISCLAIMER,
  type AdvisorProblem,
  type AdvisorStatus,
  type Confidence,
  type Figure,
  type Review,
  type Summary,
  type Verdict,
  formatRetry,
  mapAdvisorError,
  parseReview,
  parseStatus,
  parseSummary,
} from "../lib/advisor";
import { requestAuth } from "../lib/auth";
import { Badge } from "../ui/badge";
import { Banner } from "../ui/banner";
import { Button } from "../ui/button";
import { InfoBox } from "../ui/info-box";
import { Label } from "../ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Skeleton } from "../ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { callTool } from "../use-tool.ts";

// IMPORTANT: every string that came from the LLM (rationale, risks, highlights, labels…)
// is rendered below as a plain React text node. Never dangerouslySetInnerHTML, never a
// markdown renderer: the model's output is untrusted.

/** Whether the engine has the advisor on. Any failure (older engine, down) = hidden. */
export function useAdvisorStatus(): AdvisorStatus | null {
  const app = useMcpApp();
  const q = useQuery<AdvisorStatus | null>({
    queryKey: ["TAILOR_ADVISOR_STATUS"],
    enabled: !!app,
    retry: false,
    staleTime: 30_000,
    refetchInterval: 60_000,
    queryFn: async () => parseStatus(await callTool(app, "TAILOR_ADVISOR_STATUS", {})),
  });
  return q.data?.enabled ? q.data : null;
}

/** Seconds left until `seconds` from now elapse; restarts when `restartKey` changes. */
function useCountdown(seconds: number | undefined, restartKey: unknown): number {
  const [left, setLeft] = useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: restartKey intentionally restarts the timer
  useEffect(() => {
    if (!seconds) {
      setLeft(0);
      return;
    }
    const end = Date.now() + seconds * 1000;
    setLeft(seconds);
    const id = setInterval(() => {
      const l = Math.max(0, Math.ceil((end - Date.now()) / 1000));
      setLeft(l);
      if (l <= 0) clearInterval(id);
    }, 1000);
    return () => clearInterval(id);
  }, [seconds, restartKey]);
  return left;
}

const VERDICT: Record<
  Verdict,
  { label: string; variant: "success-soft" | "warning-soft" | "destructive-soft"; icon: typeof Eye }
> = {
  ok: { label: "OK", variant: "success-soft", icon: CheckCircle2 },
  review: { label: "Review", variant: "warning-soft", icon: Eye },
  risky: { label: "Risky", variant: "destructive-soft", icon: ShieldAlert },
};

/** Seal = icon + word + colour; colour is never the only signal. */
export function VerdictBadge({ verdict }: { verdict: Verdict }) {
  const v = VERDICT[verdict];
  const Icon = v.icon;
  return (
    <Badge variant={v.variant} className="px-2.5 py-1 text-sm">
      <Icon aria-hidden className="size-4!" />
      <span className="sr-only">Verdict: </span>
      {v.label}
    </Badge>
  );
}

function ConfidenceBadge({ confidence }: { confidence: Confidence }) {
  return <Badge variant="outline">Confidence: {confidence}</Badge>;
}

/** Permanent notice: shown before, during and after any advisor output. */
export function AdvisorDisclaimer({ className }: { className?: string }) {
  return (
    <p className={`flex items-center gap-1.5 text-xs text-muted-foreground ${className ?? ""}`}>
      <Sparkles aria-hidden className="size-3.5 shrink-0" />
      {ADVISOR_DISCLAIMER}
    </p>
  );
}

function Meta({ model, cached }: { model: string; cached: boolean }) {
  return (
    <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
      {cached && <Badge variant="secondary">Cached</Badge>}
      {model && (
        <span>
          Model: <span className="font-mono">{model}</span>
        </span>
      )}
    </div>
  );
}

function FiguresTable({ figures }: { figures: Figure[] }) {
  if (figures.length === 0) return null;
  return (
    <section className="flex flex-col gap-2">
      <h4 className="text-sm font-semibold">Numbers cited</h4>
      <div className="overflow-hidden rounded-xl border border-border">
        <Table aria-label="Numbers cited by the advisor">
          <TableHeader>
            <TableRow>
              <TableHead className="pl-4">Figure</TableHead>
              <TableHead className="text-right">Value</TableHead>
              <TableHead className="pr-4">Unit</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {figures.map((f, i) => (
              <TableRow key={`${f.label}-${i}`}>
                <TableCell className="pl-4 whitespace-normal">{f.label}</TableCell>
                <TableCell className="text-right">{String(f.value)}</TableCell>
                <TableCell className="pr-4 text-muted-foreground">{f.unit || "—"}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </section>
  );
}

function TextList({ title, items }: { title: string; items: string[] }) {
  if (items.length === 0) return null;
  return (
    <section className="flex flex-col gap-2">
      <h4 className="text-sm font-semibold">{title}</h4>
      <ul className="list-disc space-y-1 pl-5 text-sm">
        {items.map((t, i) => (
          <li key={`${i}-${t.slice(0, 24)}`} className="whitespace-pre-line">
            {t}
          </li>
        ))}
      </ul>
    </section>
  );
}

export function ProblemBanner({
  problem,
  onRetry,
  retryIn,
}: {
  problem: AdvisorProblem;
  onRetry?: () => void;
  retryIn?: number;
}) {
  const action =
    problem.kind === "unauthorized" && ENGINE_AUTH_UI ? (
      <Button
        size="sm"
        variant="outline"
        onClick={() => requestAuth({ reason: "manual", target: "engine" })}
      >
        Enter token
      </Button>
    ) : problem.retryable && onRetry ? (
      <Button size="sm" variant="outline" onClick={onRetry} disabled={!!retryIn}>
        {retryIn ? `Try again in ${formatRetry(retryIn)}` : "Try again"}
      </Button>
    ) : undefined;
  return (
    <Banner
      variant={problem.variant}
      title={problem.title}
      description={problem.description}
      action={action}
    />
  );
}

function Pending({ label }: { label: string }) {
  return (
    <div role="status" className="flex flex-col gap-3">
      <span className="text-sm text-muted-foreground">{label}</span>
      <Skeleton className="h-4 w-3/4" />
      <Skeleton className="h-4 w-full" />
      <Skeleton className="h-4 w-2/3" />
    </div>
  );
}

export function ReviewResultView({ review }: { review: Review }) {
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <VerdictBadge verdict={review.verdict} />
        <ConfidenceBadge confidence={review.confidence} />
      </div>
      {review.rationale && (
        <section className="flex flex-col gap-1">
          <h4 className="text-sm font-semibold">Rationale</h4>
          <p className="text-sm leading-relaxed whitespace-pre-line">{review.rationale}</p>
        </section>
      )}
      <TextList title="Risks" items={review.risks} />
      {review.suggestedAction && (
        <InfoBox tone="muted" icon={false} title="Suggested action (not applied)">
          <span className="whitespace-pre-line">{review.suggestedAction}</span>
        </InfoBox>
      )}
      <FiguresTable figures={review.figures} />
      <Meta model={review.model} cached={review.cached} />
    </div>
  );
}

export function SummaryResultView({
  summary,
  considered,
  total,
}: { summary: Summary; considered: number; total: number }) {
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <VerdictBadge verdict={summary.verdict} />
        <ConfidenceBadge confidence={summary.confidence} />
        <span className="text-xs text-muted-foreground tabular-nums">
          Based on the top {considered} of {total} workloads by weekly waste
          {summary.omitted > 0 && ` (${summary.omitted} more not sent)`}
        </span>
      </div>
      {summary.overview && (
        <section className="flex flex-col gap-1">
          <h4 className="text-sm font-semibold">Overview</h4>
          <p className="text-sm leading-relaxed whitespace-pre-line">{summary.overview}</p>
        </section>
      )}
      <TextList title="Highlights" items={summary.highlights} />
      {summary.attention.length > 0 && (
        <section className="flex flex-col gap-2">
          <h4 className="text-sm font-semibold">Needs attention</h4>
          <div className="overflow-hidden rounded-xl border border-border">
            <Table aria-label="Workloads needing attention">
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4">Workload</TableHead>
                  <TableHead className="pr-4">Why</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {summary.attention.map((a, i) => (
                  <TableRow key={`${a.workload}-${i}`}>
                    <TableCell className="pl-4 align-top font-medium whitespace-normal break-all">
                      {a.workload}
                    </TableCell>
                    <TableCell className="pr-4 align-top whitespace-pre-line whitespace-normal">
                      {a.reason}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </section>
      )}
      <FiguresTable figures={summary.figures} />
      <Meta model={summary.model} cached={summary.cached} />
    </div>
  );
}

/**
 * Shared frame: heading, run button, permanent disclaimer, and an aria-live region that
 * always exists so screen readers announce the result/error when it lands.
 */
function AdvisorFrame({
  title,
  hint,
  controls,
  pending,
  pendingLabel,
  problem,
  onRetry,
  result,
  idle,
}: {
  title: string;
  hint: string;
  controls: ReactNode;
  pending: boolean;
  pendingLabel: string;
  problem: AdvisorProblem | null;
  onRetry: () => void;
  result: ReactNode;
  idle: boolean;
}) {
  const titleId = useId();
  const retryIn = useCountdown(problem?.retryAfter, problem);
  return (
    <section
      aria-labelledby={titleId}
      className="flex flex-col gap-3 rounded-xl border border-border p-4"
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-1">
          <h3
            id={titleId}
            className="flex items-center gap-2 text-base font-semibold tracking-tight"
          >
            <Sparkles aria-hidden className="size-4 text-primary" />
            {title}
          </h3>
          <p className="text-sm text-muted-foreground">{hint}</p>
        </div>
        <div className="flex flex-wrap items-end gap-2">{controls}</div>
      </div>
      <AdvisorDisclaimer />
      <div aria-live="polite" aria-atomic="false" className="flex flex-col gap-3">
        {pending ? (
          <Pending label={pendingLabel} />
        ) : problem ? (
          <ProblemBanner problem={problem} onRetry={onRetry} retryIn={retryIn} />
        ) : (
          result
        )}
        {idle && !pending && !problem && !result && (
          <p className="text-sm text-muted-foreground">
            Nothing yet. The advisor only runs when you ask.
          </p>
        )}
      </div>
    </section>
  );
}

/** Workload panel: one container (picker when there are several) -> review. */
export function ReviewPanel({
  namespace,
  kind,
  workload,
  container,
  containers,
  onContainer,
}: {
  namespace: string;
  kind: string;
  workload: string;
  container: string;
  containers: string[];
  onContainer: (c: string) => void;
}) {
  const app = useMcpApp();
  const status = useAdvisorStatus();
  const pickId = useId();
  const run = useMutation({
    mutationFn: async () =>
      parseReview(
        await callTool(app, "TAILOR_ADVISOR_REVIEW", { namespace, kind, workload, container }),
      ),
  });
  // A different container is a different question: drop the previous answer.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset on identity change only
  useEffect(() => run.reset(), [namespace, kind, workload, container]);

  if (!status) return null;
  const problem = run.error ? mapAdvisorError(run.error) : null;
  const blocked = status.breakerState === "open";
  return (
    <AdvisorFrame
      title="AI review"
      hint={`A second opinion on this recommendation from ${status.model || "the configured model"}.`}
      idle={!run.data}
      pending={run.isPending}
      pendingLabel="Analyzing with AI…"
      problem={problem}
      onRetry={() => run.mutate()}
      controls={
        <>
          {containers.length > 1 && (
            <div className="flex flex-col gap-1">
              <Label htmlFor={pickId} className="text-xs text-muted-foreground">
                Container
              </Label>
              <Select value={container} onValueChange={onContainer}>
                <SelectTrigger id={pickId} size="sm" className="min-w-40">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {containers.map((c) => (
                    <SelectItem key={c} value={c}>
                      {c}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}
          <Button
            size="sm"
            variant="outline"
            loading={run.isPending}
            onClick={() => run.mutate()}
            title={
              blocked ? "The provider circuit breaker is open; the call may be refused" : undefined
            }
          >
            <Sparkles aria-hidden />
            {run.data ? "Analyze again" : "Analyze with AI"}
          </Button>
        </>
      }
      result={run.data && !run.isPending ? <ReviewResultView review={run.data.review} /> : null}
    />
  );
}

/** Cluster-wide summary of the engine's current decisions. */
export function SummaryPanel() {
  const app = useMcpApp();
  const status = useAdvisorStatus();
  const run = useMutation({
    mutationFn: async () => parseSummary(await callTool(app, "TAILOR_ADVISOR_SUMMARY", {})),
  });
  if (!status) return null;
  const problem = run.error ? mapAdvisorError(run.error) : null;
  return (
    <AdvisorFrame
      title="AI summary"
      hint={`${status.model || "The configured model"} reads the current decisions and flags what deserves a human look.`}
      idle={!run.data}
      pending={run.isPending}
      pendingLabel="Summarizing the current decisions…"
      problem={problem}
      onRetry={() => run.mutate()}
      controls={
        <Button size="sm" variant="outline" loading={run.isPending} onClick={() => run.mutate()}>
          <Sparkles aria-hidden />
          {run.data ? "Summarize again" : "AI summary"}
        </Button>
      }
      result={
        run.data && !run.isPending ? (
          <SummaryResultView
            summary={run.data.summary}
            considered={run.data.considered}
            total={run.data.total}
          />
        ) : null
      }
    />
  );
}
