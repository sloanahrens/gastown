package storypoints

import (
	"testing"
	"time"

	_ "time/tzdata" // the DST tests load America/Chicago, so they must not depend on the host's zoneinfo
)

// hourWant is one expected HourPoint, without its hour.
type hourWant struct {
	points             int
	weighted           int
	unweighted         int
	rejectedPoints     int
	rejectedUnweighted int
}

// wantHours checks one class's buckets against the expected counts.
func wantHours(t *testing.T, class string, got []HourPoint, want []hourWant) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d hours, want %d", class, len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Points != w.points || g.Weighted != w.weighted || g.Unweighted != w.unweighted ||
			g.RejectedPoints != w.rejectedPoints || g.RejectedUnweighted != w.rejectedUnweighted {
			t.Errorf("%s hour %d = (points %d, weighted %d, unweighted %d, rejected %d, rejected unweighted %d), want (%d, %d, %d, %d, %d)",
				class, i,
				g.Points, g.Weighted, g.Unweighted, g.RejectedPoints, g.RejectedUnweighted,
				w.points, w.weighted, w.unweighted, w.rejectedPoints, w.rejectedUnweighted)
		}
	}
}

// TestHourlyCountsPointsPerHourAndClass pins the points, weighted and
// unweighted counts of every hour for all landings and for each seat class.
// An agent with no seat class counts in all only.
func TestHourlyCountsPointsPerHourAndClass(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, loc)
	landings := []Landing{
		{At: base.Add(10 * time.Minute), Agent: "sbx-haiku", PolecatSecs: 600},                // 8 points
		{At: base.Add(50 * time.Minute), Agent: "sbx-haiku", PolecatSecs: 120},                // 2 points
		{At: base.Add(2 * time.Hour), Agent: "claude-sonnet", PolecatSecs: 0},                 // no time
		{At: base.Add(2*time.Hour + 15*time.Minute), Agent: "claude-opus", PolecatSecs: 300},  // 5 points, no class
		{At: base.Add(2*time.Hour + 30*time.Minute), Agent: "deepseek-v3", PolecatSecs: 1800}, // 34 points, flash
		{At: base.Add(2*time.Hour + 45*time.Minute), Agent: "sbx-flash", PolecatSecs: 3600, LowerBound: true},
	}

	s := Hourly(landings, nil, base, 3, loc)

	if len(s.Classes) != 4 {
		t.Fatalf("series has %d classes, want 4: all, haiku, flash and sonnet", len(s.Classes))
	}
	wantHours(t, "all", s.Classes[AllClass], []hourWant{
		{points: 10, weighted: 2},
		{},
		{points: 39, weighted: 2, unweighted: 2},
	})
	wantHours(t, "haiku", s.Classes["haiku"], []hourWant{
		{points: 10, weighted: 2},
		{},
		{},
	})
	wantHours(t, "flash", s.Classes["flash"], []hourWant{
		{},
		{},
		{points: 34, weighted: 1, unweighted: 1},
	})
	wantHours(t, "sonnet", s.Classes["sonnet"], []hourWant{
		{},
		{},
		{unweighted: 1},
	})

	for i, hp := range s.Classes[AllClass] {
		if want := base.Add(time.Duration(i) * time.Hour); !hp.Hour.Equal(want) {
			t.Errorf("all hour %d starts at %s, want %s", i, hp.Hour, want)
		}
	}
}

// TestHourlySeparatesUnweightedAndRejectedPoints pins that a landing with no
// polecat time is unweighted and carries no points, and that rejected points
// are counted apart from landed points.
func TestHourlySeparatesUnweightedAndRejectedPoints(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	landings := []Landing{
		{At: base.Add(5 * time.Minute), Agent: "sbx-haiku", PolecatSecs: 600}, // 8 points
		{At: base.Add(10 * time.Minute), Agent: "sbx-haiku", PolecatSecs: 0},  // no time
	}
	rejections := []Rejection{
		{At: base.Add(15 * time.Minute), Agent: "sbx-haiku", PolecatSecs: 600}, // 8 rejected points
		{At: base.Add(20 * time.Minute), Agent: "sbx-haiku", PolecatSecs: 600, LowerBound: true},
	}

	s := Hourly(landings, rejections, base, 1, time.UTC)

	want := []hourWant{{points: 8, weighted: 1, unweighted: 1, rejectedPoints: 8, rejectedUnweighted: 1}}
	wantHours(t, "all", s.Classes[AllClass], want)
	wantHours(t, "haiku", s.Classes["haiku"], want)
	wantHours(t, "flash", s.Classes["flash"], []hourWant{{}})
	wantHours(t, "sonnet", s.Classes["sonnet"], []hourWant{{}})
}

// TestHourlyWindowEdges pins which records a window of three hours counts: one
// outside each end is ignored, the record on the start boundary is in the
// first hour, and the record just before the end is in the last.
func TestHourlyWindowEdges(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	landings := []Landing{
		{At: base.Add(-time.Second), PolecatSecs: 60},
		{At: base, PolecatSecs: 60},
		{At: base.Add(3 * time.Hour), PolecatSecs: 60},
		{At: base.Add(3*time.Hour - time.Nanosecond), PolecatSecs: 60},
	}

	s := Hourly(landings, nil, base, 3, time.UTC)

	wantHours(t, "all", s.Classes[AllClass], []hourWant{
		{points: 1, weighted: 1},
		{},
		{points: 1, weighted: 1},
	})
}

// TestHourlyAcrossADSTChange pins the hour each record lands in when the
// window crosses a daylight saving change, in both directions.
func TestHourlyAcrossADSTChange(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("spring forward skips the missing hour", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 3, 8, 0, 0, 0, 0, loc) // 00:00 CST
		landings := []Landing{
			{At: base, PolecatSecs: 60},                                   // 00:00
			{At: base.Add(time.Hour), PolecatSecs: 60},                    // 01:00 CST
			{At: base.Add(2 * time.Hour), PolecatSecs: 60},                // 03:00 CDT
			{At: base.Add(2*time.Hour + 30*time.Minute), PolecatSecs: 60}, // 03:30 CDT
		}

		s := Hourly(landings, nil, base, 4, loc)

		wantHours(t, "all", s.Classes[AllClass], []hourWant{
			{points: 1, weighted: 1},
			{points: 1, weighted: 1},
			{points: 2, weighted: 2},
			{},
		})
		for i, hp := range s.Classes[AllClass] {
			if hp.Hour.Hour() == 2 {
				t.Errorf("hour %d is the wall hour 02:00, which does not exist on a spring-forward day", i)
			}
		}
		if got, want := s.Classes[AllClass][2].Hour, time.Date(2026, 3, 8, 3, 0, 0, 0, loc); !got.Equal(want) {
			t.Errorf("hour 2 starts at %s, want %s", got, want)
		}
	})

	t.Run("fall back starts a window in the second repeated hour", func(t *testing.T) {
		t.Parallel()
		second := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC) // 01:00 CST, the second 01:00
		firstHour := second.Add(30 * time.Minute)              // 01:30 CST
		landings := []Landing{
			{At: second.Add(-20 * time.Minute), PolecatSecs: 600}, // 01:40 CDT, before the window: 8 points, ignored
			{At: second.Add(10 * time.Minute), PolecatSecs: 60},   // 01:10 CST
			{At: second.Add(70 * time.Minute), PolecatSecs: 60},   // 02:10 CST
		}

		s := Hourly(landings, nil, firstHour, 2, loc)

		wantHours(t, "all", s.Classes[AllClass], []hourWant{
			{points: 1, weighted: 1},
			{points: 1, weighted: 1},
		})
		for i, hp := range s.Classes[AllClass] {
			if want := second.Add(time.Duration(i) * time.Hour); !hp.Hour.Equal(want) {
				t.Errorf("hour %d starts at %s, want %s", i, hp.Hour, want)
			}
		}
	})

	t.Run("fall back keeps both occurrences of the repeated hour", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 11, 1, 0, 0, 0, 0, loc) // 00:00 CDT
		landings := []Landing{
			{At: base, PolecatSecs: 60},                    // 00:00 CDT
			{At: base.Add(time.Hour), PolecatSecs: 60},     // 01:00 CDT
			{At: base.Add(2 * time.Hour), PolecatSecs: 60}, // 01:00 CST
			{At: base.Add(3 * time.Hour), PolecatSecs: 60}, // 02:00 CST, the window end
		}

		s := Hourly(landings, nil, base, 3, loc)

		wantHours(t, "all", s.Classes[AllClass], []hourWant{
			{points: 1, weighted: 1},
			{points: 1, weighted: 1},
			{points: 1, weighted: 1},
		})
		first, second := s.Classes[AllClass][1].Hour, s.Classes[AllClass][2].Hour
		if first.Equal(second) {
			t.Fatalf("the two 01:00 hours are the same instant %s", first)
		}
		if first.Hour() != 1 || second.Hour() != 1 {
			t.Errorf("repeated hours read %02d:00 and %02d:00, want 01:00 twice", first.Hour(), second.Hour())
		}
		_, off1 := first.Zone()
		_, off2 := second.Zone()
		if off1 == off2 {
			t.Errorf("both repeated hours have UTC offset %d, want the change of offset", off1)
		}
	})
}

// TestHourStartNilLocIsTimeLocal pins that a nil loc reads the hour in
// time.Local, the zone a caller with no zone of its own is in, so the hour
// matches the one a caller that names time.Local gets.
func TestHourStartNilLocIsTimeLocal(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC)
	if got, want := HourStart(at, nil), HourStart(at, time.Local); !got.Equal(want) {
		t.Errorf("HourStart(%s, nil) = %s, want the time.Local hour %s", at, got, want)
	}
}

// TestHourlyAcrossAHalfHourFallBack pins the window start in Australia/Lord_Howe,
// whose fall-back is 02:00 daylight (+11) to 01:30 standard (+10:30), so the
// wall-clock 01:30 to 02:00 range repeats and no wall-clock :00 starts the
// standard occurrence.
func TestHourlyAcrossAHalfHourFallBack(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("Australia/Lord_Howe")
	if err != nil {
		t.Fatal(err)
	}
	change := time.Date(2026, 4, 4, 15, 0, 0, 0, time.UTC) // 01:30 +10:30, the offset change

	t.Run("second occurrence starts at the offset change", func(t *testing.T) {
		t.Parallel()
		firstHour := change.Add(15 * time.Minute) // 01:45 +10:30
		landings := []Landing{
			{At: change.Add(10 * time.Minute), PolecatSecs: 60}, // 01:40 +10:30
		}

		s := Hourly(landings, nil, firstHour, 2, loc)

		if got := s.Classes[AllClass][0].Hour; !got.Equal(change) {
			t.Errorf("hour 0 starts at %s, want the offset change %s", got, change)
		}
		wantHours(t, "all", s.Classes[AllClass], []hourWant{
			{points: 1, weighted: 1},
			{},
		})
	})

	t.Run("first occurrence starts at its wall-clock hour", func(t *testing.T) {
		t.Parallel()
		firstHour := change.Add(-15 * time.Minute) // 01:45 +11:00
		want := change.Add(-time.Hour)             // 01:00 +11:00
		landings := []Landing{
			{At: change.Add(-10 * time.Minute), PolecatSecs: 60}, // 01:50 +11:00
		}

		s := Hourly(landings, nil, firstHour, 1, loc)

		if got := s.Classes[AllClass][0].Hour; !got.Equal(want) {
			t.Errorf("hour 0 starts at %s, want %s", got, want)
		}
		wantHours(t, "all", s.Classes[AllClass], []hourWant{
			{points: 1, weighted: 1},
		})
	})
}
