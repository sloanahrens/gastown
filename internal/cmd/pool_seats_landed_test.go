package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
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
			writeLandingLogForPool(t, town, "gt-ruby", c.unreadableLog, c.landedAt)

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

// writeLandingLogForPool writes the gastown rig's landing log with one record
// for bead landed at landedAt, or an empty log when landedAt is nil. With
// unreadable set the log path is a directory, so reading it fails.
func writeLandingLogForPool(t *testing.T, townRoot, bead string, unreadable bool, landedAt *time.Time) {
	t.Helper()
	dir := filepath.Join(constants.TownRuntimePath(townRoot), "landings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gastown.jsonl")
	if unreadable {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return
	}
	if landedAt == nil {
		return
	}
	line, err := json.Marshal(land.LandingRecord{BeadID: bead, Rig: "gastown", Head: "abc123", LandedAt: *landedAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
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
	writeLandingLogForPool(t, town, "gt-ruby", true, nil)

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
