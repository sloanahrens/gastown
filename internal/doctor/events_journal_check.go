package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// EventsJournalCheck reports whether each store's config has bd's events
// journal on. gt tail reads the journal, and bd writes one only when the
// store's config.yaml turns it on (gt-7iwy0.7). A store that has it off is
// named in the details, not failed; only a store whose setting cannot be read
// fails the check. The value is read with any inherited BD_EVENTS_JOURNAL
// removed.
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
			CheckDescription: "Report whether every beads store has the events journal on",
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

	var on, off, unreadable, details []string
	checked := 0
	for _, name := range stores {
		dir := doltserver.FindRigBeadsDir(ctx.TownRoot, name)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		checked++
		v, err := ctx.bd(filepath.Dir(dir), beads.EventsJournalProbeEnv(env(), dir)).ConfigGet(beads.EventsJournalKey)
		configPath := filepath.Join(dir, "config.yaml")
		switch {
		case err != nil:
			unreadable = append(unreadable, name)
			details = append(details, fmt.Sprintf("%s: cannot read %s: %s", name, beads.EventsJournalKey, bdOutput(err)))
		case beads.EventsJournalOn(v):
			on = append(on, name)
		case strings.TrimSpace(v) == "":
			off = append(off, name)
			details = append(details, fmt.Sprintf("%s: %s is not set in %s; bd's default is off", name, beads.EventsJournalKey, configPath))
		default:
			off = append(off, name)
			details = append(details, fmt.Sprintf("%s: %s is off in %s", name, beads.EventsJournalKey, configPath))
		}
	}

	if len(unreadable) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Cannot read the %s setting in %d store(s)", beads.EventsJournalKey, len(unreadable)),
			Details: details,
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("Events journal on in %d of %d store(s)", len(on), checked),
		Details: details,
	}
}
