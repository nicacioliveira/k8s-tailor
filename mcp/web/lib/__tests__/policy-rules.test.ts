import { describe, expect, test } from "bun:test";
import {
  INHERIT,
  blankRule,
  describeRule,
  moveRule,
  parseList,
  ruleHasMatch,
  setOpt,
  setProxyAnnotations,
  setRes,
} from "../policy-rules";
import type { ContainerRuleView } from "../types";

describe("container rules editing", () => {
  test("parseList", () => {
    expect(parseList(" a, b ,,c")).toEqual(["a", "b", "c"]);
    expect(parseList("")).toEqual([]);
  });
  test("a rule without names or classes matches nothing", () => {
    expect(ruleHasMatch(blankRule())).toBe(false);
    expect(ruleHasMatch({ match: { names: ["istio-proxy"] } })).toBe(true);
    expect(ruleHasMatch({ match: { classes: ["app"] } })).toBe(true);
  });
  test("inherit removes the key; other fields of the rule survive", () => {
    const r: ContainerRuleView = {
      match: { names: ["x"] },
      mode: "InPlace",
      cpu: { quantile: 0.99, margin: 0.3, min: "20m" },
    };
    expect(setOpt(r, "mode", INHERIT).mode).toBeUndefined();
    expect(setOpt(r, "apply", "Rollout").apply).toBe("Rollout");
    const c = setRes(r, "cpu", "max", "2");
    expect(c.cpu).toEqual({ quantile: 0.99, margin: 0.3, min: "20m", max: "2" });
    expect(
      setRes(setRes(blankRule(), "memory", "min", "96Mi"), "memory", "min", "").memory,
    ).toBeUndefined();
  });
  test("proxyAnnotations is tri-state", () => {
    const r = blankRule();
    expect(setProxyAnnotations(r, "true").proxyAnnotations).toBe(true);
    expect(setProxyAnnotations(r, "false").proxyAnnotations).toBe(false);
    expect(
      setProxyAnnotations({ ...r, proxyAnnotations: true }, INHERIT).proxyAnnotations,
    ).toBeUndefined();
  });
  test("moveRule swaps within bounds only (first match wins)", () => {
    const a = { match: { names: ["a"] } };
    const b = { match: { names: ["b"] } };
    expect(moveRule([a, b], 1, -1)).toEqual([b, a]);
    expect(moveRule([a, b], 0, -1)).toEqual([a, b]);
    expect(moveRule([a, b], 1, 1)).toEqual([a, b]);
  });
  test("describeRule", () => {
    expect(describeRule({ match: { names: ["vault-agent*"] }, exclude: true })).toBe(
      "vault-agent* → excluded",
    );
    expect(
      describeRule({ match: { names: ["istio-proxy"] }, mode: "InPlace", proxyAnnotations: true }),
    ).toBe("istio-proxy → mode InPlace, proxy annotations on");
    expect(describeRule(blankRule())).toBe("no match → no overrides");
  });
});
