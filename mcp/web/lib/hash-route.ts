import { useCallback, useEffect, useState } from "react";

// Screens are hash routes so they can be linked and survive a reload, both standalone
// and inside an MCP host iframe: `#/<view>?<params>`. View state that should be
// shareable (workload filters, sort) lives in the params.

export type HashRoute = { view: string; params: URLSearchParams };

export function parseHash(hash: string): HashRoute {
  const h = hash.replace(/^#\/?/, "");
  const i = h.indexOf("?");
  const view = (i < 0 ? h : h.slice(0, i)).replace(/\/$/, "");
  return { view, params: new URLSearchParams(i < 0 ? "" : h.slice(i + 1)) };
}

export function buildHash(view: string, params?: URLSearchParams): string {
  const qs = params?.toString() ?? "";
  return `#/${view}${qs ? `?${qs}` : ""}`;
}

const EVT = "tailor:hashreplace";

/** Current hash route; `setParams` rewrites the query without adding history entries. */
export function useHashRoute() {
  const read = () => parseHash(globalThis.location?.hash ?? "");
  const [route, setRoute] = useState<HashRoute>(read);
  useEffect(() => {
    const on = () => setRoute(read());
    window.addEventListener("hashchange", on);
    window.addEventListener(EVT, on);
    return () => {
      window.removeEventListener("hashchange", on);
      window.removeEventListener(EVT, on);
    };
  }, []);
  const setParams = useCallback((params: URLSearchParams) => {
    const { view } = parseHash(location.hash);
    const next = buildHash(view || "overview", params);
    if (next === location.hash) return;
    history.replaceState(null, "", next);
    window.dispatchEvent(new Event(EVT));
  }, []);
  return { ...route, setParams };
}

export function navigate(view: string): void {
  location.hash = buildHash(view);
}
