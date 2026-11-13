package polecat

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/land/landtest"
)

// landedSeatTestRig is the rig every fixture here lands in.
const landedSeatTestRig = "gastown"

// TestSubmissionLandedReadsTheLandingRecord pins the one predicate the pool,
// the patrol's seat cap and the classifier share. A landing counts only when
// its record was written at or after the submission it answers; a failed or
// rejected landing writes no record and leaves the seat held.
func TestSubmissionLandedReadsTheLandingRecord(t *testing.T) {
	t.Parallel()
	submitted := time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC)
	submittedSeat := func(at time.Time) intent.Record {
		return intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-jhz3t0", SubmittedAt: at}
	}
	cases := []struct {
		name    string
		rec     intent.Record
		landed  []time.Time // records written to the rig's landing log
		noLog   bool        // no landing log at all
		want    bool
		wantErr bool
	}{
		{
			name:   "a record written after the submission releases the seat",
			rec:    submittedSeat(submitted),
			landed: []time.Time{submitted.Add(time.Hour)},
			want:   true,
		},
		{
			name:   "a record stamped exactly at the submission releases the seat",
			rec:    submittedSeat(submitted),
			landed: []time.Time{submitted},
			want:   true,
		},
		{
			name:  "no landing record keeps the seat held",
			rec:   submittedSeat(submitted),
			noLog: true,
			want:  false,
		},
		{
			name:   "a record from an earlier submission does not release a resubmitted seat",
			rec:    submittedSeat(submitted),
			landed: []time.Time{submitted.Add(-time.Hour)},
			want:   false,
		},
		{
			name:   "a submission with no stamp has nothing to outlive, so any record for its bead releases it",
			rec:    intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-jhz3t0"},
			landed: []time.Time{submitted.Add(-time.Hour)},
			want:   true,
		},
		{
			name:   "a seat that is not submitted is not landed",
			rec:    intent.Record{Desired: intent.DesiredRun, WorkBead: "gt-jhz3t0", SubmittedAt: submitted},
			landed: []time.Time{submitted.Add(time.Hour)},
			want:   false,
		},
		{
			name:   "a submission naming no bead is not landed",
			rec:    intent.Record{Desired: intent.DesiredSubmitted, SubmittedAt: submitted},
			landed: []time.Time{submitted.Add(time.Hour)},
			want:   false,
		},
		{
			// A landing log that cannot be read is an error, never a seat read as
			// landed or as free: the caller decides what the count does.
			name:    "an unreadable landing log is an error",
			rec:     submittedSeat(submitted),
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			switch {
			case c.wantErr:
				// The rig's landing log path is a directory, so the read fails.
				landtest.MakeLandingLogUnreadable(t, town, landedSeatTestRig)
			case !c.noLog:
				recs := make([]land.LandingRecord, 0, len(c.landed))
				for _, at := range c.landed {
					recs = append(recs, land.LandingRecord{BeadID: c.rec.WorkBead, Rig: landedSeatTestRig, Head: "abc123", LandedAt: at})
				}
				landtest.WriteLandingLog(t, town, landedSeatTestRig, recs...)
			}
			got, err := SubmissionLanded(town, landedSeatTestRig, c.rec)
			if c.wantErr {
				if err == nil {
					t.Fatalf("SubmissionLanded = %v, want an error for an unreadable landing log", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SubmissionLanded: %v", err)
			}
			if got != c.want {
				t.Fatalf("SubmissionLanded = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSubmissionLandedRefusesAnInvalidRig: a rig name that cannot name a landing
// log is an error, not a seat read as not landed.
func TestSubmissionLandedRefusesAnInvalidRig(t *testing.T) {
	t.Parallel()
	rec := intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-jhz3t0", SubmittedAt: time.Now()}
	if got, err := SubmissionLanded(t.TempDir(), "a/b", rec); err == nil {
		t.Fatalf("SubmissionLanded = %v, want an error for a rig name with a path separator", got)
	}
}

// TestLandedSeatDoesNotCountTowardCapacity: a submitted seat whose landing is
// recorded does not hold a capacity seat. A failed push, a failed merge request
// and a submission with no landing record keep counting.
func TestLandedSeatDoesNotCountTowardCapacity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		in     WorkstateInput
		counts bool
	}{
		{
			name:   "a submitted seat whose landing is recorded frees its seat",
			in:     WorkstateInput{State: StateSubmitted, HookBeadSubmitted: true, LandingRecorded: true},
			counts: false,
		},
		{
			name:   "a submitted seat with no landing record holds its seat",
			in:     WorkstateInput{State: StateSubmitted, HookBeadSubmitted: true},
			counts: true,
		},
		{
			name:   "a push that failed keeps the seat when no landing is recorded",
			in:     WorkstateInput{State: StateSubmitted, HookBeadSubmitted: true, PushFailed: true},
			counts: true,
		},
		{
			name:   "a merge request that failed keeps the seat when no landing is recorded",
			in:     WorkstateInput{State: StateSubmitted, HookBeadSubmitted: true, MRFailed: true},
			counts: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := DecideWorkstate(c.in)
			if got.CountsTowardCapacity != c.counts {
				t.Fatalf("CountsTowardCapacity = %v, want %v (verdict %q, reason %q)",
					got.CountsTowardCapacity, c.counts, got.Verdict, got.Reason)
			}
		})
	}
}
