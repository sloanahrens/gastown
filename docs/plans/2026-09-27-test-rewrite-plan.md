# Test Suite Rewrite — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the gastown unit test tier finish in 90 s or less, uncached, in a pane macOS still scans, with no flaky tests. Every rule is enforced by a test that runs in `make test`.

**Architecture:** An enforcement package (`internal/testpolicy`) ratchets packages from an "unconverted" list to enforced. Each package is converted by injecting seams (an exec runner, a `clockwork.Clock`), replacing real-tool tests with shared behaviour fakes that are checked by contract suites, and moving real-tool tests behind `//go:build integration`. This plan details steps 0 and 1 of the design (the scaffolding and the tmux pilot) and gives a repeatable per-package task for everything after.

**Tech Stack:** Go 1.26.2, module `github.com/steveyegge/gastown`, `github.com/jonboulle/clockwork`, `go/parser`/`go/ast`/`go/build` (standard library), GNU make, bash, tmux, launchd (for `test-timing` only).

**Spec:** `docs/plans/2026-09-27-test-rewrite-design.md`

## Global Constraints

- Work in `~/gt/gastown/crew/sloan` or in worktrees made from it (`git worktree add ../sloan-<task> -b crew/sloan/<branch> origin/main`). Always `git fetch origin` before branching. Never fetch in `mayor/rig` or `refinery/rig`.
- Each MR goes on its own branch off fresh `origin/main`. Push with `git push origin <branch>`, then run `gt mq submit`, which hands it to the gastown refinery. Never sling. Never push to main.
- No AI attribution anywhere: no `Co-Authored-By` trailer and no mention of Claude in commits, code or docs. Before every push, check that `git log origin/main..HEAD --format=%B | grep -ci 'co-authored\|claude'` prints `0`.
- After each commit, from `~/.claude`: `bd comments add claude-a3e.2 "commit: <hash> — <summary>"`.
- Gate before every push: `make build`, `make lint`, and `GOFLAGS=-p=8 make test`. Decide pass or fail by the **exit code** only, never by a piped `tail` or `grep` (memory: failed-measurement-serializes-as-success).
- Unit-tier rules (design §4): no executable files, no `go build`, no subprocess except `git`, no `time.Sleep`/`After`/`NewTimer`/`NewTicker`/`Tick`/`AfterFunc`, no `Setenv`/`Unsetenv`, no `Chdir`, no assignments to package-level variables declared in production files, no `t.Skip*`, and every top-level `Test*` calls `t.Parallel()`.
- Line exemption format: `//testpolicy:allow <rule> — <reason>`. The reason is mandatory.
- Integration tests: the file starts with `//go:build integration` and every test function in it is named `TestIntegration*`.
- The budget is 10 s per converted package, measured by `go test -json` package elapsed time.
- Targets, measured with `scripts/test-timing.sh`: pilot `tmux` unit tier ≤ 5 s; tmux integration tier ≤ 60 s; final unit tier ≤ 90 s; integration tier ≤ 90 s; full suite ≤ 3 min.
- Each package-conversion MR's commit message lists every deleted test as `- TestName: reason`, and gives statement coverage before and after for the package (`go test -cover ./internal/<pkg>/`). A drop of more than 2 points needs a sentence of justification.
- The Windows code is deleted in Task 12, not before. Until then, `process_group_windows.go` and `flock_windows.go` must keep compiling (`GOOS=windows go vet ./internal/tmux/`).
- Kill processes only by explicit pid. Never remove Docker containers by name pattern.

## File Structure

| Path | Responsibility | Task |
|---|---|---|
| `internal/testpolicy/scan.go` | `ScanDir`: parse one package directory and return rule violations | 1 |
| `internal/testpolicy/rules.go` | the rule tables and per-file checks | 1 |
| `internal/testpolicy/allow.go` | `//testpolicy:allow` parsing and filtering | 1 |
| `internal/testpolicy/scan_test.go` | fixture-driven tests for every rule | 1 |
| `internal/testpolicy/testdata/<case>/*.go` | one fixture directory per rule (ignored by `go build`) | 1 |
| `internal/testpolicy/policy_test.go` | `TestPolicy`: walk the repo, apply `unconverted.txt` | 2 |
| `internal/testpolicy/list.go` | `ReadList`: parse `unconverted.txt` | 2 |
| `internal/testpolicy/unconverted.txt` | packages not yet enforced; only shrinks | 2 |
| `internal/testpolicy/contracts.go` | `CheckContracts`: each `*fake` package has an exported contract with one integration caller | 2 |
| `internal/testpolicy/budget.go` | `WatchBudget`: read the `go test -json` stream, report overruns | 3 |
| `internal/testpolicy/budget_test.go` | canned-JSON tests for `WatchBudget` | 3 |
| `internal/testpolicy/cmd/budget/main.go` | runs `go test -json` as a child process and applies `WatchBudget` | 3 |
| `Makefile` | `test` goes through budget; adds `test-integration` and `test-timing` | 3 |
| `scripts/test-timing.sh` | unit-tier timing in a non-exempt, launchd-started tmux server | 3 |
| `internal/tmux/exec.go` | the `execFunc` type, `realExec`, and the `Tmux` accessors `runner()` and `clk()` | 4 |
| `internal/tmux/helpers_test.go` | `newTmuxForTest`, `recorder`, `driveClock`: shared unit-test helpers | 4, 5 |
| `internal/tmux/tmux.go`, `process_group_unix.go`, `socket_guard_unix.go`, `flock_unix.go`, `submit_verify.go`, `composer_stall.go` | route every exec and clock call through the seams | 4, 5 |
| `internal/tmux/tmuxfake/fake.go` | `Server`: in-memory tmux | 6 |
| `internal/tmux/tmuxfake/contract.go` | `Sessions` interface and `RunSessionsContract` | 6 |
| `internal/tmux/tmuxfake/fake_test.go` | runs the contract against the fake | 6 |
| `internal/tmux/contract_integration_test.go` | runs the contract against real tmux | 6 |
| `internal/tmux/*_test.go` | rewritten per the bucket procedure | 7 |
| `docs/testing.md` | rules and worked examples from the pilot | 8 |

MR boundaries: **MR 1** is Tasks 1–3. **MR 2** is Tasks 4–5. **MR 3** is Tasks 6–8. Tasks 9–13 are one MR per package or step.

---

## MR 1 — scaffolding (no behaviour change)

Branch: `crew/sloan/test-policy-scaffold` off fresh `origin/main`.

### Task 1: The policy scanner

**Files:**
- Create: `internal/testpolicy/scan.go`, `internal/testpolicy/rules.go`, `internal/testpolicy/allow.go`
- Create: `internal/testpolicy/scan_test.go`
- Create fixture directories, each holding one or two `.go` files: `internal/testpolicy/testdata/{clean,sleep,env,chdir,skip,exec_other,exec_go,exec_git,exec_file,shebang,global_swap,global_shadow,parallel_missing,allow_ok,allow_noreason,prod_setenv,prod_sleep_clock,integration_skipped}/`

**Interfaces:**
- Produces:
  - `type Violation struct { Pos token.Position; Rule, Msg string }`
  - `func (v Violation) String() string`
  - `func ScanDir(dir string) ([]Violation, error)`
  - rule-name constants `RuleNoSleep = "no-sleep"`, `RuleNoEnv = "no-env"`, `RuleNoChdir = "no-chdir"`, `RuleNoSkip = "no-skip"`, `RuleNoSubprocess = "no-subprocess"`, `RuleNoBuild = "no-build"`, `RuleNoExecFiles = "no-exec-files"`, `RuleNoGlobalSwap = "no-global-swap"`, `RuleParallel = "parallel"`, `RuleAllowReason = "allow-reason"`, `RuleProdSetenv = "prod-no-setenv"`, `RuleProdSleep = "prod-no-sleep"`

- [ ] **Step 1: Write the fixtures.** These are package directories under `testdata`, so the Go toolchain ignores them. The scanner parses them; nothing compiles them. Examples:

`testdata/sleep/a_test.go`:
```go
package sleep

import (
	"testing"
	"time"
)

func TestA(t *testing.T) {
	t.Parallel()
	time.Sleep(time.Millisecond)
}
```

`testdata/global_swap/prod.go` and `testdata/global_swap/a_test.go`:
```go
package globalswap

var runCmd = func() error { return nil }
```
```go
package globalswap

import "testing"

func TestA(t *testing.T) {
	t.Parallel()
	runCmd = func() error { return nil }
}
```

`testdata/global_shadow/prod.go` has the same `var runCmd`. Its test declares `runCmd := func() error { return nil }; runCmd = nil; _ = runCmd` inside the test, and **must produce no violation** (a local shadow is not a swap).

`testdata/exec_git/a_test.go` uses `exec.Command("git", "init")` and must be clean. `exec_go` uses `exec.Command("go", "build")` and must report `no-build`. `exec_other` uses `exec.CommandContext(ctx, "tmux", "ls")` and must report `no-subprocess`.

`testdata/exec_file/a_test.go` calls `os.WriteFile(p, b, 0o755)` and must report `no-exec-files`. `testdata/shebang/a_test.go` contains the literal `"#!/bin/sh\necho hi\n"` and must report `no-exec-files`.

`testdata/allow_ok/a_test.go`:
```go
func TestA(t *testing.T) {
	t.Parallel()
	time.Sleep(time.Millisecond) //testpolicy:allow no-sleep — measures real scheduler latency
}
```
It must be clean. `allow_noreason` has `//testpolicy:allow no-sleep` with no reason, and must report `allow-reason` while still suppressing `no-sleep`.

`testdata/integration_skipped/a_test.go` starts with `//go:build integration` and calls `time.Sleep`. It must produce no violation, because the file is excluded by its build constraint.

`testdata/prod_setenv/prod.go` is `package prodsetenv` and calls `os.Setenv("A","b")`. It must report `prod-no-setenv`.

`testdata/prod_sleep_clock/prod.go` imports `github.com/jonboulle/clockwork` and calls `time.Sleep`. It must report `prod-no-sleep`.

`testdata/parallel_missing/a_test.go` has a `TestA(t *testing.T)` without `t.Parallel()`. It must report `parallel`. The same file also has `func TestMain(m *testing.M) { os.Exit(m.Run()) }`, which must **not** be reported.

`testdata/clean/` holds a production file plus a test that obeys every rule. It must produce zero violations.

- [ ] **Step 2: Write the failing table test** `internal/testpolicy/scan_test.go`:

```go
package testpolicy

import (
	"path/filepath"
	"sort"
	"testing"
)

func TestScanDirFixtures(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{ // fixture dir -> sorted expected rules
		"clean":               nil,
		"sleep":               {RuleNoSleep},
		"env":                 {RuleNoEnv, RuleNoEnv},       // os.Setenv and t.Setenv
		"chdir":               {RuleNoChdir, RuleNoChdir},   // os.Chdir and t.Chdir
		"skip":                {RuleNoSkip, RuleNoSkip, RuleNoSkip}, // Skip, Skipf, SkipNow
		"exec_other":          {RuleNoSubprocess},
		"exec_go":             {RuleNoBuild},
		"exec_git":            nil,
		"exec_file":           {RuleNoExecFiles},
		"shebang":             {RuleNoExecFiles},
		"global_swap":         {RuleNoGlobalSwap},
		"global_shadow":       nil,
		"parallel_missing":    {RuleParallel},
		"allow_ok":            nil,
		"allow_noreason":      {RuleAllowReason},
		"prod_setenv":         {RuleProdSetenv},
		"prod_sleep_clock":    {RuleProdSleep},
		"integration_skipped": nil,
	}
	for dir, want := range cases {
		dir, want := dir, want
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			vs, err := ScanDir(filepath.Join("testdata", dir))
			if err != nil {
				t.Fatalf("ScanDir: %v", err)
			}
			var got []string
			for _, v := range vs {
				got = append(got, v.Rule)
			}
			sort.Strings(got)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("rules = %v, want %v\n%v", got, want, vs)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("rules = %v, want %v\n%v", got, want, vs)
				}
			}
		})
	}
}
```

Make the fixture files match those counts exactly: `env` calls `os.Setenv` once and `t.Setenv` once; `chdir` calls `os.Chdir` once and `t.Chdir` once; `skip` calls `t.Skip`, `t.Skipf` and `t.SkipNow` once each. Every fixture test function other than the one in `parallel_missing` calls `t.Parallel()` first.

- [ ] **Step 3: Run it and confirm it fails**

Run: `go test ./internal/testpolicy/ -run TestScanDirFixtures`
Expected: build failure, `undefined: ScanDir`.

- [ ] **Step 4: Implement `scan.go`**

```go
// Package testpolicy enforces the unit-test rules described in docs/testing.md.
// It works on syntax only (go/parser), so scanning the whole repository takes
// seconds and needs no type-checking.
package testpolicy

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Violation is one broken rule at one source position.
type Violation struct {
	Pos  token.Position
	Rule string
	Msg  string
}

func (v Violation) String() string { return fmt.Sprintf("%s: [%s] %s", v.Pos, v.Rule, v.Msg) }

// ScanDir checks the Go files of one package directory, not recursively.
// Files excluded by build constraints for the current platform with no extra
// tags are skipped, and that includes every //go:build integration file.
func ScanDir(dir string) ([]Violation, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var prod, tests []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		ok, err := build.Default.MatchFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(name, "_test.go") {
			tests = append(tests, f)
		} else {
			prod = append(prod, f)
		}
	}
	pkgVars := packageVars(prod)
	clocked := importsPath(prod, "github.com/jonboulle/clockwork")
	var vs []Violation
	for _, f := range tests {
		vs = append(vs, checkTestFile(fset, f, pkgVars)...)
	}
	for _, f := range prod {
		vs = append(vs, checkProdFile(fset, f, clocked)...)
	}
	vs = applyAllows(fset, append(append([]*ast.File{}, tests...), prod...), vs)
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Pos.Filename != vs[j].Pos.Filename {
			return vs[i].Pos.Filename < vs[j].Pos.Filename
		}
		return vs[i].Pos.Line < vs[j].Pos.Line
	})
	return vs, nil
}

// packageVars returns the names of package-level variables declared in the
// production files of a package.
func packageVars(files []*ast.File) map[string]bool {
	vars := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, s := range gd.Specs {
				for _, n := range s.(*ast.ValueSpec).Names {
					vars[n.Name] = true
				}
			}
		}
	}
	return vars
}

func importsPath(files []*ast.File, path string) bool {
	for _, f := range files {
		for _, im := range f.Imports {
			if strings.Trim(im.Path.Value, `"`) == path {
				return true
			}
		}
	}
	return false
}

// imports maps each import's local name to its path for one file.
func imports(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, im := range f.Imports {
		p := strings.Trim(im.Path.Value, `"`)
		name := filepath.Base(p)
		if im.Name != nil {
			name = im.Name.Name
		}
		m[name] = p
	}
	return m
}
```

- [ ] **Step 5: Implement `rules.go`**

```go
package testpolicy

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

const (
	RuleNoSleep      = "no-sleep"
	RuleNoEnv        = "no-env"
	RuleNoChdir      = "no-chdir"
	RuleNoSkip       = "no-skip"
	RuleNoSubprocess = "no-subprocess"
	RuleNoBuild      = "no-build"
	RuleNoExecFiles  = "no-exec-files"
	RuleNoGlobalSwap = "no-global-swap"
	RuleParallel     = "parallel"
	RuleAllowReason  = "allow-reason"
	RuleProdSetenv   = "prod-no-setenv"
	RuleProdSleep    = "prod-no-sleep"
)

// bannedTestCalls maps "importpath.Func" to the rule it breaks in a unit test.
var bannedTestCalls = map[string]string{
	"time.Sleep": RuleNoSleep, "time.After": RuleNoSleep, "time.AfterFunc": RuleNoSleep,
	"time.NewTimer": RuleNoSleep, "time.NewTicker": RuleNoSleep, "time.Tick": RuleNoSleep,
	"os.Setenv": RuleNoEnv, "os.Unsetenv": RuleNoEnv,
	"os.Chdir": RuleNoChdir,
}

// bannedTestMethods maps a method name called on a *testing.T/B/F or
// testing.TB value to the rule it breaks.
var bannedTestMethods = map[string]string{
	"Setenv": RuleNoEnv, "Chdir": RuleNoChdir,
	"Skip": RuleNoSkip, "Skipf": RuleNoSkip, "SkipNow": RuleNoSkip,
}

func checkTestFile(fset *token.FileSet, f *ast.File, pkgVars map[string]bool) []Violation {
	imp := imports(f)
	tvars := testingVars(f, imp)
	var vs []Violation
	add := func(n ast.Node, rule, msg string) {
		vs = append(vs, Violation{Pos: fset.Position(n.Pos()), Rule: rule, Msg: msg})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			checkTestCall(n, imp, tvars, add)
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				if s, err := strconv.Unquote(n.Value); err == nil && strings.HasPrefix(s, "#!") {
					add(n, RuleNoExecFiles, "writes a script; use a fake instead of an executable")
				}
			}
		case *ast.AssignStmt:
			if n.Tok != token.ASSIGN {
				return true
			}
			for _, lhs := range n.Lhs {
				switch l := lhs.(type) {
				case *ast.Ident:
					if l.Obj == nil && pkgVars[l.Name] {
						add(l, RuleNoGlobalSwap, "assigns package variable "+l.Name+"; inject the dependency instead")
					}
				case *ast.SelectorExpr:
					if x, ok := l.X.(*ast.Ident); ok && x.Obj == nil && imp[x.Name] != "" {
						add(l, RuleNoGlobalSwap, "assigns package variable "+x.Name+"."+l.Sel.Name)
					}
				}
			}
		}
		return true
	})
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "Test") || fd.Name.Name == "TestMain" {
			continue
		}
		param := testingParam(fd, imp, "T")
		if param == "" {
			continue
		}
		if !callsMethodOn(fd.Body, param, "Parallel") {
			add(fd.Name, RuleParallel, fd.Name.Name+" does not call "+param+".Parallel()")
		}
	}
	return vs
}

func checkTestCall(c *ast.CallExpr, imp map[string]string, tvars map[string]bool, add func(ast.Node, string, string)) {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	if tvars[x.Name] {
		if rule, bad := bannedTestMethods[sel.Sel.Name]; bad {
			add(c, rule, x.Name+"."+sel.Sel.Name+" is not allowed in a unit test")
		}
		return
	}
	path := imp[x.Name]
	if path == "" || x.Obj != nil {
		return
	}
	full := path + "." + sel.Sel.Name
	if rule, bad := bannedTestCalls[full]; bad {
		add(c, rule, full+" is not allowed in a unit test")
		return
	}
	switch full {
	case "os/exec.Command", "os/exec.CommandContext":
		i := 0
		if sel.Sel.Name == "CommandContext" {
			i = 1
		}
		if len(c.Args) <= i {
			return
		}
		name := stringLit(c.Args[i])
		switch name {
		case "git":
		case "go":
			add(c, RuleNoBuild, "runs the go tool; build nothing in a unit test")
		default:
			add(c, RuleNoSubprocess, "runs a subprocess ("+name+"); only git is allowed in a unit test")
		}
	case "os.WriteFile", "os.OpenFile":
		if len(c.Args) == 3 && execMode(c.Args[2]) {
			add(c, RuleNoExecFiles, "creates an executable file")
		}
	case "os.Chmod":
		if len(c.Args) == 2 && execMode(c.Args[1]) {
			add(c, RuleNoExecFiles, "makes a file executable")
		}
	}
}

func checkProdFile(fset *token.FileSet, f *ast.File, clocked bool) []Violation {
	imp := imports(f)
	var vs []Violation
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Obj != nil {
			return true
		}
		full := imp[x.Name] + "." + sel.Sel.Name
		switch {
		case (full == "os.Setenv" || full == "os.Unsetenv") && f.Name.Name != "main":
			vs = append(vs, Violation{fset.Position(c.Pos()), RuleProdSetenv, full + " mutates the process environment; pass config.Runtime instead"})
		case full == "time.Sleep" && clocked:
			vs = append(vs, Violation{fset.Position(c.Pos()), RuleProdSleep, "package holds a clockwork.Clock; sleep through it"})
		}
		return true
	})
	return vs
}

// testingVars returns the names of every parameter in the file whose type is
// *testing.T, *testing.B, *testing.F or testing.TB.
func testingVars(f *ast.File, imp map[string]string) map[string]bool {
	vars := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		var ft *ast.FuncType
		switch n := n.(type) {
		case *ast.FuncDecl:
			ft = n.Type
		case *ast.FuncLit:
			ft = n.Type
		default:
			return true
		}
		for _, field := range ft.Params.List {
			if isTestingType(field.Type, imp, "T", "B", "F", "TB") {
				for _, name := range field.Names {
					vars[name.Name] = true
				}
			}
		}
		return true
	})
	return vars
}

func testingParam(fd *ast.FuncDecl, imp map[string]string, kind string) string {
	ps := fd.Type.Params.List
	if len(ps) != 1 || len(ps[0].Names) != 1 || !isTestingType(ps[0].Type, imp, kind) {
		return ""
	}
	return ps[0].Names[0].Name
}

func isTestingType(e ast.Expr, imp map[string]string, kinds ...string) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || imp[x.Name] != "testing" {
		return false
	}
	for _, k := range kinds {
		if sel.Sel.Name == k {
			return true
		}
	}
	return false
}

func callsMethodOn(body *ast.BlockStmt, recv, method string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == recv {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

func stringLit(e ast.Expr) string {
	if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
		s, _ := strconv.Unquote(bl.Value)
		return s
	}
	return ""
}

// execMode reports whether e is an integer literal with any execute bit set.
func execMode(e ast.Expr) bool {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.INT {
		return false
	}
	v, err := strconv.ParseInt(bl.Value, 0, 64)
	return err == nil && v&0o111 != 0
}
```

- [ ] **Step 6: Implement `allow.go`**

```go
package testpolicy

import (
	"go/ast"
	"go/token"
	"strings"
)

const allowPrefix = "//testpolicy:allow "

type allowKey struct {
	file string
	line int
	rule string
}

// applyAllows drops violations exempted by a "//testpolicy:allow <rule> — <reason>"
// comment on the same line or the line above. An exemption with no reason
// still suppresses its rule, but adds an allow-reason violation.
func applyAllows(fset *token.FileSet, files []*ast.File, vs []Violation) []Violation {
	allowed := map[allowKey]bool{}
	var out []Violation
	for _, f := range files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if !strings.HasPrefix(c.Text, allowPrefix) {
					continue
				}
				fields := strings.Fields(strings.TrimPrefix(c.Text, allowPrefix))
				if len(fields) == 0 {
					continue
				}
				pos := fset.Position(c.Pos())
				rule := fields[0]
				allowed[allowKey{pos.Filename, pos.Line, rule}] = true
				allowed[allowKey{pos.Filename, pos.Line + 1, rule}] = true
				reason := strings.TrimSpace(strings.TrimLeft(strings.Join(fields[1:], " "), "—-"))
				if reason == "" {
					out = append(out, Violation{Pos: pos, Rule: RuleAllowReason, Msg: "exemption for " + rule + " has no reason"})
				}
			}
		}
	}
	for _, v := range vs {
		if !allowed[allowKey{v.Pos.Filename, v.Pos.Line, v.Rule}] {
			out = append(out, v)
		}
	}
	return out
}
```

- [ ] **Step 7: Run it and confirm it passes**

Run: `go test ./internal/testpolicy/ -run TestScanDirFixtures -v`
Expected: PASS for all 18 subtests. If a count is wrong, fix the fixture or the rule, whichever doesn't match the Global Constraints.

- [ ] **Step 8: Commit**

```bash
git add internal/testpolicy
git commit -m "testpolicy: syntax scanner for the unit-test rules"
```

### Task 2: Repo-wide policy test with the unconverted ratchet

**Files:**
- Create: `internal/testpolicy/list.go`, `internal/testpolicy/contracts.go`, `internal/testpolicy/policy_test.go`, `internal/testpolicy/unconverted.txt`

**Interfaces:**
- Consumes: `ScanDir`, `Violation` (Task 1)
- Produces:
  - `func ReadList(path string) (map[string]bool, error)`: keys are slash-separated directories relative to the repo root, such as `internal/tmux`. Blank lines and `#` comments are ignored.
  - `func PackageDirs(root string) ([]string, error)`: every directory under `root` holding `.go` files, skipping `.git`, `testdata`, `vendor`, and names that start with `.` or `_`.
  - `func CheckContracts(root string, dirs []string) ([]Violation, error)`

- [ ] **Step 1: Write the failing test** `internal/testpolicy/policy_test.go`:

```go
package testpolicy

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var seed = flag.Bool("seed", false, "print the unconverted.txt a fresh checkout needs, instead of failing")

// TestPolicy applies the unit-test rules to every package not listed in
// unconverted.txt, and fails a listed package that already passes, so the
// list can only shrink.
func TestPolicy(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	unconverted, err := ReadList("unconverted.txt")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	var dirty []string
	for _, dir := range dirs {
		rel := filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator)))
		vs, err := ScanDir(dir)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if len(vs) > 0 {
			dirty = append(dirty, rel)
		}
		listed := unconverted[rel]
		delete(unconverted, rel)
		switch {
		case *seed:
		case listed && len(vs) == 0:
			t.Errorf("%s is in unconverted.txt but passes every rule: delete its line", rel)
		case !listed:
			for _, v := range vs {
				t.Error(v.String())
			}
		}
	}
	if *seed {
		sort.Strings(dirty)
		fmt.Println(strings.Join(dirty, "\n"))
		return
	}
	for rel := range unconverted {
		t.Errorf("unconverted.txt lists %s, which is not a Go package directory", rel)
	}
	cvs, err := CheckContracts(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range cvs {
		t.Error(v.String())
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/testpolicy/ -run TestPolicy`
Expected: build failure, `undefined: ReadList`.

- [ ] **Step 3: Implement `list.go`**

```go
package testpolicy

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ReadList reads a list of repo-relative package directories, one per line.
func ReadList(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m[line] = true
	}
	return m, sc.Err()
}

// PackageDirs returns every directory under root that holds .go files.
func PackageDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != root && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
			return filepath.SkipDir
		}
		matches, err := filepath.Glob(filepath.Join(p, "*.go"))
		if err != nil {
			return err
		}
		if len(matches) > 0 {
			dirs = append(dirs, p)
		}
		return nil
	})
	return dirs, err
}
```

- [ ] **Step 4: Implement `contracts.go`**

Two rules:
- Every directory whose base name ends in `fake` must declare an exported top-level `func Run…Contract` in a non-test file.
- Across all `//go:build integration` files, each such contract must be called at least once.
- Every function named `Test…` in an integration file must be named `TestIntegration…`.

```go
package testpolicy

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	RuleContract        = "fake-contract"
	RuleIntegrationName = "integration-name"
)

var contractName = regexp.MustCompile(`^Run[A-Z]\w*Contract$`)

// CheckContracts enforces that every *fake package exports a Run…Contract
// function, that some integration-tagged file calls it, and that integration
// tests are named TestIntegration….
func CheckContracts(root string, dirs []string) ([]Violation, error) {
	fset := token.NewFileSet()
	want := map[string]token.Position{} // "fakepkg.RunXContract" -> declaration
	called := map[string]bool{}
	var vs []Violation
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		isFake := strings.HasSuffix(filepath.Base(dir), "fake")
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return nil, err
			}
			integration := hasIntegrationTag(f)
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil {
					continue
				}
				if isFake && !strings.HasSuffix(name, "_test.go") && contractName.MatchString(fd.Name.Name) {
					want[filepath.Base(dir)+"."+fd.Name.Name] = fset.Position(fd.Pos())
				}
				if integration && strings.HasPrefix(fd.Name.Name, "Test") && !strings.HasPrefix(fd.Name.Name, "TestIntegration") && fd.Name.Name != "TestMain" {
					vs = append(vs, Violation{fset.Position(fd.Pos()), RuleIntegrationName, fd.Name.Name + " is in an integration file; name it TestIntegration…"})
				}
			}
			if integration {
				ast.Inspect(f, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && contractName.MatchString(sel.Sel.Name) {
						if x, ok := sel.X.(*ast.Ident); ok {
							called[x.Name+"."+sel.Sel.Name] = true
						}
					}
					return true
				})
			}
		}
		if isFake {
			found := false
			for k := range want {
				if strings.HasPrefix(k, filepath.Base(dir)+".") {
					found = true
				}
			}
			if !found {
				vs = append(vs, Violation{token.Position{Filename: dir}, RuleContract, "fake package exports no Run…Contract function"})
			}
		}
	}
	for k, pos := range want {
		if !called[k] {
			vs = append(vs, Violation{pos, RuleContract, k + " is never run against the real implementation in an integration test"})
		}
	}
	return vs, nil
}

func hasIntegrationTag(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				expr, err := constraint.Parse(c.Text)
				if err == nil && expr.Eval(func(tag string) bool { return tag == "integration" }) {
					return true
				}
			}
		}
	}
	return false
}
```

Add to `scan_test.go` a fixture test for `CheckContracts`: `testdata/contracts/goodfake/fake.go` declares `func RunThingContract(t *testing.T)`, and `testdata/contracts/real/int_test.go` has `//go:build integration` and calls `goodfake.RunThingContract(t)` inside `func TestIntegrationThing`. `testdata/contracts/badfake/fake.go` declares no contract. Call `CheckContracts` with those three directories and expect exactly one `fake-contract` violation, on `badfake`.

- [ ] **Step 5: Seed `unconverted.txt`**

```bash
printf '# Packages not yet converted to the unit-test rules (docs/testing.md).\n# This list only shrinks: converting a package deletes its line.\n' > internal/testpolicy/unconverted.txt
go test ./internal/testpolicy/ -run TestPolicy -count=1 -args -seed | grep -v '^ok\|^PASS' >> internal/testpolicy/unconverted.txt
```

Check that the file contains `internal/tmux` and `internal/testpolicy/...` does **not** appear. `testpolicy` itself must pass its own rules. Run `wc -l` and record the count in the commit message.

- [ ] **Step 6: Run it and confirm it passes**

Run: `go test ./internal/testpolicy/ -count=1 -v`
Expected: PASS. `TestPolicy` should finish in under 5 s (the elapsed time is printed).

- [ ] **Step 7: Commit**

```bash
git add internal/testpolicy
git commit -m "testpolicy: repo-wide policy test with an only-shrinking unconverted list

Seeded with N packages (every package that breaks a rule today)."
```

### Task 3: The budget runner, Makefile targets, timing script, clockwork

**Files:**
- Create: `internal/testpolicy/budget.go`, `internal/testpolicy/budget_test.go`, `internal/testpolicy/cmd/budget/main.go`, `scripts/test-timing.sh`
- Modify: `Makefile` (the `test` target, plus new `test-integration` and `test-timing`), `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `ReadList` (Task 2)
- Produces:
  - `type Overrun struct { Package string; Elapsed time.Duration; Slowest []TestTime }`
  - `type TestTime struct { Name string; Elapsed time.Duration }`
  - `func WatchBudget(r io.Reader, w io.Writer, budget time.Duration, exempt map[string]bool, module string) ([]Overrun, error)`
  - the Makefile targets `test`, `test-integration`, `test-timing`

- [ ] **Step 1: Write the failing test** `internal/testpolicy/budget_test.go`:

```go
package testpolicy

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const stream = `{"Action":"output","Package":"m/internal/a","Output":"ok\n"}
{"Action":"pass","Package":"m/internal/a","Test":"TestSlow","Elapsed":9.5}
{"Action":"pass","Package":"m/internal/a","Test":"TestFast","Elapsed":0.1}
{"Action":"pass","Package":"m/internal/a","Elapsed":12.0}
{"Action":"pass","Package":"m/internal/b","Elapsed":30.0}
{"Action":"pass","Package":"m/internal/c","Elapsed":1.0}
`

func TestWatchBudget(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	over, err := WatchBudget(strings.NewReader(stream), &out, 10*time.Second, map[string]bool{"internal/b": true}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 1 || over[0].Package != "internal/a" || over[0].Elapsed != 12*time.Second {
		t.Fatalf("overruns = %+v, want internal/a at 12s only (b is exempt, c is under)", over)
	}
	if len(over[0].Slowest) == 0 || over[0].Slowest[0].Name != "TestSlow" {
		t.Fatalf("slowest = %+v, want TestSlow first", over[0].Slowest)
	}
	if out.String() != "ok\n" {
		t.Fatalf("output = %q, want the Output fields passed through", out.String())
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/testpolicy/ -run TestWatchBudget`
Expected: `undefined: WatchBudget`.

- [ ] **Step 3: Implement `budget.go`**

```go
package testpolicy

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

type TestTime struct {
	Name    string
	Elapsed time.Duration
}

type Overrun struct {
	Package string
	Elapsed time.Duration
	Slowest []TestTime
}

type testEvent struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

// WatchBudget copies the human-readable output of a `go test -json` stream to w
// and returns every package, outside exempt, whose test binary ran longer than
// budget. Package names are reported relative to module.
func WatchBudget(r io.Reader, w io.Writer, budget time.Duration, exempt map[string]bool, module string) ([]Overrun, error) {
	tests := map[string][]TestTime{}
	var over []Overrun
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev testEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			// go test prints build failures as plain text; pass them through.
			io.WriteString(w, sc.Text()+"\n")
			continue
		}
		if ev.Action == "output" {
			io.WriteString(w, ev.Output)
			continue
		}
		if ev.Action != "pass" && ev.Action != "fail" {
			continue
		}
		d := time.Duration(ev.Elapsed * float64(time.Second))
		pkg := strings.TrimPrefix(strings.TrimPrefix(ev.Package, module), "/")
		if ev.Test != "" {
			if !strings.Contains(ev.Test, "/") {
				tests[pkg] = append(tests[pkg], TestTime{ev.Test, d})
			}
			continue
		}
		if d > budget && !exempt[pkg] {
			ts := tests[pkg]
			sort.Slice(ts, func(i, j int) bool { return ts[i].Elapsed > ts[j].Elapsed })
			if len(ts) > 3 {
				ts = ts[:3]
			}
			over = append(over, Overrun{pkg, d, ts})
		}
	}
	return over, sc.Err()
}
```

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test ./internal/testpolicy/ -run TestWatchBudget -v`
Expected: PASS.

- [ ] **Step 5: Implement `cmd/budget/main.go`.** It runs `go test` itself instead of reading a pipe, so `go test`'s exit code can't be lost in a shell pipeline.

```go
// Command budget runs `go test -json <args>` and fails when a converted package
// (one not listed in unconverted.txt) runs longer than the budget.
//
//	go run ./internal/testpolicy/cmd/budget -- -timeout 20m ./...
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

func main() {
	budget := flag.Duration("budget", 10*time.Second, "per-package test time limit for converted packages")
	list := flag.String("unconverted", "internal/testpolicy/unconverted.txt", "packages exempt from the budget")
	flag.Parse()

	exempt, err := testpolicy.ReadList(*list)
	if err != nil {
		fmt.Fprintln(os.Stderr, "budget:", err)
		os.Exit(2)
	}
	cmd := exec.Command("go", append([]string{"test", "-json"}, flag.Args()...)...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "budget:", err)
		os.Exit(2)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "budget:", err)
		os.Exit(2)
	}
	over, scanErr := testpolicy.WatchBudget(stdout, os.Stdout, *budget, exempt, "github.com/steveyegge/gastown")
	waitErr := cmd.Wait()

	for _, o := range over {
		fmt.Fprintf(os.Stderr, "BUDGET: %s took %s (limit %s); slowest:", o.Package, o.Elapsed.Round(time.Millisecond), *budget)
		for _, t := range o.Slowest {
			fmt.Fprintf(os.Stderr, " %s %s", t.Name, t.Elapsed.Round(time.Millisecond))
		}
		fmt.Fprintln(os.Stderr)
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(waitErr, &exitErr):
		os.Exit(exitErr.ExitCode())
	case waitErr != nil || scanErr != nil:
		fmt.Fprintln(os.Stderr, "budget:", waitErr, scanErr)
		os.Exit(2)
	case len(over) > 0:
		os.Exit(1)
	}
}
```

- [ ] **Step 6: Wire the Makefile.** Replace only the final command line of `test:` (keep its comment block and its dependency on `test-makefile`), and add the two new targets after `test-changed`:

```make
	GT_TEST_DOCKER=$${GT_TEST_DOCKER:-1} go run ./internal/testpolicy/cmd/budget -- -timeout 20m ./...
```

```make
# test-integration runs only the //go:build integration tier (real tmux, bd,
# Dolt, gt binary; tests named TestIntegration*). main_branch_test runs it
# after merge. Docker-backed suites still need `gt slot run` around the call.
test-integration:
	GT_TEST_DOCKER=$${GT_TEST_DOCKER:-1} go test -tags integration -run '^TestIntegration' -timeout 20m ./...

# test-timing measures the unit tier in a tmux server started by launchd, which
# macOS does not exempt from its first-run scan of new executables. It is the
# acceptance measurement for docs/plans/2026-09-27-test-rewrite-design.md.
# PKGS narrows the run, e.g. make test-timing PKGS=./internal/tmux/...
test-timing:
	bash scripts/test-timing.sh $(PKGS)
```

Add `test-integration test-timing` to the `.PHONY` line if the Makefile has one.

- [ ] **Step 7: Write `scripts/test-timing.sh`**

```bash
#!/usr/bin/env bash
# test-timing.sh — time the unit tier where macOS still scans new executables.
#
# A tmux server that launchd starts is not covered by a Developer Tools
# exemption, which is exactly the town's condition after a reboot. We start
# one via `launchctl submit`, prove its pane is taxed with a 20-script probe,
# then run the unit tier in it and report wall time against TARGET_SECONDS.
set -euo pipefail
[[ "$(uname)" == Darwin ]] || { echo "test-timing: macOS only" >&2; exit 2; }
pkgs=("${@:-./...}")
target=${TARGET_SECONDS:-90}
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
sock="gt-test-timing-$$"
label="com.gastown.test-timing.$$"
tmux_bin=$(command -v tmux)

cleanup() {
  "$tmux_bin" -L "$sock" kill-server 2>/dev/null || true
  launchctl remove "$label" 2>/dev/null || true
}
trap cleanup EXIT

# launchd owns the new-session process; KeepAlive would respawn it, so remove
# the job as soon as the server is up (the server itself has daemonized).
launchctl submit -l "$label" -- "$tmux_bin" -L "$sock" new-session -d -s timing
for _ in $(seq 50); do
  "$tmux_bin" -L "$sock" has-session -t timing 2>/dev/null && break
  sleep 0.2
done
launchctl remove "$label" 2>/dev/null || true
"$tmux_bin" -L "$sock" has-session -t timing

cat > "$work/run.sh" <<EOF
set -u
cd '$repo'
d=\$(mktemp -d)
for i in \$(seq 20); do printf '#!/bin/sh\n:\n' > "\$d/s\$i"; chmod +x "\$d/s\$i"; done
s=\$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
for i in \$(seq 20); do "\$d/s\$i"; done
e=\$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
perl -e "printf \"%d\n\", (\$e-\$s)*1000/20" > '$work/probe'
start=\$(date +%s)
GT_TEST_DOCKER=0 GOFLAGS=-p=8 go run ./internal/testpolicy/cmd/budget -- -count=1 ${pkgs[*]} > '$work/log' 2>&1
echo "\$? \$(( \$(date +%s) - start ))" > '$work/done'
EOF
"$tmux_bin" -L "$sock" send-keys -t timing "bash '$work/run.sh'" Enter

for _ in $(seq 900); do [[ -s "$work/done" ]] && break; sleep 2; done
[[ -s "$work/done" ]] || { echo "test-timing: no result after 30 min; log: $work/log" >&2; exit 2; }

probe=$(cat "$work/probe")
read -r rc secs < "$work/done"
echo "probe: ${probe} ms per new executable (taxed pane expected >= 50)"
echo "unit tier: ${secs}s (target ${target}s), go test exit ${rc}; log: $work/log"
if (( probe < 50 )); then
  echo "test-timing: pane is EXEMPT (probe ${probe} ms); this measurement does not prove the target" >&2
  exit 3
fi
(( rc == 0 )) || exit "$rc"
(( secs <= target )) || { echo "test-timing: over target" >&2; exit 1; }
```

`chmod +x scripts/test-timing.sh`.

- [ ] **Step 8: Add the clock dependency**

Run: `go get github.com/jonboulle/clockwork@latest && go mod tidy`
Expected: `go.mod` gains `github.com/jonboulle/clockwork` as a direct requirement once Task 4 imports it. For now `go mod tidy` may mark it `// indirect` or drop it. If tidy drops it, skip this step here and do it in Task 4.

- [ ] **Step 9: Verify the whole gate**

Run each of these and check its exit code:
- `make build`
- `make lint`
- `GOFLAGS=-p=8 make test`

Expected: all exit 0. `make test` output ends like before, with no `BUDGET:` lines, because every slow package is in `unconverted.txt`.

Then run `make test-timing PKGS=./internal/testpolicy/...` once.
Expected: `probe:` of 50 ms or more, meaning the pane is taxed, and a unit tier time of 10 s or less. If the probe reads under 50 ms, stop and report it: the launchd approach didn't give a taxed pane on this machine, and the script needs a different way to start the server. Don't paper over it.

- [ ] **Step 10: Commit, push, submit MR 1**

```bash
git add Makefile go.mod go.sum internal/testpolicy scripts/test-timing.sh
git commit -m "test: budget runner, test-integration and test-timing targets"
git log origin/main..HEAD --format=%B | grep -ci 'co-authored\|claude'   # must print 0
git push origin crew/sloan/test-policy-scaffold
gt mq submit
```

Watch until it lands. Verify with `git fetch origin && git log origin/main --oneline | grep testpolicy`, not by the MR bead's status.

---

## MR 2 — tmux seams (no behaviour change)

Branch: `crew/sloan/tmux-seams` off fresh `origin/main`, after MR 1 has landed.

### Task 4: The exec seam

**Files:**
- Create: `internal/tmux/exec.go`, `internal/tmux/exec_test.go`, `internal/tmux/helpers_test.go`
- Modify: `internal/tmux/tmux.go`: the `Tmux` struct at `:188`, `commandContext`/`runContext` at `:282-305`, `kill`/`ps` calls at `:736-750, 829-845, 911, 1030-1045, 1088-1103, 1174, 2912, 2968, 3257`
- Modify: `internal/tmux/process_group_unix.go:14-44`, `internal/tmux/socket_guard_unix.go:119-140`
- Keep compiling: `internal/tmux/process_group_windows.go`

**Interfaces:**
- Produces:
  - `type execFunc func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)`
  - `func realExec(ctx context.Context, name string, args ...string) ([]byte, []byte, error)`
  - `func (t *Tmux) runner() execFunc`: returns `t.exec`, or `realExec` when it's nil (tests build `&Tmux{}` directly)
  - `func newTmuxForTest(socket string, ex execFunc, clk clockwork.Clock) *Tmux`, in the **test** file `internal/tmux/helpers_test.go`. It's used only by package `tmux`'s own unit tests, and production packages carry no test-only code.

- [ ] **Step 1: Write the failing test** `internal/tmux/exec_test.go`:

```go
package tmux

import (
	"context"
	"reflect"
	"testing"
)

type call struct {
	name string
	args []string
}

// recorder returns an execFunc that records calls and answers from out.
func recorder(out map[string]string) (execFunc, *[]call) {
	var calls []call
	return func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		calls = append(calls, call{name, args})
		return []byte(out[name]), nil, nil
	}, &calls
}

func TestHasSessionSendsHasSession(t *testing.T) {
	t.Parallel()
	ex, calls := recorder(nil)
	tm := newTmuxForTest("gt-test-x", ex, nil)
	ok, err := tm.HasSession("alpha")
	if err != nil || !ok {
		t.Fatalf("HasSession = %v, %v; want true, nil", ok, err)
	}
	want := call{"tmux", []string{"-u", "-L", "gt-test-x", "has-session", "-t", "=alpha"}}
	if len(*calls) == 0 || !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("calls = %+v, want first %+v", *calls, want)
	}
}
```

Before writing the expected arguments, read `HasSession` in `tmux.go` and copy the exact arguments it builds (for example whether it uses `-t =name` or `-t name`). The test pins today's behaviour; it doesn't invent new behaviour.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/tmux/ -run TestHasSessionSendsHasSession`
Expected: `undefined: newTmuxForTest`.

- [ ] **Step 3: Implement `exec.go`**

```go
package tmux

import (
	"bytes"
	"context"
	"os/exec"
	"time"

	"github.com/jonboulle/clockwork"
)

// execFunc runs a program and returns its stdout and stderr. Tmux sends every
// tmux, ps and kill invocation through one, so tests can record and answer
// them without starting processes.
type execFunc func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

func realExec(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	hideConsoleWindow(cmd)
	if _, ok := ctx.Deadline(); ok {
		cmd.WaitDelay = 100 * time.Millisecond
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (t *Tmux) runner() execFunc {
	if t.exec == nil {
		return realExec
	}
	return t.exec
}

func (t *Tmux) clk() clockwork.Clock {
	if t.clock == nil {
		return clockwork.NewRealClock()
	}
	return t.clock
}
```

`internal/tmux/helpers_test.go`:

```go
package tmux

import "github.com/jonboulle/clockwork"

// newTmuxForTest builds a Tmux whose processes and clock are supplied by the
// test. A nil ex or clk falls back to the real one.
func newTmuxForTest(socket string, ex execFunc, clk clockwork.Clock) *Tmux {
	return &Tmux{socketName: socket, exec: ex, clock: clk}
}
```

Add two fields to the struct at `tmux.go:188`:
```go
type Tmux struct {
	socketName string          // tmux socket name (-L flag), empty = default socket
	exec       execFunc        // nil = realExec; see exec.go
	clock      clockwork.Clock // nil = real clock; see exec.go
}
```

- [ ] **Step 4: Route `runContext` through the runner.** Replace `runContext`'s body so it builds the same arguments as `commandContext` and calls `t.runner()(ctx, "tmux", allArgs...)`. Keep `wrapError` as it is:

```go
func (t *Tmux) tmuxArgs(args []string) []string {
	allArgs := []string{"-u"}
	if t.socketName != "" {
		allArgs = append(allArgs, "-L", t.socketName)
	}
	return append(allArgs, args...)
}

func (t *Tmux) runContext(ctx context.Context, args ...string) (string, error) {
	stdout, stderr, err := t.runner()(ctx, "tmux", t.tmuxArgs(args)...)
	if err != nil {
		return "", t.wrapError(err, string(stderr), args)
	}
	return strings.TrimSpace(string(stdout)), nil
}
```

`commandContext` stays only where a caller needs an `*exec.Cmd` object, for example to attach a pty or read stdout incrementally. Search for its callers (`grep -n 'commandContext(' internal/tmux/*.go`). Each caller that just runs the command and reads its output switches to `t.runner()`. Keep any that really need the `*exec.Cmd`, and add `//testpolicy:allow` only if a test file is involved; production code is not policed for exec.

- [ ] **Step 5: Route every `kill` and `ps` through the runner.**
  - Each `exec.Command("kill", "-TERM", pid).Run()` inside a `Tmux` method becomes `_, _, _ = t.runner()(context.Background(), "kill", "-TERM", pid)`.
  - Each `exec.Command("ps", …).Output()` becomes `out, _, err := t.runner()(context.Background(), "ps", …)`.
  - The package-level helpers in `process_group_unix.go` (`:21`, `:31`, `:44`) take `ex execFunc` as their first parameter; update their callers to pass `t.runner()`.
  - Give the Windows helpers in `process_group_windows.go` the same signature change so `GOOS=windows go vet ./internal/tmux/` still passes.
  - `tmux -V` at `:1174` goes through the runner too.

- [ ] **Step 6: Run the new test and the old suite**

Run: `go test ./internal/tmux/ -run TestHasSessionSendsHasSession -v`
Expected: PASS.

Run: `GOFLAGS=-p=8 go test -count=1 ./internal/tmux/ ./internal/polecat/ ./internal/daemon/ ./internal/cmd/`
Expected: PASS, with the same test count as on `origin/main`. Existing tests still use real tmux, which proves the default path behaves the same.

Run: `GOOS=windows go vet ./internal/tmux/`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
git add internal/tmux go.mod go.sum
git commit -m "tmux: route tmux, ps and kill through an injectable exec runner"
```

### Task 5: The clock seam

**Files:**
- Modify: `internal/tmux/tmux.go` (70 time calls), `submit_verify.go:266,310,319`, `composer_stall.go:306,341,349,627,637`, `flock_unix.go:16-41`, `socket_guard_unix.go:90-103`, `process_group_unix.go:14`
- Keep compiling: `flock_windows.go`
- Test: `internal/tmux/flock_unix_test.go` (create it, or add to it if it exists)

**Interfaces:**
- Consumes: `Tmux.clk()` (Task 4)
- Produces: `func acquireFlockLock(clk clockwork.Clock, lockPath string, timeout time.Duration) (func(), error)`, with the clock as a new first parameter

- [ ] **Step 1: Write the failing test.** A fake clock must drive the flock timeout without any real waiting:

```go
package tmux

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

func TestAcquireFlockLockTimesOutOnFakeClock(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "n.lock")
	release, err := acquireFlockLock(clockwork.NewRealClock(), lock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	clk := clockwork.NewFakeClock()
	done := make(chan error, 1)
	go func() {
		_, err := acquireFlockLock(clk, lock, 5*time.Second)
		done <- err
	}()
	err = driveClock(t, clk, 100*time.Millisecond, done)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want timeout", err)
	}
}
```

Add `driveClock` to `helpers_test.go`. It's the one correct way to move a fake clock while waiting for a result: it never blocks after the goroutine under test has finished.

```go
// driveClock advances clk by step every time a goroutine blocks on it, until
// done delivers a value. It fails the test if nothing blocks within 10 s.
func driveClock[T any](t *testing.T, clk *clockwork.FakeClock, step time.Duration, done <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		blocked := make(chan error, 1)
		go func() { blocked <- clk.BlockUntilContext(ctx, 1) }()
		select {
		case v := <-done:
			return v
		case err := <-blocked:
			if err != nil {
				t.Fatalf("nothing blocked on the fake clock: %v", err)
			}
			clk.Advance(step)
		}
	}
}
```

The same process holds the lock through a different file descriptor. `flock` locks belong to the open file description, so the second open conflicts, which is what the test needs.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/tmux/ -run TestAcquireFlockLockTimesOutOnFakeClock`
Expected: a compile error, because `acquireFlockLock` takes 2 arguments.

- [ ] **Step 3: Convert `acquireFlockLock`.** Change `time.Now()` to `clk.Now()` and `time.Sleep(100 * time.Millisecond)` to `clk.Sleep(100 * time.Millisecond)`. Update the caller at `tmux.go:2080` to pass `t.clk()`. Make the same signature change in `flock_windows.go`.

- [ ] **Step 4: Convert every other time call in package `tmux`**, using these rules:
  - Inside a `Tmux` method: `time.Sleep(d)` → `t.clk().Sleep(d)`; `time.Now()` → `t.clk().Now()`; `time.Since(x)` → `t.clk().Since(x)`; `time.After(d)` → `t.clk().After(d)`; `time.NewTimer(d)` → `t.clk().NewTimer(d)` (use `.Chan()` in place of `.C`); `time.NewTicker(d)` → `t.clk().NewTicker(d)` (again `.Chan()`).
  - In a package-level function: add a `clk clockwork.Clock` parameter and pass `t.clk()` from the callers.
  - `composer_stall.go:341` passes `time.Now` as a function value; pass `t.clk().Now` instead.
  - `time.Duration` constants and arithmetic stay as they are. Only calls that read or wait on the clock change.

When it's done, `grep -nE 'time\.(Sleep|After|NewTimer|NewTicker|Tick|Now|Since|Until)\(' internal/tmux/*.go | grep -v _test | grep -v _windows` must print nothing.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/tmux/ -run TestAcquireFlockLockTimesOutOnFakeClock -v`
Expected: PASS in well under 1 s.

Run: `GOFLAGS=-p=8 go test -count=1 ./internal/tmux/ ./internal/polecat/ ./internal/daemon/ ./internal/cmd/`
Expected: PASS, with the same test count. The existing tests use the real clock through the nil default.

Run: `go test ./internal/testpolicy/`
Expected: PASS. `tmux` is still listed as unconverted, and `prod-no-sleep` now applies to `tmux` because it imports clockwork; step 4 left no `time.Sleep`, so nothing is reported.

- [ ] **Step 6: Gate, commit, push, submit MR 2**

```bash
make build && make lint && GOFLAGS=-p=8 make test; echo "exit=$?"   # decide on exit=0 only
git add internal/tmux
git commit -m "tmux: inject a clockwork clock for every sleep, timer and time read"
git log origin/main..HEAD --format=%B | grep -ci 'co-authored\|claude'   # must print 0
git push origin crew/sloan/tmux-seams
gt mq submit
```

Verify it landed with `git fetch origin && git log origin/main --oneline -5`.

---

## MR 3 — tmux rewrite

Branch: `crew/sloan/tmux-rewrite` off fresh `origin/main`, after MR 2 has landed.

### Task 6: `tmuxfake` and its contract

**Files:**
- Create: `internal/tmux/tmuxfake/contract.go`, `internal/tmux/tmuxfake/fake.go`, `internal/tmux/tmuxfake/fake_test.go`, `internal/tmux/contract_integration_test.go`

**Interfaces:**
- Consumes: `tmux.ErrSessionExists`, `tmux.ErrSessionNotFound`, `tmux.NewTmuxWithSocket`, `constants.TestSocketName`, `(*tmux.Tmux).KillServer`
- Produces:
  - `type Sessions interface { NewSession(name, workDir string) error; NewSessionWithCommandAndEnv(name, workDir, command string, env map[string]string) error; HasSession(name string) (bool, error); ListSessions() ([]string, error); KillSession(name string) error; KillSessionWithProcesses(name string) error; SetEnvironment(session, key, value string) error; GetEnvironment(session, key string) (string, error); GetPaneCommand(session string) (string, error) }`
  - `func RunSessionsContract(t *testing.T, newImpl func(t *testing.T) Sessions)`
  - `func New(clk clockwork.Clock) *Server`, plus every `*tmux.Tmux` method listed in the design §1 table, with **identical signatures**
  - scripting helpers on `*Server`: `SetScreen(session string, lines ...string)`, `SetPaneCommand(session, cmd string)`, `SetIdle(session string, idle bool)`, `Exit(session string)`, `Sent(session string) []string`, `Env(session string) map[string]string`

- [ ] **Step 1: Write the contract** `tmuxfake/contract.go`. It's a non-test file so both tiers can import it:

```go
// Package tmuxfake is an in-memory tmux server for unit tests. Its behaviour
// is pinned to real tmux by RunSessionsContract, which runs against the fake in
// the unit tier and against a real tmux server in the integration tier.
package tmuxfake

import (
	"errors"
	"sort"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

// Sessions is the session-lifecycle surface that both the fake and *tmux.Tmux provide.
type Sessions interface {
	NewSession(name, workDir string) error
	NewSessionWithCommandAndEnv(name, workDir, command string, env map[string]string) error
	HasSession(name string) (bool, error)
	ListSessions() ([]string, error)
	KillSession(name string) error
	KillSessionWithProcesses(name string) error
	SetEnvironment(session, key, value string) error
	GetEnvironment(session, key string) (string, error)
	GetPaneCommand(session string) (string, error)
}

// RunSessionsContract checks the behaviour every Sessions implementation must share.
func RunSessionsContract(t *testing.T, newImpl func(t *testing.T) Sessions) {
	t.Run("new/has/list/kill", func(t *testing.T) {
		s := newImpl(t)
		dir := t.TempDir()
		if err := s.NewSession("gt-c-a", dir); err != nil {
			t.Fatal(err)
		}
		if err := s.NewSession("gt-c-b", dir); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.HasSession("gt-c-a"); err != nil || !ok {
			t.Fatalf("HasSession(a) = %v, %v", ok, err)
		}
		names, err := s.ListSessions()
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		if len(names) != 2 || names[0] != "gt-c-a" || names[1] != "gt-c-b" {
			t.Fatalf("ListSessions = %v", names)
		}
		if err := s.KillSession("gt-c-a"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.HasSession("gt-c-a"); ok {
			t.Fatal("session a survived KillSession")
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		s := newImpl(t)
		dir := t.TempDir()
		if err := s.NewSession("gt-c-dup", dir); err != nil {
			t.Fatal(err)
		}
		if err := s.NewSession("gt-c-dup", dir); !errors.Is(err, tmux.ErrSessionExists) {
			t.Fatalf("second NewSession = %v, want ErrSessionExists", err)
		}
	})
	t.Run("has on missing", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSession("gt-c-other", t.TempDir()); err != nil { // ensure a server exists
			t.Fatal(err)
		}
		if ok, err := s.HasSession("gt-c-missing"); err != nil || ok {
			t.Fatalf("HasSession(missing) = %v, %v; want false, nil", ok, err)
		}
	})
	t.Run("environment round trip", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSessionWithCommandAndEnv("gt-c-env", t.TempDir(), "sleep 300", map[string]string{"GT_ROLE": "polecat"}); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetEnvironment("gt-c-env", "GT_ROLE"); err != nil || v != "polecat" {
			t.Fatalf("GetEnvironment(GT_ROLE) = %q, %v", v, err)
		}
		if err := s.SetEnvironment("gt-c-env", "GT_X", "1"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetEnvironment("gt-c-env", "GT_X"); err != nil || v != "1" {
			t.Fatalf("GetEnvironment(GT_X) = %q, %v", v, err)
		}
	})
	t.Run("pane command", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSessionWithCommandAndEnv("gt-c-cmd", t.TempDir(), "sleep 300", nil); err != nil {
			t.Fatal(err)
		}
		if c, err := s.GetPaneCommand("gt-c-cmd"); err != nil || c != "sleep" {
			t.Fatalf("GetPaneCommand = %q, %v; want sleep", c, err)
		}
	})
}
```

- [ ] **Step 2: Write the integration runner first, and pin the semantics against real tmux.** `internal/tmux/contract_integration_test.go`:

```go
//go:build integration

package tmux_test

import (
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
)

func TestIntegrationSessionsContract(t *testing.T) {
	tmuxfake.RunSessionsContract(t, func(t *testing.T) tmuxfake.Sessions {
		tm := tmux.NewTmuxWithSocket(constants.TestSocketName("gt-test-contract"))
		t.Cleanup(func() { _ = tm.KillServer() })
		return tm
	})
}
```

Run: `go test -tags integration -run TestIntegrationSessionsContract -count=1 -v ./internal/tmux/`
Expected: PASS. **If any contract case fails against real tmux, the contract is wrong, not tmux.** Correct the assertion to match real behaviour (for example, if a killed-then-queried session returns an error rather than `false, nil`), then carry on. The fake must copy whatever real tmux does.

If `constants.TestSocketName` returns the same name for the same prefix every time, each subtest's `newImpl` must produce a unique socket. Append `t.Name()` or a counter.

- [ ] **Step 3: Write the failing unit runner** `tmuxfake/fake_test.go`:

```go
package tmuxfake

import (
	"testing"

	"github.com/jonboulle/clockwork"
)

func TestFakeSessionsContract(t *testing.T) {
	t.Parallel()
	RunSessionsContract(t, func(t *testing.T) Sessions { return New(clockwork.NewFakeClock()) })
}
```

Run: `go test ./internal/tmux/tmuxfake/`
Expected: `undefined: New`.

- [ ] **Step 4: Implement `fake.go`.** State model and core methods:

```go
package tmuxfake

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tmux"
)

type session struct {
	workDir string
	command string
	env     map[string]string
	paneCmd string
	screen  []string
	sent    []string
	idle    bool
}

// Server is an in-memory tmux server. Methods mirror *tmux.Tmux signatures.
type Server struct {
	mu       sync.Mutex
	clock    clockwork.Clock
	sessions map[string]*session
	changed  chan struct{} // closed and replaced on every mutation; wakes WaitFor*
}

func New(clk clockwork.Clock) *Server {
	return &Server{clock: clk, sessions: map[string]*session{}, changed: make(chan struct{})}
}

// mutate runs f under the lock and wakes any waiter.
func (s *Server) mutate(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Server) NewSession(name, workDir string) error {
	return s.NewSessionWithCommandAndEnv(name, workDir, "", nil)
}

func (s *Server) NewSessionWithCommandAndEnv(name, workDir, command string, env map[string]string) error {
	var err error
	s.mutate(func() {
		if _, ok := s.sessions[name]; ok {
			err = tmux.ErrSessionExists
			return
		}
		pc := "zsh"
		if f := strings.Fields(command); len(f) > 0 {
			pc = filepath.Base(f[0])
		}
		e := map[string]string{}
		for k, v := range env {
			e[k] = v
		}
		s.sessions[name] = &session{workDir: workDir, command: command, env: e, paneCmd: pc}
	})
	return err
}

func (s *Server) HasSession(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[name]
	return ok, nil
}

func (s *Server) ListSessions() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.sessions))
	for n := range s.sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func (s *Server) KillSession(name string) error {
	var err error
	s.mutate(func() {
		if _, ok := s.sessions[name]; !ok {
			err = tmux.ErrSessionNotFound
			return
		}
		delete(s.sessions, name)
	})
	return err
}

func (s *Server) KillSessionWithProcesses(name string) error { return s.KillSession(name) }

func (s *Server) SetEnvironment(session, key, value string) error {
	var err error
	s.mutate(func() {
		ss, ok := s.sessions[session]
		if !ok {
			err = tmux.ErrSessionNotFound
			return
		}
		ss.env[key] = value
	})
	return err
}

func (s *Server) GetEnvironment(session, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[session]
	if !ok {
		return "", tmux.ErrSessionNotFound
	}
	v, ok := ss.env[key]
	if !ok {
		return "", fmt.Errorf("tmux show-environment: unknown variable: %s", key)
	}
	return v, nil
}

func (s *Server) GetPaneCommand(session string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[session]
	if !ok {
		return "", tmux.ErrSessionNotFound
	}
	return ss.paneCmd, nil
}

// waitFor blocks until cond holds (checked under the lock) or timeout passes on
// the injected clock.
func (s *Server) waitFor(timeout time.Duration, cond func() bool) error {
	deadline := s.clock.After(timeout)
	for {
		s.mu.Lock()
		ok, ch := cond(), s.changed
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-deadline:
			return fmt.Errorf("timeout after %s", timeout)
		}
	}
}
```

Implement the remaining consumer methods on the same model, with signatures copied **exactly** from `internal/tmux`:
- `CapturePane(session string, lines int) (string, error)`: the last `lines` entries of `screen`, joined with `"\n"`.
- `CapturePaneLines(session string, lines int) ([]string, error)`: the same, returned as a slice.
- `SendKeys`, `SendKeysRaw(session, keys string) error`, `SendKeysDebounced(session, keys string, debounceMs int) error`, `NudgeSession(session, message string) error`: append to `sent`, and return `ErrSessionNotFound` for an unknown session.
- `RespawnPane(pane, command string) error`: set `command`, and set `paneCmd` from the command's first word.
- `IsIdle(session string) bool`: returns `idle`.
- `WaitForIdle(session string, timeout time.Duration) error`: `waitFor(timeout, func() bool { return s.sessions[session] != nil && s.sessions[session].idle })`.
- `IsAgentRunning(session string, expectedPaneCommands ...string) bool`: false if the session is missing. With no expected commands, true if `paneCmd` isn't a shell (`bash`, `zsh`, `sh`, `fish`). Otherwise true if `paneCmd` is one of the expected commands.
- `WaitForCommand(session string, excludeCommands []string, timeout time.Duration) error`: waits until `paneCmd` isn't in `excludeCommands`.
- `WaitForSessionExit(session string, timeout time.Duration) error`: waits until the session is gone. Read the real signature before copying it.
- `WaitForRuntimeReady(session string, rc *config.RuntimeConfig, timeout time.Duration) error`: waits until the session's `env[tmux.EnvAgentReady]` is non-empty, because that's the variable the real implementation checks through the SessionStart hook.

Scripting helpers, each going through `mutate`:
- `SetScreen(session string, lines ...string)`
- `SetPaneCommand(session, cmd string)`
- `SetIdle(session string, idle bool)`
- `Exit(session string)`, which deletes the session
- `Sent(session string) []string`, which returns a copy
- `Env(session string) map[string]string`, which returns a copy

Add a compile-time check in `fake.go`: `var _ Sessions = (*Server)(nil)`. Also add a test-only assertion that `*tmux.Tmux` satisfies `Sessions`. The integration test's return statement already checks that at compile time.

- [ ] **Step 5: Run both tiers**

Run: `go test ./internal/tmux/tmuxfake/ -count=1 -v`
Expected: PASS in under 1 s.

Run: `go test -tags integration -run TestIntegrationSessionsContract -count=1 ./internal/tmux/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/tmux/tmuxfake internal/tmux/contract_integration_test.go
git commit -m "tmuxfake: in-memory tmux server pinned to real tmux by a shared contract"
```

### Task 7: Rewrite the tmux tests, bucket by bucket

**Files:**
- Modify or delete: the 22 `internal/tmux/*_test.go` files
- Create: `internal/tmux/*_integration_test.go`, for the tests kept in the real-tmux bucket

**Interfaces:**
- Consumes: `newTmuxForTest`, `recorder` (Task 4), `clockwork.NewFakeClock`, `tmuxfake` (Task 6)

- [ ] **Step 1: Record the baseline**

```bash
go test -count=1 -cover ./internal/tmux/ | tee /tmp/tmux-cover-before.txt
go test -count=1 -json ./internal/tmux/ > /tmp/tmux-before.json
grep -c '^func Test' internal/tmux/*_test.go | awk -F: '{s+=$2} END {print s}'   # expect 258
```

- [ ] **Step 2: Triage every test into one bucket.** Write the result to `/tmp/tmux-triage.tsv` with the columns `file`, `test`, `bucket`, `reason`. The buckets:
  - **T (translation):** the test checks what a method sends to tmux. Rewrite it as a unit test with `recorder`, asserting the `call` values. Canned stdout goes through the `out` map. For methods that call `tmux` several times, extend `recorder` to answer by the first tmux subcommand, `args[3]` after `-u -L sock`.
  - **L (logic):** the test checks parsing, idle or composer detection, dialog acceptance, respawn decisions, or submit verification over captured pane text or timings. Rewrite it as a unit test with canned runner output and `clockwork.NewFakeClock()`. Move time only with `driveClock` (from `helpers_test.go`), as in `TestAcquireFlockLockTimesOutOnFakeClock`.
  - **R (real tmux):** the test checks behaviour only real tmux has: the visible-tail-versus-history split, zombie or respawn hooks, socket ownership and kill-server, the process tree. Move it to an `_integration_test.go` file with `//go:build integration` and rename it `TestIntegration<OldName>`. If it's really about session lifecycle, add the case to `RunSessionsContract` instead.
  - **D (delete):** the test re-enacts one incident whose behaviour a T, L or R test already covers, or it pins an implementation detail that no caller relies on. Delete it and record why.

Pure functions already in the package, such as `analyzeComposerState`, `consumptionVerdict` and `stripAnsiTrackDim`, are bucket L with no seams needed. Those tests usually only need `t.Parallel()`.

- [ ] **Step 3: Convert one file at a time, smallest first.** After each file, run this and check the exit code:

```bash
go test -count=1 ./internal/tmux/ && go test ./internal/testpolicy/ -run TestPolicy -count=1 2>&1 | grep 'internal/tmux' | grep -v unconverted
```

The second command lists the remaining violations in `tmux` while it's still in `unconverted.txt`. The file is finished when the policy reports nothing for it. Commit after each file:

```bash
git commit -am "tmux: convert <file> tests (T:n L:n R:n D:n)"
```

The worked example for bucket L below is the pattern to copy. It replaces an 8 s real dialog poll:

```go
func TestAcceptWorkspaceTrustDialogGivesUpOnMissingSession(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClock()
	ex := func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		return nil, []byte("can't find session: gt-x"), errors.New("exit status 1")
	}
	tm := newTmuxForTest("gt-test-x", ex, clk)
	done := make(chan error, 1)
	go func() { done <- tm.AcceptWorkspaceTrustDialog("gt-x") }()
	if err := driveClock(t, clk, time.Second, done); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}
```

Read the real `AcceptWorkspaceTrustDialog` signature and error contract before writing this, and assert what the code actually returns.

- [ ] **Step 4: Remove `internal/tmux` (and `internal/tmux/tmuxfake`, if the seed listed it) from `internal/testpolicy/unconverted.txt`.**

Run: `go test ./internal/testpolicy/ -count=1`
Expected: PASS, with no violation lines for `internal/tmux`.

Run: `grep -c 'testpolicy:allow' internal/tmux/*_test.go | awk -F: '{s+=$2} END {print s}'`
Expected: `0`.

- [ ] **Step 5: Measure against the targets**

```bash
go test -count=1 -cover ./internal/tmux/ | tee /tmp/tmux-cover-after.txt
make test-timing PKGS=./internal/tmux/...                                 # unit tier, taxed pane: must be <= 5s
go test -tags integration -run '^TestIntegration' -count=1 ./internal/tmux/...  # time it: must be <= 60s
```

If the unit tier takes more than 5 s, **stop**. This is checkpoint 1: report the numbers and the slowest tests (`go test -json` elapsed) to the crew session before touching any other package.

- [ ] **Step 6: Commit** the `unconverted.txt` change:

```bash
git commit -am "testpolicy: enforce internal/tmux"
```

### Task 8: `docs/testing.md`, then MR 3

**Files:**
- Create: `docs/testing.md`

- [ ] **Step 1: Write `docs/testing.md`** with these sections, each illustrated by code copied from the tmux rewrite:
  1. The two tiers and the commands (`make test`, `make test-integration`, `make test-timing`).
  2. The rules, the rule names `testpolicy` prints, and the exemption format.
  3. Seams: the exec runner pattern (`execFunc`, `runner()`, `newTmuxForTest`) and the clock pattern (`clk()` and `driveClock`).
  4. Fakes and contracts: how to add a `Run…Contract` and run it in both tiers.
  5. The bucket procedure (T/L/R/D) and the commit-message format for deletions and coverage.
  6. How to take a package off `unconverted.txt`.

- [ ] **Step 2: Gate, write the MR commit message, push and submit**

```bash
make build && make lint && GOFLAGS=-p=8 make test; echo "exit=$?"
git add docs/testing.md && git commit -m "docs: testing.md — rules and patterns for the unit and integration tiers"
```

Write the bucket totals, the deleted-test list (`- TestName: reason`, from the D rows of the triage), coverage before and after, and the step 5 timings into the body of the last commit with `git commit --amend`. Then:

```bash
git log origin/main..HEAD --format=%B | grep -ci 'co-authored\|claude'   # must print 0
git push origin crew/sloan/tmux-rewrite
gt mq submit
```

**Checkpoint 1 review (crew session):** read the diff and the triage. Confirm the timing numbers with a fresh `make test-timing PKGS=./internal/tmux/...`. Only then start Tasks 9 onward.

---

## After the pilot: one task per package

### Task 9 (template): convert one package

Use this template for `slot`, `polecat`, `editorial`, `daemon`, `refinery`, `witness`, `doctor`, and the rest of `internal/*`, in the design's §6 order. Every instance is one subagent in its own worktree, and one MR.

**Files:** `internal/<pkg>/**`, plus deleting its line from `internal/testpolicy/unconverted.txt`

**Interfaces:**
- Consumes: `docs/testing.md`; the shared fakes that exist when the task starts (`tmuxfake`, and later `beadsfake` and `notifyfake`); `clockwork`
- Produces: a package that passes `TestPolicy` with no exemptions and runs its unit tests in 10 s or less

- [ ] **Step 1:** `git fetch origin && git worktree add ../sloan-<pkg> -b crew/sloan/convert-<pkg> origin/main`. Read `docs/testing.md` and this package's rows in `~/.claude/docs/research/test-rewrite/profile.md` and `profile/top25.tsv`.
- [ ] **Step 2: Baseline.** Coverage (`go test -count=1 -cover`), the `go test -json` elapsed time per test, and the test count.
- [ ] **Step 3: Seams (a separate commit, no behaviour change).** Inject a clock into every type that sleeps, polls or ticks. Replace package-level stub variables and `Set*ForTest` with constructor parameters or unexported fields plus a `newXForTest` in a non-test file. Replace `os.Getenv` below the boundary with parameters, and production `os.Setenv` with an explicit `Cmd.Env`. Replace working-directory lookups with explicit `townRoot`. For `slot`: merge the `runningGateContainers` and `removeContainer` variables and their `Set*ForTest` setters into a `slot.ContainerRuntime{List, Remove, Info}` interface, and route `doctor/container_capacity_check.go:34` through it. The existing tests must still pass unchanged, with the same count.
- [ ] **Step 4: Triage every test** into T, L, R or D (the Task 7 definitions). Consumers of tmux, beads or notify use the shared fakes; never write a new PATH stub.
- [ ] **Step 5: Convert file by file,** committing after each (the Task 7 step 3 loop).
- [ ] **Step 6:** Delete the package's line from `unconverted.txt`. `go test ./internal/testpolicy/` must pass. The count of `testpolicy:allow` lines must be 0, or each one justified in the MR.
- [ ] **Step 7: Measure.** `make test-timing PKGS=./internal/<pkg>/...` must be 10 s or less. The integration tests for the package must pass.
- [ ] **Step 8: Gate** (`make build`, `make lint`, `GOFLAGS=-p=8 make test`, by exit code). The commit body carries the bucket totals, the deleted-test list, coverage before and after, and the timings. Run the attribution check, then push, then `gt mq submit`. The crew session reviews before submit.

### Task 10: `beads.Client` and `beadsfake` (shared interface; one agent, no concurrent consumers)

- [ ] Inventory the consumer calls on `*beads.Beads`:
  ```bash
  grep -rhoE '\b[a-zA-Z_]+\.(<method list from beads.go>)\(' --include='*.go' --exclude='*_test.go' internal cmd | sort | uniq -c
  ```
  Also list the 159 `beads.Command*` argv sites (`seams.md` §1).
- [ ] For each argv site, add or reuse a typed method on `*beads.Beads`, and move the site onto it, one consumer package per commit, with no behaviour change. When the sites are gone, delete the exported `beads.Command*` builders.
- [ ] Give `*beads.Beads` an unexported `runner` field over `newBDCmd`, following the Task 4 pattern, and use a recording runner for the package's own tests.
- [ ] Write `beadsfake`: an in-memory store implementing the methods consumers call. Use `beadsdk.Storage` types (`beads.go:105-124` aliases). Add `RunClientContract`, and run it against the fake (unit) and against real `bd` plus Dolt under `gt slot run` (integration, `TestIntegrationClientContract`).
- [ ] The gate, commit rules and MR steps are the same as Task 9.

### Task 11: `notify` (shared interface; one agent)

- [ ] Create `internal/notify` with `type Notifier interface { MailSend(ctx context.Context, to, subject, body string) error; Nudge(ctx context.Context, target, message string) error; Escalate(ctx context.Context, severity, reason, message string) error }`. The production implementation calls `mail.(*Router).Send`, `nudge.Enqueue`, and escalation logic moved out of `internal/cmd/escalate_impl.go:24-232` into `internal/notify/escalate.go`. `cmd` keeps a thin wrapper that calls it.
- [ ] Add `notifyfake.Recorder` and `RunNotifierContract`. The integration run uses a `ScratchTown`.
- [ ] Move the `exec.Command("gt", "mail"|"nudge"|"escalate", …)` sites in daemon, witness, refinery and deacon onto an injected `Notifier`, one package per commit.

### Task 12: `cmd` extraction and the finish (decided at checkpoint 2)

- [ ] Move each `run*` function's logic into a leaf package. The function takes typed options instead of the cobra flag globals. `cmd` binds flags into the options and calls it. `cmd` tests shrink to integration smoke tests on the built binary.
- [ ] Delete `make test-changed`, `changedGoPackages`, the `{packages}` substitution in `test_verify_command`, `-timeout 20m` in `make test`, and `GT_TEST_DOCKER` from `make test`.
- [ ] Delete `.github/workflows/{windows-ci,e2e,nightly-integration,close-stale-needs,remove-needs-info,remove-needs-triage,triage-label,update-nix-flake}.yml`, every `*_windows.go` file, and every `runtime.GOOS == "windows"` branch. Keep `ci.yml` reduced to `make lint` and `make test` on ubuntu-latest, and make it green.
- [ ] Switch the `main_branch_test` command in the gastown rig config to `make test-integration` (a config change; tell the mayor).
- [ ] `unconverted.txt` holds only its header comment.

### Task 13: Final acceptance

- [ ] `make test-timing` (whole unit tier) takes 90 s or less, with a probe of 50 ms or more.
- [ ] `for i in $(seq 20); do go test -count=1 ./... || echo FAIL $i; done`, run while `make test-integration` runs concurrently: zero `FAIL` lines.
- [ ] Unit plus integration takes 3 min or less.
- [ ] The GitHub CI run on the final merge is green.
- [ ] Record the numbers in claude-a3e.2 and close it. claude-a3e.3 (phase 3) is then unblocked.
