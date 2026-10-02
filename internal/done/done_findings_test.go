package done

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/role"
)

// TestResolveDoneAgentIdentityKeepsSeededPolecatWhenDetectionIsUnnamed pins
// gt-638go.14 finding 2: a detection that names no role must refine the seeded
// identity, never replace it. The CLI layer reports RoleUnknown with
// named=false for a failed or deleted GT_ROLE; overwriting the seeded
// RolePolecat with Unknown would blank Agent.BeadID() and skip every
// agent-bead write (completion metadata, active_mr, cleanup_status and the
// hooked-bead close), stranding the slot.
func TestResolveDoneAgentIdentityKeepsSeededPolecatWhenDetectionIsUnnamed(t *testing.T) {
	t.Parallel()
	detect := func(cwd, townRoot string, getenv func(string) string) (Agent, string, bool) {
		return Agent{Role: role.Unknown, Rig: "gastown", Polecat: "refuge", TownRoot: townRoot, WorkDir: cwd}, "", false
	}
	ctx, actor := resolveDoneAgentIdentity(detect, func(string) string { return "" }, "/w", "/town", "gastown", "refuge")

	if ctx.Role != role.Polecat {
		t.Fatalf("role = %q, want %q: the seeded polecat role must survive an unnamed detection", ctx.Role, role.Polecat)
	}
	if ctx.BeadID() == "" {
		t.Error("BeadID() is empty; every agent-bead write in gt done would be skipped")
	}
	if actor != "" {
		t.Errorf("actor = %q, want empty: an unnamed detection contributes no log actor", actor)
	}
}

// assignedLister is an issueLister returning a fixed set for the hooked status
// and nothing otherwise.
type assignedLister struct{ issues []*beads.Issue }

func (l assignedLister) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if opts.Status == beads.StatusHooked {
		return l.issues, nil
	}
	return nil, nil
}

// TestFindAssignedBeadsForAgentFindsAssignmentOnlyInTownBeads pins
// gt-638go.14 finding 3: the lookup keeps every fallback gt hook has. An
// assignment filed only in the town .beads store is still found after the
// caller's workdir and the rig's mayor/rig directory come back empty.
func TestFindAssignedBeadsForAgentFindsAssignmentOnlyInTownBeads(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0o755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	workDir := filepath.Join(townRoot, "gastown", "polecats", "refuge", "gastown")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	const agent = "gastown/polecats/refuge"
	open := func(dir string) issueLister {
		if filepath.Clean(dir) == filepath.Clean(townBeadsDir) {
			return assignedLister{issues: []*beads.Issue{{ID: "gt-town-only", Assignee: agent, Status: beads.StatusHooked}}}
		}
		return assignedLister{}
	}

	got := findAssignedBeadsForAgentIn(workDir, townRoot, agent, open)
	if len(got) != 1 || got[0] != "gt-town-only" {
		t.Fatalf("assigned = %v, want [gt-town-only] from the town beads fallback", got)
	}
}

// fakeStashGit is the branchStashGit popBranchStashes reads.
type fakeStashGit struct {
	entries []git.StashEntry
	popErr  error
	dirty   bool
}

func (f *fakeStashGit) StashListForBranch() ([]git.StashEntry, error) { return f.entries, nil }

func (f *fakeStashGit) StashPop(string) error { return f.popErr }

func (f *fakeStashGit) CheckUncommittedWork() (*git.UncommittedWorkStatus, error) {
	return &git.UncommittedWorkStatus{HasUncommittedChanges: f.dirty}, nil
}

// TestPopBranchStashesKeepsStashWhenAPopFails pins gt-638go.14 finding 5: a pop
// chain that stops on a conflict leaves the stashes in place, so the cleanup
// status stays "stash". Returning "" here would let the slot read as
// reclaimed while its stashes are still orphaned.
func TestPopBranchStashesKeepsStashWhenAPopFails(t *testing.T) {
	t.Parallel()
	g := &fakeStashGit{
		entries: []git.StashEntry{{Ref: "stash@{0}", Message: "WIP on main"}},
		popErr:  errors.New("conflict"),
	}
	if got := popBranchStashes(g, "stash"); got != "stash" {
		t.Fatalf("popBranchStashes = %q, want %q: a stopped pop chain leaves the stashes in place", got, "stash")
	}
}

// TestPopBranchStashesRecomputesOnlyAfterAFullChain is the other half: a full
// pop chain that moves content into the tree reports "uncommitted", and one
// that produces nothing dirty recomputes normally.
func TestPopBranchStashesRecomputesOnlyAfterAFullChain(t *testing.T) {
	t.Parallel()
	dirty := &fakeStashGit{entries: []git.StashEntry{{Ref: "stash@{0}"}}, dirty: true}
	if got := popBranchStashes(dirty, "stash"); got != "uncommitted" {
		t.Errorf("popBranchStashes with content in the tree = %q, want %q", got, "uncommitted")
	}
	clean := &fakeStashGit{entries: []git.StashEntry{{Ref: "stash@{0}"}}}
	if got := popBranchStashes(clean, "stash"); got != "" {
		t.Errorf("popBranchStashes with nothing dirty = %q, want %q", got, "")
	}
}

// TestBeadFlagSuppressesTheStaleBranchGuard pins gt-638go.14 finding 4: --bead
// fills the same issue slot --issue does, so a run that passes --bead does not
// have its issue id overridden by the hooked bead when the branch name embeds a
// different one.
func TestBeadFlagSuppressesTheStaleBranchGuard(t *testing.T) {
	t.Parallel()
	const branchIssue, sender = "gt-stale-branch", "gastown/polecats/refuge"

	if !staleBranchGuardApplies("", branchIssue, sender) {
		t.Error("guard should run when no flag names an issue")
	}
	issue := doneIssueFromFlags(Options{Bead: "bd-source"})
	if issue != "bd-source" {
		t.Fatalf("doneIssueFromFlags(--bead) = %q, want %q", issue, "bd-source")
	}
	if staleBranchGuardApplies(issue, branchIssue, sender) {
		t.Error("--bead must suppress the stale-branch guard, exactly as --issue does")
	}
	// --issue still wins over --bead when both are set.
	if got := doneIssueFromFlags(Options{Issue: "bd-issue", Bead: "bd-bead"}); got != "bd-issue" {
		t.Errorf("doneIssueFromFlags(both flags) = %q, want --issue to win", got)
	}
}
