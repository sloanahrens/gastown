package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
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
