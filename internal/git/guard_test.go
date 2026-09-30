package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// townRoot makes a directory that isTownRoot recognizes, with the runtime
// directories the guard protects.
func townRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{"mayor/town.json", ".runtime/sentinel", ".dolt-data/x", ".beads/metadata.json", "daemon/daemon.pid"} {
		path := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// inTownRoot answers the guard's top-level probe for dir with root, the way
// git answers it for a directory inside the town root's repository.
func inTownRoot(s *scripted, dir, root string) *scripted {
	return s.on("-C "+dir+" rev-parse --show-toplevel", ok(root+"\n"))
}

func requireUnsafe(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnsafeTownRootGitMutation) {
		t.Fatalf("error = %v, want ErrUnsafeTownRootGitMutation", err)
	}
}

func TestTownRootMutatingGitCommandsAreBlocked(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		run  func(*Git) error
	}{
		{"checkout", func(g *Git) error { return g.Checkout("polecat/safety") }},
		{"checkout new branch", func(g *Git) error { return g.CheckoutNewBranch("polecat/new", "polecat/safety") }},
		{"checkout reset branch", func(g *Git) error { return g.CheckoutResetBranch("polecat/reset", "polecat/safety") }},
		{"checkout detach force", func(g *Git) error { return g.CheckoutDetachForce("polecat/safety") }},
		{"reset hard", func(g *Git) error { return g.ResetHard("polecat/safety") }},
		{"clean force", func(g *Git) error { return g.CleanForce() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := townRoot(t)
			s := inTownRoot(newScripted(nil), root, root)
			err := tt.run(&Git{workDir: root, exec: s.run})
			requireUnsafe(t, err)
			if sent := s.sent(); len(sent) != 1 {
				t.Fatalf("git calls = %q, want only the top-level probe", sent)
			}
		})
	}
}

func TestTownRootReadOnlyStashListIsAllowed(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	s := inTownRoot(newScripted(map[string]reply{"stash list": ok("")}), root, root)
	count, err := (&Git{workDir: root, exec: s.run}).StashCount()
	if err != nil || count != 0 {
		t.Fatalf("StashCount = %d, %v; want 0, nil", count, err)
	}
	if !s.hasSent("stash list") {
		t.Fatalf("stash list was not sent: %q", s.sent())
	}
}

// A work dir nested under the town root resolves, through git, to the town
// root's repository; the guard judges the repository, not the directory.
func TestNestedWorkDirResolvingToTownRootGitIsBlocked(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	rigDir := filepath.Join(root, "gastown")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := inTownRoot(newScripted(nil), rigDir, root)
	g := &Git{workDir: rigDir, exec: s.run}
	requireUnsafe(t, g.Checkout("polecat/safety"))
	requireUnsafe(t, g.ResetHard("polecat/safety"))
	requireUnsafe(t, g.CleanForce())
	for _, c := range s.sent() {
		if c != "-C "+rigDir+" rev-parse --show-toplevel" {
			t.Fatalf("a mutating call reached git: %q", c)
		}
	}
}

// A mutation in an ordinary repository goes through.
func TestMutationOutsideTownRootIsAllowed(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"reset --hard HEAD~1": ok("")})
	g := newTestGit(t, s)
	s.on("-C "+g.workDir+" rev-parse --show-toplevel", ok(g.workDir+"\n"))
	if err := g.ResetHard("HEAD~1"); err != nil {
		t.Fatalf("ResetHard: %v", err)
	}
	if !s.hasSent("reset --hard HEAD~1") {
		t.Fatalf("reset was not sent: %q", s.sent())
	}
}

func TestWorktreeAddCannotTargetTownRootRuntimePaths(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	link := filepath.Join(t.TempDir(), "townlink")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		root,
		filepath.Join(root, "mayor", "bad-worktree"),
		filepath.Join(root, ".dolt-data", "bad-worktree"),
		filepath.Join(root, ".runtime", "bad-worktree"),
		filepath.Join(root, ".beads", "bad-worktree"),
		filepath.Join(root, "daemon", "bad-worktree"),
		filepath.Join(link, ".runtime", "linked-worktree"),
	} {
		s := newScripted(nil)
		g := &Git{gitDir: filepath.Join(t.TempDir(), ".repo.git"), exec: s.run}
		requireUnsafe(t, g.WorktreeAddFromRef(target, "polecat/town-root", "HEAD"))
		for _, c := range s.sent() {
			if len(c) > 0 && c != "-C "+target+" rev-parse --show-toplevel" {
				t.Fatalf("worktree add toward %s reached git: %q", target, s.sent())
			}
		}
	}
}

func TestCloneCannotTargetTownRootRuntimePaths(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	link := filepath.Join(t.TempDir(), "townlink")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	s := newScripted(nil)
	g := newTestGit(t, s)
	requireUnsafe(t, g.Clone("/srv/src.git", root))
	requireUnsafe(t, g.Clone("/srv/src.git", filepath.Join(link, ".dolt-data", "clone")))

	// A relative destination resolves against the process's working
	// directory, so name the town root's runtime dir relative to it.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, filepath.Join(root, ".runtime", "relative-clone"))
	if err != nil || filepath.IsAbs(rel) {
		t.Fatalf("relative path to town root: %q, %v", rel, err)
	}
	requireUnsafe(t, g.Clone("/srv/src.git", rel))
	if sent := s.sent(); len(sent) != 0 {
		t.Fatalf("a refused clone reached git: %q", sent)
	}
}
