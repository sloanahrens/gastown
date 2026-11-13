package landworker

import (
	"errors"
	"fmt"
	"time"
)

// CommitVerdictState is what a rig's durable main state says about one commit's
// post-landing verdict.
type CommitVerdictState int

// The states. Unknown is the zero value: a commit with no verdict recorded and
// none pending is not a green one.
const (
	// CommitVerdictUnknown: no verdict is recorded for the commit and none is pending.
	CommitVerdictUnknown CommitVerdictState = iota
	// CommitVerdictPending: the verdict has been started and not yet decided.
	CommitVerdictPending
	// CommitVerdictGreen: the last post-landing run at the commit passed.
	CommitVerdictGreen
	// CommitVerdictRed: the last post-landing run at the commit failed.
	CommitVerdictRed
)

// CommitVerdict is the post-landing verdict of one commit. Since is when a
// pending verdict started pending; it is zero for every other state.
type CommitVerdict struct {
	State CommitVerdictState
	Since time.Time
}

// VerdictOf reads the post-landing verdict of commit from the rig's durable main
// state, the store the attention collector reads too. It writes nothing. A store
// that cannot be read is an error, never a verdict.
func (p *PostLandRunner) VerdictOf(commit string) (CommitVerdict, error) {
	if p.State == nil {
		return CommitVerdict{}, errors.New("no main state store is wired")
	}
	st, err := p.State.Load()
	if err != nil {
		return CommitVerdict{}, fmt.Errorf("reading the main state: %w", err)
	}
	return commitVerdictIn(st, commit), nil
}

// commitVerdictIn decides the verdict of commit from one read of the main state.
// A pending entry wins: a commit still on the pending list has not been decided,
// even when an earlier run reached a verdict at it. Otherwise LastRun names the
// newest commit a run reached a verdict at, and LastGreen tells that verdict's
// colour.
func commitVerdictIn(st MainState, commit string) CommitVerdict {
	if commit == "" {
		return CommitVerdict{}
	}
	var since time.Time
	pending := false
	for _, e := range st.PostLandPending {
		if e.Land.Commit != commit {
			continue
		}
		if !pending || e.Since.Before(since) {
			since = e.Since
		}
		pending = true
	}
	switch {
	case pending:
		return CommitVerdict{State: CommitVerdictPending, Since: since}
	case st.LastRun != commit:
		return CommitVerdict{}
	case st.LastGreen == commit:
		return CommitVerdict{State: CommitVerdictGreen}
	default:
		return CommitVerdict{State: CommitVerdictRed}
	}
}
