import { beforeEach, describe, expect, test } from "bun:test";
import { createFixtureShim } from "../../dev-fixtures/fixtures";
import { AuthError, clearToken, onAuthRequired } from "../auth";
import {
  WorkloadApiError,
  applyContainerWrite,
  classifyWriteError,
  controlsAvailability,
  currentChoice,
  isExcluded,
  needsOptInConfirm,
  recallExcluded,
  rememberExcluded,
  successText,
  writeEntries,
} from "../container-write";
import { createRestShim, route } from "../rest-transport";
import { rec } from "./fixtures";

const proxy = rec({
  container: "istio-proxy",
  containerClass: "injected-sidecar",
  mesh: "istio",
  containerPolicy: "default:injected-sidecar",
  mode: "Recommend",
  automated: false,
});

describe("controlsAvailability", () => {
  test("app, native and injected sidecars are enabled", () => {
    expect(controlsAvailability(rec()).enabled).toBe(true);
    expect(controlsAvailability(rec({ containerClass: "native-sidecar" })).enabled).toBe(true);
    expect(controlsAvailability(proxy).enabled).toBe(true);
    // an engine from before classification sends no class: a plain app
    expect(controlsAvailability(rec({ containerClass: undefined })).enabled).toBe(true);
  });
  test.each(["init", "ephemeral"] as const)("%s containers are 'never resized'", (cls) => {
    const a = controlsAvailability(rec({ containerClass: cls }));
    expect(a.enabled).toBe(false);
    if (!a.enabled) {
      expect(a.reason).toBe("never-resized");
      expect(a.message).toContain("never resized");
    }
  });
  test.each(["ksvc", "group"])("%s is not supported for per-container writes", (kind) => {
    const a = controlsAvailability(rec({ kind }));
    expect(a.enabled).toBe(false);
    if (!a.enabled) {
      expect(a.reason).toBe("unsupported-kind");
      expect(a.message).toContain("Per-container writes are not supported for this kind");
    }
  });
  test("never-resized wins over the kind", () => {
    const a = controlsAvailability(rec({ kind: "ksvc", containerClass: "init" }));
    expect(!a.enabled && a.reason).toBe("never-resized");
  });
});

describe("currentChoice / opt-in", () => {
  test("only an explicit annotation shows a mode, everything else inherits", () => {
    expect(currentChoice({ containerPolicy: "policy", mode: "Both" })).toBe("inherit");
    expect(currentChoice({ containerPolicy: "default:injected-sidecar", mode: "Recommend" })).toBe(
      "inherit",
    );
    expect(currentChoice({ containerPolicy: "annotation", mode: "InPlace" })).toBe("InPlace");
    expect(currentChoice({ containerPolicy: "annotation", mode: "" })).toBe("inherit");
  });
  test("an actuating mode on a non-actuated injected sidecar needs confirmation", () => {
    expect(needsOptInConfirm(proxy, "InPlace")).toBe(true);
    expect(needsOptInConfirm(proxy, "Both")).toBe(true);
    expect(needsOptInConfirm(proxy, "Webhook")).toBe(true);
    expect(needsOptInConfirm(proxy, "Recommend")).toBe(false);
    expect(needsOptInConfirm(proxy, "Off")).toBe(false);
    expect(needsOptInConfirm(proxy, "inherit")).toBe(false);
  });
  test("no confirmation for apps, native sidecars or an already actuated sidecar", () => {
    expect(needsOptInConfirm(rec(), "Both")).toBe(false);
    expect(needsOptInConfirm(rec({ containerClass: "native-sidecar" }), "InPlace")).toBe(false);
    expect(needsOptInConfirm({ ...proxy, automated: true, mode: "InPlace" }, "Both")).toBe(false);
  });
});

describe("optimistic edit and bodies", () => {
  const ref = { namespace: "portal", kind: "Deployment", workload: "api", container: "api" };
  test("an explicit mode becomes an annotation-sourced, actuated mode", () => {
    const n = applyContainerWrite(rec(), { ...ref, mode: "InPlace" });
    expect([n.mode, n.automated, n.containerPolicy, n.overridden]).toEqual([
      "InPlace",
      true,
      "annotation",
      true,
    ]);
    expect(applyContainerWrite(rec(), { ...ref, mode: "Off" }).automated).toBe(false);
  });
  test("inherit drops the annotation origin and leaves the rest to the refresh", () => {
    const n = applyContainerWrite(
      rec({ containerPolicy: "annotation", mode: "InPlace", automated: true }),
      { ...ref, mode: "inherit" },
    );
    expect(n.containerPolicy).toBe("policy");
    expect(n.mode).toBe("InPlace");
  });
  test("exclude is remembered and rolled back through the store", () => {
    const r = rec({ container: "x" });
    expect(isExcluded(r)).toBe(false);
    rememberExcluded(r, true);
    expect(isExcluded(r)).toBe(true);
    rememberExcluded(r, undefined);
    expect(recallExcluded(r)).toBeUndefined();
    expect(isExcluded(applyContainerWrite(r, { ...ref, container: "x", exclude: true }))).toBe(
      true,
    );
  });
  test("annotation entries and success text", () => {
    expect(writeEntries({ mode: "InPlace", exclude: true })).toEqual({
      mode: "InPlace",
      exclude: "true",
    });
    expect(writeEntries({ mode: "inherit", exclude: false })).toEqual({});
    expect(successText({ ...ref, mode: "InPlace" })).toBe("Saved: api mode set to InPlace.");
    expect(successText({ ...ref, exclude: false })).toBe("Saved: api no longer excluded.");
  });
});

describe("classifyWriteError", () => {
  test.each([
    ["invalid_mode", 400, "not valid"],
    ["container_not_found", 404, "not seen this container"],
    ["container_not_resizable", 400, "never resized"],
    ["unsupported_kind", 400, "not supported for this kind"],
    ["workload_not_found", 404, "does not see this workload"],
    ["nothing_to_change", 400, "Nothing to change"],
    ["bad_request", 400, "could not read"],
  ])("REST %s maps to a human sentence", (code, status, frag) => {
    const f = classifyWriteError(new WorkloadApiError(status, code, "raw"));
    expect(f.kind).toBe("api");
    expect(f.code).toBe(code);
    expect(f.message).toContain(frag);
    expect(f.message).not.toContain(code);
  });
  test("the MCP text prefix is parsed the same way", () => {
    const f = classifyWriteError(
      new Error('[engine:container_not_found:404] "x" in Deployment a/b'),
    );
    expect([f.kind, f.code, f.status]).toEqual(["api", "container_not_found", 404]);
    expect(f.detail).toContain("Deployment a/b");
  });
  test("guards without a code map by status", () => {
    expect(classifyWriteError(new WorkloadApiError(415, "", "x")).message).toContain(
      "content type",
    );
    expect(classifyWriteError(new WorkloadApiError(413, "", "x")).message).toContain("too large");
    expect(classifyWriteError(new WorkloadApiError(403, "", "x")).message).toContain("cross-site");
  });
  test("auth failures are recognised from AuthError and from MCP text", () => {
    expect(classifyWriteError(new AuthError(401, "needs token", true)).kind).toBe("auth");
    expect(
      classifyWriteError(new Error("engine /api/workload -> 401: the engine rejected")).kind,
    ).toBe("auth");
    expect(
      classifyWriteError(new Error("engine /api/workload -> 503: TAILOR_API_AUTH_REQUIRED")).code,
    ).toBe("writes_disabled");
  });
  test("network and unknown errors", () => {
    expect(classifyWriteError(new TypeError("Failed to fetch")).kind).toBe("network");
    expect(classifyWriteError(new Error("boom")).kind).toBe("unknown");
  });
});

describe("REST client", () => {
  test("the tool call posts the container body unchanged", () => {
    expect(
      route("TAILOR_SET_WORKLOAD", {
        namespace: "a",
        kind: "Deployment",
        workload: "w",
        container: "c",
        mode: "inherit",
      }),
    ).toEqual([
      "POST",
      "/api/workload",
      { namespace: "a", kind: "Deployment", workload: "w", container: "c", mode: "inherit" },
    ]);
  });
  const shim = (status: number, body: unknown, headers: Record<string, string> = {}) =>
    createRestShim(
      "",
      async () =>
        new Response(typeof body === "string" ? body : JSON.stringify(body), { status, headers }),
    );
  const call = (s: ReturnType<typeof shim>) =>
    s.callServerTool({
      name: "TAILOR_SET_WORKLOAD",
      arguments: { namespace: "a", kind: "Deployment", workload: "w", container: "c", mode: "Off" },
    });
  test("a 400 keeps status and code", async () => {
    const e = (await call(shim(400, { error: "bad mode", code: "invalid_mode" })).catch(
      (x) => x,
    )) as WorkloadApiError;
    expect(e).toBeInstanceOf(WorkloadApiError);
    expect([e.status, e.code, e.message]).toEqual([400, "invalid_mode", "bad mode"]);
  });
  test("a 404 container_not_found keeps its code", async () => {
    const e = (await call(shim(404, { error: "gone", code: "container_not_found" })).catch(
      (x) => x,
    )) as WorkloadApiError;
    expect(e.code).toBe("container_not_found");
  });
  test("a 401 raises the token dialog and an AuthError", async () => {
    clearToken();
    let asked = 0;
    const off = onAuthRequired(() => asked++);
    const e = await call(shim(401, { error: "unauthorized" })).catch((x) => x);
    off();
    expect(e).toBeInstanceOf(AuthError);
    expect(asked).toBe(1);
  });
  test("a plain-text 415 has no code", async () => {
    const e = (await call(shim(415, "unsupported media type")).catch((x) => x)) as WorkloadApiError;
    expect([e.status, e.code]).toEqual([415, ""]);
  });
});

describe("fixture engine contract", () => {
  let fx: ReturnType<typeof createFixtureShim>;
  beforeEach(() => {
    clearToken();
    fx = createFixtureShim("");
  });
  const write = (args: Record<string, unknown>) =>
    fx
      .callServerTool({ name: "TAILOR_SET_WORKLOAD", arguments: args })
      .then((r: any) => r.structuredContent);
  const fail = async (args: Record<string, unknown>) =>
    (await write(args).catch((e) => e)) as WorkloadApiError;
  const portal = { namespace: "portal", kind: "Deployment", workload: "gateway" };

  test("workload write is unchanged: {ok:true}, and inherit is an alias of ''", async () => {
    expect(await write({ ...portal, mode: "Both" })).toEqual({ ok: true });
    expect(await write({ ...portal, mode: "inherit" })).toEqual({ ok: true });
  });
  test("container write echoes the engine response", async () => {
    expect(
      await write({ ...portal, container: "istio-proxy", mode: "InPlace", exclude: false }),
    ).toEqual({
      ok: true,
      ...portal,
      container: "istio-proxy",
      mode: "InPlace",
      exclude: false,
    });
    const list: any = await fx
      .callServerTool({ name: "TAILOR_LIST_RECOMMENDATIONS", arguments: {} })
      .then((r: any) => r.structuredContent);
    const p = list.recommendations.find(
      (r: any) => r.container === "istio-proxy" && r.workload === "gateway",
    );
    expect([p.mode, p.automated, p.containerPolicy]).toEqual(["InPlace", true, "annotation"]);
    expect((await write({ ...portal, container: "istio-proxy", mode: "" })).mode).toBe("inherit");
  });
  test("refusals carry the engine's status and code", async () => {
    const cases: [Record<string, unknown>, number, string][] = [
      [{ ...portal, container: "istio-proxy", mode: "Auto" }, 400, "invalid_mode"],
      [{ ...portal, container: "istio-proxy" }, 400, "nothing_to_change"],
      [{ ...portal, container: "istio-proxy", mode: "Off", profile: "web" }, 400, "bad_request"],
      [{ ...portal, exclude: true }, 400, "bad_request"],
      [{ ...portal, container: "istio-init", mode: "Off" }, 400, "container_not_resizable"],
      [{ ...portal, container: "nope", mode: "Off" }, 404, "container_not_found"],
      [{ ...portal, workload: "ghost", container: "x", mode: "Off" }, 404, "workload_not_found"],
      [
        { namespace: "serving", kind: "ksvc", workload: "api", container: "api", mode: "Off" },
        400,
        "unsupported_kind",
      ],
      [
        {
          namespace: "pay",
          kind: "Deployment",
          workload: "payments",
          container: "audit-agent",
          mode: "Both",
        },
        404,
        "container_not_found",
      ],
    ];
    for (const [args, status, code] of cases) {
      const e = await fail(args);
      expect(e).toBeInstanceOf(WorkloadApiError);
      expect([e.status, e.code]).toEqual([status, code]);
    }
  });
  test("the auth variant answers 401 until the demo token is stored", async () => {
    fx = createFixtureShim("auth");
    const e = await write({ ...portal, container: "istio-proxy", mode: "Off" }).catch((x) => x);
    expect(e).toBeInstanceOf(AuthError);
  });
});
