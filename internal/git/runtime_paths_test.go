package git

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestIsGasTownRuntimePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{".claude/", true},
		{".claude/settings.json", true},
		{".claude/commands/foo.md", true},
		{".claude", true},
		{".runtime/", true},
		{".runtime/state.json", true},
		{".runtime", true},
		{".opencode/", true},
		{".opencode/plugins/gastown.js", true},
		{".opencode/commands/handoff.md", true},
		{".beads/", true},
		{".beads/db.json", true},
		{".beads\\db.json", true},
		{".beads/.runtime/state.json", true},
		{".logs/agent.log", true},
		{"__pycache__/", true},
		{"__pycache__/foo.cpython-312.pyc", true},
		{"src/__pycache__/bar.pyc", true},
		{"services/cyrus/workflow-cyrus-edge/node_modules/pkg/index.js", true},
		{"services\\cyrus\\workflow-cyrus-edge\\node_modules\\pkg\\index.js", true},
		{"dashboard/public/meridian-dashboard/.vite/vitest/hash/results.json", true},
		{"services/workflows/collateral-internal/execution_log.db", true},
		{"api/.pytest_cache/v/cache/nodeids", true},
		{"api/.mypy_cache/3.12/module.meta.json", true},
		{"api/.ruff_cache/0.8.0/cache", true},
		{"coverage/lcov.info", true},
		{"htmlcov/index.html", true},
		{"src/module.pyc", true},
		{"frontend/.DS_Store", true},
		{"src/main.go", false},
		{"README.md", false},
		{".gitignore", false},
		{"claude-stuff/foo", false},
		{"src/coverage_report.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isGasTownRuntimePath(tt.path)
			if got != tt.want {
				t.Errorf("isGasTownRuntimePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestCleanExcludingRuntime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    UncommittedWorkStatus
		want bool
	}{
		{
			name: "only runtime artifacts",
			s: UncommittedWorkStatus{
				HasUncommittedChanges: true,
				UntrackedFiles:        []string{".claude/", ".opencode/plugins/gastown.js", ".runtime/state.json"},
			},
			want: true,
		},
		{
			name: "real code changes",
			s: UncommittedWorkStatus{
				HasUncommittedChanges: true,
				ModifiedFiles:         []string{"src/main.go"},
			},
			want: false,
		},
		{
			name: "runtime path conflict blocks",
			s: UncommittedWorkStatus{
				HasUncommittedChanges: true,
				UnmergedFiles:         []string{".opencode/plugins/gastown.js"},
			},
			want: false,
		},
		{
			name: "mix of runtime and real",
			s: UncommittedWorkStatus{
				HasUncommittedChanges: true,
				UntrackedFiles:        []string{".claude/settings.json"},
				ModifiedFiles:         []string{"src/main.go"},
			},
			want: false,
		},
		{
			name: "clean",
			s:    UncommittedWorkStatus{},
			want: true,
		},
		{
			name: "stashes ignored (survive worktree deletion)",
			s: UncommittedWorkStatus{
				StashCount: 1,
			},
			want: true,
		},
		{
			// Unpushed commits alone do not affect CleanExcludingRuntime — this
			// function only evaluates uncommitted file changes. Unpushed commits
			// are handled separately by the CommitsAhead check in gt done (gas-7vg).
			name: "unpushed commits alone do not block",
			s: UncommittedWorkStatus{
				UnpushedCommits: 2,
			},
			want: true,
		},
		{
			// The primary bug scenario (gas-7vg): polecat commits work (1 unpushed
			// commit) then calls gt done with only infrastructure files untracked.
			// CleanExcludingRuntime must return true so gt done is not blocked.
			name: "unpushed commit with only runtime artifacts",
			s: UncommittedWorkStatus{
				HasUncommittedChanges: true,
				UnpushedCommits:       1,
				UntrackedFiles:        []string{".beads/", ".claude/commands/done.md", ".runtime/state.json"},
			},
			want: true,
		},
		{
			name: "pycache untracked",
			s: UncommittedWorkStatus{
				HasUncommittedChanges: true,
				UntrackedFiles:        []string{"__pycache__/foo.pyc", ".beads/db"},
			},
			want: true,
		},
		{
			name: "nested dependency and cache artifacts",
			s: UncommittedWorkStatus{
				HasUncommittedChanges: true,
				UntrackedFiles: []string{
					"services/cyrus/workflow-cyrus-edge/node_modules/pkg/index.js",
					"dashboard/public/meridian-dashboard/.vite/vitest/hash/results.json",
					"services/workflows/collateral-internal/execution_log.db",
					"api/.pytest_cache/v/cache/nodeids",
					"src/__pycache__/module.cpython-312.pyc",
				},
			},
			want: true,
		},
		{
			// CLAUDE.local.md is a Gas Town overlay file (gt-p35) that must not
			// block gt done or be auto-committed.
			name: "CLAUDE.local.md is runtime artifact",
			s: UncommittedWorkStatus{
				HasUncommittedChanges: true,
				UntrackedFiles:        []string{"CLAUDE.local.md"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.s.CleanExcludingRuntime()
			if got != tt.want {
				t.Errorf("CleanExcludingRuntime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRuntimeArtifactPaths(t *testing.T) {
	t.Parallel()
	status := UncommittedWorkStatus{
		HasUncommittedChanges: true,
		ModifiedFiles: []string{
			"services/workflows/collateral-internal/execution_log.db",
			"src/handler.go",
		},
		UntrackedFiles: []string{
			".opencode/plugins/gastown.js",
			"services/cyrus/workflow-cyrus-edge/node_modules/pkg/index.js",
			"services/cyrus/workflow-cyrus-edge/node_modules/pkg/package.json",
			"dashboard/public/meridian-dashboard/.vite/vitest/hash/results.json",
			"api/.pytest_cache/v/cache/nodeids",
			"src/__pycache__/module.cpython-312.pyc",
			"cmd/new_feature.go",
		},
	}

	got := status.RuntimeArtifactPaths()
	want := []string{
		"services/workflows/collateral-internal/execution_log.db",
		".opencode/",
		"services/cyrus/workflow-cyrus-edge/node_modules/",
		"dashboard/public/meridian-dashboard/.vite/",
		"api/.pytest_cache/",
		"src/__pycache__/",
	}
	if len(got) != len(want) {
		t.Fatalf("RuntimeArtifactPaths() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RuntimeArtifactPaths()[%d] = %q, want %q (all: %#v)", i, got[i], want[i], got)
		}
	}
}

func TestRuntimeArtifactPathspecs(t *testing.T) {
	t.Parallel()
	got := RuntimeArtifactPathspecs([]string{
		".beads/redirect",
		"web/.beads/redirect",
		"web/.beads/db.sqlite",
		".opencode/settings.json",
		"api/.opencode/settings.json",
		"./.runtime/state.json",
		"svc/.runtime/state.json",
		"tools/.claude/settings.json",
		"src/main.go",
		"pkg/cache.pyc",
	})
	want := []string{
		".beads/",
		"web/.beads/",
		".opencode/",
		"api/.opencode/",
		".runtime/",
		"svc/.runtime/",
		"tools/.claude/",
		"pkg/cache.pyc",
	}
	if len(got) != len(want) {
		t.Fatalf("RuntimeArtifactPathspecs() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RuntimeArtifactPathspecs()[%d] = %q, want %q (all: %#v)", i, got[i], want[i], got)
		}
	}
}

func TestParsePorcelainStatusEntryPreservesRenameCopySourceAndConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		line       string
		wantCode   string
		wantSource string
		wantPath   string
		wantPaths  []string
		unmerged   bool
	}{
		{
			name:       "rename",
			line:       "R  README.md -> .opencode/plugins/gastown.js",
			wantCode:   "R ",
			wantSource: "README.md",
			wantPath:   ".opencode/plugins/gastown.js",
			wantPaths:  []string{"README.md", ".opencode/plugins/gastown.js"},
		},
		{
			name:       "copy",
			line:       "C  README.md -> .opencode/plugins/gastown.js",
			wantCode:   "C ",
			wantSource: "README.md",
			wantPath:   ".opencode/plugins/gastown.js",
			wantPaths:  []string{"README.md", ".opencode/plugins/gastown.js"},
		},
		{
			name:      "unmerged",
			line:      "UU .opencode/plugins/gastown.js",
			wantCode:  "UU",
			wantPath:  ".opencode/plugins/gastown.js",
			wantPaths: []string{".opencode/plugins/gastown.js"},
			unmerged:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePorcelainStatusEntry(tt.line)
			if !ok {
				t.Fatal("parsePorcelainStatusEntry returned ok=false")
			}
			if got.Code != tt.wantCode || got.SourcePath != tt.wantSource || got.Path != tt.wantPath || got.Unmerged != tt.unmerged {
				t.Fatalf("parsePorcelainStatusEntry(%q) = %+v", tt.line, got)
			}
			if paths := got.paths(); strings.Join(paths, "\x00") != strings.Join(tt.wantPaths, "\x00") {
				t.Fatalf("paths = %v, want %v", paths, tt.wantPaths)
			}
		})
	}
}

func TestComparisonRefCandidatesPreferRemoteTrackingRef(t *testing.T) {
	t.Parallel()
	got := comparisonRefCandidates("main", "origin")
	want := []string{"upstream/main", "origin/main", "main"}
	if len(got) != len(want) {
		t.Fatalf("comparisonRefCandidates length = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("comparisonRefCandidates()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The checkout's tracked source decides which coverage/ and .cache/ directories
// are fixtures (gt-za3u1). Beside source that is among the changed paths the
// output is reset, but a file HEAD tracks there is a fixture or an edit and
// stays, and no pathspec reaches it or the source (gt-qdvp2). A listing that
// fails counts as holding source, or as everything under it being tracked.
func TestRuntimeArtifactPathspecsListing(t *testing.T) {
	t.Parallel()
	lister := func(files map[string][]string) func(root string) ([]string, error) {
		return func(root string) ([]string, error) { return files[root], nil }
	}
	failing := func(string) ([]string, error) { return nil, errors.New("listing failed") }
	pkg := lister(map[string][]string{"pkg/coverage/": {"pkg/coverage/collector.go", "pkg/coverage/testdata/report.golden"}})
	for _, tc := range []struct {
		name       string
		paths      []string
		list, head func(root string) ([]string, error)
		want       []string
	}{
		{
			name:  "tracked source spares only its own directory",
			paths: []string{"internal/coverage/testdata/report.golden", "web/coverage/lcov.info"},
			list: lister(map[string][]string{
				"internal/coverage/": {"internal/coverage/collector.go"},
				"web/coverage/":      {"web/coverage/README.md"},
			}),
			head: lister(map[string][]string{"internal/coverage/": {"internal/coverage/collector.go"}}),
			want: []string{"web/coverage/"},
		},
		{
			name:  "a failed listing spares every directory",
			paths: []string{"internal/coverage/testdata/report.golden", "web/coverage/lcov.info"},
			list:  failing, head: failing,
			want: nil,
		},
		{
			name:  "no tracked source resets both",
			paths: []string{"internal/coverage/testdata/report.golden", "web/coverage/lcov.info"},
			list:  lister(nil), head: lister(nil),
			want: []string{"internal/coverage/", "web/coverage/"},
		},
		{
			name:  "edited source leaves the output beside it reset",
			paths: []string{"pkg/coverage/collector.go", "pkg/coverage/lcov-report/index.html", "pkg/coverage/coverage.out"},
			list:  pkg, head: pkg,
			want: []string{"pkg/coverage/lcov-report/", "pkg/coverage/coverage.out"},
		},
		{
			name:  "unchanged source keeps the output as a possible fixture",
			paths: []string{"pkg/coverage/lcov-report/index.html", "pkg/coverage/coverage.out"},
			list:  pkg, head: pkg,
			want: nil,
		},
		{
			name:  "edited source does not reset a tracked fixture beside it",
			paths: []string{"pkg/coverage/collector.go", "pkg/coverage/testdata/report.golden", "pkg/coverage/coverage.out"},
			list:  pkg, head: pkg,
			want: []string{"pkg/coverage/coverage.out"},
		},
		{
			name:  "a tracked fixture keeps new output in its directory from a whole-directory reset",
			paths: []string{"pkg/coverage/collector.go", "pkg/coverage/lcov-report/index.html", "pkg/coverage/lcov-report/keep.golden", "pkg/coverage/testdata/report.golden"},
			list:  lister(map[string][]string{"pkg/coverage/": {"pkg/coverage/collector.go", "pkg/coverage/lcov-report/keep.golden", "pkg/coverage/testdata/report.golden"}}),
			head:  lister(map[string][]string{"pkg/coverage/": {"pkg/coverage/collector.go", "pkg/coverage/lcov-report/keep.golden", "pkg/coverage/testdata/report.golden"}}),
			want:  []string{"pkg/coverage/lcov-report/index.html"},
		},
		{
			name:  "edited source spares a new fixture beside it but resets the output",
			paths: []string{"pkg/coverage/collector.go", "pkg/coverage/testdata/new.golden", "pkg/coverage/lcov-report/index.html", "pkg/coverage/coverage.out"},
			list:  pkg, head: pkg,
			want: []string{"pkg/coverage/lcov-report/", "pkg/coverage/coverage.out"},
		},
		{
			name:  "new source spares a new fixture beside it when nothing is tracked",
			paths: []string{"internal/coverage/collector.go", "internal/coverage/testdata/report.golden", "internal/coverage/coverage.out"},
			list:  lister(nil), head: lister(nil),
			want: []string{"internal/coverage/coverage.out"},
		},
		{
			name:  "a new lcov.info under testdata is a fixture, one beside it is output",
			paths: []string{"pkg/coverage/collector.go", "pkg/coverage/testdata/lcov.info", "pkg/coverage/lcov.info"},
			list:  pkg, head: pkg,
			want: []string{"pkg/coverage/lcov.info"},
		},
		{
			name:  "a .cache directory beside edited source stays output",
			paths: []string{"pkg/.cache/store.py", "pkg/.cache/blob.golden"},
			list:  lister(map[string][]string{"pkg/.cache/": {"pkg/.cache/store.py"}}), head: lister(map[string][]string{"pkg/.cache/": {"pkg/.cache/store.py"}}),
			want: []string{"pkg/.cache/blob.golden"},
		},
		{
			name:  "edited source with a failed HEAD listing spares everything",
			paths: []string{"pkg/coverage/collector.go", "pkg/coverage/coverage.out"},
			list:  pkg, head: failing,
			want: nil,
		},
		{
			name:  "source under another root does not spare this one",
			paths: []string{"pkg/coverage/lcov-report/index.html", "web/coverage/main.go"},
			list:  pkg, head: pkg,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RuntimeArtifactPathspecsListing(tc.paths, tc.list, tc.head)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("RuntimeArtifactPathspecsListing = %q, want %q", got, tc.want)
			}
			for _, spec := range got {
				for _, src := range []string{"pkg/coverage/collector.go", "pkg/coverage/testdata/report.golden"} {
					if strings.HasPrefix(src, spec) {
						t.Errorf("pathspec %q covers %s", spec, src)
					}
				}
			}
		})
	}
}

// The status takes the same side as the reset: gt done ignores the output the
// checkpoint dog resets beside edited source, and blocks on the fixture it keeps.
func TestUncommittedWorkStatusOutputBesideEditedSourceIsArtifact(t *testing.T) {
	t.Parallel()
	fixture := "pkg/coverage/testdata/report.golden"
	s := &UncommittedWorkStatus{
		HasUncommittedChanges: true,
		ModifiedFiles:         []string{"pkg/coverage/collector.go", fixture},
		UntrackedFiles:        []string{"pkg/coverage/lcov-report/index.html"},
		beside: sourceBeside{
			roots: []string{"pkg/coverage/"},
			head:  map[string][]string{"pkg/coverage/": {"pkg/coverage/collector.go", fixture}},
		},
	}
	want := []string{"pkg/coverage/collector.go", fixture}
	if got := s.NonRuntimePaths(); !slices.Equal(got, want) {
		t.Errorf("NonRuntimePaths = %q, want %q", got, want)
	}
	if got := s.RuntimeArtifactPaths(); !slices.Equal(got, []string{"pkg/coverage/lcov-report/"}) {
		t.Errorf("RuntimeArtifactPaths = %q, want the lcov-report directory", got)
	}
}

// With pkg/coverage/collector.go tracked and edited, the generated output
// staged beside it is reset and the source stays staged (gt-qdvp2).
func TestRuntimeArtifactPathspecsBesideResetsArtifactsUnderSourceRoot(t *testing.T) {
	t.Parallel()
	files := func(string) ([]string, error) { return []string{"pkg/coverage/collector.go"}, nil }
	got := RuntimeArtifactPathspecsListing(
		[]string{"pkg/coverage/collector.go", "pkg/coverage/lcov-report/index.html", "pkg/coverage/coverage.out"}, files, files)
	if want := []string{"pkg/coverage/lcov-report/", "pkg/coverage/coverage.out"}; !slices.Equal(got, want) {
		t.Fatalf("RuntimeArtifactPathspecsListing = %q, want %q", got, want)
	}
}

// A new hand-written fixture beside edited tracked source is not in HEAD, so
// HEAD cannot vouch for it, but it does not look generated either: it stays
// staged while the output beside it is reset (gt-v90u4).
func TestRuntimeArtifactPathspecsBesideKeepsNewFixtureBesideEditedSource(t *testing.T) {
	t.Parallel()
	files := func(string) ([]string, error) { return []string{"pkg/coverage/collector.go"}, nil }
	fixture := "pkg/coverage/testdata/report.golden"
	got := RuntimeArtifactPathspecsListing(
		[]string{"pkg/coverage/collector.go", fixture, "pkg/coverage/lcov-report/index.html", "pkg/coverage/coverage.out"}, files, files)
	if want := []string{"pkg/coverage/lcov-report/", "pkg/coverage/coverage.out"}; !slices.Equal(got, want) {
		t.Fatalf("RuntimeArtifactPathspecsListing = %q, want %q", got, want)
	}
	for _, spec := range got {
		if strings.HasPrefix(fixture, spec) {
			t.Errorf("pathspec %q covers the new fixture %s", spec, fixture)
		}
	}
}

// The blanket .out entry in looksGenerated resets a profile written under any
// name (unit.out) beside edited source, but never a .out file HEAD tracks, nor
// one under testdata/ (gt-ey0e3).
func TestRuntimeArtifactPathspecsBesideSparesTrackedAndFixtureOutFiles(t *testing.T) {
	t.Parallel()
	list := func(string) ([]string, error) {
		return []string{"pkg/coverage/collector.go", "pkg/coverage/expected.out"}, nil
	}
	got := RuntimeArtifactPathspecsListing([]string{
		"pkg/coverage/collector.go", "pkg/coverage/expected.out",
		"pkg/coverage/testdata/golden.out", "pkg/coverage/unit.out",
	}, list, list)
	if want := []string{"pkg/coverage/unit.out"}; !slices.Equal(got, want) {
		t.Fatalf("RuntimeArtifactPathspecsListing = %q, want %q", got, want)
	}
}

// go tool cover -html and -coverprofile write reports under whatever name the
// caller picks; the common ones are generator output beside edited source even
// though looksGenerated has no entry for the exact file (gt-dlwi0).
func TestRuntimeArtifactPathspecs_UnlistedGeneratorOutputBesideEditedSource(t *testing.T) {
	t.Parallel()
	files := func(string) ([]string, error) { return []string{"coverage/collector.go"}, nil }
	got := RuntimeArtifactPathspecsListing(
		[]string{"coverage/collector.go", "coverage/coverage.html", "coverage/c.prof"}, files, files)
	want := []string{"coverage/coverage.html", "coverage/c.prof"}
	if !slices.Equal(got, want) {
		t.Fatalf("RuntimeArtifactPathspecsListing = %q, want %q", got, want)
	}
}

// looksGenerated is the line between a new fixture and generator output under
// a coverage/ or .cache/ root.
func TestLooksGenerated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		root, path string
		want       bool
	}{
		{"pkg/coverage/", "pkg/coverage/coverage.out", true},
		{"pkg/coverage/", "pkg/coverage/unit.out", true},
		{"pkg/coverage/", "pkg/coverage/Unit.OUT", true},
		{"pkg/coverage/", "pkg/coverage/sub/unit.out", true},
		{"pkg/coverage/", "pkg/coverage/testdata/golden.out", false},
		{"pkg/coverage/", "pkg/coverage/out", false},
		{"pkg/coverage/", "pkg/coverage/unit.out.md", false},
		{"pkg/coverage/", "pkg/coverage/layout.outline", false},
		{"pkg/coverage/", "pkg/coverage/lcov.info", true},
		{"pkg/coverage/", "pkg/coverage/lcov-report/index.html", true},
		{"pkg/coverage/", "pkg/coverage/lcov-report/css/a.css", true},
		{"pkg/coverage/", "pkg/coverage/default_1.profraw", true},
		{"pkg/coverage/", "pkg/coverage/c.prof", true},
		{"pkg/coverage/", "pkg/coverage/cpu.pprof", true},
		{"pkg/coverage/", "pkg/coverage/coverage.html", true},
		{"pkg/coverage/", "pkg/coverage/coverage.lcov", false},
		{"pkg/coverage/", "pkg/coverage/report.html", false},
		{"pkg/coverage/", "pkg/coverage/template.html", false},
		{"pkg/coverage/", "pkg/coverage/testdata/coverage.html", false},
		{"pkg/coverage/", "pkg/coverage/testdata/c.prof", false},
		{"pkg/coverage/", "pkg/coverage/testdata/report.golden", false},
		{"pkg/coverage/", "pkg/coverage/testdata/lcov.info", false},
		{"pkg/coverage/", "pkg/coverage/testdata/lcov-report/index.html", false},
		{"pkg/coverage/", "pkg/coverage/helper_notes.md", false},
		{"pkg/.cache/", "pkg/.cache/blob.bin", true},
		{"pkg/.cache/", "pkg/.cache/testdata/blob.golden", true},
	} {
		if got := looksGenerated(tc.root, tc.path); got != tc.want {
			t.Errorf("looksGenerated(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}

// coverage.json, coverage.txt, cover.html, coverage.profile and .lcov are names
// a person also writes by hand as fixtures, so an untracked one beside new
// source is kept, not reset as generator output (gt-mpf4y).
func TestLooksGeneratedKeepsHandwrittenFixtureNames(t *testing.T) {
	t.Parallel()
	fixtures := []string{
		"pkg/coverage/coverage.json",
		"pkg/coverage/coverage.txt",
		"pkg/coverage/cover.html",
		"pkg/coverage/coverage.profile",
		"pkg/coverage/coverage.lcov",
	}
	for _, f := range fixtures {
		if looksGenerated("pkg/coverage/", f) {
			t.Errorf("looksGenerated(%q) = true, want false for a hand-written fixture", f)
		}
	}
	list := func(string) ([]string, error) { return []string{"pkg/coverage/collector.go"}, nil }
	got := RuntimeArtifactPathspecsListing(append([]string{"pkg/coverage/collector.go"}, fixtures...), list, list)
	if len(got) != 0 {
		t.Fatalf("RuntimeArtifactPathspecsListing = %q, want no pathspec over the fixtures", got)
	}
}
