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
	"testing/fstest"
)

// rawBdConstructors are the internal/beads functions that hand a caller bd
// argv instead of a typed method.
var rawBdConstructors = map[string]bool{
	"CommandWithEnv":         true,
	"CommandContextWithEnv":  true,
	"CommandWithPath":        true,
	"CommandContextWithPath": true,
	"ConfigureCommand":       true,
}

// rawBdKey is one file's count of one kind of raw bd site.
type rawBdKey struct {
	File string
	Kind string // "argv", "BdCmd" or "BDRunner"
}

// scanRawBd counts, in one Go file, the ways code outside internal/beads
// reaches bd past Client: argv (a beads.Command* builder, ConfigureCommand,
// or exec.Command naming "bd"), BdCmd (a bare BdCmd(...) call, the shape the
// deleted NewBdCmd alias took) and BDRunner (the in-process bd test seam).
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

// rawBdFileFloor is the fewest Go files a walk of the repo may read before its
// counts mean anything: a walk that reads almost nothing passes every count.
const rawBdFileFloor = 1000

// scanRawBdTree counts the raw bd sites in every Go file under fsys, and how
// many Go files it read. It skips internal/beads, the one package allowed to
// hand out bd argv, and the trees that hold no hand-written Go.
func scanRawBdTree(fsys fs.FS) (map[rawBdKey]int, int, error) {
	counts := map[rawBdKey]int{}
	files := 0
	err := fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
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
	return counts, files, err
}

// rawBdProblems reports every way the raw bd sites under fsys break the
// one-interface rule against baseline: a count that grew, a baseline entry
// that no longer matches its site, and a walk that read fewer than floor Go
// files or could not finish. An empty result passes. A walk error is itself a
// problem, never a silent pass.
func rawBdProblems(fsys fs.FS, baseline map[rawBdKey]int, floor int) []string {
	counts, files, err := scanRawBdTree(fsys)
	if err != nil {
		return []string{fmt.Sprintf("walk stopped before the counts were complete: %v", err)}
	}
	var problems []string
	if files < floor {
		problems = append(problems, fmt.Sprintf("scanned %d Go files (floor %d); the walk has stopped reading the repo", files, floor))
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
			problems = append(problems, fmt.Sprintf("%s: %d raw bd %s site(s), baseline %d; call a typed beads method instead", k.File, got, k.Kind, want))
		case got < want && got == 0:
			problems = append(problems, fmt.Sprintf("raw-bd-baseline.txt: %s %s: baseline %d, found none; remove the entry", k.File, k.Kind, want))
		case got < want:
			problems = append(problems, fmt.Sprintf("raw-bd-baseline.txt: %s %s: baseline %d, found %d; lower it to %d", k.File, k.Kind, want, got, got))
		}
	}
	return problems
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
	src, err := os.ReadFile("raw-bd-baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := parseRawBdBaseline(string(src))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..", "..")
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	for _, problem := range rawBdProblems(r.FS(), baseline, rawBdFileFloor) {
		t.Error(problem)
	}
}

// TestRawBdPolicy drives the rule behind TestNoNewRawBdSites over in-memory
// trees, so every way it refuses has a case here: a site in any Go file
// outside internal/beads (test files included), a count that grew, a baseline
// entry that no longer matches, a walk too small to trust, and a walk that
// fails.
func TestRawBdPolicy(t *testing.T) {
	t.Parallel()

	clean := "package p\n\nfunc f() {}\n"
	site := "package p\n\nfunc f() {\n\texec.Command(\"bd\", \"list\")\n}\n"
	twoSites := "package p\n\nfunc f() {\n\texec.Command(\"bd\", \"list\")\n\texec.Command(\"bd\", \"show\", \"x\")\n}\n"

	cases := []struct {
		name     string
		fsys     fs.FS
		baseline map[rawBdKey]int
		floor    int
		want     []string
	}{
		{
			name:  "a clean tree passes",
			fsys:  tree("internal/cmd/clean.go", clean),
			floor: 1,
		},
		{
			name:  "the seam package may hand out bd argv",
			fsys:  tree("internal/beads/seam.go", site, "internal/cmd/clean.go", clean),
			floor: 1,
		},
		{
			name:  "testdata and vendor are not scanned",
			fsys:  tree("internal/cmd/clean.go", clean, "internal/cmd/testdata/x.go", site, "vendor/dep/x.go", site),
			floor: 1,
		},
		{
			name:  "a file outside the seam that execs bd fails",
			fsys:  tree("internal/cmd/run.go", site),
			floor: 1,
			want:  []string{"internal/cmd/run.go: 1 raw bd argv site(s), baseline 0; call a typed beads method instead"},
		},
		{
			name:     "a count above its baseline fails",
			fsys:     tree("internal/cmd/two.go", twoSites),
			baseline: map[rawBdKey]int{{"internal/cmd/two.go", "argv"}: 1},
			floor:    1,
			want:     []string{"internal/cmd/two.go: 2 raw bd argv site(s), baseline 1"},
		},
		{
			name:     "a count below its baseline fails",
			fsys:     tree("internal/cmd/one.go", site),
			baseline: map[rawBdKey]int{{"internal/cmd/one.go", "argv"}: 2},
			floor:    1,
			want:     []string{"internal/cmd/one.go argv: baseline 2, found 1; lower it to 1"},
		},
		{
			name:     "a baseline entry with no site left fails",
			fsys:     tree("internal/cmd/clean.go", clean),
			baseline: map[rawBdKey]int{{"internal/cmd/gone.go", "argv"}: 1},
			floor:    1,
			want:     []string{"internal/cmd/gone.go argv: baseline 1, found none; remove the entry"},
		},
		{
			name:  "a walk that reads too few files fails",
			fsys:  tree("internal/cmd/clean.go", clean),
			floor: 2,
			want:  []string{"scanned 1 Go files (floor 2); the walk has stopped reading the repo"},
		},
		{
			name:  "a walk error is a problem, not a pass",
			fsys:  walkFailFS{FS: tree("internal/cmd/clean.go", clean, "internal/cmd/run.go", site), path: "internal/cmd"},
			floor: 0,
			want:  []string{"walk stopped before the counts were complete:"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rawBdProblems(tc.fsys, tc.baseline, tc.floor)
			if len(got) != len(tc.want) {
				t.Fatalf("problems = %q, want %d matching %q", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

// tree builds an in-memory tree from alternating path and content arguments.
func tree(paths ...string) fs.FS {
	m := fstest.MapFS{}
	for i := 0; i < len(paths); i += 2 {
		m[paths[i]] = &fstest.MapFile{Data: []byte(paths[i+1])}
	}
	return m
}

// walkFailFS hands fs.WalkDir a tree whose one directory cannot be opened,
// standing in for a read the walk cannot finish. It hides ReadDirFS so the
// walk falls back to Open, which is where the failure lands.
type walkFailFS struct {
	fs.FS
	path string
}

func (f walkFailFS) Open(name string) (fs.File, error) {
	if name == f.path {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return f.FS.Open(name)
}

func TestScanRawBd(t *testing.T) {
	t.Parallel()
	src := `package p
func f() {
	beads.CommandWithEnv("", nil, "show")
	beads.CommandContextWithEnv(ctx, "", nil, "list")
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
