import { describe, expect, test } from "bun:test";
import {
  annotateSnippet,
  classLabel,
  classOf,
  enableSnippet,
  groupSiblings,
  needsSidecarOptIn,
  originText,
  pathIsNone,
  pathText,
} from "../containers";
import {
  EMPTY_FILTERS,
  filterRecs,
  parseFilters,
  serializeFilters,
  sortRecs,
} from "../workload-filter";
import { rec } from "./fixtures";

const proxy = rec({
  container: "istio-proxy",
  containerClass: "injected-sidecar",
  mesh: "istio",
  nativeSidecar: true,
  inTemplate: false,
  containerPolicy: "default:injected-sidecar",
  actuationPath: "none",
});

describe("class", () => {
  test("an engine without classification means app", () => {
    expect(classOf(rec())).toBe("app");
    expect(classLabel(rec())).toBe("App");
  });
  test("labels", () => {
    expect(classLabel(proxy)).toBe("Sidecar · istio");
    expect(classLabel({ containerClass: "injected-sidecar", mesh: "" })).toBe("Sidecar");
    expect(classLabel({ containerClass: "native-sidecar", mesh: "" })).toBe("Native sidecar");
    expect(classLabel({ containerClass: "init", mesh: "" })).toBe("Init");
    expect(classLabel({ containerClass: "ephemeral", mesh: "" })).toBe("Ephemeral");
  });
});

describe("origin and path in words", () => {
  test("origins", () => {
    expect(originText({})).toBe("from policy");
    expect(originText({ containerPolicy: "policy" })).toBe("from policy");
    expect(originText(proxy)).toBe("default for injected sidecars");
    expect(originText({ containerPolicy: "annotation" })).toBe("container annotation");
    expect(originText({ containerPolicy: "policy:prod-mesh containers[0]" })).toBe(
      "rule 1 of policy prod-mesh",
    );
    expect(originText({ containerPolicy: "profile:envoy containers[2]" })).toBe(
      "rule 3 of profile envoy",
    );
  });
  test("paths", () => {
    expect(pathText({ actuationPath: "in-place" })).toBe("in-place");
    expect(pathText({ actuationPath: "webhook+in-place" })).toBe(
      "webhook (at admission) + in-place",
    );
    expect(pathText({ actuationPath: "proxy-annotations:istio" })).toBe(
      "proxy annotations (istio)",
    );
    expect(pathText({ actuationPath: "none:not-in-template" })).toBe("not in template");
    expect(pathText({ actuationPath: "in-place+fallback:proxy-annotations:linkerd" })).toBe(
      "in-place + fallback to proxy annotations (linkerd)",
    );
    expect(pathText({ actuationPath: "template" })).toBe("template rollout");
    expect(pathText({})).toBe("not reported");
  });
  test("pathIsNone only when nothing can change the container", () => {
    expect(pathIsNone({ actuationPath: "none" })).toBe(true);
    expect(pathIsNone({ actuationPath: "none:not-in-template" })).toBe(true);
    expect(pathIsNone({ actuationPath: "in-place+none:not-in-template" })).toBe(false);
    expect(pathIsNone({})).toBe(false);
  });
});

describe("sidecar opt-in", () => {
  test("warns for an injected sidecar at Recommend, not once an annotation set it", () => {
    expect(needsSidecarOptIn(proxy)).toBe(true);
    expect(needsSidecarOptIn({ ...proxy, containerPolicy: "policy:p containers[1]" })).toBe(true);
    expect(needsSidecarOptIn({ ...proxy, mode: "InPlace", containerPolicy: "annotation" })).toBe(
      false,
    );
    expect(needsSidecarOptIn({ ...proxy, mode: "InPlace" })).toBe(false);
    expect(needsSidecarOptIn({ ...proxy, mode: "Off" })).toBe(false);
    expect(needsSidecarOptIn(rec())).toBe(false);
  });
  test("snippet is the documented per-container annotation, quoted", () => {
    expect(enableSnippet({ ...proxy, namespace: "portal", workload: "gateway" })).toBe(
      "kubectl -n 'portal' annotate deployment/'gateway' 'tailor.k8s-tailor.dev/container.istio-proxy.mode=InPlace' 'tailor.k8s-tailor.dev/container.istio-proxy.proxy-annotations=true' --overwrite",
    );
  });
  test("no proxy-annotations suggestion for a container that is in the template", () => {
    const s = enableSnippet({ ...proxy, inTemplate: true, workload: "x" });
    expect(s).not.toContain("proxy-annotations");
  });
  test("YAML for kinds without a kubectl resource", () => {
    const s = annotateSnippet({ namespace: "a", kind: "group", workload: "g" }, "c", {
      exclude: "true",
    });
    expect(s).toContain('tailor.k8s-tailor.dev/container.c.exclude: "true"');
    expect(s).toContain("pods");
  });
});

describe("siblings, filters and URL", () => {
  const app = rec({ workload: "co", container: "co", wasteUsdWeek: 5 });
  const other = rec({ workload: "sessions", container: "sessions", wasteUsdWeek: 3 });
  const sc = rec({
    workload: "co",
    container: "istio-proxy",
    containerClass: "injected-sidecar",
    mesh: "istio",
    wasteUsdWeek: 0.1,
  });
  const nat = rec({
    workload: "co",
    container: "fb",
    containerClass: "native-sidecar",
    wasteUsdWeek: 0.2,
  });
  const recs = [app, other, sc, nat];

  test("containers of a workload stay together, app first, between-workload order kept", () => {
    expect(groupSiblings(recs).map((r) => r.container)).toEqual([
      "co",
      "fb",
      "istio-proxy",
      "sessions",
    ]);
    expect(sortRecs(recs, "", "desc").map((r) => r.container)).toEqual([
      "co",
      "fb",
      "istio-proxy",
      "sessions",
    ]);
  });
  test("filters by class and mesh", () => {
    expect(filterRecs(recs, { ...EMPTY_FILTERS, cls: "injected-sidecar" })).toEqual([sc]);
    expect(filterRecs(recs, { ...EMPTY_FILTERS, cls: "app" }).length).toBe(2);
    expect(filterRecs(recs, { ...EMPTY_FILTERS, mesh: "istio" })).toEqual([sc]);
    expect(filterRecs(recs, { ...EMPTY_FILTERS, mesh: "linkerd" })).toEqual([]);
  });
  test("class and mesh round-trip through the URL", () => {
    const f = { ...EMPTY_FILTERS, cls: "native-sidecar", mesh: "istio" };
    const p = serializeFilters(f);
    expect(p.toString()).toBe("class=native-sidecar&mesh=istio");
    expect(parseFilters(p)).toMatchObject({ cls: "native-sidecar", mesh: "istio" });
  });
});
