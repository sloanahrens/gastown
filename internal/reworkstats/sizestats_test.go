package reworkstats

import (
	"fmt"
	"math"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
)

// sizeRec is one landing record for the size tables: the lines its change
// added, the polecat seconds it took, and how many seats it records.
func sizeRec(bead string, addedLines int, secs float64, attempts int) landings.Record {
	rec := landings.Record{Bead: bead, AddedLines: addedLines, PolecatSecs: secs}
	for i := 0; i < attempts; i++ {
		rec.PolecatAttempts = append(rec.PolecatAttempts, land.PolecatAttempt{Seat: fmt.Sprintf("%s-seat-%d", bead, i)})
	}
	return rec
}

// sizeGotWant is one bucket's figures as the tests state them.
type sizeGotWant struct {
	label      string
	beads      int
	median     float64
	multiShare float64
	over30     float64
}

// TestRunawayBeadSizeStatsBucketsRecords is the hand-computed set of
// gt-vqevh3: one record set whose every bucket holds records of known lines and
// time, so a record that lands in the wrong bucket, a median of the wrong pair,
// or a share of the wrong count shows in the row for each of the six buckets.
// The lines 49/50, 149/150, 299/300, 599/600 and 999/1000 put a record on each
// side of every edge, and one record has no attempts so it counts as one.
func TestRunawayBeadSizeStatsBucketsRecords(t *testing.T) {
	t.Parallel()
	recs := []landings.Record{
		// under 50: 10, 20 and 40 minutes; two seats on one, none recorded on
		// one (one attempt, so it is not the multi-attempt one).
		sizeRec("gt-a1", 10, 600, 1),
		sizeRec("gt-a2", 49, 1200, 2),
		sizeRec("gt-a3", 25, 2400, 0),
		// 50 to 149: 5 and 7 minutes.
		sizeRec("gt-b1", 50, 300, 1),
		sizeRec("gt-b2", 149, 420, 1),
		// 150 to 299: exactly 30 minutes is not over 30; 50 minutes is.
		sizeRec("gt-c1", 150, 1800, 1),
		sizeRec("gt-c2", 299, 3000, 2),
		// 300 to 599: 1 and 60 minutes.
		sizeRec("gt-d1", 300, 3600, 1),
		sizeRec("gt-d2", 599, 60, 1),
		// 600 to 999: 15 and 20 minutes.
		sizeRec("gt-e1", 600, 900, 2),
		sizeRec("gt-e2", 999, 1200, 1),
		// 1000 or more: 90 minutes and three seats.
		sizeRec("gt-f1", 1000, 5400, 3),
	}
	want := []sizeGotWant{
		{"under 50", 3, 20, 1.0 / 3, 1.0 / 3},
		{"50 to 149", 2, 6, 0, 0},
		{"150 to 299", 2, 40, 0.5, 0.5},
		{"300 to 599", 2, 30.5, 0, 0.5},
		{"600 to 999", 2, 17.5, 0.5, 0},
		{"1000 or more", 1, 90, 1, 1},
	}

	buckets, skipped := SizeStats(recs)
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0: every record carries added lines and a polecat time", skipped)
	}
	if len(buckets) != len(want) {
		t.Fatalf("buckets = %d, want %d", len(buckets), len(want))
	}
	for i, w := range want {
		got := buckets[i]
		if got.Label != w.label {
			t.Errorf("bucket %d label = %q, want %q", i, got.Label, w.label)
		}
		if got.Beads != w.beads {
			t.Errorf("%s: Beads = %d, want %d", w.label, got.Beads, w.beads)
		}
		sizeCheckFloat(t, w.label+" MedianMinutes", got.MedianMinutes, w.median)
		sizeCheckFloat(t, w.label+" MultiAttemptShare", got.MultiAttemptShare, w.multiShare)
		sizeCheckFloat(t, w.label+" Over30Share", got.Over30Share, w.over30)
	}
}

// TestRunawayBeadSizeStatsSkipsUntimedOrUnsized is the second acceptance box:
// a record with no polecat time or no added lines takes no bucket, is counted
// in the second return value, and does not stop a record beside it from landing.
func TestRunawayBeadSizeStatsSkipsUntimedOrUnsized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		recs        []landings.Record
		wantSkipped int
		wantBeads   map[string]int
	}{
		{
			name:        "no polecat time",
			recs:        []landings.Record{sizeRec("gt-x1", 100, 0, 1)},
			wantSkipped: 1,
		},
		{
			name:        "no added lines",
			recs:        []landings.Record{sizeRec("gt-x2", 0, 600, 1)},
			wantSkipped: 1,
		},
		{
			name:        "neither time nor lines",
			recs:        []landings.Record{sizeRec("gt-x3", 0, 0, 1)},
			wantSkipped: 1,
		},
		{
			name:        "negative polecat time",
			recs:        []landings.Record{sizeRec("gt-x4", 100, -60, 1)},
			wantSkipped: 1,
		},
		{
			name:        "negative added lines",
			recs:        []landings.Record{sizeRec("gt-x5", -1, 600, 1)},
			wantSkipped: 1,
		},
		{
			name: "untimed and unsized beside two that land",
			recs: []landings.Record{
				sizeRec("gt-x1", 100, 0, 1),
				sizeRec("gt-x2", 0, 600, 1),
				sizeRec("gt-x3", 0, 0, 1),
				sizeRec("gt-x6", 10, 600, 1),
				sizeRec("gt-x7", 1000, 600, 1),
			},
			wantSkipped: 3,
			wantBeads:   map[string]int{"under 50": 1, "1000 or more": 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			buckets, skipped := SizeStats(tt.recs)
			if skipped != tt.wantSkipped {
				t.Errorf("skipped = %d, want %d", skipped, tt.wantSkipped)
			}
			for _, bucket := range buckets {
				want := tt.wantBeads[bucket.Label]
				if bucket.Beads != want {
					t.Errorf("%s: Beads = %d, want %d", bucket.Label, bucket.Beads, want)
				}
			}
		})
	}
}

// TestRunawayBeadSizeStatsEmptySlice is the third acceptance box: a town that
// has landed nothing gets the six buckets by name, empty, and no skipped
// records, so a report prints its headings rather than nothing.
func TestRunawayBeadSizeStatsEmptySlice(t *testing.T) {
	t.Parallel()
	wantLabels := []string{"under 50", "50 to 149", "150 to 299", "300 to 599", "600 to 999", "1000 or more"}
	tests := []struct {
		name string
		recs []landings.Record
	}{
		{"nil", nil},
		{"empty", []landings.Record{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			buckets, skipped := SizeStats(tt.recs)
			if skipped != 0 {
				t.Errorf("skipped = %d, want 0", skipped)
			}
			if len(buckets) != len(wantLabels) {
				t.Fatalf("buckets = %d, want %d", len(buckets), len(wantLabels))
			}
			for i, label := range wantLabels {
				got := buckets[i]
				if got != (SizeBucket{Label: label}) {
					t.Errorf("bucket %d = %+v, want an empty %q bucket", i, got, label)
				}
			}
		})
	}
}

// sizeCheckFloat compares a bucket figure with the test's hand-computed value.
func sizeCheckFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
