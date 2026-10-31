// Package keycheck decides, before a dispatch, whether a workspace's model
// provider key still works (plan row K3, gt-thrtt).
//
// Check reads the workspace's current key from the key store, asks a Provider
// to verify it, and turns the answer into a Decision: dispatch, or do not
// dispatch and move the workspace to paused. It changes no state — moving the
// workspace to paused, and putting the reason on the beads already in flight,
// is the caller's work.
//
// # One check per dispatch tick
//
// One provider call per bead would ask the same question once for every bead a
// tick dispatches to a workspace, so the check belongs to the tick instead of
// the bead. The caller creates one Tick when a dispatch tick starts and passes
// that same Tick to every Check the tick makes:
//
//	tick := keycheck.NewTick()
//	for _, bead := range due {
//		d, err := keycheck.Check(ctx, store, provider, bead.Workspace, tick)
//		...
//	}
//
// The first Check for a workspace verifies its key and records the result;
// every later Check for that workspace with the same Tick returns that result
// without calling the provider again. The result is reused whatever it is, an
// error included, so a key store that fails once in a tick is read once and a
// retry is the next tick's. A new tick passes a new Tick, so a key that looked
// valid in the last tick is verified again.
//
// One result is not recorded: the error a Check returns because its own
// caller's context ended. That says the caller gave up, not that anything is
// wrong with the key, so another Check for the workspace in the same tick with
// a live context runs the check again instead of inheriting the cancellation.
//
// # A refusal pauses; a failed check does not
//
// A Provider returns ErrRefused, wrapped, when the model provider rejects the
// key. Any other Verify error means the check could not be made — a network
// error, a timeout, a provider outage — and is not a refusal: pausing a
// workspace over the caller's own network trouble would be wrong, so the
// Decision dispatches with Warning set to say the key was not verified and the
// caller decides whether to go ahead. Check returns an error, and no Decision,
// when it cannot make the check: a nil Tick, store or Provider, an empty
// workspace, a context that has already ended, a context that ends while the
// check runs (the caller's own cancellation, returned as ctx.Err()), or the key
// store's own error (a workspace with no enabled key revision is
// keystore.ErrNotFound). No other outcome is an error.
//
// # No key ever leaves
//
// A refusal carries no part of the key: the Decision states ReasonPausedKey and
// nothing about the key itself. A transient error dispatches with the fixed
// WarningUnverified rather than the provider's text, so no part of a key can
// reach a Decision or an error Check returns, whatever a Provider put in its
// own errors.
//
// # Callers
//
// Nothing calls this package yet. Its callers are the dispatcher wiring and key
// rotation (plan rows K5 and the wiring bead): the dispatcher asks Check before
// it dispatches to a workspace, and rotation asks it after it puts a new key
// revision. The tests use the in-memory keystore.Fake and a fake Provider, so
// nothing here makes a provider or cloud call.
package keycheck

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/steveyegge/gastown/internal/keystore"
)

// KeyName is the name of the workspace's model provider key in the key store:
// the key Check reads and verifies.
const KeyName = "api-key"

// ReasonPausedKey is the Reason on a Decision that refuses dispatch, for the
// caller to record when it moves the workspace to paused.
const ReasonPausedKey = "paused: key"

// WarningUnverified is the Warning on a Decision that dispatches although the
// key was not verified, because the provider could not be asked, for the caller
// to record or log. It names the rule rather than the provider's error, which
// could carry part of the key.
const WarningUnverified = "provider check failed; dispatching"

// ErrRefused is what a Provider returns, wrapped, when the model provider
// rejects the key: revoked, expired, or otherwise no longer accepted. Any other
// error from Verify is a failed check, not a refusal.
var ErrRefused = errors.New("keycheck: provider refused the key")

var (
	// ErrNoTick reports a Check with no Tick. Without one the key would be
	// verified once per bead, the cost this package exists to avoid.
	ErrNoTick = errors.New("keycheck: check without a tick")

	// ErrNoStore reports a Check with no key store to read.
	ErrNoStore = errors.New("keycheck: check without a key store")

	// ErrNoProvider reports a Check with no provider to verify with.
	ErrNoProvider = errors.New("keycheck: check without a provider")

	// ErrNoWorkspace reports a Check with no workspace: there is no key to
	// read and nothing to pause.
	ErrNoWorkspace = errors.New("keycheck: check without a workspace")
)

// Provider asks the model provider whether a key still works. Verify returns
// nil when the provider accepts the key, an error wrapping ErrRefused when the
// provider rejects it, and any other error when the check could not be made.
// An error must not contain the key: Check never copies that text out, but it
// cannot clean an error a Provider also logs itself.
type Provider interface {
	Verify(ctx context.Context, key keystore.Secret) error
}

// Decision is what Check decided for one workspace: whether the dispatch may go
// ahead, and when it may not, the reason and the workspace to pause.
type Decision struct {
	// Dispatch is true when the dispatch may proceed. It is false only on a
	// refusal, so a caller reads Reason before it dispatches.
	Dispatch bool

	// Reason is ReasonPausedKey when Dispatch is false, and empty otherwise.
	Reason string

	// Workspace is the workspace to move to paused when Dispatch is false, and
	// empty otherwise.
	Workspace string

	// Warning is set when Dispatch is true but the key went unverified because
	// the provider could not be asked; the caller decides whether to dispatch
	// anyway. It is empty when the key was verified.
	Warning string
}

// Tick scopes the one-check-per-workspace rule to one dispatch tick. Create one
// with NewTick when a tick starts and pass it to every Check of that tick; the
// next tick gets a new one.
type Tick struct {
	mu      sync.Mutex
	results map[string]*tickResult
}

// NewTick returns a Tick for one dispatch tick.
func NewTick() *Tick { return &Tick{} }

// tickResult is one workspace's result in a tick. decision, err and canceled
// are written once before done is closed and read only after, so the close
// orders the write before every read of it.
type tickResult struct {
	done     chan struct{}
	decision Decision
	err      error

	// canceled reports that err is the checking caller's own cancellation
	// rather than an answer about the key. Such a result is dropped from the
	// tick, and a later caller with a live context runs the check itself.
	canceled bool
}

// Check decides whether a dispatch to workspace may go ahead, verifying the key
// once per Tick. A Check whose own context ends returns that context's error and
// leaves nothing in the Tick for the workspace, so a caller with a live context
// checks it again.
func Check(ctx context.Context, store keystore.Store, prov Provider, workspace string, tick *Tick) (Decision, error) {
	if tick == nil {
		return Decision{}, ErrNoTick
	}
	if store == nil {
		return Decision{}, ErrNoStore
	}
	if prov == nil {
		return Decision{}, ErrNoProvider
	}
	if workspace == "" {
		return Decision{}, ErrNoWorkspace
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	return tick.check(ctx, store, prov, workspace)
}

// check returns the workspace's result for this tick, running the check the
// first time. A second call waits for a check already running rather than
// verifying the key twice; it runs its own only when the check it waited for
// was abandoned by a canceled caller.
func (t *Tick) check(ctx context.Context, store keystore.Store, prov Provider, workspace string) (Decision, error) {
	for {
		t.mu.Lock()
		if t.results == nil {
			t.results = make(map[string]*tickResult)
		}
		res := t.results[workspace]
		if res == nil {
			res = &tickResult{done: make(chan struct{})}
			t.results[workspace] = res
			t.mu.Unlock()
			res.decision, res.err = evaluate(ctx, store, prov, workspace)
			if res.err != nil && ctx.Err() != nil {
				// This caller gave up, so its error says nothing about the
				// key. Drop the entry instead of handing the cancellation to
				// the rest of the tick.
				res.canceled = true
				t.mu.Lock()
				if t.results[workspace] == res {
					delete(t.results, workspace)
				}
				t.mu.Unlock()
			}
			close(res.done)
			return res.decision, res.err
		}
		t.mu.Unlock()

		<-res.done
		if res.canceled && ctx.Err() == nil {
			continue
		}
		return res.decision, res.err
	}
}

// evaluate reads the workspace's key and asks the provider about it.
func evaluate(ctx context.Context, store keystore.Store, prov Provider, workspace string) (Decision, error) {
	key, err := store.Get(ctx, workspace, KeyName)
	if err != nil {
		return Decision{}, fmt.Errorf("keycheck: reading the key for workspace %q: %w", workspace, err)
	}
	if err := prov.Verify(ctx, key); err != nil {
		if errors.Is(err, ErrRefused) {
			return Decision{Reason: ReasonPausedKey, Workspace: workspace}, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Decision{}, ctxErr
		}
		return Decision{Dispatch: true, Warning: WarningUnverified}, nil
	}
	return Decision{Dispatch: true}, nil
}
