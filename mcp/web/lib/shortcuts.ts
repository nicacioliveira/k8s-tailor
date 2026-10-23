import { useEffect, useRef } from "react";

// Global keyboard shortcuts (GitHub-style):
//   /        focus the page's filter field
//   g then o|w|s|p|r|d|h   go to Overview, Workloads, Savings, Policies, gRouping, Decisions, Health
//   ?        show the shortcut help
// Ignored while typing in a field, with Ctrl/Meta/Alt held, or inside an open dialog.

export const GO_KEYS: Record<string, string> = {
  o: "overview",
  w: "workloads",
  s: "savings",
  p: "policies",
  r: "grouping",
  d: "decisions",
  h: "health",
};

export type ShortcutAction =
  | { type: "focus-filter" }
  | { type: "help" }
  | { type: "go"; view: string }
  | null;

/** Pure step: (pending "g"?, key) -> action + new pending state. */
export function step(
  pendingG: boolean,
  key: string,
): { action: ShortcutAction; pendingG: boolean } {
  if (pendingG) {
    const view = GO_KEYS[key.toLowerCase()];
    return { action: view ? { type: "go", view } : null, pendingG: false };
  }
  if (key === "/") return { action: { type: "focus-filter" }, pendingG: false };
  if (key === "?") return { action: { type: "help" }, pendingG: false };
  if (key === "g") return { action: null, pendingG: true };
  return { action: null, pendingG: false };
}

export function isTypingTarget(t: EventTarget | null): boolean {
  if (!(t instanceof HTMLElement)) return false;
  if (t.isContentEditable) return true;
  const tag = t.tagName;
  if (tag === "TEXTAREA" || tag === "SELECT") return true;
  if (tag === "INPUT") {
    const type = (t as HTMLInputElement).type;
    return !["checkbox", "radio", "button", "submit", "reset", "range"].includes(type);
  }
  return t.getAttribute("role") === "combobox" || t.getAttribute("role") === "textbox";
}

/** Attribute that marks the field `/` focuses. */
export const FILTER_ATTR = "data-shortcut-filter";

export function useShortcuts(handlers: {
  go: (view: string) => void;
  help: () => void;
  focusFilter: () => void;
}) {
  const ref = useRef(handlers);
  ref.current = handlers;
  useEffect(() => {
    let pendingG = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
      if (isTypingTarget(e.target)) return;
      if (document.querySelector('[role="dialog"][data-state="open"], [role="alertdialog"]'))
        return;
      const r = step(pendingG, e.key);
      pendingG = r.pendingG;
      clearTimeout(timer);
      if (pendingG) timer = setTimeout(() => (pendingG = false), 1_200);
      if (!r.action) return;
      e.preventDefault();
      if (r.action.type === "go") ref.current.go(r.action.view);
      else if (r.action.type === "help") ref.current.help();
      else ref.current.focusFilter();
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      clearTimeout(timer);
    };
  }, []);
}
