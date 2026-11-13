package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// gt-jhz3t0: the patrol's seat cap counts a polecat whose submitted work is
// recorded as landed as holding no seat, so a restart held at a full pool
// proceeds once the only other seats are landed. A seat whose landing is not
// recorded still holds its seat, and a landing log that cannot be read stops
// the restart's answer rather than freeing or filling a seat.
func TestSeatCapCensusIgnoresALandedSeat(t *testing.T) {
	t.Parallel()
	const (
		poolAgent = "deepseek-flash"
		poolSeat  = "polecat_pool.seats." + poolAgent + ".cap"
		settings  = `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"` + poolAgent + `":{"cap":1}}}}`
	)
	submittedAt := testEpoch.Add(-time.Hour)
	ruby := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ruby"}

	cases := []struct {
		name string
		// landedAt is the time of the landing record for ruby's bead; nil means
		// the landing log holds no record for it.
		landedAt *time.Time
		// unreadableLog makes the landing log a directory, so its read fails.
		unreadableLog bool
		want          patrolscan.SeatCap
		wantErr       bool
	}{
		{
			name:     "the only other seat is landed, so the restart proceeds",
			landedAt: timePtr(testEpoch.Add(-30 * time.Minute)),
		},
		{
			name:     "a submitted seat whose landing is not recorded holds the seat",
			landedAt: nil,
			want:     patrolscan.SeatCap{Live: 1, Limit: 1, Key: poolSeat},
		},
		{
			// The record is from an earlier submission of the bead: it does not
			// release the resubmitted seat.
			name:     "a landing recorded before the resubmission holds the seat",
			landedAt: timePtr(testEpoch.Add(-2 * time.Hour)),
			want:     patrolscan.SeatCap{Live: 1, Limit: 1, Key: poolSeat},
		},
		{
			name:          "a landing log that cannot be read is an error",
			unreadableLog: true,
			wantErr:       true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTownFile(t, town, "settings/config.json", settings)
			clk := newFixedClock()
			tm := newFakeTmux(clk)
			seatEnv(t, tm, sessionEnv{rig: "gastown", name: "ruby", agent: poolAgent})
			if err := intent.MarkSubmitted(town, ruby, "gt-ruby", "test", submittedAt); err != nil {
				t.Fatal(err)
			}
			writeLandingLogForCensus(t, town, tc.unreadableLog, tc.landedAt)

			h := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: town}, tmux: tm, clock: clk}, agentRecord: noAgentRecord}
			got, err := h.seatCap("gastown", "nova")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("seatCap = %+v, want an error for an unreadable landing log", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("seatCap: %v", err)
			}
			if got != tc.want {
				t.Fatalf("seatCap = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// writeLandingLogForCensus writes the gastown rig's landing log: one record for
// gt-ruby landed at landedAt, or an empty log when landedAt is nil. With
// unreadable set the log path is a directory, so reading it fails.
func writeLandingLogForCensus(t *testing.T, town string, unreadable bool, landedAt *time.Time) {
	t.Helper()
	dir := filepath.Join(constants.TownRuntimePath(town), "landings")
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
	line, err := json.Marshal(land.LandingRecord{BeadID: "gt-ruby", Rig: "gastown", Head: "abc123", LandedAt: *landedAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSeatCapFailsWhenALiveSeatsIntentRecordIsUnreadable: a live polecat whose
// intent record cannot be read is not known to be landed or not, so the
// census returns the error and the restart waits, rather than counting the
// seat as free.
func TestSeatCapFailsWhenALiveSeatsIntentRecordIsUnreadable(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownFile(t, town, "settings/config.json",
		`{"type":"town-settings","version":1,"polecat_pool":{"seats":{"deepseek-flash":{"cap":1}}}}`)
	clk := newFixedClock()
	tm := newFakeTmux(clk)
	seatEnv(t, tm, sessionEnv{rig: "gastown", name: "ruby", agent: "deepseek-flash"})
	ruby := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ruby"}
	if err := os.MkdirAll(filepath.Dir(ruby.Path(town)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ruby.Path(town), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: town}, tmux: tm, clock: clk}, agentRecord: noAgentRecord}
	if got, err := h.seatCap("gastown", "nova"); err == nil {
		t.Fatalf("seatCap = %+v, want an error for an unreadable intent record", got)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
