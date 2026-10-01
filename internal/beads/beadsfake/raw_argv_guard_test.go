package beadsfake

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// rawBdConstructors are the internal/beads functions that hand a caller bd
// argv instead of a typed method.
var rawBdConstructors = map[string]bool{
	"Command":                true,
	"CommandContext":         true,
	"CommandContextBounded":  true,
	"CommandContextWithBin":  true,
	"CommandWithEnv":         true,
	"CommandContextWithEnv":  true,
	"CommandWithPath":        true,
	"CommandContextWithPath": true,
	"ConfigureCommand":       true,
	"NewBdCmd":               true,
}

// rawBdKey is one file's count of one kind of raw bd site.
type rawBdKey struct {
	File string
	Kind string // "argv", "BdCmd" or "BDRunner"
}

// scanRawBd counts, in one Go file, the ways code outside internal/beads
// reaches bd past Client: argv (a beads.Command* builder, ConfigureCommand,
// NewBdCmd, or exec.Command naming "bd"), BdCmd (package cmd's alias of
// NewBdCmd) and BDRunner (the in-process bd test seam).
func scanRawBd(file string, src []byte, counts map[rawBdKey]int) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "beads" && n.Sel.Name == "BDRunner" {
				counts[rawBdKey{file, "BDRunner"}]++
			}
		case *ast.CallExpr:
			switch fn := n.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "BdCmd" {
					counts[rawBdKey{file, "BdCmd"}]++
				}
			case *ast.SelectorExpr:
				pkg, ok := fn.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case pkg.Name == "beads" && rawBdConstructors[fn.Sel.Name]:
					counts[rawBdKey{file, "argv"}]++
				case pkg.Name == "exec" && (fn.Sel.Name == "Command" || fn.Sel.Name == "CommandContext"):
					bin := 0
					if fn.Sel.Name == "CommandContext" {
						bin = 1
					}
					if len(n.Args) > bin {
						if lit, ok := n.Args[bin].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if s, _ := strconv.Unquote(lit.Value); s == "bd" {
								counts[rawBdKey{file, "argv"}]++
							}
						}
					}
				}
			}
		}
		return true
	})
	return nil
}

func parseRawBdBaseline(text string) (map[rawBdKey]int, error) {
	out := map[rawBdKey]int{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("baseline line %d: want \"<file> <kind> <count>\", got %q", n, line)
		}
		count, err := strconv.Atoi(f[2])
		if err != nil || count < 1 {
			return nil, fmt.Errorf("baseline line %d: bad count %q", n, f[2])
		}
		k := rawBdKey{f[0], f[1]}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("baseline line %d: duplicate entry %s %s", n, k.File, k.Kind)
		}
		out[k] = count
	}
	return out, sc.Err()
}

// TestNoNewRawBdSites is the ratchet half of the one-interface rule
// (gt-7iwy0.4.1): outside internal/beads, code reaches bd through
// beads.Client and the other typed methods, not argv. The sites still
// waiting to move are counted per file and kind in raw-bd-baseline.txt,
// which may only shrink.
//
// Fix a failure by calling a typed method (adding one to Client, the fake and
// the contract when none fits), never by raising a count.
func TestNoNewRawBdSites(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..")
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	fsys := r.FS()

	counts := map[rawBdKey]int{}
	files := 0
	err = fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case rel == "internal/beads", d.Name() == ".git", d.Name() == "testdata", d.Name() == "vendor", d.Name() == "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(rel) != ".go" {
			return nil
		}
		src, err := fs.ReadFile(fsys, rel)
		if err != nil {
			return err
		}
		files++
		return scanRawBd(rel, src, counts)
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that reads almost nothing passes every count.
	if files < 1000 {
		t.Fatalf("scanned %d Go files (floor 1000); the walk has stopped reading the repo", files)
	}

	src, err := os.ReadFile("raw-bd-baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := parseRawBdBaseline(string(src))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[rawBdKey]bool{}
	for k := range counts {
		keys[k] = true
	}
	for k := range baseline {
		keys[k] = true
	}
	sorted := make([]rawBdKey, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].File != sorted[j].File {
			return sorted[i].File < sorted[j].File
		}
		return sorted[i].Kind < sorted[j].Kind
	})
	for _, k := range sorted {
		got, want := counts[k], baseline[k]
		switch {
		case got > want:
			t.Errorf("%s: %d raw bd %s site(s), baseline %d; call a typed beads method instead", k.File, got, k.Kind, want)
		case got < want && got == 0:
			t.Errorf("raw-bd-baseline.txt: %s %s: baseline %d, found none; remove the entry", k.File, k.Kind, want)
		case got < want:
			t.Errorf("raw-bd-baseline.txt: %s %s: baseline %d, found %d; lower it to %d", k.File, k.Kind, want, got, got)
		}
	}
}

func TestScanRawBd(t *testing.T) {
	t.Parallel()
	src := `package p
func f() {
	beads.CommandWithEnv("", nil, "show")
	beads.NewBdCmd("list").Run()
	exec.Command("bd", "list")
	exec.CommandContext(ctx, "bd", "list")
	exec.Command("git", "status")
	exec.Command(bin, "list")
	BdCmd("show")
	var run beads.BDRunner
	beads.NewPlain("", nil).Show("x")
}`
	counts := map[rawBdKey]int{}
	if err := scanRawBd("p.go", []byte(src), counts); err != nil {
		t.Fatal(err)
	}
	want := map[rawBdKey]int{{"p.go", "argv"}: 4, {"p.go", "BdCmd"}: 1, {"p.go", "BDRunner"}: 1}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Errorf("counts = %v, want %v", counts, want)
	}
}
