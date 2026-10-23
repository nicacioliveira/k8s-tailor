import { describe, expect, test } from "bun:test";
import { csvCell, exportName, recsToCSV, recsToJSON } from "../export";
import { kubectlCommands, shq } from "../kubectl";
import { rec } from "./fixtures";

describe("CSV", () => {
  test("escapes quotes, commas and newlines (RFC 4180)", () => {
    expect(csvCell('a "b", c')).toBe('"a ""b"", c"');
    expect(csvCell("x\ny")).toBe('"x\ny"');
    expect(csvCell(undefined)).toBe("");
    expect(csvCell(-1.5)).toBe("-1.5");
    expect(csvCell(true)).toBe("true");
  });
  test("neutralises spreadsheet formulas in text cells", () => {
    expect(csvCell("=HYPERLINK(1)")).toBe("'=HYPERLINK(1)");
    expect(csvCell("@cmd")).toBe("'@cmd");
  });
  test("header + one row per rec with raw numbers in explicit units", () => {
    const csv = recsToCSV([rec({ wasteUsdWeek: 6.84 })]);
    const [head, row] = csv.trim().split("\r\n");
    const cols = head.split(",");
    const vals = row.split(",");
    const at = (c: string) => vals[cols.indexOf(c)];
    expect(at("namespace")).toBe("portal");
    expect(at("cpu_request_millicores")).toBe("1000");
    expect(at("cpu_recommended_millicores")).toBe("500");
    expect(at("mem_recommended_mib")).toBe("256");
    expect(at("waste_usd_week")).toBe("6.84");
    expect(at("status")).toBe("Over-provisioned");
    expect(at("mode")).toBe("Recommend");
  });
  test("JSON is the raw recs", () => {
    expect(JSON.parse(recsToJSON([rec()]))[0].workload).toBe("api");
  });
  test("file name carries a sortable timestamp", () => {
    expect(exportName("csv", new Date(2026, 9, 21, 18, 5))).toBe(
      "k8s-tailor-workloads-2026-10-21T1805.csv",
    );
  });
});

describe("kubectl from the dry-run patch", () => {
  const plan = {
    method: "InPlace",
    inPlace: { body: { spec: { containers: [{ name: "api" }] } } },
    rollout: { body: { spec: { template: { spec: { containers: [{ name: "api" }] } } } } },
  };
  test("single-quotes for the shell", () => {
    expect(shq("it's")).toBe(`'it'\\''s'`);
  });
  test("builds both commands, with and without server dry-run", () => {
    const c = kubectlCommands(
      { namespace: "portal", kind: "Deployment", workload: "api" },
      plan,
      true,
    );
    expect(c.rollout).toBe(
      `kubectl -n 'portal' patch deployment/'api' --type strategic -p '{"spec":{"template":{"spec":{"containers":[{"name":"api"}]}}}}' --dry-run=server -o yaml`,
    );
    expect(c.inPlace).toContain("patch pod POD --subresource resize --type strategic");
    const live = kubectlCommands(
      { namespace: "portal", kind: "Deployment", workload: "api" },
      plan,
      false,
    );
    expect(live.rollout).not.toContain("--dry-run");
  });
  test("no template patch for non-built-in kinds, with the reason", () => {
    const c = kubectlCommands({ namespace: "s", kind: "ksvc", workload: "api" }, plan, false);
    expect(c.rollout).toBeNull();
    expect(c.rolloutNote).toContain("not a built-in controller");
  });
});
