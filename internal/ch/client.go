// Package ch is a tiny HTTP client for a single-node ClickHouse — the durable
// store for applied decisions (and, later, extra telemetry + ML features). We
// talk the HTTP interface (port 8123) with JSONEachRow rather than pulling the
// native driver: decisions are low-volume events, and this keeps the binary's
// dependency surface minimal and matches the rest of the codebase's HTTP style.
//
// Tables live under one database (default "tailor"), one table per concern, so
// analytics, retention (TTL) and future ML feature tables stay independent.
package ch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
)

type Client struct {
	base string // http://host:8123
	db   string
	user string
	pass string
	http *http.Client
}

// New returns a client, or nil when base is empty (feature disabled → callers
// fall back to the in-memory ring buffer).
func New(base, db, user, pass string) *Client {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return nil
	}
	if db == "" {
		db = "tailor"
	}
	return &Client{
		base: base, db: db, user: user, pass: pass,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// chDateTime formats a time as ClickHouse expects for DateTime64(3) in
// JSONEachRow without best_effort parsing: 'YYYY-MM-DD HH:MM:SS.fff' in UTC.
func chDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000")
}

// exec runs a statement/query. When params is non-nil, values are sent as
// param_<name> so the SQL can reference {name:Type} placeholders (safe binding,
// no string interpolation of user input). body is an optional request payload
// (used by INSERT ... FORMAT JSONEachRow). Returns the raw response body.
func (c *Client) exec(ctx context.Context, sql string, params map[string]string, body []byte) (out []byte, err error) {
	defer func() {
		if err != nil {
			metrics.QueryErrors.Inc("clickhouse")
		}
	}()
	q := url.Values{}
	q.Set("query", sql)
	// Session context stays on `default`; every object is fully qualified as
	// c.db.<table>, so CREATE DATABASE works before the db exists.
	q.Set("database", "default")
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/?"+q.Encode(), rdr)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.Header.Set("X-ClickHouse-User", c.user)
		req.Header.Set("X-ClickHouse-Key", c.pass)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("clickhouse %d: %s", res.StatusCode, strings.TrimSpace(buf.String()))
	}
	return buf.Bytes(), nil
}

// EnsureSchema creates the database and tables if absent (idempotent, run on
// startup). decisions is a MergeTree ordered for the two dominant access
// patterns — filter by workload, scan by time — partitioned monthly and TTL'd
// so a single node never grows unbounded.
func (c *Client) EnsureSchema(ctx context.Context, retentionDays int) error {
	if c == nil {
		return nil
	}
	if retentionDays <= 0 {
		retentionDays = 90
	}
	stmts := []string{
		fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, c.db),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.decisions (
			ts        DateTime64(3, 'UTC'),
			namespace LowCardinality(String),
			kind      LowCardinality(String),
			workload  String,
			container String,
			mode      LowCardinality(String),
			direction LowCardinality(String),
			from_cpu  Float64,
			to_cpu    Float64,
			from_mem  Float64,
			to_mem    Float64,
			msg       String
		) ENGINE = MergeTree
		PARTITION BY toYYYYMM(ts)
		ORDER BY (namespace, workload, container, ts)
		TTL toDateTime(ts) + INTERVAL %d DAY
		SETTINGS index_granularity = 8192`, c.db, retentionDays),
	}
	for _, s := range stmts {
		if _, err := c.exec(ctx, s, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// Push implements actuation.Sink: batch-inserts decision records as JSONEachRow.
// Keys in each record must match the decisions columns (see actuation.record).
func (c *Client) Push(ctx context.Context, records []map[string]any) error {
	if c == nil || len(records) == 0 {
		return nil
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	_, err := c.exec(ctx, fmt.Sprintf("INSERT INTO %s.decisions FORMAT JSONEachRow", c.db), nil, body.Bytes())
	return err
}

// DecisionStats returns per-workload-container actuation counts + last-applied
// time from the durable log — so the UI's "applied N ago" survives engine
// restarts (the in-memory ring buffer does not). Keyed ns/kind/workload/container.
func (c *Client) DecisionStats(ctx context.Context) (map[string]struct {
	Count int   `json:"count"`
	Last  int64 `json:"last"`
}, error) {
	out := map[string]struct {
		Count int   `json:"count"`
		Last  int64 `json:"last"`
	}{}
	if c == nil {
		return out, nil
	}
	sql := fmt.Sprintf(`SELECT concat(namespace,'/',kind,'/',workload,'/',container) AS k,
		toUInt32(count()) AS count, toUnixTimestamp(max(ts)) AS last
		FROM %s.decisions GROUP BY k FORMAT JSONEachRow`, c.db)
	raw, err := c.exec(ctx, sql, nil, nil)
	if err != nil {
		return out, err
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row struct {
			K     string `json:"k"`
			Count int    `json:"count"`
			Last  any    `json:"last"`
		}
		if json.Unmarshal(line, &row) != nil || row.K == "" {
			continue
		}
		var last int64
		switch v := row.Last.(type) {
		case float64:
			last = int64(v)
		case string:
			last, _ = strconv.ParseInt(v, 10, 64)
		}
		out[row.K] = struct {
			Count int   `json:"count"`
			Last  int64 `json:"last"`
		}{Count: row.Count, Last: last}
	}
	return out, sc.Err()
}

// Decisions reads the decision feed for [start,end], newest first, optionally
// filtered by a free-text term matched against workload/namespace/msg. Returns
// rows as parsed JSON objects (time is unix seconds under key "time").
func (c *Client) Decisions(ctx context.Context, start, end time.Time, term string, limit int) ([]map[string]any, error) {
	if c == nil {
		return nil, nil
	}
	if end.IsZero() {
		end = time.Now()
	}
	if start.IsZero() {
		start = end.Add(-30 * 24 * time.Hour)
	}
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	params := map[string]string{
		"start": chDateTime(start),
		"end":   chDateTime(end),
		"lim":   strconv.Itoa(limit),
	}
	filter := ""
	if term = strings.TrimSpace(term); term != "" {
		params["q"] = "%" + term + "%"
		filter = "AND (workload ILIKE {q:String} OR namespace ILIKE {q:String} OR msg ILIKE {q:String})"
	}
	sql := fmt.Sprintf(`SELECT
			toUnixTimestamp(ts) AS time,
			mode, namespace, kind, workload, container,
			from_cpu AS fromCpu, to_cpu AS toCpu, from_mem AS fromMem, to_mem AS toMem
		FROM %s.decisions
		WHERE ts >= {start:DateTime64(3)} AND ts <= {end:DateTime64(3)} %s
		ORDER BY ts DESC
		LIMIT {lim:UInt32}
		FORMAT JSONEachRow`, c.db, filter)
	raw, err := c.exec(ctx, sql, params, nil)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}
