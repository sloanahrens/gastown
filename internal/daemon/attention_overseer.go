package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/overseer"
	"github.com/steveyegge/gastown/internal/question"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// The silent-overseer rule (gt-3a6h6): when the overseer's heartbeat is older
// than overseer.silent_after while dispatchable work or a question waits on it,
// the collector raises overseer.silent as info and pushes to the person once.
// It acts on no session: it restarts, kills and unhooks nothing.

// overseerTickFresh is how old the spec dispatcher's newest tick may be and
// still say whether ready work exists. The ticker runs every minute; past this
// the ticker is off or stuck, and an old candidate count is not a fact about
// now.
const overseerTickFresh = 10 * time.Minute

// overseerReads are the outside reads and the one write the silent-overseer
// collector makes.
type overseerReads struct {
	// heartbeat reads heartbeat.json; have is false before the first beat.
	heartbeat func() (hb overseer.Heartbeat, have bool, err error)
	// silentAfter is the window past which the heartbeat is stale.
	silentAfter time.Duration
	// readyWork is how many ready beads the spec dispatcher's newest tick
	// found, or 0 when it has no tick fresh enough to say.
	readyWork func(now time.Time) int
	// pushName is the notify command the push goes through; "" pushes nothing.
	pushName string
	push     func(name, title, body string) error
	// pushed and markPushed are the once-per-period marker.
	pushed     func() (overseer.SilentPush, bool, error)
	markPushed func(overseer.SilentPush) error
}

// productionOverseerReads reads the overseer's files under the town root, the
// dispatcher's ticks from memory, and pushes through notify.
func (d *Daemon) productionOverseerReads() overseerReads {
	root := d.config.TownRoot
	cfg := d.loadOperationalConfig().GetOverseerConfig()
	return overseerReads{
		heartbeat:   func() (overseer.Heartbeat, bool, error) { return overseer.ReadHeartbeat(root) },
		silentAfter: cfg.SilentAfterD(),
		readyWork:   func(now time.Time) int { return readyWorkOfTicks(d.dispatchTickRecords(), now) },
		pushName:    cfg.SilentPushName(),
		push: func(name, title, body string) error {
			return notify.PushIn(root, name, title, body)
		},
		pushed:     func() (overseer.SilentPush, bool, error) { return overseer.ReadSilentPush(root) },
		markPushed: func(p overseer.SilentPush) error { return overseer.WriteSilentPush(root, p) },
	}
}

// readyWorkOfTicks is the candidate count of the newest tick, or 0 when that
// tick is older than overseerTickFresh. Candidates are the ready beads the
// dispatcher considered after its filters, so a bead held for a person or
// parked by a ruling does not count.
func readyWorkOfTicks(ticks []townhealth.DispatchTick, now time.Time) int {
	if len(ticks) == 0 {
		return 0
	}
	latest := ticks[len(ticks)-1]
	if now.Sub(latest.At) > overseerTickFresh {
		return 0
	}
	return latest.Candidates
}

// collectOverseerSilent raises one item while the overseer is silent and work
// waits. read is the shared question snapshot; the collector asks for it only
// when the heartbeat is stale and no ready work already answers the question,
// and the summary names the first thing it found.
func (s *attentionSources) collectOverseerSilent(ctx context.Context, read func(context.Context) (*questionSnapshot, error)) ([]attention.Item, error) {
	o := s.overseer
	if o.heartbeat == nil {
		return nil, nil
	}
	hb, have, err := o.heartbeat()
	if err != nil {
		return nil, err
	}
	if !overseer.Silent(hb, have, s.now, o.silentAfter) {
		return nil, nil
	}
	ready := 0
	if o.readyWork != nil {
		ready = o.readyWork(s.now)
	}
	asked := 0
	if ready == 0 {
		snap, err := read(ctx)
		if err != nil {
			return nil, err
		}
		asked = overseerOwedQuestions(snap)
	}
	if ready == 0 && asked == 0 {
		return nil, nil
	}

	summary := overseerSilentSummary(hb, have, s.now, o.silentAfter, ready, asked)
	s.pushOverseerSilent(o, hb, summary)
	return []attention.Item{{
		Key:      "overseer:silent",
		Kind:     attention.KindOverseerSilent,
		Severity: attention.SeverityLow,
		Summary:  summary,
	}}, nil
}

// overseerOwedQuestions counts the open questions the overseer owes a move on:
// none commented on yet, or answered by Sloan and not yet acted on. A question
// whose newest comment is the overseer's waits on Sloan, not on the overseer.
func overseerOwedQuestions(snap *questionSnapshot) int {
	n := 0
	for _, q := range snap.questions {
		if len(q.comments) == 0 || question.Answered(q.comments) {
			n++
		}
	}
	return n
}

// pushOverseerSilent pushes the silent summary once per silent period. A
// period is a heartbeat that stays stale: the marker names that heartbeat's
// time, so a flapping work count, a restart and a failed collector read cannot
// push twice, and a new beat that goes stale again can. A push that fails is
// not marked, so the next tick tries again.
func (s *attentionSources) pushOverseerSilent(o overseerReads, hb overseer.Heartbeat, summary string) {
	if o.pushName == "" || o.push == nil || o.pushed == nil || o.markPushed == nil {
		return
	}
	marker, have, err := o.pushed()
	if err != nil {
		// An unreadable marker cannot say whether this period was pushed, and
		// pushing blind would repeat every tick while the file stays broken.
		s.d.logger.Printf("attention: overseer.silent: reading the push marker: %v", err)
		return
	}
	if have && marker.Heartbeat.Equal(hb.TS) {
		return
	}
	if err := o.push(o.pushName, "Overseer is silent", summary); err != nil {
		s.d.logger.Printf("attention: overseer.silent: push through %q failed: %v", o.pushName, err)
		return
	}
	if err := o.markPushed(overseer.SilentPush{Heartbeat: hb.TS, PushedAt: s.now}); err != nil {
		s.d.logger.Printf("attention: overseer.silent: writing the push marker: %v", err)
	}
}

// overseerSilentSummary is the item's one line and the push's body: how long
// the overseer has been quiet and what waits.
func overseerSilentSummary(hb overseer.Heartbeat, have bool, now time.Time, after time.Duration, ready, asked int) string {
	quiet := fmt.Sprintf("no overseer heartbeat (window %s)", townhealth.Short(after))
	if have {
		quiet = fmt.Sprintf("overseer silent %s, last %s", townhealth.Short(now.Sub(hb.TS)), hb.State)
	}
	if ready > 0 {
		return fmt.Sprintf("%s; %d ready bead(s) waiting", quiet, ready)
	}
	return fmt.Sprintf("%s; %d question(s) waiting", quiet, asked)
}
