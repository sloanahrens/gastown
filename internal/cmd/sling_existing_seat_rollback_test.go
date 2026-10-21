package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// lostReplyStore is a hook store whose connection dies mid-write: Update
// either applies the write and then reports the dead connection (commit), or
// reports it without touching the bead. Reads keep answering, as they do once
// the caller reconnects.
type lostReplyStore struct {
	slingFake
	h      *slingHarness
	commit bool
}

func (s *lostReplyStore) Show(id string) (*beads.Issue, error) {
	b := s.h.bead(id)
	if b == nil {
		return nil, fmt.Errorf("bead '%s' not found", id)
	}
	return &beads.Issue{ID: id, Title: b.Title, Status: b.Status, Assignee: b.Assignee, Description: b.Description}, nil
}

func (s *lostReplyStore) Update(id string, opts beads.UpdateOptions) error {
	if s.commit {
		s.h.mu.Lock()
		if b := s.h.beads[id]; b != nil {
			if opts.Status != nil {
				b.Status = *opts.Status
			}
			if opts.Assignee != nil {
				b.Assignee = *opts.Assignee
			}
			if opts.Description != nil {
				b.Description = *opts.Description
			}
		}
		s.h.mu.Unlock()
	}
	return fmt.Errorf("bd update %s: connection refused", id)
}

// A hook write whose reply was lost is read as not landed unless the bead is
// re-read: bd commits the update, the connection dies before the reply, and
// the next attempt fails fast on the dead connection. For a seat that already
// existed the hand-back was then skipped, leaving the bead hooked to a crew
// seat that never ran it (gt-lanz0).
func TestSlingOntoExistingSeatRollsBackALostHookReply(t *testing.T) {
	t.Parallel()
	const beadID = "gt-abc"
	const seat = "gastown/crew/sloan"

	for _, tc := range []struct {
		name string
		// commit is whether bd applied the write before the reply was lost.
		commit bool
		// wantHandBack is whether the rollback must hand the bead back.
		wantHandBack bool
	}{
		{name: "a write that committed and lost its reply is handed back", commit: true, wantHandBack: true},
		{name: "a write that never landed leaves the bead alone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(beadID, beadInfo{Status: "open"})
			store := &lostReplyStore{slingFake: slingFake{beadsfake.New()}, h: h, commit: tc.commit}
			stores := slingStores{pinned: func(string) slingStore { return store }}
			// The hook runs the real hookWithRetry, so the re-read that tells a
			// landed write from an unlanded one is the code under test.
			h.run.hook = func(id, agent, hookDir, _ string, fields *beadFieldUpdates) error {
				return stores.hookWithRetry(nil, id, agent, hookDir, fields)
			}

			if err := h.sling(beadID, seat); err == nil {
				t.Fatal("the sling must report the lost hook reply")
			}
			if got := h.bead(beadID); got.Status != "open" || got.Assignee != "" {
				t.Fatalf("bead = %s/%s, want it left open and unassigned", got.Status, got.Assignee)
			}
			if tc.wantHandBack {
				h.wantCalls("restore bead", "restore bead "+beadID+" open ")
			} else {
				h.wantNo("restore bead")
			}
			h.wantCalls("restore raw fields", "restore raw fields "+beadID)
			h.wantCalls("rollback")
		})
	}
}

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
		// thief is what another actor does to the bead after this sling's hook
		// write landed and before the rollback runs; nil means nobody does.
		thief func(b *beadInfo)
		want  [2]string
	}{
		{name: "fresh bead goes back to open and unassigned", original: [2]string{"open", ""},
			want: [2]string{"open", ""}},
		{name: "force-stolen bead goes back to its holder", original: [2]string{"hooked", "gastown/polecats/pearl"},
			force: true, want: [2]string{"hooked", "gastown/polecats/pearl"}},
		{name: "a bead another actor claims is left with that actor",
			original: [2]string{"open", ""}, thief: func(b *beadInfo) { b.Status, b.Assignee = "hooked", "gastown/polecats/granite" },
			want: [2]string{"hooked", "gastown/polecats/granite"}},
		// Another sling's force unhook leaves open/unassigned, the state the
		// unhook restore is guarded on; restoring the old holder here would
		// take the bead back from the sling that unhooked it (gt-moxlb).
		{name: "a bead another sling unhooks is left alone",
			original: [2]string{"hooked", "gastown/polecats/pearl"}, force: true,
			thief: func(b *beadInfo) { b.Status, b.Assignee = "open", "" },
			want:  [2]string{"open", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(beadID, beadInfo{Status: tc.original[0], Assignee: tc.original[1]})
			// The hook write lands, then the step that confirms it fails — the
			// write a failed sling used to leave on the bead. The typed error is
			// what hookWithRetry reports once a write reached bd.
			h.run.hook = func(id, agent, _, _ string, _ *beadFieldUpdates) error {
				h.mu.Lock()
				if b := h.beads[id]; b != nil {
					b.Status, b.Assignee = "hooked", agent
					if tc.thief != nil {
						tc.thief(b)
					}
				}
				h.mu.Unlock()
				return &hookWriteLandedError{errInjected}
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
			// The real hand-back prints and writes through the harness
			// releaser; a declined one records nothing.
			if tc.thief != nil {
				h.wantCalls("restore bead")
			} else {
				h.wantCalls("restore bead", "restore bead "+beadID+" "+tc.want[0]+" "+tc.want[1])
			}
			h.wantCalls("restore raw fields", "restore raw fields "+beadID)
			if tc.force {
				// The force unhook cleared the outgoing polecat's agent bead;
				// the same rollback puts it back (gt-u0zq0).
				h.wantCalls("restore reassigned", "restore reassigned gastown/polecats/pearl")
			}
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
	// The sling never reaches its hook write, so the unhook restore owns the
	// state and the hand-back is not tried.
	h.wantCalls("restore bead")
	h.wantCalls("restore hold", "restore hold "+beadID+" hooked "+pearl)
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
			h.wantCalls("restore bead")
		})
	}
}
