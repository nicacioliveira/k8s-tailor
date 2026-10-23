package recommender

// Anti-flapping building blocks (see docs/BACKTEST.md, bursty scenario):
//
//   - medianFilter: the change-point detector runs on a running-median-filtered
//     series, so a burst shorter than ~half the filter window cannot look like a
//     level shift (a median filter removes short pulses but preserves step edges);
//   - seasonLagCorr / effectiveSeason: the seasonal forecast is used only when the
//     series is actually periodic (hour-of-day profile correlated with itself one
//     season earlier), not when random bursts happened to land in that hour;
//   - stabilizer: HPA-style scale-down stabilization — the request used for a
//     scale-down is the max of the raw recommendations of the last
//     ScaleDownStabilization window, so a transient dip never shrinks capacity.

import (
	"math"
	"sort"
	"sync"
	"time"
)

// cpFilterHalfWidth is the half-width (in samples) of the running median applied
// before change-point detection: 6 samples at the 5-minute query step = a 65-min
// window, so a pulse of up to 30 min is removed while a sustained shift survives.
const cpFilterHalfWidth = 3600 / stepSec / 2

// seasonMinCorr is the DEFAULT minimum lag-one-season correlation of the hourly
// profile for the seasonal forecast to be trusted (policy seasonGate.minCorrelation
// overrides it). Random bursts give ~0 (±0.2 with one day of pairs); a real
// daily cycle gives >0.8.
const seasonMinCorr = 0.5

// seasonMinPairs: by default at least one full season-lag day of hourly pairs
// (policy seasonGate.minPairs overrides it).
const seasonMinPairs = 24

// medianFilter returns s with each value replaced by the median of the values
// within ±k positions (truncated at the edges). s must be time-ordered.
func medianFilter(s []Sample, k int) []Sample {
	n := len(s)
	if n == 0 || k <= 0 {
		return s
	}
	out := make([]Sample, n)
	buf := make([]float64, 0, 2*k+1)
	for i := range s {
		lo, hi := i-k, i+k
		if lo < 0 {
			lo = 0
		}
		if hi > n-1 {
			hi = n - 1
		}
		buf = buf[:0]
		for j := lo; j <= hi; j++ {
			buf = append(buf, s[j].V)
		}
		sort.Float64s(buf)
		m := len(buf)
		v := buf[m/2]
		if m%2 == 0 {
			v = (buf[m/2-1] + buf[m/2]) / 2
		}
		out[i] = Sample{T: s[i].T, V: v}
	}
	return out
}

// hourlyMeans buckets samples by absolute hour (floor(T/3600)) and returns the
// mean per hour.
func hourlyMeans(s []Sample) map[int64]float64 {
	type acc struct{ sum, n float64 }
	b := map[int64]*acc{}
	for _, x := range s {
		h := int64(math.Floor(x.T / 3600))
		a := b[h]
		if a == nil {
			a = &acc{}
			b[h] = a
		}
		a.sum += x.V
		a.n++
	}
	out := make(map[int64]float64, len(b))
	for h, a := range b {
		out[h] = a.sum / a.n
	}
	return out
}

// seasonLagCorr is the Pearson correlation between the hourly-mean series and
// itself lagHours earlier, over every hour where both exist. ok=false when there
// are fewer than minPairs pairs (0 = seasonMinPairs): not enough history to judge.
func seasonLagCorr(s []Sample, lagHours int64, minPairs int) (float64, bool) {
	if minPairs <= 0 {
		minPairs = seasonMinPairs
	}
	hm := hourlyMeans(s)
	var xs, ys []float64
	for h, y := range hm {
		if x, ok := hm[h-lagHours]; ok {
			xs = append(xs, x)
			ys = append(ys, y)
		}
	}
	if len(xs) < minPairs {
		return 0, false
	}
	n := float64(len(xs))
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= n
	my /= n
	var sxy, sxx, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx <= 0 || syy <= 0 {
		return 0, true // flat: nothing periodic to forecast
	}
	return sxy / math.Sqrt(sxx*syy), true
}

// effectiveSeason is effectiveSeasonGate with the default gate.
func effectiveSeason(samples []Sample, season string) string {
	return effectiveSeasonGate(samples, season, 0, 0)
}

// effectiveSeasonGate narrows the policy's season to the seasonalities the data
// actually shows. Daily needs the 24h-lag correlation >= minCorr. Weekly needs
// the 168h-lag correlation, measured as soon as the window holds minPairs
// week-lag hourly pairs (default 24: a window of at least 8 days; 14 days to
// cover every hour of the week). When the window is too short for a week-lag
// (e.g. the default 7d window), the daily-lag correlation is used as the proxy
// "this series is regular" (the weekly bucket then holds a single week): a
// weekly-only pattern (Monday peak, weekdays without a daily cycle) is then
// rejected. minCorr / minPairs <= 0 = the defaults (seasonMinCorr,
// seasonMinPairs). Returns "off" when no requested seasonality is supported.
func effectiveSeasonGate(samples []Sample, season string, minCorr float64, minPairs int) string {
	if minCorr <= 0 {
		minCorr = seasonMinCorr
	}
	if season == "off" || season == "none" {
		return "off"
	}
	m := meanByTime(samples)
	daily := func() bool {
		c, ok := seasonLagCorr(m, 24, minPairs)
		return ok && c >= minCorr
	}
	weekly := func() bool {
		if c, ok := seasonLagCorr(m, 168, minPairs); ok {
			return c >= minCorr
		}
		return daily()
	}
	switch season {
	case "weekly":
		if weekly() {
			return "weekly"
		}
	case "both":
		d, w := daily(), weekly()
		switch {
		case d && w:
			return "both"
		case d:
			return "daily"
		case w:
			return "weekly"
		}
	default: // "" | "daily"
		if daily() {
			return "daily"
		}
	}
	return "off"
}

// stabPoint is one raw (pre-stabilization) recommendation.
type stabPoint struct {
	t        time.Time
	cpu, mem float64
}

// stabilizer keeps, per container, the raw recommendations of the last
// stabilization window (HPA's "recommendations" history).
type stabilizer struct {
	mu   sync.Mutex
	hist map[string][]stabPoint
}

// floor returns the max raw CPU / memory request recorded for key within
// (now-w, now]. Zero when w <= 0 or nothing recorded.
func (s *stabilizer) floor(key string, now time.Time, w time.Duration) (cpu, mem float64) {
	if w <= 0 {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.hist[key] {
		if now.Sub(p.t) < w && !p.t.After(now) {
			cpu = math.Max(cpu, p.cpu)
			mem = math.Max(mem, p.mem)
		}
	}
	return cpu, mem
}

// has reports whether any history is held for key.
func (s *stabilizer) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.hist[key]
	return ok
}

// record appends a raw recommendation and drops points outside the window.
func (s *stabilizer) record(key string, now time.Time, w time.Duration, cpu, mem float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hist == nil {
		s.hist = map[string][]stabPoint{}
	}
	if w <= 0 {
		delete(s.hist, key)
		return
	}
	kept := s.hist[key][:0]
	for _, p := range s.hist[key] {
		if now.Sub(p.t) < w && !p.t.After(now) {
			kept = append(kept, p)
		}
	}
	s.hist[key] = append(kept, stabPoint{t: now, cpu: cpu, mem: mem})
}

// retain drops the history of every container not in keep (deleted workloads).
func (s *stabilizer) retain(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.hist {
		if !keep[k] {
			delete(s.hist, k)
		}
	}
}
