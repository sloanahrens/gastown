package docslint

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the package to the unit tier's run-time rules: no goroutine a
// test started outlives it, and no test starts an external tool.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// memFS is the checkout a test describes: each file path mapped to its
// contents. WalkFiles iterates a map, so its order is the unspecified one the
// real walker also gives.
type memFS map[string]string

func (m memFS) Exists(p string) bool { _, ok := m[p]; return ok }

func (m memFS) ReadFile(p string) ([]byte, error) {
	contents, ok := m[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(contents), nil
}

func (m memFS) WalkFiles(dir string) []string {
	var out []string
	for p := range m {
		if strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	return out
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
			if got := AgentFacing(tc.files); !reflect.DeepEqual(got, tc.want) {
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
	files := memFS{}
	got, err := Reference(files)
	if err != nil {
		t.Fatalf("Reference() on an empty root: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Reference() on an empty root = %q, want empty", got)
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

func (failingReadFS) WalkFiles(dir string) []string {
	if dir == "docs" || dir == "docs/design" {
		return []string{"docs/design/x.md"}
	}
	return nil
}

func (failingReadFS) ReadFile(p string) ([]byte, error) {
	return nil, fs.ErrPermission
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
	if got := Go(files); !reflect.DeepEqual(got, want) {
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
	agentFacing := AgentFacing(files)
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
	if got := CeilingTier(files); !reflect.DeepEqual(got, want) {
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

func TestOSFilesReadsACheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, contents string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", rel, err)
		}
	}
	write("AGENTS.md", "# agents\n")
	write("docs/design/retired.md", "> Status: historical\n")
	write("docs/design/live.md", "# live\n")
	write("plugins/p/plugin.md", "# plugin\n")
	write("internal/pkg/a.go", "package pkg\n")
	write("internal/pkg/a_test.go", "package pkg\n")

	fsys := OSFiles{Root: root}

	if !fsys.Exists("AGENTS.md") {
		t.Error("Exists(AGENTS.md) = false, want true")
	}
	if fsys.Exists("docs/plans") || fsys.Exists("missing.md") {
		t.Error("Exists reported a path that is not a file")
	}
	if data, err := fsys.ReadFile("docs/design/live.md"); err != nil || string(data) != "# live\n" {
		t.Errorf("ReadFile(docs/design/live.md) = %q, %v", data, err)
	}

	wantHistorical := []string{"docs/design/retired.md"}
	got, err := Historical(fsys)
	if err != nil {
		t.Fatalf("Historical(): %v", err)
	}
	if !reflect.DeepEqual(got, wantHistorical) {
		t.Errorf("Historical() = %q, want %q", got, wantHistorical)
	}

	wantGo := []string{"internal/pkg/a.go"}
	if got := Go(fsys); !reflect.DeepEqual(got, wantGo) {
		t.Errorf("Go() = %q, want %q", got, wantGo)
	}

	wantAgentFacing := []string{"AGENTS.md", "plugins/p/plugin.md"}
	if got := AgentFacing(fsys); !reflect.DeepEqual(got, wantAgentFacing) {
		t.Errorf("AgentFacing() = %q, want %q", got, wantAgentFacing)
	}

	// The walker reports repo-relative slash paths, never the root prefix.
	for _, dir := range []string{"docs", "plugins", "internal", "cmd"} {
		for _, p := range fsys.WalkFiles(dir) {
			if strings.HasPrefix(p, root) || strings.HasPrefix(p, "./") || strings.Contains(p, "\\") {
				t.Errorf("WalkFiles(%q) returned %q, want a clean repo-relative slash path", dir, p)
			}
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
