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
	// Age is how long the escalation has been open, not the window that made
	// it owed: one open for 3 days says so.
	Age time.Duration
}

// Line is the one line the push carries: how long the escalation has waited,
// then the id, so the text names the dashboard row it is about, then the title.
func (u UnhandledPush) Line() string {
	return fmt.Sprintf("Unhandled for %s: %s %s", ageText(u.Age), u.ID, u.Title)
}

// ageText is a wait as a message spells it: whole minutes below an hour, and
// townhealth's short form (seconds, hours, days) otherwise.
func ageText(age time.Duration) string {
	if age >= time.Minute && age < time.Hour {
		return fmt.Sprintf("%d min", int(age/time.Minute))
	}
	return townhealth.Short(age)
}

// unhandledPushes returns the pushes owed for the open escalations: the ones
// the overseer owns, not acked, not already pushed, and open longer than
// window, each carrying how long it has been open. An escalation whose age does not parse has no wait to measure and is
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
		out = append(out, UnhandledPush{ID: issue.ID, Title: title, Age: now.Sub(raised)})
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

// DefaultUnhandledMaxPerPass is how many escalations one pass pushes at most.
//
// A pass runs inside a heartbeat, and a notify command that hangs holds it for
// pushcmd.Timeout each: the cap is what makes the wait a small multiple of that
// timeout instead of one timeout per unread escalation, so a town deployed with
// silent_push set texts a few old escalations per heartbeat rather than every
// open one in the first pass after the deploy (gt-8db90). It stays well under
// the 3m default heartbeat (config.DefaultRecoveryHeartbeatInterval).
const DefaultUnhandledMaxPerPass = 5

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
	// Max is the most pushes one pass makes; 0 or less is
	// DefaultUnhandledMaxPerPass.
	Max int
	// memory remembers the sends whose record failed; nil remembers nothing.
	memory *pushMemory
}

// maxPerPass is the cap this pass runs under.
func (r UnhandledRun) maxPerPass() int {
	if r.Max > 0 {
		return r.Max
	}
	return DefaultUnhandledMaxPerPass
}

// Run pushes once for each escalation that has waited out the window, and
// returns the ids it pushed.
//
// One pass is bounded: it pushes at most maxPerPass escalations, and every
// escalation it does not reach waits for a later pass untouched — nothing is
// pushed twice, dropped or marked on its behalf (gt-8db90). A pass costs one
// push timeout per push it makes, so the cap is what keeps a heartbeat from
// paying the timeout once per open escalation.
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
	// The cap counts the pushes the pass makes, outcomes included: a failed
	// push still spent a timeout, which is what the cap bounds. A skipped
	// memory hit costs nothing and so is not one of them.
	pushes := 0
	for _, u := range unhandledPushes(issues, now, r.Window) {
		if r.memory.has(u.ID) {
			// This process already texted the human about it; only the record
			// of that send was missing.
			continue
		}
		if pushes >= r.maxPerPass() {
			break
		}
		pushes++
		if err := r.Push(r.Name, u.Line(), ""); err != nil {
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
