package forgejoprobe

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// callTimeout bounds one read of the Forgejo API, so a hung server cannot hold
// the probe past its own deadline.
const callTimeout = 30 * time.Second

// Statuses is the part of the Forgejo API the probe reads. forgejo.Client
// satisfies it; tests put an in-process fake behind it.
type Statuses interface {
	CombinedStatus(ctx context.Context, owner, repo, ref string) (*forgejo.CombinedStatus, error)
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
	ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]forgejo.ActionRunJob, error)
	JobLogsTail(ctx context.Context, owner, repo string, jobID, maxBytes int64) (string, error)
}

var _ Statuses = (*forgejo.Client)(nil)

// Clock is the probe's time source. A fake advances on Sleep, so a poll loop
// runs without waiting in tests.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// Outcome is how a Wait ended.
type Outcome int

const (
	// Green means the context reported success.
	Green Outcome = iota
	// Red means the context reported a state other than success or pending.
	Red
	// TimedOut means the context reported nothing settled before the deadline.
	TimedOut
	// Unreadable means the combined status could not be read.
	Unreadable
)

// Verdict is how a Wait ended. State is set for Red and Err for Unreadable.
type Verdict struct {
	Outcome Outcome
	State   forgejo.CommitState
	Err     error
}

// Line renders the verdict as the probe prints it on stderr.
func (v Verdict) Line(rig, contextName, sha string, timeout time.Duration) string {
	switch v.Outcome {
	case Green:
		return fmt.Sprintf("green %s's gate is green on %s (%s)", rig, sha, contextName)
	case TimedOut:
		return fmt.Sprintf("timeout %s reported nothing on %s within %ds", contextName, sha, int(timeout.Seconds()))
	case Red:
		return fmt.Sprintf("other %s is %s on %s", contextName, v.State, sha)
	default:
		return fmt.Sprintf("other %s is unreadable (%v) on %s", contextName, v.Err, sha)
	}
}

// Wait polls the combined status of sha until contextName settles, or until
// timeout passes, and returns how it ended. It reads at once, then every poll.
func Wait(ctx context.Context, st Statuses, c Clock, owner, repo, sha, contextName string, timeout, poll time.Duration) Verdict {
	deadline := c.Now().Add(timeout)
	for {
		callCtx, cancel := context.WithDeadline(ctx, c.Now().Add(callTimeout))
		cs, err := st.CombinedStatus(callCtx, owner, repo, sha)
		cancel()
		if err != nil {
			return Verdict{Outcome: Unreadable, Err: err}
		}
		// The newest status wins: a re-probe of an unchanged main leaves an
		// older status on the same context, and that one must not decide.
		if s := newestStatus(cs, contextName); s != nil {
			switch s.Status {
			case forgejo.StateSuccess:
				return Verdict{Outcome: Green}
			case forgejo.StatePending:
			default:
				return Verdict{Outcome: Red, State: s.Status}
			}
		}
		now := c.Now()
		if !now.Before(deadline) {
			return Verdict{Outcome: TimedOut}
		}
		c.Sleep(min(deadline.Sub(now), poll))
	}
}

// newestStatus returns the status on contextName with the greatest id, or nil.
func newestStatus(cs *forgejo.CombinedStatus, contextName string) *forgejo.CommitStatus {
	if cs == nil {
		return nil
	}
	var best *forgejo.CommitStatus
	for i := range cs.Statuses {
		s := &cs.Statuses[i]
		if s.Context != contextName {
			continue
		}
		if best == nil || s.ID > best.ID {
			best = s
		}
	}
	return best
}
