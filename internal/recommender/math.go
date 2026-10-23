package recommender

import (
	"math"
	"sort"
)

// Sample is a (timestamp seconds, value) point from a range query.
type Sample struct {
	T float64
	V float64
}

// Quantile returns the linear-interpolated quantile (q in [0,1]) of values.
func Quantile(values []float64, q float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return values[0]
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return s[lo]
	}
	frac := pos - float64(lo)
	return s[lo]*(1-frac) + s[hi]*frac
}

// WeightedQuantile is the VPA-style estimator: each sample is weighted by
// 0.5^(age/halfLife) so recent samples dominate, then we take the value where
// cumulative weight crosses q of the total. now defaults to the newest sample.
func WeightedQuantile(samples []Sample, q, halfLifeSec, now float64) float64 {
	n := len(samples)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return samples[0].V
	}
	if now == 0 {
		for _, s := range samples {
			if s.T > now {
				now = s.T
			}
		}
	}
	type wv struct{ v, w float64 }
	ws := make([]wv, n)
	total := 0.0
	for i, s := range samples {
		w := 1.0
		if halfLifeSec > 0 {
			w = math.Pow(2, -(now-s.T)/halfLifeSec)
		}
		ws[i] = wv{s.V, w}
		total += w
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].v < ws[j].v })
	if total == 0 {
		return ws[n-1].v
	}
	target := q * total
	cum := 0.0
	for _, x := range ws {
		cum += x.w
		if cum >= target {
			return x.v
		}
	}
	return ws[n-1].v
}

// Peak returns the maximum value in samples.
func Peak(samples []Sample) float64 {
	m := 0.0
	for _, s := range samples {
		if s.V > m {
			m = s.V
		}
	}
	return m
}

// linearFit returns the least-squares slope (value per second) and the fitted
// value at the LATEST timestamp. Order-independent. ok=false when there are fewer
// than 2 samples or all share one timestamp.
func linearFit(samples []Sample) (slope, atLast, tLast float64, ok bool) {
	n := len(samples)
	if n < 2 {
		return 0, 0, 0, false
	}
	t0 := samples[0].T
	tLast = samples[0].T
	var sx, sy, sxx, sxy float64
	for _, s := range samples {
		if s.T > tLast {
			tLast = s.T
		}
		x := s.T - t0
		sx += x
		sy += s.V
		sxx += x * x
		sxy += x * s.V
	}
	fn := float64(n)
	denom := fn*sxx - sx*sx
	if denom == 0 {
		return 0, 0, 0, false
	}
	slope = (fn*sxy - sx*sy) / denom
	intercept := (sy - slope*sx) / fn // value at x=0 (t0)
	return slope, intercept + slope*(tLast-t0), tLast, true
}

// LinearSlope returns the least-squares slope (value per second), or ok=false.
func LinearSlope(samples []Sample) (float64, bool) {
	slope, _, _, ok := linearFit(samples)
	return slope, ok
}

// PredictLinear projects horizonSec past the LATEST sample using the fitted
// linear trend (Prometheus predict_linear semantics: start from the regression
// line, not from whichever raw sample happens to be last in the slice).
func PredictLinear(samples []Sample, horizonSec float64) (float64, bool) {
	slope, atLast, _, ok := linearFit(samples)
	if !ok {
		return 0, false
	}
	return atLast + slope*horizonSec, true
}

// Trend gate (TrendForecast): a linear projection is only published when the
// fit means something. Below trendMinPoints distinct timestamps (2 h at the
// 300 s step) or with R² < trendMinR2 the "slope" is noise, and multiplying
// noise by a 24 h horizon is how a 44 MB proxy read as 829 MB.
const (
	trendMinPoints = 24
	trendMinR2     = 0.5
)

// TrendForecast is PredictLinear with validation, for a TIME-ORDERED series
// with one sample per timestamp (meanByTime): at least trendMinPoints samples,
// a coefficient of determination R² >= trendMinR2, and the horizon capped at
// the span of the data (never extrapolate further ahead than the history the
// slope was fitted on). The result is floored at 0. ok=false = no trend.
func TrendForecast(samples []Sample, horizonSec float64) (float64, bool) {
	n := len(samples)
	if n < trendMinPoints || horizonSec <= 0 {
		return 0, false
	}
	slope, atLast, tLast, ok := linearFit(samples)
	if !ok {
		return 0, false
	}
	tFirst := tLast
	var mean float64
	for _, s := range samples {
		if s.T < tFirst {
			tFirst = s.T
		}
		mean += s.V
	}
	mean /= float64(n)
	// R² = 1 - SSres/SStot, residuals against the fitted line through atLast.
	var ssRes, ssTot float64
	for _, s := range samples {
		fit := atLast + slope*(s.T-tLast)
		ssRes += (s.V - fit) * (s.V - fit)
		ssTot += (s.V - mean) * (s.V - mean)
	}
	if ssTot <= 0 || 1-ssRes/ssTot < trendMinR2 {
		return 0, false // flat (or noise around a level): no trend to project
	}
	if span := tLast - tFirst; horizonSec > span {
		horizonSec = span
	}
	return math.Max(0, atLast+slope*horizonSec), true
}

// phaseDist is the circular distance between two phases within a season.
func phaseDist(a, b, season float64) float64 {
	d := math.Abs(a - b)
	if d > season/2 {
		d = season - d
	}
	return d
}

// seasonForecast dispatches to the daily and/or weekly SeasonalForecast per the
// policy's season setting, returning the higher when "both" so we never
// under-provision ahead of a recurring ramp. Hour-of-day / hour-of-week buckets.
func seasonForecast(samples []Sample, q float64, season string, targetT float64) float64 {
	const day = 24 * 3600.0
	const week = 7 * day
	const hr = 3600.0
	switch season {
	case "off", "none":
		return 0
	case "weekly":
		return SeasonalForecast(samples, q, week, hr, targetT)
	case "both":
		d := SeasonalForecast(samples, q, day, hr, targetT)
		w := SeasonalForecast(samples, q, week, hr, targetT)
		if w > d {
			return w
		}
		return d
	default: // "" or "daily"
		return SeasonalForecast(samples, q, day, hr, targetT)
	}
}

// SeasonalForecast returns the q-quantile of usage observed in the same
// time-of-season bucket as targetT — capturing daily/weekly seasonality so the
// recommendation can anticipate a recurring peak instead of lagging it.
// seasonSec = season length (e.g. 86400 daily), bucketSec = bucket width.
// Returns 0 when there isn't enough history in the matching bucket.
func SeasonalForecast(samples []Sample, q, seasonSec, bucketSec, targetT float64) float64 {
	if len(samples) < 8 || seasonSec <= 0 || bucketSec <= 0 {
		return 0
	}
	tb := math.Mod(targetT, seasonSec)
	bucket := make([]float64, 0, len(samples))
	for _, s := range samples {
		if phaseDist(math.Mod(s.T, seasonSec), tb, seasonSec) <= bucketSec/2 {
			bucket = append(bucket, s.V)
		}
	}
	if len(bucket) < 4 {
		return 0
	}
	return Quantile(bucket, q)
}

// RoundCPUCores rounds up to the next millicore (min 1m).
func RoundCPUCores(cores float64) float64 {
	milli := math.Ceil(cores * 1000)
	if milli < 1 {
		milli = 1
	}
	return milli / 1000
}

// RoundMemBytes rounds up to the next MiB (min 1MiB).
func RoundMemBytes(bytes float64) float64 {
	const mib = 1024 * 1024
	m := math.Ceil(bytes / mib)
	if m < 1 {
		m = 1
	}
	return m * mib
}
