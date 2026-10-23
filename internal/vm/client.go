// Package vm is a minimal client for the VictoriaMetrics single-node MetricsQL
// HTTP API (Prometheus /api/v1/query[_range] compatible).
package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
)

// Point is a (timestamp seconds, value) sample. Kept local to avoid an import
// cycle with the recommender package (which imports vm).
type Point struct {
	T float64
	V float64
}

type Client struct {
	base    string
	http    *http.Client
	maxBody int64 // response body cap (bytes)
}

// defaultMaxBody caps one query response. A whole-namespace range query over
// the recommendation window is the largest we issue; 256MiB is far above it and
// still bounds memory if a query (or a misbehaving backend) runs away.
const defaultMaxBody = 256 << 20

func New(base string) *Client {
	return &Client{base: base, http: &http.Client{Timeout: 30 * time.Second}, maxBody: defaultMaxBody}
}

// capReader fails (instead of silently truncating) once more than n bytes are read.
type capReader struct {
	r io.Reader
	n int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		var one [1]byte
		if k, _ := c.r.Read(one[:]); k > 0 {
			return 0, errTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	k, err := c.r.Read(p)
	c.n -= int64(k)
	return k, err
}

var errTooLarge = errors.New("vmsingle response exceeds the size limit")

// finite reports whether v is a usable sample (not NaN / ±Inf).
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Series is one returned time series: its labels plus parsed samples.
type Series struct {
	Metric  map[string]string
	Samples []Point
}

type apiResp struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`  // instant
			Values [][]any           `json:"values"` // range
		} `json:"result"`
	} `json:"data"`
}

func (c *Client) get(ctx context.Context, path string, params url.Values) (out *apiResp, err error) {
	defer func() {
		if err != nil {
			metrics.QueryErrors.Inc("vm")
		}
	}()
	u := c.base + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vmsingle %s -> %d", path, res.StatusCode)
	}
	var resp apiResp
	max := c.maxBody
	if max <= 0 {
		max = defaultMaxBody
	}
	if err := json.NewDecoder(&capReader{r: res.Body, n: max}).Decode(&resp); err != nil {
		if errors.Is(err, errTooLarge) {
			return nil, fmt.Errorf("%w (%d bytes)", errTooLarge, max)
		}
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("vmsingle %s error: %s", path, resp.Error)
	}
	return &resp, nil
}

// QueryRange returns each matched series with its parsed samples.
func (c *Client) QueryRange(ctx context.Context, expr string, start, end, step time.Time, stepSec int) ([]Series, error) {
	p := url.Values{}
	p.Set("query", expr)
	p.Set("start", strconv.FormatInt(start.Unix(), 10))
	p.Set("end", strconv.FormatInt(end.Unix(), 10))
	p.Set("step", strconv.Itoa(stepSec))
	resp, err := c.get(ctx, "/api/v1/query_range", p)
	if err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		s := Series{Metric: r.Metric}
		for _, pair := range r.Values {
			if len(pair) != 2 {
				continue
			}
			ts, _ := pair[0].(float64)
			vs, _ := pair[1].(string)
			v, err := strconv.ParseFloat(vs, 64)
			if err != nil || !finite(v) {
				continue // NaN/±Inf poison every quantile/max downstream
			}
			s.Samples = append(s.Samples, Point{T: ts, V: v})
		}
		out = append(out, s)
	}
	return out, nil
}

// QueryScalar runs an instant query and returns the first series' value.
func (c *Client) QueryScalar(ctx context.Context, expr string) (float64, bool) {
	p := url.Values{}
	p.Set("query", expr)
	resp, err := c.get(ctx, "/api/v1/query", p)
	if err != nil || len(resp.Data.Result) == 0 {
		return 0, false
	}
	v := resp.Data.Result[0].Value
	if len(v) != 2 {
		return 0, false
	}
	s, _ := v[1].(string)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || !finite(f) {
		return 0, false
	}
	return f, true
}

func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return false
	}
	res, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// WithJob adds job="<job>" to a label selector ("{a=\"b\"}"); job == "" leaves
// it unchanged. Usage queries are pinned to the kubelet scrape job so a series
// with the same metric name exported by any other job can't feed the
// recommendation (see config.VMJob).
func WithJob(sel, job string) string {
	if job == "" || !strings.HasSuffix(sel, "}") {
		return sel
	}
	inner := strings.TrimSuffix(sel, "}")
	if inner != "{" {
		inner += ","
	}
	return inner + fmt.Sprintf("job=%q}", job)
}
