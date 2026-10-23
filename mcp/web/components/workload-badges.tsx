import {
  AlertTriangle,
  ArrowDown,
  ArrowUp,
  Box,
  Check,
  CirclePlay,
  Link2,
  Shield,
  ShieldAlert,
  Syringe,
} from "lucide-react";
import type * as React from "react";
import { classHint, classLabel, classOf } from "../lib/containers";
import { ago, nowSec } from "../lib/fmt";
import { type StatusId, statusId, statusLabel } from "../lib/status";
import type { Rec, Stat } from "../lib/types";
import { Badge } from "../ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "../ui/tooltip";

type BadgeVariant = React.ComponentProps<typeof Badge>["variant"];

/** Short explanation on hover/focus. Never the only carrier of meaning. */
export function Hint({ text, children }: { text: React.ReactNode; children: React.ReactElement }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>{children}</TooltipTrigger>
      <TooltipContent>{text}</TooltipContent>
    </Tooltip>
  );
}

const STATUS_VARIANT: Record<StatusId, BadgeVariant> = {
  oom: "destructive-soft",
  limit: "destructive-soft",
  gathering: "outline",
  over: "warning-soft",
  under: "destructive-soft",
  ok: "success-soft",
};

// At-a-glance verdict. Always a word, the tint only reinforces it.
export function workloadStatus(r: Rec): { label: string; variant: BadgeVariant } {
  const id = statusId(r);
  return { label: statusLabel(id), variant: STATUS_VARIANT[id] };
}

export function StatusBadge({ r }: { r: Rec }) {
  const s = workloadStatus(r);
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <Badge variant={s.variant}>{s.label}</Badge>
      {r.specDrift && s.label === "Right-sized" && (
        <Hint text="Live pods are right-sized in place, but the workload spec still has the old request — it reverts on the next rollout until the spec is persisted.">
          <Badge variant="info-soft">Spec pending</Badge>
        </Hint>
      )}
    </span>
  );
}

const CLASS_ICON = {
  app: Box,
  "injected-sidecar": Syringe,
  "native-sidecar": Link2,
  init: CirclePlay,
  ephemeral: CirclePlay,
} as const;

/** Container role: icon + text ("App", "Sidecar · istio", "Native sidecar", "Init"). */
export function ClassBadge({ r }: { r: Rec }) {
  const cls = classOf(r);
  const Icon = CLASS_ICON[cls];
  return (
    <Hint text={classHint(r)}>
      <Badge variant={cls === "app" ? "outline" : "info-soft"}>
        <Icon aria-hidden />
        {classLabel(r)}
      </Badge>
    </Hint>
  );
}

const KIND_ABBR: Record<string, string> = {
  Deployment: "Deploy",
  StatefulSet: "STS",
  DaemonSet: "DS",
  Pod: "Pod",
  ksvc: "ksvc",
  group: "Group",
};

export function pdbText(r: Rec) {
  return r.pdbProtected
    ? `PDB ${r.pdbName} · ${r.pdbAllowed} disruption${r.pdbAllowed === 1 ? "" : "s"} allowed`
    : "No PodDisruptionBudget";
}

/** Kind, autoscaler, Knative, management and PDB markers for one workload. */
export function KindBadges({ r }: { r: Rec }) {
  const as = r.autoscaler;
  const held =
    as?.kind === "HPA"
      ? [as.managesCpu && "cpu", as.managesMem && "mem"].filter(Boolean).join("·")
      : "";
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <Hint text={r.strategy || r.kind}>
        <Badge variant="outline">{KIND_ABBR[r.kind] ?? r.kind}</Badge>
      </Hint>
      {r.knative?.service && r.kind !== "ksvc" && (
        <Hint text={`Knative ksvc ${r.knative.service} — a durable resize mints a new revision`}>
          <Badge variant="outline">ksvc</Badge>
        </Hint>
      )}
      {as?.kind === "HPA" && (
        <Hint
          text={
            held
              ? `HPA ${as.name} scales on ${held} — those requests are held; the rest is managed`
              : `HPA ${as.name} — vertical-safe`
          }
        >
          <Badge variant={held ? "warning-soft" : "info-soft"}>HPA{held ? ` · ${held}` : ""}</Badge>
        </Hint>
      )}
      {as?.kind === "KPA" && (
        <Hint text="Knative KPA scales on concurrency — vertical-safe">
          <Badge variant="info-soft">KPA</Badge>
        </Hint>
      )}
      {!r.managed && (
        <Hint text="No controller owns this pod">
          <Badge variant="outline">Unmanaged</Badge>
        </Hint>
      )}
      {r.oomRisk && (
        <Badge variant="destructive-soft">
          <AlertTriangle aria-hidden />
          OOMKilled
        </Badge>
      )}
      {r.limitCapped && !r.oomRisk && <Badge variant="warning-soft">Capped at limit</Badge>}
      {r.pdbProtected && (
        <Hint text={pdbText(r)}>
          <Badge variant={r.pdbAllowed <= 0 ? "warning-soft" : "outline"}>
            {r.pdbAllowed <= 0 ? <ShieldAlert aria-hidden /> : <Shield aria-hidden />}
            PDB
          </Badge>
        </Hint>
      )}
    </span>
  );
}

/** Actuation method, direction and "applied" markers. */
export function ActuationBadges({ r, stat }: { r: Rec; stat?: Stat }) {
  return (
    <>
      {r.automated && (
        <Hint text="Actuation method (from the policy)">
          <Badge variant="secondary">{r.apply || "InPlace"}</Badge>
        </Hint>
      )}
      {r.direction === "Down" && (
        <Hint text="Shrink-only: reclaims waste, never raises a resource">
          <Badge variant="outline">
            <ArrowDown aria-hidden />
            Shrink-only
          </Badge>
        </Hint>
      )}
      {r.direction === "Up" && (
        <Hint text="Grow-only: protects under-provisioned workloads, never reclaims">
          <Badge variant="outline">
            <ArrowUp aria-hidden />
            Grow-only
          </Badge>
        </Hint>
      )}
      {stat && (
        <Hint text={`${stat.count} change${stat.count === 1 ? "" : "s"} applied by the tailor`}>
          <Badge variant={nowSec() - stat.last < 120 ? "success" : "success-soft"}>
            <Check aria-hidden />
            Applied {ago(stat.last)}
          </Badge>
        </Hint>
      )}
    </>
  );
}
