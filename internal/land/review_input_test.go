package land

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land/policy"
)

// TestBuildReviewInput is gt-8af9s7's pin on the move: the one function that
// assembles a review's inputs must give, for every field, what the landing
// worker's inline literal gave. The expected values are written out rather
// than read back through the helpers BuildReviewInput calls, so a change to a
// helper's output shows up here instead of passing on both sides. PolicyText
// is the exception: it is a checked-in asset, and comparing it to the block's
// source is the assertion that the field carries the policy at all.
func TestBuildReviewInput(t *testing.T) {
	t.Parallel()
	const (
		description = "## Goal\ncount each widget once\n\n## Constraints\n- internal/land only\n\n## Acceptance\n- [ ] the count is one\n"
		// The brief BeadBrief renders from description and comments.
		briefText = "# Work item\n\n## Goal\n\n> count each widget once\n\n## Constraints\n\n> - internal/land only\n\n## Decision comments\n\n[OPERATOR DECISION from the-operator, 2026-11-01T00:00:00Z]\n> OPERATOR DECISION: count twice\n"
	)
	oneRejection := FormatRejectionNote(RejectionNote{
		Attempt: 1, Kind: "review", Reason: "om requested changes",
		Branch: "polecat/x/gt-abc", Target: "main", MR: "gt-abc",
		Findings: []Finding{{ID: "f-1", Severity: "major", Path: "internal/land/a.go", Line: 7, Title: "the count runs twice"}},
	})
	twoRejections := oneRejection + "\n\n" + FormatRejectionNote(RejectionNote{
		Attempt: 2, Kind: "review", Reason: "om requested changes",
		Branch: "polecat/x/gt-abc", Target: "main", MR: "gt-abc",
		Findings: []Finding{{ID: "f-2", Severity: "minor", Path: "internal/land/b.go", Line: 9, Title: "the label is stale"}},
	})

	// The operator's decision comment is entitled by op and reaches the brief;
	// the polecat's comment is not a decision and does not.
	op := &config.OperatorConfig{Names: []string{"the-operator"}}
	comments := []beads.Comment{
		{ID: "c-1", Author: "the-operator", CreatedAt: "2026-11-01T00:00:00Z", Text: "OPERATOR DECISION: count twice"},
		{ID: "c-2", Author: "polecat/furiosa", CreatedAt: "2026-11-02T00:00:00Z", Text: "a note that is not a decision"},
	}
	repo := baseRepo{}

	for _, tc := range []struct {
		name  string
		notes string
		prior []Finding
		round int
	}{
		{"no notes", "", nil, 1},
		{"one MERGE REJECTION", oneRejection, []Finding{{ID: "f-1", Severity: "major", Path: "internal/land/a.go", Line: 7, Title: "the count runs twice"}}, 2},
		{"two MERGE REJECTIONS", twoRejections, []Finding{{ID: "f-2", Severity: "minor", Path: "internal/land/b.go", Line: 9, Title: "the label is stale"}}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := &beads.Issue{ID: "gt-abc", Description: description, Notes: tc.notes}
			got := BuildReviewInput(issue, comments, op, "accepted risk text", "rounds text", "base-sha", "head-sha", "/work", repo)
			want := ReviewInput{
				Dir: "/work", Base: "base-sha", Head: "head-sha", Repo: repo, BeadID: "gt-abc",
				CriteriaText:      "- [ ] the count is one",
				PriorFindings:     tc.prior,
				AcceptedRisksText: "accepted risk text",
				BriefText:         briefText,
				PolicyText:        policy.PolicyBlock(),
				Round:             tc.round,
				RoundsText:        "rounds text",
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("BuildReviewInput =\n%#v\nwant\n%#v", got, want)
			}
		})
	}
}

// TestReviewInputGuard is gt-8af9s7's structural guard: the landing worker
// assembles a review's inputs through BuildReviewInput, and the local round
// that will join it calls the same function, so neither can drift into judging
// a bead against material the other never gathered. A construction in any
// non-test file of the package outside BuildReviewInput and the legacy
// OMReviewer.Review is a second assembly with nothing keeping it in step,
// which is exactly the state this bead removes.
func TestReviewInputGuard(t *testing.T) {
	t.Parallel()

	t.Run("no non-test file has a second assembly", func(t *testing.T) {
		t.Parallel()
		// Go runs a package's tests with the package directory as the working
		// directory, so this reads the files the test compiles with.
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		scanned := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			hits, err := reviewInputConstructions(name, src)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			scanned++
			for _, hit := range hits {
				t.Errorf("%s:%d builds a ReviewInput (%s); call BuildReviewInput instead so the landing review and the local round read one assembly", hit.file, hit.line, hit.what)
			}
		}
		if scanned == 0 {
			t.Fatal("no non-test .go file found to scan; the guard read nothing")
		}
	})

	t.Run("a construction outside land.go is flagged", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			src  string
			want []string
		}{
			{
				"composite literal",
				"package land\n\nfunc assemble() ReviewInput {\n\treturn ReviewInput{Dir: \"work\"}\n}\n",
				[]string{"fixture.go:4 composite literal"},
			},
			{
				"qualified literal",
				"package land\n\nfunc assemble() land.ReviewInput {\n\treturn land.ReviewInput{}\n}\n",
				[]string{"fixture.go:4 composite literal"},
			},
			{
				"declared, then filled",
				"package land\n\nfunc assemble() ReviewInput {\n\tvar in ReviewInput\n\tin.Dir = \"work\"\n\treturn in\n}\n",
				[]string{"fixture.go:4 variable declaration"},
			},
			{
				"new, then filled",
				"package land\n\nfunc assemble() *ReviewInput {\n\treturn new(ReviewInput)\n}\n",
				[]string{"fixture.go:4 new(ReviewInput)"},
			},
			{
				"the one assembly is allowed",
				"package land\n\nfunc BuildReviewInput() ReviewInput {\n\treturn ReviewInput{Dir: \"work\"}\n}\n",
				nil,
			},
			{
				"the legacy Review path is allowed",
				"package land\n\nfunc (r OMReviewer) Review(dir, base, head string) ReviewInput {\n\treturn ReviewInput{Dir: dir, Base: base, Head: head}\n}\n",
				nil,
			},
			{
				"another method of the reviewer is not",
				"package land\n\nfunc (r OMReviewer) ReviewBead() ReviewInput {\n\treturn ReviewInput{}\n}\n",
				[]string{"fixture.go:4 composite literal"},
			},
			{
				"a call is no assembly",
				"package land\n\nfunc assemble() ReviewInput {\n\treturn BuildReviewInput(nil, nil, nil, \"\", \"\", \"base\", \"head\", \"work\", nil)\n}\n",
				nil,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				hits, err := reviewInputConstructions("fixture.go", []byte(tc.src))
				if err != nil {
					t.Fatalf("parse fixture: %v", err)
				}
				var got []string
				for _, hit := range hits {
					got = append(got, fmt.Sprintf("%s:%d %s", hit.file, hit.line, hit.what))
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("constructions in the fixture = %q, want %q", got, tc.want)
				}
			})
		}
	})
}

// reviewInputConstruction is one place a file starts assembling a review's
// inputs.
type reviewInputConstruction struct {
	file string
	line int
	what string
}

// reviewInputConstructions parses one Go source file and returns every
// ReviewInput construction it holds outside the declarations allowed to build
// one. The shapes it flags are a composite literal, a variable declared at the
// type, and new(ReviewInput); a file that only calls BuildReviewInput has
// none.
func reviewInputConstructions(name string, src []byte) ([]reviewInputConstruction, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	var hits []reviewInputConstruction
	add := func(pos token.Pos, what string) {
		hits = append(hits, reviewInputConstruction{file: name, line: fset.Position(pos).Line, what: what})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			// An allowed body is the assembly; walk nothing below it.
			return !allowedAssembly(node)
		case *ast.CompositeLit:
			if isReviewInputType(node.Type) {
				add(node.Pos(), "composite literal")
			}
		case *ast.ValueSpec:
			if node.Type != nil && isReviewInputType(node.Type) {
				add(node.Pos(), "variable declaration")
			}
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "new" && len(node.Args) == 1 && isReviewInputType(node.Args[0]) {
				add(node.Pos(), "new(ReviewInput)")
			}
		}
		return true
	})
	return hits, nil
}

// allowedAssembly reports whether fd may build a ReviewInput: BuildReviewInput
// is the package's one assembly, and OMReviewer.Review is the legacy entry
// point a reviewer that implements only Review reaches, with no bead material
// to pass on (review.go).
func allowedAssembly(fd *ast.FuncDecl) bool {
	if fd.Recv == nil {
		return fd.Name.Name == "BuildReviewInput"
	}
	return fd.Name.Name == "Review" && receiverType(fd.Recv) == "OMReviewer"
}

// receiverType names the type a method's receiver declares, "" for a shape it
// does not name (a generic or a parenthesized receiver).
func receiverType(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) != 1 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// isReviewInputType reports whether expr names the ReviewInput type, written
// bare (same package) or qualified (land.ReviewInput).
func isReviewInputType(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "ReviewInput"
	case *ast.SelectorExpr:
		return e.Sel != nil && e.Sel.Name == "ReviewInput"
	default:
		return false
	}
}
