package recommender

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/nicacioliveira/k8s-tailor/internal/k8s"
)

// The ValidatingAdmissionPolicy (chart/templates/admissionpolicy.yaml) lets the
// engine's ServiceAccount touch EXACTLY the mesh proxy-sizing annotations this
// package writes. If they drift, either the VAP blocks a legit rollout
// (key added here) or it allows a key the engine never writes (stale entry —
// possibly one that swaps the proxy image). Compared against the template
// source always, and against `helm template` output when helm is installed.
func TestVAPMeshKeysMatchProxyAnnotationKeys(t *testing.T) {
	var want []string
	for _, mesh := range []string{k8s.MeshIstio, k8s.MeshLinkerd} {
		ks := ProxyAnnotationKeys(mesh)
		if len(ks) != 4 {
			t.Fatalf("mesh %s: %d keys, want 4", mesh, len(ks))
		}
		want = append(want, ks...)
	}
	sort.Strings(want)

	_, file, _, _ := runtime.Caller(0)
	chart := filepath.Join(filepath.Dir(file), "..", "..", "chart")
	src, err := os.ReadFile(filepath.Join(chart, "templates", "admissionpolicy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\$meshKeys := list ((?:"[^"]+"\s*)+)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("$meshKeys list not found in admissionpolicy.yaml")
	}
	var tpl []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(m[1], -1) {
		tpl = append(tpl, string(q[1]))
	}
	sort.Strings(tpl)
	if strings.Join(tpl, ",") != strings.Join(want, ",") {
		t.Fatalf("VAP template meshKeys = %v\nProxyAnnotationKeys     = %v", tpl, want)
	}

	if _, err := exec.LookPath("helm"); err != nil {
		t.Log("helm not installed: rendered VAP not checked (chart/tests/render_test.py covers it)")
		return
	}
	// The subchart tarball is gitignored: only render when it is really there
	// (an empty or half-built charts/ directory must not fail the suite).
	if tgz, _ := filepath.Glob(filepath.Join(chart, "charts", "*.tgz")); len(tgz) == 0 {
		t.Log("chart dependencies not built (run `helm dependency build chart`): rendered VAP not checked")
		return
	}
	out, err := exec.Command("helm", "template", "t", chart, "--set", "tailor.admissionPolicy.enabled=true").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	rm := regexp.MustCompile(`- name: meshKeys\s+expression: "\[([^\]]*)\]"`).FindAllSubmatch(out, -1)
	if len(rm) == 0 {
		t.Fatal("meshKeys variable not found in the rendered VAP")
	}
	for _, r := range rm {
		var got []string
		for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(r[1], -1) {
			got = append(got, string(q[1]))
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("rendered VAP meshKeys = %v, want %v", got, want)
		}
	}
}
