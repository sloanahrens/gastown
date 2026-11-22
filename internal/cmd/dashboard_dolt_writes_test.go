package cmd

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The actors' write deltas between two reads land in the buckets under the
// actor's name at the read's time: the first read adds nothing, a counter that
// went down (the gateway restarted) adds the new count, and an actor first seen
// on a later read adds its count.
func TestDoltWriteDeltasLandInTheBucketsByActor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var buckets dashboard.Buckets
	var w doltWriteDeltas
	read := func(step int, reads map[string]uint64, want map[string]int) {
		t.Helper()
		w.add(&buckets, now, reads)
		for actor, n := range want {
			series := buckets.Series(now, actor)
			if got := series[len(series)-1]; got != n {
				t.Errorf("step %d: bucket of %q holds %d, want %d", step, actor, got, n)
			}
		}
	}
	read(1, map[string]uint64{"a": 10, "b": 0}, map[string]int{"a": 0, "b": 0})
	read(2, map[string]uint64{"a": 70, "b": 5}, map[string]int{"a": 60, "b": 5})
	read(3, map[string]uint64{"a": 3, "b": 5}, map[string]int{"a": 63, "b": 5})
	read(4, map[string]uint64{"a": 3, "b": 5, "(other)": 2}, map[string]int{"a": 63, "b": 5, "(other)": 2})
	read(5, map[string]uint64{"a": 3, "b": 5, "(other)": 2}, map[string]int{"a": 63, "b": 5, "(other)": 2})
}

// TestDoltWritesSectionRanksTheSixBusiestActors: the section lists the six
// busiest actors by writes per minute over the last two ten-minute buckets,
// counted over the minutes the dashboard has watched inside them (gt-tiwtir).
func TestDoltWritesSectionRanksTheSixBusiestActors(t *testing.T) {
	t.Parallel()
	at := func(h, m int) time.Time { return time.Date(2026, 11, 22, h, m, 0, 0, time.UTC) }
	seven := func(scale uint64) map[string]uint64 {
		m := map[string]uint64{}
		for i, name := range []string{"g1", "g2", "g3", "g4", "g5", "g6", "g7"} {
			m[name] = scale * uint64(i+1)
		}
		return m
	}
	for _, tc := range []struct {
		name  string
		reads []map[string]uint64
		times []time.Time
		want  [][]string
	}{
		{"a ten minute span ranks a first", []map[string]uint64{{"a": 10, "b": 0}, {"a": 70, "b": 5}}, []time.Time{at(12, 0), at(12, 10)}, [][]string{{"a", "6.0"}, {"b", "0.5"}}},
		{"a dashboard five minutes old divides by five", []map[string]uint64{{"a": 0}, {"a": 30}}, []time.Time{at(12, 5), at(12, 10)}, [][]string{{"a", "6.0"}}},
		{"seven actors list six, the quietest left out", []map[string]uint64{seven(0), seven(10)}, []time.Time{at(12, 0), at(12, 10)}, [][]string{{"g7", "7.0"}, {"g6", "6.0"}, {"g5", "5.0"}, {"g4", "4.0"}, {"g3", "3.0"}, {"g2", "2.0"}}},
		{"equal rates list by name", []map[string]uint64{{"b": 0, "a": 0}, {"b": 20, "a": 20}}, []time.Time{at(12, 0), at(12, 10)}, [][]string{{"a", "2.0"}, {"b", "2.0"}}},
		{"a write in the previous bucket still counts", []map[string]uint64{{"a": 0}, {"a": 60}, {"a": 60}}, []time.Time{at(12, 5), at(12, 15), at(12, 25)}, [][]string{{"a", "4.0"}}},
		{"an actor quiet for two buckets drops out", []map[string]uint64{{"a": 0}, {"a": 60}, {"a": 60}, {"a": 60}, {"a": 60}}, []time.Time{at(12, 0), at(12, 10), at(12, 20), at(12, 30), at(12, 40)}, nil},
		{"reads five minutes apart keep counting", []map[string]uint64{{"a": 0}, {"a": 30}, {"a": 60}}, []time.Time{at(12, 0), at(12, 5), at(12, 10)}, [][]string{{"a", "6.0"}}},
		{"a read after a gap counts nothing and restarts the span", []map[string]uint64{{"a": 0}, {"a": 60}, {"a": 90}}, []time.Time{at(12, 0), at(12, 35), at(12, 40)}, [][]string{{"a", "6.0"}}},
		{"the overflow aggregate follows the six real actors", []map[string]uint64{{"g1": 0, "g2": 0, "g3": 0, "g4": 0, "g5": 0, "g6": 0, "g7": 0, "(other)": 0}, {"g1": 10, "g2": 20, "g3": 30, "g4": 40, "g5": 50, "g6": 60, "g7": 70, "(other)": 500}}, []time.Time{at(12, 0), at(12, 10)}, [][]string{{"g7", "7.0"}, {"g6", "6.0"}, {"g5", "5.0"}, {"g4", "4.0"}, {"g3", "3.0"}, {"g2", "2.0"}, {"(other)", "50.0"}}},
		{"nothing written lists nothing", []map[string]uint64{{"a": 4}, {"a": 4}}, []time.Time{at(12, 0), at(12, 10)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			i := 0
			clock := 0
			s := newDoltWritesSection(func(context.Context) (map[string]uint64, error) { i++; return tc.reads[i-1], nil },
				func() time.Time { clock++; return tc.times[clock-1] })
			var got dashboard.DoltSection
			for range tc.reads {
				var err error
				if got, err = s.read(context.Background(), time.Time{}); err != nil {
					t.Fatalf("read: %v", err)
				}
			}
			if !reflect.DeepEqual(got.Rows, tc.want) {
				t.Errorf("rows = %q, want %q", got.Rows, tc.want)
			}
		})
	}
	t.Run("a failed stats read is the section's error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("gateway down")
		s := newDoltWritesSection(func(context.Context) (map[string]uint64, error) { return nil, boom }, time.Now)
		if _, err := s.read(context.Background(), time.Time{}); !errors.Is(err, boom) {
			t.Errorf("err = %v, want the stats error", err)
		}
	})
	t.Run("the registered section says the stats are not readable yet", func(t *testing.T) {
		t.Parallel()
		for _, r := range doltSections.snapshot() {
			if r.key == "writes" {
				if _, err := r.read(context.Background(), time.Now()); !errors.Is(err, errDoltWritesNoStats) {
					t.Errorf("registered read err = %v, want errDoltWritesNoStats", err)
				}
				return
			}
		}
		t.Error("no writes section is registered")
	})
}

// TestDoltWritesSectionCarriesTheHourAndThePeak: the writes of the six newest
// buckets, every actor together, are the hour, and the largest bucket of the 24
// hours is the peak (gt-tq82en).
func TestDoltWritesSectionCarriesTheHourAndThePeak(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 5, 0, 0, time.UTC)
	type add struct {
		ago time.Duration
		key string
		n   int
	}
	for _, tc := range []struct {
		name       string
		adds       []add
		hour, peak int
	}{
		{"the three newest buckets and one 20 hours old", []add{{0, "a", 30}, {10 * time.Minute, "a", 4}, {20 * time.Minute, "a", 6}, {20 * time.Hour, "a", 90}}, 40, 90},
		{"two actors in one bucket add up to the peak", []add{{3 * time.Hour, "a", 50}, {3 * time.Hour, "b", 45}}, 0, 95},
		{"the sixth bucket is in the hour and the seventh is not", []add{{0, "a", 7}, {50 * time.Minute, "a", 5}, {60 * time.Minute, "a", 100}}, 12, 100},
		{"nothing counted reads zero", nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buckets dashboard.Buckets
			// newest first: Buckets drops a count dated over a minute after its latest time
			for _, a := range tc.adds {
				buckets.Add(now.Add(-a.ago), a.key, a.n)
			}
			if hour, peak := doltWritesHourAndPeak(&buckets, now); hour != tc.hour || peak != tc.peak {
				t.Errorf("hour, peak = %d, %d, want %d, %d", hour, peak, tc.hour, tc.peak)
			}
		})
	}
	t.Run("the section's note carries both", func(t *testing.T) {
		t.Parallel()
		reads := []map[string]uint64{{"a": 0}, {"a": 60}}
		times := []time.Time{now, now.Add(10 * time.Minute)}
		i := 0
		s := newDoltWritesSection(func(context.Context) (map[string]uint64, error) { i++; return reads[i-1], nil },
			func() time.Time { return times[i-1] })
		var got dashboard.DoltSection
		for range reads {
			var err error
			if got, err = s.read(context.Background(), time.Time{}); err != nil {
				t.Fatalf("read: %v", err)
			}
		}
		if want := "last hour: 60 writes; peak 10-minute bucket of the last 24 hours: 60"; got.Note != want {
			t.Errorf("note = %q, want %q", got.Note, want)
		}
	})
}

// TestDoltWritesSectionLeavesOutActorsTheAnswerLacks: an answer with no per-actor
// counts (nil) draws no actor rows and says so in the Note, an answer whose actors
// wrote nothing (empty) does not, and a later answer with actors counts as usual
// (gt-usakr9). A stats error is the section's error (the failed-read row of
// TestDoltWritesSectionRanksTheSixBusiestActors).
func TestDoltWritesSectionLeavesOutActorsTheAnswerLacks(t *testing.T) {
	t.Parallel()
	at := func(m int) time.Time { return time.Date(2026, 11, 22, 12, m, 0, 0, time.UTC) }
	reads := []map[string]uint64{nil, {}, {"a": 0}, {"a": 60}}
	i := 0
	s := newDoltWritesSection(func(context.Context) (map[string]uint64, error) { i++; return reads[i-1], nil },
		func() time.Time { return at(5 * i) })
	for step, want := range []struct {
		note string
		rows [][]string
	}{
		{"the gateway's stats carry no per-actor counts, so the actor rows are left out", nil},
		{"last hour: 0 writes; peak 10-minute bucket of the last 24 hours: 0", nil},
		{"last hour: 0 writes; peak 10-minute bucket of the last 24 hours: 0", nil},
		{"last hour: 60 writes; peak 10-minute bucket of the last 24 hours: 60", [][]string{{"a", "6.0"}}},
	} {
		got, err := s.read(context.Background(), time.Time{})
		if err != nil {
			t.Fatalf("step %d: %v", step+1, err)
		}
		if got.Note != want.note || !reflect.DeepEqual(got.Rows, want.rows) {
			t.Errorf("step %d: note %q rows %q, want %q and %q", step+1, got.Note, got.Rows, want.note, want.rows)
		}
	}
}
