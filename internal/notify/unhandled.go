package notify

import (
	"fmt"
	"strings"
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
}

// Run pushes once for each escalation that has waited out the window, and
// returns the ids it pushed. A push that fails is not recorded, so the next
// pass tries it again; a failure to record one does not stop the others.
func (r UnhandledRun) Run(now time.Time) ([]string, error) {
	if r.Name == "" || r.Push == nil {
		return nil, nil
	}
	issues, err := r.List()
	if err != nil {
		return nil, err
	}
	var pushed []string
	for _, u := range unhandledPushes(issues, now, r.Window) {
		if err := r.Push(r.Name, u.Line(r.Window), ""); err != nil {
			return pushed, fmt.Errorf("pushing %s: %w", u.ID, err)
		}
		if r.Mark != nil {
			if err := r.Mark(u.ID); err != nil {
				return pushed, fmt.Errorf("recording the push of %s: %w", u.ID, err)
			}
		}
		pushed = append(pushed, u.ID)
	}
	return pushed, nil
}

// PushUnhandled is one pass against the town at townRoot: the open escalations
// and the push command come from the town's own config, as the silent-overseer
// push does. It returns the ids it pushed.
func PushUnhandled(townRoot string, now time.Time) ([]string, error) {
	cfg, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("loading escalation config: %w", err)
	}
	pushName := config.LoadOperationalConfig(townRoot).GetOverseerConfig().SilentPushName()
	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	return UnhandledRun{
		List:   func() ([]*beads.Issue, error) { return beads.ListEscalations(bd) },
		Push:   func(name, title, body string) error { return PushIn(townRoot, name, title, body) },
		Mark:   func(id string) error { return beads.MarkEscalationPushed(bd, id) },
		Name:   pushName,
		Window: cfg.GetUnhandledAfter(),
	}.Run(now)
}
