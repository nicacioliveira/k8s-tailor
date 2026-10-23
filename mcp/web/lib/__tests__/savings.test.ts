import { describe, expect, test } from "bun:test";
import {
  appliedUsdWeek,
  decisionUsdWeek,
  neededUsdWeek,
  savingsTimeline,
  topWaste,
} from "../savings";
import { decision, rec } from "./fixtures";

const prices = { cpuCoreHr: 0.03, gbHr: 0.004 };

describe("savings maths (mirrors the engine formula)", () => {
  test("one decision: (Δcpu·$/core-hr + ΔGiB·$/GiB-hr) × 168 × replicas", () => {
    // 0.5 core and 0.5 GiB cut: (0.015 + 0.002) × 168 = 2.856 per replica
    expect(decisionUsdWeek(decision(), prices)).toBeCloseTo(2.856, 6);
    expect(decisionUsdWeek(decision(), prices, 3)).toBeCloseTo(8.568, 6);
    // a raise is a negative saving
    expect(
      decisionUsdWeek(decision({ fromCpu: 0.5, toCpu: 1, fromMem: 0, toMem: 0 }), prices),
    ).toBeLessThan(0);
  });
  test("timeline accumulates decisions at their time, with replicas from the current rec", () => {
    const series = [
      { T: 900, V: 40 },
      { T: 1000, V: 38 },
      { T: 1100, V: 35 },
    ];
    const tl = savingsTimeline(
      series,
      [decision({ time: 1000 }), decision({ time: 800 })],
      prices,
      [rec({ replicas: 2 })],
    );
    expect(tl.map((p) => p.applied)).toEqual([0, 5.71, 5.71]); // the t=800 one is before the window
    expect(tl.map((p) => p.projected)).toEqual([40, 38, 35]);
    expect(savingsTimeline([], [decision()], prices, [])).toEqual([]);
  });
  test("applied total, top-10 and needed", () => {
    expect(appliedUsdWeek([decision(), decision({ time: 10 })], prices, [], 500)).toBe(2.86);
    const recs = [rec({ wasteUsdWeek: 1 }), rec({ wasteUsdWeek: 5 }), rec({ wasteUsdWeek: -2 })];
    expect(topWaste(recs, 10).map((r) => r.wasteUsdWeek)).toEqual([5, 1]);
    expect(neededUsdWeek(recs)).toBe(2);
  });
});
