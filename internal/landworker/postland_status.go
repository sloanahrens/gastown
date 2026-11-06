package landworker

import (
	"context"
	"fmt"
	"sort"
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

// PostLandStatusReadTimeout bounds one status read on a worker pass, so a
// stalled Forgejo cannot hold the pass: the read becomes an UNKNOWN and the
// commit is read again on a later pass.
const PostLandStatusReadTimeout = 10 * time.Second

// PostLandStatusStoreFailureAfter is how many passes running may fail to read
// or write the pending verdicts before the rig raises one attention item. Below
// it a transient write error is retried silently; at it, the post-land safety
// net is blind and nothing else would say so.
const PostLandStatusStoreFailureAfter = 3

// PostLandStatusPassBudget bounds the whole of one worker pass's status reads:
// the reads are sequential, each bounded by PostLandStatusReadTimeout, so without
// one deadline for the pass a long pending list would hold the pass for the
// count times the read timeout. Entries the budget leaves unread are read first
// on the next pass.
const PostLandStatusPassBudget = 20 * time.Second

// PostLandAlarmWindow is how long a timed-out verdict keeps raising its
// attention item, counted from the moment it timed out: long enough for the 24
// hour townhealth reading to be the backstop behind it (gt-gg9hv.7.2), and
// bounded so an old silence does not stay in the queue forever. The pending
// entry is dropped when the window ends.
const PostLandAlarmWindow = 7 * 24 * time.Hour

// postLandPendingLimit is how many pending entries a rig keeps. An entry past it
// is dropped oldest first, and the drop is recorded in the same state
// (MainState.PostLandDropped), so the attention queue says entries were lost
// rather than the loss being a log line only.
const postLandPendingLimit = 200

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

// PostLandStoreFailure says the rig's pending-verdict store could not be read
// or written for PostLandStatusStoreFailureAfter passes running: no verdict is
// being reached, and the post-land safety net is blind until a pass reaches the
// store again.
type PostLandStoreFailure struct {
	Rig string
	// Error is the last failure, made safe for one line.
	Error string
}

// Key is the attention queue's stable identity: one per rig, so a failure
// raised again keeps the item it already has.
func (f PostLandStoreFailure) Key() string {
	return "post-land-store-failure:" + f.Rig
}

// Summary is the attention item's one line, naming the failure.
func (f PostLandStoreFailure) Summary() string {
	return fmt.Sprintf("the pending post-land verdicts for %s cannot be read or written, so no verdict is reached: %s",
		f.Rig, f.Error)
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
// reported through StoreFailure at once, so the silence is an attention item.
func (p *PostLandRunner) recordPending(ctx context.Context, pl PostLand, name string) {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	now := p.statusClock().Now()
	st, err := p.State.Load()
	if err != nil {
		p.unrecorded(pl, fmt.Errorf("reading the main state: %w", err))
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
		d := &PostLandDropped{Count: drop, Oldest: st.PostLandPending[0].Land.Commit, At: now}
		if prev := st.PostLandDropped; prev != nil && now.Before(prev.At.Add(PostLandAlarmWindow)) {
			d.Count += prev.Count
			d.Oldest = prev.Oldest
		}
		st.PostLandDropped = d
		st.PostLandPending = st.PostLandPending[drop:]
	}
	if err := p.State.Save(st); err != nil {
		p.unrecorded(pl, fmt.Errorf("saving the main state: %w", err))
		return
	}
	p.logf("pending at %s (%s): status %q is read on the next pass; nothing waits", short(pl.Commit), pl.by(), name)
}

// unrecorded is a landed commit whose pending verdict could not be written: no
// entry exists for the alarm to derive from, so the rig reports its store as
// failing at once (StoreFailure), not after several passes, until a pass reaches
// the store again.
func (p *PostLandRunner) unrecorded(pl PostLand, err error) {
	p.logf("WARNING could not record the pending verdict for %s (%s): %s", short(pl.Commit), pl.by(), sanitizeStatusText(err.Error()))
	p.storeMu.Lock()
	p.storeFailures = max(p.storeFailures+1, PostLandStatusStoreFailureAfter)
	p.storeLastErr = sanitizeStatusText(err.Error())
	p.storeMu.Unlock()
}

// PollPending reads every pending verdict once and decides what it can, without
// waiting on CI. A verdict that is green or red fires OnGreen or OnRed once for
// its landing and its entry is removed; an UNKNOWN read leaves the entry.
//
// A timeout is NOT an event of this pass. It is a condition of the durable
// entry: Since plus the configured timeout has passed. The entry stays in the
// store, is no longer read, and the attention collector derives its alarm from
// the store through Alarms, so there is no second store, no callback and no
// confirm-and-remove step to lose an alarm in. An entry is dropped only when its
// alarm window has ended, or, past postLandPendingLimit, oldest first with the
// drop recorded in the same state.
//
// The pass reads at most PostLandStatusPassBudget in all, entries least
// recently read first, so a long list neither holds the worker pass nor starves
// its newest entries. A store that cannot be read keeps every entry; after
// PostLandStatusStoreFailureAfter passes running StoreFailure says so. A store
// that cannot be written keeps the entries and does not fire a verdict twice.
func (p *PostLandRunner) PollPending(ctx context.Context) {
	if p.State == nil || ctx.Err() != nil {
		return
	}
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	st, err := p.State.Load()
	if err != nil {
		p.storeFailure(fmt.Errorf("reading the main state: %w", err))
		return
	}
	now := p.statusClock().Now()
	timeout := p.statusTimeout()
	entries := st.PostLandPending
	changed := false
	var decided []string
	var dropped *PostLandDropped

	// An entry whose alarm window has ended is dropped, with a log line.
	live := entries[:0:0]
	for _, e := range entries {
		if !now.Before(e.Since.Add(timeout).Add(PostLandAlarmWindow)) {
			p.logf("dropping the pending verdict for %s (%s): its alarm window ended", short(e.Land.Commit), e.Land.by())
			decided = append(decided, e.Land.Commit)
			changed = true
			continue
		}
		live = append(live, e)
	}
	entries = live
	// Past the bound the oldest entries go, and the drop is recorded in the
	// state the attention collector reads.
	if over := len(entries) - postLandPendingLimit; over > 0 {
		for _, e := range entries[:over] {
			p.logf("dropping the pending verdict for %s (%s): the rig keeps the newest %d", short(e.Land.Commit), e.Land.by(), postLandPendingLimit)
			decided = append(decided, e.Land.Commit)
		}
		dropped = &PostLandDropped{Count: over, Oldest: entries[0].Land.Commit, At: now}
		if prev := st.PostLandDropped; prev != nil && now.Before(prev.At.Add(PostLandAlarmWindow)) {
			dropped.Count += prev.Count
			dropped.Oldest = prev.Oldest
		}
		entries = entries[over:]
		changed = true
	}

	// The reads: due entries, least recently read first, inside one budget.
	order := make([]int, 0, len(entries))
	for i, e := range entries {
		if p.done[e.Land.Commit] || p.timedOut(e, now, timeout) {
			continue
		}
		if p.Status != nil && (e.LastRead.IsZero() || now.Sub(e.LastRead) >= PostLandStatusPollInterval) {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return entries[order[a]].LastRead.Before(entries[order[b]].LastRead) })
	passCtx, cancel := context.WithTimeout(ctx, p.passBudget())
	defer cancel()
	removed := map[int]bool{}
	var fires []func()
	for _, i := range order {
		if passCtx.Err() != nil {
			break
		}
		e := entries[i]
		pl, name := e.Land, e.Context
		res := p.readStatus(passCtx, pl, name)
		if passCtx.Err() != nil {
			break // the read was cut off by the budget or a stop: nothing learned
		}
		changed = true
		if res.Verdict != PostLandStatusUnknown {
			p.markDone(pl.Commit)
			decided = append(decided, pl.Commit)
			removed[i] = true
			fires = append(fires, func() { p.fire(ctx, pl, name, res) })
			continue
		}
		if res.State != e.LastState {
			p.logf("waiting at %s (%s): status %q is now %s: %s", short(pl.Commit), pl.by(), name, res.State, res.Description)
		}
		entries[i].LastRead, entries[i].LastState, entries[i].LastDescription = now, res.State, res.Description
	}
	// A done entry whose removal save failed earlier is finished here.
	keep := make([]PendingPostLand, 0, len(entries))
	for i, e := range entries {
		if removed[i] {
			continue
		}
		if p.done[e.Land.Commit] {
			changed = true
			decided = append(decided, e.Land.Commit) // its removal is saved below, which clears the marker
			continue
		}
		keep = append(keep, e)
	}
	if !changed {
		p.storeOK()
		return
	}
	for _, fire := range fires {
		fire()
	}
	if ctx.Err() != nil {
		// The daemon is stopping mid-pass. The entries stay in the store for
		// the restarted runner, and done keeps this process from deciding one
		// of them again.
		return
	}
	// Re-read before writing: the callbacks save this same main state (a green
	// verdict records LastGreen there, for one), and this save must carry
	// that through rather than write the state read before they ran.
	fresh, err := p.State.Load()
	if err != nil {
		p.storeFailure(fmt.Errorf("reading the main state back: %w", err))
		return
	}
	fresh.PostLandPending = keep
	if dropped != nil {
		fresh.PostLandDropped = dropped
	}
	if err := p.State.Save(fresh); err != nil {
		p.storeFailure(fmt.Errorf("saving the main state: %w", err))
		return
	}
	for _, sha := range decided {
		delete(p.done, sha)
	}
	p.storeOK()
}

// timedOut reports whether the entry's timeout has passed: a condition of the
// durable entry, derived from its Since and the configured timeout.
func (p *PostLandRunner) timedOut(e PendingPostLand, now time.Time, timeout time.Duration) bool {
	return !now.Before(e.Since.Add(timeout))
}

// passBudget bounds one pass's reads: a test's override, else
// PostLandStatusPassBudget.
func (p *PostLandRunner) passBudget() time.Duration {
	if p.passBudgetOverride > 0 {
		return p.passBudgetOverride
	}
	return PostLandStatusPassBudget
}

// PostLandAlarms is what the attention collector derives from a rig's durable
// pending entries: the timed-out verdicts still inside their alarm window, and
// the record of entries the bound dropped.
type PostLandAlarms struct {
	Unknown []PostLandUnknown
	Dropped *PostLandDropped
}

// Alarms reads the rig's pending entries and derives the alarms from them, with
// no state of its own: the same entries a restarted daemon finds give the same
// alarms. An unreadable store is an error, never zero alarms.
func (p *PostLandRunner) Alarms(now time.Time) (PostLandAlarms, error) {
	if p.State == nil {
		return PostLandAlarms{}, nil
	}
	st, err := p.State.Load()
	if err != nil {
		return PostLandAlarms{}, fmt.Errorf("reading the main state: %w", err)
	}
	timeout := p.statusTimeout()
	var out PostLandAlarms
	for _, e := range st.PostLandPending {
		at := e.Since.Add(timeout)
		if now.Before(at) || !now.Before(at.Add(PostLandAlarmWindow)) {
			continue
		}
		state := e.LastState
		if state == "" {
			state = "unread"
		}
		out.Unknown = append(out.Unknown, PostLandUnknown{
			Rig: p.Rig, SHA: e.Land.Commit, Context: e.Context, State: state, At: at,
		})
	}
	if d := st.PostLandDropped; d != nil && now.Before(d.At.Add(PostLandAlarmWindow)) {
		out.Dropped = d
	}
	return out, nil
}

// markDone records that commit's verdict already fired, so a store write that
// fails does not fire it again on the next pass.
func (p *PostLandRunner) markDone(commit string) {
	if p.done == nil {
		p.done = map[string]bool{}
	}
	p.done[commit] = true
}

// storeFailure counts a pass that could not read or write the pending verdicts,
// keeping them for the next pass.
func (p *PostLandRunner) storeFailure(err error) {
	p.storeMu.Lock()
	p.storeFailures++
	p.storeLastErr = sanitizeStatusText(err.Error())
	n, msg := p.storeFailures, p.storeLastErr
	p.storeMu.Unlock()
	p.logf("WARNING %s cannot reach the pending post-land verdicts (%d passes running); every entry is kept for the next pass: %s", p.Rig, n, msg)
}

// storeOK records a pass that reached the store.
func (p *PostLandRunner) storeOK() {
	p.storeMu.Lock()
	was := p.storeFailures
	p.storeFailures, p.storeLastErr = 0, ""
	p.storeMu.Unlock()
	if was >= PostLandStatusStoreFailureAfter {
		p.logf("%s reaches its pending post-land verdicts again", p.Rig)
	}
}

// StoreFailure is the rig's blind-safety-net record once
// PostLandStatusStoreFailureAfter passes running have failed to read or write the
// pending verdicts, nil otherwise. The attention collector reads it.
func (p *PostLandRunner) StoreFailure() *PostLandStoreFailure {
	p.storeMu.Lock()
	defer p.storeMu.Unlock()
	if p.storeFailures < PostLandStatusStoreFailureAfter {
		return nil
	}
	return &PostLandStoreFailure{Rig: p.Rig, Error: p.storeLastErr}
}

// readStatus reads the landed commit's combined status once, bounded by
// PostLandStatusReadTimeout, with its text made safe for a bead comment, an
// attention summary and a log line.
func (p *PostLandRunner) readStatus(ctx context.Context, pl PostLand, name string) PostLandStatusResult {
	readCtx, cancel := context.WithTimeout(ctx, p.readTimeout())
	defer cancel()
	cs, err := p.Status(readCtx, pl)
	res := postLandStatusRead(cs, err, name, pl.Commit)
	res.State = sanitizeStatusText(res.State)
	res.Description = sanitizeStatusText(res.Description)
	return res
}

// readTimeout bounds one status read: a test's override, else
// PostLandStatusReadTimeout.
func (p *PostLandRunner) readTimeout() time.Duration {
	if p.readTimeoutOverride > 0 {
		return p.readTimeoutOverride
	}
	return PostLandStatusReadTimeout
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
