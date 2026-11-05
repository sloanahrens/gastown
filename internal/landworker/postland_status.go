package landworker

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// PostLandStatusPollInterval is the least time between two reads of one
// pending landed commit's combined status.
const PostLandStatusPollInterval = 30 * time.Second

// PostLandStatusDefaultTimeout bounds a pending verdict whose runner names no
// timeout. Production passes merge_queue.post_land_timeout instead.
const PostLandStatusDefaultTimeout = 60 * time.Minute

// postLandPendingLimit is how many pending verdicts a rig keeps. The newest are
// read; an older one is dropped with a log line.
const postLandPendingLimit = 20

// postLandStatusTextRunes bounds the status text that reaches a bead comment,
// an attention summary or a log line. It is untrusted text from Forgejo.
const postLandStatusTextRunes = 200

// PostLandClock is the time source status mode reads its deadlines by. A
// clockwork.Clock satisfies it, so the daemon passes its own clock.
type PostLandClock interface {
	Now() time.Time
}

// PostLandStatusVerdict is what the configured status context says about a
// landed commit.
type PostLandStatusVerdict int

// The verdicts. UNKNOWN is the zero value: anything not proven green or red
// is no verdict, never a default.
const (
	// PostLandStatusUnknown is no verdict: CI has not reported, or what it
	// reported is not trustworthy.
	PostLandStatusUnknown PostLandStatusVerdict = iota
	// PostLandStatusGreen: every status carrying the context is success.
	PostLandStatusGreen
	// PostLandStatusRed: a status carrying the context is failure or error.
	PostLandStatusRed
)

// PostLandStatusResult is one read of a landed commit's status context.
type PostLandStatusResult struct {
	Verdict PostLandStatusVerdict
	// State is the last state seen for the context: "missing", "unreadable",
	// "pending", "success", "failure", "error", "warning", "skipped" or
	// "posted by hand".
	State string
	// Description is the failing status's description on red, and the reason
	// an UNKNOWN is unknown.
	Description string
}

// postLandStatusRead turns one combined-status read into a verdict. Every
// status carrying context is read: one a user posted makes the read UNKNOWN,
// because Forgejo attributes a workflow-posted status to no user
// (gt-fn9e6.7), and with no creator on any of them one failure or error is red
// even beside a success.
func postLandStatusRead(cs *forgejo.CombinedStatus, err error, name, sha string) PostLandStatusResult {
	if err != nil {
		return PostLandStatusResult{
			Verdict:     PostLandStatusUnknown,
			State:       "unreadable",
			Description: fmt.Sprintf("reading the combined status of %s: %v", short(sha), err),
		}
	}
	if cs == nil {
		return PostLandStatusResult{
			Verdict:     PostLandStatusUnknown,
			State:       "unreadable",
			Description: "the combined status of " + short(sha) + " was empty",
		}
	}
	statuses := cs.StatusesFor(name)
	if len(statuses) == 0 {
		return PostLandStatusResult{
			Verdict:     PostLandStatusUnknown,
			State:       "missing",
			Description: fmt.Sprintf("no %q status is on %s", name, short(sha)),
		}
	}
	for i := range statuses {
		if st := &statuses[i]; st.HasUserCreator() {
			return PostLandStatusResult{
				Verdict: PostLandStatusUnknown,
				State:   "posted by hand",
				Description: fmt.Sprintf("the %q status on %s was posted by user %q, not by a workflow run",
					name, short(sha), st.CreatorLogin()),
			}
		}
	}
	for i := range statuses {
		st := statuses[i]
		if st.Status != forgejo.StateFailure && st.Status != forgejo.StateError {
			continue
		}
		desc := strings.TrimSpace(st.Description)
		if desc == "" {
			desc = fmt.Sprintf("the %q status is %s", name, st.Status)
		}
		return PostLandStatusResult{Verdict: PostLandStatusRed, State: string(st.Status), Description: desc}
	}
	allSuccess := true
	for i := range statuses {
		if statuses[i].Status != forgejo.StateSuccess {
			allSuccess = false
			break
		}
	}
	if allSuccess {
		return PostLandStatusResult{Verdict: PostLandStatusGreen, State: string(forgejo.StateSuccess)}
	}
	// No red and no green: name what CI is still waiting on, else what it
	// reported instead of a verdict. A state this code does not know lands in
	// the final branch as UNKNOWN too.
	for _, state := range []forgejo.CommitState{forgejo.StatePending, forgejo.StateWarning, forgejo.StateSkipped} {
		for i := range statuses {
			if statuses[i].Status == state {
				return PostLandStatusResult{
					Verdict:     PostLandStatusUnknown,
					State:       string(state),
					Description: fmt.Sprintf("the %q status on %s is %s", name, short(sha), state),
				}
			}
		}
	}
	return PostLandStatusResult{
		Verdict:     PostLandStatusUnknown,
		State:       string(statuses[0].Status),
		Description: fmt.Sprintf("the %q status on %s is %q, which is not a verdict", name, short(sha), statuses[0].Status),
	}
}

// sanitizeStatusText makes untrusted status text safe for one line: control
// characters and newlines become a space, the text is cut to
// postLandStatusTextRunes runes, and its ends are trimmed.
func sanitizeStatusText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > postLandStatusTextRunes {
		s = string([]rune(s)[:postLandStatusTextRunes])
	}
	return strings.TrimSpace(s)
}

// PendingPostLand is a landed commit whose status-mode verdict is still
// waiting on CI. It lives in the rig's MainState, so a daemon restart resumes
// the wait (gt-gg9hv.7.1).
type PendingPostLand struct {
	// Land is the landing the verdict is for: the run the callbacks receive.
	Land PostLand `json:"land"`
	// Context is the status context the verdict is read from.
	Context string `json:"context"`
	// Since is when the verdict was recorded. The timeout counts from it.
	Since time.Time `json:"since"`
	// LastRead is the last time the status was read; zero before the first read.
	LastRead time.Time `json:"last_read,omitzero"`
	// LastState and LastDescription are what that read found.
	LastState       string `json:"last_state,omitempty"`
	LastDescription string `json:"last_description,omitempty"`
}

// PostLandUnknown is a landed commit whose post-land status context never
// reached a verdict before post_land_timeout: CI stopped reporting, so the
// post-land check is silent.
type PostLandUnknown struct {
	Rig     string
	SHA     string
	Context string
	State   string
	At      time.Time
}

// Key is the attention queue's stable identity for the record: one per rig
// and landed commit, so a rerun at the same commit raises no second item.
func (u PostLandUnknown) Key() string {
	return "post-land-unknown:" + u.Rig + ":" + u.SHA
}

// Summary is the attention item's one line, naming the context and the last
// state seen.
func (u PostLandUnknown) Summary() string {
	return fmt.Sprintf("post-land verdict unknown for %s (%s): the %q status is %s",
		short(u.SHA), u.Rig, u.Context, u.State)
}

// statusContext is the configured status-mode context, "" when status mode is
// off.
func (p *PostLandRunner) statusContext() string {
	if p.StatusContext == nil {
		return ""
	}
	return strings.TrimSpace(p.StatusContext())
}

// statusEnabled reports whether status mode decides this runner's verdict: a
// context is set, and the runner can both read the status and keep the
// pending verdict it waits on.
func (p *PostLandRunner) statusEnabled() bool {
	return p.Status != nil && p.State != nil && p.statusContext() != ""
}

func (p *PostLandRunner) statusClock() PostLandClock {
	if p.Clock != nil {
		return p.Clock
	}
	return realPostLandClock{}
}

func (p *PostLandRunner) statusTimeout() time.Duration {
	if p.Timeout <= 0 {
		return PostLandStatusDefaultTimeout
	}
	return p.Timeout
}

// realPostLandClock is time's own clock.
type realPostLandClock struct{}

func (realPostLandClock) Now() time.Time { return time.Now() }

// recordPending starts status mode's wait for pl. It writes a pending verdict
// into the rig's main state and returns: nothing waits on CI, and PollPending
// reads the status on a later pass. A verdict that cannot be recorded is
// raised as UNKNOWN at once, so the silence is an attention item.
func (p *PostLandRunner) recordPending(ctx context.Context, pl PostLand, name string) {
	now := p.statusClock().Now()
	st, err := p.State.Load()
	if err != nil {
		p.unrecorded(ctx, pl, name, fmt.Errorf("reading the main state: %w", err))
		return
	}
	for _, e := range st.PostLandPending {
		if e.Land.Commit == pl.Commit && e.Context == name {
			return
		}
	}
	st.PostLandPending = append(st.PostLandPending, PendingPostLand{Land: pl, Context: name, Since: now})
	if drop := len(st.PostLandPending) - postLandPendingLimit; drop > 0 {
		for _, e := range st.PostLandPending[:drop] {
			p.logf("dropping the pending verdict for %s (%s): the rig keeps the newest %d", short(e.Land.Commit), e.Land.by(), postLandPendingLimit)
		}
		st.PostLandPending = st.PostLandPending[drop:]
	}
	if err := p.State.Save(st); err != nil {
		p.unrecorded(ctx, pl, name, fmt.Errorf("saving the main state: %w", err))
		return
	}
	p.logf("pending at %s (%s): status %q is read on the next pass; nothing waits", short(pl.Commit), pl.by(), name)
}

// unrecorded raises the UNKNOWN for a landed commit whose pending verdict could
// not be written.
func (p *PostLandRunner) unrecorded(ctx context.Context, pl PostLand, name string, err error) {
	p.logf("WARNING could not record the pending verdict for %s (%s): %v", short(pl.Commit), pl.by(), err)
	p.raiseUnknown(ctx, pl, name, PostLandStatusResult{
		State:       "unrecorded",
		Description: sanitizeStatusText(err.Error()),
	})
}

// PollPending is one pass over the pending verdicts. It never waits: a pending
// commit is read at most once per PostLandStatusPollInterval; a green or red
// read fires its callback once and drops the entry; a commit still open at its
// timeout raises one UNKNOWN and is dropped. The entries are saved before any
// callback runs, so a verdict fires at most once, even across a restart.
func (p *PostLandRunner) PollPending(ctx context.Context) {
	if p.State == nil || ctx.Err() != nil {
		return
	}
	st, err := p.State.Load()
	if err != nil {
		p.logf("reading the pending post-land verdicts: %v", err)
		return
	}
	if len(st.PostLandPending) == 0 {
		return
	}
	now := p.statusClock().Now()
	timeout := p.statusTimeout()
	var keep []PendingPostLand
	var fires []func()
	changed := false
	for _, e := range st.PostLandPending {
		if ctx.Err() != nil {
			keep = append(keep, e)
			continue
		}
		pl, name := e.Land, e.Context
		if p.Status != nil && (e.LastRead.IsZero() || now.Sub(e.LastRead) >= PostLandStatusPollInterval) {
			res := p.readStatus(ctx, pl, name)
			if ctx.Err() != nil {
				keep = append(keep, e)
				continue
			}
			changed = true
			if res.Verdict != PostLandStatusUnknown {
				fires = append(fires, func() { p.fire(ctx, pl, name, res) })
				continue
			}
			if res.State != e.LastState {
				p.logf("waiting at %s (%s): status %q is now %s: %s", short(pl.Commit), pl.by(), name, res.State, res.Description)
			}
			e.LastRead, e.LastState, e.LastDescription = now, res.State, res.Description
		}
		if !now.Before(e.Since.Add(timeout)) {
			last := PostLandStatusResult{State: e.LastState, Description: e.LastDescription}
			if last.State == "" {
				last.State = "unread"
			}
			changed = true
			fires = append(fires, func() { p.raiseUnknown(ctx, pl, name, last) })
			continue
		}
		keep = append(keep, e)
	}
	if !changed {
		return
	}
	st.PostLandPending = keep
	if err := p.State.Save(st); err != nil {
		// Nothing fires without its record, so the entries are read again on
		// the next pass.
		p.logf("saving the pending post-land verdicts: %v", err)
		return
	}
	for _, fire := range fires {
		fire()
	}
}

// readStatus reads the landed commit's combined status once, with its text
// made safe for a bead comment, an attention summary and a log line.
func (p *PostLandRunner) readStatus(ctx context.Context, pl PostLand, name string) PostLandStatusResult {
	cs, err := p.Status(ctx, pl)
	res := postLandStatusRead(cs, err, name, pl.Commit)
	res.State = sanitizeStatusText(res.State)
	res.Description = sanitizeStatusText(res.Description)
	return res
}

// fire hands a decided read to the runner's callback: OnGreen for green, and
// for red a bead comment and OnRed. An UNKNOWN calls neither.
func (p *PostLandRunner) fire(ctx context.Context, pl PostLand, name string, res PostLandStatusResult) {
	switch res.Verdict {
	case PostLandStatusGreen:
		p.logf("green at %s (%s): status %q is success", short(pl.Commit), pl.by(), name)
		if p.OnGreen != nil {
			p.OnGreen(ctx, name, pl, PostLandResult{ExitCode: 0})
		}
	case PostLandStatusRed:
		p.logf("RED at %s (%s): status %q is %s: %s", short(pl.Commit), pl.by(), name, res.State, res.Description)
		if p.Beads != nil && pl.BeadID != "" {
			msg := fmt.Sprintf("post-landing status RED at %s: status %q is %s: %s", pl.Commit, name, res.State, res.Description)
			if err := p.Beads.AddComment(pl.BeadID, msg); err != nil {
				p.logf("commenting on %s: %v", pl.BeadID, err)
			}
		}
		if p.OnRed != nil {
			p.OnRed(ctx, name, pl, PostLandResult{ExitCode: 1, Tail: res.Description})
		}
	}
}

// raiseUnknown records that a landed commit's status context gave no verdict
// before its timeout, and hands the record on for the attention queue. Its
// pending entry is already gone, so each commit raises once.
func (p *PostLandRunner) raiseUnknown(ctx context.Context, pl PostLand, name string, last PostLandStatusResult) {
	u := PostLandUnknown{
		Rig:     p.Rig,
		SHA:     pl.Commit,
		Context: name,
		State:   last.State,
		At:      p.statusClock().Now(),
	}
	p.logf("UNKNOWN: no verdict for status %q on %s (%s) (last state %s: %s); the post-land check is silent and nothing is reverted",
		name, short(pl.Commit), pl.by(), last.State, last.Description)
	if p.OnUnknown != nil {
		p.OnUnknown(ctx, u)
	}
}
