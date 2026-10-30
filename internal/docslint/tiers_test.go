package docslint

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the package to the unit tier's run-time rules: no goroutine a
// test started outlives it, and no test starts an external tool.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// memFS is the checkout a test describes: each file path mapped to its
// contents. It holds files and no directories, which is what Exists reports on
// the real filesystem too. WalkFiles iterates a map, so its order is the
// unspecified one the real walker also gives.
type memFS map[string]string

func (m memFS) Exists(p string) bool { _, ok := m[p]; return ok }

func (m memFS) ReadFile(p string) ([]byte, error) {
	contents, ok := m[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(contents), nil
}

func (m memFS) WalkFiles(dir string) ([]string, error) {
	var out []string
	for p := range m {
		if strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	return out, nil
}

// walkErrorFS is a tree with one subtree the walk cannot enter, the way a
// directory the process may not read fails on disk.
type walkErrorFS struct {
	memFS
	failDir string
}

func (w walkErrorFS) WalkFiles(dir string) ([]string, error) {
	if dir == w.failDir {
		return nil, fs.ErrPermission
	}
	return w.memFS.WalkFiles(dir)
}

func TestAgentFacingTier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files memFS
		want  []string
	}{
		{
			name: "every fixed path and both globs",
			files: memFS{
				"AGENTS.md":                                    "# agents\n",
				"internal/templates/polecat-CLAUDE.md":         "# polecat\n",
				"docs/HOOKS.md":                                "# hooks\n",
				"plugins/alpha/plugin.md":                      "# alpha\n",
				"plugins/beta/plugin.md":                       "# beta\n",
				"plugins/beta/notes.md":                        "# not a plugin.md\n",
				"plugins/nested/deeper/plugin.md":              "# too deep for the glob\n",
				"internal/formula/formulas/mol-a.formula.toml": "description = \"a\"\n",
				"internal/formula/formulas/mol-b.formula.toml": "description = \"b\"\n",
				"internal/formula/formulas/README.md":          "# not a formula\n",
			},
			want: []string{
				"AGENTS.md",
				"docs/HOOKS.md",
				"internal/formula/formulas/mol-a.formula.toml",
				"internal/formula/formulas/mol-b.formula.toml",
				"internal/templates/polecat-CLAUDE.md",
				"plugins/alpha/plugin.md",
				"plugins/beta/plugin.md",
			},
		},
		{
			name: "a fixed path that is absent is left out",
			files: memFS{
				"AGENTS.md":               "# agents\n",
				"plugins/alpha/plugin.md": "# alpha\n",
			},
			want: []string{"AGENTS.md", "plugins/alpha/plugin.md"},
		},
		{
			name: "a glob that matches nothing adds nothing and is not an error",
			files: memFS{
				"AGENTS.md":                           "# agents\n",
				"docs/HOOKS.md":                       "# hooks\n",
				"internal/formula/formulas/README.md": "# no formula here\n",
			},
			want: []string{"AGENTS.md", "docs/HOOKS.md"},
		},
		{
			name:  "a root holding none of them is an empty tier",
			files: memFS{"docs/guides/guide.md": "# guide\n"},
			want:  []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AgentFacing(tc.files)
			if err != nil {
				t.Fatalf("AgentFacing(): %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("AgentFacing() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHistoricalAndReferenceTiers(t *testing.T) {
	t.Parallel()
	files := memFS{
		"README.md":               "# readme\n",
		"docs/HOOKS.md":           "# hooks\n",
		"docs/guides/guide.md":    "# guide\n",
		"docs/plans/old-plan.md":  "> Status: abandoned\n\n# old plan\n",
		"docs/research/survey.md": "# survey\n",
		"docs/design/live.md":     "# live design\n\n> Status: historical\n",
		"docs/design/retired.md":  "\n \n> Status: historical (2026-01). Abandoned.\n\n# retired\n",
		"docs/design/indented.md": "  > Status: historical (2026-02). Abandoned.\n",
		"docs/design/notes.txt":   "not markdown\n",
		"docs/plans/data.json":    "{}\n",
	}

	t.Run("is historical, by path", func(t *testing.T) {
		tests := []struct {
			path string
			want bool
		}{
			{"docs/plans/old-plan.md", true},
			{"docs/research/survey.md", true},
			{"docs/research/deep/inner.md", true},
			{"docs/design/retired.md", true},
			{"docs/design/indented.md", true},
			{"docs/design/live.md", false},
			{"docs/guides/guide.md", false},
			{"docs/HOOKS.md", false},
			{"README.md", false},
		}
		for _, tc := range tests {
			got, err := IsHistorical(files, tc.path)
			if err != nil {
				t.Fatalf("IsHistorical(%q): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("IsHistorical(%q) = %v, want %v", tc.path, got, tc.want)
			}
		}
	})

	t.Run("historical tier", func(t *testing.T) {
		want := []string{
			"docs/design/indented.md",
			"docs/design/retired.md",
			"docs/plans/old-plan.md",
			"docs/research/survey.md",
		}
		got, err := Historical(files)
		if err != nil {
			t.Fatalf("Historical(): %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Historical() = %q, want %q", got, want)
		}
	})

	t.Run("reference tier", func(t *testing.T) {
		want := []string{
			"README.md",
			"docs/design/live.md",
			"docs/guides/guide.md",
		}
		got, err := Reference(files)
		if err != nil {
			t.Fatalf("Reference(): %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Reference() = %q, want %q", got, want)
		}
	})

	t.Run("docs/HOOKS.md belongs to neither", func(t *testing.T) {
		historical, err := Historical(files)
		if err != nil {
			t.Fatalf("Historical(): %v", err)
		}
		reference, err := Reference(files)
		if err != nil {
			t.Fatalf("Reference(): %v", err)
		}
		for _, list := range [][]string{historical, reference} {
			for _, p := range list {
				if p == "docs/HOOKS.md" {
					t.Fatalf("docs/HOOKS.md is in %q; it is agent-facing only", list)
				}
			}
		}
	})
}

func TestHistoricalUnreadableDesignDocIsAnError(t *testing.T) {
	t.Parallel()
	// A docs/design file that cannot be read is not a live one and not a
	// historical one: the tier functions report it rather than guess.
	if got, err := Reference(memFS{}); err != nil || len(got) != 0 {
		t.Fatalf("Reference() on an empty root = %q, %v; want empty, nil", got, err)
	}

	unreadable := failingReadFS{}
	if _, err := Historical(unreadable); err == nil {
		t.Error("Historical() with an unreadable design doc returned no error")
	}
	if _, err := Reference(unreadable); err == nil {
		t.Error("Reference() with an unreadable design doc returned no error")
	}
}

// failingReadFS is a tree with one design doc that every read fails on.
type failingReadFS struct{}

func (failingReadFS) Exists(p string) bool { return p == "docs/design/x.md" }

func (failingReadFS) WalkFiles(dir string) ([]string, error) {
	if dir == "docs" || dir == "docs/design" {
		return []string{"docs/design/x.md"}, nil
	}
	return nil, nil
}

func (failingReadFS) ReadFile(p string) ([]byte, error) {
	return nil, fs.ErrPermission
}

func TestWalkErrorsAreReturned(t *testing.T) {
	t.Parallel()
	// A tier that could not read part of the tree must fail, not list what it
	// managed to see: the checks would report a clean slice of a tree they
	// never finished reading.
	files := memFS{
		"AGENTS.md":           "# agents\n",
		"plugins/p/plugin.md": "# plugin\n",
		"internal/formula/formulas/f.formula.toml": "description = \"f\"\n",
		"docs/design/retired.md":                   "> Status: historical\n",
		"docs/guides/guide.md":                     "# guide\n",
		"internal/pkg/a.go":                        "package pkg\n",
	}
	tests := []struct {
		name    string
		failDir string
		tier    func(FS) ([]string, error)
	}{
		{"agent-facing plugin glob", "plugins", AgentFacing},
		{"agent-facing formula glob", "internal/formula/formulas", AgentFacing},
		{"ceiling tier", "plugins", CeilingTier},
		{"historical", "docs/design", Historical},
		{"reference", "docs", Reference},
		{"go", "internal", Go},
		{"list reference", "docs", func(fsys FS) ([]string, error) { return List(fsys, TierReference) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.tier(walkErrorFS{memFS: files, failDir: tc.failDir})
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("error = %v, want the walker's %v", err, fs.ErrPermission)
			}
			if got != nil {
				t.Errorf("tier = %q, want nil alongside the error", got)
			}
		})
	}
}

// TestOSFilesWalkFailsClosed drives the branches a real tree reaches only
// through a directory the process may not read, which the unit tier cannot
// build; the integration tier takes the same branches through the real walker.
func TestOSFilesWalkFailsClosed(t *testing.T) {
	t.Parallel()
	const root = "/repo"
	tests := []struct {
		name    string
		walk    func(string, fs.WalkDirFunc) error
		want    []string
		wantErr error
	}{
		{
			name:    "a walk error is returned",
			walk:    func(string, fs.WalkDirFunc) error { return fs.ErrPermission },
			wantErr: fs.ErrPermission,
		},
		{
			name: "an unreadable subtree stops the walk",
			walk: func(root string, fn fs.WalkDirFunc) error {
				return fn(filepath.Join(root, "locked"), nil, fs.ErrPermission)
			},
			wantErr: fs.ErrPermission,
		},
		{
			name: "a missing root is an empty tier",
			walk: func(root string, fn fs.WalkDirFunc) error {
				return fn(root, nil, fs.ErrNotExist)
			},
			want: nil,
		},
		{
			name: "the walk root is itself a file",
			walk: func(root string, fn fs.WalkDirFunc) error {
				return fn(root, fs.FileInfoToDirEntry(notADirectory{}), nil)
			},
			want: []string{"docs"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := OSFiles{Root: root, walk: tc.walk}
			got, err := fsys.WalkFiles("docs")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("WalkFiles() error = %v, want %v", err, tc.wantErr)
			}
			if err != nil && got != nil {
				t.Errorf("WalkFiles() = %q, want nil alongside the error", got)
			}
			if err == nil && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("WalkFiles() = %q, want %q", got, tc.want)
			}
		})
	}
}

// notADirectory is the walk root's entry when the tier names a file.
type notADirectory struct{}

func (notADirectory) Name() string       { return "docs" }
func (notADirectory) Size() int64        { return 0 }
func (notADirectory) Mode() fs.FileMode  { return 0o644 }
func (notADirectory) ModTime() time.Time { return time.Time{} }
func (notADirectory) IsDir() bool        { return false }
func (notADirectory) Sys() any           { return nil }

func TestExistsReportsOnlyFiles(t *testing.T) {
	t.Parallel()
	// The fake holds files and no directories, so a directory path is absent,
	// which is what OSFiles.Exists reports for one too.
	files := memFS{
		"AGENTS.md":        "# agents\n",
		"docs/design/x.md": "# x\n",
	}
	tests := []struct {
		path string
		want bool
	}{
		{"AGENTS.md", true},
		{"docs/design/x.md", true},
		{"docs/design", false},
		{"docs", false},
		{"missing.md", false},
	}
	for _, tc := range tests {
		if got := files.Exists(tc.path); got != tc.want {
			t.Errorf("Exists(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestGoTierExcludesTests(t *testing.T) {
	t.Parallel()
	files := memFS{
		"cmd/gt/main.go":         "package main\n",
		"cmd/gt/main_test.go":    "package main\n",
		"internal/pkg/a.go":      "package pkg\n",
		"internal/pkg/a_test.go": "package pkg\n",
		"internal/pkg/data.json": "{}\n",
		"docs/guides/guide.md":   "# guide\n",
	}
	want := []string{"cmd/gt/main.go", "internal/pkg/a.go"}
	got, err := Go(files)
	if err != nil {
		t.Fatalf("Go(): %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Go() = %q, want %q", got, want)
	}
}

func TestMissingDirectoriesAreEmpty(t *testing.T) {
	t.Parallel()
	// A docs-only root: no cmd/ and no internal/, so the go tier is empty and
	// every tier still lists.
	files := memFS{
		"AGENTS.md":              "# agents\n",
		"docs/HOOKS.md":          "# hooks\n",
		"docs/plans/old-plan.md": "> Status: abandoned\n",
	}
	tiers := map[string][]string{
		TierAgentFacing: {"AGENTS.md", "docs/HOOKS.md"},
		TierReference:   {},
		TierHistorical:  {"docs/plans/old-plan.md"},
		TierGo:          {},
	}
	for tier, want := range tiers {
		got, err := List(files, tier)
		if err != nil {
			t.Fatalf("List(%q): %v", tier, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("List(%q) = %q, want %q", tier, got, want)
		}
	}
}

func TestCeilingTierExemptions(t *testing.T) {
	t.Parallel()
	files := memFS{
		"internal/formula/formulas/mol-polecat-work.formula.toml":          "description = \"work\"\n",
		"internal/formula/formulas/mol-polecat-work-monorepo.formula.toml": "description = \"monorepo\"\n",
		"internal/formula/formulas/mol-big.formula.toml":                   "description = \"big\"\n",
		"AGENTS.md": "# agents\n",
	}
	agentFacing, err := AgentFacing(files)
	if err != nil {
		t.Fatalf("AgentFacing(): %v", err)
	}
	for _, p := range []string{
		"internal/formula/formulas/mol-polecat-work.formula.toml",
		"internal/formula/formulas/mol-polecat-work-monorepo.formula.toml",
	} {
		if !contains(agentFacing, p) {
			t.Errorf("AgentFacing() = %q, want it to hold exempt formula %q", agentFacing, p)
		}
	}

	want := []string{
		"AGENTS.md",
		"internal/formula/formulas/mol-big.formula.toml",
	}
	got, err := CeilingTier(files)
	if err != nil {
		t.Fatalf("CeilingTier(): %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CeilingTier() = %q, want %q", got, want)
	}
}

func TestListTable(t *testing.T) {
	t.Parallel()
	files := memFS{
		"README.md":              "# readme\n",
		"AGENTS.md":              "# agents\n",
		"docs/HOOKS.md":          "# hooks\n",
		"docs/Zed.md":            "# zed\n",
		"docs/apple.md":          "# apple\n",
		"docs/plans/old-plan.md": "> Status: abandoned\n",
		"plugins/p/plugin.md":    "# plugin\n",
		"internal/formula/formulas/f.formula.toml": "description = \"f\"\n",
		"internal/pkg/a.go":                        "package pkg\n",
		"internal/pkg/a_test.go":                   "package pkg\n",
	}
	tests := []struct {
		tier string
		want []string
	}{
		{
			tier: TierAgentFacing,
			want: []string{
				"AGENTS.md",
				"docs/HOOKS.md",
				"internal/formula/formulas/f.formula.toml",
				"plugins/p/plugin.md",
			},
		},
		{
			tier: TierReference,
			// Bytewise, so the uppercase Z sorts before the lowercase a.
			want: []string{"README.md", "docs/Zed.md", "docs/apple.md"},
		},
		{
			tier: TierHistorical,
			want: []string{"docs/plans/old-plan.md"},
		},
		{
			tier: TierGo,
			want: []string{"internal/pkg/a.go"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.tier, func(t *testing.T) {
			got, err := List(files, tc.tier)
			if err != nil {
				t.Fatalf("List(%q): %v", tc.tier, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("List(%q) = %q, want %q", tc.tier, got, tc.want)
			}
		})
	}

	for _, tier := range []string{"nope", "", "Agent-Facing"} {
		got, err := List(files, tier)
		wantErr := "docs-lint: unknown tier '" + tier + "'"
		if err == nil || err.Error() != wantErr {
			t.Errorf("List(%q) error = %v, want %q", tier, err, wantErr)
		}
		if got != nil {
			t.Errorf("List(%q) = %q, want nil alongside the error", tier, got)
		}
	}
}

func contains(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}
