package doctor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestOrphanedRoleOverridesCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup populates a fresh town root; paths are relative to it.
		setup      func(t *testing.T, townRoot string)
		wantStatus CheckStatus
		// wantDetails is compared exactly, in order, relative to the town root.
		wantDetails []string
		// wantDetailPart matches when the details are error text rather than
		// paths; each entry must appear in exactly one detail line.
		wantDetailPart []string
		// wantMessagePart matches text the result reports in its message
		// instead of its details.
		wantMessagePart string
	}{
		{
			name:       "no role directories",
			wantStatus: StatusOK,
		},
		{
			name: "town-level override is reported",
			setup: func(t *testing.T, townRoot string) {
				writeTestFile(t, filepath.Join(townRoot, "roles", "polecat.toml"))
			},
			wantStatus:  StatusWarning,
			wantDetails: []string{"roles/polecat.toml"},
		},
		{
			name: "rig-level override is reported",
			setup: func(t *testing.T, townRoot string) {
				writeRigs(t, townRoot, "gastown")
				writeTestFile(t, filepath.Join(townRoot, "gastown", "roles", "witness.toml"))
			},
			wantStatus:  StatusWarning,
			wantDetails: []string{"gastown/roles/witness.toml"},
		},
		{
			name: "non-toml files are ignored",
			setup: func(t *testing.T, townRoot string) {
				writeRigs(t, townRoot, "gastown")
				writeTestFile(t, filepath.Join(townRoot, "roles", "notes.md"))
				writeTestFile(t, filepath.Join(townRoot, "gastown", "roles", "README"))
			},
			wantStatus: StatusOK,
		},
		{
			name: "roles in an unregistered directory are not scanned",
			setup: func(t *testing.T, townRoot string) {
				writeTestFile(t, filepath.Join(townRoot, "scratch", "roles", "polecat.toml"))
			},
			wantStatus: StatusOK,
		},
		{
			name: "details are sorted across the town and its rigs",
			setup: func(t *testing.T, townRoot string) {
				writeRigs(t, townRoot, "zeta", "alpha")
				writeTestFile(t, filepath.Join(townRoot, "roles", "zeta.toml"))
				writeTestFile(t, filepath.Join(townRoot, "roles", "alpha.toml"))
				writeTestFile(t, filepath.Join(townRoot, "zeta", "roles", "a.toml"))
				writeTestFile(t, filepath.Join(townRoot, "alpha", "roles", "b.toml"))
			},
			wantStatus: StatusWarning,
			wantDetails: []string{
				"alpha/roles/b.toml",
				"roles/alpha.toml",
				"roles/zeta.toml",
				"zeta/roles/a.toml",
			},
		},
		{
			name: "unreadable roles directory is skipped, not clean",
			setup: func(t *testing.T, townRoot string) {
				// A regular file where the directory should be makes ReadDir
				// fail with ENOTDIR whatever the test runs as, unlike a mode
				// that root ignores.
				writeTestFile(t, filepath.Join(townRoot, "roles"))
			},
			wantStatus:     StatusSkipped,
			wantDetailPart: []string{"roles: "},
		},
		{
			name: "unreadable rig registry is skipped",
			setup: func(t *testing.T, townRoot string) {
				writeTestFileContent(t, filepath.Join(townRoot, "mayor", "rigs.json"), "{not json")
			},
			wantStatus:      StatusSkipped,
			wantMessagePart: "rig registry",
		},
		{
			name: "unreadable rig roles still lists orphans from readable directories",
			setup: func(t *testing.T, townRoot string) {
				writeRigs(t, townRoot, "gastown")
				writeTestFile(t, filepath.Join(townRoot, "roles", "crew.toml"))
				writeTestFile(t, filepath.Join(townRoot, "gastown", "roles"))
			},
			wantStatus:     StatusSkipped,
			wantDetailPart: []string{"gastown/roles: ", "roles/crew.toml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, townRoot)
			}

			result := NewOrphanedRoleOverridesCheck().Run(&CheckContext{TownRoot: townRoot})

			if result.Status != tt.wantStatus {
				t.Fatalf("status = %v, want %v (message %q, details %v)", result.Status, tt.wantStatus, result.Message, result.Details)
			}
			if tt.wantMessagePart != "" {
				if !strings.Contains(result.Message, tt.wantMessagePart) {
					t.Errorf("message %q does not contain %q", result.Message, tt.wantMessagePart)
				}
				return
			}
			if tt.wantDetailPart != nil {
				for _, part := range tt.wantDetailPart {
					if !slices.ContainsFunc(result.Details, func(d string) bool { return strings.Contains(d, part) }) {
						t.Errorf("details %v contain no line with %q", result.Details, part)
					}
				}
				return
			}
			var want []string
			for _, d := range tt.wantDetails {
				want = append(want, filepath.Join(townRoot, d))
			}
			if !slices.Equal(result.Details, want) {
				t.Errorf("details = %v, want %v", result.Details, want)
			}
		})
	}
}

// TestRoleOverridesHaveNoReader pins the premise of
// OrphanedRoleOverridesCheck (gt-yip5x): no non-test Go file passes a "roles"
// segment to filepath.Join, the shape every on-disk path in this repo is built
// with, so naming <town>/roles/*.toml and <rig>/roles/*.toml as dead config is
// true of this tree. The reader that did was retired in gt-7mthw; role
// definitions load from the binary's embedded files. The check's own directory
// scan is the one allowed hit.
//
// A reader reintroduced as a hit here means the check must be narrowed or
// deleted, never that the reader belongs in the allowlist: doctor would
// otherwise tell users to delete live config.
func TestRoleOverridesHaveNoReader(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}

	// The check itself, the only place a roles directory is named on disk.
	const allowed = "internal/doctor/orphaned_role_overrides_check.go"

	var hits []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "vendor" || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Join" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "filepath" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && lit.Value == `"roles"` {
					hits = append(hits, rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning %s: %v", root, err)
	}

	// A scan that found nothing would pass on any tree, including one this
	// test never touched, so the check's own hit proves the scan works.
	if !slices.ContainsFunc(hits, func(h string) bool { return strings.HasPrefix(h, allowed+":") }) {
		t.Fatalf("scan found no filepath.Join(..., \"roles\") in %s; hits: %v", allowed, hits)
	}
	for _, hit := range hits {
		if !strings.HasPrefix(hit, allowed+":") {
			t.Errorf("%s builds a path into a roles directory on disk; OrphanedRoleOverridesCheck would then name live config as orphaned", hit)
		}
	}
}

// writeRigs registers rigs in <town>/mayor/rigs.json.
func writeRigs(t *testing.T, townRoot string, rigs ...string) {
	t.Helper()
	var entries []string
	seen := make(map[string]bool, len(rigs))
	for _, rig := range rigs {
		if seen[rig] {
			continue
		}
		seen[rig] = true
		entries = append(entries, `"`+rig+`": {"git_url": "https://example.com/`+rig+`"}`)
	}
	writeTestFileContent(t, filepath.Join(townRoot, "mayor", "rigs.json"),
		`{"version": 1, "rigs": {`+strings.Join(entries, ", ")+`}}`)
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	writeTestFileContent(t, path, "x = 1\n")
}

func writeTestFileContent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
