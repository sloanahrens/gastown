package git

import "testing"

// cleanWorkdir returns a Git on dir, which is outside any town root, and a
// scripted runner already answering the guard's top-level probe for it.
func cleanWorkdir(t *testing.T, dir string) (*Git, *scripted) {
	t.Helper()
	s := newScripted(map[string]reply{
		"-C " + dir + " rev-parse --show-toplevel": ok(dir + "\n"),
	})
	return &Git{workDir: dir, exec: s.run}, s
}

// TestCleanForceIgnoredSweepsIgnoredFiles pins the argv: the sweep worktree is
// reused from cycle to cycle, so a leftover that .gitignore hides (a stale
// build artifact, a cover profile) survives CleanForce and can decide a GREEN
// verdict no commit earned (gt-oyrav).
func TestCleanForceIgnoredSweepsIgnoredFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	g, s := cleanWorkdir(t, dir)
	s.on("clean -fdx", ok(""))

	if err := g.CleanForceIgnored(); err != nil {
		t.Fatalf("CleanForceIgnored: %v", err)
	}
	if !s.hasSent("clean -fdx") {
		t.Errorf("clean -fdx was not sent: %q", s.sent())
	}
	s.noUnscripted(t)
}

func TestCleanForceIgnoredReportsAFailedClean(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	g, s := cleanWorkdir(t, dir)
	s.on("clean -fdx", fail(128, "fatal: not a git repository\n"))

	if err := g.CleanForceIgnored(); err == nil {
		t.Fatal("CleanForceIgnored succeeded on a failed clean")
	}
}
