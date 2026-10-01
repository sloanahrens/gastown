package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// EventsJournalCheck fails when a store's config leaves bd's events journal
// off. The convoy manager polls the journal (gt-7iwy0.2); gastown's own bd
// calls journal regardless (BD_EVENTS_JOURNAL=1), but a close an agent makes
// with bd directly reaches the journal only when the store's config.yaml turns
// it on (gt-7iwy0.7). The value is read with gastown's override removed.
type EventsJournalCheck struct {
	BaseCheck
	// environ is the environment bd runs under before the check pins it to
	// a store; nil is os.Environ.
	environ func() []string
}

// NewEventsJournalCheck creates a new events journal check.
func NewEventsJournalCheck() *EventsJournalCheck {
	return &EventsJournalCheck{
		BaseCheck: BaseCheck{
			CheckName:        "events-journal",
			CheckDescription: "Check every beads store has the events journal on",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run reads events-journal from hq and every registered rig's store.
func (c *EventsJournalCheck) Run(ctx *CheckContext) *CheckResult {
	rigsConfig, err := config.LoadRigsConfig(filepath.Join(ctx.TownRoot, "mayor", "rigs.json"))
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Could not load rigs registry",
			Details: []string{err.Error()},
		}
	}
	stores := []string{"hq"}
	rigs := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		rigs = append(rigs, name)
	}
	sort.Strings(rigs)
	stores = append(stores, rigs...)

	env := os.Environ
	if c.environ != nil {
		env = c.environ
	}

	var off, details []string
	checked := 0
	for _, name := range stores {
		dir := doltserver.FindRigBeadsDir(ctx.TownRoot, name)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		checked++
		v, err := ctx.bd(filepath.Dir(dir), beads.EventsJournalProbeEnv(env(), dir)).ConfigGet(beads.EventsJournalKey)
		switch {
		case err != nil:
			off = append(off, name)
			details = append(details, fmt.Sprintf("%s: cannot read %s: %s", name, beads.EventsJournalKey, bdOutput(err)))
		case !beads.EventsJournalOn(v):
			off = append(off, name)
			details = append(details, fmt.Sprintf("%s: %s is off in %s", name, beads.EventsJournalKey, filepath.Join(dir, "config.yaml")))
		}
	}

	if len(off) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("Events journal on in %d store(s)", checked),
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("Events journal off in %d store(s): convoys miss closes made with bd directly", len(off)),
		Details: details,
		FixHint: fmt.Sprintf("In each store's beads dir run 'BEADS_DIR=<dir> bd config set %s true', then commit config.yaml where it is git-tracked", beads.EventsJournalKey),
	}
}
