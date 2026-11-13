package land

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
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

// TestLandHasNoReviewInputLiteral is gt-8af9s7's structural guard: the landing
// worker assembles a review's inputs through BuildReviewInput, and the local
// round that will join it calls the same function, so neither can drift into
// judging a bead against material the other never gathered. A ReviewInput{}
// literal inside land.go is a second assembly with nothing keeping the two in
// step, which is exactly the state this bead removes.
func TestLandHasNoReviewInputLiteral(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	// Go runs a package's tests with the package directory as the working
	// directory, so this reads the land.go the test compiles with.
	file, err := parser.ParseFile(fset, "land.go", nil, 0)
	if err != nil {
		t.Fatalf("parse land.go: %v", err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isReviewInputType(lit.Type) {
			return true
		}
		t.Errorf("land.go:%d builds a ReviewInput literal; call BuildReviewInput instead so the landing review and the local round read one assembly", fset.Position(lit.Pos()).Line)
		return true
	})
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
