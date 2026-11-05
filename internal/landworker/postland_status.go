package landworker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// PostLandStatusPollInterval is how long status mode waits between reads of
// the landed commit's combined status.
const PostLandStatusPollInterval = 30 * time.Second

// PostLandStatusDefaultTimeout bounds a status wait whose runner names no
// timeout. Production passes merge_queue.post_land_timeout instead.
const PostLandStatusDefaultTimeout = 60 * time.Minute

// PostLandClock is the time source status mode polls with. A clockwork.Clock
// satisfies it, so the daemon passes its own clock.
type PostLandClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
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

// statusEnabled reports whether status mode can decide this runner's verdict.
func (p *PostLandRunner) statusEnabled() bool {
	return p.Status != nil && p.statusContext() != ""
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

func (realPostLandClock) Now() time.Time                         { return time.Now() }
func (realPostLandClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// runStatus waits for the landed commit's status context to reach a verdict,
// then calls OnGreen or OnRed exactly once. A wait that ends at the timeout
// with no verdict calls neither: it hands OnUnknown one record for the commit,
// so a silent CI is never read as green (gt-gg9hv.7.1).
func (p *PostLandRunner) runStatus(ctx context.Context, pl PostLand, name string) {
	clock := p.statusClock()
	timeout := p.statusTimeout()
	deadline := clock.Now().Add(timeout)
	last := p.readStatus(ctx, pl, name)
	if ctx.Err() != nil {
		return
	}
	if p.verdict(ctx, pl, name, last) {
		return
	}
	if !clock.Now().Before(deadline) {
		p.raiseUnknown(ctx, pl, name, last)
		return
	}
	p.logf("UNKNOWN at %s (%s): status %q is %s; waiting up to %s: %s",
		short(pl.Commit), pl.by(), name, last.State, timeout, last.Description)
	logged := last.State
	for {
		wait := PostLandStatusPollInterval
		if remaining := deadline.Sub(clock.Now()); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-clock.After(wait):
			}
		}
		last = p.readStatus(ctx, pl, name)
		if ctx.Err() != nil {
			return
		}
		if p.verdict(ctx, pl, name, last) {
			return
		}
		if last.State != logged {
			p.logf("waiting at %s (%s): status %q is now %s: %s",
				short(pl.Commit), pl.by(), name, last.State, last.Description)
			logged = last.State
		}
		if !clock.Now().Before(deadline) {
			p.raiseUnknown(ctx, pl, name, last)
			return
		}
	}
}

// readStatus reads the landed commit's combined status once.
func (p *PostLandRunner) readStatus(ctx context.Context, pl PostLand, name string) PostLandStatusResult {
	cs, err := p.Status(ctx, pl)
	return postLandStatusRead(cs, err, name, pl.Commit)
}

// verdict calls the runner's callback for a decided read and reports whether
// the wait is over.
func (p *PostLandRunner) verdict(ctx context.Context, pl PostLand, name string, res PostLandStatusResult) bool {
	switch res.Verdict {
	case PostLandStatusGreen:
		p.logf("green at %s (%s): status %q is success", short(pl.Commit), pl.by(), name)
		if p.OnGreen != nil {
			p.OnGreen(ctx, name, pl, PostLandResult{ExitCode: 0})
		}
		return true
	case PostLandStatusRed:
		p.logf("RED at %s (%s): status %q is %s: %s", short(pl.Commit), pl.by(), name, res.State, res.Description)
		if p.OnRed != nil {
			p.OnRed(ctx, name, pl, PostLandResult{ExitCode: 1, Tail: res.Description})
		}
		return true
	}
	return false
}

// raiseUnknown records that the commit's status context never gave a verdict,
// once per landed commit, and hands the record on for the attention queue.
func (p *PostLandRunner) raiseUnknown(ctx context.Context, pl PostLand, name string, last PostLandStatusResult) {
	u := PostLandUnknown{
		Rig:     p.Rig,
		SHA:     pl.Commit,
		Context: name,
		State:   last.State,
		At:      p.statusClock().Now(),
	}
	key := u.Key()
	p.unknownMu.Lock()
	if p.unknownRaised == nil {
		p.unknownRaised = map[string]bool{}
	}
	if p.unknownRaised[key] {
		p.unknownMu.Unlock()
		return
	}
	p.unknownRaised[key] = true
	p.unknownMu.Unlock()
	p.logf("UNKNOWN: no verdict for status %q on %s (%s) within %s (last state %s: %s); the post-land check is silent and nothing is reverted",
		name, short(pl.Commit), pl.by(), p.statusTimeout(), last.State, last.Description)
	if p.OnUnknown != nil {
		p.OnUnknown(ctx, u)
	}
}
