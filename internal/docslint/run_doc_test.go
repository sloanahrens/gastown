package docslint

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// runDoc is the doc comment of func Run in the Go source src, as its text reads.
func runDoc(src string) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", src, parser.ParseComments)
	if err != nil {
		return "", err
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Run" && fn.Doc != nil {
			return fn.Doc.Text(), nil
		}
	}
	return "", errors.New("Run has no doc comment")
}

// restatedUsage is each line of doc that copies a docs-lint usage line.
func restatedUsage(doc string) []string {
	var got []string
	for _, line := range strings.Split(doc, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "docs-lint ") {
			got = append(got, l)
		}
	}
	return got
}

// TestCommandDocListsOptions holds the option list to one home: the cmd/docs-lint
// package doc, the only place a reader of the command finds it. The command
// refuses an unknown option on stderr and prints no usage block, so the doc names
// the options without claiming the command prints them.
func TestCommandDocListsOptions(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "../../cmd/docs-lint/main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse cmd/docs-lint/main.go: %v", err)
	}
	if file.Doc == nil {
		t.Fatal("cmd/docs-lint has no package doc")
	}
	doc := file.Doc.Text()
	for _, opt := range []string{"docs-lint --list <tier>", "docs-lint --slice N M", "docs-lint --words"} {
		if !strings.Contains(doc, opt) {
			t.Errorf("package doc does not document %q", opt)
		}
	}
	if strings.Contains(strings.ToLower(doc), "usage") {
		t.Error("package doc claims the command prints a usage block")
	}
}

// TestRunDocDoesNotRestateUsage keeps the option list out of the Run doc comment:
// it has one home, the package doc, and a second copy goes stale when the options
// change (docs/writing-for-agents.md R2, R3).
func TestRunDocDoesNotRestateUsage(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatalf("read run.go: %v", err)
	}
	doc, err := runDoc(string(src))
	if err != nil {
		t.Fatalf("run.go: %v", err)
	}
	if got := restatedUsage(doc); len(got) > 0 {
		t.Errorf("Run doc restates the usage block: %q", got)
	}
}

// TestRunDocFixtures drives each branch of the doc check over source text: the
// pre-fix comment that copied the usage block, a comment that names no option,
// a Run with no doc, and source that does not parse.
func TestRunDocFixtures(t *testing.T) {
	t.Parallel()
	const preFix = `package docslint

// Run is the docs-lint command: args are the words after its name.
//
//	docs-lint                 run all checks; one line per finding; exit 1 on any
//	docs-lint --list <tier>   agent-facing | reference | historical | go
//	docs-lint --slice N M     N docs and M go files, least recently committed first
//	docs-lint --words         total words across the agent-facing tier
//
// A check that cannot run exits 2.
func Run() {}
`
	cases := []struct {
		name      string
		src       string
		wantLines int
		wantErr   bool
	}{
		{"pre-fix comment restates all four usage lines", preFix, 4, false},
		{"comment that names no option restates none", "package docslint\n\n// Run runs the checks.\nfunc Run() {}\n", 0, false},
		{"Run without a doc comment is refused", "package docslint\n\nfunc Run() {}\n", 0, true},
		{"source that does not parse is refused", "package docslint\n\nfunc Run(\n", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			doc, err := runDoc(c.src)
			if (err != nil) != c.wantErr {
				t.Fatalf("runDoc error = %v, want error %v", err, c.wantErr)
			}
			if got := restatedUsage(doc); len(got) != c.wantLines {
				t.Errorf("restated usage lines = %q, want %d", got, c.wantLines)
			}
		})
	}
}
