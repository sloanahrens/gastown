package beads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestNoBeadsLiteralsOutsideConstructor is the gt-9i6z structural regression
// test. gt-3vh2 fixed pinnedToBeadsDir's workDir:filepath.Dir(beadsDir)
// composite literal to stat the derived workDir and fall back to townRoot
// when it doesn't exist (an aliased rig never checked out locally).
// forIssueID hand-rolled an identical &Beads{workDir: filepath.Dir(resolved),
// ...} literal with no existence check, so the same opaque "fork/exec: no
// such file or directory" failure gt-3vh2 fixed in one place stayed
// reachable through the other, hotter path (per-ID Show/Update).
//
// Patching forIssueID alone would leave the class alive for a copy #3 (this
// town has paid for that already: WorkstateInput's fail-open bug survived
// three separate patches before a structural fix — see
// TestNoWorkstateInputLiteralsOutsideConstructor in internal/polecat). This
// test enforces the structural fix instead: every *Beads must be built
// through newBeads (beads.go), never through a &Beads{} composite literal of
// its own — the same guard pattern applied here.
func TestNoBeadsLiteralsOutsideConstructor(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		violations = append(violations, beadsLiteralsOutsideConstructor(t, path)...)
	}

	if len(violations) > 0 {
		t.Fatalf("do not build &Beads{} directly outside newBeads (beads.go); gather fields into a beadsFields and call newBeads(fields) instead, so the workDir-existence fallback lives in exactly one place:\n%s", strings.Join(violations, "\n"))
	}
}

// beadsLiteralsOutsideConstructor returns "file:line" entries for every
// &Beads{} composite literal in path, excluding the one inside newBeads
// itself (beads.go's designated construction point).
func beadsLiteralsOutsideConstructor(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	base := filepath.Base(path)
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok {
			if base == "beads.go" && fn.Name != nil && fn.Name.Name == "newBeads" {
				return false // this is the constructor itself; skip its body
			}
		}
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if isBeadsType(lit.Type) {
			pos := fset.Position(lit.Pos())
			out = append(out, "  "+base+":"+strconv.Itoa(pos.Line))
		}
		return true
	})
	return out
}

func isBeadsType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "Beads"
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "Beads"
	default:
		return false
	}
}

// TestSafeWorkDirForBeadsDir covers the fallback logic extracted from
// pinnedToBeadsDir and now shared with forIssueID (gt-9i6z). Before this
// unification, pinnedToBeadsDir had zero test coverage of its own — this
// asserts both halves the bead called out: the fallback (missing workDir ->
// townRoot) and the non-fallback (existing workDir -> unchanged), so the
// non-fallback half proves the change is a no-op for rigs that are actually
// checked out.
func TestSafeWorkDirForBeadsDir(t *testing.T) {
	townRoot, rigDir := newTestTown(t)
	b := New(rigDir)

	t.Run("existing beadsDir parent is unchanged", func(t *testing.T) {
		existingRig := filepath.Join(townRoot, "other-rig")
		if err := os.MkdirAll(existingRig, 0o755); err != nil {
			t.Fatal(err)
		}
		beadsDir := filepath.Join(existingRig, ".beads")

		got := b.safeWorkDirForBeadsDir(beadsDir)
		if got != existingRig {
			t.Errorf("safeWorkDirForBeadsDir(%q) = %q, want %q unchanged", beadsDir, got, existingRig)
		}
	})

	t.Run("missing beadsDir parent falls back to townRoot", func(t *testing.T) {
		aliasedRig := filepath.Join(townRoot, "aliased-rig-never-checked-out")
		beadsDir := filepath.Join(aliasedRig, ".beads")

		got := b.safeWorkDirForBeadsDir(beadsDir)
		if got != townRoot {
			t.Errorf("safeWorkDirForBeadsDir(%q) = %q, want townRoot %q as fallback", beadsDir, got, townRoot)
		}
	})
}

// TestPinnedToBeadsDir closes the "pinnedToBeadsDir has ZERO test coverage"
// gap noted on gt-9i6z, exercising the method directly rather than only its
// extracted helper.
func TestPinnedToBeadsDir(t *testing.T) {
	townRoot, rigDir := newTestTown(t)

	t.Run("existing beadsDir parent is used as-is", func(t *testing.T) {
		existingRig := filepath.Join(townRoot, "other-rig")
		if err := os.MkdirAll(existingRig, 0o755); err != nil {
			t.Fatal(err)
		}
		beadsDir := filepath.Join(existingRig, ".beads")

		b := NewIsolated(rigDir)
		pinned := b.pinnedToBeadsDir(beadsDir)

		if pinned.workDir != existingRig {
			t.Errorf("workDir = %q, want %q", pinned.workDir, existingRig)
		}
		if pinned.beadsDir != beadsDir {
			t.Errorf("beadsDir = %q, want %q", pinned.beadsDir, beadsDir)
		}
		if !pinned.noRoute {
			t.Error("noRoute = false, want true")
		}
		if !pinned.isolated {
			t.Error("isolated not preserved from parent wrapper")
		}
	})

	t.Run("missing beadsDir parent falls back to townRoot", func(t *testing.T) {
		aliasedRig := filepath.Join(townRoot, "aliased-rig-never-checked-out")
		beadsDir := filepath.Join(aliasedRig, ".beads")

		b := NewIsolated(rigDir)
		pinned := b.pinnedToBeadsDir(beadsDir)

		if pinned.workDir != townRoot {
			t.Errorf("workDir = %q, want townRoot %q", pinned.workDir, townRoot)
		}
	})
}

// TestForIssueIDFallsBackToTownRoot is the direct regression test for the
// gt-9i6z bug: forIssueID hand-rolled pinnedToBeadsDir's
// workDir:filepath.Dir(resolved) pattern without pinnedToBeadsDir's
// existence check, so an issue ID that routes to a rig directory that
// exists in routes.jsonl but was never checked out locally (an aliased rig)
// produced a workDir that chdir would fail on with an opaque "fork/exec: no
// such file or directory" — the exact failure gt-3vh2 fixed for
// pinnedToBeadsDir but left reachable here.
func TestForIssueIDFallsBackToTownRoot(t *testing.T) {
	townRoot, rigDir := newTestTown(t)
	routesDir := filepath.Join(townRoot, ".beads")

	t.Run("routed dir exists: workDir is that dir", func(t *testing.T) {
		existingRig := filepath.Join(townRoot, "checked-out-rig")
		if err := os.MkdirAll(existingRig, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := WriteRoutes(routesDir, []Route{{Prefix: "yy-", Path: "checked-out-rig"}}); err != nil {
			t.Fatal(err)
		}

		b := NewIsolated(rigDir)
		routed := b.forIssueID("yy-abc123")

		wantWorkDir := existingRig
		if routed.workDir != wantWorkDir {
			t.Errorf("workDir = %q, want %q", routed.workDir, wantWorkDir)
		}
		if !routed.noRoute {
			t.Error("noRoute = false, want true")
		}
	})

	t.Run("routed dir never checked out locally: workDir falls back to townRoot", func(t *testing.T) {
		if err := WriteRoutes(routesDir, []Route{{Prefix: "zz-", Path: "aliased-rig-never-checked-out"}}); err != nil {
			t.Fatal(err)
		}

		b := NewIsolated(rigDir)
		routed := b.forIssueID("zz-abc123")

		if routed.workDir != townRoot {
			t.Errorf("workDir = %q, want townRoot %q (aliased-rig-never-checked-out doesn't exist)", routed.workDir, townRoot)
		}
		if !routed.noRoute {
			t.Error("noRoute = false, want true")
		}
	})
}

// newTestTown creates a minimal Gas Town root (mayor/town.json) with a real
// rig directory under it, and returns both paths. Routes are written to
// townRoot/.beads by callers that need them; rigDir itself has no
// .beads/routes.jsonl, so ResolveBeadsDirForID falls through to the town's.
func newTestTown(t *testing.T) (townRoot, rigDir string) {
	t.Helper()
	tmp := t.TempDir()

	townRoot = filepath.Join(tmp, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir = filepath.Join(townRoot, "gastown", "polecats", "marble")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	return townRoot, rigDir
}
