package cmd

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// drainingTickRoster is the roster read of the gt-97ppg tick: capable is a live
// seat, pool-claim/c1 is a claim the roster counts, and gt-flash is a draining
// polecat the roster released and the sling still holds.
func drainingTickRoster() []poolSession {
	return []poolSession{
		{name: "gt-capable", agent: "claude-sonnet", rig: "gastown", polecat: "capable"},
		{name: "pool-claim/c1", agent: "deepseek-flash"},
	}
}

// drainingTickHeld is the sling-side read over the same tick: the roster's two
// occupancies, once each, and gt-flash, which the roster does not count.
func drainingTickHeld() []poolSession {
	return []poolSession{
		{name: "gt-capable", agent: "claude-sonnet", rig: "gastown", polecat: "capable"},
		{name: "gt-flash", agent: "claude-sonnet", rig: "gastown", polecat: "flash"},
		{name: "pool-claim/c1", agent: "deepseek-flash"},
	}
}

// TestSpecTickTownTotalSkipsOnceAcrossCandidates drives a tick over the golden
// board, with a town of three where the roster reads two and the sling-side
// read holds three. The occupancy is read exactly once for the whole tick, not
// once per candidate, and every candidate that reaches a seat is skipped on the
// town total with the exact line; nothing is slung.
func TestSpecTickTownTotalSkipsOnceAcrossCandidates(t *testing.T) {
	t.Parallel()
	f := specGoldenTown()
	f.maxTotal = 3
	env := f.env()
	occupiedReads := 0
	env.Roster = func() (specRoster, error) {
		return specRosterStep(
			func() (poolSeatSnapshot, error) { return poolSeatSnapshot{sessions: drainingTickRoster()}, nil },
			nil,
			func(poolSeatSnapshot) func() ([]poolSession, error) {
				return func() ([]poolSession, error) {
					occupiedReads++
					return drainingTickHeld(), nil
				}
			},
			func(poolSeatSnapshot) {},
		)
	}
	r := runSpecDispatchCycle(env)

	if occupiedReads != 1 {
		t.Fatalf("sling-side occupancy read %d times in one tick, want exactly 1", occupiedReads)
	}
	if len(f.slingRecords) != 0 {
		t.Fatalf("slung %d bead(s) on a town at its total, want none", len(f.slingRecords))
	}
	var skips []string
	for _, e := range r.Skipped {
		if strings.Contains(e.Line, "over the town total") {
			skips = append(skips, e.Line)
		}
	}
	sort.Strings(skips)
	const reason = ": no seat: over the town total: 3/3 live polecats across every seat"
	want := []string{
		"gt-box2" + reason,
		"gt-c1" + reason,
		"gt-c2" + reason,
		"gt-c3" + reason,
		"gt-c4" + reason,
		"gt-c5" + reason,
		"gt-c6" + reason,
		"gt-flip" + reason,
		"gt-fmt" + reason,
		"gt-last" + reason,
		"gt-late" + reason,
		"gt-second" + reason,
	}
	if !reflect.DeepEqual(skips, want) {
		t.Fatalf("town-total skip lines =\n%q\nwant\n%q", skips, want)
	}
	if len(r.Errors) != 0 {
		t.Fatalf("tick errors = %q, want none on a readable roster", r.Errors)
	}
}

// TestSpecTickRosterFailsClosed: a tick whose seat or sling-side read fails
// dispatches nothing, and the report names the failure in its exact words.
func TestSpecTickRosterFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		read     func() (poolSeatSnapshot, error)
		occupied func() ([]poolSession, error)
		want     string
	}{
		{
			name:     "the seat snapshot read fails",
			read:     func() (poolSeatSnapshot, error) { return poolSeatSnapshot{}, errors.New("tmux down") },
			occupied: func() ([]poolSession, error) { return nil, nil },
			want:     "roster unreadable, not dispatching: listing polecat seats: tmux down",
		},
		{
			name: "the sling-side read fails",
			read: func() (poolSeatSnapshot, error) { return poolSeatSnapshot{sessions: drainingTickRoster()}, nil },
			occupied: func() ([]poolSession, error) {
				return nil, errors.New("tmux down")
			},
			want: "roster unreadable, not dispatching: counting the seats a sling will see: tmux down",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := specGoldenTown()
			f.maxTotal = 3
			env := f.env()
			env.Roster = func() (specRoster, error) {
				return specRosterStep(tc.read, nil,
					func(poolSeatSnapshot) func() ([]poolSession, error) { return tc.occupied },
					func(poolSeatSnapshot) {})
			}
			r := runSpecDispatchCycle(env)

			if len(f.slingRecords) != 0 {
				t.Fatalf("slung %d bead(s) on an unreadable roster, want none", len(f.slingRecords))
			}
			found := false
			for _, e := range r.Errors {
				if e == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("report errors = %q, want the line %q", r.Errors, tc.want)
			}
		})
	}
}
