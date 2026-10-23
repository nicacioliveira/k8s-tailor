package vm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

// P2: NaN / ±Inf samples (a division by zero in a rate, a broken exporter) must
// be dropped at ingestion — one NaN poisons every quantile/max downstream.
func TestQueryRangeDropsNonFinite(t *testing.T) {
	c := serve(t, `{"status":"success","data":{"result":[{"metric":{"pod":"p"},"values":[[1,"1.5"],[2,"NaN"],[3,"+Inf"],[4,"-Inf"],[5,"2"]]}]}}`)
	got, err := c.QueryRange(context.Background(), "x", time.Unix(0, 0), time.Unix(10, 0), time.Time{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Samples) != 2 || got[0].Samples[0].V != 1.5 || got[0].Samples[1].V != 2 {
		t.Fatalf("samples = %+v, want only the finite 1.5 and 2", got)
	}
}

func TestQueryScalarRejectsNonFinite(t *testing.T) {
	c := serve(t, `{"status":"success","data":{"result":[{"metric":{},"value":[1,"NaN"]}]}}`)
	if _, ok := c.QueryScalar(context.Background(), "x"); ok {
		t.Fatal("NaN scalar accepted")
	}
}

// P1-11: the response body is bounded — a runaway query must fail cleanly, not
// buffer an unbounded body into memory.
func TestResponseBodyIsBounded(t *testing.T) {
	big := `{"status":"success","data":{"result":[{"metric":{"pod":"` + strings.Repeat("a", 2048) + `"},"values":[]}]}}`
	c := serve(t, big)
	c.maxBody = 1024
	if _, err := c.QueryRange(context.Background(), "x", time.Unix(0, 0), time.Unix(10, 0), time.Time{}, 1); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a size-limit error", err)
	}
}
