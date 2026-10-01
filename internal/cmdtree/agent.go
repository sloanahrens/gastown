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
// repo's agent commands and skills, and AGENTS.md. Repo scripts, git hooks
// and Go exec literals are run by humans or by gt itself, not by agents.
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
	return primeSource(rel)
}

// primeSource reports whether rel is Go that renders gt prime's output, the
// role context every agent reads at session start.
func primeSource(rel string) bool {
	dir, base := path.Split(rel)
	return dir == "internal/cmd/" && strings.HasPrefix(base, "prime") &&
		strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, "_test.go")
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
		scan := ScanGoStrings
		if !primeSource(rel) {
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
