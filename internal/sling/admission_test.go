package sling

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// blockedBead is an open bead with the given dependency edges: what a show
// answers for a bead bd's ready board leaves off (gt-e3hl2).
func blockedBead(deps ...beads.IssueDep) *Bead {
	return &Bead{Status: "open", Dependencies: deps}
}

func blocks(id, status string) beads.IssueDep {
	return beads.IssueDep{ID: id, Status: status, DependencyType: "blocks"}
}

// TestRunRefusesABeadWithAnUnresolvedBlocker pins the admission refusal: the
// dispatch names each open blocker and its status, and it stops before the
// spawn, so nothing downstream of the policy — the branch push, the image
// check and the sandbox create a spawn makes — ever runs.
func TestRunRefusesABeadWithAnUnresolvedBlocker(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		deps []beads.IssueDep
		want []string // each must appear in the refusal
	}{
		{name: "open blocker", deps: []beads.IssueDep{blocks("gt-block", "open")}, want: []string{"gt-block (open)"}},
		{name: "deferred blocker", deps: []beads.IssueDep{blocks("gt-block", "deferred")}, want: []string{"gt-block (deferred)"}},
		{name: "in progress blocker", deps: []beads.IssueDep{blocks("gt-block", "in_progress")}, want: []string{"gt-block (in_progress)"}},
		{
			name: "two open blockers are both named",
			deps: []beads.IssueDep{blocks("gt-one", "open"), blocks("gt-two", "in_progress")},
			want: []string{"gt-one (open)", "gt-two (in_progress)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSlingDeps{reads: []holdTestRead{{bead: blockedBead(tc.deps...)}}}

			result, err := Run(context.Background(), f.deps(), holdTestOptions())
			if err == nil {
				t.Fatalf("a blocked bead was dispatched (result %+v)", result)
			}
			if !strings.Contains(err.Error(), dispatch.SlingRefusalMarker) {
				t.Errorf("err = %v, want the refusal marker %q: an automatic dispatcher must read this as a deferral, not a failed dispatch",
					err, dispatch.SlingRefusalMarker)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to name %q", err, want)
				}
			}
			if result == nil || result.ErrMsg != "blocked" {
				t.Errorf("result = %+v, want ErrMsg %q", result, "blocked")
			}
			if f.spawns != 0 {
				t.Errorf("spawned %d polecat(s) for a blocked bead, want 0", f.spawns)
			}
			if f.hooks != 0 || f.started != 0 {
				t.Errorf("hooks = %d, sessions = %d, want 0 of each for a blocked bead", f.hooks, f.started)
			}
			if len(f.overrides) != 0 {
				t.Errorf("wrote %d --ignore-blockers record(s) for a plain refusal, want 0: %v", len(f.overrides), f.overrides)
			}
		})
	}
}

// TestRunForceDoesNotOverrideBlockerAdmission: the automated redispatch paths
// pass --force for the guards they need, so a blocked bead that --force opened
// would be flung by every one of them. The override is the separate flag.
func TestRunForceDoesNotOverrideBlockerAdmission(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{reads: []holdTestRead{{bead: blockedBead(blocks("gt-block", "open"))}}}
	opts := holdTestOptions()
	opts.Force = true

	_, err := Run(context.Background(), f.deps(), opts)
	if err == nil || !strings.Contains(err.Error(), "gt-block (open)") {
		t.Fatalf("err = %v, want a refusal naming the blocker", err)
	}
	if f.spawns != 0 {
		t.Errorf("spawned %d polecat(s), want 0: --force must not open the blocker guard", f.spawns)
	}
}

// TestRunIgnoreBlockersDispatchesAndRecordsTheOverride: the flag is the
// override, and it dispatches the bead while leaving the record that says who
// went over which blockers. The record is written before the spawn, so a
// dispatch that fails halfway is still attributable.
func TestRunIgnoreBlockersDispatchesAndRecordsTheOverride(t *testing.T) {
	t.Parallel()
	deps := []beads.IssueDep{
		blocks("gt-block", "open"),
		blocks("gt-done", "closed"),
		blocks("gt-later", "deferred"),
	}
	f := &fakeSlingDeps{reads: []holdTestRead{{bead: blockedBead(deps...)}}}
	opts := holdTestOptions()
	opts.IgnoreBlockers = true

	result, err := Run(context.Background(), f.deps(), opts)
	if err != nil {
		t.Fatalf("--ignore-blockers did not dispatch the bead: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("result = %+v, want a successful dispatch", result)
	}
	if f.spawns != 1 || f.hooks != 1 {
		t.Errorf("spawns = %d, hooks = %d, want 1 of each", f.spawns, f.hooks)
	}
	want := []string{"daemon/spec-dispatch over gt-block (open), gt-later (deferred)"}
	if !slices.Equal(f.overrides, want) {
		t.Errorf("overrides = %v, want %v (the actor, and every unresolved blocker named; the closed one is not a blocker)", f.overrides, want)
	}
	if wantSeq := []string{"override", "spawn"}; !slices.Equal(f.admissionSequence, wantSeq) {
		t.Errorf("calls = %v, want %v: the record must be written before the spawn", f.admissionSequence, wantSeq)
	}
}

// TestRunIgnoreBlockersRecordsTheOperatorNotTheQueue: a queued sling is
// dispatched by the daemon, so the Actor dep names the daemon. The person who
// passed --ignore-blockers is the one the comment must name, and the operator
// arrives in its own field for exactly that reason (gt-e3hl2).
func TestRunIgnoreBlockersRecordsTheOperatorNotTheQueue(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{reads: []holdTestRead{{bead: blockedBead(blocks("gt-block", "open"))}}}
	opts := holdTestOptions()
	opts.IgnoreBlockers = true
	opts.BlockerOverrideActor = "gastown/crew/overseer"

	if _, err := Run(context.Background(), f.deps(), opts); err != nil {
		t.Fatalf("--ignore-blockers did not dispatch the bead: %v", err)
	}
	want := []string{"gastown/crew/overseer over gt-block (open)"}
	if !slices.Equal(f.overrides, want) {
		t.Errorf("overrides = %v, want %v: the comment names the operator, not the daemon that ran the queue", f.overrides, want)
	}
}

// TestRunIgnoreBlockersRefusesWhenTheOverrideCannotBeRecorded: the comment is
// the condition the override is granted on, so a dispatch that could not leave
// one is refused rather than slung over blockers unrecorded.
func TestRunIgnoreBlockersRefusesWhenTheOverrideCannotBeRecorded(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{
		reads:       []holdTestRead{{bead: blockedBead(blocks("gt-block", "open"))}},
		overrideErr: errors.New("bd comments add: database is read only"),
	}
	opts := holdTestOptions()
	opts.IgnoreBlockers = true

	_, err := Run(context.Background(), f.deps(), opts)
	if err == nil || !strings.Contains(err.Error(), "database is read only") {
		t.Fatalf("err = %v, want the record's failure", err)
	}
	if f.spawns != 0 || f.hooks != 0 {
		t.Errorf("spawns = %d, hooks = %d, want 0 of each: the dispatch has no record to stand on", f.spawns, f.hooks)
	}
}

// TestRunTakesABeadWhoseBlockersAreClosed: admission refuses unresolved edges,
// not the presence of the dependency list, so work whose blockers landed
// dispatches as it always did.
func TestRunTakesABeadWhoseBlockersAreClosed(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{reads: []holdTestRead{{bead: blockedBead(blocks("gt-done", "closed"))}}}

	result, err := Run(context.Background(), f.deps(), holdTestOptions())
	if err != nil {
		t.Fatalf("a bead with only closed blockers was refused: %v", err)
	}
	if f.spawns != 1 || f.hooks != 1 || result == nil || !result.Success {
		t.Fatalf("spawns = %d, hooks = %d, result %+v, want the dispatch to take the bead", f.spawns, f.hooks, result)
	}
	if len(f.overrides) != 0 {
		t.Errorf("wrote %d --ignore-blockers record(s) with nothing to override: %v", len(f.overrides), f.overrides)
	}
}
