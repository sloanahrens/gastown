package cmd

import (
	"context"
	"testing"
)

// A sling that hooks a bead onto a seat that already existed (no polecat
// spawned) and then fails leaves the bead as it was before the sling. The hook
// is a claim this sling wrote, and with no spawn to roll back nothing else
// gives it back: without the hand-back the bead stays hooked or in_progress to
// a seat that never runs it, and the dispatcher never re-slings an in_progress
// bead (gt-moxlb).
func TestSlingOntoExistingSeatHandsBackHookAfterFailure(t *testing.T) {
	t.Parallel()
	const beadID = "gt-abc"
	const seat = "gastown/crew/sloan"

	for _, tc := range []struct {
		name string
		// original is the bead's status and assignee before the sling.
		original [2]string
		force    bool
		// thief is an actor that hooks the bead after this sling's hook write
		// landed and before the rollback runs; "" means nobody does.
		thief string
		want  [2]string
	}{
		{name: "fresh bead goes back to open and unassigned", original: [2]string{"open", ""},
			want: [2]string{"open", ""}},
		{name: "force-stolen bead goes back to its holder", original: [2]string{"hooked", "gastown/polecats/pearl"},
			force: true, want: [2]string{"hooked", "gastown/polecats/pearl"}},
		{name: "a bead another actor takes is left with that actor",
			original: [2]string{"open", ""}, thief: "gastown/polecats/granite",
			want: [2]string{"hooked", "gastown/polecats/granite"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(beadID, beadInfo{Status: tc.original[0], Assignee: tc.original[1]})
			// The hook write lands, then the step that confirms it fails — the
			// write a failed sling used to leave on the bead.
			h.run.hook = func(id, agent, _, _ string, _ *beadFieldUpdates) error {
				h.mu.Lock()
				if b := h.beads[id]; b != nil {
					b.Status, b.Assignee = "hooked", agent
				}
				h.mu.Unlock()
				if tc.thief != "" {
					h.mu.Lock()
					h.beads[id].Assignee = tc.thief
					h.mu.Unlock()
				}
				return errInjected
			}

			// The harness parses no flags: opts is the parsed command line.
			h.run.opts.force = tc.force
			err := h.sling(beadID, seat)
			if err == nil {
				t.Fatal("the sling must report the injected hook failure")
			}
			got := h.bead(beadID)
			if got.Status != tc.want[0] || got.Assignee != tc.want[1] {
				t.Fatalf("bead = %s/%s, want %s/%s (handed back to its pre-sling state)",
					got.Status, got.Assignee, tc.want[0], tc.want[1])
			}
			if tc.thief != "" {
				h.wantCalls("hand back", "hand back skipped "+beadID)
				return
			}
			h.wantCalls("hand back", "hand back "+beadID+" "+tc.want[0]+" "+tc.want[1])
			// No spawn, so the hand-back is the only rollback of the hook.
			h.wantCalls("rollback")
		})
	}
}

// A force re-sling that fails before its hook still gives the stolen bead back
// through the unhook restore (gt-ct2ob). The hand-back only covers a hook this
// sling wrote; until that write lands the bead is open and unassigned, which is
// the unhook restore's state.
func TestSlingOntoExistingSeatHandBackLeavesTheUnhookRestoreAlone(t *testing.T) {
	t.Parallel()
	const beadID = "gt-abc"
	const pearl = "gastown/polecats/pearl"

	h := newSlingHarness(t)
	h.addBead(beadID, beadInfo{Status: "hooked", Assignee: pearl})
	h.run.opts.force = true
	h.run.unhook = func(_, id string) error {
		h.mu.Lock()
		if b := h.beads[id]; b != nil {
			b.Status, b.Assignee = "open", ""
		}
		h.mu.Unlock()
		return nil
	}
	// A polecat target carries the auto-applied formula, so the sling fails
	// between the unhook and the hook write.
	h.run.instantiateFormula = func(_ context.Context, _, _, _, _, _ string, _ []string) (*FormulaOnBeadResult, error) {
		return nil, errInjected
	}

	if err := h.sling(beadID, "gastown/polecats/Toast"); err == nil {
		t.Fatal("the sling must report the injected formula failure")
	}
	if got := h.bead(beadID); got.Status != "hooked" || got.Assignee != pearl {
		t.Fatalf("bead = %s/%s, want it back with its holder %s/hooked", got.Status, got.Assignee, pearl)
	}
	if got := h.matching("hand back"); len(got) != 1 || got[0] != "hand back skipped "+beadID {
		t.Fatalf("hand back = %q, want the unhook restore to own this state", got)
	}
}

// The spawn path keeps its own rollback: a sling that spawns a polecat and
// fails after the hook still rolls the spawn back, which is what releases the
// bead (gt-7evi4). The hand-back is for the seat that already existed.
func TestSlingOntoExistingSeatHandBackLeavesSpawnRollbackAlone(t *testing.T) {
	t.Parallel()
	const beadID = "gt-abc"

	// A rig target dispatches on the engine; a named polecat resolves and
	// spawns in this package. Both own their spawn's rollback.
	for _, tc := range []struct {
		name   string
		target string
		spawn  bool
	}{
		{name: "engine dispatch", target: "gastown"},
		{name: "inline spawn", target: "gastown/polecats/Toast", spawn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(beadID, beadInfo{Status: "open"})
			h.run.hook = func(id, agent, _, _ string, _ *beadFieldUpdates) error { return errInjected }
			if tc.spawn {
				h.run.resolveTarget = func(string, ResolveTargetOptions) (*ResolvedTarget, error) {
					spawn := h.newSpawn("gastown")
					return &ResolvedTarget{Agent: spawn.AgentID(), WorkDir: spawn.ClonePath, NewPolecatInfo: spawn}, nil
				}
			}

			if err := h.sling(beadID, tc.target); err == nil {
				t.Fatal("the sling must report the injected hook failure")
			}
			h.wantCalls("rollback", "rollback Toast bead="+beadID)
			h.wantCalls("hand back")
		})
	}
}
