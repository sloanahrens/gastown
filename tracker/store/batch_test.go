package store

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/steveyegge/gastown/tracker/api"
)

// doomedTx fails the test if an operation reaches the transaction: the op kind
// is refused before any statement runs.
type doomedTx struct{ t *testing.T }

func (d doomedTx) refuse() { d.t.Helper(); d.t.Fatal("the refused op kind reached the transaction") }

func (d doomedTx) Begin(context.Context) (pgx.Tx, error) { d.refuse(); return nil, nil }
func (d doomedTx) Commit(context.Context) error          { d.refuse(); return nil }
func (d doomedTx) Rollback(context.Context) error        { d.refuse(); return nil }
func (d doomedTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	d.refuse()
	return 0, nil
}
func (d doomedTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { d.refuse(); return nil }
func (d doomedTx) LargeObjects() pgx.LargeObjects                         { d.refuse(); return pgx.LargeObjects{} }
func (d doomedTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	d.refuse()
	return nil, nil
}
func (d doomedTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.refuse()
	return pgconn.CommandTag{}, nil
}
func (d doomedTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	d.refuse()
	return nil, nil
}
func (d doomedTx) QueryRow(context.Context, string, ...any) pgx.Row { d.refuse(); return nil }
func (d doomedTx) Conn() *pgx.Conn                                  { d.refuse(); return nil }

// The guard keeps an op kind that api gains without a case here from running
// as a no-op write. It returns before the transaction, so the batch's own
// writes are untouched.
func TestBatchDispatchRefusesAnOpNoCaseHandles(t *testing.T) {
	t.Parallel() // unit tier: no database
	if _, err := batchOpInTx(context.Background(), doomedTx{t}, api.BatchOperation{Op: "delete"}, "town/crew/x"); err == nil {
		t.Error("batchOpInTx accepted an op kind with no case")
	}
}

// Two batches over the same issues must take their locks in one order, so the
// lock set is sorted, deduplicated, and leaves out the ids a create in the
// same batch supplies: those rows do not exist until that op runs.
func TestBatchLocksItsTargetsInOneFixedOrder(t *testing.T) {
	t.Parallel() // unit tier: pure over the request
	ops := []api.BatchOperation{
		{Op: api.BatchOpUpdate, Update: &api.BatchUpdate{ID: "gt-b"}},
		{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: "gt-new", Title: "t"}},
		{Op: api.BatchOpComment, Comment: &api.BatchComment{ID: "gt-a", Text: "x"}},
		{Op: api.BatchOpClose, Close: &api.BatchClose{IDs: []string{"gt-c", "gt-a"}}},
		{Op: api.BatchOpComment, Comment: &api.BatchComment{ID: "gt-new", Text: "x"}},
		{Op: api.BatchOpUpdate, Update: &api.BatchUpdate{ID: "gt-b"}},
	}
	got := batchLockIDs(ops)
	if want := []string{"gt-a", "gt-b", "gt-c"}; !slices.Equal(got, want) {
		t.Errorf("batchLockIDs = %v, want %v sorted and deduplicated", got, want)
	}
	if ids := batchLockIDs([]api.BatchOperation{{Op: api.BatchOpCreate, Create: &api.CreateParams{Rig: "gt", Title: "t"}}}); len(ids) != 0 {
		t.Errorf("batchLockIDs of a batch of creates = %v, want none: no row exists to lock", ids)
	}
}
