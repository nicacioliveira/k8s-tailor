import { describe, expect, test } from "bun:test";
import { formatDelta, formatNumber, formatUSD } from "../../ui/lib/format";
import { ago, change, mib, milli, nowSec, pct1 } from "../fmt";

describe("number formatting (en-US, typographic minus)", () => {
  test("groups thousands and fixes decimals on request", () => {
    expect(formatNumber(1234567.8)).toBe("1,234,567.8");
    expect(formatNumber(2.5, 2)).toBe("2.50");
  });
  test("negatives use U+2212, never a hyphen", () => {
    expect(formatNumber(-3)).toBe("−3");
    expect(formatUSD(-3)).toBe("−$3.00");
    expect(formatDelta(-5.2)).toBe("−5.2%");
    expect(formatDelta(12.3)).toBe("+12.3%");
  });
  test("USD always has cents", () => {
    expect(formatUSD(12.5)).toBe("$12.50");
    expect(formatUSD(0)).toBe("$0.00");
  });
});

describe("resource units", () => {
  test("cores -> millicores, bytes -> MiB, missing -> em dash", () => {
    expect(milli(0.25)).toBe("250m");
    expect(milli(1.2345)).toBe("1,235m");
    expect(mib(512 * 1024 * 1024)).toBe("512Mi");
    expect(milli(undefined)).toBe("—");
    expect(mib(null)).toBe("—");
  });
  test("ratio and relative change", () => {
    expect(pct1(0.123)).toBe("12.3%");
    expect(change(500, 250)).toBe("−50%");
    expect(change(100, 120)).toBe("+20%");
    expect(change(0, 5)).toBe("new");
    expect(change(0, 0)).toBe("—");
    expect(change(10, 10)).toBe("0%");
  });
  test("relative age", () => {
    const n = nowSec();
    expect(ago(n)).toBe("just now");
    expect(ago(n - 30)).toBe("30s ago");
    expect(ago(n - 300)).toBe("5m ago");
    expect(ago(n - 7200)).toBe("2h ago");
    expect(ago(n - 3 * 86400)).toBe("3d ago");
  });
});
