import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useMcpApp } from "../context.tsx";
import { callTool } from "../use-tool.ts";
import {
  type ContainerWrite,
  applyContainerWrite,
  containerKey,
  recallExcluded,
  rememberExcluded,
} from "./container-write";
import { workloadKey } from "./fmt";
import { AUTOMATED_MODES, LIST_KEY, type ListResp, type Rec } from "./types";

export type WorkloadPatch = {
  namespace: string;
  kind: string;
  workload: string;
  profile?: string;
  mode?: string;
};

/**
 * Per-workload override (profile / mode). Optimistic: the list reflects the change at
 * once, rolls back on error, and refetches when settled.
 */
export function useSetWorkload(onError?: (e: unknown) => void) {
  const app = useMcpApp();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (b: WorkloadPatch) => callTool(app, "TAILOR_SET_WORKLOAD", b),
    onMutate: async (b) => {
      await qc.cancelQueries({ queryKey: LIST_KEY });
      const prev = qc.getQueryData<ListResp>(LIST_KEY);
      qc.setQueryData<ListResp>(LIST_KEY, (old) =>
        old
          ? {
              ...old,
              recommendations: old.recommendations.map((r) => {
                if (workloadKey(r) !== workloadKey(b)) return r;
                const next = { ...r };
                if (typeof b.profile === "string") next.profile = b.profile;
                if (typeof b.mode === "string") {
                  next.mode = b.mode;
                  next.automated = AUTOMATED_MODES.includes(b.mode);
                  next.overridden = b.mode !== "";
                }
                return next;
              }),
            }
          : old,
      );
      return { prev };
    },
    onError: (e, _b, ctx) => {
      if (ctx?.prev) qc.setQueryData(LIST_KEY, ctx.prev);
      onError?.(e);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: LIST_KEY }),
  });
}

/** True while a mutation for this workload is in flight. */
export function isPendingFor(m: ReturnType<typeof useSetWorkload>, r: Rec) {
  return m.isPending && !!m.variables && workloadKey(m.variables) === workloadKey(r);
}

/**
 * Per-container write (mode and/or exclude) through TAILOR_SET_WORKLOAD with `container`.
 * Optimistic on the recommendations list, rolled back on any error; the caller shows the
 * classified error. The engine reflects an explicit mode at once; "inherit" and re-inclusion
 * wait for its next refresh, so those refetch a little later instead of flashing stale data.
 */
export function useSetContainer() {
  const app = useMcpApp();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (w: ContainerWrite) => callTool(app, "TAILOR_SET_WORKLOAD", w),
    onMutate: async (w) => {
      await qc.cancelQueries({ queryKey: LIST_KEY });
      const prev = qc.getQueryData<ListResp>(LIST_KEY);
      const prevExcluded = recallExcluded(w);
      qc.setQueryData<ListResp>(LIST_KEY, (old) =>
        old
          ? {
              ...old,
              recommendations: old.recommendations.map((r) =>
                containerKey(r) === containerKey(w) ? applyContainerWrite(r, w) : r,
              ),
            }
          : old,
      );
      if (w.exclude !== undefined) rememberExcluded(w, w.exclude);
      return { prev, prevExcluded };
    },
    onError: (_e, w, ctx) => {
      if (ctx?.prev) qc.setQueryData(LIST_KEY, ctx.prev);
      if (w.exclude !== undefined) rememberExcluded(w, ctx?.prevExcluded);
    },
    onSettled: (_d, e, w) => {
      const lagging = w.mode === "inherit" || w.exclude === false;
      if (e || !lagging) qc.invalidateQueries({ queryKey: LIST_KEY });
      else setTimeout(() => qc.invalidateQueries({ queryKey: LIST_KEY }), 2_000);
    },
  });
}
