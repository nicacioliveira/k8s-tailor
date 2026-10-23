package backtest

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Record is one usage observation of one pod's container. CSV columns / JSONL
// keys share these names:
//
//	timestamp,namespace,workload,pod,container,cpu_cores,mem_bytes[,cpu_request,cpu_limit,mem_request,mem_limit]
//
// timestamp is unix seconds (int or float) or RFC3339. The optional request /
// limit columns (cores / bytes) give the workload's resources BEFORE any
// strategy acts; the first non-empty value per workload wins.
type Record struct {
	Timestamp  int64
	Namespace  string
	Workload   string
	Pod        string
	Container  string
	CPU        float64
	Mem        float64
	CPURequest float64
	CPULimit   float64
	MemRequest float64
	MemLimit   float64
}

var csvColumns = []string{"timestamp", "namespace", "workload", "pod", "container", "cpu_cores", "mem_bytes", "cpu_request", "cpu_limit", "mem_request", "mem_limit"}

func parseTimestamp(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(math.Floor(f)), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("timestamp %q: want unix seconds or RFC3339", s)
	}
	return t.Unix(), nil
}

func parseOptFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// ReadCSV parses usage records from CSV with a header row (column order free;
// the first seven columns of csvColumns are required).
func ReadCSV(r io.Reader) ([]Record, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("csv header: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, c := range csvColumns[:7] {
		if _, ok := idx[c]; !ok {
			return nil, fmt.Errorf("csv: missing required column %q", c)
		}
	}
	get := func(row []string, col string) string {
		i, ok := idx[col]
		if !ok || i >= len(row) {
			return ""
		}
		return row[i]
	}
	var out []Record
	line := 1
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("csv line %d: %w", line, err)
		}
		var rec Record
		if rec.Timestamp, err = parseTimestamp(get(row, "timestamp")); err != nil {
			return nil, fmt.Errorf("csv line %d: %w", line, err)
		}
		rec.Namespace, rec.Workload = get(row, "namespace"), get(row, "workload")
		rec.Pod, rec.Container = get(row, "pod"), get(row, "container")
		fields := []struct {
			col string
			dst *float64
		}{
			{"cpu_cores", &rec.CPU}, {"mem_bytes", &rec.Mem},
			{"cpu_request", &rec.CPURequest}, {"cpu_limit", &rec.CPULimit},
			{"mem_request", &rec.MemRequest}, {"mem_limit", &rec.MemLimit},
		}
		for _, f := range fields {
			v, err := parseOptFloat(get(row, f.col))
			if err != nil {
				return nil, fmt.Errorf("csv line %d column %s: %w", line, f.col, err)
			}
			*f.dst = v
		}
		out = append(out, rec)
	}
	return out, nil
}

// ReadJSONL parses one JSON object per line with the same keys as the CSV.
func ReadJSONL(r io.Reader) ([]Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var out []Record
	line := 0
	for sc.Scan() {
		line++
		b := strings.TrimSpace(sc.Text())
		if b == "" {
			continue
		}
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("jsonl line %d: %w", line, err)
		}
		str := func(k string) string {
			switch v := m[k].(type) {
			case string:
				return v
			case json.Number:
				return v.String()
			}
			return ""
		}
		var rec Record
		var err error
		if rec.Timestamp, err = parseTimestamp(str("timestamp")); err != nil {
			return nil, fmt.Errorf("jsonl line %d: %w", line, err)
		}
		rec.Namespace, rec.Workload, rec.Pod, rec.Container = str("namespace"), str("workload"), str("pod"), str("container")
		for k, dst := range map[string]*float64{
			"cpu_cores": &rec.CPU, "mem_bytes": &rec.Mem,
			"cpu_request": &rec.CPURequest, "cpu_limit": &rec.CPULimit,
			"mem_request": &rec.MemRequest, "mem_limit": &rec.MemLimit,
		} {
			if *dst, err = parseOptFloat(str(k)); err != nil {
				return nil, fmt.Errorf("jsonl line %d key %s: %w", line, k, err)
			}
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// BuildTraces groups records into one Trace per namespace/workload/container on
// a grid of stepSec. Samples are snapped to the nearest grid point; two samples
// of the same pod in one bucket keep the MAX (peak-preserving). Missing points
// stay NaN. defaults fills initial resources the input didn't carry.
func BuildTraces(recs []Record, stepSec int64, defaults Resources) ([]*Trace, error) {
	if stepSec <= 0 {
		return nil, fmt.Errorf("step must be > 0")
	}
	type key struct{ ns, wl, c string }
	groups := map[key][]Record{}
	for _, r := range recs {
		if r.Namespace == "" || r.Workload == "" || r.Pod == "" || r.Container == "" {
			return nil, fmt.Errorf("record at %d missing namespace/workload/pod/container", r.Timestamp)
		}
		k := key{r.Namespace, r.Workload, r.Container}
		groups[k] = append(groups[k], r)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.ns != b.ns {
			return a.ns < b.ns
		}
		if a.wl != b.wl {
			return a.wl < b.wl
		}
		return a.c < b.c
	})
	var out []*Trace
	for _, k := range keys {
		rs := groups[k]
		minT, maxT := rs[0].Timestamp, rs[0].Timestamp
		for _, r := range rs {
			minT = min(minT, r.Timestamp)
			maxT = max(maxT, r.Timestamp)
		}
		start := minT - ((minT%stepSec)+stepSec)%stepSec
		n := int((maxT-start+stepSec/2)/stepSec) + 1
		pods := map[string]*PodTrace{}
		tr := &Trace{Namespace: k.ns, Workload: k.wl, Container: k.c, Start: start, StepSec: stepSec}
		for _, r := range rs {
			p := pods[r.Pod]
			if p == nil {
				p = &PodTrace{Name: r.Pod, CPU: nanSlice(n), Mem: nanSlice(n)}
				pods[r.Pod] = p
			}
			i := int((r.Timestamp - start + stepSec/2) / stepSec)
			if i >= n {
				i = n - 1
			}
			if math.IsNaN(p.CPU[i]) || r.CPU > p.CPU[i] {
				p.CPU[i] = r.CPU
			}
			if math.IsNaN(p.Mem[i]) || r.Mem > p.Mem[i] {
				p.Mem[i] = r.Mem
			}
			setOnce := func(dst *float64, v float64) {
				if *dst == 0 && v > 0 {
					*dst = v
				}
			}
			setOnce(&tr.Initial.CPURequest, r.CPURequest)
			setOnce(&tr.Initial.CPULimit, r.CPULimit)
			setOnce(&tr.Initial.MemRequest, r.MemRequest)
			setOnce(&tr.Initial.MemLimit, r.MemLimit)
		}
		names := make([]string, 0, len(pods))
		for name := range pods {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			tr.Pods = append(tr.Pods, *pods[name])
		}
		fillDefaults(&tr.Initial, defaults)
		out = append(out, tr)
	}
	return out, nil
}

func fillDefaults(r *Resources, d Resources) {
	if r.CPURequest == 0 {
		r.CPURequest = d.CPURequest
	}
	if r.CPULimit == 0 {
		r.CPULimit = d.CPULimit
	}
	if r.MemRequest == 0 {
		r.MemRequest = d.MemRequest
	}
	if r.MemLimit == 0 {
		r.MemLimit = d.MemLimit
	}
}

// WriteCSV writes traces in the ReadCSV format (NaN points are skipped), so a
// synthetic scenario can be exported, inspected, edited and fed back.
func WriteCSV(w io.Writer, traces []*Trace) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvColumns); err != nil {
		return err
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	for _, tr := range traces {
		for i := 0; i < tr.Len(); i++ {
			for _, p := range tr.Pods {
				if math.IsNaN(p.CPU[i]) || math.IsNaN(p.Mem[i]) {
					continue
				}
				row := []string{
					strconv.FormatInt(tr.TimeAt(i), 10), tr.Namespace, tr.Workload, p.Name, tr.Container,
					f(p.CPU[i]), f(p.Mem[i]), "", "", "", "",
				}
				if i == 0 {
					row[7], row[8], row[9], row[10] = f(tr.Initial.CPURequest), f(tr.Initial.CPULimit), f(tr.Initial.MemRequest), f(tr.Initial.MemLimit)
				}
				if err := cw.Write(row); err != nil {
					return err
				}
			}
		}
	}
	cw.Flush()
	return cw.Error()
}
