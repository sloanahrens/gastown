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
// is given and answers with the verdict or error the test set. It ignores ctx
// on purpose, so a round under an expired bound reaches the deadline only if
// RunLocalRound enforces the bound itself, not because the fake checked it.
// onReview runs inside the call, before the verdict is returned, so a test can
// make the bound expire while the review is in flight.
type fakeBeadReviewer struct {
	verdict  Verdict
	err      error
	onReview func()
	seen     []ReviewInput
}

func (f *fakeBeadReviewer) ReviewBead(_ context.Context, in ReviewInput) (Verdict, error) {
	f.seen = append(f.seen, in)
	if f.onReview != nil {
		f.onReview()
	}
	if f.err != nil {
		return Verdict{}, f.err
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
		// so the test waits on no wall clock. The fake ignores ctx and would
		// answer approve, so only the round's own check of the bound keeps its
		// findings out.
		start := time.Now()
		ctx, cancel := context.WithDeadline(context.Background(), start.Add(-time.Second))
		defer cancel()
		r := &fakeBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Findings: []Finding{{ID: "f-1", Severity: "minor", Title: "late"}}}}

		got, err := RunLocalRound(ctx, r, localRoundInput())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunLocalRound error = %v, want context.DeadlineExceeded", err)
		}
		if got != nil {
			t.Errorf("RunLocalRound findings = %+v under an expired bound, want none", got)
		}
		if len(r.seen) != 0 {
			t.Errorf("reviewer called %d times under an expired bound, want 0", len(r.seen))
		}
	})

	t.Run("bound expires during the review", func(t *testing.T) {
		t.Parallel()
		// The review starts under a live bound and the fake cancels it before
		// answering, the way a slow om outlives the caller's timeout: its
		// findings are too late to be trusted.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := &fakeBeadReviewer{
			verdict:  Verdict{Verdict: VerdictApprove, Findings: []Finding{{ID: "f-1", Severity: "minor", Title: "late"}}},
			onReview: cancel,
		}

		got, err := RunLocalRound(ctx, r, localRoundInput())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunLocalRound error = %v, want context.Canceled", err)
		}
		if got != nil {
			t.Errorf("RunLocalRound findings = %+v after the bound expired, want none", got)
		}
		if len(r.seen) != 1 {
			t.Errorf("reviewer called %d times, want 1", len(r.seen))
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

// TestRunLocalRoundRefusesTypedNilReviewer: an interface holding a nil pointer
// is a missing reviewer too, not a call into a nil receiver.
func TestRunLocalRoundRefusesTypedNilReviewer(t *testing.T) {
	t.Parallel()
	var r *fakeBeadReviewer

	got, err := RunLocalRound(context.Background(), r, localRoundInput())
	if err == nil || got != nil {
		t.Fatalf("RunLocalRound with a typed-nil reviewer = %+v, %v; want no findings and an error", got, err)
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("RunLocalRound error = %q, want it to name the missing reviewer", err)
	}
}

// localRoundNames is every identifier RunLocalRound may name: its parameters,
// results and locals, the helper that refuses a missing reviewer, and the
// standard-library names those need. An identifier outside it — a verdict store,
// the review log, the rounds ledger, or any symbol added later — fails the test
// until it is deliberately added here, so a store cannot reach the round by a
// name this list has never heard of.
var localRoundNames = map[string]bool{
	"RunLocalRound": true,
	"ctx":           true,
	"r":             true,
	"in":            true,
	"v":             true,
	"err":           true,
	"context":       true,
	"Context":       true,
	"BeadReviewer":  true,
	"ReviewInput":   true,
	"Finding":       true,
	"error":         true,
	"nil":           true,
	"isNilReviewer": true,
	"errors":        true,
	"New":           true,
	"Err":           true,
	"ReviewBead":    true,
	"Findings":      true,
}

// TestRunLocalRoundTakesNoStores: the round is a preview, so the verdict store,
// the review log and the rounds ledger are outside it. The signature admits
// only ctx, the reviewer and the ReviewInput, and every identifier in the
// function's declaration is one the preview's own allowlist names, so gaining
// access to a store takes an edit here.
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

	seen := map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		seen[id.Name] = true
		if !localRoundNames[id.Name] {
			t.Errorf("RunLocalRound names %s at %s; the local round writes no verdict, review-log record or ledger round, so add the name to localRoundNames only if it reaches no store", id.Name, fset.Position(id.Pos()))
		}
		return true
	})
	for name := range localRoundNames {
		if !seen[name] {
			t.Errorf("localRoundNames entry %q matches no identifier; remove it so it cannot silently allow a future symbol", name)
		}
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
