package config

import (
	"bufio"
	"bytes"
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

// gtEnvIdentity are the GT_ variables production code may read from its
// environment (D5 Q4, gt-y3pgh.2): what a process is spawned with — role, rig,
// agent name, session, town root, tmux socket. GT_TOWN_ROOT is the one name gt
// reads for the town root; GT_ROOT survives only as the alias bd reads.
var gtEnvIdentity = map[string]bool{
	"GT_ROLE": true, "GT_RIG": true, "GT_CREW": true, "GT_POLECAT": true,
	"GT_DOG_NAME": true, "GT_SESSION": true, EnvAgent: true,
	EnvAgentOverride: true, "GT_TOWN_ROOT": true, "GT_TMUX_SOCKET": true,
}

// gtEnvPreferences are the GT_ variables that carry the operator's display and
// invocation preferences rather than a fact about the town: theme, pager,
// agent-mode marker, command name, and the ~/.gt data home. They stay env
// because a user sets them once for a shell and nothing resolves them.
var gtEnvPreferences = map[string]bool{
	"GT_THEME": true, "GT_PAGER": true, "GT_NO_PAGER": true,
	"GT_AGENT_MODE": true, "GT_COMMAND": true, "GT_HOME": true,
}

// gtEnvInvocation are the per-invocation session and hook signals: a hook
// command, a spawning session, or gt itself sets one for the single process it
// launches (prime's session-start hook, handoff's done call, the statusline's
// issue). They describe an invocation, not the town, so nothing outside it can
// supply them (gt-y3pgh.2).
var gtEnvInvocation = map[string]bool{
	"GT_SESSION_ID": true, "GT_SESSION_ID_ENV": true, "GT_HOOK_SOURCE": true,
	"GT_SESSION_START_CALLER": true, "GT_SESSION_START_REASON": true,
	"GT_DONE_FROM_HANDOFF": true, "GT_ISSUE": true, "GT_STALE_WARNED": true,
	"GT_PROCESS_NAMES": true,
}

// gtEnvAllowed reports whether production code may read name from the
// environment without a baseline line.
func gtEnvAllowed(name string) bool {
	return gtEnvIdentity[name] || gtEnvPreferences[name] || gtEnvInvocation[name]
}

// gtEnvReaders are the call names that read one variable from an
// environment: os.Getenv and os.LookupEnv, and the getenv seams that stand
// in for them.
var gtEnvReaders = map[string]bool{
	"Getenv": true, "LookupEnv": true, "getenv": true, "lookupEnv": true, "lookupEnvVar": true,
}

// TestNoNewGTEnvReads fails on any production read of a GT_ variable that
// gtEnvAllowed does not name and gtenv-baseline.txt does not already count
// (gt-y3pgh.2). The baseline holds the sites still waiting to move to config;
// it may only shrink. Fix a failure by reading the fact from config, never by
// raising a count. internal/testutil is the test harness and is not scanned.
func TestNoNewGTEnvReads(t *testing.T) {
	t.Parallel()
	reads, err := scanGTEnvReads(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Just under the 24 reads gtenv-baseline.txt listed on 2026-10-01: a
	// scanner gone blind passes everything.
	if len(reads) < 20 {
		t.Fatalf("scanGTEnvReads found %d non-allowlisted GT_ reads (floor 20); the scanner has stopped seeing them", len(reads))
	}
	src, err := os.ReadFile("gtenv-baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := parseGTEnvBaseline(string(src))
	if err != nil {
		t.Fatal(err)
	}
	fresh, stale := compareGTEnvBaseline(reads, baseline)
	for _, r := range fresh {
		t.Errorf("%s:%d reads %s from the environment; read it from config (gt-y3pgh.2)", r.file, r.line, r.name)
	}
	for _, s := range stale {
		t.Errorf("gtenv-baseline.txt: %s", s)
	}
}

func TestGTEnvScanFindsReads(t *testing.T) {
	t.Parallel()
	src := `package p

import "os"

const knob = "GT_KNOB"

func f(getenv func(string) string) {
	_ = os.Getenv("GT_A")
	_, _ = os.LookupEnv(knob)
	_ = getenv("GT_B")
	_ = os.Getenv("GT_ROLE")
	_ = os.Getenv("HOME")
	_ = other.Thing("GT_C")
}
`
	got, err := gtEnvReadsInFile("p/p.go", []byte(src), map[string]string{"other.KNOB": "GT_OTHER"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.name)
	}
	if want := "GT_A GT_KNOB GT_B"; strings.Join(names, " ") != want {
		t.Fatalf("reads = %v, want %s", names, want)
	}
}

func TestCompareGTEnvBaseline(t *testing.T) {
	t.Parallel()
	r := func(file, name string) gtEnvRead { return gtEnvRead{file: file, name: name} }
	reads := []gtEnvRead{r("a.go", "GT_X"), r("a.go", "GT_X"), r("b.go", "GT_Y"), r("c.go", "GT_Z")}
	baseline := map[gtEnvKey]int{
		{"a.go", "GT_X"}: 1, // grew
		{"b.go", "GT_Y"}: 1, // held
		{"c.go", "GT_Z"}: 3, // shrank
		{"d.go", "GT_W"}: 1, // gone
	}
	fresh, stale := compareGTEnvBaseline(reads, baseline)
	if len(fresh) != 2 || fresh[0].file != "a.go" {
		t.Errorf("fresh = %v, want both a.go GT_X reads", fresh)
	}
	if len(stale) != 2 {
		t.Errorf("stale = %v, want c.go lowered and d.go removed", stale)
	}
}

type gtEnvRead struct {
	file string
	line int
	name string
}

type gtEnvKey struct{ file, name string }

func (k gtEnvKey) String() string { return k.file + " " + k.name }

// scanGTEnvReads parses every non-test Go file under root's cmd and internal
// trees and returns its non-identity GT_ reads.
func scanGTEnvReads(root string) ([]gtEnvRead, error) {
	type file struct {
		rel string
		src []byte
	}
	var files []file
	consts := map[string]string{} // "<package name>.<Name>" -> "GT_..."
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if d.Name() == "testdata" || rel == "internal/testutil" {
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
			if !bytes.Contains(src, []byte(`"GT_`)) {
				return nil
			}
			files = append(files, file{rel, src})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// Exported GT_ constants first, so a read through pkg.Name resolves.
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f.rel, f.src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for name, v := range gtStringConsts(af) {
			consts[af.Name.Name+"."+name] = v
		}
	}
	var reads []gtEnvRead
	for _, f := range files {
		got, err := gtEnvReadsInFile(f.rel, f.src, consts)
		if err != nil {
			return nil, err
		}
		reads = append(reads, got...)
	}
	return reads, nil
}

// gtStringConsts returns the string constants of f whose value names a GT_
// variable.
func gtStringConsts(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(v, "GT_") {
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out
}

// gtEnvReadsInFile returns the non-identity GT_ reads in one file: a call to
// a gtEnvReaders name whose first argument is a GT_ string literal, a GT_
// constant of the file's own package, or pkg.Const from consts.
func gtEnvReadsInFile(rel string, src []byte, consts map[string]string) ([]gtEnvRead, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	local := map[string]string{}
	for k, v := range consts {
		if pkg, name, _ := strings.Cut(k, "."); pkg == f.Name.Name {
			local[name] = v
		}
	}
	for name, v := range gtStringConsts(f) {
		local[name] = v
	}
	var reads []gtEnvRead
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var fn string
		switch c := call.Fun.(type) {
		case *ast.Ident:
			fn = c.Name
		case *ast.SelectorExpr:
			fn = c.Sel.Name
		}
		if !gtEnvReaders[fn] {
			return true
		}
		var name string
		switch a := call.Args[0].(type) {
		case *ast.BasicLit:
			if a.Kind == token.STRING {
				name, _ = strconv.Unquote(a.Value)
			}
		case *ast.Ident:
			name = local[a.Name]
		case *ast.SelectorExpr:
			if pkg, ok := a.X.(*ast.Ident); ok {
				name = consts[pkg.Name+"."+a.Sel.Name]
			}
		}
		if strings.HasPrefix(name, "GT_") && !gtEnvAllowed(name) {
			reads = append(reads, gtEnvRead{file: rel, line: fset.Position(call.Pos()).Line, name: name})
		}
		return true
	})
	return reads, nil
}

func parseGTEnvBaseline(text string) (map[gtEnvKey]int, error) {
	out := map[gtEnvKey]int{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("baseline line %d: want \"<file> <GT_VAR> <count>\", got %q", n, line)
		}
		count, err := strconv.Atoi(f[2])
		if err != nil || count < 1 {
			return nil, fmt.Errorf("baseline line %d: bad count %q", n, f[2])
		}
		k := gtEnvKey{f[0], f[1]}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("baseline line %d: duplicate entry %s", n, k)
		}
		out[k] = count
	}
	return out, sc.Err()
}

// compareGTEnvBaseline splits reads against the baseline: a key whose count
// grew reports all its reads as fresh; a key whose count shrank, or
// vanished, is stale and must be lowered so the baseline only goes down.
func compareGTEnvBaseline(reads []gtEnvRead, baseline map[gtEnvKey]int) (fresh []gtEnvRead, stale []string) {
	byKey := map[gtEnvKey][]gtEnvRead{}
	for _, r := range reads {
		k := gtEnvKey{r.file, r.name}
		byKey[k] = append(byKey[k], r)
	}
	for k, rs := range byKey {
		if len(rs) > baseline[k] {
			fresh = append(fresh, rs...)
		}
	}
	for k, want := range baseline {
		switch got := len(byKey[k]); {
		case got == 0:
			stale = append(stale, fmt.Sprintf("%s: baseline %d, found none; remove the entry", k, want))
		case got < want:
			stale = append(stale, fmt.Sprintf("%s: baseline %d, found %d; lower it to %d", k, want, got, got))
		}
	}
	sort.Slice(fresh, func(i, j int) bool {
		if fresh[i].file != fresh[j].file {
			return fresh[i].file < fresh[j].file
		}
		return fresh[i].line < fresh[j].line
	})
	sort.Strings(stale)
	return fresh, stale
}
