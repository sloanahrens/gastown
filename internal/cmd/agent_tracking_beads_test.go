package cmd

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// agentTrackingTown lays out a town whose refinery rig worktree redirects
// its .beads to the rig's mayor clone, and returns the town root, the town
// beads, the rig worktree and the rig beads it redirects to.
func agentTrackingTown(t *testing.T) (townRoot, townBeads, rigWorkDir, rigBeads string) {
	t.Helper()
	townRoot = filepath.Join(t.TempDir(), "gt")
	townBeads = filepath.Join(townRoot, ".beads")
	rigWorkDir = filepath.Join(townRoot, "gastown", "refinery", "rig")
	rigRedirect := filepath.Join(rigWorkDir, ".beads")
	rigBeads = filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	for _, dir := range []string{townBeads, rigRedirect, rigBeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(rigRedirect, "redirect"), []byte("../../mayor/rig/.beads"), 0o644); err != nil {
		t.Fatalf("write rig redirect: %v", err)
	}
	return townRoot, townBeads, rigWorkDir, rigBeads
}

func TestResolveAgentTrackingBeadsDirPrefersCwdRigRedirectOverBeadsDir(t *testing.T) {
	t.Parallel()
	townRoot, townBeads, rigWorkDir, rigBeads := agentTrackingTown(t)

	gotWorkDir, err := findBeadsWorkDirFrom(rigWorkDir)
	if err != nil || gotWorkDir != rigWorkDir {
		t.Fatalf("findBeadsWorkDirFrom() = %q, %v; want %q", gotWorkDir, err, rigWorkDir)
	}

	gotBeadsDir, err := resolveAgentTrackingBeadsDirFrom(rigWorkDir, townBeads)
	if err != nil || gotBeadsDir != rigBeads {
		t.Fatalf("resolveAgentTrackingBeadsDirFrom() = %q, %v; want %q", gotBeadsDir, err, rigBeads)
	}

	// The project-work resolver keeps the inherited BEADS_DIR first.
	gotLocalWorkDir, err := localBeadsWorkDir(rigWorkDir, townBeads)
	if err != nil || gotLocalWorkDir != townRoot {
		t.Fatalf("localBeadsWorkDir() = %q, %v; want env parent %q", gotLocalWorkDir, err, townRoot)
	}
}

// TestResolveAgentTrackingBeadsDirFallsBackToBeadsDir: with no .beads above
// the cwd, the inherited BEADS_DIR is the database.
func TestResolveAgentTrackingBeadsDirFallsBackToBeadsDir(t *testing.T) {
	t.Parallel()
	_, townBeads, _, _ := agentTrackingTown(t)
	got, err := resolveAgentTrackingBeadsDirFrom(t.TempDir(), townBeads)
	if err != nil || got != townBeads {
		t.Fatalf("resolveAgentTrackingBeadsDirFrom() = %q, %v; want %q", got, err, townBeads)
	}
}

// TestModifyAgentStateRewritesLabels: a state change keeps the colon-free
// labels, applies the operation and stamps the heartbeat.
func TestModifyAgentStateRewritesLabels(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-gastown-refinery", Labels: []string{"gt:agent", "idle:2", "keep"}})
	if err := modifyAgentState(db, io.Discard, "gt-gastown-refinery", agentLabelOps{set: []string{"idle=0"}}, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("modifyAgentState() error = %v", err)
	}
	got, err := db.Show("gt-gastown-refinery")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gt:agent", "heartbeat:1700000000", "idle:0", "keep"}; !slices.Equal(got.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Labels, want)
	}
}
