package config

import (
	"testing"
	"time"
)

func TestDisruptiveMaxWait(t *testing.T) {
	t.Setenv("DISRUPTIVE_MAX_WAIT", "")
	if got := Load().DisruptiveMaxWait; got != 15*time.Minute {
		t.Fatalf("default = %s, want 15m", got)
	}
	t.Setenv("DISRUPTIVE_MAX_WAIT", "0")
	if got := Load().DisruptiveMaxWait; got != 0 {
		t.Fatalf("0 = %s, want 0 (wait forever)", got)
	}
	if err := (Config{DisruptiveMaxWait: -time.Second, OOMMemCapFactor: 4, OOMGrowthFactor: 1.5, CPULimitGrowth: 1.5, CPUCensorMaxFactor: 8}).Validate(); err == nil {
		t.Fatal("negative DISRUPTIVE_MAX_WAIT accepted")
	}
}

func TestAutoQuantileConfig(t *testing.T) {
	t.Setenv("AUTO_QUANTILE", "")
	t.Setenv("BURST_THRESHOLD", "")
	c := Load()
	if c.AutoQuantile || c.BurstThreshold != 3 {
		t.Fatalf("defaults: %v %v, want false / 3", c.AutoQuantile, c.BurstThreshold)
	}
	t.Setenv("AUTO_QUANTILE", "true")
	t.Setenv("BURST_THRESHOLD", "5")
	if c = Load(); !c.AutoQuantile || c.BurstThreshold != 5 {
		t.Fatalf("env not read: %v %v", c.AutoQuantile, c.BurstThreshold)
	}
	base := Config{OOMMemCapFactor: 4, OOMGrowthFactor: 1.5, CPULimitGrowth: 1.5, CPUCensorMaxFactor: 8, CPUCensorMaxSteps: 3}
	for _, bad := range []float64{1, 0.5, -2} {
		base.BurstThreshold = bad
		if err := base.Validate(); err == nil {
			t.Fatalf("BURST_THRESHOLD=%v accepted", bad)
		}
	}
	base.BurstThreshold = 3
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestSeasonWindowConfig(t *testing.T) {
	t.Setenv("SEASON_WINDOW", "")
	t.Setenv("SEASON_REFRESH", "")
	c := Load()
	if c.SeasonWindow != 14*24*time.Hour || c.SeasonRefresh != time.Hour {
		t.Fatalf("defaults: %s / %s, want 14d / 1h", c.SeasonWindow, c.SeasonRefresh)
	}
	base := Config{OOMMemCapFactor: 4, OOMGrowthFactor: 1.5, CPULimitGrowth: 1.5, CPUCensorMaxFactor: 8, CPUCensorMaxSteps: 3, BurstThreshold: 3}
	for _, ok := range []time.Duration{0, 14 * 24 * time.Hour, MaxSeasonWindow} {
		base.SeasonWindow = ok
		if err := base.Validate(); err != nil {
			t.Errorf("SEASON_WINDOW=%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []time.Duration{-time.Hour, MaxSeasonWindow + time.Hour} {
		base.SeasonWindow = bad
		if err := base.Validate(); err == nil {
			t.Errorf("SEASON_WINDOW=%s accepted: the extra query cost is unbounded", bad)
		}
	}
}
