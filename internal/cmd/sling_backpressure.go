package cmd

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
)

// Merge-queue backpressure (gt-xidg, plan Task 3 / A3).
//
// Dispatch used to be unlimited: `gt sling` spawned a polecat whenever it was
// asked, whatever the rig's merge queue looked like. The queue is what the
// town can actually absorb, and it is the refinery's one lane — so a run of
// slings while the queue was already deep produced polecats whose MRs waited
// behind a backlog, spending GPU and merge-gate capacity on work that could
// not land any sooner.
//
// The knob is `merge_queue.max_ready_for_dispatch` in the target rig's
// settings file (`<rig>/settings/config.json`, where batch_max and the gate
// commands already live); zero, the default, leaves the guard off entirely.
// Above the ceiling a sling is refused with the count, and two things pass
// anyway: a bead labeled `rework` (the town has already paid for that work
// once, and its MR is what the queue is waiting on) and an explicit --force.

// errQueueBackpressure identifies a refusal, so callers and tests can match it
// with errors.Is without depending on the rig name in the message.
var errQueueBackpressure = errors.New("merge queue backpressure")

// queueBackpressureError is the typed refusal. It carries the numbers so a
// caller can report them without parsing the message, and the message is the
// operator-facing contract: it names the rig, the count, the ceiling, and the
// two ways through.
type queueBackpressureError struct {
	Rig   string
	Ready int
	Max   int
}

func (e *queueBackpressureError) Error() string {
	return fmt.Sprintf("sling refused: %s has %d ready MRs (> %d); pass --force or label the bead rework",
		e.Rig, e.Ready, e.Max)
}

func (e *queueBackpressureError) Unwrap() error { return errQueueBackpressure }

// dispatchMRLister is the slice of beads the guard reads. An interface so
// tests can count ready MRs without a Dolt server.
type dispatchMRLister interface {
	ListMergeRequests(opts beads.ListOptions) ([]*beads.Issue, error)
}

// newDispatchMRLister is a var so tests can substitute a fake lister and
// assert whether one was built at all.
var newDispatchMRLister = func(rigPath string) dispatchMRLister {
	return beads.New(rigPath)
}

// checkSlingBackpressure refuses a dispatch while the target rig's merge queue
// is over its configured ready-MR ceiling. It returns nil whenever the guard
// does not apply: no knob configured, --force, a rework bead, or an unreadable
// queue.
//
// The rig directory is derived the way the rig manager derives it (town root
// + rig name) rather than resolved through the manager, because this check
// deliberately runs before the spawn path does any of its work — including
// before the pool decision, which spends a tmux round trip and may claim a
// local seat.
func checkSlingBackpressure(townRoot, rigName string, opts SlingSpawnOptions) error {
	rigPath := filepath.Join(townRoot, rigName)

	maxReady := 0
	settings, err := config.LoadRigSettings(filepath.Join(rigPath, "settings", "config.json"))
	switch {
	case err == nil:
		maxReady = settings.MergeQueue.GetMaxReadyForDispatch()
	case !errors.Is(err, config.ErrNotFound):
		// An unreadable settings file leaves the guard off, like an unset
		// knob. Say so: silently dispatching through a ceiling the operator
		// believes is set is the failure this line exists to prevent.
		style.PrintWarning("could not read %s settings for dispatch backpressure: %v", rigName, err)
	}
	// Guard off, or the operator is overriding deliberately.
	if maxReady <= 0 || opts.Force {
		return nil
	}

	// Rework is not new pressure on the queue: it is the bead whose MR is
	// already in it (or was rejected out of it), and requeueing it is the way
	// the queue drains. An unreadable bead is treated as unlabeled — the
	// sling's own bead validation reports that failure, and the refusal below
	// tells the operator the two ways through.
	if opts.HookBead != "" {
		if bead, err := poolBeadLookup(townRoot, opts.HookBead); err == nil && bead.hasLabel(reworkLabel) {
			return nil
		}
	}

	ready, err := countReadyMergeRequests(newDispatchMRLister(rigPath), rigName)
	if err != nil {
		// Fail open. A queue we cannot read is not evidence of a queue that is
		// full, and refusing on a Dolt hiccup would stop the whole town.
		style.PrintWarning("could not read %s merge queue for dispatch backpressure: %v", rigName, err)
		return nil
	}
	if ready > maxReady {
		return &queueBackpressureError{Rig: rigName, Ready: ready, Max: maxReady}
	}
	return nil
}

// countReadyMergeRequests counts the rig's ready MRs — the same predicate
// `gt mq list --ready` uses — in one bulk, wisps-aware query. MRs are created
// as ephemeral wisps, so `bd list --label` would undercount the queue it is
// here to measure; ListMergeRequests reads both tables.
func countReadyMergeRequests(lister dispatchMRLister, rigName string) (int, error) {
	issues, err := lister.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1, // no priority filter
		Rig:      rigName,
	})
	if err != nil {
		return 0, err
	}
	ready := 0
	for _, issue := range issues {
		if isMergeRequestReadyForSelection(issue) {
			ready++
		}
	}
	return ready, nil
}
