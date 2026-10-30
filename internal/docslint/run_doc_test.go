package docslint

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
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

// usageClaim matches a claim that the command prints a usage block. A negated
// claim ("prints no usage block") and a bare mention of usage do not match.
var usageClaim = regexp.MustCompile(`(?i)\bprint(?:s|ing)?\s+(?:an?\s+|the\s+)?usage\b`)

// claimsUsageBlock reports whether doc says the command prints a usage block.
func claimsUsageBlock(doc string) bool {
	return usageClaim.MatchString(doc)
}

// docOptions is each option the command doc names: the word after the command
// name on a line, when that word is an option.
func docOptions(doc string) []string {
	var got []string
	for _, line := range strings.Split(doc, "\n") {
		if words := strings.Fields(line); len(words) >= 2 && words[0] == "docs-lint" && strings.HasPrefix(words[1], "--") {
			got = append(got, words[1])
		}
	}
	return got
}

// parsedOptions is each option parseArgs has a case for, read from the source of
// run.go, so the test compares the doc with the code that refuses or accepts.
func parsedOptions(src string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", src, 0)
	if err != nil {
		return nil, err
	}
	var got []string
	var unquoteErr error
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "parseArgs" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range clause.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					opt, err := strconv.Unquote(lit.Value)
					if err != nil {
						unquoteErr = err
					}
					got = append(got, opt)
				}
			}
			return true
		})
	}
	if unquoteErr != nil {
		return nil, unquoteErr
	}
	if got == nil {
		return nil, errors.New("parseArgs has no option cases")
	}
	return got, nil
}

// TestCommandDocHasNoOptionList keeps the option list out of the cmd/docs-lint
// package doc. The list is a copy of parseArgs that goes stale when an option
// changes, and the doc points at parseArgs instead (docs/writing-for-agents.md
// R2, R3). The command refuses an unknown option on stderr and prints no usage
// block, so the doc also makes no usage claim.
func TestCommandDocHasNoOptionList(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "../../cmd/docs-lint/main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse cmd/docs-lint/main.go: %v", err)
	}
	if file.Doc == nil {
		t.Fatal("cmd/docs-lint has no package doc")
	}
	doc := file.Doc.Text()
	if got := docOptions(doc); len(got) > 0 {
		t.Errorf("package doc restates the option list: %q", got)
	}
	if claimsUsageBlock(doc) {
		t.Error("package doc claims the command prints a usage block")
	}
}

// TestParsedOptionsFixtures drives the read of parseArgs's cases over source text:
// every string in a case clause counts, and a source with no parseArgs or that
// does not parse is refused.
func TestParsedOptionsFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		src     string
		want    []string
		wantErr bool
	}{
		{"each case string is an option", "package docslint\n\nfunc parseArgs(args []string) {\n\tswitch args[0] {\n\tcase \"--a\":\n\tcase \"--b\", \"--c\":\n\t}\n}\n", []string{"--a", "--b", "--c"}, false},
		{"no parseArgs is refused", "package docslint\n\nfunc Run() {}\n", nil, true},
		{"source that does not parse is refused", "package docslint\n\nfunc parseArgs(\n", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsedOptions(c.src)
			if (err != nil) != c.wantErr {
				t.Fatalf("parsedOptions error = %v, want error %v", err, c.wantErr)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("parsedOptions = %q, want %q", got, c.want)
			}
		})
	}
}

// TestClaimsUsageBlock drives the usage-claim check over doc text: a positive
// claim, a negated claim, a bare mention of usage and a doc with no usage word.
func TestClaimsUsageBlock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  string
		want bool
	}{
		{"positive claim with a usage block", "A refused line prints a usage block on stderr.", true},
		{"positive claim with the usage", "It prints the usage and exits 2.", true},
		{"negated claim", "The command refuses an unknown option and prints no usage block.", false},
		{"bare mention of usage", "See the usage notes in the README.", false},
		{"no usage word", "docs-lint --words   total words across the agent-facing tier", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := claimsUsageBlock(c.doc); got != c.want {
				t.Errorf("claimsUsageBlock(%q) = %v, want %v", c.doc, got, c.want)
			}
		})
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
