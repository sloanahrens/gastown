package git

import (
	"errors"
	"reflect"
	"slices"
	"strings"
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

// git reset on a whole coverage/ directory unstages the source beside the
// artifact, so a directory that holds source is narrowed to the artifact's own
// subtree; pure output directories still reset as one pathspec.
func TestRuntimeArtifactPathspecs_MixedCoverageDirKeepsSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{
			name:  "fixture beside source is work, not output",
			paths: []string{"internal/coverage/collector.go", "internal/coverage/testdata/report.golden"},
			want:  nil,
		},
		{
			name:  "output beside source stays one pathspec",
			paths: []string{"internal/coverage/collector.go", "internal/coverage/testdata/report.golden", "internal/coverage/coverage.out"},
			want:  []string{"internal/coverage/coverage.out"},
		},
		{
			name:  "output in a directory with a fixture keeps the fixture out of the pathspec",
			paths: []string{"coverage/main.go", "coverage/lcov-report/index.html", "coverage/lcov-report/testdata/sample.golden"},
			want:  []string{"coverage/lcov-report/index.html"},
		},
		{
			name:  "artifact in the source directory itself",
			paths: []string{"pkg/.cache/store.py", "pkg/.cache/blob.bin", "pkg/.cache/other.bin"},
			want:  []string{"pkg/.cache/blob.bin", "pkg/.cache/other.bin"},
		},
		{
			name:  "output tree below a source directory stays one pathspec",
			paths: []string{"coverage/main.go", "coverage/lcov-report/index.html", "coverage/lcov-report/css/a.css"},
			want:  []string{"coverage/lcov-report/"},
		},
		{
			name:  "source in a sibling subtree does not narrow",
			paths: []string{"coverage/lcov.info", "coverage/lcov-report/index.html"},
			want:  []string{"coverage/"},
		},
		{
			name:  "source under a different coverage directory",
			paths: []string{"pkg/coverage/collector.go", "web/coverage/lcov.info"},
			want:  []string{"web/coverage/"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RuntimeArtifactPathspecs(tc.paths)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("RuntimeArtifactPathspecs(%q) = %q, want %q", tc.paths, got, tc.want)
			}
			for _, src := range tc.paths {
				if !isSourceCodePath(src) || isGasTownRuntimePath(src) {
					continue
				}
				for _, spec := range got {
					if strings.HasPrefix(src, spec) {
						t.Errorf("pathspec %q covers source file %q", spec, src)
					}
				}
			}
		})
	}
}

// A fixture beside tracked source is work, not output: the status must not
// count it as a runtime artifact, or gt done passes its clean check and the
// fixture never lands.
func TestUncommittedWorkStatus_FixtureBesideTrackedSourceIsNotArtifact(t *testing.T) {
	t.Parallel()
	fixture := "internal/coverage/testdata/report.golden"
	s := &UncommittedWorkStatus{
		HasUncommittedChanges: true,
		UntrackedFiles:        []string{fixture, "web/coverage/lcov.info"},
		beside:                sourceBeside{roots: []string{"internal/coverage/"}},
	}
	if got := s.NonRuntimePaths(); !slices.Equal(got, []string{fixture}) {
		t.Errorf("NonRuntimePaths = %q, want the fixture", got)
	}
	if s.CleanExcludingRuntime() {
		t.Error("CleanExcludingRuntime = true; the fixture must block")
	}
	if got := s.RuntimeArtifactPaths(); !slices.Equal(got, []string{"web/coverage/"}) {
		t.Errorf("RuntimeArtifactPaths = %q, want only web/coverage/", got)
	}

	s.beside = sourceBeside{}
	if !s.CleanExcludingRuntime() {
		t.Error("without tracked source both files are artifacts")
	}
	if got := s.NonRuntimePaths(); len(got) != 0 {
		t.Errorf("NonRuntimePaths without tracked source = %q, want none", got)
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
		"diff --name-only --no-renames main...feature": ok("\"internal/daemon/caf\\303\\251.go\"\nREADME.md\n"),
	})
	got, err := newTestGit(t, s).DiffNameOnly("main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/daemon/café.go", "README.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffNameOnly = %q, want %q", got, want)
	}
}

// A move out of a rule's reach is invisible when git pairs it: --name-only
// with rename detection on prints the destination alone, so a caller reading
// the list against its rules loses the path that left. The call must ask for
// both names (gt-j8m6g).
func TestDiffNameOnlyNoRenames(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --name-only --no-renames main...moved": ok("internal/daemon/x.go\ninternal/misc/x.go\n"),
	})
	got, err := newTestGit(t, s).DiffNameOnly("main", "moved")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/daemon/x.go", "internal/misc/x.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffNameOnly = %q, want %q", got, want)
	}
	s.noUnscripted(t)
}

// A coverage/ directory holding a committed report and no source file is not
// an artifact directory: HEAD tracks the report, so the polecat's edit to it
// is work, while generated output written beside it is still reset (gt-42qp3).
// The listing stubs mirror the checkpoint dog's state after git add -A, where
// the index lists the generated output too and only HEAD knows what is content.
func TestRuntimeArtifactPathspecsListing_KeepsCommittedFileWithoutSource(t *testing.T) {
	t.Parallel()
	list := func(root string) ([]string, error) {
		switch root {
		case "coverage/":
			return []string{"coverage/report.html", "coverage/coverage.out"}, nil
		case "web/.cache/":
			return []string{"web/.cache/blob.bin"}, nil
		}
		return nil, nil
	}
	head := func(root string) ([]string, error) {
		if root == "coverage/" {
			return []string{"coverage/report.html"}, nil
		}
		return nil, nil
	}

	for _, tc := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{
			name:  "the committed report is not an artifact",
			paths: []string{"coverage/report.html"},
			want:  nil,
		},
		{
			name:  "output beside the committed report is reset, narrowed away from it",
			paths: []string{"coverage/report.html", "coverage/coverage.out"},
			want:  []string{"coverage/coverage.out"},
		},
		{
			name:  "a directory of untracked generated files is still an artifact directory",
			paths: []string{"web/.cache/blob.bin"},
			want:  []string{"web/.cache/"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RuntimeArtifactPathspecsListing(tc.paths, list, head); !slices.Equal(got, tc.want) {
				t.Fatalf("RuntimeArtifactPathspecsListing(%q) = %q, want %q", tc.paths, got, tc.want)
			}
		})
	}
}

// git ls-tree HEAD cannot be read: whether the file under the root is
// committed content is unknown, so it is kept (gt-42qp3).
func TestRuntimeArtifactPathspecsListing_KeepsFilesWhenHeadListingFails(t *testing.T) {
	t.Parallel()
	empty := func(string) ([]string, error) { return nil, nil }
	failing := func(string) ([]string, error) { return nil, errors.New("listing failed") }
	got := RuntimeArtifactPathspecsListing([]string{"coverage/coverage.out"}, empty, failing)
	if got != nil {
		t.Fatalf("RuntimeArtifactPathspecsListing with a failed HEAD listing = %q, want none", got)
	}
}

// An unborn HEAD — git's own failure for a repo before its first commit —
// reaches the same keep rather than reset through the real *Git path, not only
// through the listing seam above: nothing can be told about what is committed
// under the root, so its output is kept (gt-ji90i).
func TestRuntimeArtifactPathspecs_UnbornHeadKeepsOutput(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"ls-files -z -- coverage/":                    ok("coverage/coverage.out\x00"),
		"ls-tree -r -z --name-only HEAD -- coverage/": fail(128, "fatal: Not a valid object name HEAD\n"),
	})
	if got := newTestGit(t, s).RuntimeArtifactPathspecs([]string{"coverage/coverage.out"}); got != nil {
		t.Fatalf("RuntimeArtifactPathspecs with an unborn HEAD = %q, want no pathspec", got)
	}
	s.noUnscripted(t)
}

// Each artifact root costs one index listing and one HEAD listing, however
// many files it holds: a generated directory of ten files must not spawn ten
// git calls on the checkpoint dog's and gt done's shared path (gt-ji90i).
func TestRuntimeArtifactPathspecs_ReadsEachRootOnce(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"ls-files -z -- coverage/":                      ok("coverage/a.out\x00coverage/b.out\x00coverage/c.out\x00"),
		"ls-tree -r -z --name-only HEAD -- coverage/":   ok(""),
		"ls-files -z -- web/.cache/":                    ok("web/.cache/blob.bin\x00web/.cache/other.bin\x00"),
		"ls-tree -r -z --name-only HEAD -- web/.cache/": ok(""),
	})
	g := newTestGit(t, s)
	paths := []string{
		"coverage/a.out", "coverage/b.out", "web/.cache/blob.bin",
		"coverage/c.out", "web/.cache/other.bin",
	}
	if got, want := g.RuntimeArtifactPathspecs(paths), []string{"coverage/", "web/.cache/"}; !slices.Equal(got, want) {
		t.Fatalf("RuntimeArtifactPathspecs = %q, want %q", got, want)
	}
	want := []string{
		"ls-files -z -- coverage/",
		"ls-tree -r -z --name-only HEAD -- coverage/",
		"ls-files -z -- web/.cache/",
		"ls-tree -r -z --name-only HEAD -- web/.cache/",
	}
	if got := s.sent(); !slices.Equal(got, want) {
		t.Errorf("git calls = %q, want one listing pair per root %q", got, want)
	}
	s.noUnscripted(t)
}

// DiffNumstat keeps renames off, so a moved file reads as a delete and an add,
// and a binary file keeps its "-" counts.
func TestDiffNumstatNoRenames(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --numstat --no-renames main..feature": ok("3\t1\tinternal/x.go\n-\t-\tlogo.png\n"),
	})
	got, err := newTestGit(t, s).DiffNumstat("main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if want := "3\t1\tinternal/x.go\n-\t-\tlogo.png"; got != want {
		t.Fatalf("DiffNumstat = %q, want %q", got, want)
	}
	s.noUnscripted(t)
}
