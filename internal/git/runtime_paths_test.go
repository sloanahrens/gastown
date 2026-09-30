package git

import (
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
