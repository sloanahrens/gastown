package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// fakeWorkReleaser is an in-memory polecatWorkReleaser: bead -> (status, assignee).
type fakeWorkReleaser struct {
	beads    map[string][2]string
	readErr  error
	released []string
	resets   []string
}

func (f *fakeWorkReleaser) HookState(beadID string) (string, string, error) {
	if f.readErr != nil {
		return "", "", f.readErr
	}
	b, ok := f.beads[beadID]
	if !ok {
		return "", "", errors.New("not found")
	}
	return b[0], b[1], nil
}

func (f *fakeWorkReleaser) ReleaseBead(beadID string) error {
	f.released = append(f.released, beadID)
	f.beads[beadID] = [2]string{"open", ""}
	return nil
}

func (f *fakeWorkReleaser) ResetSlot(agentID string) error {
	f.resets = append(f.resets, agentID)
	return nil
}

// --- releasePolecatWork: the shared compare-and-release helper --------------

func TestReleasePolecatWorkComparesAssigneeBeforeRelease(t *testing.T) {
	const me = "gastown/polecats/basalt"
	cases := []struct {
		name         string
		status, who  string
		readErr      error
		wantReleased bool
	}{
		{name: "hooked to this polecat", status: "hooked", who: me, wantReleased: true},
		{name: "in_progress on this polecat", status: "in_progress", who: me, wantReleased: true},
		{name: "re-slung to another polecat", status: "hooked", who: "gastown/polecats/granite"},
		{name: "closed", status: "closed", who: me},
		{name: "already open", status: "open", who: ""},
		{name: "unreadable", readErr: errors.New("dolt down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {tc.status, tc.who}}, readErr: tc.readErr}
			out := releasePolecatWork(rel, me, "gt-elvf4", false)
			if out.Released != tc.wantReleased || (len(rel.released) == 1) != tc.wantReleased {
				t.Fatalf("released = %v (%v), want %v", out.Released, rel.released, tc.wantReleased)
			}
			if !tc.wantReleased && out.SkipNote == "" {
				t.Fatal("a skipped release must say why")
			}
			if len(rel.resets) != 0 {
				t.Fatalf("resetSlot=false must not reset the slot, got %v", rel.resets)
			}
		})
	}
}

func TestReleasePolecatWorkResetsSlotOnlyWhenAsked(t *testing.T) {
	rel := &fakeWorkReleaser{beads: map[string][2]string{}}
	out := releasePolecatWork(rel, "gastown/polecats/basalt", "", true)
	if !out.SlotReset || len(rel.resets) != 1 || rel.resets[0] != "gastown/polecats/basalt" {
		t.Fatalf("slot reset = %v %v", out.SlotReset, rel.resets)
	}
	if len(rel.released) != 0 {
		t.Fatalf("no bead named, nothing to release: %v", rel.released)
	}
}

// --- gt polecat nuke (gt-vm5g4) ---------------------------------------------

func TestNukeReleaseHookedWorkOnlyWhileAssigneeMatches(t *testing.T) {
	p := &polecat.Polecat{Name: "basalt", Rig: "gastown", Issue: "gt-elvf4"}

	t.Run("still hooked to the nuked polecat", func(t *testing.T) {
		rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {"hooked", "gastown/polecats/basalt"}}}
		out := nukeReleaseHookedWork(rel, "gastown", p)
		if !out.Released || len(rel.released) != 1 {
			t.Fatalf("nuke must release its hooked bead: %+v %v", out, rel.released)
		}
		if len(rel.resets) != 0 {
			t.Fatal("nuke leaves the agent-bead reset to sandbox removal")
		}
	})
	t.Run("already re-slung elsewhere", func(t *testing.T) {
		rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {"hooked", "gastown/polecats/granite"}}}
		if out := nukeReleaseHookedWork(rel, "gastown", p); out.Released || len(rel.released) != 0 {
			t.Fatalf("nuke must not unassign another polecat's bead: %+v", out)
		}
	})
	t.Run("no hooked work", func(t *testing.T) {
		rel := &fakeWorkReleaser{readErr: errors.New("must not be read")}
		if out := nukeReleaseHookedWork(rel, "gastown", &polecat.Polecat{Name: "basalt", Rig: "gastown"}); out != (workReleaseOutcome{}) {
			t.Fatalf("no issue, no work: %+v", out)
		}
	})
}
