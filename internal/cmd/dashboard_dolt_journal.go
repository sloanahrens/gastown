package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// The journal section draws one row per store: its events-journal setting as
// bd reads it now, because the journal flip is the experiment the operator
// watches store by store. It reads the setting and never the journal's records.

func init() {
	registerDoltSection("journal", doltJournalRead)
}

// doltJournalRead fills the journal section of the panel dashboardDolt draws.
func doltJournalRead(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
	townRoot, ok := doltTownRoot(ctx)
	if !ok {
		return dashboard.DoltSection{}, fmt.Errorf("the events journal section has no town root")
	}
	return doltJournalSection(townRoot)
}

// doltJournalSection reads every store's events-journal setting into one row
// each, hq first and then the rigs the town registry names — the store set gt
// tail reads. get is one store's config read, injectable so a test reads fakes
// per store; production passes none and reads each store's own beads directory.
func doltJournalSection(townRoot string, get ...func(store string) (string, error)) (dashboard.DoltSection, error) {
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return dashboard.DoltSection{}, fmt.Errorf("naming the town's stores: %w", err)
	}
	read := journalConfigGet(townRoot)
	if len(get) > 0 && get[0] != nil {
		read = get[0]
	}
	section := dashboard.DoltSection{
		Title: "Events journal",
		Note:  "Read live from each store's bd config: hq, then every rig the town registry names.",
		Head:  []string{"store", "value", "state"},
		Rows:  make([][]string, 0, len(rigs)+1),
	}
	for _, store := range append([]string{"hq"}, rigs...) {
		value, err := read(store)
		if err != nil {
			section.Rows = append(section.Rows, []string{store, "", "unreadable: " + dashWhy(err)})
			continue
		}
		section.Rows = append(section.Rows, []string{store, value, journalState(value)})
	}
	return section, nil
}

// journalState draws one store's events-journal value as the panel's state.
func journalState(value string) string {
	switch {
	case strings.TrimSpace(value) == "":
		return "unset (off)"
	case beads.EventsJournalOn(value):
		return "on"
	default:
		return "off"
	}
}

// journalConfigGet reads one store's events-journal setting through the store's
// own beads directory. A store without a directory is an error for its row,
// never a read against the empty path.
func journalConfigGet(townRoot string) func(store string) (string, error) {
	return func(store string) (string, error) {
		dir := doltserver.FindRigBeadsDir(townRoot, store)
		if dir == "" {
			return "", fmt.Errorf("store %s has no beads directory", store)
		}
		return beads.NewWithBeadsDir(townRoot, dir).ConfigGet(beads.EventsJournalKey)
	}
}
