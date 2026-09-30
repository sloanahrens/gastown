package doctor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/util"
)

// eventsLogWarnBytes is four times the daemon's default events_prune cap: a
// file this big means the patrol is off or failing (gt-ori5j).
const eventsLogWarnBytes int64 = 64 << 20

// EventsLogCheck reports the size of the town's raw event log, which only the
// daemon's events_prune patrol bounds.
type EventsLogCheck struct {
	BaseCheck
}

// NewEventsLogCheck creates a new events log size check.
func NewEventsLogCheck() *EventsLogCheck {
	return &EventsLogCheck{
		BaseCheck: BaseCheck{
			CheckName:        "events-log-size",
			CheckDescription: "Check .events.jsonl is bounded by the daemon's events_prune patrol",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run stats <town>/.events.jsonl.
func (c *EventsLogCheck) Run(ctx *CheckContext) *CheckResult {
	info, err := os.Stat(filepath.Join(ctx.TownRoot, events.EventsFile))
	if os.IsNotExist(err) {
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "no events log yet"}
	}
	if err != nil {
		return &CheckResult{Name: c.Name(), Status: StatusWarning, Message: fmt.Sprintf("cannot stat events log: %v", err)}
	}
	size := util.FormatBytesHuman(uint64(info.Size())) //nolint:gosec // G115: file size is non-negative
	if info.Size() > eventsLogWarnBytes {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf(".events.jsonl is %s", size),
			Details: []string{"The daemon's events_prune patrol keeps it under 16 MB by default; it looks off or failing."},
			FixHint: `Check daemon.log for "events_prune" and patrols.events_prune in mayor/daemon.json`,
		}
	}
	return &CheckResult{Name: c.Name(), Status: StatusOK, Message: fmt.Sprintf(".events.jsonl is %s", size)}
}
