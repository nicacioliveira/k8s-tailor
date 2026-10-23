package policy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/nicacioliveira/k8s-tailor/internal/config"
)

// ErrInvalid marks a user-input validation failure (the API answers 400).
var ErrInvalid = errors.New("invalid policy")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Bounds shared with the CRD OpenAPI schema (chart/templates/crds). Keep in sync.
const (
	maxPriority    = 10000
	maxMargin      = 10.0
	maxLimitFactor = 100.0
)

var (
	modes       = []string{"", "Off", "Recommend", "Webhook", "InPlace", "Both"}
	applies     = []string{"", "InPlace", "Rollout", "InPlacePreferred"}
	directions  = []string{"", "Both", "Down", "Up"}
	guaranteeds = []string{"", "skip", "resizeBoth"}
	seasons     = []string{"", "daily", "weekly", "both", "off"}
	weekdays    = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	classes     = []string{ClassApp, ClassInjectedSidecar, ClassNativeSidecar}
	hhmm        = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

func oneOf(field, v string, allowed []string) error {
	if containsStr(allowed, v) {
		return nil
	}
	return invalid("%s %q not in %s", field, v, strings.Join(allowed[1:], "|"))
}

// ValidatePolicyView checks UI/MCP input before it reaches the API server, with
// the same bounds the CRD schema enforces (the CRD is the backstop for kubectl/GitOps).
func ValidatePolicyView(v PolicyView) error {
	if errs := validation.IsDNS1123Subdomain(v.Name); len(errs) > 0 {
		return invalid("name %q: %s", v.Name, strings.Join(errs, "; "))
	}
	if v.Priority < -maxPriority || v.Priority > maxPriority {
		return invalid("priority %d outside [-%d, %d]", v.Priority, maxPriority, maxPriority)
	}
	for _, c := range []error{
		oneOf("mode", v.Mode, modes), oneOf("apply", v.Apply, applies),
		oneOf("direction", v.Direction, directions), oneOf("season", v.Season, seasons),
	} {
		if c != nil {
			return c
		}
	}
	if err := validateRes("cpu", resSpec{Margin: v.CPUMargin, Min: v.CPUMin, Max: v.CPUMax, LimitFactor: v.CPULimitFactor}); err != nil {
		return err
	}
	if err := validateRes("memory", resSpec{Margin: v.MemMargin, Min: v.MemMin, Max: v.MemMax, LimitFactor: v.MemLimitFactor}); err != nil {
		return err
	}
	if v.HalfLife != "" {
		if d, err := config.ParseDuration(v.HalfLife); err != nil || d <= 0 {
			return invalid("halfLife %q is not a positive duration (e.g. 24h, 3d)", v.HalfLife)
		}
	}
	if err := validateSchedule(v.Schedule); err != nil {
		return err
	}
	for i, r := range v.Containers {
		if err := validateRule(r); err != nil {
			return fmt.Errorf("containers[%d]: %w", i, err)
		}
	}
	return nil
}

func validateRes(field string, r resSpec) error {
	if r.Quantile < 0 || r.Quantile > 1 {
		return invalid("%s.quantile %g outside (0, 1]", field, r.Quantile)
	}
	if r.Margin < 0 || r.Margin > maxMargin {
		return invalid("%s.margin %g outside [0, %g]", field, r.Margin, maxMargin)
	}
	// 0 = "no limit" (cpu) / chart default; otherwise a limit below the request
	// would be rejected by the API server (limit < request) — so >= 1.
	if r.LimitFactor != 0 && (r.LimitFactor < 1 || r.LimitFactor > maxLimitFactor) {
		return invalid("%s.limitFactor %g must be 0 (unset) or within [1, %g]", field, r.LimitFactor, maxLimitFactor)
	}
	var lo, hi float64
	for _, q := range []struct {
		name, v string
		dst     *float64
	}{{"min", r.Min, &lo}, {"max", r.Max, &hi}} {
		if q.v == "" {
			continue
		}
		parsed, err := resource.ParseQuantity(q.v)
		if err != nil || parsed.Sign() < 0 {
			return invalid("%s.%s %q is not a non-negative quantity (e.g. 250m, 512Mi)", field, q.name, q.v)
		}
		*q.dst = parsed.AsApproximateFloat64()
	}
	if r.Min != "" && r.Max != "" && hi > 0 && lo > hi {
		return invalid("%s.min %s > %s.max %s", field, r.Min, field, r.Max)
	}
	return nil
}

func validateSchedule(s ScheduleView) error {
	if err := oneOf("schedule.outsideMode", s.OutsideMode, modes); err != nil {
		return err
	}
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return invalid("schedule.timezone %q: %v", s.Timezone, err)
		}
	}
	for i, w := range s.Windows {
		for _, d := range w.Days {
			if !containsStr(weekdays, d) {
				return invalid("schedule.windows[%d].days: %q not in %s", i, d, strings.Join(weekdays, "|"))
			}
		}
		if (w.Start == "") != (w.End == "") {
			return invalid("schedule.windows[%d]: start and end go together (HH:MM)", i)
		}
		for _, t := range []string{w.Start, w.End} {
			if t != "" && !hhmm.MatchString(t) {
				return invalid("schedule.windows[%d]: %q is not HH:MM", i, t)
			}
		}
		for _, d := range []string{w.From, w.Until} {
			if d == "" {
				continue
			}
			if _, err := time.Parse("2006-01-02", d); err != nil {
				return invalid("schedule.windows[%d]: %q is not YYYY-MM-DD", i, d)
			}
		}
	}
	return nil
}

func validateRule(r ContainerRule) error {
	if len(r.Match.Names) == 0 && len(r.Match.Classes) == 0 {
		return invalid("match needs names and/or classes")
	}
	for _, c := range r.Match.Classes {
		if !containsStr(classes, c) {
			return invalid("match.classes: %q not in %s", c, strings.Join(classes, "|"))
		}
	}
	for _, c := range []error{oneOf("mode", r.Mode, modes), oneOf("apply", r.Apply, applies), oneOf("direction", r.Direction, directions), oneOf("guaranteed", r.Guaranteed, guaranteeds)} {
		if c != nil {
			return c
		}
	}
	if err := validateRes("cpu", r.CPU); err != nil {
		return err
	}
	return validateRes("memory", r.Memory)
}

// ModeInherit is the API value that removes a mode override annotation, so the
// workload (or container) falls back to what the policies resolve.
const ModeInherit = "inherit"

// ParseModeOverride validates a mode sent to the per-workload / per-container
// write API. It returns the annotation value to write, or nil to remove the
// annotation ("" or "inherit"). Anything else is ErrInvalid: an unknown mode in
// the annotation would silently resolve to "not actuating" while the UI shows
// the typo.
func ParseModeOverride(m string) (*string, error) {
	if m == "" || m == ModeInherit {
		return nil, nil
	}
	if !containsStr(modes, m) {
		return nil, invalid("mode %q not in %s|%s", m, strings.Join(modes[1:], "|"), ModeInherit)
	}
	return &m, nil
}
