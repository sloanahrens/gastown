package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// A bead the stopped seat still holds is stored with the seat's full assignee,
// so the ownership check must compare against that, not the bare polecat name.
func TestOverrunSeatAssignee(t *testing.T) {
	t.Parallel()
	seat := overrunSeat{Rig: "beads", Polecat: "abacus"}
	if got, want := overrunSeatAssignee(seat), "beads/polecats/abacus"; got != want {
		t.Errorf("overrunSeatAssignee() = %q, want %q", got, want)
	}
}

func TestOverrunSeatOwnsBead(t *testing.T) {
	t.Parallel()
	seat := overrunSeat{Rig: "beads", Polecat: "abacus"}
	cases := []struct {
		name     string
		status   string
		assignee string
		want     bool
	}{
		{name: "assigned to the seat", status: "hooked", assignee: "beads/polecats/abacus", want: true},
		{name: "unassigned", status: "hooked", assignee: "", want: true},
		{name: "bare polecat name", status: "hooked", assignee: "abacus", want: false},
		{name: "another polecat", status: "hooked", assignee: "beads/polecats/other", want: false},
		{name: "closed", status: "closed", assignee: "beads/polecats/abacus", want: false},
		{name: "tombstone", status: "tombstone", assignee: "beads/polecats/abacus", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := overrunSeatOwnsBead(seat, tc.status, tc.assignee); got != tc.want {
				t.Errorf("overrunSeatOwnsBead(%q, %q) = %v, want %v", tc.status, tc.assignee, got, tc.want)
			}
		})
	}
}

// stillOursFor runs the production stillOurs closure for stopSeat over a bead
// store seeded with the bead's status and assignee.
func stillOursFor(t *testing.T, bd *beadsfake.Fake) (bool, error) {
	t.Helper()
	d := &Daemon{
		config:           &Config{TownRoot: t.TempDir()},
		openOverrunBeads: func() overrunHandBackStore { return bd },
	}
	return d.overrunStopSeams(overrunSeams{}).stillOurs(stopSeat)
}

// The closure must judge the bead by the full assignee the seat's bead carries,
// so a bead the seat still holds is its own, and one held by a bare name or
// another polecat is not.
func TestStillOursClosureJudgesTheSeatsFullAssignee(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		status   string
		assignee string
		want     bool
	}{
		{name: "assigned to the seat", status: "hooked", assignee: "gastown/polecats/capable", want: true},
		{name: "unassigned", status: "hooked", assignee: "", want: true},
		{name: "bare polecat name", status: "hooked", assignee: "capable", want: false},
		{name: "another polecat", status: "hooked", assignee: "gastown/polecats/other", want: false},
		{name: "closed", status: "closed", assignee: "gastown/polecats/capable", want: false},
		{name: "tombstone", status: "tombstone", assignee: "gastown/polecats/capable", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := beadsfake.New()
			bd.Seed(beads.Issue{ID: stopSeat.Bead, Status: tc.status, Assignee: tc.assignee})
			got, err := stillOursFor(t, bd)
			if err != nil {
				t.Fatalf("stillOurs(%s) error = %v", stopSeat.Bead, err)
			}
			if got != tc.want {
				t.Errorf("stillOurs(%s) on %s held by %q = %v, want %v", stopSeat.Bead, tc.status, tc.assignee, got, tc.want)
			}
		})
	}
}

// A bead that cannot be read is not the seat's by default: the closure reports
// the read's error and grants no ownership.
func TestStillOursClosureReportsAnUnreadableBead(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	bd.Seed(beads.Issue{ID: stopSeat.Bead, Status: "hooked", Assignee: "gastown/polecats/capable"})
	bd.FailWith("show", errors.New("dolt timeout"))
	got, err := stillOursFor(t, bd)
	if err == nil {
		t.Fatal("stillOurs answered for a bead it could not read")
	}
	if got {
		t.Error("stillOurs granted ownership with a read error")
	}
	if !strings.Contains(err.Error(), stopSeat.Bead) {
		t.Errorf("error %q does not name the bead %s", err, stopSeat.Bead)
	}
}
