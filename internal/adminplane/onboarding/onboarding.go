// Package onboarding is the hosted platform's onboarding order as a state
// machine (plan row O5, gt-yx24m): the steps a new customer takes, the one order
// they may be taken in, and the small state that records how far they got.
//
// Nothing calls it yet. Its caller is the onboarding console, a later bead. The
// package holds no HTTP and no database; its one I/O is the model-key step's
// store, which the caller supplies.
package onboarding

import (
	"errors"
	"fmt"
)

// Step is one step of onboarding.
type Step string

// The steps, in the only order they may be taken. Every step but GCP is
// required; GCP is the one a customer may leave for later.
const (
	SignIn       Step = "sign-in"
	OrgWorkspace Step = "organization-and-workspace"
	ModelKey     Step = "model-key"
	Repository   Step = "repository"
	FirstProject Step = "first-project"
	GCP          Step = "gcp"
)

// order is the whole rule: a step starts only once the step before it in this
// list is done. A step named in no position is refused, never allowed by
// default.
var order = []Step{SignIn, OrgWorkspace, ModelKey, Repository, FirstProject, GCP}

// set is the set of steps a State has reached. The zero value holds none.
type set map[Step]bool

// has reports whether s holds step.
func (s set) has(step Step) bool { return s[step] }

// with returns s plus step in a new map, so the State with got keeps the map it
// already had: Advance hands back a new State, not a changed one.
func (s set) with(step Step) set {
	out := make(set, len(s)+1)
	for k := range s {
		out[k] = true
	}
	out[step] = true
	return out
}

// State is where an onboarding stands: the steps done and the steps skipped.
// The zero value is nobody having done anything. The sets behind it are private,
// so a caller reads a state through HasDone and HasSkipped and changes it only
// by handing it to Advance or Skip.
type State struct {
	done    set
	skipped set
}

// HasDone reports whether the onboarding has completed step.
func (s State) HasDone(step Step) bool { return s.done.has(step) }

// HasSkipped reports whether the onboarding left step for later.
func (s State) HasSkipped(step Step) bool { return s.skipped.has(step) }

// StoreKey writes the workspace's model key to the key store of plan row K2
// (keystore.Store's Put); a non-nil return means the key was not stored. Advance
// calls it for the model-key step alone, so the caller binds it to the
// workspace and value it is onboarding; a nil StoreKey is refused there.
type StoreKey func() error

var (
	// ErrUnknownStep refuses a step this package does not define, so a caller's
	// typo is refused rather than treated as a step no rule mentions.
	ErrUnknownStep = errors.New("onboarding: unknown step")
	// ErrNoKeyStore refuses the model-key step without the store that writes the
	// key: the step completes only through it, so with none it would complete
	// having stored nothing.
	ErrNoKeyStore = errors.New("onboarding: the model-key step needs a key store")
)

// OrderError is Advance's refusal of a step whose predecessor is not done. It
// names the step asked for and the step it needs, so the refusal reads without
// the request that caused it.
type OrderError struct {
	Step  Step
	Needs Step
}

func (e *OrderError) Error() string {
	return fmt.Sprintf("onboarding: step %q needs step %q done first", e.Step, e.Needs)
}

// SettledError refuses a step the onboarding has reached already: a done step is
// not done again, and a skipped step is not started.
type SettledError struct {
	Step    Step
	Skipped bool
}

func (e *SettledError) Error() string {
	if e.Skipped {
		return fmt.Sprintf("onboarding: step %q was skipped", e.Step)
	}
	return fmt.Sprintf("onboarding: step %q is already done", e.Step)
}

// NotSkippableError is Skip's refusal: GCP is the one step a customer may leave
// for later.
type NotSkippableError struct{ Step Step }

func (e *NotSkippableError) Error() string {
	return fmt.Sprintf("onboarding: step %q may not be skipped; only %q may", e.Step, GCP)
}

// StoreKeyError is Advance's report that the key store did not store the model
// key, which leaves the model-key step undone so the caller retries it. Unwrap
// reaches the store's own error; the message names the step alone, so the
// store's text, which may carry the key, never reaches a log.
type StoreKeyError struct{ Err error }

func (e *StoreKeyError) Error() string {
	return fmt.Sprintf("onboarding: step %q did not store the model key", ModelKey)
}

func (e *StoreKeyError) Unwrap() error { return e.Err }

// Advance completes step to and returns the State that records it. A step starts
// only once the one before it is done, so a step asked for out of order returns
// s unchanged and an *OrderError. A done or skipped step returns a
// *SettledError, and a step this package does not define returns an error
// wrapping ErrUnknownStep.
//
// The model-key step completes only through storeKey, so a store error returns s
// unchanged and a *StoreKeyError and the step stays undone. storeKey is unused
// for every other step.
func Advance(s State, to Step, storeKey StoreKey) (State, error) {
	i, ok := position(to)
	if !ok {
		return s, fmt.Errorf("%w: %q", ErrUnknownStep, to)
	}
	if s.skipped.has(to) {
		return s, &SettledError{Step: to, Skipped: true}
	}
	if s.done.has(to) {
		return s, &SettledError{Step: to}
	}
	if i > 0 {
		if needs := order[i-1]; !s.done.has(needs) {
			return s, &OrderError{Step: to, Needs: needs}
		}
	}
	if to == ModelKey {
		if storeKey == nil {
			return s, ErrNoKeyStore
		}
		if err := storeKey(); err != nil {
			return s, &StoreKeyError{Err: err}
		}
	}
	return State{done: s.done.with(to), skipped: s.skipped}, nil
}

// Skip records step as left for later and returns the State that records it.
// Skipping does not start the step, so it needs no predecessor done; Skip
// refuses every step but GCP with a *NotSkippableError, a done step with a
// *SettledError, and a step this package does not define with an error wrapping
// ErrUnknownStep. Skipping an already skipped step returns s unchanged and no
// error, since the state it asks for already holds.
func Skip(s State, step Step) (State, error) {
	if _, ok := position(step); !ok {
		return s, fmt.Errorf("%w: %q", ErrUnknownStep, step)
	}
	if step != GCP {
		return s, &NotSkippableError{Step: step}
	}
	if s.done.has(step) {
		return s, &SettledError{Step: step}
	}
	if s.skipped.has(step) {
		return s, nil
	}
	return State{done: s.done, skipped: s.skipped.with(step)}, nil
}

// position returns step's place in order, and false for a step this package
// does not define.
func position(step Step) (int, bool) {
	for i, s := range order {
		if s == step {
			return i, true
		}
	}
	return 0, false
}
