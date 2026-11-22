package deployerinbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/clusterapproval"
)

// ErrNoApproval reports that the broker has written no approval record for a
// proposal; the steps return it and Handle answers the request refused
// (gt-sj8wdq).
var ErrNoApproval = errors.New("deployerinbox: the broker has written no approval record")

// ErrApplyRefused reports that Apply refused to touch the cluster because the
// files it re-derived or a live version it re-read had moved from the verified
// value; Handle answers the request refused rather than failed (gt-sj8wdq).
var ErrApplyRefused = errors.New("deployerinbox: the plan or a live version moved after the verify")

// Steps is the deployer's work as Handle sequences it, injected so the
// sequence runs in a test without a cluster (gt-v3nzkf).
type Steps interface {
	// Plan renders and hashes the plan named name, reading no approval record.
	Plan(ctx context.Context, name string) (clusterapproval.Plan, error)
	// WriteProposal writes proposal as the proposal of the plan named name.
	WriteProposal(ctx context.Context, name string, proposal []byte, id string) error
	// LiveVersions returns the live resourceVersion of every object p names.
	LiveVersions(ctx context.Context, p clusterapproval.Plan) (map[string]string, error)
	// Apply applies exactly verified to the cluster whose live versions were
	// verifiedLive; when the files or a verified key's live version have moved
	// since Verify read them, it applies nothing and wraps ErrApplyRefused
	// (gt-sj8wdq).
	Apply(ctx context.Context, name string, verified clusterapproval.Plan, verifiedLive map[string]string) error
	// ReadRecord returns the broker's approval record for proposalID, and
	// ErrNoApproval when it has written none.
	ReadRecord(ctx context.Context, proposalID string) (clusterapproval.Record, error)
	// Rollback undoes the applied plan named name to its recorded revisions, and
	// undoes exactly verified, the plan whose approval record Verify checked: an
	// implementation that finds another plan under name (a different digest) acts
	// on nothing. Handle calls it only for a rollback request whose approval
	// record verified, with the same plan value it hands Apply (gt-iiccy3).
	Rollback(ctx context.Context, name string, verified clusterapproval.Plan) error
}

// Handle runs one inbox request through steps and returns the answer it earns
// (gt-v3nzkf). The apply kind applies only the plan value Verify checked, and
// the rollback kind undoes the plan only on that same verified record
// (gt-iiccy3). Every other kind is refused here.
func Handle(ctx context.Context, steps Steps, req Request, now time.Time) Answer {
	answer := Answer{ID: req.ID, At: now}
	switch req.Kind {
	case KindPlan:
		plan, err := steps.Plan(ctx, req.Plan)
		if err != nil {
			return answerFailed(answer, detailPlanFailed)
		}
		proposal, id, err := clusterapproval.NewProposal(plan)
		if err != nil {
			return answerRefused(answer, err)
		}
		if err := steps.WriteProposal(ctx, req.Plan, proposal, id); err != nil {
			return answerFailed(answer, detailWriteFailed)
		}
		answer.Status = StatusProposed
		answer.PlanSHA256 = plan.SHA256
		answer.Detail = fmt.Sprintf("proposal %s awaits approval", id)
		return answer
	case KindApply:
		return handleApply(ctx, steps, req, answer, now)
	case KindRollback:
		return handleRollback(ctx, steps, req, answer, now)
	default:
		return answerRefused(answer, fmt.Errorf("the kind %q is not handled", req.Kind))
	}
}

// handleApply verifies the approval, then hands Apply the plan and live versions
// Verify checked, so what Apply applies is what Verify checked (gt-sj8wdq).
func handleApply(ctx context.Context, steps Steps, req Request, answer Answer, now time.Time) Answer {
	plan, live, answer, ok := verifyApproval(ctx, steps, req, answer, now)
	if !ok {
		return answer
	}
	if err := steps.Apply(ctx, req.Plan, plan, live); err != nil {
		if errors.Is(err, ErrApplyRefused) {
			return answerRefused(answer, ErrApplyRefused)
		}
		return answerFailed(answer, detailApplyFailed)
	}
	answer.Status = StatusApplied
	answer.PlanSHA256 = plan.SHA256
	return answer
}

// handleRollback runs the same approval check as handleApply and undoes the
// plan only when that check passes. Handle reaches it for a rollback request
// alone, so a failed apply never leads to a rollback; the rollback is never
// automatic (gt-iiccy3).
func handleRollback(ctx context.Context, steps Steps, req Request, answer Answer, now time.Time) Answer {
	plan, _, answer, ok := verifyApproval(ctx, steps, req, answer, now)
	if !ok {
		return answer
	}
	if err := steps.Rollback(ctx, req.Plan, plan); err != nil {
		return answerFailed(answer, detailRollbackFailed)
	}
	answer.Status = StatusRolledBack
	answer.PlanSHA256 = plan.SHA256
	return answer
}

// verifyApproval plans once, reads the approval record for that plan, and
// verifies it against the plan and the live versions. It returns the plan and
// the live versions Verify checked, and true; or, when a step or check refuses
// or fails, the answer to send and false. Apply and Rollback both gate on it, so
// the two kinds cannot drift apart (gt-sj8wdq, gt-iiccy3).
func verifyApproval(ctx context.Context, steps Steps, req Request, answer Answer, now time.Time) (clusterapproval.Plan, map[string]string, Answer, bool) {
	plan, err := steps.Plan(ctx, req.Plan)
	if err != nil {
		return clusterapproval.Plan{}, nil, answerFailed(answer, detailPlanFailed), false
	}
	_, id, err := clusterapproval.NewProposal(plan)
	if err != nil {
		return clusterapproval.Plan{}, nil, answerRefused(answer, err), false
	}
	record, err := steps.ReadRecord(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNoApproval) {
			// ErrNoApproval's own text, never the callee's wrapping of it.
			return clusterapproval.Plan{}, nil, answerRefused(answer, fmt.Errorf("the plan %q is not approved yet: %w", req.Plan, ErrNoApproval)), false
		}
		return clusterapproval.Plan{}, nil, answerFailed(answer, detailRecordFailed), false
	}
	live, err := steps.LiveVersions(ctx, plan)
	if err != nil {
		return clusterapproval.Plan{}, nil, answerFailed(answer, detailLiveFailed), false
	}
	if err := clusterapproval.Verify(record, plan, live, now); err != nil {
		return clusterapproval.Plan{}, nil, answerRefused(answer, err), false
	}
	return plan, live, answer, true
}

// The detail of a failed step is fixed text naming the step, never the step's
// own error: the answer file sits in a directory others read, and a callee's
// error can carry what it was handling (a kubectl stderr, a path, a token).
const (
	detailPlanFailed     = "the plan step failed"
	detailWriteFailed    = "writing the proposal failed"
	detailRecordFailed   = "reading the approval record failed"
	detailLiveFailed     = "reading the live versions failed"
	detailApplyFailed    = "the apply failed"
	detailRollbackFailed = "the rollback failed"
)

// answerFailed records that a step errored and nothing further ran.
func answerFailed(answer Answer, detail string) Answer {
	answer.Status = StatusFailed
	answer.Detail = detail
	return answer
}

// answerRefused records that a check refused the request before any write.
func answerRefused(answer Answer, cause error) Answer {
	answer.Status = StatusRefused
	answer.Detail = cause.Error()
	return answer
}
