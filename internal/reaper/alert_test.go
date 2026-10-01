package reaper

import (
	"strings"
	"testing"
)

func TestOpenWispAlert(t *testing.T) {
	t.Parallel()

	previous := &OpenWispSample{OpenWisps: 138, Databases: 2}

	tests := []struct {
		name     string
		current  OpenWispSample
		previous *OpenWispSample
		want     bool
	}{
		{
			name:    "no baseline yet",
			current: OpenWispSample{OpenWisps: 5000, Databases: 2},
		},
		{
			name:     "steady state, the count the old threshold false-alarmed on",
			current:  OpenWispSample{OpenWisps: 138, Databases: 2},
			previous: previous,
		},
		{
			name:     "growth under the factor",
			current:  OpenWispSample{OpenWisps: 200, Databases: 2},
			previous: previous,
		},
		{
			name:     "exactly the factor, under the floor",
			current:  OpenWispSample{OpenWisps: 276, Databases: 2},
			previous: previous,
		},
		{
			name:     "at the factor and past the floor",
			current:  OpenWispSample{OpenWisps: 400, Databases: 2},
			previous: previous,
			want:     true,
		},
		{
			name:     "a small count doubling is noise, not accumulation",
			current:  OpenWispSample{OpenWisps: 6, Databases: 1},
			previous: &OpenWispSample{OpenWisps: 3, Databases: 1},
		},
		{
			name:     "a big absolute jump from a small base still clears the floor",
			current:  OpenWispSample{OpenWisps: 810, Databases: 1},
			previous: &OpenWispSample{OpenWisps: 400, Databases: 1},
			want:     true,
		},
		{
			name:     "shrinking is never an alert",
			current:  OpenWispSample{OpenWisps: 111, Databases: 2},
			previous: &OpenWispSample{OpenWisps: 276, Databases: 2},
		},
		{
			name:     "a dry run does not compare against a live cycle",
			current:  OpenWispSample{OpenWisps: 900, Databases: 2, DryRun: true},
			previous: previous,
		},
		{
			name:     "a wider database set is not growth",
			current:  OpenWispSample{OpenWisps: 900, Databases: 5},
			previous: previous,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alert, detail := OpenWispAlert(tt.current, tt.previous)
			if alert != tt.want {
				t.Fatalf("OpenWispAlert(%+v, %+v) = %v, want %v", tt.current, tt.previous, alert, tt.want)
			}
			if !alert && detail != "" {
				t.Errorf("a non-alert must carry no description, got %q", detail)
			}
			if alert && detail == "" {
				t.Error("an alert must describe the growth it reports")
			}
		})
	}
}

// The threshold the escalation reported was below the town's steady state, so
// the alert fired on a healthy town and said nothing (gt-11kyy). A steady state
// large enough to have tripped any absolute threshold must not alert on its
// own: only the change from the previous cycle can.
func TestOpenWispAlertIsSilentAtSteadyStateAboveAnyAbsoluteThreshold(t *testing.T) {
	t.Parallel()

	for _, count := range []int{138, 1966, 3000, 12000} {
		previous := &OpenWispSample{OpenWisps: count, Databases: 6}
		current := OpenWispSample{OpenWisps: count, Databases: 6}
		if alert, detail := OpenWispAlert(current, previous); alert {
			t.Errorf("steady state of %d alerted: %s", count, detail)
		}
	}
}

func TestOpenWispAlertDescribesTheGrowth(t *testing.T) {
	t.Parallel()

	alert, detail := OpenWispAlert(
		OpenWispSample{OpenWisps: 900, Databases: 2},
		&OpenWispSample{OpenWisps: 138, Databases: 2},
	)
	if !alert {
		t.Fatal("138 to 900 is growth past the factor and the floor")
	}
	for _, want := range []string{"138", "900", "+762", "2 database"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not name %q", detail, want)
		}
	}
}
