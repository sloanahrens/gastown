package config

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// gtEnvIdentity are the GT_ variables production code may read from its
// environment (D5 Q4, gt-y3pgh.2): what a process is spawned with — role, rig,
// agent name, session, town root, tmux socket, and the polecat worktree path
// the session manager exports at spawn. GT_TOWN_ROOT is the one name gt reads
// for the town root; GT_ROOT survives only as the alias bd reads.
var gtEnvIdentity = map[string]bool{
	"GT_ROLE": true, "GT_RIG": true, "GT_CREW": true, "GT_POLECAT": true,
	"GT_DOG_NAME": true, "GT_SESSION": true, EnvAgent: true,
	EnvAgentOverride: true, "GT_TOWN_ROOT": true, "GT_TMUX_SOCKET": true,
	"GT_POLECAT_PATH": true,
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

// gtEnvTestHarness are the GT_ variables internal/testutil — or the Makefile,
// for an integration run — sets in the environment of the gt and bd
// SUBPROCESSES a test spawns. No in-process seam can replace them: the process
// that reads one is not the process that set it. Each name states why it
// cannot come from config.
var gtEnvTestHarness = map[string]bool{
	// The harness marks gt subprocesses so they suppress the operator's usage
	// log (internal/cmd/telemetry.go) and event log (internal/events).
	"GT_TEST_HERMETIC": true,
	// The harness points this at the live town root so workspace resolution in
	// a gt subprocess whose cwd walks up into it finds no workspace.
	"GT_TEST_FORBIDDEN_TOWN_ROOT": true,
	// A test's Dolt server runs off the default port; gt install probes it
	// instead of adopting it as the town's own.
	"GT_TEST_EXTERNAL_DOLT": true,
	// The container-test opt-in, read from the hook environment being judged.
	"GT_TEST_DOCKER": true,
	// The Makefile pins the Dolt init pool size for an integration run's
	// processes.
	"GT_TEST_DOLT_INIT_CONCURRENCY": true,
}

// gtEnvAllowed reports whether production code may read name from the
// environment.
func gtEnvAllowed(name string) bool {
	return gtEnvIdentity[name] || gtEnvPreferences[name] ||
		gtEnvInvocation[name] || gtEnvTestHarness[name]
}

// gtEnvReaders are the call names that read one variable from an
// environment: os.Getenv and os.LookupEnv, and the getenv seams that stand
// in for them.
var gtEnvReaders = map[string]bool{
	"Getenv": true, "LookupEnv": true, "getenv": true, "lookupEnv": true, "lookupEnvVar": true,
}

// TestNoNewGTEnvReads fails on any production read of a GT_ variable that
// gtEnvAllowed does not name (gt-y3pgh.2). Everything else comes from config:
// a read of a new GT_ variable is the signal that a config field is missing.
// internal/testutil is the test harness and is not scanned.
func TestNoNewGTEnvReads(t *testing.T) {
	t.Parallel()
	reads, err := scanGTEnvReads(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Floor at 102, under the GT_ reads the allowlists named when the last
	// baseline entry moved into them (gt-y3pgh.2.10). A scanner that has gone
	// blind reports none and trips it, while a legitimate edit to an allowlist
	// still leaves headroom.
	if len(reads) < 102 {
		t.Fatalf("scanGTEnvReads found %d GT_ reads (floor 102); the scanner has stopped seeing them", len(reads))
	}
	for _, r := range reads {
		if !r.allowed {
			t.Errorf("%s:%d reads %s from the environment; read it from config (gt-y3pgh.2)", r.file, r.line, r.name)
		}
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
	if want := "GT_A GT_KNOB GT_B GT_ROLE"; strings.Join(names, " ") != want {
		t.Fatalf("reads = %v, want %s", names, want)
	}
	if !got[len(got)-1].allowed {
		t.Errorf("GT_ROLE read not marked allowlisted: %+v", got[len(got)-1])
	}
	for _, r := range got[:len(got)-1] {
		if r.allowed {
			t.Errorf("%s read marked allowlisted, want not: %+v", r.name, r)
		}
	}
}

// TestGTEnvAllowlistedReadsCountTowardTotal feeds the scanner a file whose only
// GT_ read is allowlisted: it must still count toward the total, so
// TestNoNewGTEnvReads' floor cannot be met by a scanner that sees only the
// reads it is about to reject.
func TestGTEnvAllowlistedReadsCountTowardTotal(t *testing.T) {
	t.Parallel()
	src := `package p

import "os"

func f() {
	_ = os.Getenv("GT_ROLE")
}
`
	got, err := gtEnvReadsInFile("p/p.go", []byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].name != "GT_ROLE" {
		t.Fatalf("reads = %+v, want one GT_ROLE read", got)
	}
	if !got[0].allowed {
		t.Fatalf("GT_ROLE read not marked allowlisted: %+v", got[0])
	}
}

type gtEnvRead struct {
	file string
	line int
	name string
	// allowed records whether gtEnvAllowed names the variable. The scanner
	// returns allowlisted reads too, so the floor in TestNoNewGTEnvReads
	// counts every GT_ read.
	allowed bool
}

// scanGTEnvReads parses every non-test Go file under root's cmd and internal
// trees and returns its GT_ reads, allowlisted ones included.
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

// gtEnvReadsInFile returns the GT_ reads in one file: a call to a gtEnvReaders
// name whose first argument is a GT_ string literal, a GT_ constant of the
// file's own package, or pkg.Const from consts. Each read records whether
// gtEnvAllowed names it.
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
		if strings.HasPrefix(name, "GT_") {
			reads = append(reads, gtEnvRead{
				file:    rel,
				line:    fset.Position(call.Pos()).Line,
				name:    name,
				allowed: gtEnvAllowed(name),
			})
		}
		return true
	})
	return reads, nil
}
