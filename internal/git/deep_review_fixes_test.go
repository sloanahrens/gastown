package git

import (
	"reflect"
	"slices"
	"testing"
)

// ls-remote matches its pattern against a trailing path component, so asking
// for integration/foo also lists refs/heads/team/integration/foo. Only an
// exact refs/heads/<branch> line means the branch exists.
func TestRemoteBranchExists_NoSuffixMatch(t *testing.T) {
	t.Parallel()
	const sha = "5e06d1a1792be1930476f5a334963d4de44e8ea3"
	suffixOnly := ok(sha + "\trefs/heads/team/integration/foo\n")

	s := plainRemotes().on("ls-remote --heads origin integration/foo", suffixOnly)
	if got, err := newTestGit(t, s).RemoteBranchExists("origin", "integration/foo"); err != nil || got {
		t.Fatalf("RemoteBranchExists with only a suffix match = %v, %v; want false", got, err)
	}

	s = splitRemotes().on("ls-remote --heads /srv/fork.git integration/foo", suffixOnly)
	if got, err := newTestGit(t, s).PushRemoteBranchExists("origin", "integration/foo"); err != nil || got {
		t.Fatalf("PushRemoteBranchExists with only a suffix match = %v, %v; want false", got, err)
	}

	exact := ok(sha + "\trefs/heads/team/integration/foo\n" + sha + "\trefs/heads/integration/foo\n")
	s = plainRemotes().on("ls-remote --heads origin integration/foo", exact)
	if got, err := newTestGit(t, s).RemoteBranchExists("origin", "integration/foo"); err != nil || !got {
		t.Fatalf("RemoteBranchExists with the exact ref = %v, %v; want true", got, err)
	}
	s = splitRemotes().on("ls-remote --heads /srv/fork.git integration/foo", exact)
	if got, err := newTestGit(t, s).PushRemoteBranchExists("origin", "integration/foo"); err != nil || !got {
		t.Fatalf("PushRemoteBranchExists with the exact ref = %v, %v; want true", got, err)
	}
}

// A package directory named coverage or .cache holds source; only generated
// output under those names is a runtime artifact.
func TestRuntimeArtifactRoot_SourceDirNamedCoverage(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"pkg/coverage/collector.go", "internal/.cache/store.py", "coverage/report.rs"} {
		if root, ok := runtimeArtifactRoot(path); ok {
			t.Errorf("runtimeArtifactRoot(%q) = %q, true; want a source file", path, root)
		}
	}
	for path, want := range map[string]string{
		"coverage/lcov.info":                 "coverage/",
		"web/coverage/lcov-report/sorter.js": "web/coverage/",
		"htmlcov/index.html":                 "htmlcov/",
		"pkg/.cache/blob.bin":                "pkg/.cache/",
	} {
		if root, ok := runtimeArtifactRoot(path); !ok || root != want {
			t.Errorf("runtimeArtifactRoot(%q) = %q, %v; want %q, true", path, root, ok, want)
		}
	}

	s := &UncommittedWorkStatus{ModifiedFiles: []string{"pkg/coverage/collector.go", "coverage/lcov.info"}}
	if got := s.NonRuntimePaths(); !slices.Equal(got, []string{"pkg/coverage/collector.go"}) {
		t.Errorf("NonRuntimePaths = %q, want the source file only", got)
	}
}

// Only the mode fields of a raw diff line say whether it is a gitlink; a blob
// whose path contains 160000 is not a submodule change.
func TestSubmoduleChanges_IgnoresBlobWith160000InPath(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --raw main feature": ok(":000000 100644 0000000 1111111 A\ttestdata/160000.json\n" +
			":100644 100644 160000a 160000b M\tdocs/notes.md\n"),
	})
	changes, err := newTestGit(t, s).SubmoduleChanges("main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("SubmoduleChanges = %+v, want none", changes)
	}
}

// A submodule whose default branch is release/1.0 is pushed to that branch,
// not to a stray branch named 1.0.
func TestSubmoduleDefaultBranchSlashBranch(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"-C /srv/sub symbolic-ref refs/remotes/origin/HEAD": ok("refs/remotes/origin/release/1.0\n"),
	})
	if b, err := submoduleDefaultBranch(s.run, "/srv/sub", "origin"); err != nil || b != "release/1.0" {
		t.Fatalf("submoduleDefaultBranch = %q, %v; want release/1.0", b, err)
	}
}

// git C-quotes a non-ASCII name in --name-only output, so the list has to be
// unquoted before it is handed back to git as pathspecs.
func TestStagedDeletionsNonASCIIPath(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --cached --name-only --diff-filter=D": ok("\"caf\\303\\251.go\"\nplain.go\n"),
	})
	got, err := newTestGit(t, s).StagedDeletions()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"café.go", "plain.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StagedDeletions = %q, want %q", got, want)
	}
}

func TestDiffNameOnlyNonASCIIPath(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --name-only main...feature": ok("\"internal/daemon/caf\\303\\251.go\"\nREADME.md\n"),
	})
	got, err := newTestGit(t, s).DiffNameOnly("main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/daemon/café.go", "README.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffNameOnly = %q, want %q", got, want)
	}
}
