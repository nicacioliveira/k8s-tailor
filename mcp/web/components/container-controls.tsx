import { Loader2, TriangleAlert } from "lucide-react";
import { useEffect, useId, useSyncExternalStore } from "react";
import {
  ACTUATING_MODES,
  CONTAINER_MODES,
  type ContainerMode,
  type ContainerWrite,
  MODE_LABEL,
  OPT_IN_WARNING,
  classifyWriteError,
  containerRefOf,
  controlsAvailability,
  currentChoice,
  excludedVersion,
  isExcluded,
  needsOptInConfirm,
  subscribeExcluded,
  successText,
} from "../lib/container-write";
import { enableSnippet, excludeSnippet, isInjectedSidecar } from "../lib/containers";
import { requestAuth } from "../lib/auth";
import { useSetContainer } from "../lib/mutations";
import type { Rec } from "../lib/types";
import { Banner } from "../ui/banner";
import { Button } from "../ui/button";
import { useConfirm } from "../ui/confirm-dialog";
import { Label } from "../ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Switch } from "../ui/switch";
import { CopyButton } from "./copy-button";

/**
 * Real controls for one container: a mode select (Inherit … Both) and an Exclude switch,
 * written through POST /api/workload with `container`. Optimistic with rollback; every
 * engine refusal is mapped to a sentence. Copy-the-annotation stays as the GitOps route.
 *
 * `compact` (workloads table) renders the select only and hands errors to `onError`.
 */
export function ContainerControls({
  r,
  compact,
  onError,
}: {
  r: Rec;
  compact?: boolean;
  onError?: (message: string) => void;
}) {
  const ids = { mode: useId(), exclude: useId(), note: useId(), warn: useId() };
  const avail = controlsAvailability(r);
  const set = useSetContainer();
  const { confirm, confirmDialog } = useConfirm();
  useSyncExternalStore(subscribeExcluded, excludedVersion);
  const excluded = isExcluded(r);
  const choice = currentChoice(r);
  const disabled = !avail.enabled;
  const failure = set.isError ? classifyWriteError(set.error) : null;
  const sidecarOptIn = isInjectedSidecar(r) && !r.automated;

  // Let the success line be heard, then clear it.
  const { isSuccess, reset } = set;
  useEffect(() => {
    if (!isSuccess) return;
    const t = setTimeout(reset, 6_000);
    return () => clearTimeout(t);
  }, [isSuccess, reset]);

  const run = (w: Pick<ContainerWrite, "mode" | "exclude">) =>
    set.mutate(
      { ...containerRefOf(r), ...w },
      {
        onError: (e) => {
          const f = classifyWriteError(e);
          if (onError) onError(`${r.container}: ${f.message}`);
        },
      },
    );

  const onMode = async (v: string) => {
    const mode = v as ContainerMode;
    if (mode === choice) return;
    if (needsOptInConfirm(r, mode)) {
      const ok = await confirm({
        title: `Opt ${r.container} in to actuation?`,
        description: (
          <>
            {OPT_IN_WARNING}. With <b className="font-semibold">{MODE_LABEL[mode]}</b> the engine
            will change the resources of this injected sidecar
            {r.mesh ? ` (${r.mesh})` : ""}, bounded by its policy and guardrails. The workload
            toggle never did this; you can set it back to Inherit at any time.
          </>
        ),
        confirmLabel: `Opt in with ${MODE_LABEL[mode]}`,
      });
      if (!ok) return;
    }
    run({ mode });
  };

  const status = set.isPending
    ? "Saving…"
    : set.isSuccess && set.variables
      ? successText(set.variables)
      : "";

  const select = (
    <Select value={choice} disabled={disabled || set.isPending} onValueChange={onMode}>
      <SelectTrigger
        id={ids.mode}
        size="sm"
        className={compact ? "w-32" : "w-36"}
        aria-label={compact ? `Mode for ${r.container}` : undefined}
        aria-describedby={disabled ? ids.note : sidecarOptIn ? ids.warn : undefined}
      >
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {CONTAINER_MODES.map((m) => (
          <SelectItem key={m} value={m}>
            {MODE_LABEL[m]}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );

  if (compact) {
    return (
      <div className="flex flex-col items-start gap-1">
        {select}
        {disabled && (
          <span id={ids.note} className="text-xs text-muted-foreground">
            {avail.message}
          </span>
        )}
        <span role="status" aria-live="polite" className="sr-only">
          {status}
        </span>
        {confirmDialog}
      </div>
    );
  }

  return (
    <div data-slot="container-controls" className="flex flex-col gap-2">
      <div className="flex flex-wrap items-end gap-x-5 gap-y-2">
        <div className="grid gap-1">
          <Label htmlFor={ids.mode}>
            Mode<span className="sr-only"> for {r.container}</span>
          </Label>
          {select}
        </div>
        <div className="flex h-control-sm items-center gap-2">
          <Switch
            id={ids.exclude}
            checked={excluded}
            disabled={disabled || set.isPending}
            aria-describedby={disabled ? ids.note : undefined}
            onCheckedChange={(v) => run({ exclude: v })}
          />
          <Label htmlFor={ids.exclude} className="font-normal">
            Exclude<span className="sr-only"> {r.container}</span>
          </Label>
          <span className="text-xs text-muted-foreground">no recommendation, no actuation</span>
        </div>
        {set.isPending && (
          <span className="inline-flex items-center gap-1.5 pb-1.5 text-xs text-muted-foreground">
            <Loader2 aria-hidden className="size-3.5 animate-spin" />
            Saving…
          </span>
        )}
      </div>

      {disabled && (
        <p id={ids.note} className="text-xs text-muted-foreground">
          {avail.message}
        </p>
      )}
      {!disabled && sidecarOptIn && (
        <p id={ids.warn} className="inline-flex items-center gap-1.5 text-xs text-warning-text">
          <TriangleAlert aria-hidden className="size-3.5 shrink-0" />
          {OPT_IN_WARNING}: pick {ACTUATING_MODES.map((m) => MODE_LABEL[m]).join(", ")} to enable it
          (you will be asked to confirm).
        </p>
      )}

      <p role="status" aria-live="polite" className="min-h-4 text-xs text-success-text">
        {set.isPending ? "" : status}
      </p>
      {failure && (
        <Banner
          variant="danger"
          role="alert"
          className="px-3 py-2"
          title={`Could not save ${r.container}`}
          description={failure.message}
          action={
            failure.kind === "auth" && failure.status === 401 ? (
              <Button
                size="sm"
                variant="outline"
                onClick={() =>
                  requestAuth({ reason: "manual", message: failure.message, target: "engine" })
                }
              >
                Enter token
              </Button>
            ) : undefined
          }
          onDismiss={set.reset}
        />
      )}

      <details className="text-xs">
        <summary className="w-fit cursor-pointer rounded-md text-muted-foreground outline-none hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring">
          Manage through GitOps instead: copy the annotation
        </summary>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          {(avail.enabled || avail.reason === "unsupported-kind") && (
            <>
              <CopyButton
                text={enableSnippet(r)}
                label={sidecarOptIn ? "Copy: opt in with InPlace" : "Copy: set mode InPlace"}
              />
              <CopyButton text={excludeSnippet(r)} label="Copy: exclude container" />
            </>
          )}
        </div>
      </details>
      {confirmDialog}
    </div>
  );
}
