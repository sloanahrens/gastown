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
