package ring

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envRead is a call that reads the process environment. The guard refuses
// all of them: the signing key reaches this package as a parameter, and a key
// read from the environment here would let any process that can set an
// environment variable sign a Ring.
//
// pkg and fun are kept apart so this file never spells the call it forbids;
// the acceptance check greps for that spelling under internal/ring.
type envRead struct{ pkg, fun string }

var envReads = []envRead{
	{"os", "Getenv"},
	{"os", "LookupEnv"},
	{"os", "ExpandEnv"},
	{"config", "ExpandEnvRefs"},
}

// envKeyReads returns one refusal for every environment read in the Go files
// under root, naming the file and line, and how many files it read. No Go file
// at all is an error: a walk that sees nothing has gone blind, not found the
// package clean.
func envKeyReads(root string) (refusals []string, files int, err error) {
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, read := range envReads {
				if pkg.Name == read.pkg && sel.Sel.Name == read.fun {
					refusals = append(refusals, fmt.Sprintf("%s:%d calls %s.%s; the signing key is a parameter of this package",
						path, fset.Position(call.Pos()).Line, read.pkg, read.fun))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, files, err
	}
	if files == 0 {
		return nil, 0, fmt.Errorf("no Go files under %s: the guard has nothing to read", root)
	}
	return refusals, files, nil
}

// TestNoEnvironmentKey: this package reads no signing key from the
// environment.
func TestNoEnvironmentKey(t *testing.T) {
	t.Parallel()
	refusals, files, err := envKeyReads(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, refusal := range refusals {
		t.Error(refusal)
	}
	if files < 3 {
		t.Fatalf("scanned %d files; the walk has gone blind", files)
	}
}

// TestNoEnvironmentKeyDrivesItsRefusal: the guard's failing branch names the
// file that reads the environment, and a file that takes the key as a
// parameter passes. Without the failing rows the guard is a pin: it would
// stay green with its detector deleted.
func TestNoEnvironmentKeyDrivesItsRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// read is the environment read the fake file makes; the zero value
		// makes a file that takes the key as a parameter.
		read envRead
		// src overrides the generated source (an unreadable file).
		src string
		// wantErr is set for a fixture the walk itself must refuse.
		wantErr bool
	}{
		{name: "a key parameter"},
		{name: "getenv", read: envRead{"os", "Getenv"}},
		{name: "lookupenv", read: envRead{"os", "LookupEnv"}},
		{name: "expandenv", read: envRead{"os", "ExpandEnv"}},
		{name: "the config reference expander", read: envRead{"config", "ExpandEnvRefs"}},
		{name: "an unparseable file", src: "package fake\n\nfunc (", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src := tc.src
			if src == "" {
				src = envReadSource(tc.read)
			}
			if err := os.WriteFile(filepath.Join(dir, "fake.go"), []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			refusals, files, err := envKeyReads(dir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("envKeyReads = %v, nil; want a refusal", refusals)
				}
				return
			}
			if err != nil {
				t.Fatalf("envKeyReads: %v", err)
			}
			if files != 1 {
				t.Fatalf("read %d files, want 1", files)
			}
			if tc.read.fun == "" {
				if len(refusals) != 0 {
					t.Fatalf("a file that takes the key as a parameter was refused: %v", refusals)
				}
				return
			}
			if len(refusals) != 1 {
				t.Fatalf("refusals = %v, want one naming fake.go", refusals)
			}
			if !strings.Contains(refusals[0], "fake.go") || !strings.Contains(refusals[0], tc.read.fun) {
				t.Errorf("refusal %q does not name fake.go and %s", refusals[0], tc.read.fun)
			}
		})
	}
}

// TestNoEnvironmentKeyRefusesAnEmptyWalk: a walk that reads no Go file is an
// error, never a clean result.
func TestNoEnvironmentKeyRefusesAnEmptyWalk(t *testing.T) {
	t.Parallel()
	if _, _, err := envKeyReads(t.TempDir()); err == nil {
		t.Fatal("envKeyReads of a directory with no Go file = nil error, want a refusal")
	}
}

// envReadSource is a fake Go file making read, or taking the key as a
// parameter when read is zero.
func envReadSource(read envRead) string {
	if read.fun == "" {
		return "package fake\n\nfunc Sign(key []byte) []byte { return key }\n"
	}
	return fmt.Sprintf("package fake\n\nfunc Key() string { return %s.%s(\"GT_RING_KEY\") }\n", read.pkg, read.fun)
}
