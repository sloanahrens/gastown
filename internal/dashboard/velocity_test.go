package dashboard

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updateVelocityGolden = flag.Bool("update-velocity-golden", false, "rewrite testdata/velocity_feed.golden")

// fixedVelocity is the feed the golden pins: one hour with every value defined
// and a second with its differences unavailable, a class slice, and figures
// with every value defined. It is fixed data, so the golden does not move with
// the clock.
func fixedVelocity() Velocity {
	hour := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)
	return Velocity{
		Hours: []VelocityHour{
			{
				Hour: hour, Points: 13, Weighted: 4, Unweighted: 1,
				RejectedPoints: 3, RejectedUnweighted: 1,
				Smoothed: ptrFloat(2.5), D1: ptrFloat(-0.5), D2: ptrFloat(0.25),
				D1State: VelocityStateSignal, D2State: VelocityStateNoise,
			},
			{
				Hour: hour.Add(time.Hour), Points: 5, Weighted: 2,
				D1State: VelocityStateInsufficient, D2State: VelocityStateInsufficient,
			},
		},
		Classes: map[string][]VelocityHour{
			"all":   {{Hour: hour, Points: 13, Weighted: 4, Unweighted: 1, RejectedPoints: 3, RejectedUnweighted: 1, Smoothed: ptrFloat(2.5), D1: ptrFloat(-0.5), D2: ptrFloat(0.25), D1State: VelocityStateSignal, D2State: VelocityStateNoise}},
			"haiku": {{Hour: hour, Points: 8, Weighted: 2, Unweighted: 1, Smoothed: ptrFloat(1.5), D1State: VelocityStateNoise, D2State: VelocityStateInsufficient}},
		},
		Figures: VelocityFigures{
			Points24h: 21, Unweighted24h: 1,
			NowPerHour: ptrFloat(3), AvgPerHour: ptrFloat(2.5), MedianPerLanding: ptrInt(5),
		},
	}
}

// TestVelocityGolden pins the feed's JSON shape: its snake_case names, the
// omitted nil values, both state strings as the feed carries them, and a class
// slice beside the whole series.
func TestVelocityGolden(t *testing.T) {
	t.Parallel()
	got, err := json.MarshalIndent(fixedVelocity(), "", "  ")
	if err != nil {
		t.Fatalf("marshalling the fixed velocity: %v", err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "velocity_feed.golden")
	if *updateVelocityGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the velocity feed differs from %s (rewrite with -update-velocity-golden):\n%s\nwant:\n%s", golden, got, want)
	}
}

// TestTrendVelocityOmittedWhenNil: a trend with no velocity carries no
// "velocity" key, so a page cannot read a missing feed as an empty one; with
// the feed set, the key carries it.
func TestTrendVelocityOmittedWhenNil(t *testing.T) {
	t.Parallel()
	hour := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)
	without := Trend{Hours: []TrendHour{{Hour: hour, Landed: 1}}}

	raw, err := json.Marshal(without)
	if err != nil {
		t.Fatalf("marshalling the trend: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decoding the trend: %v", err)
	}
	if _, ok := fields["velocity"]; ok {
		t.Errorf("a trend with a nil velocity carries the key: %s", raw)
	}

	with := without
	with.Velocity = &Velocity{Hours: []VelocityHour{{Hour: hour}}}
	raw, err = json.Marshal(with)
	if err != nil {
		t.Fatalf("marshalling the trend with a velocity: %v", err)
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decoding the trend with a velocity: %v", err)
	}
	var got Velocity
	if err := json.Unmarshal(fields["velocity"], &got); err != nil {
		t.Fatalf("the velocity key does not decode as a Velocity: %v", err)
	}
	if len(got.Hours) != 1 || !got.Hours[0].Hour.Equal(hour) {
		t.Errorf("velocity = %+v, want one hour at %v", got, hour)
	}
}

func ptrInt(v int) *int { return &v }
