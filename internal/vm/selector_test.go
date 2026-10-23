package vm

import "testing"

func TestWithJob(t *testing.T) {
	cases := []struct{ sel, job, want string }{
		{`{namespace="a",container!=""}`, "kubelet-resource", `{namespace="a",container!="",job="kubelet-resource"}`},
		{`{namespace="a"}`, "", `{namespace="a"}`},
		{`{}`, "j", `{job="j"}`},
		{`{namespace="a"}`, `we"ird`, `{namespace="a",job="we\"ird"}`},
	}
	for _, c := range cases {
		if got := WithJob(c.sel, c.job); got != c.want {
			t.Errorf("WithJob(%s, %q) = %s, want %s", c.sel, c.job, got, c.want)
		}
	}
}
