import { describe, expect, test } from "bun:test";
import { setWorkloadSchema } from "./tools.ts";

const base = { namespace: "portal", kind: "Deployment", workload: "gateway" };
const ok = (v: unknown) => setWorkloadSchema.safeParse(v).success;

describe("TAILOR_SET_WORKLOAD schema", () => {
  test("legacy workload-level calls still validate", () => {
    expect(ok({ ...base, mode: "Both" })).toBe(true);
    expect(ok({ ...base, profile: "web" })).toBe(true);
    expect(ok({ ...base, mode: "", profile: "" })).toBe(true);
    expect(ok({ ...base, mode: "inherit" })).toBe(true);
  });
  test("container writes need mode and/or exclude", () => {
    expect(ok({ ...base, container: "istio-proxy", mode: "InPlace" })).toBe(true);
    expect(ok({ ...base, container: "istio-proxy", exclude: false })).toBe(true);
    expect(ok({ ...base, container: "istio-proxy", mode: "inherit", exclude: true })).toBe(true);
    expect(ok({ ...base, container: "istio-proxy" })).toBe(false);
  });
  test("profile with container is invalid; exclude without container is invalid", () => {
    expect(ok({ ...base, container: "app", mode: "Off", profile: "web" })).toBe(false);
    expect(ok({ ...base, exclude: true })).toBe(false);
  });
  test("rejects unknown modes and empty identifiers", () => {
    expect(ok({ ...base, mode: "Auto" })).toBe(false);
    expect(ok({ ...base, container: "", mode: "Off" })).toBe(false);
    expect(ok({ namespace: "", kind: "Deployment", workload: "x", mode: "Off" })).toBe(false);
  });
  test("JSON schema export still works for tools/list", async () => {
    const { z } = await import("zod");
    const js = z.toJSONSchema(setWorkloadSchema) as { properties: Record<string, unknown> };
    expect(Object.keys(js.properties)).toEqual(
      expect.arrayContaining(["container", "exclude", "mode", "profile"]),
    );
  });
});
