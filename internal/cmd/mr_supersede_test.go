package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// fakeMRStore is a mrSupersedeStore plus the clear calls the supersede loop is
// expected to make, without a Dolt server.
type fakeMRStore struct {
	open      []*beads.Issue
	findErr   error
	closeErr  map[string]error
	closed    []string
	closeArgs []string
}

func (f *fakeMRStore) FindOpenMRsForIssue(string) ([]*beads.Issue, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.open, nil
}

func (f *fakeMRStore) CloseWithReason(reason string, ids ...string) error {
	for _, id := range ids {
		if err := f.closeErr[id]; err != nil {
			return err
		}
		f.closed = append(f.closed, id)
		f.closeArgs = append(f.closeArgs, reason)
	}
	return nil
}

type fakeAgentClearer struct {
	// pointers maps agent bead ID -> the MR its active_mr names.
	pointers map[string]string
	calls    []string
	err      error
}

func (f *fakeAgentClearer) ClearAgentActiveMRIfMatches(id, expectedMR string) (bool, error) {
	f.calls = append(f.calls, id+"="+expectedMR)
	if f.err != nil {
		return false, f.err
	}
	if f.pointers[id] != expectedMR {
		return false, nil
	}
	delete(f.pointers, id)
	return true, nil
}

func mrFixture(id, description string) *beads.Issue {
	return &beads.Issue{ID: id, Status: string(beads.StatusOpen), Description: description}
}

// townWithRoute writes the town-level routes file GetPrefixForRig reads, so
// the worker-derived agent bead id is exercised for real.
func townWithRoute(t *testing.T, rig, prefix string) string {
	t.Helper()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	line := `{"prefix":"` + prefix + `-","path":"` + rig + `/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	return townRoot
}

// TestSupersedeOpenMRsClearsSupersededWorkersActiveMR is the gt-c5uv
// regression: a newer submission closes the old MR *and* clears the active_mr
// pointer on the agent bead of the polecat that submitted it — a different
// polecat than the submitter, which is why nothing else does it.
func TestSupersedeOpenMRsClearsSupersededWorkersActiveMR(t *testing.T) {
	townRoot := townWithRoute(t, "gastown", "gt")
	store := &fakeMRStore{open: []*beads.Issue{
		mrFixture("gt-wisp-new", "branch: polecat/nux/gt-8ib\ntarget: main\nsource_issue: gt-8ib\nrig: gastown\nagent_bead: gt-gastown-polecat-nux\nworker: nux"),
		mrFixture("gt-wisp-old", "branch: polecat/furiosa/gt-8ib\ntarget: main\nsource_issue: gt-8ib\nrig: gastown\nagent_bead: gt-gastown-polecat-furiosa\nworker: furiosa"),
	}}
	agents := &fakeAgentClearer{pointers: map[string]string{"gt-gastown-polecat-furiosa": "gt-wisp-old"}}

	got := supersedeOpenMRsForIssue(store, agents, "gt-8ib", "gt-wisp-new", townRoot, "gastown")

	if len(got) != 1 || got[0].ID != "gt-wisp-old" {
		t.Fatalf("superseded = %+v, want exactly gt-wisp-old", got)
	}
	if got[0].AgentBead != "gt-gastown-polecat-furiosa" || !got[0].AgentCleared {
		t.Fatalf("superseded[0] = %+v, want furiosa's bead cleared", got[0])
	}
	if len(store.closed) != 1 || store.closed[0] != "gt-wisp-old" {
		t.Fatalf("closed = %v, want [gt-wisp-old]", store.closed)
	}
	if store.closeArgs[0] != "superseded by gt-wisp-new" {
		t.Fatalf("close reason = %q, want %q", store.closeArgs[0], "superseded by gt-wisp-new")
	}
	if agents.pointers["gt-gastown-polecat-furiosa"] != "" {
		t.Fatalf("furiosa's active_mr still set: %q", agents.pointers["gt-gastown-polecat-furiosa"])
	}
}

// TestSupersedeOpenMRsDerivesAgentBeadFromWorker covers MRs with no agent_bead
// field (gt mq submit writes only the branch's worker name).
func TestSupersedeOpenMRsDerivesAgentBeadFromWorker(t *testing.T) {
	townRoot := townWithRoute(t, "gastown", "gt")
	store := &fakeMRStore{open: []*beads.Issue{
		mrFixture("gt-wisp-new", "branch: polecat/nux/gt-8ib\nrig: gastown"),
		mrFixture("gt-wisp-old", "branch: polecat/furiosa/gt-8ib\nrig: gastown\nworker: furiosa"),
	}}
	agents := &fakeAgentClearer{pointers: map[string]string{"gt-gastown-polecat-furiosa": "gt-wisp-old"}}

	got := supersedeOpenMRsForIssue(store, agents, "gt-8ib", "gt-wisp-new", townRoot, "gastown")

	if len(got) != 1 || got[0].AgentBead != "gt-gastown-polecat-furiosa" || !got[0].AgentCleared {
		t.Fatalf("superseded = %+v, want furiosa's derived bead cleared", got)
	}
}

// TestSupersedeOpenMRsFallsBackToTheMRsOwnRig: an MR that names its rig is
// resolved from that rig, not from the caller's.
func TestSupersedeOpenMRsFallsBackToTheMRsOwnRig(t *testing.T) {
	townRoot := townWithRoute(t, "elsewhere", "el")
	store := &fakeMRStore{open: []*beads.Issue{
		mrFixture("gt-wisp-new", "branch: polecat/nux/gt-8ib\nrig: elsewhere"),
		mrFixture("gt-wisp-old", "branch: polecat/furiosa/gt-8ib\nrig: elsewhere\nworker: furiosa"),
	}}
	agents := &fakeAgentClearer{pointers: map[string]string{"el-elsewhere-polecat-furiosa": "gt-wisp-old"}}

	got := supersedeOpenMRsForIssue(store, agents, "gt-8ib", "gt-wisp-new", townRoot, "gastown")

	if len(got) != 1 || got[0].AgentBead != "el-elsewhere-polecat-furiosa" || !got[0].AgentCleared {
		t.Fatalf("superseded = %+v, want el-elsewhere-polecat-furiosa cleared", got)
	}
}

// TestSupersedeOpenMRsLeavesAMovedPointerAlone: if the superseded worker has
// since submitted something else, its active_mr must not be clobbered.
func TestSupersedeOpenMRsLeavesAMovedPointerAlone(t *testing.T) {
	store := &fakeMRStore{open: []*beads.Issue{
		mrFixture("gt-wisp-new", "branch: polecat/nux/gt-8ib\nrig: gastown"),
		mrFixture("gt-wisp-old", "branch: polecat/furiosa/gt-8ib\nrig: gastown\nagent_bead: gt-gastown-polecat-furiosa\nworker: furiosa"),
	}}
	agents := &fakeAgentClearer{pointers: map[string]string{"gt-gastown-polecat-furiosa": "gt-wisp-newer"}}

	got := supersedeOpenMRsForIssue(store, agents, "gt-8ib", "gt-wisp-new", "", "gastown")

	if len(got) != 1 || got[0].AgentCleared {
		t.Fatalf("superseded = %+v, want the stale pointer left for its owner to fix", got)
	}
	if agents.pointers["gt-gastown-polecat-furiosa"] != "gt-wisp-newer" {
		t.Fatalf("active_mr = %q, want gt-wisp-newer untouched", agents.pointers["gt-gastown-polecat-furiosa"])
	}
}

// TestSupersedeOpenMRsWithoutReplacementClosesNothing: superseding with no new
// MR in hand would empty the queue for that issue.
func TestSupersedeOpenMRsWithoutReplacementClosesNothing(t *testing.T) {
	store := &fakeMRStore{open: []*beads.Issue{mrFixture("gt-wisp-old", "source_issue: gt-8ib\nworker: furiosa")}}
	agents := &fakeAgentClearer{pointers: map[string]string{"gt-gastown-polecat-furiosa": "gt-wisp-old"}}

	if got := supersedeOpenMRsForIssue(store, agents, "gt-8ib", "", "", "gastown"); got != nil {
		t.Fatalf("superseded = %+v, want none", got)
	}
	if len(store.closed) != 0 || len(agents.calls) != 0 {
		t.Fatalf("closed = %v, clear calls = %v, want no writes", store.closed, agents.calls)
	}
}

// TestSupersedeOpenMRsSurvivesCloseAndClearFailures: a submission that
// succeeded must not be failed by queue hygiene, and one bad old MR must not
// stop the others from being superseded.
func TestSupersedeOpenMRsSurvivesCloseAndClearFailures(t *testing.T) {
	townRoot := townWithRoute(t, "gastown", "gt")
	store := &fakeMRStore{
		open: []*beads.Issue{
			mrFixture("gt-wisp-new", "rig: gastown"),
			mrFixture("gt-wisp-stuck", "rig: gastown\nworker: furiosa"),
			mrFixture("gt-wisp-old", "rig: gastown\nworker: nux"),
		},
		closeErr: map[string]error{"gt-wisp-stuck": errors.New("bd exploded")},
	}
	agents := &fakeAgentClearer{err: errors.New("dolt unreachable")}

	got := supersedeOpenMRsForIssue(store, agents, "gt-8ib", "gt-wisp-new", townRoot, "gastown")

	if len(got) != 1 || got[0].ID != "gt-wisp-old" || got[0].AgentCleared {
		t.Fatalf("superseded = %+v, want only gt-wisp-old with no clear", got)
	}
	if len(store.closed) != 1 {
		t.Fatalf("closed = %v, want only the closable MR", store.closed)
	}
	if !strings.Contains(strings.Join(agents.calls, ","), "gt-gastown-polecat-nux=gt-wisp-old") {
		t.Fatalf("clear calls = %v, want nux's derived bead attempted", agents.calls)
	}
}

// TestSupersedeOpenMRsLookupFailureIsQuiet: an unreadable queue is not a
// reason to fail the submission that already landed.
func TestSupersedeOpenMRsLookupFailureIsQuiet(t *testing.T) {
	store := &fakeMRStore{findErr: errors.New("queue unreadable")}
	agents := &fakeAgentClearer{}

	if got := supersedeOpenMRsForIssue(store, agents, "gt-8ib", "gt-wisp-new", "", "gastown"); got != nil {
		t.Fatalf("superseded = %+v, want none", got)
	}
	if len(store.closed) != 0 || len(agents.calls) != 0 {
		t.Fatalf("closed = %v, clear calls = %v, want no writes", store.closed, agents.calls)
	}
}

func TestSupersededMRAgentBeadResolution(t *testing.T) {
	townRoot := townWithRoute(t, "gastown", "gt")
	tests := []struct {
		name     string
		mr       *beads.Issue
		fallback string
		want     string
	}{
		{
			name: "recorded agent_bead wins",
			mr:   mrFixture("gt-wisp-1", "agent_bead: gt-gastown-polecat-furiosa\nworker: nux\nrig: gastown"),
			want: "gt-gastown-polecat-furiosa",
		},
		{
			name: "worker and rig derive a polecat bead",
			mr:   mrFixture("gt-wisp-2", "worker: furiosa\nrig: gastown"),
			want: "gt-gastown-polecat-furiosa",
		},
		{
			name:     "rig falls back to the caller's",
			mr:       mrFixture("gt-wisp-3", "worker: furiosa"),
			fallback: "gastown",
			want:     "gt-gastown-polecat-furiosa",
		},
		{
			name: "no worker is unresolvable",
			mr:   mrFixture("gt-wisp-4", "branch: main"),
			want: "",
		},
		{
			name: "no rig is unresolvable",
			mr:   mrFixture("gt-wisp-5", "worker: furiosa"),
			want: "",
		},
		{
			name: "a nil MR is unresolvable",
			mr:   nil,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := supersededMRAgentBead(tc.mr, townRoot, tc.fallback); got != tc.want {
				t.Fatalf("supersededMRAgentBead = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSupersededMRAgentBeadUnroutedRigUsesTheTownPrefixFallback: a rig the
// town has no prefix for still yields a well-formed guess, because
// GetPrefixForRig falls back to "gt". The guess need not be right — the caller
// clears through a match-and-clear, so an id that names no (or someone else's)
// bead clears nothing. See
// TestSupersedeOpenMRsLeavesAMovedPointerAlone.
func TestSupersededMRAgentBeadUnroutedRigUsesTheTownPrefixFallback(t *testing.T) {
	mr := mrFixture("gt-wisp-1", "worker: furiosa\nrig: nowhere")
	if got := supersededMRAgentBead(mr, t.TempDir(), "gastown"); got != "gt-nowhere-polecat-furiosa" {
		t.Fatalf("supersededMRAgentBead = %q, want gt-nowhere-polecat-furiosa", got)
	}
}
