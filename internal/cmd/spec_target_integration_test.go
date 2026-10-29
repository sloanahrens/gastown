//go:build integration

package cmd

// The path check's production read runs git, so its coverage of the read lives
// behind the integration tag: the unit tier runs no git (internal/testpolicy,
// realgit.txt), and spec_target_test.go pins the shape around the read with a
// fake opener. These tests pin the read itself — refs in a real bare repo —
// which no fake can show.

import (
	"os"
	"path/filepath"
	"testing"
)

// specTargetWork is a working repo on main carrying files.
func specTargetWork(t *testing.T, files ...string) string {
	t.Helper()
	work := t.TempDir()
	gitOut(t, work, "init", "-b", "main")
	gitOut(t, work, "config", "user.email", "spec-target@example.invalid")
	gitOut(t, work, "config", "user.name", "spec target")
	specTargetCommit(t, work, files...)
	return work
}

// specTargetCommit writes files and commits them on the branch HEAD names.
func specTargetCommit(t *testing.T, repo string, files ...string) {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(repo, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-m", "seed")
}

// specTargetBare clones work to dest as the bare repo a rig keeps at
// .repo.git, and points dest's origin/<branch> at work's branch as a fetch of
// the real remote would.
func specTargetBare(t *testing.T, work, dest, branch string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, filepath.Dir(dest), "clone", "--bare", work, dest)
	gitOut(t, dest, "fetch", work, branch)
	gitOut(t, dest, "update-ref", "refs/remotes/origin/"+branch, "FETCH_HEAD")
	return dest
}

// The inventory is origin's branch, read with git from the rig's shared repo:
// the branch a landing targets, not the local one a rig may have left behind
// (gt-7t1im).
func TestIntegrationSpecTargetTreeReadsOriginWithGit(t *testing.T) {
	t.Parallel()
	work := specTargetWork(t, "internal/cmd/spec.go")
	rigRoot := t.TempDir()
	bare := specTargetBare(t, work, filepath.Join(rigRoot, ".repo.git"), "main")

	// origin/main moves ahead of the bare repo's local main.
	ahead := t.TempDir()
	gitOut(t, ahead, "clone", bare, ahead)
	specTargetCommit(t, ahead, "internal/cmd/landed.go")
	gitOut(t, bare, "fetch", ahead, "main")
	gitOut(t, bare, "update-ref", "refs/remotes/origin/main", "FETCH_HEAD")

	target := specTargetTree(rigRoot, openRigTree)
	if target.Err != "" {
		t.Fatalf("Err = %q, want the origin inventory", target.Err)
	}
	if target.Branch != "main" {
		t.Errorf("Branch = %q, want main", target.Branch)
	}
	for _, want := range []string{"internal/cmd/spec.go", "internal/cmd/landed.go"} {
		if _, ok := target.Files[want]; !ok {
			t.Errorf("Files = %v, want %s", target.Files, want)
		}
	}
}

// The local branch answers when the bare repo has no origin ref, and a repo
// holding neither branch is a read that failed, carried as an error rather than
// read as a branch holding no files (gt-7t1im).
func TestIntegrationSpecTargetTreeFallsBackThenFailsLoudly(t *testing.T) {
	t.Parallel()
	work := specTargetWork(t, "internal/cmd/spec.go")
	rigRoot := t.TempDir()
	bare := specTargetBare(t, work, filepath.Join(rigRoot, ".repo.git"), "main")
	gitOut(t, bare, "update-ref", "-d", "refs/remotes/origin/main")

	target := specTargetTree(rigRoot, openRigTree)
	if target.Err != "" {
		t.Fatalf("Err = %q, want the local inventory", target.Err)
	}
	if _, ok := target.Files["internal/cmd/spec.go"]; !ok {
		t.Errorf("Files = %v, want the local branch's files", target.Files)
	}

	gitOut(t, bare, "update-ref", "-d", "refs/heads/main")
	target = specTargetTree(rigRoot, openRigTree)
	if target.Err == "" || len(target.Files) != 0 {
		t.Fatalf("target = %+v, want an error and no files", target)
	}
}

// gt spec lint asks the bead's rig for its inventory: a real town layout, a
// real bare repo under the rig, and the file the bead's Constraints names is
// read from origin/main (gt-7t1im).
func TestIntegrationSpecLintTargetReadsTheBeadsRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	work := specTargetWork(t, "internal/cmd/spec.go")
	specTargetBare(t, work, filepath.Join(townRoot, "gastown", "mayor", "rig"), "main")

	target := specLintTarget(townRoot, "gt-bead", openRigTree)
	if target.Err != "" {
		t.Fatalf("Err = %q, want the rig's inventory", target.Err)
	}
	if target.Branch != "main" {
		t.Errorf("Branch = %q, want main", target.Branch)
	}
	if _, ok := target.Files["internal/cmd/spec.go"]; !ok {
		t.Errorf("Files = %v, want internal/cmd/spec.go", target.Files)
	}
}
