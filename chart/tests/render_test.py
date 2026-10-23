#!/usr/bin/env python3
"""Render tests for the k8s-tailor chart (no cluster needed).

    python3 chart/tests/render_test.py            # from the repo root
    make chart-test                               # same, plus helm lint --strict

chart/charts/ (the victoria-metrics-single dependency) is gitignored, so
a clean clone has no subchart: the test runs `helm dependency build chart` first
(needs network once; Chart.lock pins the version) and fails clearly if it can't.

Renders the chart in several value combinations with `helm template`, asserts the
security/namespace invariants, and validates every manifest with kubeconform
(CRDs and cert-manager/monitoring CRs are skipped: no public schema).
Exit 1 on the first failing combination summary.
"""
import json
import os
import re
import subprocess
import sys

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
CHART = os.path.join(ROOT, "chart")
FAILS = []


def ensure_dependencies():
    """Vendor the chart dependencies (chart/charts/ is gitignored) or exit clearly."""
    for tool in ("helm", "kubeconform"):
        if subprocess.run(["which", tool], capture_output=True).returncode != 0:
            sys.exit(f"render_test: `{tool}` not found in PATH — install it to run the chart tests")
    p = subprocess.run(["helm", "dependency", "list", CHART], capture_output=True, text=True)
    if p.returncode == 0 and "missing" not in p.stdout and "wrong version" not in p.stdout:
        return
    print("render_test: chart dependencies missing — running `helm dependency build chart`")
    p = subprocess.run(["helm", "dependency", "build", CHART], capture_output=True, text=True)
    if p.returncode != 0:
        sys.exit("render_test: `helm dependency build chart` failed (network? repo URL in Chart.yaml) — "
                 "the chart cannot render without its subchart:\n" + p.stderr)


ensure_dependencies()


def helm(*args, ok=True):
    p = subprocess.run(["helm", "template", "t", CHART, *args], capture_output=True, text=True)
    if ok and p.returncode != 0:
        raise AssertionError(f"helm template {' '.join(args)} failed:\n{p.stderr}")
    return p


def docs(out):
    return [d for d in yaml.safe_load_all(out) if d]


def kubeconform(out, name):
    p = subprocess.run(
        ["kubeconform", "-strict", "-summary", "-kubernetes-version", "1.33.0",
         "-skip", "CustomResourceDefinition,Certificate,Issuer,ServiceMonitor,TailorProfile",
         "-schema-location", "default", "-"],
        input=out, capture_output=True, text=True)
    if p.returncode != 0:
        raise AssertionError(f"kubeconform [{name}]:\n{p.stdout}{p.stderr}")


def find(ds, kind, name=None):
    return [d for d in ds if d["kind"] == kind and (name is None or d["metadata"]["name"] == name)]


def one(ds, kind, name):
    r = find(ds, kind, name)
    assert len(r) == 1, f"expected 1 {kind}/{name}, got {len(r)}"
    return r[0]


def env(container):
    return {e["name"]: e for e in container.get("env", [])}


def container(ds, deploy, cname):
    d = one(ds, "Deployment", deploy)
    return next(c for c in d["spec"]["template"]["spec"]["containers"] if c["name"] == cname)


def check(name):
    def case(fn):
        try:
            fn()
            print(f"PASS {name}")
        except Exception as e:  # noqa: BLE001 — report any breakage as FAIL
            print(f"FAIL {name}: {e}")
            FAILS.append(name)
        return fn
    return case


NAMESPACED = {"Deployment", "StatefulSet", "Service", "ConfigMap", "Secret", "ServiceAccount",
              "NetworkPolicy", "Role", "RoleBinding", "PodDisruptionBudget", "Certificate",
              "Issuer", "ServiceMonitor"}


@check("release namespace: every namespaced object + derived URLs + exclusions (-n other)")
def _():
    out = helm("-n", "other").stdout
    kubeconform(out, "-n other")
    ds = docs(out)
    assert "k8s-tailor.svc" not in out and "namespace: k8s-tailor\n" not in out
    for d in ds:
        if d["kind"] in NAMESPACED:
            assert d["metadata"].get("namespace") == "other", f"{d['kind']}/{d['metadata']['name']} ns={d['metadata'].get('namespace')}"
    for crb in find(ds, "ClusterRoleBinding") + find(ds, "RoleBinding"):
        for s in crb["subjects"]:
            assert s["namespace"] == "other", crb["metadata"]["name"]
    e = env(container(ds, "k8s-tailor", "tailor"))
    excl = e["EXCLUDE_NAMESPACES"]["value"].split(",")
    for ns in ["other", "kube-system", "kube-public", "kube-node-lease", "istio-system", "linkerd", "cert-manager"]:
        assert ns in excl, f"{ns} not excluded: {excl}"
    assert excl[0] == "other"
    assert e["CH_URL"]["value"] == "http://k8s-tailor-clickhouse.other.svc:8123"
    assert e["VM_URL"]["value"] == "http://k8s-tailor-vmsingle.other.svc:8428"
    assert env(container(ds, "k8s-tailor-mcp", "mcp"))["TAILOR_API"]["value"] == "http://k8s-tailor.other.svc:8080"


@check("legacy namespace value that disagrees with the release fails clearly")
def _():
    p = helm("-n", "other", "--set", "namespace=k8s-tailor", ok=False)
    assert p.returncode != 0 and "no longer moves resources" in p.stderr, p.stderr
    helm("-n", "k8s-tailor", "--set", "namespace=k8s-tailor")  # matching = accepted


@check("defaults: write API auth ON with a generated, kept token shared with the MCP")
def _():
    out = helm("-n", "k8s-tailor").stdout
    kubeconform(out, "defaults")
    ds = docs(out)
    sec = one(ds, "Secret", "k8s-tailor-api-auth")
    assert sec["metadata"]["annotations"]["helm.sh/resource-policy"] == "keep"
    assert len(sec["stringData"]["token"]) == 32
    e = env(container(ds, "k8s-tailor", "tailor"))
    assert e["TAILOR_API_AUTH_REQUIRED"]["value"] == "true"
    assert e["TAILOR_API_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "k8s-tailor-api-auth"
    m = env(container(ds, "k8s-tailor-mcp", "mcp"))
    assert m["TAILOR_API_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "k8s-tailor-api-auth"
    # MCP behind the sidecar: loopback only, probes via the sidecar port.
    assert m["MCP_HOST"]["value"] == "127.0.0.1"
    c = container(ds, "k8s-tailor-mcp", "mcp")
    # Named ports only resolve within the probed container: the sidecar port is numeric.
    assert c["livenessProbe"]["httpGet"]["port"] == 8080 and c["readinessProbe"]["httpGet"]["port"] == 8080
    assert "MCP_ALLOW_UNAUTHENTICATED" not in m
    # MIN_SAMPLES prod default: one daily cycle at 300s steps.
    assert e["MIN_SAMPLES"]["value"] == "288"


@check("apiAuth.existingSecret: no chart Secret, both pods read the external one")
def _():
    ds = docs(helm("--set", "tailor.apiAuth.existingSecret=ext", "--set", "tailor.apiAuth.secretKey=api-token").stdout)
    assert not find(ds, "Secret", "k8s-tailor-api-auth")
    for dep, c in [("k8s-tailor", "tailor"), ("k8s-tailor-mcp", "mcp")]:
        ref = env(container(ds, dep, c))["TAILOR_API_TOKEN"]["valueFrom"]["secretKeyRef"]
        assert ref == {"name": "ext", "key": "api-token"}, ref


@check("apiAuth.enabled=false: open write API (explicit opt-out)")
def _():
    ds = docs(helm("--set", "tailor.apiAuth.enabled=false").stdout)
    e = env(container(ds, "k8s-tailor", "tailor"))
    assert "TAILOR_API_TOKEN" not in e and "TAILOR_API_AUTH_REQUIRED" not in e
    assert not find(ds, "Secret", "k8s-tailor-api-auth")


@check("mcp.auth.enabled=false: Bun on 0.0.0.0 enforces MCP_API_TOKEN itself")
def _():
    out = helm("--set", "mcp.auth.enabled=false").stdout
    kubeconform(out, "mcp no sidecar")
    ds = docs(out)
    m = env(container(ds, "k8s-tailor-mcp", "mcp"))
    assert m["MCP_HOST"]["value"] == "0.0.0.0"
    assert m["MCP_API_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "k8s-tailor-mcp-auth"
    np = one(ds, "NetworkPolicy", "k8s-tailor-mcp")
    assert np["spec"]["ingress"][0]["ports"][0]["port"] == 8020
    ds = docs(helm("--set", "mcp.auth.enabled=false", "--set", "mcp.auth.allowUnauthenticated=true").stdout)
    m = env(container(ds, "k8s-tailor-mcp", "mcp"))
    assert m["MCP_ALLOW_UNAUTHENTICATED"]["value"] == "true" and "MCP_API_TOKEN" not in m


@check("NetworkPolicy for the MCP: release namespace + mcpFrom only")
def _():
    ds = docs(helm("-n", "x", "--set-json",
                   'networkPolicy.mcpFrom=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"ingress-nginx"}}}]').stdout)
    np = one(ds, "NetworkPolicy", "k8s-tailor-mcp")
    frm = np["spec"]["ingress"][0]["from"]
    assert frm[0] == {"podSelector": {}} and frm[1]["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"] == "ingress-nginx"
    assert np["spec"]["ingress"][0]["ports"][0]["port"] == 8080
    eng = one(ds, "NetworkPolicy", "k8s-tailor-engine")
    assert {"podSelector": {"matchLabels": {"app": "k8s-tailor-mcp"}}} in eng["spec"]["ingress"][0]["from"]


@check("images: GHCR defaults pinned to v<appVersion>, no :dev; string + digest overrides")
def _():
    out = helm().stdout
    assert ":dev" not in out
    ds = docs(out)
    app = yaml.safe_load(open(os.path.join(CHART, "Chart.yaml")))["appVersion"]
    assert container(ds, "k8s-tailor", "tailor")["image"] == f"ghcr.io/nicacioliveira/k8s-tailor:v{app}"
    assert container(ds, "k8s-tailor-mcp", "mcp")["image"] == f"ghcr.io/nicacioliveira/k8s-tailor-mcp:v{app}"
    ds = docs(helm("--set", "tailor.image=k8s-tailor:dev", "--set", "mcp.image.digest=sha256:abc").stdout)
    assert container(ds, "k8s-tailor", "tailor")["image"] == "k8s-tailor:dev"
    assert container(ds, "k8s-tailor-mcp", "mcp")["image"] == "ghcr.io/nicacioliveira/k8s-tailor-mcp@sha256:abc"


@check("CRDs are templates (upgraded with the release), kept on uninstall, togglable")
def _():
    ds = docs(helm().stdout)
    crds = find(ds, "CustomResourceDefinition")
    assert len(crds) == 4
    for c in crds:
        assert c["metadata"]["annotations"]["helm.sh/resource-policy"] == "keep"
    assert not os.path.isdir(os.path.join(CHART, "crds")), "chart/crds/ is never upgraded by Helm"
    assert not find(docs(helm("--set", "crds.install=false").stdout), "CustomResourceDefinition")
    ds = docs(helm("--set", "crds.keep=false").stdout)
    assert all("annotations" not in c["metadata"] for c in find(ds, "CustomResourceDefinition"))


@check("CRD numeric/enum validation present (limitFactor >= 1 or 0, quantile, margin, durations)")
def _():
    ds = docs(helm().stdout)
    pol = one(ds, "CustomResourceDefinition", "tailorpolicies.tailor.k8s-tailor.dev")
    spec = pol["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]
    lf = spec["memory"]["properties"]["limitFactor"]
    assert lf["minimum"] == 0 and lf["x-kubernetes-validations"][0]["rule"] == "self == 0.0 || self >= 1.0"
    assert lf["default"] == 1.0
    q = spec["cpu"]["properties"]["quantile"]
    assert q["maximum"] == 1 and q["exclusiveMinimum"] is True
    assert spec["cpu"]["properties"]["margin"]["maximum"] == 10
    assert "pattern" in spec["halfLife"] and "pattern" in spec["scaleDown"]["properties"]["cooldown"]
    assert spec["headroomThreshold"]["maximum"] == 0.5
    rule = spec["containers"]["items"]["properties"]["cpu"]["properties"]["limitFactor"]
    assert "x-kubernetes-validations" in rule and "default" not in rule
    prof = one(ds, "CustomResourceDefinition", "tailorprofiles.tailor.k8s-tailor.dev")
    pspec = prof["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]
    assert "x-kubernetes-validations" in pspec["memory"]["properties"]["limitFactor"]
    # memory.enforceLimit (opt-in boolean, no default) on every memory model,
    # container rules included; never on cpu (the engine ignores it there)
    for sp in (spec, pspec):
        for mem in (sp["memory"], sp["containers"]["items"]["properties"]["memory"]):
            el = mem["properties"]["enforceLimit"]
            assert el["type"] == "boolean" and "default" not in el, el
        assert "enforceLimit" not in sp["cpu"]["properties"]
    # guaranteed: skip (default) | resizeBoth on the policy and on every container
    # rule (policy and profile), the same two values internal/policy validates
    g = spec["guaranteed"]
    assert g["enum"] == ["skip", "resizeBoth"] and g["default"] == "skip", g
    for sp in (spec, pspec):
        gr = sp["containers"]["items"]["properties"]["guaranteed"]
        assert gr["enum"] == ["skip", "resizeBoth"] and "default" not in gr, gr
    # autoQuantile / burstThreshold: opt-in, no CRD default (the engine env is the default)
    aq, bt = spec["autoQuantile"], spec["burstThreshold"]
    assert aq["type"] == "boolean" and "default" not in aq, aq
    assert bt["minimum"] == 1 and bt["exclusiveMinimum"] is True and bt["maximum"] == 1000 and "default" not in bt, bt
    # seasonGate bounds mirror internal/policy (minCorrelation (0, 1], minPairs [2, 10000])
    sg = spec["seasonGate"]["properties"]
    assert sg["minCorrelation"]["maximum"] == 1 and sg["minCorrelation"]["exclusiveMinimum"] is True
    assert sg["minPairs"]["type"] == "integer" and sg["minPairs"]["minimum"] == 2 and sg["minPairs"]["maximum"] == 10000
    assert "default" not in sg["minCorrelation"] and "default" not in sg["minPairs"]


@check("webhook certMode=helm without a live API fails loudly; certManager/existingSecret render")
def _():
    p = helm("--set", "tailor.webhook.enabled=true", "--set", "tailor.actuateNamespaces=sandbox", ok=False)
    assert p.returncode != 0 and "certMode=certManager" in p.stderr, p.stderr
    out = helm("--set", "tailor.webhook.enabled=true", "--set", "tailor.actuateNamespaces=sandbox",
               "--set", "tailor.webhook.allowEphemeralCA=true").stdout
    kubeconform(out, "webhook helm (ephemeral CA)")
    out = helm("-n", "other", "--set", "tailor.webhook.enabled=true", "--set", "tailor.actuateNamespaces=sandbox",
               "--set", "tailor.webhook.certMode=certManager").stdout
    kubeconform(out, "certManager")
    ds = docs(out)
    mwc = find(ds, "MutatingWebhookConfiguration")[0]
    assert mwc["metadata"]["annotations"]["cert-manager.io/inject-ca-from"] == "other/k8s-tailor-webhook"
    assert mwc["webhooks"][0]["clientConfig"]["service"]["namespace"] == "other"
    assert one(ds, "Certificate", "k8s-tailor-webhook")["spec"]["dnsNames"][0] == "k8s-tailor.other.svc"
    out = helm("--set", "tailor.webhook.enabled=true", "--set", "tailor.actuateNamespaces=sandbox",
               "--set", "tailor.webhook.certMode=existingSecret", "--set", "tailor.webhook.tls.existingSecret=wh",
               "--set", "tailor.webhook.tls.caBundle=Zm9v").stdout
    kubeconform(out, "existingSecret")


@check("secretGeneration.failWithoutLookup: GitOps render without existingSecrets fails, with them passes")
def _():
    p = helm("--set", "secretGeneration.failWithoutLookup=true", ok=False)
    assert p.returncode != 0 and "existingSecret" in p.stderr, p.stderr
    out = helm("--set", "secretGeneration.failWithoutLookup=true",
               "--set", "tailor.apiAuth.existingSecret=k8s-tailor", "--set", "mcp.auth.existingSecret=k8s-tailor",
               "--set", "clickhouse.auth.existingSecret=k8s-tailor").stdout
    ds = docs(out)
    assert not [s for s in find(ds, "Secret") if s["metadata"]["name"].startswith("k8s-tailor-")]


@check("externalStores: bundled VM/CH off, URLs from values, no stray secrets/RBAC")
def _():
    out = helm("--set", "victoria-metrics-single.enabled=false", "--set", "clickhouse.enabled=false",
               "--set", "tailor.env.VM_URL=http://vm.mon:8428", "--set", "tailor.env.CH_URL=http://ch.data:8123").stdout
    kubeconform(out, "externalStores")
    ds = docs(out)
    e = env(container(ds, "k8s-tailor", "tailor"))
    assert e["VM_URL"]["value"] == "http://vm.mon:8428" and e["CH_URL"]["value"] == "http://ch.data:8123"
    assert not find(ds, "StatefulSet") and not find(ds, "ClusterRole", "k8s-tailor-vmsingle")


@check("RBAC: engine least privilege; nodes/proxy only on vmsingle's SA")
def _():
    ds = docs(helm().stdout)
    rules = one(ds, "ClusterRole", "k8s-tailor-engine")["rules"]
    flat = [(g, r, v) for rl in rules for g in rl["apiGroups"] for r in rl["resources"] for v in rl["verbs"]]
    assert not [f for f in flat if f[1].startswith("nodes")], "engine must not read nodes"
    assert not [f for f in flat if f[2] == "watch"], "engine never watches"
    assert ("", "pods", "update") not in flat and ("apps", "deployments", "update") not in flat
    assert not [f for f in flat if f[0] == "events.k8s.io"]
    assert not [f for f in flat if f[1] == "secrets"]
    vm = one(ds, "ClusterRole", "k8s-tailor-vmsingle")["rules"]
    assert {"apiGroups": [""], "resources": ["nodes/proxy"], "verbs": ["get"]} in vm
    sts = one(ds, "StatefulSet", "k8s-tailor-vmsingle")
    assert sts["spec"]["template"]["spec"]["serviceAccountName"] == "k8s-tailor-vmsingle"
    assert one(ds, "ClusterRoleBinding", "k8s-tailor-vmsingle")["subjects"][0]["name"] == "k8s-tailor-vmsingle"


@check("engine probes + PDB only with replicas > 1")
def _():
    ds = docs(helm().stdout)
    c = container(ds, "k8s-tailor", "tailor")
    assert c["livenessProbe"]["httpGet"]["path"] == "/livez" and c["readinessProbe"]["httpGet"]["path"] == "/healthz"
    assert not find(ds, "PodDisruptionBudget")
    out = helm("--set", "tailor.replicas=2").stdout
    kubeconform(out, "replicas=2")
    assert one(docs(out), "PodDisruptionBudget", "k8s-tailor")["spec"]["minAvailable"] == 1
    p = helm("--set", "tailor.replicas=2", "--set", "tailor.leaderElection.enabled=false", ok=False)
    assert p.returncode != 0


@check("advisor + admissionPolicy render and validate")
def _():
    out = helm("-n", "other", "--set", "tailor.advisor.enabled=true", "--set", "tailor.advisor.baseURL=http://llm:4000/v1",
               "--set", "tailor.advisor.model=m", "--set", "tailor.advisor.existingSecret=llm",
               "--set", "tailor.admissionPolicy.enabled=true").stdout
    kubeconform(out, "advisor+vap")
    ds = docs(out)
    vap = one(ds, "ValidatingAdmissionPolicy", "k8s-tailor-patch-scope-workloads")
    assert "system:serviceaccount:other:k8s-tailor" in vap["spec"]["matchConditions"][0]["expression"]
    assert len(find(ds, "ValidatingAdmissionPolicy")) == 3
    assert one(ds, "ValidatingAdmissionPolicyBinding", "k8s-tailor-patch-scope-pods")["spec"]["validationActions"] == ["Deny"]


@check("dev/k3s values render (smoke overrides)")
def _():
    out = helm("-n", "k8s-tailor", "-f", os.path.join(ROOT, "dev/k3s/values-dev.yaml"),
               "--set", "tailor.image=k8s-tailor:dev", "--set", "mcp.image=k8s-tailor-mcp:dev").stdout
    kubeconform(out, "dev")
    ds = docs(out)
    assert container(ds, "k8s-tailor", "tailor")["image"] == "k8s-tailor:dev"
    assert env(container(ds, "k8s-tailor", "tailor"))["MIN_SAMPLES"]["value"] == "2"


@check("NetworkPolicy k8s-tailor-vmsingle: :8428 only from the engine + metricsFrom")
def _():
    peer = {"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "monitoring"}}}
    ds = docs(helm("-n", "x", "--set-json", "networkPolicy.metricsFrom=" + json.dumps([peer])).stdout)
    np = one(ds, "NetworkPolicy", "k8s-tailor-vmsingle")
    assert np["metadata"]["namespace"] == "x"
    assert np["spec"]["policyTypes"] == ["Ingress"]
    sel = np["spec"]["podSelector"]["matchLabels"]
    assert sel == {"app.kubernetes.io/name": "victoria-metrics-single", "app.kubernetes.io/instance": "t"}, sel
    # the selector really matches the subchart's pods
    sts = one(ds, "StatefulSet", "k8s-tailor-vmsingle")
    labels = sts["spec"]["template"]["metadata"]["labels"]
    assert all(labels.get(k) == v for k, v in sel.items()), labels
    ing = np["spec"]["ingress"]
    assert len(ing) == 1, ing
    assert ing[0]["ports"] == [{"protocol": "TCP", "port": 8428}], ing[0]["ports"]
    assert ing[0]["from"] == [{"podSelector": {"matchLabels": {"app": "k8s-tailor"}}}, peer], ing[0]["from"]
    # default: the engine alone
    np = one(docs(helm().stdout), "NetworkPolicy", "k8s-tailor-vmsingle")
    assert np["spec"]["ingress"][0]["from"] == [{"podSelector": {"matchLabels": {"app": "k8s-tailor"}}}]
    # no bundled VM / NetworkPolicy off: no policy
    assert not find(docs(helm("--set", "victoria-metrics-single.enabled=false",
                              "--set", "tailor.env.VM_URL=http://vm:8428").stdout), "NetworkPolicy", "k8s-tailor-vmsingle")
    assert not find(docs(helm("--set", "networkPolicy.enabled=false").stdout), "NetworkPolicy", "k8s-tailor-vmsingle")


@check("mcp.auth.token charset: inline token validated at render; existingSecret is not")
def _():
    for bad in ["has space", "semi;colon", "quote'x", "dollar$x", "brace}x", "nl\nx"]:
        p = helm("--set-string", f"mcp.auth.token={bad}", ok=False)
        assert p.returncode != 0 and "may only contain" in p.stderr, (bad, p.stderr)
    good = "Abc.0_9~+/=-xyz"
    ds = docs(helm("--set-string", f"mcp.auth.token={good}").stdout)
    assert one(ds, "Secret", "k8s-tailor-mcp-auth")["stringData"]["token"] == good
    # existingSecret: the chart never sees the value, so it can't validate it —
    # the render succeeds and no chart Secret is emitted (documented limitation).
    ds = docs(helm("--set", "mcp.auth.existingSecret=ext", "--set-string", "mcp.auth.token=has space").stdout)
    assert not find(ds, "Secret", "k8s-tailor-mcp-auth")
    dep = one(ds, "Deployment", "k8s-tailor-mcp")
    refs = [e["valueFrom"]["secretKeyRef"]["name"] for c in dep["spec"]["template"]["spec"]["containers"]
            for e in c.get("env", []) if "secretKeyRef" in e.get("valueFrom", {})
            and e["valueFrom"]["secretKeyRef"]["name"] != "k8s-tailor-api-auth"]
    assert refs and set(refs) == {"ext"}, refs


@check("VAP meshKeys == ProxyAnnotationKeys in internal/recommender/containers.go (8 keys)")
def _():
    src = open(os.path.join(ROOT, "internal/recommender/containers.go")).read()
    body = re.search(r"func ProxyAnnotationKeys\(.*?\n}\n", src, re.S).group(0)
    want = sorted(re.findall(r'"([a-z0-9.\-]+/[A-Za-z0-9\-]+)"', body))
    assert len(want) == 8, want
    ds = docs(helm("--set", "tailor.admissionPolicy.enabled=true").stdout)
    vaps = [v for v in find(ds, "ValidatingAdmissionPolicy")
            if any(x["name"] == "meshKeys" for x in v["spec"].get("variables", []))]
    assert vaps, "no VAP declares the meshKeys variable"
    for v in vaps:
        expr = next(x["expression"] for x in v["spec"]["variables"] if x["name"] == "meshKeys")
        got = sorted(re.findall(r"'([^']+)'", expr))
        assert got == want, f"{v['metadata']['name']}: {got} != {want}"


@check("global.imagePullSecrets reaches every pod (engine, MCP, ClickHouse, vmsingle subchart)")
def _():
    out = helm("--set-json", 'global.imagePullSecrets=[{"name":"ghcr-pull"}]').stdout
    kubeconform(out, "imagePullSecrets")
    ds = docs(out)
    pods = [d for d in ds if d["kind"] in ("Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob")]
    names = {d["metadata"]["name"] for d in pods}
    assert {"k8s-tailor", "k8s-tailor-mcp", "k8s-tailor-clickhouse", "k8s-tailor-vmsingle"} <= names, names
    for d in pods:
        spec = d["spec"]["template"]["spec"]
        assert spec.get("imagePullSecrets") == [{"name": "ghcr-pull"}], f"{d['kind']}/{d['metadata']['name']}: {spec.get('imagePullSecrets')}"
    # plain-string form is normalised for our pods
    ds = docs(helm("--set-json", 'global.imagePullSecrets=["ghcr-pull"]').stdout)
    for dep in ("k8s-tailor", "k8s-tailor-mcp"):
        assert one(ds, "Deployment", dep)["spec"]["template"]["spec"]["imagePullSecrets"] == [{"name": "ghcr-pull"}]
    # default: none anywhere
    assert "imagePullSecrets" not in helm().stdout


@check("engine: VM_JOB from tailor.vmJob (default kubelet-resource = vmsingle job), terminationGracePeriodSeconds")
def _():
    ds = docs(helm().stdout)
    e = env(container(ds, "k8s-tailor", "tailor"))
    assert e["VM_JOB"]["value"] == "kubelet-resource"
    vals = yaml.safe_load(open(os.path.join(CHART, "values.yaml")))
    jobs = [j["job_name"] for j in vals["victoria-metrics-single"]["server"]["scrape"]["config"]["scrape_configs"]]
    assert e["VM_JOB"]["value"] in jobs, jobs
    spec = one(ds, "Deployment", "k8s-tailor")["spec"]["template"]["spec"]
    assert spec["terminationGracePeriodSeconds"] >= 30
    e = env(container(docs(helm("--set", "tailor.vmJob=").stdout), "k8s-tailor", "tailor"))
    assert e["VM_JOB"]["value"] == "", e["VM_JOB"]
    e = env(container(docs(helm("--set", "tailor.env.VM_JOB=cadvisor").stdout), "k8s-tailor", "tailor"))
    assert e["VM_JOB"]["value"] == "cadvisor"



@check("engine: VM_THROTTLE_JOB from tailor.vmThrottleJob (default kubelet-cadvisor = vmsingle cAdvisor job)")
def _():
    e = env(container(docs(helm().stdout), "k8s-tailor", "tailor"))
    assert e["VM_THROTTLE_JOB"]["value"] == "kubelet-cadvisor"
    vals = yaml.safe_load(open(os.path.join(CHART, "values.yaml")))
    jobs = [j["job_name"] for j in vals["victoria-metrics-single"]["server"]["scrape"]["config"]["scrape_configs"]]
    assert e["VM_THROTTLE_JOB"]["value"] in jobs, jobs
    e = env(container(docs(helm("--set", "tailor.vmThrottleJob=").stdout), "k8s-tailor", "tailor"))
    assert e["VM_THROTTLE_JOB"]["value"] == "", e["VM_THROTTLE_JOB"]
    e = env(container(docs(helm("--set", "tailor.env.VM_THROTTLE_JOB=cadvisor").stdout), "k8s-tailor", "tailor"))
    assert e["VM_THROTTLE_JOB"]["value"] == "cadvisor"


@check("engine: OOM_MEM_CAP_FACTOR from tailor.oomMemCapFactor (default 4, validated >= OOM_GROWTH_FACTOR)")
def _():
    e = env(container(docs(helm().stdout), "k8s-tailor", "tailor"))
    assert e["OOM_MEM_CAP_FACTOR"]["value"] == "4", e.get("OOM_MEM_CAP_FACTOR")
    e = env(container(docs(helm("--set", "tailor.oomMemCapFactor=2.5").stdout), "k8s-tailor", "tailor"))
    assert e["OOM_MEM_CAP_FACTOR"]["value"] == "2.5", e["OOM_MEM_CAP_FACTOR"]
    # tailor.env wins (rendered once, from env)
    e = env(container(docs(helm("--set", "tailor.env.OOM_MEM_CAP_FACTOR=6").stdout), "k8s-tailor", "tailor"))
    assert e["OOM_MEM_CAP_FACTOR"]["value"] == "6", e["OOM_MEM_CAP_FACTOR"]
    # below the growth factor, zero or not a number: the render fails
    for bad in (["--set", "tailor.oomMemCapFactor=1.2"],
                ["--set", "tailor.oomMemCapFactor=3", "--set", "tailor.env.OOM_GROWTH_FACTOR=3.5"],
                ["--set", "tailor.oomMemCapFactor=-1"], ["--set", "tailor.oomMemCapFactor=0"],
                ["--set-string", "tailor.oomMemCapFactor=four"]):
        r = helm(*bad, ok=False)
        assert r.returncode != 0 and "oomMemCapFactor" in r.stderr, (bad, r.returncode, r.stderr[-300:])


@check("values-vmsingle-chown.yaml: opt-in chown init container with the minimum privileges, absent by default")
def _():
    def vm_pod(ds):
        d = next(d for d in ds if d.get("kind") in ("Deployment", "StatefulSet") and d["metadata"]["name"] == "k8s-tailor-vmsingle")
        return d["spec"]["template"]["spec"]
    assert not vm_pod(docs(helm().stdout)).get("initContainers"), "the chown init container must be opt-in"
    spec = vm_pod(docs(helm("-f", os.path.join(CHART, "values-vmsingle-chown.yaml")).stdout))
    (ic,) = spec["initContainers"]
    sc = ic["securityContext"]
    assert ic["command"] == ["chown", "-R", "65534:65534", "/storage"], ic["command"]
    assert sc["runAsUser"] == 0 and sc["allowPrivilegeEscalation"] is False and sc["readOnlyRootFilesystem"] is True, sc
    assert sc["capabilities"] == {"drop": ["ALL"], "add": ["CHOWN"]}, sc["capabilities"]
    main = next(c for c in spec["containers"] if c["name"] == "vmsingle")
    data_mount = next(m for m in main["volumeMounts"] if m["name"] == "server-volume")
    assert data_mount["mountPath"] == "/storage" == ic["volumeMounts"][0]["mountPath"], (data_mount, ic["volumeMounts"])
    assert spec["securityContext"]["runAsNonRoot"] is True and spec["securityContext"]["runAsUser"] == 65534, "the vmsingle container itself stays non-root"


@check("warm cache: namespaced Role for k8s-tailor-warm + WARM_CACHE/WARM_TTL, off with tailor.warmCache.enabled=false")
def _():
    ds = docs(helm("-n", "rel-ns").stdout)
    role = one(ds, "Role", "k8s-tailor-warm-cache")
    assert role["metadata"]["namespace"] == "rel-ns", role["metadata"]
    cm = [r for r in role["rules"] if r["resources"] == ["configmaps"]]
    assert len(cm) == 2, role["rules"]
    create = next(r for r in cm if "create" in r["verbs"])
    assert create["verbs"] == ["create"] and "resourceNames" not in create, create
    upd = next(r for r in cm if "update" in r["verbs"])
    assert sorted(upd["verbs"]) == ["get", "update"] and upd["resourceNames"] == ["k8s-tailor-warm"], upd
    rb = one(ds, "RoleBinding", "k8s-tailor-warm-cache")
    assert rb["roleRef"]["name"] == "k8s-tailor-warm-cache" and rb["subjects"][0]["namespace"] == "rel-ns", rb
    e = env(container(ds, "k8s-tailor", "tailor"))
    assert e["WARM_CACHE"]["value"] == "true" and e["WARM_TTL"]["value"] == "15m", (e.get("WARM_CACHE"), e.get("WARM_TTL"))
    assert e["POD_NAMESPACE"], "the warm cache lives in POD_NAMESPACE"
    off = docs(helm("--set", "tailor.warmCache.enabled=false", "--set", "tailor.warmCache.ttl=5m").stdout)
    assert not [d for d in off if d.get("kind") in ("Role", "RoleBinding") and d["metadata"]["name"] == "k8s-tailor-warm-cache"]
    e = env(container(off, "k8s-tailor", "tailor"))
    assert e["WARM_CACHE"]["value"] == "false" and e["WARM_TTL"]["value"] == "5m", (e["WARM_CACHE"], e["WARM_TTL"])
    e = env(container(docs(helm("--set", "tailor.env.WARM_TTL=1h").stdout), "k8s-tailor", "tailor"))
    assert e["WARM_TTL"]["value"] == "1h", e["WARM_TTL"]


@check("engine: DISRUPTIVE_MAX_WAIT from tailor.disruptive.maxWait (default 15m, tailor.env wins)")
def _():
    e = env(container(docs(helm().stdout), "k8s-tailor", "tailor"))
    assert e["DISRUPTIVE_MAX_WAIT"]["value"] == "15m", e.get("DISRUPTIVE_MAX_WAIT")
    e = env(container(docs(helm("--set", "tailor.disruptive.maxWait=0").stdout), "k8s-tailor", "tailor"))
    assert e["DISRUPTIVE_MAX_WAIT"]["value"] == "0", e["DISRUPTIVE_MAX_WAIT"]
    e = env(container(docs(helm("--set", "tailor.env.DISRUPTIVE_MAX_WAIT=1h").stdout), "k8s-tailor", "tailor"))
    assert e["DISRUPTIVE_MAX_WAIT"]["value"] == "1h", e["DISRUPTIVE_MAX_WAIT"]


@check("tailor.rbac.clusterLookups: nodes/limitranges list + CLUSTER_LOOKUPS only when on")
def _():
    def role(args):
        ds = docs(helm(*args).stdout)
        return one(ds, "ClusterRole", "k8s-tailor-engine"), env(container(ds, "k8s-tailor", "tailor"))
    def lookups(r):
        return [x for x in r["rules"] if "nodes" in x["resources"] or "limitranges" in x["resources"]]
    r, e = role([])
    assert lookups(r) == [] and "CLUSTER_LOOKUPS" not in e, (lookups(r), e.get("CLUSTER_LOOKUPS"))
    r, e = role(["--set", "tailor.rbac.clusterLookups=true"])
    (rule,) = lookups(r)
    assert rule["apiGroups"] == [""] and sorted(rule["resources"]) == ["limitranges", "nodes"] and rule["verbs"] == ["list"], rule
    assert e["CLUSTER_LOOKUPS"]["value"] == "true", e.get("CLUSTER_LOOKUPS")


@check("engine ClusterRole/Binding named k8s-tailor-engine (no misleading -readonly name)")
def _():
    ds = docs(helm().stdout)
    assert not [d for d in ds if d["metadata"]["name"] == "k8s-tailor-readonly"]
    one(ds, "ClusterRole", "k8s-tailor-engine")
    crb = one(ds, "ClusterRoleBinding", "k8s-tailor-engine")
    assert crb["roleRef"] == {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "k8s-tailor-engine"}
    assert crb["subjects"] == [{"kind": "ServiceAccount", "name": "k8s-tailor", "namespace": "default"}], crb["subjects"]


@check("apiAuth.protectReads: TAILOR_API_PROTECT_READS only when on; needs apiAuth.enabled")
def _():
    e = env(container(docs(helm().stdout), "k8s-tailor", "tailor"))
    assert "TAILOR_API_PROTECT_READS" not in e
    out = helm("--set", "tailor.apiAuth.protectReads=true").stdout
    kubeconform(out, "protectReads")
    e = env(container(docs(out), "k8s-tailor", "tailor"))
    assert e["TAILOR_API_PROTECT_READS"]["value"] == "true"
    assert e["TAILOR_API_AUTH_REQUIRED"]["value"] == "true" and "secretKeyRef" in e["TAILOR_API_TOKEN"]["valueFrom"]
    # the MCP gets the same token, so its reads keep working
    m = env(container(docs(out), "k8s-tailor-mcp", "mcp"))
    assert m["TAILOR_API_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == e["TAILOR_API_TOKEN"]["valueFrom"]["secretKeyRef"]["name"]
    p = helm("--set", "tailor.apiAuth.protectReads=true", "--set", "tailor.apiAuth.enabled=false", ok=False)
    assert p.returncode != 0 and "protectReads" in p.stderr, p.stderr[-300:]


def _scrape(ds):
    cm = [d for d in find(ds, "ConfigMap") if "scrape.yml" in (d.get("data") or {})]
    assert len(cm) == 1, [d["metadata"]["name"] for d in find(ds, "ConfigMap")]
    return {j["job_name"]: j for j in yaml.safe_load(cm[0]["data"]["scrape.yml"])["scrape_configs"]}


@check("vmsingle scrape: kubelet-resource intact, kubelet-cadvisor keeps only the CFS counters")
def _():
    vals = yaml.safe_load(open(os.path.join(CHART, "values.yaml")))
    jobs = _scrape(docs(helm("-n", "other").stdout))
    assert set(jobs) == {"kubelet-resource", "kubelet-cadvisor", "tailor"}, set(jobs)
    res, cad = jobs["kubelet-resource"], jobs["kubelet-cadvisor"]
    assert res["relabel_configs"][1]["replacement"] == "/api/v1/nodes/${1}/proxy/metrics/resource"
    assert res["metric_relabel_configs"] == [{"source_labels": ["__name__"], "action": "keep",
                                              "regex": "container_(cpu_usage_seconds_total|memory_working_set_bytes)"}]
    for k in ("scheme", "tls_config", "bearer_token_file", "kubernetes_sd_configs"):
        assert cad[k] == res[k], k
    assert cad["relabel_configs"][0] == res["relabel_configs"][0]
    assert cad["relabel_configs"][1]["replacement"] == "/api/v1/nodes/${1}/proxy/metrics/cadvisor"
    keep = cad["metric_relabel_configs"][0]
    assert keep["action"] == "keep" and keep["source_labels"] == ["__name__"]
    names = re.compile("^(?:" + keep["regex"] + ")$")
    for n in ("container_cpu_cfs_throttled_periods_total", "container_cpu_cfs_periods_total"):
        assert names.match(n), n
    for n in ("container_cpu_cfs_throttled_seconds_total", "container_cpu_usage_seconds_total",
              "container_memory_working_set_bytes", "container_network_receive_bytes_total"):
        assert not names.match(n), n
    lk = cad["metric_relabel_configs"][-1]
    assert lk["action"] == "labelkeep" and set(lk["regex"].split("|")) == {"__name__", "job", "instance", "namespace", "pod", "container"}
    # the engine's throttling job name is the one scraped here
    assert "kubelet-cadvisor" in jobs and vals["victoria-metrics-single"]["enabled"] is True


@check("vmsingle scrape: engine job uses pod SD in the release ns; shared gauges folded; minimal Role")
def _():
    ds = docs(helm("-n", "other").stdout)
    t = _scrape(ds)["tailor"]
    assert "static_configs" not in t
    assert t["kubernetes_sd_configs"] == [{"role": "pod", "namespaces": {"own_namespace": True}}]
    rl = {r["source_labels"][0]: r for r in t["relabel_configs"] if r.get("action") == "keep"}
    assert rl["__meta_kubernetes_pod_label_app"]["regex"] == "k8s-tailor"
    assert rl["__meta_kubernetes_pod_container_port_number"]["regex"] == "8080"
    targets = {r["target_label"] for r in t["relabel_configs"] if "target_label" in r}
    assert {"pod", "instance"} <= targets, targets
    # the selector really matches the engine pods and port
    dep = one(ds, "Deployment", "k8s-tailor")
    assert dep["spec"]["template"]["metadata"]["labels"]["app"] == "k8s-tailor"
    assert 8080 in [p["containerPort"] for p in container(ds, "k8s-tailor", "tailor")["ports"]]
    shared = re.compile("^(?:" + t["metric_relabel_configs"][0]["regex"] + ")$")
    for n in ("tailor_recommended_cpu_cores", "tailor_current_mem_bytes", "tailor_waste_usd_week",
              "tailor_recommendations", "tailor_workloads", "tailor_paused"):
        assert shared.match(n), n
    for n in ("tailor_leader", "tailor_resizes_total", "tailor_resize_skipped_total", "tailor_mutations_total",
              "tailor_refresh_duration_seconds_bucket", "tailor_last_refresh_success_timestamp_seconds",
              "tailor_query_errors_total", "tailor_webhook_skipped_total", "tailor_pods_resize_state"):
        assert not shared.match(n), n
    role = one(ds, "Role", "k8s-tailor-vmsingle-pods")
    assert role["metadata"]["namespace"] == "other"
    assert role["rules"] == [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list", "watch"]}]
    rb = one(ds, "RoleBinding", "k8s-tailor-vmsingle-pods")
    assert rb["subjects"] == [{"kind": "ServiceAccount", "name": "k8s-tailor-vmsingle", "namespace": "other"}]
    assert rb["roleRef"]["name"] == "k8s-tailor-vmsingle-pods"
    # the vmsingle SA must not read pods cluster-wide
    vm = one(ds, "ClusterRole", "k8s-tailor-vmsingle")["rules"]
    assert not [r for r in vm if "pods" in r["resources"]], vm
    # NetworkPolicy of the engine admits the vmsingle pods on 8080
    np = one(ds, "NetworkPolicy", "k8s-tailor-engine")
    rule = next(r for r in np["spec"]["ingress"] if {"protocol": "TCP", "port": 8080} in r["ports"])
    assert {"podSelector": {"matchLabels": {"app.kubernetes.io/name": "victoria-metrics-single"}}} in rule["from"]
    # no bundled vmsingle: no Role
    ds = docs(helm("--set", "victoria-metrics-single.enabled=false", "--set", "tailor.env.VM_URL=http://vm:8428").stdout)
    assert not find(ds, "Role", "k8s-tailor-vmsingle-pods")


@check("tailor.presets: bursty TailorProfile (cpu.quantile 0.95 only) as a post-install hook, off by default")
def _():
    assert not find(docs(helm().stdout), "TailorProfile")
    ds = docs(helm("--set", "tailor.presets.enabled=true").stdout)
    p = one(ds, "TailorProfile", "bursty")
    assert p["apiVersion"] == "tailor.k8s-tailor.dev/v1alpha1"
    assert p["spec"] == {"cpu": {"quantile": 0.95}}, p["spec"]
    assert "namespace" not in p["metadata"]  # cluster-scoped CRD
    assert p["metadata"]["annotations"]["helm.sh/hook"] == "post-install,post-upgrade"
    # the spec fields exist in the CRD schema (cpu.quantile within (0, 1])
    crd = one(ds, "CustomResourceDefinition", "tailorprofiles.tailor.k8s-tailor.dev")
    q = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]["cpu"]["properties"]["quantile"]
    assert q["type"] == "number" and q["maximum"] == 1 and q["minimum"] == 0
    assert not find(docs(helm("--set", "tailor.presets.enabled=true", "--set", "tailor.enabled=false").stdout), "TailorProfile")


if FAILS:
    print(f"\n{len(FAILS)} FAILED: {FAILS}")
    sys.exit(1)
print("\nall chart render tests passed")
