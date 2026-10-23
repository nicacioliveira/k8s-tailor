package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nicacioliveira/k8s-tailor/internal/backtest"
	"github.com/nicacioliveira/k8s-tailor/internal/config"
	"github.com/nicacioliveira/k8s-tailor/internal/vm"
)

// RunBacktest is the `k8s-tailor backtest` subcommand: an offline,
// deterministic replay of usage through k8s-tailor, KRR, VPA and the static
// request. args excludes the subcommand name. Returns the process exit code.
// See docs/BACKTEST.md.
func RunBacktest(args []string) int {
	return runBacktest(args, os.Stdout, os.Stderr)
}

func runBacktest(args []string, stdout, stderr io.Writer) int {
	cfg := config.Load()
	fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		synthetic  = fs.String("synthetic", "", "synthetic scenarios: 'all' or comma list of "+strings.Join(backtest.ScenarioNames(), ","))
		input      = fs.String("input", "", "usage file (.csv or .jsonl; see docs/BACKTEST.md for columns)")
		vmURL      = fs.String("vm-url", "", "VictoriaMetrics/Prometheus base URL to read usage from")
		vmNS       = fs.String("vm-namespace", "", "namespace (with -vm-url)")
		vmPods     = fs.String("vm-pod-regex", "", "pod regex, e.g. 'api-.*' (with -vm-url)")
		vmCont     = fs.String("vm-container", "", "container name (with -vm-url)")
		vmWL       = fs.String("vm-workload", "", "workload name for the report (with -vm-url)")
		vmRange    = fs.String("vm-range", "14d", "history to fetch, ending now (with -vm-url)")
		vmJob      = fs.String("vm-job", vmJobDefault(), `scrape job of the kubelet usage series (with -vm-url; default $VM_JOB or "kubelet-resource"; "" = no job filter)`)
		seed       = fs.Uint64("seed", 42, "seed for the synthetic generator")
		days       = fs.Int("days", 14, "synthetic trace length in days")
		stepS      = fs.String("step", "5m", "grid step")
		warmupS    = fs.String("warmup", "2d", "history before strategies may act (not scored)")
		strategies = fs.String("strategies", "all", "comma list: "+strings.Join(backtest.StrategyNames, ","))
		refreshS   = fs.String("engine-refresh", "1h", "how often the real engine Refresh is replayed (production: 60s)")
		krrEveryS  = fs.String("krr-every", "24h", "how often the KRR report is (assumed) re-applied")
		priceCPU   = fs.Float64("price-cpu", cfg.PriceCPUCoreHr, "$ per core-hour (requested)")
		priceGB    = fs.Float64("price-gb", cfg.PriceGBHr, "$ per GiB-hour (requested)")
		cpuReq     = fs.Float64("cpu-request", 0, "initial CPU request (cores) when the input lacks it")
		cpuLim     = fs.Float64("cpu-limit", 0, "initial CPU limit (cores) when the input lacks it")
		memReqMi   = fs.Float64("mem-request-mi", 0, "initial memory request (MiB) when the input lacks it")
		memLimMi   = fs.Float64("mem-limit-mi", 0, "initial memory limit (MiB) when the input lacks it")
		format     = fs.String("format", "md", "stdout format: md | json | none")
		outJSON    = fs.String("out-json", "", "also write the JSON report to this file")
		outMD      = fs.String("out-md", "", "also write the markdown report to this file")
		exportCSV  = fs.String("export-csv", "", "write the (synthetic/loaded) traces as CSV to this file")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "backtest: "+format+"\n", a...)
		return 1
	}
	dur := func(name, s string) (int64, error) {
		d, err := config.ParseDuration(s)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("-%s %q: invalid duration", name, s)
		}
		return int64(d / time.Second), nil
	}
	step, err := dur("step", *stepS)
	if err != nil || step <= 0 {
		return fail("invalid -step %q", *stepS)
	}
	warmup, err := dur("warmup", *warmupS)
	if err != nil {
		return fail("%v", err)
	}
	refresh, err := dur("engine-refresh", *refreshS)
	if err != nil {
		return fail("%v", err)
	}
	krrEvery, err := dur("krr-every", *krrEveryS)
	if err != nil {
		return fail("%v", err)
	}
	defaults := backtest.Resources{CPURequest: *cpuReq, CPULimit: *cpuLim, MemRequest: *memReqMi * 1024 * 1024, MemLimit: *memLimMi * 1024 * 1024}

	sources := 0
	for _, s := range []string{*synthetic, *input, *vmURL} {
		if s != "" {
			sources++
		}
	}
	if sources != 1 {
		return fail("choose exactly one of -synthetic, -input, -vm-url")
	}

	var traces []*backtest.Trace
	var source string
	switch {
	case *synthetic != "":
		names := backtest.ScenarioNames()
		if *synthetic != "all" {
			names = strings.Split(*synthetic, ",")
		}
		for _, n := range names {
			tr, err := backtest.Generate(strings.TrimSpace(n), *seed, *days, step)
			if err != nil {
				return fail("%v", err)
			}
			traces = append(traces, tr)
		}
		source = "synthetic:" + strings.Join(names, ",")
	case *input != "":
		f, err := os.Open(*input)
		if err != nil {
			return fail("%v", err)
		}
		var recs []backtest.Record
		if strings.EqualFold(filepath.Ext(*input), ".jsonl") || strings.EqualFold(filepath.Ext(*input), ".ndjson") {
			recs, err = backtest.ReadJSONL(f)
		} else {
			recs, err = backtest.ReadCSV(f)
		}
		f.Close()
		if err != nil {
			return fail("%v", err)
		}
		if traces, err = backtest.BuildTraces(recs, step, defaults); err != nil {
			return fail("%v", err)
		}
		source = "file:" + filepath.Base(*input)
	default:
		rng, err := dur("vm-range", *vmRange)
		if err != nil {
			return fail("%v", err)
		}
		if *vmNS == "" || *vmPods == "" || *vmCont == "" {
			return fail("-vm-url needs -vm-namespace, -vm-pod-regex and -vm-container")
		}
		wl := *vmWL
		if wl == "" {
			wl = *vmPods
		}
		end := time.Now().Truncate(time.Duration(step) * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		tr, err := backtest.LoadFromVM(ctx, vm.New(strings.TrimRight(*vmURL, "/")), backtest.VMQuery{
			Namespace: *vmNS, Workload: wl, PodRegex: *vmPods, Container: *vmCont, Job: *vmJob,
			Start: end.Add(-time.Duration(rng) * time.Second), End: end, StepSec: step, Initial: defaults,
		})
		cancel()
		if err != nil {
			return fail("%v", err)
		}
		traces = []*backtest.Trace{tr}
		source = "vm:" + *vmURL
	}

	if *exportCSV != "" {
		f, err := os.Create(*exportCSV)
		if err != nil {
			return fail("%v", err)
		}
		err = backtest.WriteCSV(f, traces)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail("%v", err)
		}
	}

	factories, err := backtest.Factories(*strategies,
		backtest.EngineConfig{Base: cfg, RefreshEverySec: refresh},
		func() backtest.KRRConfig { k := backtest.DefaultKRR(); k.EverySec = krrEvery; return k }(),
		backtest.DefaultVPA())
	if err != nil {
		return fail("%v", err)
	}
	opt := backtest.Options{WarmupSec: warmup, PriceCPUCoreHr: *priceCPU, PriceGBHr: *priceGB}
	results, err := backtest.RunAll(traces, factories, opt)
	if err != nil {
		return fail("%v", err)
	}
	rep := &backtest.Report{
		Tool: "k8s-tailor backtest", Seed: *seed, Source: source, Options: opt,
		Params: map[string]any{
			"engine": map[string]any{"window": cfg.Window.String(), "cpuQuantile": cfg.CPUQuant, "memQuantile": cfg.MemQuant,
				"margin": cfg.Margin, "halfLife": cfg.HalfLife.String(), "minSamples": cfg.MinSamples, "refreshEverySec": refresh},
			"krr": func() backtest.KRRConfig { k := backtest.DefaultKRR(); k.EverySec = krrEvery; return k }(),
			"vpa": backtest.DefaultVPA(),
		},
		Results: results,
	}
	writeTo := func(path string, fn func(io.Writer) error) error {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		if err := fn(f); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	if *outJSON != "" {
		if err := writeTo(*outJSON, rep.WriteJSON); err != nil {
			return fail("%v", err)
		}
	}
	if *outMD != "" {
		if err := writeTo(*outMD, rep.WriteMarkdown); err != nil {
			return fail("%v", err)
		}
	}
	switch *format {
	case "md":
		err = rep.WriteMarkdown(stdout)
	case "json":
		err = rep.WriteJSON(stdout)
	case "none":
	default:
		return fail("unknown -format %q", *format)
	}
	if err != nil {
		return fail("%v", err)
	}
	return 0
}

// vmJobDefault mirrors the engine: VM_JOB if set (even to ""), else the
// chart's kubelet scrape job.
func vmJobDefault() string {
	if v, ok := os.LookupEnv("VM_JOB"); ok {
		return v
	}
	return config.DefaultVMJob
}
