package notify

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// UnhandledPush is one escalation nobody has acted on.
type UnhandledPush struct {
	// ID and Title name the escalation; the title is the bead's, never the
	// body's.
	ID    string
	Title string
}

// Line is the one line the push carries: the id, so the text names the
// dashboard row it is about, then the title.
func (u UnhandledPush) Line(window time.Duration) string {
	return fmt.Sprintf("Unhandled for %s: %s %s", windowText(window), u.ID, u.Title)
}

// windowText is a window as a message spells it: whole minutes while it divides
// into minutes, and townhealth's short form otherwise.
func windowText(window time.Duration) string {
	if window >= time.Minute && window%time.Minute == 0 {
		return fmt.Sprintf("%d min", int(window/time.Minute))
	}
	return townhealth.Short(window)
}

// unhandledPushes returns the pushes owed for the open escalations: the ones
// the overseer owns, not acked, not already pushed, and open longer than
// window. An escalation whose age does not parse has no wait to measure and is
// skipped, as the stale listing skips one.
func unhandledPushes(issues []*beads.Issue, now time.Time, window time.Duration) []UnhandledPush {
	var out []UnhandledPush
	for _, issue := range issues {
		if issue == nil || !beads.IsEscalationRecord(issue) {
			continue
		}
		if beads.HasLabel(issue, "acked") || beads.HasLabel(issue, beads.EscalationPushedLabel) {
			continue
		}
		fields := beads.ParseEscalationFields(issue.Description)
		if attention.EscalationOwner(fields.EscalatedBy) != attention.OwnerOverseer {
			continue
		}
		raised, ok := escalationTime(issue, fields)
		if !ok {
			continue
		}
		if now.Sub(raised) < window {
			continue
		}
		title := strings.Join(strings.Fields(issue.Title), " ")
		out = append(out, UnhandledPush{ID: issue.ID, Title: title})
	}
	return out
}

// escalationTime is when an escalation started waiting: the time it was raised,
// and the bead's creation for one that does not carry it.
func escalationTime(issue *beads.Issue, fields *beads.EscalationFields) (time.Time, bool) {
	for _, stamp := range []string{fields.EscalatedAt, issue.CreatedAt} {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(stamp)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// pushMemory is the sends one process has made that could not be recorded on
// their beads: the label Mark writes is the durable record of a push, and this
// keeps a database that refused one from turning that escalation into a text
// every heartbeat until the daemon restarts (gt-71714). It holds the ids of the
// sends whose record failed, and nothing else.
type pushMemory struct {
	mu  sync.Mutex
	ids map[string]bool
}

// newPushMemory is an empty memory.
func newPushMemory() *pushMemory { return &pushMemory{ids: map[string]bool{}} }

// has reports whether this process already pushed id. A nil memory remembers
// nothing, which is what a caller with no process to keep it for gets.
func (m *pushMemory) has(id string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ids[id]
}

// note records that this process pushed id.
func (m *pushMemory) note(id string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids[id] = true
}

// UnhandledRun is one pass of the unhandled push: where it reads the open
// escalations, where it sends and where it records the send. Tests replace them
// so no unit test touches a database or starts a process.
type UnhandledRun struct {
	List func() ([]*beads.Issue, error)
	Push func(name, title, body string) error
	Mark func(id string) error
	// Name is the notify command to push through; "" pushes nothing.
	Name string
	// Window is how long an escalation may wait on the overseer first.
	Window time.Duration
	// memory remembers the sends whose record failed; nil remembers nothing.
	memory *pushMemory
}

// Run pushes once for each escalation that has waited out the window, and
// returns the ids it pushed.
//
// One escalation's failure does not stop the others: the errors are joined into
// the one this returns. A push that fails is not recorded, so the next pass
// tries it again; a push that reached the human but could not be recorded on
// the bead is remembered in memory instead, because a record is not worth a
// second text (gt-71714).
func (r UnhandledRun) Run(now time.Time) ([]string, error) {
	if r.Name == "" || r.Push == nil {
		return nil, nil
	}
	issues, err := r.List()
	if err != nil {
		return nil, err
	}
	var pushed []string
	var errs []error
	for _, u := range unhandledPushes(issues, now, r.Window) {
		if r.memory.has(u.ID) {
			// This process already texted the human about it; only the record
			// of that send was missing.
			continue
		}
		if err := r.Push(r.Name, u.Line(r.Window), ""); err != nil {
			errs = append(errs, fmt.Errorf("pushing %s: %w", u.ID, err))
			continue
		}
		pushed = append(pushed, u.ID)
		if r.Mark == nil {
			continue
		}
		if err := r.Mark(u.ID); err != nil {
			r.memory.note(u.ID)
			errs = append(errs, fmt.Errorf("recording the push of %s: %w", u.ID, err))
		}
	}
	return pushed, errors.Join(errs...)
}

// processPushMemory is the memory every pass in this process shares, so a
// failed record survives to the heartbeat that follows it.
var processPushMemory = newPushMemory()

// PushUnhandled is one pass against the town at townRoot: the open escalations
// and the push command come from the town's own config, as the silent-overseer
// push does. It returns the ids it pushed.
//
// A town that configures no push command has nowhere to send, so the pass stops
// before it reads the escalation config or opens the beads store. A town that
// does pays one config read for the pass and reuses it for every push in it.
func PushUnhandled(townRoot string, now time.Time) ([]string, error) {
	pushName := config.LoadOperationalConfig(townRoot).GetOverseerConfig().SilentPushName()
	if pushName == "" {
		return nil, nil
	}
	cfg, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("loading escalation config: %w", err)
	}
	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	return UnhandledRun{
		List:   func() ([]*beads.Issue, error) { return beads.ListEscalations(bd) },
		Push:   func(name, title, body string) error { return pushThrough(runNotifyCommand, cfg, name, title, body) },
		Mark:   func(id string) error { return beads.MarkEscalationPushed(bd, id) },
		Name:   pushName,
		Window: cfg.GetUnhandledAfter(),
		memory: processPushMemory,
	}.Run(now)
}
