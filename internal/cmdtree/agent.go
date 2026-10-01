package cmdtree

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// AgentBdAllowed is the bd surface agent prose may name (deep review D1 rule
// 6, gt-7iwy0.6): read-only commands plus bd remember. Every mutation goes
// through a gt verb, so gt can hold the invariants bd alone cannot (the
// landing worker closes landed work, gt mol step done advances the hook).
var AgentBdAllowed = []string{"list", "query", "ready", "remember", "search", "show"}

// agentBdReason is the Reason on every agent-prose violation.
var agentBdReason = "agents may run only bd " + strings.Join(AgentBdAllowed, ", ") + "; route it through a gt verb"

// agentFacing reports whether a repo-relative path is prose an agent reads
// as instructions: formulas, role and message templates, plugins, the
// repo's agent commands and skills, AGENTS.md, and the hints gt's commands
// print. Repo scripts, git hooks and Go exec literals are run by humans or by
// gt itself, not by agents.
func agentFacing(rel string) bool {
	switch {
	case rel == "AGENTS.md":
		return true
	case strings.HasPrefix(rel, "internal/formula/formulas/"),
		strings.HasPrefix(rel, "internal/templates/"),
		strings.HasPrefix(rel, "templates/"),
		strings.HasPrefix(rel, "plugins/"),
		strings.HasPrefix(rel, ".claude/commands/"),
		strings.HasPrefix(rel, ".claude/skills/"):
		return scannerFor(rel) != nil
	}
	return primeSource(rel) || hintSource(rel)
}

// primeSource reports whether rel is Go that renders gt prime's output, the
// role context every agent reads at session start: prime*.go and the memory
// index prime injects.
func primeSource(rel string) bool {
	dir, base := path.Split(rel)
	return cmdSource(rel) && (strings.HasPrefix(base, "prime") || base == "memory_index.go") && dir == "internal/cmd/"
}

// hintSource reports whether rel is gt command Go whose printed output
// ScanGoHints reads: every non-test internal/cmd file prime does not render.
func hintSource(rel string) bool { return cmdSource(rel) && !primeSource(rel) }

func cmdSource(rel string) bool {
	dir, base := path.Split(rel)
	return dir == "internal/cmd/" && strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, "_test.go")
}

// ScanGoStrings treats every line of every string literal in a Go file as
// shell, for Go that prints instructions (gt prime). Each ref reports the
// literal's first line.
func ScanGoStrings(file string, src []byte) ([]Ref, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		line := fset.Position(lit.Pos()).Line
		for _, l := range strings.Split(s, "\n") {
			refs = append(refs, scanShellLine(file, line, l)...)
		}
		return true
	})
	return refs, nil
}

// printFuncs are the fmt calls whose string arguments ScanGoHints reads.
var printFuncs = map[string]bool{"Print": true, "Printf": true, "Println": true, "Fprint": true, "Fprintf": true, "Fprintln": true}

// helpFields are the cobra.Command fields gt help prints.
var helpFields = map[string]bool{"Use": true, "Short": true, "Long": true, "Example": true}

// ScanGoHints returns the invocations gt's commands print as advice: string
// literals passed to fmt.Print* or fmt.Fprint* (alone or joined with +),
// and cobra help text (Use, Short, Long and Example in a cobra.Command
// literal or assigned later, including a fmt.Sprintf format; gt-k7u3r),
// read line by line with scanHintLine. Errors (fmt.Errorf), other values
// built with Sprintf, and dry-run output (an if whose condition names a
// dry-run flag, or a func named dryRun*) describe what gt does, not what an
// agent should run, and are skipped (gt-7iwy0.9).
func ScanGoHints(file string, src []byte) ([]Ref, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	scanLits := func(lits []*ast.BasicLit) {
		for _, lit := range lits {
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			line := fset.Position(lit.Pos()).Line
			for i, l := range strings.Split(s, "\n") {
				// A raw string's lines are physical lines; report each
				// one where it sits.
				at := line
				if strings.HasPrefix(lit.Value, "`") {
					at += i
				}
				refs = append(refs, scanHintLine(file, at, l)...)
			}
		}
	}
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			return !namesDryRun(n.Name)
		case *ast.IfStmt:
			if namesDryRun(n.Cond) {
				if n.Else != nil {
					ast.Inspect(n.Else, visit)
				}
				return false
			}
		case *ast.CompositeLit:
			if !isPkgSel(n.Type, "cobra", "Command") {
				return true
			}
			for _, e := range n.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && helpFields[key.Name] {
						scanLits(helpLiterals(kv.Value))
					}
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && helpFields[sel.Sel.Name] && i < len(n.Rhs) {
					scanLits(helpLiterals(n.Rhs[i]))
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || !isPkgSel(sel, "fmt", sel.Sel.Name) || !printFuncs[sel.Sel.Name] {
				return true
			}
			for _, arg := range n.Args {
				scanLits(concatLiterals(arg))
			}
		}
		return true
	}
	ast.Inspect(f, visit)
	return refs, nil
}

// isPkgSel reports whether e is the selector pkg.name.
func isPkgSel(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// helpLiterals returns the fixed text of a help field: its literals, or a
// fmt.Sprintf format's.
func helpLiterals(e ast.Expr) []*ast.BasicLit {
	if call, ok := e.(*ast.CallExpr); ok && isPkgSel(call.Fun, "fmt", "Sprintf") && len(call.Args) > 0 {
		return concatLiterals(call.Args[0])
	}
	return concatLiterals(e)
}

// concatLiterals returns the string literals in e when e is a literal or a
// + chain, the forms that put fixed text on screen.
func concatLiterals(e ast.Expr) []*ast.BasicLit {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return []*ast.BasicLit{e}
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return append(concatLiterals(e.X), concatLiterals(e.Y)...)
		}
	case *ast.ParenExpr:
		return concatLiterals(e.X)
	}
	return nil
}

// namesDryRun reports whether n mentions an identifier naming a dry run.
func namesDryRun(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "dryrun") {
			found = true
		}
		return !found
	})
	return found
}

// hintLead is what may precede a command at the start of a printed hint
// line: indentation, bullets, list numbers, box rules and a shell prompt.
const hintLead = " \t-*•→>$│║0123456789.)"

// scanHintLine returns the invocations on one printed line that read as
// something to run: at the line's start (past hintLead and leading %s/%v
// icons), after a label colon ("Cook to proto:  bd cook"), or in command
// position such as a code span ("see `bd kv list`"). A diagnostic label
// ("Warning: bd blocked failed") reports a bd call gt made, and a mention
// inside prose ("Running 'bd init' here would ...") is not advice.
func scanHintLine(file string, line int, text string) []Ref {
	body := text
	for {
		trimmed := strings.TrimLeft(body, hintLead)
		trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "%s"), "%v")
		if trimmed == body {
			break
		}
		body = trimmed
	}
	offset := len(text) - len(body)
	return scanMatches(file, line, text, func(_ byte, start int) bool {
		if start < offset {
			return false
		}
		prefix := text[offset:start]
		if prefix == "" || commandPosition(prefix) {
			return true
		}
		label, ok := strings.CutSuffix(strings.TrimRight(prefix, " \t"), ":")
		return ok && !diagnosticLabel(label)
	})
}

// diagnosticLabel reports whether a label's last word marks a warning or an
// error rather than an instruction.
func diagnosticLabel(label string) bool {
	f := strings.Fields(strings.ToLower(label))
	if len(f) == 0 {
		return false
	}
	switch f[len(f)-1] {
	case "warning", "error":
		return true
	}
	return false
}

// ScanAgentProse walks root and returns every invocation in agent-facing
// prose, in walk order.
func ScanAgentProse(root string) ([]Ref, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	fsys := r.FS()

	var refs []Ref
	err = fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !agentFacing(rel) {
			return nil
		}
		var scan scanFunc
		switch {
		case primeSource(rel):
			scan = ScanGoStrings
		case hintSource(rel):
			scan = ScanGoHints
		default:
			scan = scannerFor(rel)
		}
		src, err := fs.ReadFile(fsys, rel)
		if err != nil {
			return err
		}
		found, err := scan(rel, src)
		if err != nil {
			return err
		}
		refs = append(refs, found...)
		return nil
	})
	return refs, err
}

// CheckAgentBd returns the bd invocations in refs whose command, after
// alias resolution against bd (bd new is bd create), is not in
// AgentBdAllowed. Words bd does not know are left to Check.
func CheckAgentBd(refs []Ref, bd *Tree) []Violation {
	allowed := map[string]bool{}
	for _, c := range AgentBdAllowed {
		allowed[c] = true
	}
	var out []Violation
	for _, r := range refs {
		if r.Bin != "bd" || len(r.Words) == 0 {
			continue
		}
		name, ok := bd.Canonical(r.Words[0])
		if !ok || allowed[name] {
			continue
		}
		if r.Comment {
			// "# Close with: bd close" is an instruction; bare comment
			// prose naming bd is not, and Check already skips it.
			if res := bd.Resolve(r.Words); len(res.Matched) == 0 {
				continue
			}
		}
		out = append(out, Violation{Ref: r, Reason: agentBdReason})
	}
	return out
}

// AgentBdKey is the baseline key of a violation: its file and canonical
// bd command.
type AgentBdKey struct{ File, Command string }

func (k AgentBdKey) String() string { return k.File + " bd " + k.Command }

// ParseAgentBdBaseline reads baseline lines "<file> bd <command> <count>".
// Blank lines and # comments are skipped.
func ParseAgentBdBaseline(text string) (map[AgentBdKey]int, error) {
	out := map[AgentBdKey]int{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 4 || f[1] != "bd" {
			return nil, fmt.Errorf("baseline line %d: want \"<file> bd <command> <count>\", got %q", n, line)
		}
		count, err := strconv.Atoi(f[3])
		if err != nil || count < 1 {
			return nil, fmt.Errorf("baseline line %d: bad count %q", n, f[3])
		}
		k := AgentBdKey{File: f[0], Command: f[2]}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("baseline line %d: duplicate entry %s", n, k)
		}
		out[k] = count
	}
	return out, sc.Err()
}

// CompareAgentBdBaseline splits violations against a baseline of known
// sites still waiting on a gt verb. A key whose count grew reports all its
// violations as new; a key whose count shrank, or vanished, is stale and
// must be lowered so the baseline only ever ratchets down.
func CompareAgentBdBaseline(violations []Violation, baseline map[AgentBdKey]int, bd *Tree) (fresh []Violation, stale []string) {
	byKey := map[AgentBdKey][]Violation{}
	for _, v := range violations {
		name, _ := bd.Canonical(v.Words[0])
		k := AgentBdKey{File: v.File, Command: name}
		byKey[k] = append(byKey[k], v)
	}
	for k, vs := range byKey {
		if len(vs) > baseline[k] {
			fresh = append(fresh, vs...)
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
		if fresh[i].File != fresh[j].File {
			return fresh[i].File < fresh[j].File
		}
		return fresh[i].Line < fresh[j].Line
	})
	sort.Strings(stale)
	return fresh, stale
}
