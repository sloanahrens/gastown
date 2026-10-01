package landworker

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	pkgA = "github.com/steveyegge/gastown/internal/a"
	pkgB = "github.com/steveyegge/gastown/internal/b"
)

type redMainHarness struct {
	bd     *beadsfake.Fake
	r      *RedMain
	reruns []string
	// rerunExit is each package's rerun exit code; absent means 0.
	rerunExit map[string]int
	status    []string
}

func newRedMainHarness(t *testing.T) *redMainHarness {
	t.Helper()
	h := &redMainHarness{bd: beadsfake.New(beadsfake.WithPrefix("gt")), rerunExit: map[string]int{}}
	h.r = &RedMain{Rig: "gastown", Beads: h.bd, Logf: t.Logf,
		Rerun: func(_ context.Context, cmd, pkg string, pl PostLand) PostLandResult {
			h.reruns = append(h.reruns, cmd+" "+pkg+"@"+pl.Commit)
			code := h.rerunExit[pkg]
			return PostLandResult{ExitCode: code, Tail: "--- FAIL: TestX in " + pkg}
		},
		Status: func(line string) { h.status = append(h.status, line) },
	}
	return h
}

func (h *redMainHarness) open(t *testing.T) map[string]*beads.Issue {
	t.Helper()
	issues, err := h.bd.List(beads.ListOptions{Status: "open", Label: LabelRedMain, Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*beads.Issue{}
	for _, is := range issues {
		out[is.Title] = is
	}
	return out
}

func (h *redMainHarness) lastStatus() string { return h.status[len(h.status)-1] }

func pkgs(passed map[string]bool) []land.PackageResult {
	var out []land.PackageResult
	for _, p := range []string{pkgA, pkgB} {
		if v, ok := passed[p]; ok {
			out = append(out, land.PackageResult{Package: p, Passed: v})
		}
	}
	return out
}

func TestRedMainRerunsOnceAndFilesOneBeadPerStillRedPackage(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	h.rerunExit[pkgA] = 1 // A fails again; B passes its rerun
	pl := PostLand{BeadID: "gt-land1", Commit: "c1c1c1c1c1c1"}
	res := PostLandResult{ExitCode: 2, Packages: append(pkgs(map[string]bool{pkgA: false, pkgB: false}), land.PackageResult{Package: pkgA})}
	h.r.Red(context.Background(), "make test-slow", pl, res)

	if got := strings.Join(h.reruns, "|"); got != "make test-slow "+pkgA+"@c1c1c1c1c1c1|make test-slow "+pkgB+"@c1c1c1c1c1c1" {
		t.Fatalf("reruns %s; want one per failing package", got)
	}
	open := h.open(t)
	a := open[RedMainTitle("gastown", pkgA)]
	if len(open) != 1 || a == nil || a.Priority != 1 || !strings.Contains(a.Description, "gt-land1") || !strings.Contains(a.Description, "--- FAIL: TestX in "+pkgA) {
		t.Fatalf("open red-main beads %+v; want only %s's, citing the landing and the rerun's output", open, pkgA)
	}
	if s := h.lastStatus(); !strings.HasPrefix(s, "main RED at c1c1c1c1 (landed by gt-land1): "+pkgA+" ["+a.ID+"]") || !strings.HasSuffix(s, "flaky (passed on rerun): "+pkgB) {
		t.Fatalf("status %q", s)
	}

	// Red again at a later landing: the open bead gets a comment, not a twin.
	h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-land2", Commit: "c2c2c2c2"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
	if open := h.open(t); len(open) != 1 {
		t.Fatalf("second red filed a duplicate: %+v", open)
	}
	cs, _ := h.bd.Comments(a.ID)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Text, "still red: ") || !strings.Contains(cs[0].Text, "gt-land2") {
		t.Fatalf("comments on %s: %+v", a.ID, cs)
	}
}

func TestRedMainAllFlakyFilesNothing(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-land1", Commit: "c1c1c1c1c1"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
	if open := h.open(t); len(open) != 0 {
		t.Fatalf("flaky package filed %+v", open)
	}
	if s := h.lastStatus(); s != "main GREEN at c1c1c1c1 after rerun (landed by gt-land1); flaky (passed on rerun): "+pkgA {
		t.Fatalf("status %q", s)
	}
}

func TestRedMainClosesBeadsForPackagesThatPass(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	h.rerunExit[pkgA], h.rerunExit[pkgB] = 1, 1
	ctx := context.Background()
	h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l1", Commit: "c1"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false, pkgB: false})})
	// A red run that names no package files its own bead and reruns nothing.
	h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l2", Commit: "c2"}, PostLandResult{ExitCode: 2, Tail: "shell tests FAILED"})
	if len(h.reruns) != 2 || len(h.open(t)) != 3 {
		t.Fatalf("reruns %v open %v", h.reruns, h.open(t))
	}
	// Someone else's bead on another rig is not ours to close.
	other, err := h.bd.Create(beads.CreateOptions{Title: RedMainTitle("hm", pkgA), Labels: []string{LabelRedMain}})
	if err != nil {
		t.Fatal(err)
	}

	// B passes while A stays red: only B's bead closes.
	h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l3", Commit: "c3"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false, pkgB: true})})
	open := h.open(t)
	if open[RedMainTitle("gastown", pkgB)] != nil || open[RedMainTitle("gastown", pkgA)] == nil || open[RedMainTitle("gastown", redMainNoPackage)] == nil {
		t.Fatalf("after B passed: %v", open)
	}

	// Green closes every bead of this rig, the no-package one included.
	h.r.Green(ctx, "make test-slow", PostLand{BeadID: "gt-l4", Commit: "c4c4c4c4c4"}, PostLandResult{})
	open = h.open(t)
	if len(open) != 1 || open[other.Title] == nil {
		t.Fatalf("after green: %v; want only the other rig's bead", open)
	}
	if s := h.lastStatus(); s != "main GREEN at c4c4c4c4 (landed by gt-l4)" {
		t.Fatalf("status %q", s)
	}
}

func TestRedMainStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l1", Commit: "c1"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
	if len(h.reruns) != 0 || len(h.open(t)) != 0 || len(h.status) != 0 {
		t.Fatalf("a stopping daemon reran %v, filed %v, said %v", h.reruns, h.open(t), h.status)
	}
}
