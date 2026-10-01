package doctor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/runtime"
	"github.com/steveyegge/gastown/internal/tmux"
)

// sessionHooksLister lists the live tmux sessions SessionHooksCheck reads.
type sessionHooksLister interface {
	ListSessions() ([]string, error)
}

// SessionHooksCheck reports live Gas Town sessions whose last
// hooks:present|absent event says they started without the managed guards,
// or that have no such event at all (gt-4k3fj.8.4). Every session start and
// every handoff respawn records one (runtime.ReportHooks), so the events log
// is the record of which settings each running agent loaded; the settings
// file on disk only says what the next start will load.
type SessionHooksCheck struct {
	BaseCheck

	listerForTest sessionHooksLister // nil → real tmux
}

// NewSessionHooksCheck creates a new session hooks check.
func NewSessionHooksCheck() *SessionHooksCheck {
	return &SessionHooksCheck{
		BaseCheck: BaseCheck{
			CheckName:        "session-hooks",
			CheckDescription: "Check every live session started with the managed hooks (hooks:present)",
			CheckCategory:    CategoryHooks,
		},
	}
}

// hooksEvent is the last hooks:present|absent event recorded for a session.
type hooksEvent struct {
	Type   string
	Time   string
	Reason string
}

// Run matches the live Gas Town sessions against their last hooks event.
func (c *SessionHooksCheck) Run(ctx *CheckContext) *CheckResult {
	var lister sessionHooksLister = tmux.NewTmux()
	if c.listerForTest != nil {
		lister = c.listerForTest
	}
	sessions, err := lister.ListSessions()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not list tmux sessions",
			Details: []string{err.Error()},
		}
	}
	last, err := lastHooksEvents(filepath.Join(ctx.TownRoot, events.EventsFile))
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "unknown: could not read the events log",
			Details: []string{err.Error()},
		}
	}

	var absent, unreported []string
	present := 0
	sort.Strings(sessions)
	for _, sess := range sessions {
		if sess == "" || !ctx.prefixes().IsKnownSession(sess) {
			continue
		}
		ev, ok := last[sess]
		switch {
		case !ok:
			unreported = append(unreported, sess)
		case ev.Type == runtime.EventHooksAbsent:
			absent = append(absent, fmt.Sprintf("%s: hooks:absent at %s: %s", sess, ev.Time, ev.Reason))
		default:
			present++
		}
	}

	if len(absent) == 0 && len(unreported) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("%d live session(s) started with the managed hooks", present),
		}
	}
	details := absent
	for _, sess := range unreported {
		details = append(details, fmt.Sprintf("%s: no hooks event (started before start-time reporting, or the event was pruned)", sess))
	}
	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: fmt.Sprintf("%d live session(s) started without the managed hooks, %d with no hooks event",
			len(absent), len(unreported)),
		Details: details,
		FixHint: "Run 'gt hooks sync', then restart or 'gt handoff' each listed session; the new start reports hooks:present",
	}
}

// lastHooksEvents returns the last hooks:present|absent event per session in
// the events log at path; a missing log has none.
func lastHooksEvents(path string) (map[string]hooksEvent, error) {
	last := make(map[string]hooksEvent)
	f, err := os.Open(path) //nolint:gosec // G304: path is the town's events log
	if os.IsNotExist(err) {
		return last, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	marker := []byte(`"hooks:`)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), marker) {
			continue
		}
		var ev events.Event
		// A torn or foreign line is not a hooks event; skip it.
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Type != runtime.EventHooksPresent && ev.Type != runtime.EventHooksAbsent {
			continue
		}
		sess, _ := ev.Payload["session"].(string)
		if sess == "" {
			continue
		}
		reason, _ := ev.Payload["reason"].(string)
		last[sess] = hooksEvent{Type: ev.Type, Time: ev.Timestamp, Reason: reason}
	}
	return last, sc.Err()
}
