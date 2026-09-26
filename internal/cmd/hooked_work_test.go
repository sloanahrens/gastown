package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestResolveHookLookupWorkDirUsesRouteOwnedRigDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir rig beads: %v", err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	localWorkDir := filepath.Join(townRoot, "gastown", "polecats", "toast")
	got := resolveHookLookupWorkDir(localWorkDir, "gastown/refinery", townRoot)
	if got != rigDir {
		t.Fatalf("resolveHookLookupWorkDir() = %q, want %q", got, rigDir)
	}
}

func TestResolveHookLookupWorkDirLeavesTownLevelTargetLocal(t *testing.T) {
	t.Parallel()
	workDir := filepath.Join(t.TempDir(), "mayor")
	got := resolveHookLookupWorkDir(workDir, "mayor/", t.TempDir())
	if got != workDir {
		t.Fatalf("resolveHookLookupWorkDir() = %q, want %q", got, workDir)
	}
}

func TestResolveHookLookupWorkDirRejectsUnsafeTargetPath(t *testing.T) {
	t.Parallel()
	workDir := filepath.Join(t.TempDir(), "gastown", "polecats", "toast")
	townRoot := t.TempDir()

	for _, target := range []string{"..", "../x", "gastown/../x", "/tmp/x", `gastown\..\x`, "."} {
		t.Run(target, func(t *testing.T) {
			got := resolveHookLookupWorkDir(workDir, target, townRoot)
			if got != workDir {
				t.Fatalf("resolveHookLookupWorkDir(%q) = %q, want local %q", target, got, workDir)
			}
		})
	}
}

func TestResolveHookLookupWorkDirIgnoresEscapingRoute(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "gt-", Path: "gastown/../../outside"},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	workDir := filepath.Join(townRoot, "gastown", "polecats", "toast")
	got := resolveHookLookupWorkDir(workDir, "gastown/refinery", townRoot)
	want := filepath.Join(townRoot, "gastown")
	if got != want {
		t.Fatalf("resolveHookLookupWorkDir() = %q, want safe fallback %q", got, want)
	}
}

func TestResolveHookLookupWorkDirUsesSafeUnknownRigFallback(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	workDir := filepath.Join(townRoot, "gastown", "polecats", "toast")
	got := resolveHookLookupWorkDir(workDir, "other/refinery", townRoot)
	want := filepath.Join(townRoot, "other")
	if got != want {
		t.Fatalf("resolveHookLookupWorkDir() = %q, want %q", got, want)
	}
}

func TestActiveWorkStatusesPreferHookedOverInProgress(t *testing.T) {
	t.Parallel()
	got := activeWorkStatuses()
	want := []beads.IssueStatus{beads.IssueStatusHooked, beads.StatusInProgress}
	if len(got) != len(want) {
		t.Fatalf("activeWorkStatuses length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("activeWorkStatuses()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestActiveWorkMergeBeadListsDedupeAndSort(t *testing.T) {
	t.Parallel()
	primary := []*beads.Issue{
		{ID: "gt-older", UpdatedAt: "2026-01-01T00:00:00Z"},
		{ID: "gt-same", UpdatedAt: "2026-01-02T00:00:00Z", Title: "durable"},
		{ID: "gt-whole", UpdatedAt: "2026-01-03T00:00:00Z"},
	}
	secondary := []*beads.Issue{
		{ID: "gt-fractional", UpdatedAt: "2026-01-03T00:00:00.1Z"},
		{ID: "gt-same", UpdatedAt: "2026-01-04T00:00:00Z", Title: "wisp"},
	}

	got := mergeBeadLists(primary, secondary)
	if len(got) != 4 {
		t.Fatalf("mergeBeadLists length = %d, want 4", len(got))
	}

	wantIDs := []string{"gt-fractional", "gt-whole", "gt-same", "gt-older"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("mergeBeadLists[%d].ID = %q, want %q (all=%v)", i, got[i].ID, want, got)
		}
	}
	if got[2].Title != "durable" {
		t.Fatalf("duplicate should keep primary issue, got title %q", got[2].Title)
	}
}

// The single-query rewrite merges both statuses into one result set, so the
// hooked-outranks-in_progress rule the old per-status loop got for free has to
// live somewhere. Callers read [0] as "the" hook, so it is load-bearing.
func TestPreferHookedKeepsHookedAheadOfNewerInProgress(t *testing.T) {
	t.Parallel()
	assignments := []*beads.Issue{
		{ID: "gt-inprog-newer", Status: string(beads.StatusInProgress), UpdatedAt: "2026-01-05T00:00:00Z"},
		{ID: "gt-hooked-older", Status: beads.StatusHooked, UpdatedAt: "2026-01-01T00:00:00Z"},
	}

	got := preferHooked(assignments)
	if len(got) != 1 || got[0].ID != "gt-hooked-older" {
		t.Fatalf("preferHooked() = %v, want only gt-hooked-older", got)
	}
}

func TestPreferHookedFallsBackToInProgressNewestFirst(t *testing.T) {
	t.Parallel()
	assignments := []*beads.Issue{
		{ID: "gt-inprog-older", Status: string(beads.StatusInProgress), UpdatedAt: "2026-01-01T00:00:00Z"},
		{ID: "gt-inprog-newer", Status: string(beads.StatusInProgress), UpdatedAt: "2026-01-05T00:00:00Z"},
	}

	got := preferHooked(assignments)
	wantIDs := []string{"gt-inprog-newer", "gt-inprog-older"}
	if len(got) != len(wantIDs) {
		t.Fatalf("preferHooked() length = %d, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("preferHooked()[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
}
