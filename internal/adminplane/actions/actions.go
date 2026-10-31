// Package actions is the admin plane's logged workspace state actions (plan row
// V4): Pause and Suspend move a workspace through the registry state machine
// and leave one action-log entry naming the actor who asked.
//
// Nothing calls it yet. Its callers are the console's state handlers and the
// owner's pause and resume (plan rows O6 and the later wiring bead), which
// supply a Store over the admin plane's Postgres table and an ActionLog that
// stores the entry. The package holds no database and no HTTP, so a move and
// its entry are testable without either.
package actions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
)

// ErrNoStore is a move's refusal of Deps with no Store: without one there is no
// workspace to load and nothing to save.
var ErrNoStore = errors.New("actions: Deps.Store is required")

// ErrNoLog is a move's refusal of Deps with no ActionLog: every move leaves an
// entry, so a move that could not leave one would change state unrecorded.
var ErrNoLog = errors.New("actions: Deps.Log is required")

// ErrLogNotWritten marks a move whose save succeeded and whose entry did not, so
// a caller can tell it from a move that changed nothing and retry only the
// append (the returned *LogFailure carries the entry).
var ErrLogNotWritten = errors.New("actions: workspace saved but its action log entry was not written")

// ActionEntry is one logged state action: the workspace that moved, the actor
// that asked, the state it left and entered, and when.
type ActionEntry struct {
	Actor     string
	Workspace string
	From      registry.State
	To        registry.State
	At        time.Time
}

// Store is where a move reads and writes the workspace. The admin plane's
// implementation is its Postgres table (plan row V1b); a caller retrying a
// failed log must not save again, because the state already moved.
type Store interface {
	// Load returns the workspace with the given id. An error means the move
	// cannot proceed, so no error may read as an empty workspace.
	Load(ctx context.Context, id string) (registry.Workspace, error)
	// Save stores w in its new state, replacing what Load returned.
	Save(ctx context.Context, w registry.Workspace) error
}

// ActionLog is where a move records its entry. The admin plane's implementation
// stores the action log (plan row V1b).
type ActionLog interface {
	// Append stores one entry. It is the move's last write, so its failure
	// leaves the state changed and the log short; the caller retries it.
	Append(ctx context.Context, e ActionEntry) error
}

// Deps is what a move needs: where the workspace lives, where the entry goes,
// and the clock the entry is stamped with.
type Deps struct {
	Store Store
	Log   ActionLog
	Clock registry.Clock
}

// Pause moves workspace ws to Paused on behalf of actor and logs the move.
// actor is a role such as "operator", or an owner id, never a person's name.
//
// A move the rules refuse returns the registry's *registry.TransitionError and
// writes neither a save nor an entry.
func Pause(ctx context.Context, d Deps, ws, actor string) error {
	return perform(ctx, d, ws, actor, registry.Paused)
}

// Suspend moves workspace ws to Suspended on behalf of actor and logs the move.
// actor is a role such as "operator", or an owner id, never a person's name.
//
// A move the rules refuse returns the registry's *registry.TransitionError and
// writes neither a save nor an entry.
func Suspend(ctx context.Context, d Deps, ws, actor string) error {
	return perform(ctx, d, ws, actor, registry.Suspended)
}

// perform is Pause and Suspend: load, transition, save, log. The save comes before
// the append, and an append failure is reported as *LogFailure rather than
// retried here, so the caller knows the state changed and can retry only the
// entry.
func perform(ctx context.Context, d Deps, ws, actor string, to registry.State) error {
	if d.Store == nil {
		return ErrNoStore
	}
	if d.Log == nil {
		return ErrNoLog
	}
	w, err := d.Store.Load(ctx, ws)
	if err != nil {
		return fmt.Errorf("load workspace %q: %w", ws, err)
	}
	next, ev, err := registry.Transition(d.Clock, w, to, actor)
	if err != nil {
		return err
	}
	if err := d.Store.Save(ctx, next); err != nil {
		return fmt.Errorf("save workspace %q as %q: %w", ws, to, err)
	}
	entry := ActionEntry{Actor: ev.Actor, Workspace: ev.WorkspaceID, From: ev.From, To: ev.To, At: ev.At}
	if err := d.Log.Append(ctx, entry); err != nil {
		return &LogFailure{Action: entry, Err: err}
	}
	return nil
}

// LogFailure is a move whose save succeeded and whose entry did not. The
// workspace is already in Action.To, so the caller retries only the append,
// with Action, and must not run the move again.
type LogFailure struct {
	Action ActionEntry
	Err    error
}

func (e *LogFailure) Error() string {
	return fmt.Sprintf("actions: workspace %q is now %q but its action log entry was not written: %v",
		e.Action.Workspace, e.Action.To, e.Err)
}

// Unwrap returns the Append error, so errors.Is and errors.As reach it.
func (e *LogFailure) Unwrap() error { return e.Err }

// Is reports e as ErrLogNotWritten, so a caller that only needs to know the
// entry is missing can test one sentinel.
func (e *LogFailure) Is(target error) bool { return target == ErrLogNotWritten }
