package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// TestReworkAdvice: the advisory line names the bead's points, the share as a
// whole percent and the measured landings, and it is withheld when the sample
// is too thin or the bead has no positive estimate (gt-quiycq).
func TestReworkAdvice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		points int
		share  float64
		n      int
		want   string
	}{
		{"8 points at 27%", 8, 0.27, 30, "advice: beads estimated at 8 points needed a second attempt 27% of the time (30 landings, last 14 days)"},
		{"the minimum landing count still gives a line", 13, 0.55, 20, "advice: beads estimated at 13 points needed a second attempt 55% of the time (20 landings, last 14 days)"},
		{"the share is rounded to a whole percent", 5, 0.1234, 209, "advice: beads estimated at 5 points needed a second attempt 12% of the time (209 landings, last 14 days)"},
		{"one landing under the minimum", 8, 0.27, 19, ""},
		{"zero points", 0, 0.27, 30, ""},
		{"negative points", -3, 0.27, 30, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := reworkAdvice(tc.points, tc.share, tc.n)
			if tc.want == "" {
				if ok || got != "" {
					t.Errorf("reworkAdvice(%d, %v, %d) = %q, %v; want no line", tc.points, tc.share, tc.n, got, ok)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("reworkAdvice(%d, %v, %d) = %q, %v; want %q, true", tc.points, tc.share, tc.n, got, ok, tc.want)
			}
		})
	}
}

// fakeReworkLandings is the read seam's fake: it returns fixed records or a
// fixed error, applying Since's documented filter so the cutoff reworkLandings
// computes is what decides the result.
type fakeReworkLandings struct {
	since time.Time // the cutoff the last Since call was given
	recs  []land.LandingRecord
	err   error
}

func (f *fakeReworkLandings) Since(t time.Time) ([]land.LandingRecord, error) {
	f.since = t
	if f.err != nil {
		return nil, f.err
	}
	var out []land.LandingRecord
	for _, rec := range f.recs {
		if !rec.LandedAt.Before(t) {
			out = append(out, rec)
		}
	}
	return out, nil
}

// TestReworkLandings: reworkLandings returns the rig's landings inside the
// advisory window through the read seam, keeps a landing exactly on the window
// edge, and refuses a failed read with the rig named and the records nil rather
// than an empty list the advisory would count as no landings (gt-wihqmh).
func TestReworkLandings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	windowStart := now.AddDate(0, 0, -reworkAdviceDays)

	recent := land.LandingRecord{BeadID: "gt-recent", LandedAt: now.Add(-3 * day)}
	edge := land.LandingRecord{BeadID: "gt-edge", LandedAt: windowStart}
	old := land.LandingRecord{BeadID: "gt-old", LandedAt: now.Add(-20 * day)}

	errBoom := errors.New("boom")

	for _, tc := range []struct {
		name      string
		recs      []land.LandingRecord
		readErr   error
		openErr   error
		wantIs    error // the cause the returned error must wrap; nil means no error
		wantIDs   []string
		wantSince time.Time
	}{
		{
			name:      "the window keeps a recent landing and drops an old one",
			recs:      []land.LandingRecord{recent, old},
			wantIDs:   []string{"gt-recent"},
			wantSince: windowStart,
		},
		{
			name:      "a landing exactly on the window edge is kept",
			recs:      []land.LandingRecord{edge, old},
			wantIDs:   []string{"gt-edge"},
			wantSince: windowStart,
		},
		{
			name:      "no landings is an empty result and no error",
			wantSince: windowStart,
		},
		{
			name:    "a failed read names the rig and wraps its cause",
			readErr: errBoom,
			wantIs:  errBoom,
		},
		{
			name:    "a failed open names the rig and wraps its cause",
			openErr: errBoom,
			wantIs:  errBoom,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeReworkLandings{recs: tc.recs, err: tc.readErr}
			open := func(townRoot, rig string) (reworkLandingReader, error) {
				if tc.openErr != nil {
					return nil, tc.openErr
				}
				return fake, nil
			}
			recs, err := reworkLandings(open, "/town", "gastown", now)
			if tc.wantIs != nil {
				if !errors.Is(err, tc.wantIs) || err == nil || !strings.Contains(err.Error(), "gastown") {
					t.Fatalf("reworkLandings() error = %v; want one wrapping %v and naming the rig", err, tc.wantIs)
				}
				if recs != nil {
					t.Errorf("reworkLandings() recs = %v; want nil on error", recs)
				}
				return
			}
			if err != nil {
				t.Fatalf("reworkLandings() error = %v; want none", err)
			}
			var gotIDs []string
			for _, rec := range recs {
				gotIDs = append(gotIDs, rec.BeadID)
			}
			if !slices.Equal(gotIDs, tc.wantIDs) {
				t.Errorf("reworkLandings() ids = %v; want %v", gotIDs, tc.wantIDs)
			}
			if !fake.since.Equal(tc.wantSince) {
				t.Errorf("Since() cutoff = %v; want %v", fake.since, tc.wantSince)
			}
		})
	}
}

// TestReworkLandingsOpensThroughRigLandingsFile: the closure compiles as the
// seam, and an invalid rig comes back a nil reader and an error naming the rig.
func TestReworkLandingsOpensThroughRigLandingsFile(t *testing.T) {
	t.Parallel()
	if reader, err := openRigLandings("/town", "a/b"); err == nil || reader != nil {
		t.Fatalf("openRigLandings(invalid rig) = %v, %v; want a nil reader and an error", reader, err)
	}
	if recs, err := reworkLandings(openRigLandings, "/town", "a/b", time.Time{}); err == nil || recs != nil || !strings.Contains(err.Error(), "a/b") {
		t.Errorf("reworkLandings(invalid rig) = %v, %v; want nil records and an error naming the rig", recs, err)
	}
}
