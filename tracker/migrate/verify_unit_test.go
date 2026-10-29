package migrate

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// emptyTracker is a Querier over a tracker that holds nothing: every query returns no rows and every
// count is zero. Verify against it reports the dataset as missing, and the repair check's own problems
// still come through, which is what these tests read.
type emptyTracker struct{}

type noRows struct{}

func (noRows) Close()                                       {}
func (noRows) Err() error                                   { return nil }
func (noRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (noRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (noRows) Next() bool                                   { return false }
func (noRows) Scan(...any) error                            { return nil }
func (noRows) Values() ([]any, error)                       { return nil, nil }
func (noRows) RawValues() [][]byte                          { return nil }
func (noRows) Conn() *pgx.Conn                              { return nil }
func (noRows) TypeMap() *pgtype.Map                         { return nil }

type zeroCount struct{}

func (zeroCount) Scan(dest ...any) error {
	for _, d := range dest {
		if n, ok := d.(*int); ok {
			*n = 0
		}
	}
	return nil
}

func (emptyTracker) Query(context.Context, string, ...any) (pgx.Rows, error) { return noRows{}, nil }
func (emptyTracker) QueryRow(context.Context, string, ...any) pgx.Row        { return zeroCount{} }
func (emptyTracker) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// repairProblemsOf runs verify with repair standing in for the importer's repair and returns the
// problems it listed that come from the repair check.
func repairProblemsOf(t *testing.T, ds *Dataset, repair func(*Dataset) (deps, synthesized []DepRow)) []string {
	t.Helper()
	diff, err := verify(context.Background(), emptyTracker{}, ds, repair)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var out []string
	for _, p := range diff.Problems {
		if strings.HasPrefix(p, "repair ") {
			out = append(out, p)
		}
	}
	return out
}

func childDataset() *Dataset {
	return &Dataset{Issues: []IssueRow{{ID: "gt-a"}, {ID: "gt-a.1"}}}
}

// TestVerifyRaisesTheRepairCheckThroughVerify drives the repair check at the level where it matters:
// Verify's own problem list. A repair that links the wrong parent, drops a link, or repeats one must
// each show up there, and the real repair must raise nothing.
func TestVerifyRaisesTheRepairCheckThroughVerify(t *testing.T) {
	t.Parallel()
	good := DepRow{IssueID: "gt-a.1", DependsOn: "gt-a", Type: "parent-child"}
	for name, tc := range map[string]struct {
		repair func(*Dataset) (deps, synthesized []DepRow)
		want   string
	}{
		"wrong parent": {
			repair: func(*Dataset) ([]DepRow, []DepRow) {
				return []DepRow{{IssueID: "gt-a.1", DependsOn: "gt-other", Type: "parent-child"}}, nil
			},
			want: "repair links gt-a.1 -> gt-other (parent-child), which the rule does not give it",
		},
		"missing link": {
			repair: func(*Dataset) ([]DepRow, []DepRow) { return nil, nil },
			want:   "repair of gt-a.1 -> gt-a is missing from the import",
		},
		"repeated link": {
			repair: func(*Dataset) ([]DepRow, []DepRow) { return []DepRow{good, good}, nil },
			want:   "repair links gt-a.1 -> gt-a (parent-child) more than once",
		},
	} {
		if got := repairProblemsOf(t, childDataset(), tc.repair); !slices.Contains(got, tc.want) {
			t.Errorf("%s: Verify listed %q, want it to include %q", name, got, tc.want)
		}
	}
	if got := repairProblemsOf(t, childDataset(), repairedDeps); len(got) != 0 {
		t.Errorf("the real repair raised %q, want no repair problem", got)
	}
}
