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
}

// Handle runs one inbox request through steps and returns the answer it earns
// (gt-v3nzkf). The apply kind applies only the plan value Verify checked, and
// the rollback kind is gt-iiccy3, so every kind other than plan and apply is
// refused here until its bead lands.
func Handle(ctx context.Context, steps Steps, req Request, now time.Time) Answer {
	answer := Answer{ID: req.ID, At: now}
	switch req.Kind {
	case KindPlan:
		plan, err := steps.Plan(ctx, req.Plan)
		if err != nil {
			return answerFailed(answer, fmt.Errorf("plan %q: %w", req.Plan, err))
		}
		proposal, id, err := clusterapproval.NewProposal(plan)
		if err != nil {
			return answerRefused(answer, err)
		}
		if err := steps.WriteProposal(ctx, req.Plan, proposal, id); err != nil {
			return answerFailed(answer, fmt.Errorf("write the proposal %s: %w", id, err))
		}
		answer.Status = StatusProposed
		answer.PlanSHA256 = plan.SHA256
		answer.Detail = fmt.Sprintf("proposal %s awaits approval", id)
		return answer
	case KindApply:
		return handleApply(ctx, steps, req, answer, now)
	default:
		return answerRefused(answer, fmt.Errorf("the kind %q is not handled", req.Kind))
	}
}

// handleApply plans once, verifies the approval against that plan and the live
// versions, and hands Apply those same two values, so what Apply applies is
// what Verify checked (gt-sj8wdq).
func handleApply(ctx context.Context, steps Steps, req Request, answer Answer, now time.Time) Answer {
	plan, err := steps.Plan(ctx, req.Plan)
	if err != nil {
		return answerFailed(answer, fmt.Errorf("plan %q: %w", req.Plan, err))
	}
	_, id, err := clusterapproval.NewProposal(plan)
	if err != nil {
		return answerRefused(answer, err)
	}
	record, err := steps.ReadRecord(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNoApproval) {
			return answerRefused(answer, fmt.Errorf("the plan %q is not approved yet: %w", req.Plan, err))
		}
		return answerFailed(answer, fmt.Errorf("read the approval record for proposal %s: %w", id, err))
	}
	live, err := steps.LiveVersions(ctx, plan)
	if err != nil {
		return answerFailed(answer, fmt.Errorf("read the live versions of the plan %q: %w", req.Plan, err))
	}
	if err := clusterapproval.Verify(record, plan, live, now); err != nil {
		return answerRefused(answer, err)
	}
	if err := steps.Apply(ctx, req.Plan, plan, live); err != nil {
		if errors.Is(err, ErrApplyRefused) {
			return answerRefused(answer, err)
		}
		return answerFailed(answer, fmt.Errorf("apply the plan %q: %w", req.Plan, err))
	}
	answer.Status = StatusApplied
	answer.PlanSHA256 = plan.SHA256
	return answer
}

// answerFailed records that a step errored and nothing further ran.
func answerFailed(answer Answer, cause error) Answer {
	answer.Status = StatusFailed
	answer.Detail = cause.Error()
	return answer
}

// answerRefused records that a check refused the request before any write.
func answerRefused(answer Answer, cause error) Answer {
	answer.Status = StatusRefused
	answer.Detail = cause.Error()
	return answer
}
