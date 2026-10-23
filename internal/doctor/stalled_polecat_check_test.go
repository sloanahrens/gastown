package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

// fakeSessionChecker implements polecatSessionChecker for testing, returning
// a canned (alive, err) pair for every session name it's asked about.
type fakeSessionChecker struct {
	alive bool
	err   error
}

func (f *fakeSessionChecker) HasSession(name string) (bool, error) {
	return f.alive, f.err
}

// fakePolecatGit implements polecatGit for testing.
//
// It answers land.WorkLandedRepo from canned fields: targetPreserved is whether
// the commit is on origin's default branch, and nothing else proves a landing,
// so tests that are not about supersession read as unlanded.
type fakePolecatGit struct {
	branch        string
	branchErr     error
	pushed        bool
	unpushedCount int
	pushCheckErr  error

	defaultBranch   string
	fetchErr        error
	targetPreserved bool
	targetStatusErr error
	targetsAsked    []string // "remote/target" as CommitLandedOnTarget was asked
	fetchCalls      int
	fetchedBranches []string // the branches each FetchBranchWithTimeout asked for
	pushErr         error
	pushCalls       []string
}

func (f *fakePolecatGit) CurrentBranch() (string, error) {
	return f.branch, f.branchErr
}

func (f *fakePolecatGit) BranchPushedToRemote(localBranch, remote string) (bool, int, error) {
	return f.pushed, f.unpushedCount, f.pushCheckErr
}

func (f *fakePolecatGit) RemoteDefaultBranch() string { return f.defaultBranch }

func (f *fakePolecatGit) FetchBranchWithTimeout(remote, branch string, timeout time.Duration) error {
	f.fetchCalls++
	f.fetchedBranches = append(f.fetchedBranches, remote+"/"+branch)
	return f.fetchErr
}

func (f *fakePolecatGit) CommitLandedOnTarget(remote, target, commit string) bool {
	f.targetsAsked = append(f.targetsAsked, remote+"/"+target)
	return f.targetPreserved && f.targetStatusErr == nil
}

func (f *fakePolecatGit) Rev(string) (string, error)            { return "head", nil }
func (f *fakePolecatGit) RefExists(string) (bool, error)        { return false, nil }
func (f *fakePolecatGit) IsAncestor(a, d string) (bool, error)  { return false, nil }
func (f *fakePolecatGit) MergeBase(a, b string) (string, error) { return "", errors.New("not faked") }
func (f *fakePolecatGit) DiffNameOnly(b, h string) ([]string, error) {
	return nil, errors.New("not faked")
}
func (f *fakePolecatGit) DiffPathsNameOnly(a, b string, p []string) ([]string, error) {
	return nil, errors.New("not faked")
}

func (f *fakePolecatGit) Push(remote, branch string, force bool) error {
	f.pushCalls = append(f.pushCalls, remote+"/"+branch)
	return f.pushErr
}

// makePolecatDir creates townRoot/<rig>/polecats/<name>/<rig>/.git so
// resolveClonePath finds a real clone path for the polecat.
func makePolecatDir(t *testing.T, townRoot, rig, name string) {
	t.Helper()
	clonePath := filepath.Join(townRoot, rig, "polecats", name, rig)
	if err := os.MkdirAll(filepath.Join(clonePath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
}

// checkFor runs the check against a single dead polecat whose git answers are
// supplied by f. Tests never reach tmux or bd: the session checker and bead
// lookup are both injected, so the check runs identically on a host with no
// tmux server and no Dolt.
func checkFor(t *testing.T, f *fakePolecatGit, beadStatus func(townRoot, beadID string) (string, bool)) (*StalledPolecatCheck, *CheckResult) {
	t.Helper()
	townRoot := t.TempDir()
	makePolecatDir(t, townRoot, "testrig", "furiosa")

	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(string) polecatGit { return f }
	check.beadStatus = beadStatus

	return check, check.Run(&CheckContext{TownRoot: townRoot, RigName: "testrig"})
}

// newTestCheck is NewStalledPolecatCheck with the bead read stubbed out: a
// test town has no bd, and an unreadable bead would turn every "nothing proves
// it landed" verdict into a probe failure.
func newTestCheck() *StalledPolecatCheck {
	check := NewStalledPolecatCheck()
	check.beadIssue = func(string, string) (*beads.Issue, error) { return nil, nil }
	return check
}

// openBeads is the injected lookup for tests that are not about supersession.
func openBeads(string, string) (string, bool) { return "open", true }

func TestStalledPolecatCheck_Properties(t *testing.T) {
	t.Parallel()
	check := NewStalledPolecatCheck()

	if check.Name() != "stalled-polecats" {
		t.Errorf("Name() = %q, want %q", check.Name(), "stalled-polecats")
	}

	if check.Description() == "" {
		t.Error("Description() should not be empty")
	}

	if !check.CanFix() {
		t.Error("CanFix() should be true — stalled polecats can have branches pushed")
	}

	if check.Category() != CategoryCleanup {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryCleanup)
	}
}

func TestStalledPolecatCheck_EmptyTownRoot(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir}

	check := newTestCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK for empty town root", result.Status)
	}
}

func TestStalledPolecatCheck_NoPolecats(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}

	check := newTestCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK when no polecats dir exists", result.Status)
	}
}

func TestStalledPolecatCheck_FixNoStalled(t *testing.T) {
	t.Parallel()
	check := newTestCheck()
	// Fix with no stalled polecats should be a no-op
	if err := check.Fix(&CheckContext{TownRoot: t.TempDir()}); err != nil {
		t.Errorf("Fix() with no stalled polecats returned error: %v", err)
	}
}

func TestStalledPolecatCheck_SessionLivenessErrorIsSkipped(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	makePolecatDir(t, tmpDir, "testrig", "furiosa")

	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{err: errors.New("tmux: no server running")}

	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped — session liveness could not be determined", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "furiosa") {
		t.Errorf("Details = %v, want the polecat named in the unknown reason", result.Details)
	}
}

func TestStalledPolecatCheck_PushStatusErrorIsSkipped(t *testing.T) {
	t.Parallel()
	_, result := checkFor(t, &fakePolecatGit{
		branch:       "polecat/furiosa/gt-abc+abc123",
		pushCheckErr: errors.New("git: unable to contact origin"),
	}, openBeads)

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped — push status could not be determined", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
}

func TestStalledPolecatCheck_UnknownDoesNotMaskConfirmedStall(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	makePolecatDir(t, tmpDir, "testrig", "furiosa")
	makePolecatDir(t, tmpDir, "testrig", "slate")

	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(clonePath string) polecatGit {
		if strings.Contains(clonePath, "furiosa") {
			return &fakePolecatGit{branchErr: errors.New("not a git repository")}
		}
		return &fakePolecatGit{branch: "polecat/slate-def456", unpushedCount: 3}
	}
	check.beadStatus = openBeads

	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
	result := check.Run(ctx)

	// A real, confirmed stall must still be reported even when another
	// polecat in the same run could not be checked.
	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning — a confirmed stall must not be masked by an unknown", result.Status)
	}
	if !strings.Contains(result.Message, "1 stalled") {
		t.Errorf("Message = %q, want it to report the 1 confirmed stall", result.Message)
	}
}

func TestStalledPolecatCheck_ResolveClonePath_NoDir(t *testing.T) {
	t.Parallel()
	check := newTestCheck()
	path := check.resolveClonePath(t.TempDir(), "testrig", "furiosa")
	if path != "" {
		t.Errorf("resolveClonePath() = %q, want empty for nonexistent", path)
	}
}

// --- the trigger's population ---

// TestStalledPolecatCheck_BranchOnOriginIsNotAtRisk is the gt-4vbn review
// regression. Judging "at risk" by whether the content is on the DEFAULT BRANCH
// flags every dead polecat sitting between `gt done` and the merge — the normal
// resting state of the whole merge queue — because pushed-but-unmerged work is
// not on main yet. Origin is custody: a branch there survives the worktree.
func TestStalledPolecatCheck_BranchOnOriginIsNotAtRisk(t *testing.T) {
	t.Parallel()
	_, result := checkFor(t, &fakePolecatGit{
		branch:          "polecat/furiosa/gt-abc+abc123",
		pushed:          true,
		unpushedCount:   0,
		defaultBranch:   "main",
		targetPreserved: false, // mid-merge-queue: pushed, not yet on main
	}, openBeads)

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK — a branch on origin is already in custody (details: %v)", result.Status, result.Details)
	}
}

// TestStalledPolecatCheck_BranchAbsentFromOriginIsAtRisk is the true positive
// the check exists for: dead session, commits on no remote, content on no
// default branch.
func TestStalledPolecatCheck_BranchAbsentFromOriginIsAtRisk(t *testing.T) {
	t.Parallel()
	check, result := checkFor(t, &fakePolecatGit{
		branch:          "polecat/furiosa/gt-abc+abc123",
		pushed:          false,
		unpushedCount:   2,
		defaultBranch:   "main",
		targetPreserved: false,
	}, openBeads)

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning for work on no remote (details: %v)", result.Status, result.Details)
	}
	if len(check.stalledPolecats) != 1 {
		t.Fatalf("stalledPolecats = %+v, want one entry", check.stalledPolecats)
	}
}

// TestStalledPolecatCheck_ContentOnDefaultBranchIsSuperseded covers the other
// half of the trigger: absent from origin, but the content is on the default
// branch, so nothing is lost by leaving it alone.
func TestStalledPolecatCheck_ContentOnDefaultBranchIsSuperseded(t *testing.T) {
	t.Parallel()
	f := &fakePolecatGit{
		branch:          "polecat/furiosa/gt-abc+abc123",
		pushed:          false,
		unpushedCount:   1,
		defaultBranch:   "main",
		targetPreserved: true,
	}
	check, result := checkFor(t, f, openBeads)

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK — content is on main (details: %v)", result.Status, result.Details)
	}
	if len(check.stalledPolecats) != 0 {
		t.Fatalf("stalledPolecats = %+v, want none", check.stalledPolecats)
	}
	if len(f.targetsAsked) != 1 || f.targetsAsked[0] != "origin/main" {
		t.Errorf("content check targets = %v, want [origin/main] — the default branch is the question", f.targetsAsked)
	}
	if f.fetchCalls == 0 {
		t.Error("expected the default branch to be refreshed before the comparison")
	}
	if len(f.fetchedBranches) == 0 || f.fetchedBranches[0] != "origin/main" {
		t.Errorf("refreshed branches = %v, want origin/main refreshed before the comparison", f.fetchedBranches)
	}
}

// TestStalledPolecatCheck_RefreshesTheBranchTheVerdictJudges is gt-kpj41: the
// rig's landings target its configured default branch while its remote's HEAD
// still points at main. Refreshing origin's HEAD would leave the judged branch
// stale, so a real landing on the rig's target reads as unlanded — the exact
// false positive the refresh exists to prevent.
func TestStalledPolecatCheck_RefreshesTheBranchTheVerdictJudges(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	makePolecatDir(t, townRoot, "testrig", "furiosa")
	cfg := `{"type": "rig", "name": "testrig", "default_branch": "develop"}`
	if err := os.WriteFile(filepath.Join(townRoot, "testrig", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write rig config: %v", err)
	}

	f := &fakePolecatGit{
		branch:        "polecat/furiosa/gt-abc+abc123",
		unpushedCount: 1,
		defaultBranch: "main", // origin/HEAD, which this rig's landings never touch
	}
	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(string) polecatGit { return f }
	check.beadStatus = openBeads

	check.Run(&CheckContext{TownRoot: townRoot, RigName: "testrig"})

	if len(f.fetchedBranches) != 1 || f.fetchedBranches[0] != "origin/develop" {
		t.Fatalf("refreshed branches = %v, want [origin/develop] — the rig's landing target, not the remote's HEAD", f.fetchedBranches)
	}
	if len(f.targetsAsked) == 0 {
		t.Fatal("the verdict was never asked about a target")
	}
	for _, target := range f.targetsAsked {
		if target != "origin/develop" {
			t.Errorf("judged target = %q, want origin/develop — the refresh must fetch the branch the verdict reads", target)
		}
	}
}

// TestStalledPolecatCheck_ContentCheckErrorStillWarns pins the fail-safe
// direction: the content check only ever narrows the trigger, so when it cannot
// answer, the branch stays flagged. Silence here would trade a loud false
// positive for a quiet false negative.
func TestStalledPolecatCheck_ContentCheckErrorStillWarns(t *testing.T) {
	t.Parallel()
	_, result := checkFor(t, &fakePolecatGit{
		branch:          "polecat/furiosa/gt-abc+abc123",
		unpushedCount:   1,
		defaultBranch:   "main",
		targetStatusErr: errors.New("no target/custody refs resolved"),
	}, openBeads)

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning when the content check is inconclusive (details: %v)", result.Status, result.Details)
	}
}

func TestStalledPolecatCheck_BranchSupersededByTerminalBead(t *testing.T) {
	t.Parallel()
	check := newTestCheck()
	branch := polecat.FormatGeneratedBranchName("furiosa", "gt-999", "abc123")

	tests := []struct {
		name       string
		beadStatus func(string, string) (string, bool)
		branch     string
		want       bool
	}{
		{"closed bead", func(string, string) (string, bool) { return "closed", true }, branch, true},
		{"tombstoned bead", func(string, string) (string, bool) { return "tombstone", true }, branch, true},
		{"open bead", func(string, string) (string, bool) { return "open", true }, branch, false},
		{"lookup failed", func(string, string) (string, bool) { return "", false }, branch, false},
		{"nil lookup", nil, branch, false},
		{"branch has no encoded issue", func(string, string) (string, bool) { return "closed", true }, "polecat/furiosa-abc123", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check.beadStatus = tt.beadStatus
			if got := check.branchSupersededByTerminalBead("/irrelevant", tt.branch); got != tt.want {
				t.Errorf("branchSupersededByTerminalBead() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestStalledPolecatCheck_ClosedBeadIsReportedNotSuppressed is the gt-5wse
// regression: a closed bead proves the work ITEM is resolved, not that this
// branch's specific content is preserved anywhere (a "no-changes" reclose
// after a zombie reset carries no such proof). The branch must still surface
// for manual review rather than reading as a clean bill of health, even
// though Fix must not auto-push it (that stays gt-4vbn's protection).
func TestStalledPolecatCheck_ClosedBeadIsReportedNotSuppressed(t *testing.T) {
	t.Parallel()
	check, result := checkFor(t, &fakePolecatGit{
		branch:        polecat.FormatGeneratedBranchName("furiosa", "gt-closed1", "abc123"),
		unpushedCount: 1,
		defaultBranch: "main",
	}, func(_, beadID string) (string, bool) {
		return "closed", beadID == "gt-closed1"
	})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want Skipped — closed bead is not proof this content is preserved, so it must not silently clear (details: %v)", result.Status, result.Details)
	}
	if len(check.stalledPolecats) != 0 {
		t.Fatalf("stalledPolecats = %+v, want none — Fix must not auto-push a branch behind a closed bead", check.stalledPolecats)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "CLOSED-BUT-UNVERIFIED") && strings.Contains(d, "gt-closed1") {
			found = true
		}
	}
	if !found {
		t.Errorf("Details = %v, want an entry naming the closed bead for manual review", result.Details)
	}
}

// TestStalledPolecatCheck_ClosedBeadDoesNotMaskConfirmedStall pairs a
// closed-bead branch with a genuinely stalled one in the same run: the
// needsReview entry must not downgrade a real, confirmed-at-risk stall away
// from StatusWarning.
func TestStalledPolecatCheck_ClosedBeadDoesNotMaskConfirmedStall(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	makePolecatDir(t, tmpDir, "testrig", "furiosa")
	makePolecatDir(t, tmpDir, "testrig", "slate")

	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(clonePath string) polecatGit {
		if strings.Contains(clonePath, "furiosa") {
			return &fakePolecatGit{
				branch:        polecat.FormatGeneratedBranchName("furiosa", "gt-closed2", "abc123"),
				unpushedCount: 1,
				defaultBranch: "main",
			}
		}
		return &fakePolecatGit{branch: "polecat/slate-def456", unpushedCount: 3}
	}
	check.beadStatus = func(_, beadID string) (string, bool) {
		return "closed", beadID == "gt-closed2"
	}

	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning — a confirmed stall must not be masked by a closed-bead entry", result.Status)
	}
	if !strings.Contains(result.Message, "1 stalled") {
		t.Errorf("Message = %q, want it to report the 1 confirmed stall", result.Message)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "CLOSED-BUT-UNVERIFIED") && strings.Contains(d, "gt-closed2") {
			found = true
		}
	}
	if !found {
		t.Errorf("Details = %v, want the closed-bead entry surfaced alongside the confirmed stall", result.Details)
	}
}

// --- Fix must re-verify ---

// TestStalledPolecatCheck_Fix_RefusesToResurrectLandedContent covers the second
// half of gt-4vbn: the suggested fix is itself the harm. Fix runs minutes after
// Run and is reachable with every polecat session dead, so a branch superseded
// in the gap must not be pushed back to origin off a stale verdict.
func TestStalledPolecatCheck_Fix_RefusesToResurrectLandedContent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	makePolecatDir(t, townRoot, "testrig", "furiosa")

	// Run sees genuinely unpreserved work, so it caches one stalled polecat.
	f := &fakePolecatGit{
		branch:          "polecat/furiosa/gt-race1+abc123",
		unpushedCount:   1,
		defaultBranch:   "main",
		targetPreserved: false,
	}
	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(string) polecatGit { return f }
	check.beadStatus = openBeads

	ctx := &CheckContext{TownRoot: townRoot, RigName: "testrig"}
	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("Run() Status = %v, want Warning before the race", result.Status)
	}
	if len(check.stalledPolecats) != 1 {
		t.Fatalf("expected exactly one stalled polecat before the race, got %+v", check.stalledPolecats)
	}

	// The race: content lands on main between Run and Fix.
	f.targetPreserved = true

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error = %v, want nil — a superseded branch has nothing to push", err)
	}
	if len(f.pushCalls) != 0 {
		t.Errorf("Fix() pushed a superseded branch back to origin: %v", f.pushCalls)
	}
}

// TestStalledPolecatCheck_Fix_PushesStillAtRiskBranch is the positive control
// for the two refusal tests: re-verification must not become a refusal to ever
// push. Without it, deleting the push entirely would pass every other Fix test.
func TestStalledPolecatCheck_Fix_PushesStillAtRiskBranch(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	makePolecatDir(t, townRoot, "testrig", "furiosa")

	f := &fakePolecatGit{
		branch:        "polecat/furiosa/gt-live1+abc123",
		unpushedCount: 1,
		defaultBranch: "main",
	}
	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(string) polecatGit { return f }
	check.beadStatus = openBeads

	ctx := &CheckContext{TownRoot: townRoot, RigName: "testrig"}
	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("Run() Status = %v, want Warning", result.Status)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error = %v, want nil", err)
	}
	want := "origin/" + f.branch
	if len(f.pushCalls) != 1 || f.pushCalls[0] != want {
		t.Errorf("Fix() push calls = %v, want [%s]", f.pushCalls, want)
	}
}

func TestStalledPolecatCheck_Fix_RefusesToResurrectClosedBead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	makePolecatDir(t, townRoot, "testrig", "furiosa")

	f := &fakePolecatGit{
		branch:        polecat.FormatGeneratedBranchName("furiosa", "gt-race2", "abc123"),
		unpushedCount: 1,
		defaultBranch: "main",
	}
	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(string) polecatGit { return f }
	check.beadStatus = func(_, _ string) (string, bool) { return "open", true }

	ctx := &CheckContext{TownRoot: townRoot, RigName: "testrig"}
	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("Run() Status = %v, want Warning before the bead closes", result.Status)
	}

	check.beadStatus = func(_, _ string) (string, bool) { return "closed", true }

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error = %v, want nil", err)
	}
	if len(f.pushCalls) != 0 {
		t.Errorf("Fix() pushed a branch whose bead closed between Run and Fix: %v", f.pushCalls)
	}
}

// --- real-git fixtures ---
//
// The tests above pin the decision table against a fake. These run the same
// decisions through real git, because the content check is only worth anything
// if ancestry, merge-tree and patch-equivalence actually agree with the answer
// the fake was told to give.

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stalledFixture is a bare origin, which other actors land content on, and
// the polecat's own clone under the layout resolveClonePath expects, holding
// one commit that exists on no remote.
type stalledFixture struct {
	gf          *gitfake.Fake
	townRoot    string
	rigName     string
	polecatName string
	issue       string
	branch      string
	bare        string
	clonePath   string
}

func newStalledFixture(t *testing.T, rigName, polecatName, issue string) *stalledFixture {
	t.Helper()
	tmp := t.TempDir()
	gf := gitfake.New()
	bare := fakeRemote(t, gf, filepath.Join(tmp, "origin.git"), map[string]string{"README.md": "seed\n"})

	townRoot := filepath.Join(tmp, "town")
	clonePath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		t.Fatalf("mkdir polecat dir: %v", err)
	}
	gf.Clone(t, bare, clonePath)

	branch := polecat.FormatGeneratedBranchName(polecatName, issue, "abc123")
	if err := gf.OpenBranchRepo(clonePath).CheckoutNewBranch(branch, "HEAD"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(clonePath, "work.txt"), "polecat work\n")
	gf.CommitWorktree(t, clonePath, "do the work ("+issue+")")

	return &stalledFixture{
		gf: gf, townRoot: townRoot, rigName: rigName, polecatName: polecatName,
		issue: issue, branch: branch, bare: bare, clonePath: clonePath,
	}
}

// reimplementOnMain lands the same fix on main as a NEW commit, as another
// actor would. That is the gt-4vbn shape: the work is in main, the branch's
// own SHA never will be, and the polecat's clone stops fetching at the moment
// it dies — so the clone's origin/main is stale too.
func (f *stalledFixture) reimplementOnMain(t *testing.T) {
	t.Helper()
	f.gf.Commit(t, f.bare, "main", "reimplement on main under a new SHA", map[string]string{"work.txt": "polecat work\n"})
}

func (f *stalledFixture) ctx() *CheckContext {
	return withGit(&CheckContext{TownRoot: f.townRoot, RigName: f.rigName}, f.gf)
}

// onOrigin reports whether origin has branch.
func (f *stalledFixture) onOrigin(t *testing.T, branch string) bool {
	t.Helper()
	ok, err := bareGit(f.gf, f.bare).RefExists("refs/heads/" + branch)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (f *stalledFixture) run(t *testing.T, beadStatus func(string, string) (string, bool)) (*StalledPolecatCheck, *CheckResult) {
	t.Helper()
	check := newTestCheck()
	// No tmux and no bd: the fixture's polecat has no session, and the tests
	// that need a bead answer inject it.
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.beadStatus = beadStatus
	return check, check.Run(f.ctx())
}

// TestStalledPolecatCheck_FlagsThenClearsOnSupersession is the gt-4vbn
// incident end to end. The first Run reproduces the alert that fired for hours:
// a dead polecat with a commit on no remote. The second Run, after the same fix
// lands on main under a different SHA, must clear — and only because the check
// refreshed a default branch its clone would never fetch again. Without that
// refresh the warning is permanent, which is the whole complaint.
func TestStalledPolecatCheck_FlagsThenClearsOnSupersession(t *testing.T) {
	t.Parallel()
	f := newStalledFixture(t, "testrig", "test-agate", "gt-4vbn1")

	check, result := f.run(t, openBeads)
	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning — commit on no remote (details: %v)", result.Status, result.Details)
	}
	if len(check.stalledPolecats) != 1 {
		t.Fatalf("stalledPolecats = %+v, want one entry", check.stalledPolecats)
	}

	f.reimplementOnMain(t)

	// Guard the premise: the clone's own view of main is stale, so an
	// unrefreshed comparison could not possibly see the landed fix.
	cloneMain, _ := f.gf.OpenBranchRepo(f.clonePath).Rev("origin/main")
	originMain := f.gf.Ref(f.bare, "refs/heads/main")
	if cloneMain == originMain {
		t.Fatalf("fixture is not exercising the refresh: clone origin/main == origin main (%s)", cloneMain)
	}

	check, result = f.run(t, openBeads)
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK — the fix is on main under a new SHA (details: %v)", result.Status, result.Details)
	}
	if len(check.stalledPolecats) != 0 {
		t.Fatalf("stalledPolecats = %+v, want none — work landed", check.stalledPolecats)
	}
}

// TestStalledPolecatCheck_KeepsWarningWhenWorkIsGenuinelyAbsent is the
// true positive the check was built for (gt-zzd, gt-7kr shapes): dead session,
// commit on no remote, content on no default branch. The content check must not
// clear it.
func TestStalledPolecatCheck_KeepsWarningWhenWorkIsGenuinelyAbsent(t *testing.T) {
	t.Parallel()
	f := newStalledFixture(t, "testrig", "test-obsidian", "gt-orphan1")

	check, result := f.run(t, openBeads)
	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning (details: %v)", result.Status, result.Details)
	}
	if len(check.stalledPolecats) != 1 || check.stalledPolecats[0].branch != f.branch {
		t.Fatalf("stalledPolecats = %+v, want one entry for %q", check.stalledPolecats, f.branch)
	}

	// Fix must still push it: re-verification narrows the population, it does
	// not disable the remedy.
	if err := check.Fix(f.ctx()); err != nil {
		t.Fatalf("Fix() error = %v, want nil", err)
	}
	if !f.onOrigin(t, f.branch) {
		t.Error("Fix() did not push genuinely at-risk work to origin")
	}
}

// TestStalledPolecatCheck_FixDoesNotResurrectSupersededBranch covers the
// second half of gt-4vbn — "the suggested fix is actively harmful". The branch
// is flagged while genuinely unpreserved, then the fix lands on main in the gap
// between Run and Fix. Fix must re-verify rather than push on Run's snapshot.
func TestStalledPolecatCheck_FixDoesNotResurrectSupersededBranch(t *testing.T) {
	t.Parallel()
	f := newStalledFixture(t, "testrig", "test-agate2", "gt-race1")

	check, result := f.run(t, openBeads)
	if result.Status != StatusWarning {
		t.Fatalf("Run() Status = %v, want Warning before the race", result.Status)
	}

	f.reimplementOnMain(t)

	if err := check.Fix(f.ctx()); err != nil {
		t.Fatalf("Fix() error = %v, want nil — a superseded branch has nothing to push", err)
	}
	if f.onOrigin(t, f.branch) {
		t.Errorf("Fix() pushed a superseded branch back to origin")
	}
}

// --- the shared landed-work predicate (gt-wlc9d) ---

// TestStalledPolecatCheck_VerdictReasonReachesTheDetail drives the check with
// each verdict land.WorkLandedOnTarget can give: only a proof clears the
// branch, and a verdict that is not a plain "unpushed" names its Reason token
// in the line the operator reads.
func TestStalledPolecatCheck_VerdictReasonReachesTheDetail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		verdict    land.WorkLandedVerdict
		wantStatus CheckStatus
		wantInLine string
	}{
		{"landed by ref", land.WorkLandedVerdict{Landed: true, Reason: land.WorkLandedByRef}, StatusOK, ""},
		{"landed by record", land.WorkLandedVerdict{Landed: true, Reason: land.WorkLandedByRecord}, StatusOK, ""},
		{"unpushed", land.WorkLandedVerdict{Reason: land.WorkUnpushed, Detail: "bead not read"}, StatusWarning, "no remote"},
		{"landed differs", land.WorkLandedVerdict{Reason: land.WorkLandedDiffers, Detail: "work.txt"}, StatusWarning, "[landed-differs: work.txt]"},
		{"probe failed", land.WorkLandedVerdict{Reason: land.WorkProbeFailed, Detail: "read HEAD: boom"}, StatusWarning, "[probe-failed: read HEAD: boom]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakePolecatGit{branch: "polecat/furiosa/gt-abc+abc123", unpushedCount: 1, defaultBranch: "main"}
			townRoot := t.TempDir()
			makePolecatDir(t, townRoot, "testrig", "furiosa")
			check := newTestCheck()
			check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
			check.gitForTest = func(string) polecatGit { return f }
			check.beadStatus = openBeads
			check.workLanded = func(land.WorkLandedRepo, land.WorkLandedQuery) land.WorkLandedVerdict {
				return tt.verdict
			}

			result := check.Run(&CheckContext{TownRoot: townRoot, RigName: "testrig"})
			if result.Status != tt.wantStatus {
				t.Fatalf("Status = %v, want %v (details: %v)", result.Status, tt.wantStatus, result.Details)
			}
			if tt.wantInLine != "" && (len(result.Details) == 0 || !strings.Contains(result.Details[0], tt.wantInLine)) {
				t.Errorf("Details = %v, want the first line to contain %q", result.Details, tt.wantInLine)
			}
		})
	}
}

// TestStalledPolecatCheck_AsksThePredicateAboutTheBranchsBead pins what the
// check hands land.WorkLandedOnTarget: the bead its branch name encodes, and
// that bead as read, so the refs/landed and LANDING RECORD arms can run.
func TestStalledPolecatCheck_AsksThePredicateAboutTheBranchsBead(t *testing.T) {
	t.Parallel()
	f := &fakePolecatGit{
		branch:        polecat.FormatGeneratedBranchName("furiosa", "gt-ask1", "abc123"),
		unpushedCount: 1,
		defaultBranch: "main",
	}
	townRoot := t.TempDir()
	makePolecatDir(t, townRoot, "testrig", "furiosa")
	issue := &beads.Issue{ID: "gt-ask1", Status: "closed"}
	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(string) polecatGit { return f }
	check.beadStatus = openBeads
	check.beadIssue = func(_, id string) (*beads.Issue, error) {
		if id != "gt-ask1" {
			t.Errorf("read bead %q, want gt-ask1", id)
		}
		return issue, nil
	}
	var gotBead string
	var gotIssue *beads.Issue
	check.workLanded = func(_ land.WorkLandedRepo, q land.WorkLandedQuery) land.WorkLandedVerdict {
		wantRig := filepath.Join(townRoot, "testrig")
		if q.RigPath != wantRig {
			t.Errorf("predicate asked about rig %q, want %q", q.RigPath, wantRig)
		}
		if q.Issue == nil {
			// The refs/landed and target arms did not prove it, so the bead is read.
			return land.WorkLandedVerdict{Reason: land.WorkUnpushed, Detail: "bead not read"}
		}
		gotBead, gotIssue = q.BeadID, q.Issue
		return land.WorkLandedVerdict{Landed: true, Reason: land.WorkLandedByRecord}
	}

	if result := check.Run(&CheckContext{TownRoot: townRoot, RigName: "testrig"}); result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK (details: %v)", result.Status, result.Details)
	}
	if gotBead != "gt-ask1" || gotIssue != issue {
		t.Errorf("predicate asked about (%q, %v), want (gt-ask1, the bead as read)", gotBead, gotIssue)
	}
}

// TestStalledPolecatCheck_UnreadableBeadIsAProbeFailure: when the bead cannot be
// read the LANDING RECORD arm did not run, so the report says the check was
// incomplete rather than that nothing proves the work landed.
func TestStalledPolecatCheck_UnreadableBeadIsAProbeFailure(t *testing.T) {
	t.Parallel()
	f := &fakePolecatGit{
		branch:        polecat.FormatGeneratedBranchName("furiosa", "gt-ask2", "abc123"),
		unpushedCount: 1,
		defaultBranch: "main",
	}
	townRoot := t.TempDir()
	makePolecatDir(t, townRoot, "testrig", "furiosa")
	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(string) polecatGit { return f }
	check.beadStatus = openBeads
	check.beadIssue = func(string, string) (*beads.Issue, error) { return nil, errors.New("dolt down") }

	result := check.Run(&CheckContext{TownRoot: townRoot, RigName: "testrig"})
	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning — an unreadable bead must not clear the branch", result.Status)
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "[probe-failed: read bead gt-ask2: dolt down]") {
		t.Errorf("Details = %v, want the probe failure named", result.Details)
	}
}

// TestStalledPolecatCheck_ReadsNoBeadWhenALandedArmProves: the bead is only
// needed by the LANDING RECORD arm, so a branch the refs/landed or target arm
// proves landed never reads it.
func TestStalledPolecatCheck_ReadsNoBeadWhenALandedArmProves(t *testing.T) {
	t.Parallel()
	f := &fakePolecatGit{
		branch:        polecat.FormatGeneratedBranchName("furiosa", "gt-ask3", "abc123"),
		unpushedCount: 1,
		defaultBranch: "main",
	}
	townRoot := t.TempDir()
	makePolecatDir(t, townRoot, "testrig", "furiosa")
	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.gitForTest = func(string) polecatGit { return f }
	check.beadStatus = openBeads
	reads := 0
	check.beadIssue = func(string, string) (*beads.Issue, error) {
		reads++
		return nil, nil
	}
	check.workLanded = func(land.WorkLandedRepo, land.WorkLandedQuery) land.WorkLandedVerdict {
		return land.WorkLandedVerdict{Landed: true, Reason: land.WorkLandedByRef}
	}

	if result := check.Run(&CheckContext{TownRoot: townRoot, RigName: "testrig"}); result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK (details: %v)", result.Status, result.Details)
	}
	if reads != 0 {
		t.Errorf("read the bead %d time(s), want 0 when refs/landed already proves the branch landed", reads)
	}
}

// TestStalledPolecatCheck_MissingBeadIsNotAProbeFailure: a town with no beads
// database has no LANDING RECORD to read. That is absence, so the unpushed
// branch warns without a probe-failed note on its line.
func TestStalledPolecatCheck_MissingBeadIsNotAProbeFailure(t *testing.T) {
	t.Parallel()
	f := newStalledFixture(t, "testrig", "test-gone", "gt-gone1")

	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.beadStatus = openBeads
	check.beadIssue = lookupBead
	result := check.Run(f.ctx())

	if result.Status != StatusWarning || len(check.stalledPolecats) != 1 {
		t.Fatalf("Status = %v, stalled = %+v; want Warning for the unpushed branch (details: %v)", result.Status, check.stalledPolecats, result.Details)
	}
	for _, d := range result.Details {
		if strings.Contains(d, "probe-failed") {
			t.Errorf("Details = %v, want no probe-failed note for a missing beads database", result.Details)
		}
	}
}

// landingRecordRun runs the check over the fixture's real git with the bead
// closed and carrying rec as its LANDING RECORD.
func landingRecordRun(t *testing.T, f *stalledFixture, rec land.LandingRecord) *CheckResult {
	t.Helper()
	check := newTestCheck()
	check.sessionCheckerForTest = &fakeSessionChecker{alive: false}
	check.beadStatus = openBeads
	check.beadIssue = func(string, string) (*beads.Issue, error) {
		return &beads.Issue{ID: f.issue, Status: "closed", Notes: rec.NoteBlock()}, nil
	}
	return check.Run(f.ctx())
}

// TestStalledPolecatCheck_LandingRecordReadsAsLandedOverRealGit runs the
// LANDING RECORD arm through real git: the work landed on main as a commit with
// the same file contents, and the record names it. The branch reads as landed.
func TestStalledPolecatCheck_LandingRecordReadsAsLandedOverRealGit(t *testing.T) {
	t.Parallel()
	f := newStalledFixture(t, "testrig", "test-rec", "gt-rec1")
	head, err := f.gf.OpenBranchRepo(f.clonePath).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	f.reimplementOnMain(t)
	landed, err := bareGit(f.gf, f.bare).Rev("refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}

	result := landingRecordRun(t, f, land.LandingRecord{Head: head, LandedCommit: landed, Branch: f.branch})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK — the landing record names main's commit with the branch's contents (details: %v)", result.Status, result.Details)
	}
}

// TestStalledPolecatCheck_LandingRecordDifferingReadsAsDiffers runs the same
// arm where the record names a head whose file the branch changed. The branch
// is not the landed content, so it warns and names the file.
func TestStalledPolecatCheck_LandingRecordDifferingReadsAsDiffers(t *testing.T) {
	t.Parallel()
	f := newStalledFixture(t, "testrig", "test-differ", "gt-rec2")
	seed, err := bareGit(f.gf, f.bare).Rev("refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}

	result := landingRecordRun(t, f, land.LandingRecord{Head: seed, LandedCommit: seed, Branch: f.branch})
	if result.Status != StatusWarning || len(result.Details) == 0 || !strings.Contains(result.Details[0], "[landed-differs:") || !strings.Contains(result.Details[0], "work.txt") {
		t.Fatalf("Status = %v, Details = %v; want Warning naming landed-differs for work.txt", result.Status, result.Details)
	}
}

// TestStalledPolecatCheck_SquashLandedBranchReadsAsLanded runs the real
// predicate over real git. The landing squashed the branch into a commit that
// is not its ancestor and shares no patch with it; only refs/landed/<bead>
// ties the two, and the check used to see nothing there. An unlanded branch in
// the same world still warns.
func TestStalledPolecatCheck_SquashLandedBranchReadsAsLanded(t *testing.T) {
	t.Parallel()
	f := newStalledFixture(t, "testrig", "test-squash", "gt-sq1")

	f.gf.Commit(t, f.bare, "main", "gt-sq1: squashed", map[string]string{"work.txt": "polecat work, squashed\n", "extra.txt": "x\n"})
	check, result := f.run(t, openBeads)
	if result.Status != StatusWarning || len(check.stalledPolecats) != 1 {
		t.Fatalf("Status = %v, stalled = %+v; want Warning — a squash with no refs/landed is not provably landed (details: %v)", result.Status, check.stalledPolecats, result.Details)
	}

	head, err := f.gf.OpenBranchRepo(f.clonePath).Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	f.gf.SetRef(t, f.clonePath, land.LandedRef(f.issue), head)

	check, result = f.run(t, openBeads)
	if result.Status != StatusOK || len(check.stalledPolecats) != 0 {
		t.Fatalf("Status = %v, stalled = %+v; want OK — refs/landed/%s holds the branch head (details: %v)", result.Status, check.stalledPolecats, f.issue, result.Details)
	}
}

// --- bead lookup ---

// TestLookupBeadStatus_GuardsNoOpPaths pins the failures that must not reach a
// subprocess: without these, a town root with no beads database would spawn bd
// once per candidate inside a scan that has to finish.
func TestLookupBeadStatus_GuardsNoOpPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		townRoot string
		beadID   string
	}{
		{"no town root", "", "gt-abc"},
		{"no bead id", t.TempDir(), ""},
		{"no beads database", t.TempDir(), "gt-abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, ok := lookupBeadStatus(tt.townRoot, tt.beadID)
			if ok || status != "" {
				t.Errorf("lookupBeadStatus(%q, %q) = (%q, %v), want (\"\", false)", tt.townRoot, tt.beadID, status, ok)
			}
		})
	}
}
