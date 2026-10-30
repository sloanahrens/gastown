package shelltier

import (
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// StateFileName is the shell tier's state file under the town runtime. It is
// kept apart from the container packages' state.json so the two never share keys.
const StateFileName = "shell-state.json"

// StatePath is <town>/.runtime/tiertrigger/shell-state.json, the per-script
// record of each shell test's last green run.
func StatePath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "tiertrigger", StateFileName)
}

// Select returns the entries whose shell test runs in a sweep at now, in
// entries' order, and the ones it skips. Each entry is judged against its own
// record in st, by the same rule the container packages use: it runs when it
// has no record, when its daily window has passed, or when a changed path
// since its recorded commit is at or under one of its triggers. An entry whose
// record cannot be diffed runs too, and is named in unreadable with the error.
//
// loc is the zone the daily window is read in and must not be nil.
func Select(entries []Entry, st tiertrigger.State, g tiertrigger.Git, now time.Time, loc *time.Location) (run, skipped []Entry, unreadable map[string]error) {
	pkgs := make([]tiertrigger.Package, 0, len(entries))
	for _, e := range entries {
		pkgs = append(pkgs, tiertrigger.Package{Name: e.Path, Triggers: e.TriggersOrDefault()})
	}
	due, unreadable := tiertrigger.Due(st, pkgs, g, now, loc)
	isDue := make(map[string]bool, len(due))
	for _, name := range due {
		isDue[name] = true
	}
	for _, e := range entries {
		if isDue[e.Path] {
			run = append(run, e)
		} else {
			skipped = append(skipped, e)
		}
	}
	return run, skipped, unreadable
}

// Record stamps e as passing at commit when. The caller records only a green
// run, so a red script keeps its older record and runs again next sweep.
func Record(st tiertrigger.State, e Entry, commit string, now time.Time) {
	st.Ran(e.Path, commit, now)
}
