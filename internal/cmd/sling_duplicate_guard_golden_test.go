package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

var updateDuplicateGuardGolden = flag.Bool("update-duplicate-guard-golden", false, "rewrite testdata/duplicate_guard_decisions.golden")

// duplicateGuardGoldenPath is the overlap guard's decisions over one fixed
// pool, written before the pool's design/notes reads went through a cache
// across ticks (gt-l0vlf): the cache may change what a tick reads, never what
// the guard decides or prints.
const duplicateGuardGoldenPath = "duplicate_guard_decisions.golden"

// duplicateGuardFixture seeds one rig's pool so the guard's every verdict
// shape appears: an overlap that lives only in design, one only in notes, a
// file-only overlap, a peer held by an open blocker and one whose blocker has
// closed, a closed peer, a hooked peer and a bead that names nothing.
func duplicateGuardFixture(t *testing.T) *beadsfake.Fake {
	t.Helper()
	closed := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	db := beadsfake.New()
	db.Seed(
		beads.Issue{ID: "gt-p1", Type: "task", Title: "Alpha flake", Status: "open", Priority: 1,
			Description: "Unrelated prose.", Design: "Root cause: TestAlpha fails under load.",
			CreatedAt: "2026-10-15T09:00:00Z", UpdatedAt: "2026-10-18T09:00:00Z"},
		beads.Issue{ID: "gt-p2", Type: "task", Title: "Foo rework", Status: "in_progress", Priority: 2,
			Description: "Rework.", Notes: "Touches internal/cmd/foo.go only.",
			CreatedAt: "2026-10-15T09:01:00Z", UpdatedAt: "2026-10-18T09:01:00Z"},
		beads.Issue{ID: "gt-p3", Type: "task", Title: "Beta behind an open blocker", Status: "open", Priority: 1,
			Description: "TestBeta fails.", CreatedAt: "2026-10-15T09:02:00Z", UpdatedAt: "2026-10-18T09:02:00Z"},
		beads.Issue{ID: "gt-p4", Type: "task", Title: "Beta behind a closed blocker", Status: "open", Priority: 1,
			Description: "TestBeta fails again.", CreatedAt: "2026-10-15T09:03:00Z", UpdatedAt: "2026-10-18T09:03:00Z"},
		beads.Issue{ID: "gt-b1", Type: "task", Title: "open blocker", Status: "open", Priority: 3,
			Description: "Nothing shared.", CreatedAt: "2026-10-15T09:04:00Z", UpdatedAt: "2026-10-18T09:04:00Z"},
		beads.Issue{ID: "gt-b2", Type: "task", Title: "closed blocker", Status: "closed", ClosedAt: closed, CloseReason: "done",
			Description: "Nothing shared.", CreatedAt: "2026-10-15T09:05:00Z", UpdatedAt: closed},
		beads.Issue{ID: "gt-p5", Type: "task", Title: "Gamma fix", Status: "closed", ClosedAt: closed, CloseReason: "fixed",
			Description: "Fixed.", Design: "The failing test was TestGamma.",
			CreatedAt: "2026-10-15T09:06:00Z", UpdatedAt: closed},
		beads.Issue{ID: "gt-p6", Type: "task", Title: "Delta in flight", Status: "hooked", Priority: 1, Assignee: "gastown/polecats/capable",
			Description: "TestDelta fails.", CreatedAt: "2026-10-15T09:07:00Z", UpdatedAt: "2026-10-18T09:07:00Z"},
	)
	for _, dep := range [][2]string{{"gt-p3", "gt-b1"}, {"gt-p4", "gt-b2"}} {
		if err := db.AddDependency(dep[0], dep[1]); err != nil {
			t.Fatalf("AddDependency %v: %v", dep, err)
		}
	}
	return db
}

// duplicateGuardTranscript runs the guard for six candidates over pools and
// renders every decision: whether the sling is refused, each overlap with the
// reason the dispatcher would hold it, and the message the operator reads.
func duplicateGuardTranscript(t *testing.T, pools *duplicatePools) string {
	t.Helper()
	townRoot := duplicateTown(t)
	candidates := []struct {
		id   string
		info beadInfo
	}{
		{"gt-c1", beadInfo{Title: "Fix TestAlpha", Description: "TestAlpha fails after the refactor."}},
		{"gt-c2", beadInfo{Title: "Fix TestBeta", Description: "TestBeta fails."}},
		{"gt-c3", beadInfo{Title: "Edit foo", Description: "Change internal/cmd/foo.go."}},
		{"gt-c4", beadInfo{Title: "Fix TestGamma", Description: "TestGamma fails."}},
		{"gt-c5", beadInfo{Title: "Fix TestDelta", Description: "TestDelta fails."}},
		{"gt-c6", beadInfo{Title: "Nothing to match", Description: "Prose only."}},
	}
	var out strings.Builder
	for _, c := range candidates {
		info := c.info
		info.Status, info.IssueType, info.Priority, info.CreatedAt = "open", "task", 2, "2026-10-16T09:00:00Z"
		candidate, matches, err := pools.check(townRoot, c.id, &info)
		fmt.Fprintf(&out, "== %s\n", c.id)
		if err != nil {
			fmt.Fprintf(&out, "error: %v\n", err)
			continue
		}
		if candidate == nil {
			out.WriteString("not checked\n")
			continue
		}
		decision := decideSlingDuplicates(*candidate, matches)
		fmt.Fprintf(&out, "blocked: %v\n", decision.Blocked)
		for _, m := range matches {
			fmt.Fprintf(&out, "match: %s %s tests=%v files=%v held=%q\n", m.Bead.ID, m.Bead.Status, m.SharedTests, m.SharedFiles, m.Bead.Held)
		}
		out.WriteString(decision.Message)
		out.WriteString("\n")
	}
	return out.String()
}

// duplicateGuardHolds answers the dispatcher's hold question from a pool row
// and the pool, as the daemon's tick does, so a peer behind an open blocker or
// behind a working seat reads as held. The backoff is stubbed to "not
// resting": the golden pins the questions the pool answers.
func duplicateGuardHolds() func(townRoot, beadsDir string, peer duplicateCandidate, pool []duplicateCandidate) string {
	knobs := specHoldKnobs{
		MaxPriority: 4,
		ShapeGate:   specdispatch.ShapeGateOff,
		Template:    specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"},
		Warn:        io.Discard,
		Backoff:     func(string) string { return "" },
	}
	return func(_, _ string, peer duplicateCandidate, pool []duplicateCandidate) string {
		return specPeerHeldFrom(beadsfake.New(), knobs, peer, pool)
	}
}

func readDuplicateGuardGolden(t *testing.T) string {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", duplicateGuardGoldenPath))
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	return string(want)
}

// TestDuplicateGuardDecisionsGolden pins the guard's decisions and every
// printed reason over the fixture pool (gt-l0vlf).
func TestDuplicateGuardDecisionsGolden(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	pools := newDuplicatePools(func(beadsDir string) ([]duplicateCandidate, error) {
		return fetchDuplicatePoolFrom(listLikeBD{Client: db}, func(dir string, err error) {
			t.Errorf("enrichment of %s failed: %v", dir, err)
		}, beadsDir)
	}, duplicateGuardHolds())
	got := duplicateGuardTranscript(t, pools)
	if *updateDuplicateGuardGolden {
		if err := os.WriteFile(filepath.Join("testdata", duplicateGuardGoldenPath), []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want := readDuplicateGuardGolden(t); got != want {
		t.Fatalf("decisions differ from testdata/%s:\n--- got\n%s--- want\n%s", duplicateGuardGoldenPath, got, want)
	}
}
