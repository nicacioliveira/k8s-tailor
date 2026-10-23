package policy

import (
	"errors"
	"testing"
)

// ParseModeOverride is the single validation for a mode written through the API
// (workload or container annotation): a real mode is set, "" / "inherit"
// removes the annotation, anything else is ErrInvalid (never written).
func TestParseModeOverride(t *testing.T) {
	for _, m := range []string{"Off", "Recommend", "Webhook", "InPlace", "Both"} {
		v, err := ParseModeOverride(m)
		if err != nil || v == nil || *v != m {
			t.Errorf("%q: got %v, %v", m, v, err)
		}
	}
	for _, m := range []string{"", ModeInherit} {
		if v, err := ParseModeOverride(m); err != nil || v != nil {
			t.Errorf("%q must clear the annotation: got %v, %v", m, v, err)
		}
	}
	for _, m := range []string{"inplace", "Auto", "Inherit", " Off"} {
		if _, err := ParseModeOverride(m); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", m, err)
		}
	}
}
