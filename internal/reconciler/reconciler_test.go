package reconciler

import "testing"

func TestLimitFloor(t *testing.T) {
	cases := []struct {
		name               string
		rec, current, want float64
	}{
		// The bug: request*factor fell below the current limit → limit crushed.
		// A low recommended limit must be floored at the current one.
		{"never shrink below current", 0.985, 3.0, 3.0},
		{"raise when rec exceeds current", 4.0, 3.0, 4.0},
		{"equal stays", 3.0, 3.0, 3.0},
		{"current unset takes rec", 0.5, 0, 0.5},
	}
	for _, c := range cases {
		if got := limitFloor(c.rec, c.current); got != c.want {
			t.Errorf("%s: limitFloor(%v,%v)=%v want %v", c.name, c.rec, c.current, got, c.want)
		}
	}
}
