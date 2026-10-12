package landworker

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

func TestBranchForBead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ref  string
		bead string
		want bool
	}{
		{"refs/heads/polecat/opal/gt-abc+x1", "gt-abc", true},
		{"refs/heads/polecat/agate/gt-abc+x0", "gt-abc", true},
		{"refs/heads/polecat/opal/gt-abc.10+x1", "gt-abc", false}, // a child is not the parent
		{"refs/heads/polecat/opal/gt-abc.1+x1", "gt-abc", false},
		{"refs/heads/polecat/opal/gt-abc.10+x1", "gt-abc.1", false},
		{"refs/heads/polecat/opal/gt-abc.1+x1", "gt-abc.1", true},
		{"refs/heads/polecat/opal/gt-abcd+x1", "gt-abc", false},
		{"refs/heads/polecat/opal/gt-abc", "gt-abc", false}, // no mutation suffix
		{"refs/heads/polecat//gt-abc+x1", "gt-abc", false},
		{"refs/heads/polecat/gt-abc+x1", "gt-abc", false},
		{"refs/heads/sloan/gt-abc+x1", "gt-abc", false},
		{"refs/heads/main", "gt-abc", false},
	}
	for _, c := range cases {
		if got := branchForBead(c.ref, c.bead); got != c.want {
			t.Errorf("branchForBead(%q, %q) = %v; want %v", c.ref, c.bead, got, c.want)
		}
	}
}

func TestLandingDeletesTheBeadsPolecatBranches(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{
		"refs/heads/polecat/opal/gt-abc+x1":    tipHead,  // the branch that landed
		"refs/heads/polecat/agate/gt-abc+x0":   noteHead, // an earlier rejected attempt
		"refs/heads/polecat/opal/gt-abc.10+x1": noteHead, // a child bead
		"refs/heads/polecat/opal/gt-abcd+x1":   noteHead, // a longer id sharing the prefix
		"refs/heads/sloan/gt-abc+x1":           noteHead, // not a polecat branch
		"refs/heads/main":                      noteHead,
	}

	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	want := []string{"polecat/agate/gt-abc+x0", "polecat/opal/gt-abc+x1"}
	if strings.Join(h.remote.deleted, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted %v; want %v", h.remote.deleted, want)
	}
	for _, ref := range []string{
		"refs/heads/polecat/opal/gt-abc.10+x1",
		"refs/heads/polecat/opal/gt-abcd+x1",
		"refs/heads/sloan/gt-abc+x1",
		"refs/heads/main",
	} {
		if _, ok := h.remote.remoteRefs[ref]; !ok {
			t.Errorf("%s was deleted; want it kept", ref)
		}
	}
}

func TestLandingLeavesABranchThatMovedAfterListing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{
		"refs/heads/polecat/opal/gt-abc+x1":  tipHead,
		"refs/heads/polecat/agate/gt-abc+x0": noteHead,
	}
	moved := strings.Repeat("9", 40)
	h.remote.beforeDelete = func(branch string) {
		if branch == "polecat/opal/gt-abc+x1" {
			h.remote.remoteRefs["refs/heads/"+branch] = moved
		}
	}

	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	if h.remote.remoteRefs["refs/heads/polecat/opal/gt-abc+x1"] != moved {
		t.Fatal("the branch that moved after listing was deleted; want it left alone")
	}
	if _, ok := h.remote.remoteRefs["refs/heads/polecat/agate/gt-abc+x0"]; ok {
		t.Fatal("the unmoved earlier attempt survived; want it deleted")
	}
}

func TestLandingSurvivesAFailedBranchListing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{"refs/heads/polecat/opal/gt-abc+x1": tipHead}
	h.remote.listErr = errors.New("origin unreachable")
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("report %v; want the landing to succeed", rep)
	}
	if len(h.remote.deleted) != 0 {
		t.Fatalf("deleted %v; want none", h.remote.deleted)
	}
	if !logged(logs, "origin unreachable") {
		t.Fatalf("logs %q; want one naming the failed listing", logs)
	}
}

func TestLandingSurvivesAFailedBranchDelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{
		"refs/heads/polecat/opal/gt-abc+x1":  tipHead,
		"refs/heads/polecat/agate/gt-abc+x0": noteHead,
	}
	h.remote.deleteErr = map[string]error{"polecat/opal/gt-abc+x1": errors.New("no lease")}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("report %v; want the landing to succeed", rep)
	}
	if _, ok := h.remote.remoteRefs["refs/heads/polecat/opal/gt-abc+x1"]; !ok {
		t.Fatal("the branch whose delete failed was deleted anyway")
	}
	if _, ok := h.remote.remoteRefs["refs/heads/polecat/agate/gt-abc+x0"]; ok {
		t.Fatal("one delete's failure stopped the next; want the rest attempted")
	}
	if !logged(logs, "no lease") {
		t.Fatalf("logs %q; want one naming the failed delete", logs)
	}
}

func logged(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// countLogs counts the lines containing substr.
func countLogs(lines []string, substr string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// seedLeftover puts one origin polecat branch for bead on the fake remote,
// with the bead in status and the branch tipped age ago. It turns the sweep on
// (WatchTarget) so the pass that follows sweeps.
func (h *harness) seedLeftover(t *testing.T, branch, bead, status string, merged bool, age time.Duration) {
	t.Helper()
	h.w.WatchTarget = "main"
	h.bd.Seed(beads.Issue{ID: bead, Title: "work", Status: status, Type: "task"})
	if h.remote.remoteRefs == nil {
		h.remote.remoteRefs = map[string]string{}
	}
	if h.remote.landed == nil {
		h.remote.landed = map[string]bool{}
	}
	if h.remote.times == nil {
		h.remote.times = map[string]time.Time{}
	}
	head := headFor(branch)
	h.remote.remoteRefs["refs/heads/"+branch] = head
	if merged {
		h.remote.landed[branch] = true
	}
	h.remote.times[head] = h.now.Add(-age)
}

// headFor is a commit id of its own per branch, so a test can age one
// leftover branch without aging another.
func headFor(branch string) string {
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(branch))
	return fmt.Sprintf("%040x", sum.Sum64())
}

// TestSweepDeletesABranchALandingLeftBehind: the first pass of a run sweeps
// the branch a landing left on origin when the daemon died before the
// post-landing reap could run (gt-xz4ir). No bead is ready: the sweep is all
// this pass does.
func TestSweepDeletesABranchALandingLeftBehind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, "polecat/opal/gt-abc+x1", "gt-abc", "closed", true, 2*time.Hour)

	rep := h.w.Pass(context.Background())

	if rep.Landed != 0 || rep.Failed != 0 {
		t.Fatalf("report %v; want an idle pass", rep)
	}
	if len(h.remote.deleted) != 1 || h.remote.deleted[0] != "polecat/opal/gt-abc+x1" {
		t.Fatalf("deleted %v; want the landed bead's leftover branch", h.remote.deleted)
	}
}

// TestSweepDeletesTheRigReposCopyToo: the branch the sweep deletes on origin
// goes from the rig repository with it. The sweep reaped origin alone, so the
// rig's own copy of a branch its landing never reaped stayed there for good
// (gt-4j1pl).
//
// The proof is the sweep's own: BranchLandedOn saw the work on the target by
// content, which is what a squash merge leaves and what the repository's own
// main — and so git's merged test — would refuse (gt-22ewa). No landing
// record is seeded, and the target's containment is no help either, so the
// branch goes on nothing but the proof the caller passed down.
func TestSweepDeletesTheRigReposCopyToo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, branch, "gt-abc", "closed", true, 2*time.Hour)
	h.remote.localBranches = map[string]string{branch: headFor(branch)}

	rep := h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want no failure", rep)
	}
	if len(h.remote.deleted) != 1 || h.remote.deleted[0] != branch {
		t.Fatalf("deleted %v; want origin's copy gone", h.remote.deleted)
	}
	if _, ok := h.remote.localBranches[branch]; ok {
		t.Fatal("the rig repository still has the branch the sweep deleted on origin")
	}
	if len(h.remote.localForced) != 1 || h.remote.localForced[0] != branch {
		t.Fatalf("forced local deletes %v; want the sweep's own proof to force it", h.remote.localForced)
	}
}

// TestSweepDeletesTheLocalCopyOfARecordedLanding: a landing record is proof
// the bead's work is on the target whatever its tip's ancestry shows, so the
// sweep's local half takes that branch on the ledger alone (gt-4j1pl).
func TestSweepDeletesTheLocalCopyOfARecordedLanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", LandedCommit: tipHead, Target: "main"}}
	h.seedLeftover(t, branch, "gt-abc", "open", true, 2*time.Hour)
	h.remote.localBranches = map[string]string{branch: headFor(branch)}

	rep := h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want no failure", rep)
	}
	if _, ok := h.remote.localBranches[branch]; ok {
		t.Fatal("the rig repository kept the branch of a recorded landing")
	}
	if len(h.remote.localForced) != 1 || h.remote.localForced[0] != branch {
		t.Fatalf("forced local deletes %v; want the recorded landing's branch", h.remote.localForced)
	}
}

// TestSweepKeepsTheRigReposCopyOfABranchItKeeps: one it keeps is kept in both
// places (gt-4j1pl).
func TestSweepKeepsTheRigReposCopyOfABranchItKeeps(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, branch, "gt-abc", "closed", true, 10*time.Minute)
	h.remote.localBranches = map[string]string{branch: headFor(branch)}

	rep := h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want no failure", rep)
	}
	if len(h.remote.deleted) != 0 {
		t.Fatalf("deleted %v; want the young branch kept on origin", h.remote.deleted)
	}
	if _, ok := h.remote.localBranches[branch]; !ok {
		t.Fatal("the rig repository lost a branch the sweep kept on origin")
	}
}

// TestSweepLogsALocalDeleteFailureOnce: the local half is best effort like the
// rest of the pass — a failure leaves the branch and is reported through the
// sweep's once-a-run line, never as a failure of the pass (gt-4j1pl).
func TestSweepLogsALocalDeleteFailureOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, branch, "gt-abc", "closed", true, 2*time.Hour)
	h.remote.localBranches = map[string]string{branch: headFor(branch)}
	h.remote.localErr = map[string]error{branch: errors.New("checked out in a worktree")}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rep := h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want the local failure to stay out of it", rep)
	}
	if _, ok := h.remote.localBranches[branch]; !ok {
		t.Fatal("a branch whose local delete failed is gone from the rig repository")
	}
	if !logged(logs, "checked out in a worktree") {
		t.Fatalf("logs %q; want one naming the failed local delete", logs)
	}
	if !h.w.sweepLogged[branch] {
		t.Error("the local-delete failure skipped the sweep's once-a-run log")
	}
}

// TestSweepSweepsWhatALandingLeftBehind: the shape of the branch that
// started this (gt-ck1if) — the landing's reap never ran because the daemon
// restarted in the moment after the push — and the next run's first pass
// sweeps what it left.
func TestSweepSweepsWhatALandingLeftBehind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.w.WatchTarget = "main"
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{"refs/heads/polecat/opal/gt-abc+x1": tipHead}
	h.remote.times = map[string]time.Time{tipHead: h.now.Add(-2 * time.Hour)}
	h.remote.landed = map[string]bool{"polecat/opal/gt-abc+x1": true}
	h.remote.listErr = errors.New("origin unreachable") // the reap cannot run

	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want the landing", rep)
	}
	if len(h.remote.deleted) != 0 {
		t.Fatalf("deleted %v; want the reap to have failed, leaving the branch", h.remote.deleted)
	}

	// The daemon restarted before the reap could retry: the bead is closed
	// with a landing record, and the new run's first pass sweeps.
	h.remote.listErr = nil
	h.w.sweptAt = time.Time{}
	h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "closed", Type: "task"})
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", LandedCommit: tipHead, Target: "main"}}

	if rep := h.w.Pass(context.Background()); rep.Failed != 0 || rep.Landed != 0 {
		t.Fatalf("report %v; want a clean sweep pass", rep)
	}
	if len(h.remote.deleted) != 1 || h.remote.deleted[0] != "polecat/opal/gt-abc+x1" {
		t.Fatalf("deleted %v; want the branch the landing left", h.remote.deleted)
	}
}

// TestSweepSweepsABeadWithALandingRecord: a bead whose landing record the
// file holds is done even when its status was never closed (the daemon
// stopped between the push and the close).
func TestSweepSweepsABeadWithALandingRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", LandedCommit: tipHead, Target: "main"}}
	h.seedLeftover(t, "polecat/opal/gt-abc+x1", "gt-abc", "open", true, 2*time.Hour)

	rep := h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want no failure", rep)
	}
	if len(h.remote.deleted) != 1 {
		t.Fatalf("deleted %v; want the recorded landing's branch", h.remote.deleted)
	}
}

// TestSweepKeepsTheBranchOfABeadStillInPlay: a bead that is open, in
// progress, blocked, deferred or hooked keeps its branch, whatever origin
// holds.
func TestSweepKeepsTheBranchOfABeadStillInPlay(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"open", "in_progress", "blocked", "deferred", "hooked"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedLeftover(t, "polecat/opal/gt-abc+x1", "gt-abc", status, true, 2*time.Hour)

			rep := h.w.Pass(context.Background())

			if rep.Failed != 0 {
				t.Fatalf("report %v; want no failure", rep)
			}
			if len(h.remote.deleted) != 0 {
				t.Fatalf("deleted %v; want the %s bead's branch kept", h.remote.deleted, status)
			}
			if _, ok := h.remote.remoteRefs["refs/heads/polecat/opal/gt-abc+x1"]; !ok {
				t.Fatalf("the %s bead's branch is gone from origin", status)
			}
		})
	}
}

// TestSweepKeepsAnUnmergedBranchAndSaysSoOnce: work that is not on the
// landing branch is never deleted, and the sweep says so once per run, not
// once per interval.
func TestSweepKeepsAnUnmergedBranchAndSaysSoOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, "polecat/opal/gt-abc+x1", "gt-abc", "closed", false, 2*time.Hour)
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rep := h.w.Pass(context.Background())
	h.now = h.now.Add(2 * time.Hour)
	h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want no failure", rep)
	}
	if len(h.remote.deleted) != 0 {
		t.Fatalf("deleted %v; want the unmerged branch kept", h.remote.deleted)
	}
	if n := countLogs(logs, "kept polecat/opal/gt-abc+x1: not merged into main"); n != 1 {
		t.Fatalf("kept lines = %d, want 1 over two sweeps:\n%s", n, strings.Join(logs, "\n"))
	}
}

// TestSweepKeepsAYoungBranch: a branch pushed within the hour may belong to a
// landing or a rework still in flight.
func TestSweepKeepsAYoungBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, "polecat/opal/gt-abc+x1", "gt-abc", "closed", true, 10*time.Minute)

	rep := h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want no failure", rep)
	}
	if len(h.remote.deleted) != 0 {
		t.Fatalf("deleted %v; want the young branch kept", h.remote.deleted)
	}
}

// TestSweepKeepsABranchThatMoved: the delete is a lease on the hash the
// listing saw, so a branch that moved in between stays.
func TestSweepKeepsABranchThatMoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, "polecat/opal/gt-abc+x1", "gt-abc", "closed", true, 2*time.Hour)
	moved := strings.Repeat("9", 40)
	h.remote.beforeDelete = func(branch string) {
		h.remote.remoteRefs["refs/heads/"+branch] = moved
	}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rep := h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want no failure", rep)
	}
	if got := h.remote.remoteRefs["refs/heads/polecat/opal/gt-abc+x1"]; got != moved {
		t.Fatalf("the branch that moved is %s; want it kept", got)
	}
	if !logged(logs, "moved since it was listed") {
		t.Fatalf("logs %q; want one naming the refused delete", logs)
	}
}

// TestSweepNeverHoldsUpALanding: a remote that refuses the sweep's questions
// is one log line, and the landing this pass still lands.
func TestSweepNeverHoldsUpALanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.seedLeftover(t, "polecat/old/gt-old+x1", "gt-old", "closed", true, 2*time.Hour)
	h.remote.listErr = errors.New("origin unreachable")
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rep := h.w.Pass(context.Background())

	if rep.Landed != 1 || rep.Failed != 0 {
		t.Fatalf("report %v; want the landing to stand", rep)
	}
	if !logged(logs, "origin unreachable") {
		t.Fatalf("logs %q; want one naming the failed sweep", logs)
	}
}

// TestSweepRunsAtMostOnceAnInterval: the sweep is an origin listing, so a
// pass inside the interval does not list again.
func TestSweepRunsAtMostOnceAnInterval(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, "polecat/opal/gt-abc+x1", "gt-abc", "closed", true, 2*time.Hour)

	h.w.Pass(context.Background())
	afterFirst := h.remote.listCalls
	if afterFirst != 1 {
		t.Fatalf("listings after the first pass = %d, want 1", afterFirst)
	}
	h.w.Pass(context.Background())
	if h.remote.listCalls != afterFirst {
		t.Fatalf("listings after a pass inside the interval = %d, want %d", h.remote.listCalls, afterFirst)
	}
	h.now = h.now.Add(2 * time.Hour)
	h.w.Pass(context.Background())
	if h.remote.listCalls != afterFirst+1 {
		t.Fatalf("listings after the interval = %d, want %d", h.remote.listCalls, afterFirst+1)
	}
}

// TestSweepLeavesBranchesItCannotPlace: a branch that is not a polecat
// branch, or that names no bead, is none of the sweep's business.
func TestSweepLeavesBranchesItCannotPlace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLeftover(t, "polecat/opal/gt-abc+x1", "gt-abc", "closed", true, 2*time.Hour)
	for _, ref := range []string{"refs/heads/main", "refs/heads/polecat/opal/nobeat", "refs/heads/polecat//gt-abc+x1"} {
		h.remote.remoteRefs[ref] = headFor(ref)
	}

	rep := h.w.Pass(context.Background())

	if rep.Failed != 0 {
		t.Fatalf("report %v; want no failure", rep)
	}
	if len(h.remote.deleted) != 1 || h.remote.deleted[0] != "polecat/opal/gt-abc+x1" {
		t.Fatalf("deleted %v; want only the polecat branch that names a done bead", h.remote.deleted)
	}
}

func TestBeadForBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		branch string
		want   string
	}{
		{"polecat/opal/gt-abc+x1", "gt-abc"},
		{"polecat/agate/gt-abc.10+x0", "gt-abc.10"},
		{"polecat/opal/gt-abc", ""},               // no mutation suffix
		{"polecat/opal/gt-abc+", ""},              // no mutation
		{"polecat//gt-abc+x1", ""},                // no author
		{"polecat/gt-abc+x1", ""},                 // no bead segment
		{"polecat", ""},                           // the prefix alone
		{"sloan/gt-abc+x1", ""},                   // not a polecat branch
		{"refs/heads/polecat/opal/gt-abc+x1", ""}, // a full ref, not the name
		{"polecat/opal/-x+y", ""},                 // a flag is not an id
		{"polecat/opal/--read-only+y", ""},        // neither is a long one
		{"", ""},
	}
	for _, c := range cases {
		if got := beadForBranch(c.branch); got != c.want {
			t.Errorf("beadForBranch(%q) = %q; want %q", c.branch, got, c.want)
		}
	}
}

// TestLandingRefreshesEachAuthorSeat: the landing that just deleted a bead's
// origin branches refreshes the seat worktree of every polecat that authored
// one — the landed branch's author and an earlier attempt's, which can differ
// — and nobody else (gt-fn9e6.55).
func TestLandingRefreshesEachAuthorSeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{
		"refs/heads/polecat/opal/gt-abc+x1":    tipHead,  // the branch that landed
		"refs/heads/polecat/agate/gt-abc+x0":   noteHead, // an earlier rejected attempt
		"refs/heads/polecat/opal/gt-abc.10+x1": noteHead, // a child bead
		"refs/heads/sloan/gt-abc+x1":           noteHead, // not a polecat branch
		"refs/heads/main":                      noteHead,
	}
	var refreshed []string
	h.w.RefreshAuthorSeat = func(polecat string) error {
		refreshed = append(refreshed, polecat)
		return nil
	}

	rep := h.w.Pass(context.Background())

	if rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	if want := []string{"agate", "opal"}; strings.Join(refreshed, ",") != strings.Join(want, ",") {
		t.Fatalf("refreshed seats %v; want %v (every author of a branch for the bead, and only those)", refreshed, want)
	}
}

// TestLandingAuthorSeatRefreshFailureIsLoggedOnce covers the hook's best
// effort: a seat whose worktree is missing or whose fetch fails is one log
// line, and the landing it follows stands.
func TestLandingAuthorSeatRefreshFailureIsLoggedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{"refs/heads/polecat/opal/gt-abc+x1": tipHead}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	h.w.RefreshAuthorSeat = func(string) error { return errors.New("fetch: connection refused") }

	rep := h.w.Pass(context.Background())

	if rep.Landed != 1 || rep.Failed != 0 {
		t.Fatalf("report %v; want the landing to stand", rep)
	}
	lines := 0
	for _, l := range logs {
		if strings.Contains(l, "refreshing the seat worktree of gastown/opal") {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("author-seat refresh log lines = %d, want 1:\n%s", lines, strings.Join(logs, "\n"))
	}
}

// TestLandingRefreshesTheRequestsWorkerWhenTheBranchesCannotBeListed: a
// failed listing leaves the earlier attempts unknown, and the landing
// request's worker is the author that is still known.
func TestLandingRefreshesTheRequestsWorkerWhenTheBranchesCannotBeListed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.listErr = errors.New("origin unreachable")
	var refreshed []string
	h.w.RefreshAuthorSeat = func(polecat string) error {
		refreshed = append(refreshed, polecat)
		return nil
	}

	rep := h.w.Pass(context.Background())

	if rep.Landed != 1 {
		t.Fatalf("report %v; want the landing to succeed", rep)
	}
	if strings.Join(refreshed, ",") != "opal" {
		t.Fatalf("refreshed seats %v; want the landing request's worker", refreshed)
	}
}

func TestAuthorPolecat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		branch string
		want   string
	}{
		{"polecat/opal/gt-abc+x1", "opal"},
		{"polecat/agate/gt-abc.10+x0", "agate"},
		{"polecat//gt-abc+x1", ""},     // no author
		{"polecat/gt-abc+x1", ""},      // no bead segment, so no author
		{"polecat", ""},                // the prefix alone
		{"sloan/gt-abc+x1", ""},        // not a polecat branch
		{"refs/heads/polecat/x+y", ""}, // a full ref, not the short name
		{"", ""},
	}
	for _, c := range cases {
		if got := authorPolecat(c.branch); got != c.want {
			t.Errorf("authorPolecat(%q) = %q; want %q", c.branch, got, c.want)
		}
	}
}

// TestAuthorSeats covers the set a landing's seat refresh covers: every
// distinct author the bead's branches name, then the landing request's
// worker, which is the author left when no branch names one.
func TestAuthorSeats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		branches []string
		worker   string
		want     []string
	}{
		{"the landed branch and an earlier attempt", []string{"polecat/opal/gt-abc+x1", "polecat/agate/gt-abc+x0"}, "opal", []string{"opal", "agate"}},
		{"the worker is not doubled", []string{"polecat/opal/gt-abc+x1"}, "opal", []string{"opal"}},
		{"no branches listed still refreshes the request's worker", nil, "opal", []string{"opal"}},
		{"branches that name nobody leave only the worker", []string{"main", "polecat//gt-abc+x1"}, "opal", []string{"opal"}},
		{"nothing to refresh", nil, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := authorSeats(c.branches, c.worker); strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("authorSeats(%v, %q) = %v; want %v", c.branches, c.worker, got, c.want)
			}
		})
	}
}

// A landing reaps the bead's branches in the rig's own repository as well as
// on origin: the ledger's record is what proves the work landed, so its
// delete is forced (a squash or rebase merge leaves the tip outside main's
// ancestry) (gt-kne58).
func TestLandingDeletesTheBeadsLocalPolecatBranches(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{
		"refs/heads/polecat/opal/gt-abc+x1":  tipHead,
		"refs/heads/polecat/agate/gt-abc+x0": noteHead,
		"refs/heads/sloan/gt-abc+x1":         noteHead,
	}
	h.remote.localBranches = map[string]string{
		"polecat/opal/gt-abc+x1":  tipHead,
		"polecat/agate/gt-abc+x0": noteHead,
		"sloan/gt-abc+x1":         noteHead,
	}
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Head: tipHead, LandedCommit: "cccccccccc"}}

	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	const want = "polecat/agate/gt-abc+x0,polecat/opal/gt-abc+x1"
	if got := strings.Join(h.remote.localDeleted, ","); got != want {
		t.Fatalf("deleted locally %q; want %q", got, want)
	}
	if got := strings.Join(h.remote.localForced, ","); got != want {
		t.Errorf("forced locally %q; want the ledger's record to force each delete", got)
	}
	if _, ok := h.remote.localBranches["sloan/gt-abc+x1"]; !ok {
		t.Error("a branch that is not a polecat branch of the bead was deleted")
	}
}

// With no landing record the reap falls back to the target's containment: a
// tip origin's target contains goes, and one it does not is kept and logged
// (gt-kne58).
//
// That fallback is the accepted case of gt-22ewa: the branch's work reached
// origin's target and the repository's own main has not merged it — nothing
// writes that main — so the proof, and not git's merged test, has to be what
// the delete rests on.
func TestLandingDeletesTheLocalBranchTheTargetContains(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.w.WatchTarget = "main"
	h.remote.remoteRefs = map[string]string{"refs/heads/polecat/opal/gt-abc+x1": tipHead}
	h.remote.localBranches = map[string]string{"polecat/opal/gt-abc+x1": tipHead}
	h.remote.contains[tipHead] = true

	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want the landing to succeed", rep)
	}
	if len(h.remote.localDeleted) != 1 {
		t.Fatalf("deleted locally %v; want the branch whose tip the target contains", h.remote.localDeleted)
	}
	if len(h.remote.localForced) != 1 {
		t.Fatalf("forced %v; want the proof to force it past the repository's own main, which has not merged the branch", h.remote.localForced)
	}
}

// A branch only the repository's own main has merged stays: its merge is the
// repository's, not the target's, and the reap asks for the target's
// (gt-22ewa).
func TestLandingKeepsALocalBranchOnlyTheReposOwnMainHasMerged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.w.WatchTarget = "main"
	h.remote.remoteRefs = map[string]string{"refs/heads/polecat/opal/gt-abc+x1": tipHead}
	h.remote.localBranches = map[string]string{"polecat/opal/gt-abc+x1": tipHead}
	h.remote.localMerged = map[string]bool{"polecat/opal/gt-abc+x1": true}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want the landing to succeed", rep)
	}
	if _, ok := h.remote.localBranches["polecat/opal/gt-abc+x1"]; !ok {
		t.Fatal("a branch only the repository's own main had merged was deleted")
	}
	if !logged(logs, "kept local polecat/opal/gt-abc+x1: not merged") {
		t.Fatalf("logs %q; want the kept branch named", logs)
	}
}

func TestLandingKeepsTheLocalBranchThatIsNotMerged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.w.WatchTarget = "main"
	h.remote.remoteRefs = map[string]string{"refs/heads/polecat/opal/gt-abc+x1": tipHead}
	h.remote.localBranches = map[string]string{"polecat/opal/gt-abc+x1": tipHead}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want the landing to succeed", rep)
	}
	if _, ok := h.remote.localBranches["polecat/opal/gt-abc+x1"]; !ok {
		t.Fatal("an unmerged local branch with no landing record was deleted")
	}
	if !logged(logs, "kept local polecat/opal/gt-abc+x1: not merged") {
		t.Fatalf("logs %q; want the kept branch named", logs)
	}
}

// A branch a worktree has checked out is a polecat still working in it, and
// the landing that reaps its origin copy leaves the local one alone
// (gt-kne58).
func TestLandingKeepsTheLocalBranchAWorktreeHasCheckedOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{"refs/heads/polecat/opal/gt-abc+x1": tipHead}
	h.remote.localBranches = map[string]string{"polecat/opal/gt-abc+x1": tipHead}
	h.remote.checkedOut = map[string]bool{"polecat/opal/gt-abc+x1": true}
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Head: tipHead, LandedCommit: "cccccccccc"}}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want the landing to succeed", rep)
	}
	if _, ok := h.remote.localBranches["polecat/opal/gt-abc+x1"]; !ok {
		t.Fatal("a branch a worktree has checked out was deleted")
	}
	if !logged(logs, "checked out") {
		t.Fatalf("logs %q; want the refusal named", logs)
	}
}

// A local delete that fails is logged and changes nothing: the landing, the
// counters and reapBeadBranches' return are the origin half's (gt-kne58).
func TestLandingSurvivesAFailedLocalDelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.remoteRefs = map[string]string{"refs/heads/polecat/opal/gt-abc+x1": tipHead}
	h.remote.localBranches = map[string]string{"polecat/opal/gt-abc+x1": tipHead}
	h.remote.localErr = map[string]error{"polecat/opal/gt-abc+x1": errors.New("repository is locked")}
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Head: tipHead, LandedCommit: "cccccccccc"}}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("report %v; want the landing to succeed", rep)
	}
	if _, ok := h.remote.remoteRefs["refs/heads/polecat/opal/gt-abc+x1"]; ok {
		t.Error("a failed local delete stopped the origin one")
	}
	if !logged(logs, "repository is locked") {
		t.Fatalf("logs %q; want one naming the failed local delete", logs)
	}
}

// Reaping one bead twice is a no-op: the branch the first pass took is not
// there to take again, and that is not an error (gt-kne58).
func TestLocalReapTwiceIsANoOp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.remote.localBranches = map[string]string{branch: tipHead}
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc"}}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	h.w.deleteLocalBranch("gt-abc", branch, tipHead, false, h.w.logf)
	h.w.deleteLocalBranch("gt-abc", branch, tipHead, false, h.w.logf)

	if got := strings.Join(h.remote.localDeleted, ","); got != branch {
		t.Fatalf("deleted %q; want %q once", got, branch)
	}
	if n := countLogs(logs, "deleted the local"); n != 1 {
		t.Fatalf("logged the local delete %d times; want once", n)
	}
}

// A landings file that cannot be read is a keep: the record is the proof,
// and without it the branch stays (gt-kne58).
func TestLocalDeleteKeepsTheBranchWhenTheLedgerCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.files.err = errors.New("landings file: permission denied")
	h.remote.localBranches = map[string]string{branch: tipHead}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	h.w.deleteLocalBranch("gt-abc", branch, tipHead, false, h.w.logf)

	if _, ok := h.remote.localBranches[branch]; !ok {
		t.Fatal("the branch was deleted without the record that proves it landed")
	}
	if !logged(logs, "permission denied") {
		t.Fatalf("logs %q; want one naming the unreadable ledger", logs)
	}
}
