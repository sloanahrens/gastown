package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// Batch applies the operations of req in order in one transaction. See
// Store.Batch.
func (p *Postgres) Batch(ctx context.Context, req api.BatchRequest) (api.BatchResult, error) {
	// Refused before the transaction opens, so a malformed or oversized batch
	// touches nothing.
	if err := req.Validate(); err != nil {
		return api.BatchResult{}, err
	}
	actor := api.ActorFrom(ctx)
	var res api.BatchResult
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockBatchTargets(ctx, tx, req.Operations); err != nil {
			return err
		}
		var err error
		res, err = batchInTx(ctx, tx, req, actor)
		return err
	})
	if err != nil {
		return api.BatchResult{}, err
	}
	return res, nil
}

// batchInTx runs the operations in request order and stops at the first
// failure, which is returned naming its index; the caller's rollback then
// undoes every operation before it.
func batchInTx(ctx context.Context, tx pgx.Tx, req api.BatchRequest, actor string) (api.BatchResult, error) {
	res := api.BatchResult{Results: make([]api.BatchOperationResult, 0, len(req.Operations))}
	for i, op := range req.Operations {
		r, err := batchOpInTx(ctx, tx, op, actor)
		if err != nil {
			return api.BatchResult{}, api.BatchError(i, err)
		}
		r.Index = i
		if r.IDs == nil {
			r.IDs = []string{} // a close of no ids wrote none, never null on the wire
		}
		res.Results = append(res.Results, r)
	}
	return res, nil
}

// batchOpInTx runs one operation through the same in-transaction code its own
// verb uses, so a batched write records exactly what a single write does.
func batchOpInTx(ctx context.Context, tx pgx.Tx, op api.BatchOperation, actor string) (api.BatchOperationResult, error) {
	switch op.Op {
	case api.BatchOpCreate:
		params := *op.Create
		params.Actor = actor // a body actor is ignored, as on POST /v1/issues
		rig, err := validateCreate(params)
		if err != nil {
			return api.BatchOperationResult{}, err
		}
		// A generated id collides by chance as a single create's does, so the
		// batch retries it too rather than failing an operation the caller
		// cannot fix.
		attempts := 1
		if params.ID == "" && params.Parent == "" {
			attempts = idAttempts // only a random id can collide by chance
		}
		is, err := createRetryingCollisions(ctx, tx, params, rig, attempts)
		if err != nil {
			return api.BatchOperationResult{}, err
		}
		return api.BatchOperationResult{IDs: []string{is.ID}, Issue: &is}, nil
	case api.BatchOpUpdate:
		patch := op.Update.Patch
		patch.Actor = actor
		if err := validatePatch(patch); err != nil {
			return api.BatchOperationResult{}, err
		}
		if err := updateInTx(ctx, tx, op.Update.ID, patch, op.Update.Guard); err != nil {
			return api.BatchOperationResult{}, err
		}
		return api.BatchOperationResult{IDs: []string{op.Update.ID}}, nil
	case api.BatchOpClose:
		ids := uniqueStrings(op.Close.IDs)
		if len(ids) == 0 {
			// a close that names no issue has nothing to do: refuse it rather than
			// commit a silent success that hides the caller's bug
			return api.BatchOperationResult{}, fmt.Errorf("store: batch: a close names no issue")
		}
		if err := closeAllInTx(ctx, tx, ids, op.Close.Reason, op.Close.Force, actor); err != nil {
			return api.BatchOperationResult{}, err
		}
		return api.BatchOperationResult{IDs: ids}, nil
	case api.BatchOpComment:
		if err := validateComment(actor, op.Comment.Text); err != nil {
			return api.BatchOperationResult{}, err
		}
		if err := addCommentInTx(ctx, tx, op.Comment.ID, actor, op.Comment.Text); err != nil {
			return api.BatchOperationResult{}, err
		}
		return api.BatchOperationResult{IDs: []string{op.Comment.ID}}, nil
	}
	// Validate refuses an op with no case here before the transaction opens.
	return api.BatchOperationResult{}, fmt.Errorf("store: batch: op %q has no case", op.Op)
}

// createRetryingCollisions creates the issue, taking a fresh generated id while
// the last attempt collided, up to attempts times.
func createRetryingCollisions(ctx context.Context, tx pgx.Tx, params api.CreateParams, rig string, attempts int) (api.Issue, error) {
	var err error
	for i := 0; i < attempts; i++ {
		var is api.Issue
		if is, err = createInSavepoint(ctx, tx, params, rig); err == nil {
			return is, nil
		}
		if !isIDCollision(err) {
			return api.Issue{}, err
		}
	}
	return api.Issue{}, err
}

// createInSavepoint runs createInTx under a savepoint, so a failed statement
// rolls back to it instead of aborting the batch's transaction.
func createInSavepoint(ctx context.Context, tx pgx.Tx, params api.CreateParams, rig string) (api.Issue, error) {
	sub, err := tx.Begin(ctx)
	if err != nil {
		return api.Issue{}, fmt.Errorf("store: create savepoint: %w", err)
	}
	is, err := createInTx(ctx, sub, params, rig)
	if err != nil {
		_ = sub.Rollback(context.WithoutCancel(ctx))
		return api.Issue{}, err
	}
	if err := sub.Commit(ctx); err != nil {
		return api.Issue{}, fmt.Errorf("store: create savepoint: %w", err)
	}
	return is, nil
}

// lockBatchTargets takes the batch's row locks in id order before any operation
// runs, so batches whose request orders disagree queue instead of deadlocking.
// A missing id is left to the operation that names it, which reports it with
// its index.
func lockBatchTargets(ctx context.Context, tx pgx.Tx, ops []api.BatchOperation) error {
	ids := batchLockIDs(ops)
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM tracker.issues WHERE id = ANY($1) ORDER BY id FOR UPDATE", ids); err != nil {
		return fmt.Errorf("store: lock batch targets: %w", err)
	}
	return nil
}

// batchLockIDs is the ids a batch locks up front: every issue an update, close
// or comment names that no create in the same batch supplies, sorted, so the
// order never depends on the request order.
func batchLockIDs(ops []api.BatchOperation) []string {
	created := map[string]bool{}
	for _, op := range ops {
		if op.Op == api.BatchOpCreate && op.Create != nil && op.Create.ID != "" {
			created[op.Create.ID] = true
		}
	}
	var ids []string
	for _, op := range ops {
		switch {
		case op.Op == api.BatchOpCreate && op.Create != nil && op.Create.Parent != "":
			ids = append(ids, op.Create.Parent)
		case op.Op == api.BatchOpUpdate && op.Update != nil:
			ids = append(ids, op.Update.ID)
		case op.Op == api.BatchOpComment && op.Comment != nil:
			ids = append(ids, op.Comment.ID)
		case op.Op == api.BatchOpClose && op.Close != nil:
			ids = append(ids, op.Close.IDs...)
		}
	}
	out := make([]string, 0, len(ids))
	for _, id := range uniqueStrings(ids) {
		if !created[id] {
			out = append(out, id)
		}
	}
	return out
}
