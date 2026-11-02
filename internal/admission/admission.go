// Package admission decides whether a hosted workspace may start a new
// dispatch, combining the workspace state, the day's spend cap, the dispatch
// rate limiter and the model provider key check in one fixed order (gt-49t42).
//
// Admit stops at the first step that refuses and reports the registry state a
// caller should move the workspace to and the reason to record; it changes no
// workspace state itself. The order is part of the contract: the key check,
// which can call a provider, runs only when every earlier step admitted.
//
// Its caller is the dispatcher wiring in the spec dispatch tick, a later bead
// the overseer owns because that tick is a hot path. Customer zero's single
// workspace admits through the same code.
package admission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/abuse"
	"github.com/steveyegge/gastown/internal/adminplane/registry"
	"github.com/steveyegge/gastown/internal/keycheck"
	"github.com/steveyegge/gastown/internal/keystore"
	"github.com/steveyegge/gastown/internal/spendcap"
)

// The reasons a Decision carries for a refused dispatch. A key refusal carries
// keycheck.ReasonPausedKey instead, and its state move is registry.Paused.
const (
	ReasonNoWorkspace    = "input: no workspace"
	ReasonNoState        = "input: no state source"
	ReasonNoSpend        = "input: no spend source"
	ReasonNoLimiter      = "input: no limiter"
	ReasonNoClock        = "input: no clock"
	ReasonCanceled       = "input: context ended"
	ReasonStateError     = "state: unreadable"
	ReasonStatePaused    = "state: paused"
	ReasonStateSuspended = "state: suspended"
	ReasonStateClosed    = "state: closed"
	ReasonStateUnknown   = "state: unknown"
	ReasonSpendError     = "spend: unreadable"
	ReasonSpendForeign   = "spend: cap names another workspace"
	ReasonSpendAt        = "spend: at cap"
	ReasonSpendOver      = "spend: over cap"
	ReasonSpendUnknown   = "spend: unknown"
	ReasonRate           = "rate: limited"
	ReasonKeyError       = "key: check failed"
)

var (
	// ErrNoWorkspace reports Admit with an empty workspace: there is no
	// workspace to check or name in a Decision.
	ErrNoWorkspace = errors.New("admission: admit without a workspace")

	// ErrNoState reports a Deps with no state source.
	ErrNoState = errors.New("admission: admit without a state source")

	// ErrNoSpend reports a Deps with no spend source.
	ErrNoSpend = errors.New("admission: admit without a spend source")

	// ErrNoLimiter reports a Deps with no rate limiter.
	ErrNoLimiter = errors.New("admission: admit without a limiter")

	// ErrNoClock reports a Deps with no clock: the spend day cannot be judged.
	ErrNoClock = errors.New("admission: admit without a clock")

	// ErrCapWorkspace reports a spend source whose cap names a workspace other
	// than the one being admitted. Summing against that cap would read another
	// workspace's spend, so it is refused rather than treated as uncapped.
	ErrCapWorkspace = errors.New("admission: spend cap names another workspace")
)

// StateSource reports the registry state of a workspace.
type StateSource interface {
	// State returns workspace's current state, or an error when it cannot be
	// read.
	State(ctx context.Context, workspace string) (registry.State, error)
}

// SpendSource reports a workspace's usage records and daily spend cap.
type SpendSource interface {
	// Spend returns workspace's usage records and the cap that applies to it.
	// The cap must name workspace; a cap for another workspace is refused.
	Spend(ctx context.Context, workspace string) ([]spendcap.Record, spendcap.Cap, error)
}

// Deps is what Admit reads. A nil source refuses every dispatch rather than
// admitting one. The key store and provider are read only at the last step; a
// typed-nil value (an interface holding a nil pointer) is not caught by the nil
// guard and is passed to keycheck, which documents the same caveat.
type Deps struct {
	// State reports where the workspace stands.
	State StateSource
	// Spend reports the day's usage records and the workspace's cap.
	Spend SpendSource
	// Limiter meters dispatch requests; only one goroutine may use it at a
	// time, as abuse documents.
	Limiter *abuse.Limiter
	// Store is the key store the key check reads from.
	Store keystore.Store
	// Provider asks the model provider whether the key works.
	Provider keycheck.Provider
	// Now is the clock the spend day is judged by.
	Now func() time.Time
}

// Decision is what Admit decided for one workspace.
type Decision struct {
	// Allow reports whether a dispatch may start.
	Allow bool
	// State is the registry state to move the workspace to when a move is
	// needed: registry.Paused after a key refusal, and empty when no move is
	// needed.
	State registry.State
	// Reason names why the dispatch was refused, and is empty when Allow is
	// true.
	Reason string
	// Warning is set when Allow is true but the key went unverified because
	// the provider could not be asked; it is empty when the key was verified.
	Warning string
}

// refusal returns the Decision for a refused dispatch that needs no state move.
func refusal(reason string) Decision { return Decision{Reason: reason} }

// Admit decides whether a dispatch may start in workspace, checking the
// workspace state, the day's spend, the dispatch rate and the key in that
// order. A refusal returns a Decision with Allow false and no error; a source
// that cannot be read refuses and returns the error, so a broken read never
// admits. One tick reuses one key check per workspace, as keycheck documents.
//
// Admit changes no workspace state. The one thing it spends from is the
// limiter's bucket: an admitted dispatch spends a token, and a key refusal
// later in the same call does not refund it.
func Admit(ctx context.Context, deps Deps, workspace string, tick *keycheck.Tick) (Decision, error) {
	switch {
	case workspace == "":
		return refusal(ReasonNoWorkspace), ErrNoWorkspace
	case deps.State == nil:
		return refusal(ReasonNoState), ErrNoState
	case deps.Spend == nil:
		return refusal(ReasonNoSpend), ErrNoSpend
	case deps.Limiter == nil:
		return refusal(ReasonNoLimiter), ErrNoLimiter
	case deps.Now == nil:
		return refusal(ReasonNoClock), ErrNoClock
	}
	if err := ctx.Err(); err != nil {
		return refusal(ReasonCanceled), err
	}

	state, err := deps.State.State(ctx, workspace)
	if err != nil {
		return refusal(ReasonStateError), fmt.Errorf("admission: reading the state of workspace %q: %w", workspace, err)
	}
	switch state {
	case registry.Active:
	case registry.Paused:
		return refusal(ReasonStatePaused), nil
	case registry.Suspended:
		return refusal(ReasonStateSuspended), nil
	case registry.Closed:
		return refusal(ReasonStateClosed), nil
	default:
		// A state this package does not know is refused, never admitted by
		// default: a new registry state must be handled here before it serves.
		return refusal(ReasonStateUnknown), nil
	}

	records, dailyCap, err := deps.Spend.Spend(ctx, workspace)
	if err != nil {
		return refusal(ReasonSpendError), fmt.Errorf("admission: reading the spend of workspace %q: %w", workspace, err)
	}
	if dailyCap.Workspace != workspace {
		return refusal(ReasonSpendForeign), fmt.Errorf("%w: cap names %q, workspace is %q", ErrCapWorkspace, dailyCap.Workspace, workspace)
	}
	level := spendcap.Evaluate(dailyCap, records, deps.Now()).Level
	switch level {
	case spendcap.Under:
	case spendcap.At:
		return refusal(ReasonSpendAt), nil
	case spendcap.Over:
		return refusal(ReasonSpendOver), nil
	default:
		// Unknown, and any level a later spendcap version might add, stops.
		return refusal(ReasonSpendUnknown), nil
	}

	if !deps.Limiter.Allow(abuse.Dispatch, workspace, level).Allowed {
		return refusal(ReasonRate), nil
	}

	key, err := keycheck.Check(ctx, deps.Store, deps.Provider, workspace, tick)
	if err != nil {
		return refusal(ReasonKeyError), fmt.Errorf("admission: checking the key for workspace %q: %w", workspace, err)
	}
	if !key.Dispatch {
		return Decision{State: registry.Paused, Reason: key.Reason, Warning: key.Warning}, nil
	}
	return Decision{Allow: true, Warning: key.Warning}, nil
}
