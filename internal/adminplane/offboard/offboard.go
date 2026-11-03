// Package offboard is the schedule a closed workspace follows as a pure
// function: its keys are deleted when it closes, a customer export is offered
// for a 30-day window, and its data is deleted when the window ends (decision
// claude-29a.11).
//
// Nothing calls it yet. Its caller is a later close-workspace job, which records
// each step as it completes. Deletion itself is a cloud step that needs the
// cloud-change review answers the hosted-platform plan requires; this package
// performs nothing.
package offboard

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// Kind is one kind of resource a hosted workspace holds.
type Kind string

// The resource kinds an Inventory may list. A kind missing here is refused by
// Plan rather than kept or deleted by default.
const (
	Repos    Kind = "repos"
	Tracker  Kind = "tracker"
	Keys     Kind = "keys"
	Caches   Kind = "caches"
	NodePool Kind = "node-pool"
	Town     Kind = "town"
)

// kindRank is the fixed order Plan sorts steps with one due time by, which is
// the order the decision lists the kinds (claude-29a.11). A kind absent from it
// is not a kind this package knows.
var kindRank = map[Kind]int{
	Repos:    0,
	Tracker:  1,
	Keys:     2,
	Caches:   3,
	NodePool: 4,
	Town:     5,
}

// Action is what the schedule does to one resource kind.
type Action string

// The actions a Step may name.
const (
	DeleteKeys  Action = "delete-keys"
	OfferExport Action = "offer-export"
	DeleteData  Action = "delete-data"
)

// retention is the export window the operator decided on 2026-10-31: a closed
// workspace's data is kept, and its export offered, for 30 days after the close
// (claude-29a.11).
const retention = 30 * 24 * time.Hour

// Step is one scheduled action on one resource kind. Ends is when a windowed
// action is no longer offered, set only on the export offer; the zero time marks
// every other action, which has no window. A Step carries no resource contents,
// secret ids or key material.
type Step struct {
	Action Action
	Kind   Kind
	Due    time.Time
	Ends   time.Time
}

// Inventory lists the resource kinds the workspace holds. Plan refuses a kind it
// does not know and schedules a kind listed twice once.
type Inventory struct {
	Kinds []Kind
}

// ErrUnknownKind is Plan's refusal of a resource kind the schedule does not
// know: the kind is neither kept nor deleted by default.
var ErrUnknownKind = errors.New("offboard: unknown resource kind")

// Plan returns the schedule of a workspace closed at closedAt, ordered by due
// time and then by the fixed kind order. Every kind in inv is validated before
// any step is built, so an unknown kind returns no plan and an error wrapping
// ErrUnknownKind.
//
// The keys are due at the close. The customer export is one offer, made of the
// repos' git bundles and the tracker dump, so it names the repos when there are
// any and the tracker otherwise; it is due at the close and ends when the
// retention window does. Every other kind is due for deletion when that window
// ends.
func Plan(closedAt time.Time, inv Inventory) ([]Step, error) {
	kinds := make([]Kind, 0, len(inv.Kinds))
	seen := make(map[Kind]bool, len(inv.Kinds))
	for _, kind := range inv.Kinds {
		if _, ok := kindRank[kind]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
		}
		if seen[kind] {
			continue
		}
		seen[kind] = true
		kinds = append(kinds, kind)
	}

	ends := closedAt.Add(retention)
	export, hasExport := exportKind(kinds)
	steps := make([]Step, 0, len(kinds)+1)
	for _, kind := range kinds {
		if kind == Keys {
			steps = append(steps, Step{Action: DeleteKeys, Kind: kind, Due: closedAt})
			continue
		}
		if hasExport && kind == export {
			steps = append(steps, Step{Action: OfferExport, Kind: kind, Due: closedAt, Ends: ends})
		}
		steps = append(steps, Step{Action: DeleteData, Kind: kind, Due: ends})
	}
	// SliceStable keeps steps that compare equal in the order they were appended,
	// so the plan stays deterministic if a later step ever shares a due time and
	// kind rather than depending on the sort key never tying (gt-cswgv).
	sort.SliceStable(steps, func(i, j int) bool {
		if !steps[i].Due.Equal(steps[j].Due) {
			return steps[i].Due.Before(steps[j].Due)
		}
		return kindRank[steps[i].Kind] < kindRank[steps[j].Kind]
	})
	return steps, nil
}

// Due returns the steps of plan that are due at now, in plan order: the steps
// whose Due is at or before now, less a windowed step whose Ends has arrived. The
// caller records each step it completes; Due keeps no record, so the same now
// returns the same steps.
func Due(now time.Time, plan []Step) []Step {
	var due []Step
	for _, step := range plan {
		if step.Due.After(now) {
			continue
		}
		if !step.Ends.IsZero() && !now.Before(step.Ends) {
			continue
		}
		due = append(due, step)
	}
	return due
}

// exportKind returns the kind the export offer names: the repos, whose bundles
// the export is made of, or the tracker when a workspace has no repos. ok is
// false when the workspace holds neither.
func exportKind(kinds []Kind) (kind Kind, ok bool) {
	for _, candidate := range []Kind{Repos, Tracker} {
		if slices.Contains(kinds, candidate) {
			return candidate, true
		}
	}
	return "", false
}
