package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsThrowawayPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		// The gt-h1tq/gt-ozo4 report's own file.
		{"livecheck scratch test", "internal/util/zz_livecheck_test.go", true},
		{"zz prefix at root", "zz_probe.go", true},
		{"zz infix", "internal/foo/bar_zz_probe.go", true},
		{"tmp copy", "internal/util/client_tmp.go", true},
		{"tmp without a boundary", "client_tmpbak.go", false},
		{"tmp suffix", "internal/util/client_tmp", true},
		{"tmp infix", "client_tmp_v2.go", true},
		{".tmp extension", "scratch.tmp", true},
		{".temp extension", "notes.temp", true},
		{"emacs backup", "internal/util/client.go~", true},
		{"bak", "config.bak", true},
		{"orig", "patch.orig", true},
		{"rej", "patch.rej", true},
		{"vim swap", ".client.go.swp", true},
		{"vim swap swo", "client.swo", true},
		{"emacs lock", ".#client.go", true},
		{"emacs autosave", "#client.go#", true},
		{"scratch dir", "scratch/notes.md", true},
		{"tmp dir", "cmd/tmp/probe.go", true},
		{"scratchpad dir", "scratchpad/report.txt", true},
		{"dir segment case-insensitive", "Scratch/notes.md", true},

		// Real work must never be swept up.
		{"go source", "internal/checkpoint/throwaway.go", false},
		{"test source", "internal/checkpoint/throwaway_test.go", false},
		{"go template", "foo_tmpl.go", false},
		{"tmpl dir", "internal/tmpl/render.go", false},
		{"markdown", "docs/design/notes.md", false},
		{"nested real dir", "internal/daemon/checkpoint_dog.go", false},
		{"dotfile", ".gitignore", false},
		{"name containing tmp without boundary", "attempt.go", false},
		{"empty", "", false},
		{"root-level go file", "main.go", false},

		// Directory segments only count when they are a whole segment.
		{"tmp as a name prefix", "tmpdir/file.go", false},
		{"tmp inside a segment", "internal/attmp/file.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsThrowawayPath(tt.path); got != tt.want {
				t.Errorf("IsThrowawayPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestThrowawayPaths(t *testing.T) {
	got := ThrowawayPaths([]string{
		"internal/util/client.go",
		"internal/util/zz_probe_test.go",
		"internal/util/zz_probe_test.go", // duplicate
		"scratch/notes.md",
		"internal/util/server.go_tmp",
	})
	want := []string{"internal/util/zz_probe_test.go", "scratch/notes.md", "internal/util/server.go_tmp"}
	if len(got) != len(want) {
		t.Fatalf("ThrowawayPaths() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ThrowawayPaths()[%d] = %q, want %q (all: %#v)", i, got[i], want[i], got)
		}
	}
}

// TestAddedThrowawayPaths_ReportsOnlyAdditions pins the boundary of the rule: a
// repository that already tracks a matching name keeps working, because a
// modification is real work and only an addition is something the branch brings
// to the target.
func TestAddedThrowawayPaths_ReportsOnlyAdditions(t *testing.T) {
	dir := initTestRepo(t)
	// Tracked on the base branch, name notwithstanding.
	addCommit(t, dir, "zz_fixture_test.go", "package main\n", "tracked fixture")
	base := mustGitOutput(t, dir, "rev-parse", "HEAD")

	createBranch(t, dir, "polecat/garnet/gt-ozo4")
	// Modify the tracked file: not an addition, so not reported.
	writeRepoFile(t, dir, "zz_fixture_test.go", "package main\n\n// real work\n")
	mustGit(t, dir, "add", "zz_fixture_test.go")
	mustGit(t, dir, "commit", "-m", "real work on the tracked fixture")
	addCommitInDir(t, dir, "internal/util/client.go", "package util\n", "real work")
	addCommitInDir(t, dir, "internal/util/zz_livecheck_test.go", "package util\n", "WIP: checkpoint (auto)")
	addCommit(t, dir, "notes.tmp", "scratch\n", "WIP: checkpoint (auto)")

	found, err := AddedThrowawayPaths(dir, base, "HEAD")
	if err != nil {
		t.Fatalf("AddedThrowawayPaths: %v", err)
	}
	want := []string{"internal/util/zz_livecheck_test.go", "notes.tmp"}
	if len(found) != len(want) {
		t.Fatalf("AddedThrowawayPaths() = %#v, want %#v", found, want)
	}
	for i := range want {
		if found[i] != want[i] {
			t.Fatalf("AddedThrowawayPaths()[%d] = %q, want %q (all: %#v)", i, found[i], want[i], found)
		}
	}
}

func TestAddedThrowawayPaths_CleanBranch(t *testing.T) {
	dir := initTestRepo(t)
	base := mustGitOutput(t, dir, "rev-parse", "HEAD")
	createBranch(t, dir, "polecat/garnet/gt-ozo4")
	addCommitInDir(t, dir, "internal/util/client.go", "package util\n", "real work (gt-ozo4)")

	found, err := AddedThrowawayPaths(dir, base, "HEAD")
	if err != nil {
		t.Fatalf("AddedThrowawayPaths: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("AddedThrowawayPaths() = %#v, want none", found)
	}
}

// TestAddedThrowawayPaths_UncommittedDeletionStillReports covers the shape
// gt-ozo4 was filed under: the checkpoint committed the file and the polecat
// deleted it while cleaning up. The blob is still in HEAD's tree, and HEAD's
// tree is what the squash submits, so the answer must stay positive — this is
// the report's "deleting it afterwards does not help on its own".
func TestAddedThrowawayPaths_UncommittedDeletionStillReports(t *testing.T) {
	dir := initTestRepo(t)
	base := mustGitOutput(t, dir, "rev-parse", "HEAD")
	createBranch(t, dir, "polecat/garnet/gt-ozo4")
	addCommitInDir(t, dir, "internal/util/zz_livecheck_test.go", "package util\n", "WIP: checkpoint (auto)")

	if err := os.Remove(filepath.Join(dir, "internal/util/zz_livecheck_test.go")); err != nil {
		t.Fatal(err)
	}

	found, err := AddedThrowawayPaths(dir, base, "HEAD")
	if err != nil {
		t.Fatalf("AddedThrowawayPaths: %v", err)
	}
	if len(found) != 1 || found[0] != "internal/util/zz_livecheck_test.go" {
		t.Fatalf("AddedThrowawayPaths() = %#v, want the scratch file still reported", found)
	}
}

// TestAddedThrowawayPaths_CommittedDeletionClears pins the remediation the
// refusal prescribes: the file is staged out of the branch, not out of the
// working tree, and the answer goes quiet. If this stops holding, the refusal
// is telling polecats to run a command that does not clear it.
func TestAddedThrowawayPaths_CommittedDeletionClears(t *testing.T) {
	dir := initTestRepo(t)
	base := mustGitOutput(t, dir, "rev-parse", "HEAD")
	createBranch(t, dir, "polecat/garnet/gt-ozo4")
	addCommitInDir(t, dir, "internal/util/zz_livecheck_test.go", "package util\n", "WIP: checkpoint (auto)")
	if err := os.Remove(filepath.Join(dir, "internal/util/zz_livecheck_test.go")); err != nil {
		t.Fatal(err)
	}

	mustGit(t, dir, "rm", "--cached", "--", "internal/util/zz_livecheck_test.go")
	mustGit(t, dir, "commit", "-m", "remove throwaway files")

	found, err := AddedThrowawayPaths(dir, base, "HEAD")
	if err != nil {
		t.Fatalf("AddedThrowawayPaths: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("AddedThrowawayPaths() = %#v, want none after the deletion was committed", found)
	}
}

// TestAddedThrowawayPaths_UnresolvableBaseFailsClosed is the fail-closed
// contract: an answer the caller cannot get is not the empty answer. The
// submit gate refuses on this error rather than landing the branch unchecked.
func TestAddedThrowawayPaths_UnresolvableBaseFailsClosed(t *testing.T) {
	dir := initTestRepo(t)

	if _, err := AddedThrowawayPaths(dir, "origin/main", "HEAD"); err == nil {
		t.Fatal("AddedThrowawayPaths with an unresolvable baseRef returned nil error, want failure")
	}
}

func TestAddedThrowawayPaths_FindsPathWithSpaces(t *testing.T) {
	dir := initTestRepo(t)
	base := mustGitOutput(t, dir, "rev-parse", "HEAD")
	createBranch(t, dir, "polecat/garnet/gt-ozo4")
	addCommitInDir(t, dir, "scratch/notes copy.md", "scratch\n", "WIP: checkpoint (auto)")

	found, err := AddedThrowawayPaths(dir, base, "HEAD")
	if err != nil {
		t.Fatalf("AddedThrowawayPaths: %v", err)
	}
	if len(found) != 1 || found[0] != "scratch/notes copy.md" {
		t.Fatalf("AddedThrowawayPaths() = %#v, want [scratch/notes copy.md]", found)
	}
}

// addCommitInDir is addCommit for paths whose parent directory does not exist
// yet (initTestRepo's repo has none).
func addCommitInDir(t *testing.T, dir, filename, content, msg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filename)), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, dir, filename, content, msg)
}

// mustGitOutput runs a git command and returns trimmed stdout.
func mustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}
