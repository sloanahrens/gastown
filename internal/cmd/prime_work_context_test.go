package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestRenderDependencyMergeStatusWarnsOnUnmergedBlocker is acceptance criterion
// 5 at the point the worker actually reads it: a starting polecat is told, in
// words, that its prerequisite is submitted but not landed.
func TestRenderDependencyMergeStatusWarnsOnUnmergedBlocker(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderDependencyMergeStatus(&buf, []beads.DependencyMergeStatus{
		{ID: "gt-blocker", Status: "closed", State: beads.DependencyMergeUnmerged, MR: "gt-wisp-mr1", Branch: "polecat/onyx/gt-blocker"},
		{ID: "gt-done", Status: "closed", State: beads.DependencyMergeLanded},
	})
	out := buf.String()

	// The blocker and its MR must both be named, so the worker can go look.
	if !strings.Contains(out, "gt-blocker") || !strings.Contains(out, "gt-wisp-mr1") {
		t.Fatalf("output must name the unmerged blocker and its MR, got:\n%s", out)
	}
	if !strings.Contains(out, "NOT merged") {
		t.Fatalf("output must say the dependency is not merged, got:\n%s", out)
	}
	// It must not claim a merely-queued dependency has landed.
	if strings.Contains(out, "gt-blocker — landed") {
		t.Fatalf("unmerged blocker must not be reported as landed, got:\n%s", out)
	}
	// And it must not warn about a dependency that has landed.
	if strings.Contains(out, "gt-done is closed but its work is NOT") {
		t.Fatalf("landed dependency must not raise the unmerged warning, got:\n%s", out)
	}
}

// failingShow is a show that must not be reached.
func failingShow(t *testing.T) func(string) (*beads.Issue, error) {
	return func(id string) (*beads.Issue, error) {
		t.Errorf("show(%s) called; want the fast path", id)
		return nil, errors.New("unexpected show")
	}
}

// TestBeadWithFullDependenciesSkipsShowWhenNoDependencies verifies the fast
// path: a bead with no dependencies is returned unchanged, without asking
// bd show.
func TestBeadWithFullDependenciesSkipsShowWhenNoDependencies(t *testing.T) {
	t.Parallel()
	bead := &beads.Issue{ID: "gt-does-not-exist-anywhere", DependencyCount: 0}
	if got := beadWithFullDependencies(bead, failingShow(t)); got != bead {
		t.Fatalf("beadWithFullDependencies() = %#v, want the same bead pointer unchanged", got)
	}
}

// TestBeadWithFullDependenciesRefetchesViaShow is the positive half of the
// fast-path test above: a bead that `bd list` reported with dependencies
// (DependencyCount > 0 but bare relation records, so no usable IDs) is
// replaced by the full record `bd show` returns, whose dependencies carry the
// id and status the merge-status check needs.
func TestBeadWithFullDependenciesRefetchesViaShow(t *testing.T) {
	t.Parallel()
	var shown []string
	show := func(id string) (*beads.Issue, error) {
		shown = append(shown, id)
		return &beads.Issue{ID: id, Status: "hooked", DependencyCount: 1,
			Dependencies: []beads.IssueDep{{ID: "gt-blocker", Title: "Blocker", Status: "closed", DependencyType: "blocks"}}}, nil
	}

	listed := &beads.Issue{ID: "gt-hooked", DependencyCount: 1}
	got := beadWithFullDependencies(listed, show)
	if got == listed {
		t.Fatal("beadWithFullDependencies() returned the listed bead; want the re-fetched one")
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0].ID != "gt-blocker" || got.Dependencies[0].Status != "closed" {
		t.Fatalf("re-fetched dependencies = %#v, want gt-blocker (closed)", got.Dependencies)
	}
	if len(shown) != 1 || shown[0] != "gt-hooked" {
		t.Fatalf("show calls = %v, want [gt-hooked]", shown)
	}
}

// TestBeadWithFullDependenciesFallsBackWhenShowFails: a re-fetch that errors
// must not lose the hooked bead.
func TestBeadWithFullDependenciesFallsBackWhenShowFails(t *testing.T) {
	t.Parallel()
	listed := &beads.Issue{ID: "gt-hooked", DependencyCount: 1}
	show := func(string) (*beads.Issue, error) { return nil, errors.New("bd show: exit 1") }
	if got := beadWithFullDependencies(listed, show); got != listed {
		t.Fatalf("beadWithFullDependencies() = %#v, want the listed bead after a failed show", got)
	}
}

// TestBeadWithFullDependenciesNilBead: findAgentWork can return a nil bead
// (no work hooked); the re-fetch helper must not panic on it.
func TestBeadWithFullDependenciesNilBead(t *testing.T) {
	t.Parallel()
	if got := beadWithFullDependencies(nil, failingShow(t)); got != nil {
		t.Fatalf("beadWithFullDependencies(nil) = %#v, want nil", got)
	}
}

// TestRenderDependencyMergeStatusSilentWithoutDependencies: a bead with no
// dependencies gets no dependency noise in its starting context.
func TestRenderDependencyMergeStatusSilentWithoutDependencies(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderDependencyMergeStatus(&buf, nil)
	if out := buf.String(); out != "" {
		t.Fatalf("expected no output, got %q", out)
	}
}

// TestRenderDependencyMergeStatusReportsUnknownHonestly: a blocker we could not
// check must not read as landed.
func TestRenderDependencyMergeStatusReportsUnknownHonestly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderDependencyMergeStatus(&buf, []beads.DependencyMergeStatus{
		{ID: "gt-unchecked", Status: "closed", State: beads.DependencyMergeUnknown},
	})
	out := buf.String()
	if !strings.Contains(out, "unknown") {
		t.Fatalf("unchecked blocker must report unknown, got:\n%s", out)
	}
	if strings.Contains(out, "landed") {
		t.Fatalf("unchecked blocker must never read as landed, got:\n%s", out)
	}
}

// installFakeBdShow is used only by spec_test.go (slice s-z, gt-jz03n.3);
// delete it with that use.
//
// installFakeBdShow puts a POSIX-shell `bd` on PATH whose `show` prints
// showJSON and exits with showExit, and returns the file that logs each
// invocation's argv (one per line). Not parallel-safe: it sets PATH.
func installFakeBdShow(t *testing.T, showJSON string, showExit int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fake bd")
	}
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	payload := filepath.Join(binDir, "show.json")
	if err := os.WriteFile(payload, []byte(showJSON), 0o644); err != nil {
		t.Fatalf("write fake show payload: %v", err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
		"case \"$1\" in\n" +
		"  show) cat '" + payload + "'; exit " + strconv.Itoa(showExit) + ";;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}
