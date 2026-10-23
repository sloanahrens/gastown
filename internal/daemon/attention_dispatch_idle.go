package daemon

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The idle-dispatcher rule (gt-wil0h): dispatch.idle holds while the spec
// dispatcher's last attentionDispatchIdleTicks ticks each had a free seat and
// a dispatchable bead and started nothing. Every other rule watches work that
// exists; this one watches work that is not being started, such as a standoff
// that leaves every seat idle with candidates waiting. It reads the tick
// report the ticker already parses, so it makes no bd or git call.
const (
	// attentionDispatchIdleTicks is how many consecutive idle ticks raise the
	// rule: about five minutes at the default 60s interval.
	attentionDispatchIdleTicks = 5
	// attentionDispatchIdleFresh is how old the newest tick may be at the
	// default cadence and still have the streak describe now. Past the bound
	// the ticker is off or failing, and a stale streak would hold the item for
	// good. A ticker that runs slower than the default gets its own cadence's
	// bound instead, which is never shorter than this one (gt-icmdq).
	attentionDispatchIdleFresh = 10 * time.Minute
	// attentionDispatchIdleGapIntervals is how many of the ticker's own intervals
	// two ticks may be apart and still count as consecutive (gt-icmdq).
	attentionDispatchIdleGapIntervals = 5
	// attentionDispatchIdleReasons is how many skip reasons the summary names.
	attentionDispatchIdleReasons = 3
)

// dispatchIdleKey is the one item the rule raises.
const dispatchIdleKey = "dispatch:idle"

// dispatchIdleStatus is the tracker's reading: the run of idle ticks ending at
// the newest tick, and what the newest of them found.
type dispatchIdleStatus struct {
	// Streak is the consecutive idle ticks ending at the newest tick; 0 when
	// that tick was not idle.
	Streak int
	// At is when the newest tick was observed.
	At time.Time
	// Fresh is how old At may be and still have the streak describe now: the
	// ticker's own cadence, scaled, so a slow ticker's streak is not read as
	// stale between two of its ticks (gt-icmdq).
	Fresh time.Duration
	// Roster is the newest idle tick's roster, as the tick printed it.
	Roster string
	// Waiting is how many dispatchable beads that tick left undispatched.
	Waiting int
	// Reasons are those beads' skip reasons with their bead counts, most
	// common first.
	Reasons []dispatchIdleReason
}

type dispatchIdleReason struct {
	Reason string
	Beads  int
}

// dispatchIdleTracker counts idle ticks. The ticker goroutine feeds it and
// the heartbeat reads it.
type dispatchIdleTracker struct {
	mu sync.Mutex
	// interval is the ticker's cadence, set where the ticker starts. The
	// consecutive-tick gap scales with it, so a ticker slower than the default
	// still has consecutive ticks (gt-icmdq).
	interval time.Duration
	st       dispatchIdleStatus
}

// setInterval records the cadence the ticker runs at, before its first tick.
func (t *dispatchIdleTracker) setInterval(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.interval = d
}

// gap is how long the ticker may be quiet between two ticks and still have them
// count as consecutive: attentionDispatchIdleGapIntervals of its own intervals.
// A fixed five minutes would end the streak on every tick of a slower ticker,
// and the idle alarm could never raise at all (gt-icmdq). The caller holds t.mu
// — taking it here would deadlock against Observe and Status.
func (t *dispatchIdleTracker) gap() time.Duration {
	interval := t.interval
	if interval <= 0 {
		interval = defaultSpecDispatchInterval
	}
	return attentionDispatchIdleGapIntervals * interval
}

// fresh is how old the newest tick may be and still describe now. The floor
// holds a ticker at the default cadence to the same bound as before; a slower
// one is held to its own gap, which is longer, or its last tick would read as
// stale for most of every interval (gt-icmdq). The caller holds t.mu.
func (t *dispatchIdleTracker) fresh() time.Duration {
	if g := t.gap(); g > attentionDispatchIdleFresh {
		return g
	}
	return attentionDispatchIdleFresh
}

// Observe folds one tick report into the streak. A tick is idle when the
// roster shows a free seat, a dispatchable bead waited and nothing was
// dispatched. Any other tick ends the streak: a held tick decided nothing on
// purpose, and a roster that cannot be read could be hiding a full town. A tick
// arriving more than gap after the last one ends it too: the ticker was parked,
// failing or behind in between, and the idle ticks either side of that gap are
// not consecutive (gt-icmdq).
func (t *dispatchIdleTracker) Observe(r specDispatchTickReport, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.st.At.IsZero() && at.Sub(t.st.At) > t.gap() {
		t.st.Streak = 0
	}
	t.st.At = at
	if r.Hold != "" || len(r.Dispatched) > 0 || !rosterHasFreeSeat(r.Roster) {
		t.st.Streak = 0
		return
	}
	waiting, reasons := dispatchableWaiting(r)
	if waiting == 0 {
		t.st.Streak = 0
		return
	}
	t.st.Streak++
	t.st.Roster, t.st.Waiting, t.st.Reasons = r.Roster, waiting, reasons
}

// Reset ends the streak: the ticker did not tick, so the run of idle ticks
// ending at the last observation is over (gt-icmdq). The recorded tick keeps
// its time, which is what the freshness check reads.
func (t *dispatchIdleTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st.Streak = 0
}

// Status returns the tracker's current reading, with the freshness bound its
// cadence gives it: the reader cannot judge the streak without both.
func (t *dispatchIdleTracker) Status() dispatchIdleStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.st
	st.Fresh = t.fresh()
	return st
}

// rosterHasFreeSeat reports whether any seat in the roster has room. An
// unreadable roster has none, so the tick cannot count as idle.
func rosterHasFreeSeat(roster string) bool {
	seats, unreadable := dispatchRosterSeats(roster)
	if unreadable {
		return false
	}
	for _, s := range seats {
		if s.Live < s.Cap {
			return true
		}
	}
	return false
}

// dispatchableWaiting counts the beads a tick could have started and did not,
// and groups them by skip reason. A bead out of reach for a reason a free seat
// does not change (unshaped, held for a person, a container, a bad formula, no
// seat of its class) is not waiting on the dispatcher; lint refusals and
// planning holds are separate lists and never counted. Failed slings count,
// since the bead was dispatchable and the seat stayed idle.
func dispatchableWaiting(r specDispatchTickReport) (int, []dispatchIdleReason) {
	counts := map[string]int{}
	waiting := 0
	for _, e := range append(append([]specDispatchTickEntry(nil), r.Skipped...), r.Failed...) {
		reason := specSkipReason(e)
		if !dispatchableSkip(reason) {
			continue
		}
		waiting++
		counts[dispatchIdleReasonKey(reason)]++
	}
	reasons := make([]dispatchIdleReason, 0, len(counts))
	for k, n := range counts {
		reasons = append(reasons, dispatchIdleReason{Reason: k, Beads: n})
	}
	sort.Slice(reasons, func(i, j int) bool {
		if reasons[i].Beads != reasons[j].Beads {
			return reasons[i].Beads > reasons[j].Beads
		}
		return reasons[i].Reason < reasons[j].Reason
	})
	return waiting, reasons
}

// notDispatchableReasons are the skip-reason prefixes of a bead the dispatcher
// is not holding back: it cannot be started until a person or a seat of its
// class changes. They match the reason text spec dispatch writes.
//
// specdispatch.SkipStartupBackoff is not one of them: the rest after a failed
// session start ends by itself, so the bead waits on the dispatcher and the
// rule should name the failure rather than stay quiet.
var notDispatchableReasons = []string{
	specdispatch.SkipUnshaped,
	specdispatch.SkipNotEligible,
	specdispatch.SkipUnknownFormula,
	specdispatch.SkipBadFormulaVars,
	specdispatch.SkipConflictingFormulaLabels,
	specdispatch.SkipBadFormulaLabel,
	specdispatch.SkipHeld + specdispatch.SkipContainer,
	specdispatch.SkipNoSeat,
	specdispatch.SkipRigHold,
	specdispatch.SkipPerTickLimit,
}

func dispatchableSkip(reason string) bool {
	for _, p := range notDispatchableReasons {
		if strings.HasPrefix(reason, p) {
			return false
		}
	}
	return true
}

// beadIDInReason matches a bead id (a 2-3 letter prefix and five characters)
// so the same standoff over different beads reads as one reason.
var beadIDInReason = regexp.MustCompile(`\b[a-z]{2,3}-[a-z0-9]{5}\b`)

// dispatchIdleReasonKey reduces a skip reason to the part that names the
// cause: bead ids become <bead>, and the text after the cause's own colon (an
// error tail, the paths a bead names) is dropped.
func dispatchIdleReasonKey(reason string) string {
	r := beadIDInReason.ReplaceAllString(reason, "<bead>")
	if i := strings.Index(r, " and names "); i >= 0 {
		r = r[:i]
	}
	prefix := ""
	if rest, ok := strings.CutPrefix(r, "held: "); ok {
		prefix, r = "held: ", rest
	}
	if i := strings.Index(r, ": "); i >= 0 {
		r = r[:i]
	}
	return prefix + r
}

// collectDispatchIdle raises dispatch.idle while the tracker's streak has
// reached attentionDispatchIdleTicks. It raises nothing under an operator
// hold, since an idle dispatcher is what the hold asked for, and nothing from
// a streak whose newest tick is older than the ticker's own cadence allows.
func (s *attentionSources) collectDispatchIdle(ctx context.Context) ([]attention.Item, error) {
	if s.dispatchIdle == nil {
		return nil, nil
	}
	if s.dispatchHold != nil && s.dispatchHold() != "" {
		return nil, nil
	}
	st := s.dispatchIdle()
	if st.Streak < attentionDispatchIdleTicks || s.now.Sub(st.At) > st.Fresh {
		return nil, nil
	}
	return []attention.Item{{
		Key:      dispatchIdleKey,
		Kind:     attention.KindDispatchIdle,
		Severity: attention.SeverityHigh,
		Summary:  dispatchIdleSummary(st),
	}}, nil
}

// dispatchIdleSummary names the idle ticks, the roster and the commonest skip
// reasons, so a standoff reads as one.
func dispatchIdleSummary(st dispatchIdleStatus) string {
	named := make([]string, 0, attentionDispatchIdleReasons)
	for i, r := range st.Reasons {
		if i == attentionDispatchIdleReasons {
			break
		}
		named = append(named, fmt.Sprintf("%s x%d", r.Reason, r.Beads))
	}
	return fmt.Sprintf("dispatcher idle for %d ticks: roster %s has a free seat, %d dispatchable bead(s) waiting; top skips: %s",
		st.Streak, st.Roster, st.Waiting, strings.Join(named, "; "))
}
