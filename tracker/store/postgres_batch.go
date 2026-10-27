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
		rig, err := rigForCreate(params)
		if err != nil {
			return api.BatchOperationResult{}, err
		}
		// No collision retry here, unlike CreateIssue: a failed statement
		// aborts the transaction the retry would need.
		is, err := createInTx(ctx, tx, params, rig)
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
		if err := closeAllInTx(ctx, tx, ids, op.Close.Reason, op.Close.Force, actor); err != nil {
			return api.BatchOperationResult{}, err
		}
		return api.BatchOperationResult{IDs: ids}, nil
	case api.BatchOpComment:
		author := op.Comment.Author
		if author == "" {
			author = actor
		}
		if err := validateComment(author, op.Comment.Text); err != nil {
			return api.BatchOperationResult{}, err
		}
		if err := addCommentInTx(ctx, tx, op.Comment.ID, author, op.Comment.Text); err != nil {
			return api.BatchOperationResult{}, err
		}
		return api.BatchOperationResult{IDs: []string{op.Comment.ID}}, nil
	}
	// Validate refuses an op with no case here before the transaction opens.
	return api.BatchOperationResult{}, fmt.Errorf("store: batch: op %q has no case", op.Op)
}
