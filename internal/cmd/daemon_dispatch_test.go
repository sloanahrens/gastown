package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func TestDispatchDecision_NudgesWhenSeatsAreFreeAndWorkExists(t *testing.T) {
	// The acceptance case: seats free, and a single actionable ready bead.
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 0, Free: 4}
	rigs := []dispatchRig{{Rig: "gastown", Ready: 1}}

	nudge, msg := dispatchDecision(seats, rigs)

	if !nudge {
		t.Fatalf("expected a nudge with free seats and 1 ready bead, got silence")
	}
	// The nudge's contract: free seats and per-rig counts, both named.
	for _, want := range []string{"4 of 4", "gastown=1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("nudge text does not name %q: %s", want, msg)
		}
	}
}

func TestDispatchDecision_SilentWhenNoSeatIsFree(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 4, Free: 0}
	rigs := []dispatchRig{{Rig: "gastown", Ready: 5}}

	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence with every seat taken, got nudge: %s", msg)
	}
}

func TestDispatchDecision_SilentWhenNoWorkIsReady(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 3, Free: 1}
	rigs := []dispatchRig{{Rig: "gastown", Ready: 0}, {Rig: "beads", Ready: 0}}

	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence with no ready work, got nudge: %s", msg)
	}
}

func TestDispatchDecision_SilentWhenNoSeatModelIsConfigured(t *testing.T) {
	// A town with neither a pool nor scheduler.max_polecats has no answer to
	// "is a seat free?", so the patrol has nothing to report.
	seats := dispatchSeats{Source: "none"}
	rigs := []dispatchRig{{Rig: "gastown", Ready: 5}}

	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence with no seat model, got nudge: %s", msg)
	}
}

func TestDispatchDecision_BackpressuredRigIsNamedButNotCounted(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 2, Free: 2}
	rigs := []dispatchRig{
		{Rig: "om", Ready: 9, ReadyMRs: 15, MRCeiling: 12, Backpressure: true},
	}

	// Only the held rig has work: silence, because the work could not land.
	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence when every rig with work is over its ceiling, got: %s", msg)
	}

	// A second rig with work keeps the nudge, and the held rig is named with
	// its MR depth so the mayor can tell "empty board" from "board held".
	rigs = append(rigs, dispatchRig{Rig: "gastown", Ready: 4})
	nudge, msg := dispatchDecision(seats, rigs)
	if !nudge {
		t.Fatal("expected a nudge when a rig outside its ceiling has work")
	}
	if !strings.Contains(msg, "Held by merge-queue depth: om=15 ready MRs (ceiling 12)") {
		t.Errorf("nudge does not say which rig is held and why: %s", msg)
	}
	if strings.Contains(msg, "om=9") {
		t.Errorf("held rig's ready beads must not read as dispatchable work: %s", msg)
	}
}

func TestDispatchDecision_NamesUrgentSubsetOnlyWhenNonZero(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 0, Free: 4}

	_, msg := dispatchDecision(seats, []dispatchRig{{Rig: "gastown", Ready: 200, Urgent: 0}})
	if strings.Contains(msg, "P0/P1") {
		t.Errorf("a rig with no P0/P1 should not carry an empty urgent clause: %s", msg)
	}

	_, msg = dispatchDecision(seats, []dispatchRig{{Rig: "gastown", Ready: 200, Urgent: 2}})
	if !strings.Contains(msg, "gastown=200 (P0/P1 2)") {
		t.Errorf("nudge should name the urgent subset when there is one: %s", msg)
	}
}

func TestDispatchDecision_SkipsParkedRigs(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 0, Free: 4}
	rigs := []dispatchRig{{Rig: "mango", Ready: 7, Parked: true}}

	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence: the only rig with work is parked, got: %s", msg)
	}
}

func TestIsActionableReadyBead(t *testing.T) {
	cases := []struct {
		name  string
		issue *beads.Issue
		want  bool
	}{
		{"plain P2 task", &beads.Issue{ID: "gt-1", Title: "fix the thing", Priority: 2}, true},
		{"plain P0 bug", &beads.Issue{ID: "gt-2", Title: "[bug] broken", Priority: 0}, true},
		{"nil issue", nil, false},
		{"P3 backlog", &beads.Issue{ID: "gt-3", Title: "someday", Priority: 3}, false},
		{"unset priority", &beads.Issue{ID: "gt-4", Title: "unscored", Priority: -1}, false},
		{
			"agent bead",
			&beads.Issue{ID: "gt-gastown-polecat-flint", Title: "gt-gastown-polecat-flint", Priority: 2, Labels: []string{"gt:agent"}},
			false,
		},
		{
			"merge slot",
			&beads.Issue{ID: "gt-zdcv", Title: "merge-slot", Priority: 0, Labels: []string{"gt:merge-slot"}},
			false,
		},
		{
			"escalation",
			&beads.Issue{ID: "gt-5", Title: "[HIGH] disk full", Priority: 1, Labels: []string{"gt:escalation"}},
			false,
		},
		{
			"escalation titled without its label",
			&beads.Issue{ID: "gt-6", Title: "[CRITICAL] dolt is down", Priority: 0},
			false,
		},
		{
			"state collapse",
			&beads.Issue{ID: "gt-7", Title: "STATE_COLLAPSE gt-abc closed, MR still open", Priority: 1},
			false,
		},
		{
			"epic container",
			&beads.Issue{ID: "gt-8", Title: "[epic] de-flake the suite", Priority: 1, Type: "epic"},
			false,
		},
		{
			"label case and padding",
			&beads.Issue{ID: "gt-9", Title: "agent-ish", Priority: 2, Labels: []string{" GT:AGENT "}},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isActionableReadyBead(tc.issue); got != tc.want {
				t.Errorf("isActionableReadyBead = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPoolSeatPicture(t *testing.T) {
	now := time.Now()
	pool := &config.PolecatPool{
		LocalAgent:    "deepseek-flash",
		MaxLocal:      2,
		OverflowAgent: "flash-overflow",
		MaxOverflow:   2,
	}
	sessions := []poolSession{
		{name: "gastown/flint", agent: "deepseek-flash", created: now.Add(-time.Hour)},
		{name: "gastown/jade", agent: "deepseek-flash", created: now.Add(-time.Minute)},
		{name: "beads/fury", agent: "flash-overflow", created: now.Add(-time.Hour)},
	}

	seats := poolSeatPicture(pool, sessions)
	if seats.Source != "polecat_pool" {
		t.Errorf("source = %q, want polecat_pool", seats.Source)
	}
	if seats.Capacity != 4 || seats.Occupied != 3 || seats.Free != 1 {
		t.Errorf("seats = %+v, want capacity 4, occupied 3, free 1", seats)
	}

	// No pool at all: the model has nothing to say, and says so with an empty
	// source rather than a zero-seat town.
	if got := poolSeatPicture(nil, sessions); got.Source != "" {
		t.Errorf("nil pool should leave Source empty, got %+v", got)
	}

	// A pool with no local_agent is a pool that was never configured.
	if got := poolSeatPicture(&config.PolecatPool{MaxLocal: 2}, sessions); got.Source != "" {
		t.Errorf("pool without local_agent should leave Source empty, got %+v", got)
	}
}

func TestPoolSeatPicture_UncappedOverflowIsAtLeastOneFreeSeat(t *testing.T) {
	// An overflow seat with no cap is unbounded room: the pool never refuses a
	// spawn, so a full local pool must not read as a town with no seat free.
	pool := &config.PolecatPool{
		LocalAgent:    "local",
		MaxLocal:      1,
		OverflowAgent: "overflow", // MaxOverflow 0 = uncapped
	}
	sessions := []poolSession{{name: "gastown/flint", agent: "local"}}

	seats := poolSeatPicture(pool, sessions)
	if !seats.Uncapped {
		t.Fatalf("expected Uncapped for an unbounded overflow seat, got %+v", seats)
	}
	if seats.Free < 1 {
		t.Errorf("free = %d, want at least 1 while the overflow seat is uncapped", seats.Free)
	}
}
