package api

import (
	"errors"
	"fmt"
)

// MaxBatchOperations bounds one batch: past it a caller is moving data rather
// than making a change, and a long transaction holds its row locks while it
// runs.
const MaxBatchOperations = 100

// The operation kinds a batch can order. They are the per-issue writes a
// caller chains when one change must not land without another.
const (
	BatchOpCreate  = "create"
	BatchOpUpdate  = "update"
	BatchOpClose   = "close"
	BatchOpComment = "comment"
)

// BatchRequest applies its operations in order, in one transaction: either
// every operation is committed or none is.
type BatchRequest struct {
	Operations []BatchOperation `json:"operations"`
}

// BatchOperation is one write of a batch. Op names the kind, and exactly that
// kind's payload must be set.
type BatchOperation struct {
	Op      string        `json:"op"`
	Create  *CreateParams `json:"create,omitempty"`
	Update  *BatchUpdate  `json:"update,omitempty"`
	Close   *BatchClose   `json:"close,omitempty"`
	Comment *BatchComment `json:"comment,omitempty"`
}

// BatchUpdate patches the issue id while guard holds, as PATCH /v1/issues/{id}.
type BatchUpdate struct {
	ID    string      `json:"id"`
	Patch UpdatePatch `json:"patch"`
	Guard Guard       `json:"guard"`
}

// BatchClose closes ids together with reason, as POST /v1/issues:close.
type BatchClose struct {
	IDs    []string `json:"ids"`
	Reason string   `json:"reason,omitempty"`
	Force  bool     `json:"force,omitempty"`
}

// BatchComment appends a comment to the issue id. Author defaults to the
// token's actor; a caller sets it to write on someone else's behalf.
type BatchComment struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Author string `json:"author,omitempty"`
}

// BatchResult holds one entry per operation, in request order.
type BatchResult struct {
	Results []BatchOperationResult `json:"results"`
}

// BatchOperationResult is what one operation wrote.
type BatchOperationResult struct {
	// Index is the operation's position in the request, the number a failure
	// names.
	Index int `json:"index"`
	// IDs are the issues the operation wrote: the id a create minted, the
	// target of an update or a comment, and the ids a close was given.
	IDs []string `json:"ids"`
	// Issue is the issue a create made; nil for the other kinds.
	Issue *Issue `json:"issue,omitempty"`
}

// Validate refuses an empty batch, one over MaxBatchOperations, and an
// operation whose op names no kind or does not carry exactly that kind's
// payload. Every message names the offending operation's index.
func (r BatchRequest) Validate() error {
	if len(r.Operations) == 0 {
		return Invalid("a batch needs at least one operation")
	}
	if len(r.Operations) > MaxBatchOperations {
		return Invalid("a batch holds at most %d operations, got %d", MaxBatchOperations, len(r.Operations))
	}
	for i, op := range r.Operations {
		if err := op.validate(); err != nil {
			return Invalid("operation %d: %v", i, err)
		}
	}
	return nil
}

// validate names what is wrong with op, or returns nil. The error is plain so
// Validate can wrap it with the operation's index.
func (op BatchOperation) validate() error {
	payloads := 0
	for _, set := range []bool{op.Create != nil, op.Update != nil, op.Close != nil, op.Comment != nil} {
		if set {
			payloads++
		}
	}
	switch op.Op {
	case BatchOpCreate:
		if op.Create == nil {
			return errors.New("op create needs a create payload")
		}
	case BatchOpUpdate:
		if op.Update == nil {
			return errors.New("op update needs an update payload")
		}
	case BatchOpClose:
		if op.Close == nil {
			return errors.New("op close needs a close payload")
		}
	case BatchOpComment:
		if op.Comment == nil {
			return errors.New("op comment needs a comment payload")
		}
	default:
		return fmt.Errorf("unknown op %q, want create, update, close or comment", op.Op)
	}
	if payloads > 1 {
		return fmt.Errorf("op %s carries more than one payload", op.Op)
	}
	return nil
}

// BatchError names the operation that failed: err keeps its kind and its
// message gains the operation's index, so the caller learns which operation to
// fix. An error that is not an *Error is returned unchanged, for the server to
// answer as an internal failure.
func BatchError(index int, err error) error {
	var ae *Error
	if !errors.As(err, &ae) {
		return err
	}
	return &Error{Kind: ae.Kind, Message: fmt.Sprintf("operation %d: %s", index, ae.Message)}
}
