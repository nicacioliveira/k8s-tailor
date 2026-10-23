import { describe, expect, test } from "bun:test";
import { heldHint, heldResource, resourceConfidence } from "../confidence";

const c = (cpuConfident?: boolean, memConfident?: boolean) => ({ cpuConfident, memConfident });

describe("per-resource confidence", () => {
  test("the four combinations", () => {
    expect(heldHint(c(true, true))).toBeNull();
    expect(heldHint(c(true, false))).toBe("Memory held: not enough samples yet");
    expect(heldHint(c(false, true))).toBe("CPU held: not enough samples yet");
    expect(heldHint(c(false, false))).toBeNull(); // both held = "Gathering data", not a partial hold
    expect(heldResource(c(false, true))).toBe("cpu");
    expect(heldResource(c(true, false))).toBe("mem");
  });
  test("missing data is not confident", () => {
    expect(resourceConfidence(c(), "cpu")).toBe("unknown");
    expect(resourceConfidence(c(undefined, true), "cpu")).toBe("unknown");
    expect(resourceConfidence(c(true, undefined), "mem")).toBe("unknown");
    expect(heldHint(c(undefined, true))).toBeNull();
    expect(heldHint(c(false, undefined))).toBeNull();
  });
});

describe("fixtures", () => {
  test("cover all four confidence combinations", async () => {
    const { createFixtureShim } = await import("../../dev-fixtures/fixtures");
    const out: any = await createFixtureShim("").callServerTool({
      name: "TAILOR_LIST_RECOMMENDATIONS",
      arguments: {},
    });
    const seen = new Set(
      out.structuredContent.recommendations.map((r: any) => `${r.cpuConfident}/${r.memConfident}`),
    );
    expect([...seen].sort()).toEqual(["false/false", "false/true", "true/false", "true/true"]);
  });
});
