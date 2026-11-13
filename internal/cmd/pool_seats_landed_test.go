package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/land/landtest"
)

// gt-jhz3t0: the pool's occupancy does not count a seat whose submitted work is
// recorded as landed, whichever source names it: a live session, or a seat
// mid-landing. A seat whose landing is not recorded still counts, and so does
// one whose record is from an earlier submission of its bead. A landing log
// that cannot be read fails the picture.
func TestPoolOccupancyIgnoresALandedSeat(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)
	submittedAt := time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC)
	// Ruby and opal are both submitted; the cases vary ruby's landing record
	// and whether ruby still has a live session. Opal never has a record.
	cases := []struct {
		name string
		// landedAt is the time of ruby's landing record; nil means none.
		landedAt *time.Time
		// live gives ruby a live session on top of its submission.
		live bool
		// unreadableLog makes the rig's landing log a directory.
		unreadableLog bool
		want          []string
		wantErr       bool
	}{
		{
			name:     "a landed seat with no session is not occupied",
			landedAt: timePtrCmd(submittedAt.Add(time.Hour)),
			want:     []string{"landing/gastown/opal"},
		},
		{
			name:     "a live landed seat is not occupied",
			landedAt: timePtrCmd(submittedAt.Add(time.Hour)),
			live:     true,
			want:     []string{"landing/gastown/opal"},
		},
		{
			name: "a seat whose landing is not recorded is occupied",
			want: []string{"gt-ruby", "landing/gastown/opal"},
			live: true,
		},
		{
			name:     "a landing recorded before the resubmission does not free the seat",
			landedAt: timePtrCmd(submittedAt.Add(-time.Hour)),
			want:     []string{"landing/gastown/opal", "landing/gastown/ruby"},
		},
		{
			name:          "a landing log that cannot be read fails the picture",
			unreadableLog: true,
			wantErr:       true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeSubmittedLandingSeat(t, town, "ruby", "gt-ruby", submittedAt)
			writeSubmittedLandingSeat(t, town, "opal", "gt-opal", submittedAt)
			switch {
			case c.unreadableLog:
				landtest.MakeLandingLogUnreadable(t, town, "gastown")
			case c.landedAt != nil:
				landtest.WriteLandingLog(t, town, "gastown", land.LandingRecord{
					BeadID: "gt-ruby", Rig: "gastown", Head: "abc123", LandedAt: *c.landedAt,
				})
			}

			sessions := map[string]map[string]string{}
			if c.live {
				sessions["gt-ruby"] = map[string]string{"GT_ROLE": "gastown/polecats/ruby", "GT_AGENT": "deepseek-flash"}
			}
			got, err := poolSeatSessionsWith(&fakeLister{sessions: sessions}, town, nil, noHeldHooks,
				submittedWork("gt-ruby", "gt-opal"), pool)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error for an unreadable landing log, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, s := range got {
				names = append(names, s.name)
			}
			sort.Strings(names)
			if !reflect.DeepEqual(names, c.want) {
				t.Fatalf("occupied seats = %v, want %v", names, c.want)
			}
		})
	}
}

// TestWithoutLandedSeatsHoldsASeatWhoseRecordCannotBeRead: a seat whose intent
// record cannot be read is not known to be landed, so the filter holds it. The
// hold is not silent: the record read reports the failure once, naming the seat,
// which is also how the seat-class read reports the same record (gt-r0kwus).
func TestWithoutLandedSeatsHoldsASeatWhoseRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	seat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ruby"}
	if err := os.MkdirAll(filepath.Dir(seat.Path(town)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seat.Path(town), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	read := poolSeatRecordTo(town, &out)
	sessions := []poolSession{{name: "gt-ruby", agent: "deepseek-flash", rig: "gastown", polecat: "ruby"}}

	got, err := withoutLandedSeats(town, read, sessions)
	if err != nil {
		t.Fatalf("withoutLandedSeats err = %v; want the seat held, not a refusal", err)
	}
	if len(got) != 1 || got[0].name != "gt-ruby" {
		t.Fatalf("withoutLandedSeats = %+v; want the seat held", got)
	}
	want := "warning: gastown/ruby's seat record cannot be read, holding its seat"
	if warnings := out.String(); strings.Count(warnings, want) != 1 {
		t.Fatalf("warning wrote %q; want exactly one line naming gastown/ruby", warnings)
	}
}

// TestWithoutLandedSeatsHoldsEverySeatWithNoTown: a caller with no town to read
// has no record for any seat, which is a read failure rather than an empty
// record, and the landed filter holds the seat on it.
func TestWithoutLandedSeatsHoldsEverySeatWithNoTown(t *testing.T) {
	t.Parallel()
	if _, err := poolSeatRecord(nil, "gastown", "ruby"); err == nil {
		t.Fatal("poolSeatRecord with no reader = nil error; want the read failure")
	}
	sessions := []poolSession{{name: "gt-ruby", agent: "deepseek-flash", rig: "gastown", polecat: "ruby"}}
	got, err := withoutLandedSeats("", nil, sessions)
	if err != nil {
		t.Fatalf("withoutLandedSeats err = %v; want the seat held", err)
	}
	if len(got) != 1 || got[0].name != "gt-ruby" {
		t.Fatalf("withoutLandedSeats = %+v; want the seat held", got)
	}
}

// writeSubmittedLandingSeat stages polecat name as a submitted seat on bead,
// submitted at submittedAt, with its polecat directory in place.
func writeSubmittedLandingSeat(t *testing.T, townRoot, polecatName, bead string, submittedAt time.Time) {
	t.Helper()
	writeTestRigsConfig(t, townRoot, "gastown")
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "polecats", polecatName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: polecatName}, townRoot,
		intent.Record{Desired: intent.DesiredSubmitted, WorkBead: bead, SubmittedAt: submittedAt})
}

// TestPoolOccupancyFailsWhenALiveSeatsLandingLogIsUnreadable: a live seat whose
// submission is recorded but whose landing log cannot be read fails the picture,
// rather than being counted as free or held on a read that failed. Ruby is the
// only submitted seat, so the live filter is what reads the log.
func TestPoolOccupancyFailsWhenALiveSeatsLandingLogIsUnreadable(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	submittedAt := time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC)
	writeSubmittedLandingSeat(t, town, "ruby", "gt-ruby", submittedAt)
	landtest.MakeLandingLogUnreadable(t, town, "gastown")

	sessions := map[string]map[string]string{
		"gt-ruby": {"GT_ROLE": "gastown/polecats/ruby", "GT_AGENT": "deepseek-flash"},
	}
	got, err := poolSeatSessionsWith(&fakeLister{sessions: sessions}, town, nil, noHeldHooks,
		submittedWork("gt-ruby"), seatPool("deepseek-flash", 3))
	if err == nil {
		t.Fatalf("want an error for a live seat whose landing log cannot be read, got %+v", got)
	}
}

func timePtrCmd(t time.Time) *time.Time { return &t }
