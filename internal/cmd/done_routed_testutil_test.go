package cmd

// Helpers the cmd tier's gt done end-to-end tests share. gt done's submission
// logic now lives in internal/done, which carries its own copies of these
// (om finding 1); the cmd tier cannot import a _test.go helper file from
// another package, so the tests that drive runDone and the cmd-level flag vars
// keep theirs here.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// doneTestBranch is the branch the routed-submit tests run gt done on.
const doneTestBranch = "feature/routed-submit"

// setupRoutedSourceTestTown builds a town whose bd-source routes to a
// different rig than the caller's, and returns the caller's workdir and the
// two beads directories.
func setupRoutedSourceTestTown(t *testing.T) (workDir, currentBeadsDir, ownerBeadsDir string) {
	t.Helper()
	townRoot := t.TempDir()
	// Resolve symlinks now so every path derived below matches what
	// resolveDonePolecatWorktreeAt produces: it canonicalizes cwd via
	// filepath.EvalSymlinks before deriving BEADS_DIR routing. On macOS,
	// t.TempDir() lives under /var/folders/..., a symlink to
	// /private/var/folders/...; without this, runDone's canonicalized cwd
	// diverges from the non-canonical paths the bd stub below expects,
	// and every routed bd lookup falsely reports "issue not found".
	if resolved, err := filepath.EvalSymlinks(townRoot); err == nil {
		townRoot = resolved
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write town sentinel: %v", err)
	}

	workDir = filepath.Join(townRoot, "gastown", "polecats", "refuge", "gastown")
	currentBeadsDir = filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	ownerBeadsDir = filepath.Join(townRoot, "beads", "mayor", "rig", ".beads")
	townBeadsDir := filepath.Join(townRoot, ".beads")
	for _, dir := range []string{filepath.Join(workDir, ".beads"), currentBeadsDir, ownerBeadsDir, townBeadsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, ".beads", "redirect"), []byte("../../../mayor/rig/.beads\n"), 0o644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	return workDir, currentBeadsDir, ownerBeadsDir
}

func routedSourceTestTownRoot(workDir string) string {
	return filepath.Clean(filepath.Join(workDir, "..", "..", "..", ".."))
}

func assertBDLogContains(t *testing.T, log, beadsDir, args string) {
	t.Helper()
	needle := beadsDir + "\t" + args
	if !strings.Contains(log, needle) {
		t.Fatalf("bd log missing %q:\n%s", needle, log)
	}
}

func assertBDLogNotContains(t *testing.T, log, beadsDir, args string) {
	t.Helper()
	needle := beadsDir + "\t" + args
	if strings.Contains(log, needle) {
		t.Fatalf("bd log unexpectedly contains %q:\n%s", needle, log)
	}
}
