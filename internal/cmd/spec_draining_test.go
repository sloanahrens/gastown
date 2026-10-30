package cmd

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestSpecRosterCountsWhatTheSlingHolds is the gt-97ppg case. The roster reads
// a draining polecat's seat as released and drops its session, but the sling's
// own pool check has no known creation time to release it by, so it counts the
// session. The tick's roster must count it as well, or the pre-check admits a
// sling the pool then refuses on the town total.
func TestSpecRosterCountsWhatTheSlingHolds(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	lister := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-capable": {"GT_ROLE": "gastown/polecats/capable", "GT_AGENT": "claude-sonnet"},
			"gt-flash":   {"GT_ROLE": "gastown/polecats/flash", "GT_AGENT": "claude-sonnet"},
			"gt-old":     {"GT_ROLE": "gastown/polecats/old", "GT_AGENT": "claude-sonnet"},
		},
		// gt-flash has no readable creation time, so the listing stamps it with
		// the listing's own time, which is after the roster read.
		created: map[string]time.Time{
			"gt-capable": at.Add(-time.Hour),
			"gt-old":     at.Add(-time.Hour),
		},
	}
	seats := poolSeatSnapshot{
		sessions: []poolSession{{name: "gt-capable", agent: "claude-sonnet", rig: "gastown", polecat: "capable", created: at.Add(-time.Hour)}},
		released: map[string]bool{"gastown/flash": true, "gastown/old": true},
		at:       at.Add(-time.Minute),
	}
	occupied := func() ([]poolSession, error) {
		return snapshotOccupied(seats, func() sessionLister { return lister }, nil, func() time.Time { return at })()
	}

	roster, err := specRosterOf(seats, nil, occupied)
	if err != nil {
		t.Fatal(err)
	}
	if got := roster.Live["claude-sonnet"]; got != 1 {
		t.Fatalf("roster live = %d, want 1: the roster drops the released seats it reads as free", got)
	}
	if got := roster.Draining["claude-sonnet"]; got != 1 {
		t.Fatalf("draining = %d, want 1: gt-flash is counted by the sling and not by the roster", got)
	}

	// The tick's total is the sling's total: the same occupancy, counted once.
	held, err := occupied()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := poolHeldCount(held), 2; got != want {
		t.Fatalf("sling-side count = %d, want %d (gt-capable and gt-flash; gt-old was released with a known creation time)", got, want)
	}
	if tick := roster.Live["claude-sonnet"] + roster.Draining["claude-sonnet"]; tick != poolHeldCount(held) {
		t.Fatalf("tick total = %d, sling total = %d: the pre-check must read what the sling refuses on", tick, poolHeldCount(held))
	}

	// A sling-side read that fails fails the roster: an uncounted sling is not
	// an empty one.
	failing := func() ([]poolSession, error) { return nil, errors.New("tmux down") }
	if _, err := specRosterOf(seats, nil, failing); err == nil {
		t.Fatal("a failed sling-side read must fail the roster, not count it as empty")
	}
}

// TestSpecDrainingSessions: draining is the held sessions whose seat the roster
// did not count. A seat is counted once, and a seat claim is not a held session.
func TestSpecDrainingSessions(t *testing.T) {
	t.Parallel()
	rosterSeats := []poolSession{
		{name: "gt-capable", agent: "claude-sonnet", rig: "gastown", polecat: "capable"},
		{name: "dead/gastown/revived", agent: "claude-sonnet", rig: "gastown", polecat: "revived", deadHooked: true},
		{name: "pool-claim/abc", agent: "claude-sonnet"},
	}
	cases := []struct {
		name string
		held []poolSession
		want []string
	}{
		{"a released session the roster dropped is draining", []poolSession{
			{name: "gt-capable", agent: "claude-sonnet", rig: "gastown", polecat: "capable"},
			{name: "gt-flash", agent: "claude-sonnet", rig: "gastown", polecat: "flash"},
		}, []string{"gt-flash"}},
		{"a dead seat the listing now shows live is counted once", []poolSession{
			{name: "gt-revived", agent: "claude-sonnet", rig: "gastown", polecat: "revived"},
		}, nil},
		{"a claim the roster holds is not drawn from the sling's listing", nil, nil},
		{"a session new since the roster read is counted", []poolSession{
			{name: "gt-new", agent: "deepseek-flash", rig: "gastown", polecat: "new"},
		}, []string{"gt-new"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, s := range specDrainingSessions(rosterSeats, tc.held) {
				got = append(got, s.name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("draining = %v, want %v", got, tc.want)
			}
		})
	}
}
