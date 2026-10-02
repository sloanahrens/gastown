package config

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// retiredFileNames are the files the two-file layout retires (layout.go)
// and the helpers that build their paths.
var (
	retiredFileNames = map[string]bool{"rigs.json": true, "daemon.json": true, "escalation.json": true, "overseer.json": true}
	retiredPathFuncs = map[string]bool{
		"MayorRigsPath": true, "DaemonPatrolConfigPath": true, "PatrolConfigFile": true,
		"EscalationConfigPath": true, "OverseerConfigPath": true,
	}
	// rawFileCalls touch a file by path without the config loaders, which
	// are what follow a retired file into its host section.
	rawFileCalls = map[string]bool{
		"os.ReadFile": true, "os.Stat": true, "os.Lstat": true, "os.Open": true, "os.OpenFile": true,
		"os.WriteFile": true, "os.Remove": true, "atomicfile.WriteFile": true, "ioutil.ReadFile": true,
	}
)

// retiredFileRawAccess lists the functions allowed to touch a retired file
// by path, with the reason. Everything else reads and writes them through
// LoadRigsConfig, LoadDaemonPatrolConfig, UpdateConfigJSON and friends.
// The scan follows a path through local assignments, not through range
// loops or struct fields set in another function; it catches the common
// filepath.Join-then-os.ReadFile shape, not every one.
var retiredFileRawAccess = map[string]string{}

// TestRetiredConfigFilesAreReadThroughTheLoaders fails when production code
// opens or stats mayor/rigs.json, mayor/daemon.json, mayor/overseer.json or
// settings/escalation.json by path (gt-y3pgh.7). On the two-file layout
// those files do not exist; only the config loaders follow them into
// mayor/town.json and settings/config.json, so a raw read sees an empty
// town.
func TestRetiredConfigFilesAreReadThroughTheLoaders(t *testing.T) {
	t.Parallel()
	mentions, raw, err := scanRetiredFileAccess(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// The 2026-10-01 count of functions naming a retired file is ~90: a
	// scanner gone blind passes everything.
	if mentions < 40 {
		t.Fatalf("scanRetiredFileAccess saw %d functions naming a retired file (floor 40); the scanner has stopped seeing them", mentions)
	}
	for _, site := range raw {
		if _, ok := retiredFileRawAccess[site]; ok {
			continue
		}
		t.Errorf("%s touches a retired config file by path; use config.LoadRigsConfig / LoadDaemonPatrolConfig / UpdateConfigJSON, which follow it into its two-file section (gt-y3pgh.7)", site)
	}
	seen := map[string]bool{}
	for _, site := range raw {
		seen[site] = true
	}
	for site := range retiredFileRawAccess {
		if !seen[site] {
			t.Errorf("retiredFileRawAccess lists %s, which no longer touches a retired file by path; delete the entry", site)
		}
	}
}

func TestRetiredFileScanFindsRawAccess(t *testing.T) {
	t.Parallel()
	src := `package p

import (
	"os"
	"path/filepath"
)

func bad(root string) {
	p := filepath.Join(root, "mayor", "rigs.json")
	_, _ = os.ReadFile(p)
}

func good(root string) {
	_, _ = LoadRigsConfig(filepath.Join(root, "mayor", "rigs.json"))
}

func alsoBad(root string) {
	_, _ = os.Stat(DaemonPatrolConfigPath(root))
}
`
	mentions, raw, err := retiredFileAccessInFile("x.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if mentions != 3 {
		t.Errorf("mentions = %d, want 3", mentions)
	}
	if want := []string{"x.go:alsoBad", "x.go:bad"}; strings.Join(raw, ",") != strings.Join(want, ",") {
		t.Errorf("raw = %v, want %v", raw, want)
	}
}

func scanRetiredFileAccess(root string) (mentions int, raw []string, err error) {
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				// internal/config owns the files; internal/testutil builds
				// test towns.
				if d.Name() == "testdata" || rel == "internal/testutil" || rel == "internal/config" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path) //nolint:gosec // G304: repo source under the test's root
			if err != nil {
				return err
			}
			if !bytes.Contains(src, []byte(".json")) && !bytes.Contains(src, []byte("Path(")) && !bytes.Contains(src, []byte("ConfigFile(")) {
				return nil
			}
			m, r, err := retiredFileAccessInFile(rel, src)
			if err != nil {
				return err
			}
			mentions += m
			raw = append(raw, r...)
			return nil
		})
		if err != nil {
			return 0, nil, err
		}
	}
	sort.Strings(raw)
	return mentions, raw, nil
}

// retiredFileAccessInFile counts the functions in src that name a retired
// file, and returns "<rel>:<func>" for each that passes one to a raw file
// call: as a literal or path helper in the argument, or through a variable
// (or field) assigned from one earlier in the function.
func retiredFileAccessInFile(rel string, src []byte) (mentions int, raw []string, err error) {
	af, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.SkipObjectResolution)
	if err != nil {
		return 0, nil, err
	}
	for _, decl := range af.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		tainted := map[string]bool{}
		names := func(e ast.Node) bool {
			found := false
			ast.Inspect(e, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BasicLit:
					if n.Kind == token.STRING {
						if s, err := strconv.Unquote(n.Value); err == nil && retiredFileNames[filepath.Base(s)] {
							found = true
						}
					}
				case *ast.CallExpr:
					switch f := n.Fun.(type) {
					case *ast.Ident:
						found = found || retiredPathFuncs[f.Name]
					case *ast.SelectorExpr:
						found = found || retiredPathFuncs[f.Sel.Name]
					}
				case *ast.Ident, *ast.SelectorExpr:
					found = found || tainted[types.ExprString(n.(ast.Expr))]
				}
				return !found
			})
			return found
		}
		if !names(fn.Body) {
			continue
		}
		mentions++
		touches := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if i < len(n.Rhs) && names(n.Rhs[i]) {
						tainted[types.ExprString(lhs)] = true
					}
				}
			case *ast.ValueSpec:
				for i, id := range n.Names {
					if i < len(n.Values) && names(n.Values[i]) {
						tainted[id.Name] = true
					}
				}
			case *ast.CallExpr:
				f, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if x, ok := f.X.(*ast.Ident); ok && rawFileCalls[x.Name+"."+f.Sel.Name] && len(n.Args) > 0 && names(n.Args[0]) {
					touches = true
				}
			}
			return true
		})
		if touches {
			raw = append(raw, rel+":"+fn.Name.Name)
		}
	}
	sort.Strings(raw)
	return mentions, raw, nil
}
