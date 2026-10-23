import { CircleCheck, CircleHelp, Hourglass } from "lucide-react";
import {
  CONFIDENCE_LABEL,
  type Confidence,
  RESOURCE_LABEL,
  type ResourceId,
  resourceConfidence,
} from "../lib/confidence";
import type { Rec } from "../lib/types";
import { Badge } from "../ui/badge";
import { Hint } from "./workload-badges";

const ICON = { confident: CircleCheck, gathering: Hourglass, unknown: CircleHelp } as const;
const VARIANT = {
  confident: "success-soft",
  gathering: "warning-soft",
  unknown: "outline",
} as const;
const HINT: Record<Confidence, string> = {
  confident: "Enough samples to recommend and actuate this resource.",
  gathering: "Not enough samples yet: this resource is held, not actuated.",
  unknown: "This engine does not report per-resource confidence.",
};

/** Icon + text per resource ("CPU · Confident"), never colour alone. */
export function ConfidenceBadge({ r, res }: { r: Rec; res: ResourceId }) {
  const c = resourceConfidence(r, res);
  const Icon = ICON[c];
  return (
    <Hint text={HINT[c]}>
      <Badge variant={VARIANT[c]} data-confidence={c}>
        <Icon aria-hidden />
        {RESOURCE_LABEL[res]} · {CONFIDENCE_LABEL[c]}
      </Badge>
    </Hint>
  );
}

export function ConfidenceBadges({ r }: { r: Rec }) {
  return (
    <ul aria-label="Confidence by resource" className="flex flex-wrap items-center gap-1.5">
      <li>
        <ConfidenceBadge r={r} res="cpu" />
      </li>
      <li>
        <ConfidenceBadge r={r} res="mem" />
      </li>
    </ul>
  );
}
