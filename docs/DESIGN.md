# Design

Visual system for the K8s Tailor dashboard — one React app (`mcp/web/`) built into a single
self-contained HTML file and served in three places: the MCP host (as the `ui://` app resource),
the MCP server at `/`, and the Go engine at `:8080` (embedded via `web/dist`, REST transport).
All three get the same bundle, so they always look the same.

Product register: dense, operational, dark. A platform engineer scanning numbers mid-review.
Design serves the task — quiet, precise, trustworthy on sight.

## Foundation: the K8s Tailor design system

The UI is built on the project's own design system, which lives in this repository:

- `mcp/web/ds/` — design tokens (`tokens.css`), the Tailwind v4 bridge (`tailwind.css`, one
  `@theme inline` mapping utilities to `var(--…)`), base styles (`base.css`) and the Inter font
  (`fonts.css` + `fonts/`, SIL OFL). The font attribution is in `mcp/web/ds/NOTICE`.
- `mcp/web/ui/` — the subset of components the dashboard uses (Radix + CVA + Tailwind), with
  English / en-US formatting: Button, Badge, Banner, InfoBox, Card, Table, Sheet, Dialog,
  ConfirmDialog, Select, Switch, Checkbox, Slider, ToggleGroup, Tooltip, Popover, Calendar,
  NavTabs, SectionHeader, KpiCard, Sparkline, StatusIndicator, EmptyState, Skeleton, DataField,
  Chart (`ChartContainer` & co. over recharts 3).
- Build: `@tailwindcss/vite` in `mcp/vite.config.tool.ts`; `mcp/web/styles.css` imports Tailwind +
  the three token layers. `vite-plugin-singlefile` inlines JS, CSS and the font.

Rule: **app code composes `ui/` components and semantic utilities only.** No hex/rgb/oklch, no
Tailwind palette colours (`zinc-500`…), no `z-50`/`shadow-lg`, no hover-by-opacity in `.tsx`. If a
pattern is missing, compose existing components (see `components/`), don't restyle by hand.

## Theme

Dark only: `<html data-theme="dark">` in `entries/dashboard-ui.html` (+ `color-scheme: dark`).
There is no theme toggle. Light tokens still exist in `tokens.css` but are unused.

- Canvas `--background` (neutral-1), cards `--card` (neutral-2), popovers/sheets `--popover`
  (neutral-3). Elevation = one step up the 12-step neutral ladder, never opacity.
- Primary action: the light petrol `--primary` (one filled button per region).
- Status hues: `--success`, `--warning`, `--destructive`, `--info`, each with a tinted
  `*-surface` + `*-text` pair (AA contrast) used by soft badges, banners and info boxes.
- Borders are a 10% ink ring (`--border`); fields use `--input` (≥ 3:1).

## Brand

K8s Tailor has its own mark (`mcp/web/components/brand.tsx`): a hexagon (the cluster) with two
chevrons closing in (right-sizing), stroked in `currentColor` and shown in `text-primary`. The
favicon is the same drawing on a petrol tile, inlined as an SVG data URI in the HTML entry.

## Typography

Inter only (variable, weights 100–900, optical sizing), body weight 450 with `cv11`/`ss03`.
Type scale: `text-xs` 12/18, `text-sm` 14/21 (tables, body), `text-base` 16/24 (section titles),
`text-lg` (sheet titles). Labels are sentence case — no uppercase tracked "eyebrows". Every number
uses `tabular-nums` (tables get it by default) and en-US grouping (`1,024Mi`, `$412.37`); negatives
use the typographic minus `−`.

## Layout

- Sticky header: mark + workload count, actuation `StatusIndicator` (Actuating / Dry-run / Paused —
  shape + word), pricing badge (estimate vs measured), Pause/Resume, Refresh; `NavTabs` underneath.
- Screens are hash routes (`#/overview`, `#/workloads`, `#/savings`, `#/policies`, `#/grouping`,
  `#/decisions`, `#/health`) so they can be linked and survive reloads, including inside an MCP
  host iframe. Shareable view state rides in the hash query (`#/workloads?ns=portal&oom=1&sort=waste`,
  see `lib/hash-route.ts`); editing it uses `replaceState`, so filters don't flood history.
- Header extras: keyboard-shortcut help (`?`), and in engine REST mode an "Sign in / Token saved"
  control for the write token.
- Content column max 1280px, 16/24px gutters, 24px between regions. KPI grid 2 → 3 → 6 columns.
- Detail and editors open in a right `Sheet` (full width on mobile, `sm:max-w-2xl/3xl`).
- Works from 360px: the nav scrolls horizontally, tables scroll inside a focusable region.

## Screens

- **Overview** — six `KpiCard`s (cost, wasted % with a High/Moderate/Low badge, active savings with
  sparkline, automated %, over/under-provisioned counts) and the cluster CPU/memory charts.
- **Workloads** — compare chart (cluster usage, one workload's fit, or up to 4 workloads), search
  plus filters (namespace, status, kind, mode, only-OOM, only-pinned — all in the URL), sortable
  columns (workload, CPU/memory by size of the cut, $/week; `aria-sort`), export of the filtered
  rows as CSV / JSON / copy-CSV (`lib/export.ts`; formula-looking cells are neutralised),
  bulk "Automate all" / "Set all to recommend-only" (confirm dialog), table with status/kind/HPA/
  PDB/actuation badges, request → recommendation, limits, per-workload profile `Select`, automate
  `Switch` with Pinned badge + "follow policy" reset, $/week; row opens the workload sheet.
- **Workload sheet** — context notes (strategy, PDB, Knative, HPA-held, OOM, capped, low confidence),
  profile/mode/automated controls, CPU and memory fit charts, "Why this recommendation" data grid,
  dry-run patch disclosure rendered as copy-ready kubectl (in-place `--subresource resize` per pod
  and template strategic-merge patch; `--dry-run=server` toggle; raw engine JSON behind a
  `<details>`). Non-built-in kinds (ksvc…) get no template command, with the reason.
- **Savings** — reclaimable / captured (last 6 h, estimate) / needed / cost KPIs, a 6-hour
  timeline (engine reclaimable series as the area; cumulative $/week captured by applied decisions
  as the line, computed client-side with the engine's own formula in `lib/savings.ts`), and a
  top-10 reclaimable list linking into a filtered Workloads view.
- **Health** — actuation posture, capabilities and pricing basis, plus a data-source table
  (engine API, VictoriaMetrics, recommendations, usage series, savings series, decision log,
  pricing) derived from what the API returns; see *Backend gaps*.
- **Policies** / **Grouping** — table + sheet editor (sectioned form, headroom slider with a usage
  vs headroom preview, guardrails, schedule with calendar date fields; collapse-rule members table),
  destructive delete through `ConfirmDialog`.
- **Containers** — the API is per container (`/api/recommendations` has one entry per
  `(workload, container)`, see `docs/CONTAINERS.md` §2.5), so the table shows one row per
  container, kept together under their workload (engine order between workloads is preserved).
  Each row carries a **class badge** (icon + text: `App`, `Sidecar · istio`, `Native sidecar`,
  `Init`), the effective **mode + origin** (`from policy`, `default for injected sidecars`,
  `rule N of policy X`, `container annotation`) and the **actuation path in words**
  (`in-place`, `template rollout`, `proxy annotations (istio)`, `not in template`, `Not actuated`).
  An injected sidecar at Recommend gets a warning (`Setting a mode opts this sidecar in to actuation`);
  the workload Automate switch and profile select are replaced by read-only text for sidecars (the
  workload toggle never enables them). Filters `class=` and `mesh=` live in the URL hash like the
  others.
  **Per-container controls** (`components/container-controls.tsx`, writes through `POST /api/workload`
  with `container`): each container row of the workload sheet has a *Mode* select (Inherit / Off /
  Recommend / Webhook / InPlace / Both; Inherit removes the annotation) and an *Exclude* switch;
  sidecar rows in the table get the same select. Updates are optimistic with rollback; saving shows
  `Saving…`, success is announced in an `aria-live` line, and failures in a danger `Banner` with the
  engine `code` mapped to a sentence (`lib/container-write.ts`; MCP hosts get the same through the
  `[engine:<code>:<status>]` prefix of the tool error). A 401 opens the existing token dialog.
  Choosing an actuating mode on an injected sidecar that is not actuated yet asks for confirmation
  with the design-system `ConfirmDialog` (an explicit mode IS the opt-in). Init/ephemeral containers
  show the controls disabled (`never resized`), `ksvc`/`group` workloads too (`per-container writes
  are not supported for this kind`). Copying the equivalent annotation stays available under *Manage
  through GitOps instead*. Because the engine does not list excluded containers (gap 7), the
  Exclude switch remembers what this session wrote. The container list is the engine's last refresh
  (up to a minute old), so a just-created container can answer `container_not_found`.
  **Confidence per resource**: the sheet shows `CPU · Confident|Gathering data|Not reported` and
  `Memory · …` badges (icon + text); when only one resource is held, an info box and a table hint say
  `CPU held: not enough samples yet`. A missing `cpuConfident`/`memConfident` (older engine) is
  shown as *Not reported*, never as confident.
  The policy editor has a **Containers** section (rules: names/globs, classes,
  exclude, mode, apply, direction, profile, proxy annotations, cpu/memory min/max; reorderable,
  first match wins). Fields of a rule the form does not show (quantile, margin…) are preserved; a
  save that never touched the rules omits `containers`, which makes the engine keep the CR's rules.
- **Decisions** — filterable, range-bound table of applied resizes; rows expand to the exact change.

## Charts

`components/resource-chart.tsx` wraps the `ChartContainer` (legend, "View as table" alternative,
loading skeleton, empty state, `role="img"` summary). Palette `--chart-1…5` with a fixed meaning,
**max 4 series per chart**:

| Series | Token |
|---|---|
| Usage (area) | `chart-1` |
| Safety margin (stacked) | `chart-5` |
| Reclaimable / wasted headroom (stacked) | `chart-4` |
| Recommended request (line) | `chart-2` |

Compared workloads take `chart-1…4` in selection order (selection capped at 4). Reference lines
(request, limit, percentile, forecast, reserved) are dashed ink with a **text label carrying the
value**; actuation markers are muted vertical lines labelled "resized". The stack is honest: usage →
safety margin (usage..fit) → reclaimable waste (fit..request).

## States and rules

- Every data region has loading (`Skeleton` in the content's shape), empty (`EmptyState` that says
  what appears and what to do) and error (`Banner`/`InfoBox` danger with the engine's message and a
  retry where it makes sense). Failed mutations surface a dismissible danger banner.
- **Unavailable ≠ 0**: a failed overview renders "—" (announced "unavailable"), never `$0.00`.
- **Status never by colour alone**: badges carry words; $/week uses sign + trend icon + sr text;
  decision deltas show `−17%`/`+20%`; the actuation indicator has distinct shapes.
- No native `confirm()`/`alert()`: bulk actions and deletes use `ConfirmDialog` (destructive tone
  focuses Cancel; errors stay in the dialog).
- Icon-only buttons have `aria-label`; tooltips add detail but never hold the only copy of it.
- Focus: solid 2px `--ring` outline, 2px offset. Motion from the design tokens; all of it off under
  `prefers-reduced-motion`.

## Auth (standalone)

Inside an MCP host the host proxies auth. Standalone (`lib/auth.ts`, `components/auth.tsx`):

- **Engine REST mode (:8080)** — reads are open; every mutating `/api/*` call needs
  `Authorization: Bearer <TAILOR_API_TOKEN>` when the engine has one. A 401 opens a token dialog
  over the dashboard (you keep your place, the failed change shows in the danger banner and must
  be repeated); a 503 "auth required but no token configured" is reported as a server
  misconfiguration, without prompting.
- **MCP standalone (`/mcp`)** — everything is gated by `MCP_API_TOKEN`; a 401 shows a full-screen
  sign-in.
- Storage: `sessionStorage` by default, `localStorage` only with "Remember on this device".
  A shared link may carry the token only in the URL fragment (`#token=...` or `#/<view>?token=...`), which the browser never sends to a server or log; it is read once and removed from the address bar. The old `?token=` query form was removed and is ignored (and stripped).
- The MCP server forwards its own `TAILOR_API_TOKEN` env to the engine on every call
  (`server/engine.ts`) and turns a 401/503 into an explicit message.

## AI advisor (optional)

The advisor UI appears only when `GET /api/advisor/status` reports `enabled`; any failure of that call (older engine, engine down) hides it. Two surfaces, both explicit user actions (nothing runs on its own, it spends LLM budget):

- **AI review** in the workload sheet: "Analyze with AI", with a container picker when the workload has several. Calls `TAILOR_ADVISOR_REVIEW`.
- **AI summary** on Savings and Decisions: top workloads by weekly waste. Calls `TAILOR_ADVISOR_SUMMARY`.

Rules:

- All model text (rationale, risks, highlights, labels) is **untrusted** and rendered as plain React text nodes. No `innerHTML`, no markdown. `lib/advisor.ts` only coerces shapes; unknown verdicts degrade to "Review", never "OK".
- The verdict seal is icon + word + colour (OK / Review / Risky). Confidence, model and "Cached" are shown; cited numbers go in a table.
- A permanent line, "AI-generated — advisory only, never applies changes", sits in the panel before, during and after a result. The result region is `aria-live="polite"`.
- Errors use the `Banner` and are mapped in `mapAdvisorError` from the engine's `{error, code}`: 429 `rate_limited`/`budget_exceeded` (Retry-After, with a countdown on the retry button), 503 `circuit_open`, 502 `invalid_response`/`timeout`/`upstream_error`, 404 `disabled`/`not_found`, 400 `invalid_input`/`container_required`, 401 (opens the token dialog). In the MCP transport the server prefixes tool errors with `[advisor:<code>:<status>]` (+ `[retry-after:N]`) so the same mapper works.
- POSTs need the Bearer when `TAILOR_API_TOKEN` is set: the engine REST transport sends the saved token; the MCP server forwards its own `TAILOR_API_TOKEN` (the chart wires it from the same Secret as `tailor.apiAuth`).
- Dev fixtures: advisor on by default; `?fixtures=advisor-off`, `advisor-429`, `advisor-503`, `advisor-502`, `advisor-401`. The default review for the under-provisioned workload deliberately contains HTML to prove it renders literally.

## Keyboard

`/` focuses the page filter (Workloads search, Decisions filter; elsewhere it jumps to Workloads),
`g` then `o w s p r d h` navigates, `?` opens the help dialog. Ignored while typing or with a dialog
open (`lib/shortcuts.ts`).

## Bundle

Single self-contained HTML (MCP `ui://` resources must be one file), so `vite-plugin-singlefile`
inlines dynamic imports too: code-splitting / `React.lazy` saves **no bytes** here and modules are
still evaluated up front. Reductions therefore come from dead code: `@modelcontextprotocol/ext-apps`
does `import("zod/v4")` (whole namespace, every locale) only for `z.toJSONSchema` when an app
registers its own tools, which we never do — the build redirects that import to a module exposing
only `toJSONSchema` (`vite.config.tool.ts`). 1,523 kB → 1,357 kB raw (490 → 461 kB gzip) while
adding Savings/Health/export/shortcuts. Remaining weight: recharts 3 (+ redux/immer/d3) ≈ half,
react-dom, react-day-picker + date-fns, the MCP SDK, the Inter woff2 (~100 kB).

## Backend gaps (what the UI would need from the engine)

The Health and Savings screens only show what the API exposes. Missing, in priority order:

1. **Last recompute time** of recommendations (`generatedAt` is the response time, not the loop's).
2. **Per-source last error + timestamp** for the collection loop: Kubernetes informers,
   VictoriaMetrics queries, OpenCost/AWS pricing, ClickHouse writes.
3. **ClickHouse health** as such (today only inferred from `durable` on `/api/actuations`).
4. **Webhook health** (registered? last admission? failures).
5. **Engine version / build / uptime.**
6. **`/api/economy` range parameters** (it is fixed at the last 6 h, step 300 s) and an
   **applied-savings series** server-side (the UI estimates it from the decision log and the
   *current* replica count; the decision log doesn't store replicas or price at the time).
7. **Init and ephemeral containers are not listed** (they are never sized), and neither are
   **excluded** containers: the UI cannot show "excluded by rule/annotation" as a state, only the
   rules in the policy editor. A `containers[]` listing with `class` and `excluded` would fix it.
   The fixtures include an `init` row so the UI stays correct if the engine ever sends one.
8. **Policy profile (`TailorProfile`) rules** (`spec.containers` of a profile) are not exposed by
   `/api/profiles` (names only), so they cannot be edited here.

Also fixed on the way: the `TAILOR_SET_POLICY` tool schema dropped `direction` and
`oomOverridesDirection`, so saving a policy from the UI silently reset them; it now forwards both
and `containers`.

## Development

- `cd mcp && bun run dev:web` then open
  `http://localhost:5179/web/entries/dashboard-ui.html?fixtures` — mock data from
  `web/dev-fixtures/` (dev only, stripped from builds). `?fixtures=empty`, `?fixtures=error` and
  `?fixtures=noresize` exercise the empty, error and no-in-place-resize states; `?fixtures=auth`
  makes writes require the token `demo`; `?fixtures=vmdown` marks VictoriaMetrics unhealthy.
- `bun run build` (bundle + server), `bun run check` (tsc), `bun run test` (bun test: engine client
  auth against a fake engine, formatting, filters/URL, sort, CSV, kubectl, savings maths, token
  storage, REST 401/503 handling, shortcut state machine), `biome check mcp/server mcp/web`.
