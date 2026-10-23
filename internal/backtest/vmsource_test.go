package backtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// LoadFromVM pins both usage queries to q.Job (same filter as the engine);
// an empty Job leaves the selector unfiltered.
func TestLoadFromVMPinsJob(t *testing.T) {
	for _, job := range []string{"kubelet-resource", ""} {
		var mu sync.Mutex
		var qs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			qs = append(qs, r.URL.Query().Get("query"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		}))
		end := time.Now()
		_, _ = LoadFromVM(context.Background(), vm.New(srv.URL), VMQuery{Namespace: "a", PodRegex: "p-.*", Container: "c",
			Job: job, Start: end.Add(-time.Hour), End: end, StepSec: 300})
		srv.Close()
		if len(qs) != 2 {
			t.Fatalf("job=%q: %d queries, want 2", job, len(qs))
		}
		for _, q := range qs {
			if job != "" && !strings.Contains(q, `job="kubelet-resource"`) {
				t.Errorf("query without job matcher: %s", q)
			}
			if job == "" && strings.Contains(q, "job=") {
				t.Errorf("empty Job must not filter: %s", q)
			}
		}
	}
}
