import { TriangleAlert } from "lucide-react";
import {
  classOf,
  containerAnnotation,
  effectiveMode,
  enableSnippet,
  isNeverSized,
  needsSidecarOptIn,
  originText,
  pathIsNone,
  pathText,
} from "../lib/containers";
import type { Rec } from "../lib/types";
import { Badge } from "../ui/badge";
import { InfoBox } from "../ui/info-box";
import { ContainerControls } from "./container-controls";
import { CopyButton } from "./copy-button";
import { ClassBadge, Hint } from "./workload-badges";

/** How a resize reaches the container, in words (never a raw engine token). */
export function PathLine({ r }: { r: Rec }) {
  if (isNeverSized(r))
    return <span className="text-xs text-muted-foreground">Never sized or resized</span>;
  const none = pathIsNone(r);
  const bare = (r.actuationPath ?? "") === "none";
  return (
    <Hint text="How a change reaches this container (engine actuationPath)">
      <span className="text-xs text-muted-foreground">
        {none ? "Not actuated" : "Acts via"}
        {bare ? (
          <> ({effectiveMode(r).toLowerCase()})</>
        ) : (
          <>
            {none ? ": " : " "}
            <span className="font-medium text-foreground">{pathText(r)}</span>
          </>
        )}
      </span>
    </Hint>
  );
}

/** Effective mode of this container as text, with where it came from. */
export function ModeOrigin({ r }: { r: Rec }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-0.5">
      <Badge variant={r.automated ? "success-soft" : "outline"}>{effectiveMode(r)}</Badge>
      <span className="text-xs text-muted-foreground">{originText(r)}</span>
    </span>
  );
}

/**
 * Warning for an injected sidecar left at Recommend by the safe default, with the
 * annotation that opts it in and a copy button. `compact` is the one-line table variant.
 */
export function SidecarOptIn({ r, compact }: { r: Rec; compact?: boolean }) {
  if (!needsSidecarOptIn(r)) return null;
  const snippet = enableSnippet(r);
  const ann = containerAnnotation(r.container, "mode");
  if (compact) {
    return (
      <span className="inline-flex items-center gap-1 text-xs text-warning-text">
        <TriangleAlert aria-hidden className="size-3.5 shrink-0" />
        <span>Recommend-only until opted in</span>
        <Hint
          text={`Opt in with the annotation ${ann}=InPlace on the workload. Copies the kubectl command.`}
        >
          <span>
            <CopyButton iconOnly text={snippet} label={`Copy snippet to enable ${r.container}`} />
          </span>
        </Hint>
      </span>
    );
  }
  return (
    <InfoBox tone="warning" title={`${r.container} is recommend-only`}>
      <p>
        An injected sidecar stays at Recommend until a container rule or annotation sets its mode:
        the workload toggle does not enable it. Pick a mode in the container list below (you will be
        asked to confirm), or for GitOps set <code className="break-all">{ann}</code> to{" "}
        <code>InPlace</code> (or <code>Webhook</code>, <code>Both</code>) on the workload.
      </p>
      <pre className="mt-2 overflow-x-auto rounded-lg border border-border bg-card p-3 text-xs whitespace-pre-wrap break-all text-foreground">
        {snippet}
      </pre>
      <div className="mt-2">
        <CopyButton text={snippet} label="Copy enable snippet" />
      </div>
    </InfoBox>
  );
}

/** Every container of one workload with class, mode, origin and path; pick one to inspect. */
export function ContainerList({
  recs,
  current,
  onSelect,
}: {
  recs: Rec[];
  current: Rec;
  onSelect: (r: Rec) => void;
}) {
  return (
    <ul aria-label="Containers of this workload" className="flex flex-col gap-2">
      {recs.map((r) => {
        const here = r.container === current.container;
        return (
          <li
            key={r.container}
            className={`flex flex-col gap-1.5 rounded-xl border p-3 ${here ? "border-ring bg-muted" : "border-border"}`}
          >
            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                aria-current={here ? "true" : undefined}
                disabled={here}
                onClick={() => onSelect(r)}
                className="rounded-md font-medium outline-none enabled:hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
              >
                {r.container}
              </button>
              <ClassBadge r={r} />
              {here && <Badge variant="secondary">Viewing</Badge>}
              {classOf(r) === "injected-sidecar" && !r.inTemplate && (
                <Hint text="Added by the injector at admission: it is not in the workload template, so a rollout cannot patch it.">
                  <Badge variant="outline">Not in template</Badge>
                </Hint>
              )}
            </div>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
              <ModeOrigin r={r} />
              <PathLine r={r} />
            </div>
            <ContainerControls r={r} />
          </li>
        );
      })}
    </ul>
  );
}
