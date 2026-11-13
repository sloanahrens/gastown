package land

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeBeadReviewer is the local round's reviewer seam: it keeps every input it
// is given and answers with the verdict or error the test set. It obeys ctx, so
// a round under an expired bound gets the deadline back the way the real
// reviewer gives it.
type fakeBeadReviewer struct {
	verdict Verdict
	err     error
	seen    []ReviewInput
}

func (f *fakeBeadReviewer) ReviewBead(ctx context.Context, in ReviewInput) (Verdict, error) {
	f.seen = append(f.seen, in)
	if f.err != nil {
		return Verdict{}, f.err
	}
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	return f.verdict, nil
}

// localRoundInput is a ReviewInput with every field BuildReviewInput fills set,
// so the test can assert the round hands the reviewer the whole input rather
// than a subset of the landing path's fields.
func localRoundInput() ReviewInput {
	return ReviewInput{
		Dir: "/polecat/tree", Base: "base-sha", Head: "head-sha", Repo: baseRepo{omJSON: "{}"},
		BeadID:            "gt-w4rvi2",
		CriteriaText:      "- [ ] the round writes no verdict",
		AcceptedRisksText: "an overseer accepted the missing bound",
		PriorFindings:     []Finding{{ID: "f-0", Severity: "minor", Title: "the label is stale"}},
		BriefText:         "# Work item\n\n## Goal\n\nthe round returns findings",
		PolicyText:        "town policy",
		Round:             2,
		RoundsText:        `[{"round":1}]`,
	}
}

// TestRunLocalRoundReturnsFindings: the round is the landing's review reached
// from the polecat's tree, so it hands the reviewer the input unchanged and
// gives back the verdict's findings with their ids and severities.
func TestRunLocalRoundReturnsFindings(t *testing.T) {
	t.Parallel()
	in := localRoundInput()
	want := []Finding{
		{ID: "f-1", Severity: "major", Path: "internal/land/local_round.go", Line: 31, Title: "the round records a verdict"},
		{ID: "f-2", Severity: "minor", Path: "internal/land/local_round.go", Line: 40, Title: "a refusal reads as a clean review"},
	}
	r := &fakeBeadReviewer{verdict: Verdict{Verdict: VerdictRequestChanges, Findings: want}}

	got, err := RunLocalRound(context.Background(), r, in)
	if err != nil {
		t.Fatalf("RunLocalRound: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunLocalRound findings = %+v, want %+v", got, want)
	}
	if len(r.seen) != 1 {
		t.Fatalf("reviewer called %d times, want 1", len(r.seen))
	}
	if !reflect.DeepEqual(r.seen[0], in) {
		t.Errorf("reviewer input =\n%#v\nwant the round's input unchanged:\n%#v", r.seen[0], in)
	}
}

// TestRunLocalRoundPropagatesFailure: a reviewer error and an expired bound each
// come back unchanged with no findings, so the caller reports "preview
// unavailable" and never reads a failed round as a clean review.
func TestRunLocalRoundPropagatesFailure(t *testing.T) {
	t.Parallel()

	t.Run("reviewer error", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("om: the model request failed")
		r := &fakeBeadReviewer{err: sentinel}

		got, err := RunLocalRound(context.Background(), r, localRoundInput())
		if err != sentinel {
			t.Fatalf("RunLocalRound error = %v, want the reviewer's error unchanged", err)
		}
		if got != nil {
			t.Errorf("RunLocalRound findings = %+v on error, want none", got)
		}
	})

	t.Run("context deadline", func(t *testing.T) {
		t.Parallel()
		// The caller's bound has already expired: the deadline is in the past,
		// so the test waits on no wall clock.
		start := time.Now()
		ctx, cancel := context.WithDeadline(context.Background(), start.Add(-time.Second))
		defer cancel()
		r := &fakeBeadReviewer{verdict: Verdict{Verdict: VerdictApprove}}

		got, err := RunLocalRound(ctx, r, localRoundInput())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunLocalRound error = %v, want context.DeadlineExceeded", err)
		}
		if got != nil {
			t.Errorf("RunLocalRound findings = %+v under an expired bound, want none", got)
		}
	})
}

// TestRunLocalRoundRefusesNoReviewer: an unwired reviewer is a refusal, not a
// panic and not an empty round.
func TestRunLocalRoundRefusesNoReviewer(t *testing.T) {
	t.Parallel()
	got, err := RunLocalRound(context.Background(), nil, localRoundInput())
	if err == nil || got != nil {
		t.Fatalf("RunLocalRound with no reviewer = %+v, %v; want no findings and an error", got, err)
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("RunLocalRound error = %q, want it to name the missing reviewer", err)
	}
}

// TestRunLocalRoundTakesNoStores: the round is a preview, so the verdict store,
// the review log and the rounds ledger are outside it. The signature admits
// only ctx, the reviewer and the ReviewInput, and no symbol in the function's
// declaration names one of the three.
func TestRunLocalRoundTakesNoStores(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	// Go runs a package's tests with the package directory as the working
	// directory, so this reads the local_round.go the test compiles with.
	file, err := parser.ParseFile(fset, "local_round.go", nil, 0)
	if err != nil {
		t.Fatalf("parse local_round.go: %v", err)
	}
	var decl *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "RunLocalRound" {
			decl = fd
		}
	}
	if decl == nil {
		t.Fatal("local_round.go declares no RunLocalRound")
	}

	if got, want := fieldTypes(decl.Type.Params), []string{"context.Context", "BeadReviewer", "ReviewInput"}; !slices.Equal(got, want) {
		t.Errorf("RunLocalRound parameters = %v, want %v", got, want)
	}
	if got, want := fieldTypes(decl.Type.Results), []string{"[]Finding", "error"}; !slices.Equal(got, want) {
		t.Errorf("RunLocalRound results = %v, want %v", got, want)
	}

	for _, name := range []string{"VerdictStore", "ReviewsFile", "RoundsLedger"} {
		ast.Inspect(decl, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				t.Errorf("RunLocalRound names %s at %s; the local round writes no verdict, review-log record or ledger round", name, fset.Position(id.Pos()))
			}
			return true
		})
	}
}

// fieldTypes is the type of every parameter a field list declares, one entry
// per name, so a grouped declaration reads the same as an ungrouped one.
func fieldTypes(fields *ast.FieldList) []string {
	if fields == nil {
		return nil
	}
	var out []string
	for _, f := range fields.List {
		for range max(len(f.Names), 1) {
			out = append(out, types.ExprString(f.Type))
		}
	}
	return out
}
