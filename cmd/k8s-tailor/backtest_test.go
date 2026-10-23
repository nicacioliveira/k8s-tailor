package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunBacktestSyntheticJSON(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "trace.csv")
	var out, errb bytes.Buffer
	code := runBacktest([]string{"-synthetic", "memory-leak", "-days", "3", "-warmup", "1d",
		"-engine-refresh", "2h", "-format", "json", "-export-csv", csvPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var rep struct {
		Seed    uint64 `json:"seed"`
		Results []struct {
			Metrics []struct {
				Strategy     string  `json:"strategy"`
				CostRelative float64 `json:"costRelative"`
			} `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if rep.Seed != 42 || len(rep.Results) != 1 || len(rep.Results[0].Metrics) != 4 {
		t.Fatalf("unexpected report shape: %+v", rep)
	}
	// The exported CSV feeds straight back in and reproduces the same numbers.
	var out2 bytes.Buffer
	code = runBacktest([]string{"-input", csvPath, "-warmup", "1d", "-engine-refresh", "2h", "-format", "json"}, &out2, &errb)
	if code != 0 {
		t.Fatalf("re-run from CSV exit %d: %s", code, errb.String())
	}
	var rep2 struct {
		Results []struct {
			Metrics json.RawMessage `json:"metrics"`
		} `json:"results"`
	}
	var rep1 struct {
		Results []struct {
			Metrics json.RawMessage `json:"metrics"`
		} `json:"results"`
	}
	_ = json.Unmarshal(out.Bytes(), &rep1)
	_ = json.Unmarshal(out2.Bytes(), &rep2)
	if !bytes.Equal(rep1.Results[0].Metrics, rep2.Results[0].Metrics) {
		t.Fatal("synthetic run and its CSV export disagree")
	}
	if _, err := os.Stat(csvPath); err != nil {
		t.Fatal(err)
	}
}

func TestRunBacktestFlagErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runBacktest([]string{"-synthetic", "all", "-input", "x.csv"}, &out, &errb); code == 0 {
		t.Fatal("two sources must fail")
	}
	if !strings.Contains(errb.String(), "exactly one") {
		t.Fatalf("stderr: %s", errb.String())
	}
	if code := runBacktest([]string{"-synthetic", "nope"}, &out, &errb); code == 0 {
		t.Fatal("unknown scenario must fail")
	}
	if code := runBacktest([]string{"-bogus"}, &out, &errb); code != 2 {
		t.Fatal("unknown flag must exit 2")
	}
}
