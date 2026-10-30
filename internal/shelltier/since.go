package shelltier

import (
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// Checkout is the repository a --since-green run reads: the commit HEAD names,
// and the change since a recorded commit.
type Checkout struct {
	Head func() (string, error)
	Git  tiertrigger.Git
}

// Plan is what one run executes and what it may record. Run holds the entries
// to run and Skipped counts the ones left out. A plan that could not read its
// state or HEAD runs every entry and records nothing.
type Plan struct {
	Run     []Entry
	Skipped int

	recordable bool
	head       string
	store      tiertrigger.Store
}

// AllEntries is the plan for a run that ignores the state: every entry runs,
// nothing is recorded, and no state file is read or written.
func AllEntries(entries []Entry) Plan {
	return Plan{Run: entries}
}

// PlanSinceGreen selects the entries a --since-green run in town executes. The
// state is read through files and HEAD and the change since each record come
// from co. An entry runs when Select says so; the rest are counted as skipped.
//
// A HEAD or state that cannot be read runs every entry and says why on errw.
// Such a plan records nothing and writes nothing, so a state file that does not
// parse is left for a person to look at rather than replaced.
func PlanSinceGreen(entries []Entry, town string, files tiertrigger.Files, co Checkout, zone func() (*time.Location, error), now time.Time, errw io.Writer) Plan {
	everything := AllEntries(entries)
	head, err := co.Head()
	if err != nil {
		fmt.Fprintf(errw, "shelltier: read HEAD: %v: running every entry and recording none\n", err)
		return everything
	}
	store := tiertrigger.Store{Path: StatePath(town), Files: files}
	st, err := store.Load()
	if err != nil {
		fmt.Fprintf(errw, "shelltier: read the shell state: %v: running every entry and recording none\n", err)
		return everything
	}
	loc, err := zone()
	if err != nil {
		fmt.Fprintf(errw, "shelltier: read the daily zone: %v: running every entry and recording none\n", err)
		return everything
	}
	run, skipped, unreadable := Select(entries, st, co.Git, now, loc)
	for _, e := range run {
		if err, ok := unreadable[e.Path]; ok {
			fmt.Fprintf(errw, "shelltier: %s: its record cannot be read: %v: running it\n", e.Path, err)
		}
	}
	return Plan{
		Run:        run,
		Skipped:    len(skipped),
		recordable: true,
		head:       head,
		store:      store,
	}
}

// Finish records each entry of the plan that passed in res, at the commit the
// run tested, and saves the state when anything was recorded. A failed entry
// keeps its older record, so it runs again on the next sweep.
//
// res must account for every entry the plan ran: each one passed or is named in
// FailedNames. A result that does not is not trusted as green, so Finish
// records nothing and says why on errw. The state is read again just before the
// save, so records another run wrote meanwhile are kept, and a record for an
// entry that is newer than this run's stays. Finish assumes one writer at a
// time: the daemon's tier sweep is serialized per rig. A state that cannot be
// read or saved is said on errw; the verdict in res stands.
func (p Plan) Finish(res Result, now time.Time, errw io.Writer) {
	if !p.recordable {
		return
	}
	if !accounts(res, p.Run) {
		fmt.Fprintf(errw, "shelltier: the run's result does not account for every entry: recording none\n")
		return
	}
	failed := make(map[string]bool, len(res.FailedNames))
	for _, name := range res.FailedNames {
		failed[name] = true
	}
	st, err := p.store.Load()
	if err != nil {
		fmt.Fprintf(errw, "shelltier: re-read the shell state: %v: recording none\n", err)
		return
	}
	recorded := false
	for _, e := range p.Run {
		if failed[e.Path] {
			continue
		}
		if rec, ok := st[e.Path]; ok && rec.At.After(now) {
			continue
		}
		Record(st, e, p.head, now)
		recorded = true
	}
	if !recorded {
		return
	}
	if err := p.store.Save(st); err != nil {
		fmt.Fprintf(errw, "shelltier: save the shell state: %v\n", err)
	}
}

// accounts reports whether res names every entry of run exactly once: each one
// passed or is in FailedNames, and no name is failed twice or is outside run.
// Run sets FailedNames for every entry that did not pass (shelltier.Run in
// run.go), so a result that does not add up was not produced by Run.
func accounts(res Result, run []Entry) bool {
	if res.Passed+len(res.FailedNames) != len(run) {
		return false
	}
	inRun := make(map[string]bool, len(run))
	for _, e := range run {
		inRun[e.Path] = true
	}
	failed := make(map[string]bool, len(res.FailedNames))
	for _, name := range res.FailedNames {
		if !inRun[name] || failed[name] {
			return false
		}
		failed[name] = true
	}
	return true
}
