package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// The attention queue's thresholds (gt-vsct7.2). They are compiled in, not
// config keys: the queue's job is to raise the conditions a human would have
// written a threshold for, and a threshold an operator has to set is one more
// thing that is wrong on the day it matters.
const (
	// attentionSlotHeld is how long a container-build slot may stay held
	// before it is an item. A suite holds it for a few minutes; 25 minutes of
	// hold means the suite is wedged or its holder died without releasing.
	attentionSlotHeld = 25 * time.Minute
	// attentionBDSlow is how slow one bd read may be before the town's bead
	// reads are an item.
	attentionBDSlow = 5 * time.Second
	// attentionRejectedTwice is the rejection count at which a bead is an
	// item. One refusal is the landing loop working; two is the loop failing
	// on the same bead.
	attentionRejectedTwice = 2
	// attentionBeadTimeout bounds one bead read inside a collector. It is the
	// daemon's own read bound, deliberately shorter than townHealthTimeout:
	// a read that will not answer is a collector failure this tick.
	attentionBeadTimeout = 30 * time.Second
	// attentionTimeout bounds one attention tick's collectors.
	attentionTimeout = 2 * time.Minute
	// attentionAuditShareDivisor splits that budget: the resolved-FAIL audit
	// runs before the collectors, under the tick's context, so an audit whose
	// bd reads stall would otherwise spend the whole budget and leave the
	// collectors an expired context and the queue a tick stale. It gets one
	// part and the collectors the rest.
	attentionAuditShareDivisor = 4
	// attentionRefusalWindow is how long a gt done refusal stays an item. A
	// refusal older than this has either been acted on or outlived its
	// branch; keeping it forever would turn the queue into a history file.
	attentionRefusalWindow = 48 * time.Hour
)

// The landing-pipeline thresholds (gt-vsct7.3), compiled in like the queue's
// others.
const (
	// attentionPolecatStall is how long a running seat's progress evidence may
	// be unchanged before it is an item. It is deliberately shorter than
	// liveness.DefaultStallAfter (30m), which gates restarts: this item asks a
	// human to look, it does not kill anything (gt-vsct7.3).
	attentionPolecatStall = 10 * time.Minute
	// attentionDirectPushHold is how long a direct-push item holds after the
	// tip was first observed. An unacked push is forgotten after a day rather
	// than holding for good.
	attentionDirectPushHold = 24 * time.Hour
	// attentionLandingRecords is how many of a rig's most recent landing
	// records the direct-push collector searches for the tip it saw. The tip
	// moved recently by definition, so its record is at the end of the file.
	attentionLandingRecords = 50
	// attentionRiskPathWindow is how long a landing that touched a risk path
	// stays an item. A human is asked to look; a week is long enough to act
	// and short enough that the queue does not become a history file
	// (gt-vsct7.4).
	attentionRiskPathWindow = 7 * 24 * time.Hour
)

// landingState is one rig's landing state as the collectors read it
// (gt-vsct7.3): the bead a landing is on, when it started, and the stage it
// is running. The zero value is "no bead in flight".
type landingState struct {
	// bead is the bead the rig's landing pass is landing; "" between beads and
	// when no pass is running.
	bead string
	// since is when bead became the in-flight landing.
	since time.Time
	// stage is the landing stage the pass is running, by the land.Stage*
	// names; "" before the gate reports its stage (gt-84gcp).
	stage string
	// stageSince is when stage started. The landing-stuck alarm judges a
	// reported stage by its own clock, not the bead's, so a long but healthy
	// gate does not make the review that follows it look wedged.
	stageSince time.Time
}

// landingStates is the daemon's per-rig landing state. The landing worker
// writes its own rig's entry on its own goroutine and the attention tick reads
// every entry on the heartbeat goroutine, so this map is the one
// synchronization point between them. The zero value is ready to use.
type landingStates struct {
	m sync.Map
}

// get returns rig's state, or the zero value when nothing has been recorded.
func (l *landingStates) get(rig string) landingState {
	v, ok := l.m.Load(rig)
	if !ok {
		return landingState{}
	}
	p, _ := v.(landingState)
	return p
}

func (l *landingStates) put(rig string, p landingState) { l.m.Store(rig, p) }

// beginPass marks a landing pass as running with no bead in flight yet.
func (l *landingStates) beginPass(rig string) {
	p := l.get(rig)
	p.bead, p.since, p.stage, p.stageSince = "", time.Time{}, "", time.Time{}
	l.put(rig, p)
}

// setBead records a bead entering (id != "") or leaving (id == "") the
// in-flight landing. A bead entering flight has no stage yet; the landing
// reports one when it reaches the gate.
func (l *landingStates) setBead(rig, id string, now time.Time) {
	p := l.get(rig)
	p.bead, p.since, p.stage, p.stageSince = id, time.Time{}, "", time.Time{}
	if id != "" {
		p.since = now
	}
	l.put(rig, p)
}

// setStage records the stage the in-flight landing has reached. The landing
// loop's own goroutine calls it, so the bead in flight is the one reporting.
func (l *landingStates) setStage(rig, stage string, now time.Time) {
	p := l.get(rig)
	p.stage, p.stageSince = stage, now
	l.put(rig, p)
}

// endPass records the end of a landing pass.
func (l *landingStates) endPass(rig string) {
	p := l.get(rig)
	p.bead, p.since, p.stage, p.stageSince = "", time.Time{}, "", time.Time{}
	l.put(rig, p)
}

// each calls fn with every rig in flight, in no order.
func (l *landingStates) each(fn func(rig, bead string)) {
	l.m.Range(func(k, v any) bool {
		p, _ := v.(landingState)
		if p.bead != "" {
			fn(k.(string), p.bead)
		}
		return true
	})
}

// attentionCache is what the daemon carries between attention ticks: the
// state it wrote last, so an item keeps its first_seen and a failed collector
// can keep its kind's items, and the rejection-note cache.
//
// It is read and written only on the heartbeat goroutine.
type attentionCache struct {
	state         attention.State
	reworkNotes   map[string]reworkNote
	diskWarnBeats int
	// dockerProbeFails is how many consecutive ticks docker ps has failed.
	dockerProbeFails int
	// repeatedAt is when each repeating item (attention.Repeats) was last
	// said. It is memory only: after a restart an item's clock starts again.
	repeatedAt map[string]time.Time
	// backlogSamples is the review-backlog rule's one-hour count history.
	backlogSamples []backlogSample
	// riskSkipped is the work beads the risk-path collector skipped last tick
	// because their read failed, so a bead still failing is not logged again.
	riskSkipped map[string]bool
}

// reworkNote is one rework bead's cached rejection count, keyed by the
// updated_at the notes were read at so a collector re-reads only a bead that
// changed since.
type reworkNote struct {
	updatedAt string
	count     int
}

// attentionCollector is one source of items: the kind it reports, and the
// read that returns every item of that kind holding now.
//
// A collector that returns an error contributes nothing this tick and keeps
// the previous tick's items of its kind (see collectAttention); a collector
// that returns no items clears its kind.
type attentionCollector struct {
	kind    attention.Kind
	collect func(ctx context.Context) ([]attention.Item, error)
}

// attentionSources are the outside reads the collectors make. A nil func is
// the production read; tests replace the ones that reach outside the town.
type attentionSources struct {
	d *Daemon
	// now is this tick's time, the age every collector judges against.
	now time.Time
	// report is this tick's town-health report (the daemon's lastTownHealth),
	// the bd-slow collector's second trigger.
	report *townhealth.Report

	landingRigs func() []string
	redMain     func(ctx context.Context, rig string) ([]*beads.Issue, error)
	escalations func(ctx context.Context) ([]*beads.Issue, error)
	rework      func(ctx context.Context, rig string) ([]*beads.Issue, error)
	reworkNote  func(ctx context.Context, rig, id string) (string, error)
	slots       func() (slot.Report, error)
	pidAlive    func(pid int) bool
	bdLatency   func(ctx context.Context) (time.Duration, error)
	// escalationBead reads the work bead an escalation record names in its
	// related_bead field: the owner the item names comes from it.
	escalationBead func(ctx context.Context, id string) (*beads.Issue, error)

	// refusals is gt done's refusal ledger; refusalBead reads one refused
	// bead in its own rig.
	refusals    func() ([]attention.Refusal, error)
	refusalBead func(ctx context.Context, rig, id string) (*beads.Issue, error)
	// landingState reads the rig's in-flight landing, the landing-stuck
	// collector's subject.
	landingState func(rig string) landingState
	// readyToLand reads the rig's actionable gt:ready-to-land beads and
	// reports the oldest submission among them, the age queue-stuck judges.
	readyToLand func(ctx context.Context, rig string) (readyQueue, error)
	// tierSweep reads one rig's tier-sweep record (tier_sweep.go), and
	// tierSweepRigs is the rig set the sweep covers — none when the patrol is
	// off.
	tierSweep     func(rig string) (tierSweepState, error)
	tierSweepRigs func() []string
	// seats lists the town's seats: the same walk townhealth judges, which has
	// already dropped every seat whose sample is missing or stale.
	seats func() ([]townhealth.Seat, error)
	// seatWork reports whether the seat holds assigned open work.
	seatWork func(rig, name string) (bool, error)
	// remoteTip reads the rig's landing target from origin with git ls-remote.
	// It never fetches: the rig repo is the landing worker's and
	// <town>/gastown/mayor/rig is install-gt's, so a fetch here would race them.
	remoteTip func(rig string) (string, error)
	// landedCommit reports whether the rig's landings file records sha.
	landedCommit func(rig, sha string) (bool, error)
	// commitInfo describes a commit in the rig repo: its author and subject.
	// An unreadable commit is empty strings, not an error — a direct push's tip
	// is often not in the local repo yet.
	commitInfo func(rig, sha string) (author, subject string)
	// tips is the direct-push collector's persisted tip state for this tick.
	// Already loaded and written back by writeAttention.
	tips *directPushTips

	// riskLandings reads a rig's landing records in a window; riskIssue reads
	// one work bead — the notes that carry its review lines and the priority its
	// item's severity is judged on. Together they are the risk-path collector's
	// two outside reads (gt-vsct7.4).
	riskLandings func(rig string, since time.Time) ([]land.LandingRecord, error)
	riskIssue    func(ctx context.Context, rig, id string) (*beads.Issue, error)
	// findingIssue reads one finding bead — the status and the close reason
	// that resolved it — and riskAppend appends a review line to a work bead's
	// notes: the resolved-FAIL pass's read and its only write
	// (attention_risk_audit.go). A nil func turns the pass off.
	findingIssue func(ctx context.Context, id string) (*beads.Issue, error)
	riskAppend   func(ctx context.Context, rig, id, note string) error
	// riskOpen is how many risk-path items collectRiskPaths observed this tick,
	// and riskCounted whether it got as far as counting: the review-backlog
	// collector, which runs after it, reads both.
	riskOpen    int
	riskCounted bool
	// backlog is the review-backlog rule's limits, from the operational config.
	backlog backlogLimits
	// backlogSamples is the open risk-path count of each tick in the last hour,
	// carried across ticks by the daemon. It is memory only, so after a restart
	// the rise is measured from the first tick. collectReviewBacklog updates it.
	backlogSamples []backlogSample
	// riskSkipped is the work beads collectRiskPaths skipped on the last tick
	// because their read failed, carried across ticks by the daemon.
	riskSkipped map[string]bool

	// reworkNotes is the rejection-count cache, carried across ticks by the
	// daemon.
	reworkNotes map[string]reworkNote

	// host is the probes the host collectors read (attention_host.go).
	host hostProbes
	// prev is the state the tick reconciles against, set by attentionTick. A
	// host collector whose probe failed keeps its rules' previous items from
	// it, so an unanswered probe does not clear a real alarm.
	prev attention.State
	// diskWarnBeats is how many consecutive ticks host.disk_warn has held,
	// carried across ticks by the daemon. attentionTick updates it.
	diskWarnBeats int
	// dockerProbeFails is how many consecutive ticks docker ps has failed,
	// carried across ticks by the daemon. collectHostDocker updates it.
	dockerProbeFails int

	// questions is the overseer-question collectors' read of the town
	// database (attention_questions.go).
	questions questionReads
	// overseer is the silent-overseer collector's reads (attention_overseer.go).
	overseer overseerReads
	// dispatchIdle reads the idle-dispatch tracker and dispatchHold the
	// operator hold; both are the idle-dispatcher collector's reads
	// (attention_dispatch_idle.go).
	dispatchIdle func() dispatchIdleStatus
	dispatchHold func() string
	// repeatedAt is when each repeating item was last said, carried across
	// ticks by the daemon. attentionTick updates it.
	repeatedAt map[string]time.Time
}

// writeAttention is the heartbeat step after townhealth (gt-vsct7.2): it runs
// every collector, folds the items holding now into the queue and writes
// state.json. It is a non-lifecycle step, so it runs under the town E-stop
// too: the queue is a report about the town, and an E-stopped town still
// wants one.
func (d *Daemon) writeAttention() {
	d.writeAttentionWith(attentionTimeout, d.attentionSources)
}

// writeAttentionWith is writeAttention over a tick of the given budget and the
// reads sources builds, so a test can drive a stalling read against a short
// budget instead of the real one.
func (d *Daemon) writeAttentionWith(budget time.Duration, sources func(now time.Time) *attentionSources) {
	cache := d.lastAttention
	if cache == nil {
		cache = &attentionCache{}
	}
	// The previous state is carried in memory across ticks and read back from
	// state.json after a daemon restart, so an item keeps its first_seen
	// across both.
	prev := cache.state
	if prev.Updated.IsZero() {
		st, err := attention.ReadState(d.config.TownRoot)
		if err != nil {
			d.logger.Printf("attention: reading state: %v", err)
		} else {
			prev = st
		}
	}
	acks, err := attention.ReadAcks(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("attention: reading acks: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	now := d.clk().Now()
	src := sources(now)
	if cache.reworkNotes != nil {
		src.reworkNotes = cache.reworkNotes
	}
	src.diskWarnBeats = cache.diskWarnBeats
	src.dockerProbeFails = cache.dockerProbeFails
	src.repeatedAt = cache.repeatedAt
	src.backlogSamples = cache.backlogSamples
	src.riskSkipped = cache.riskSkipped
	tips, err := readDirectPushTips(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("attention: reading %s: %v", tipsFileName, err)
	}
	src.tips = &tips
	// The one write of the attention pass runs before the collectors, so a FAIL
	// whose findings closed leaves the queue on this tick. It runs under its
	// own share of the tick's budget: its bd reads are unbounded by the number
	// of landings, and the collectors behind it must still get a live context
	// (gt-0h4i0).
	auditShare := budget / attentionAuditShareDivisor
	auditCtx, auditCancel := context.WithTimeout(ctx, auditShare)
	wrote, err := src.auditResolvedRiskFails(auditCtx)
	auditOut := auditCtx.Err()
	auditCancel()
	for _, w := range wrote {
		d.logger.Printf("attention: audited resolved risk-path FAIL %s", w)
	}
	switch {
	case ctx.Err() == nil && errors.Is(auditOut, context.DeadlineExceeded):
		// The audit ran out of its own share, not the tick's: the collectors
		// below still have the rest, and the findings it did not reach wait
		// for the next tick.
		d.logger.Printf("attention: auditing resolved risk-path FAILs stopped at its %s share of the tick (%d audited): %v", auditShare, len(wrote), err)
	case err != nil:
		d.logger.Printf("attention: auditing resolved risk-path FAILs: %v", err)
	}
	state := d.attentionTick(ctx, src, prev, acks, now)
	d.lastAttention = &attentionCache{state: state, reworkNotes: src.reworkNotes, diskWarnBeats: src.diskWarnBeats, dockerProbeFails: src.dockerProbeFails, repeatedAt: src.repeatedAt, backlogSamples: src.backlogSamples, riskSkipped: src.riskSkipped}
	if len(src.tips.Rigs) == 0 {
		return
	}
	// The observed tips and the raised pushes are written after the tick that
	// read them, so a daemon restart judges the next tip against the one the
	// last tick saw and keeps an unacked push (gt-vsct7.3).
	if err := writeDirectPushTips(d.config.TownRoot, *src.tips, now); err != nil {
		d.logger.Printf("attention: writing %s: %v", tipsFileName, err)
	}
}

// attentionTick runs the collectors, reconciles what they found against prev
// and the acks, and writes the queue — and with it the acks whose items are
// still in the queue. It returns the state the daemon carries into the next
// tick.
//
// The transitions land before the state they were derived from: a state that
// advances without its events loses them, because the next tick reconciles
// against the state it now holds, sees no change, and emits nothing
// (gt-2y4b7). An append that fails therefore hands back prev, and the next
// tick re-derives the same transitions rather than losing them.
func (d *Daemon) attentionTick(ctx context.Context, src *attentionSources, prev attention.State, acks attention.Acks, now time.Time) attention.State {
	src.prev = prev
	observed := collectAttention(ctx, prev, src.collectors(), d.logger.Printf)
	res := attention.Reconcile(prev, observed, acks, now)
	var beats int
	var promoted []attention.Event
	res.State, beats, promoted = diskWarnPromotion(res.State, src.diskWarnBeats, now)
	res.Events = append(res.Events, promoted...)
	var riskPromoted []attention.Event
	res.State, riskPromoted = attention.RiskPathPromotion(res.State, now)
	res.Events = append(res.Events, riskPromoted...)
	repeatedAt, repeats := attention.Repeats(res.State, res.Events, src.repeatedAt, now)
	res.Events = append(res.Events, repeats...)
	if err := attention.AppendEvents(d.config.TownRoot, res.Events); err != nil {
		d.logger.Printf("attention: appending %s: %v", attention.EventsFileName, err)
		return prev
	}
	// The count and the repeat clocks advance only once their events are
	// durable, so a failed append re-derives the promotion and the repeat on
	// the next tick instead of skipping them.
	src.diskWarnBeats = beats
	src.repeatedAt = repeatedAt
	// One line per transition, and nothing on a tick with no change: the line
	// is the signal, not a per-beat heartbeat. It says what the append just
	// made durable, so the tick that delivered nothing says nothing here.
	for _, e := range res.Events {
		d.logger.Printf("attention: %s %s %s", attentionWord(e.State), e.Key, e.Text)
	}
	if err := attention.WriteState(d.config.TownRoot, res.State); err != nil {
		d.logger.Printf("attention: writing %s: %v", attention.StateFileName, err)
		// The events are durable, so the cache still advances: handing back
		// prev would re-derive every transition and append each one twice. A
		// restart before the state write lands replays them from the stale
		// state.json, which costs a repeated line rather than a lost event.
		return res.State
	}
	// An ack dies with its item: the reconcile dropped the keys the new state
	// no longer holds, and the survivors are written back here. PruneAcks is
	// handed res.State and not res.Acks, because it re-reads acks.json under
	// the flock and so keeps an ack made while the collectors ran (gt-cuzjj).
	if err := attention.PruneAcks(d.config.TownRoot, res.State); err != nil {
		d.logger.Printf("attention: pruning acks: %v", err)
	}
	return res.State
}

// diskWarnPromotion counts the consecutive ticks host.disk_warn has held in
// state and, on the tick that reaches attention.DiskWarnPromoteBeats, returns
// one more raised line for the item with the operator_action, wake flags a
// warning that does not clear has earned. It fires once per run of ticks: the
// count keeps rising past the threshold and resets when the item clears, so a
// disk that fills again after clearing earns a new promotion.
//
// The count is memory only, so the promotion is also marked on the item
// (PromotedAt, which the state carries and a restart reads back): a daemon
// restarted while the warning persists counts again from zero but does not
// wake a second time. An acked item is left alone, as an ack hides an item
// until it clears. The returned state carries the mark; the input is not
// modified.
func diskWarnPromotion(state attention.State, beats int, now time.Time) (attention.State, int, []attention.Event) {
	it, ok := attention.Find(state, attention.RuleHostDiskWarn)
	if !ok {
		return state, 0, nil
	}
	beats++
	if beats < attention.DiskWarnPromoteBeats || it.PromotedAt != nil || it.AckedAt != nil {
		return state, beats, nil
	}
	items := append([]attention.Item(nil), state.Items...)
	for i := range items {
		if items[i].Key == it.Key {
			promotedAt := now
			items[i].PromotedAt = &promotedAt
		}
	}
	state.Items = items
	return state, beats, []attention.Event{{
		TS:       now,
		Class:    it.Kind,
		Severity: attention.SeverityHigh,
		Text:     fmt.Sprintf("%s (unresolved for %d heartbeats)", it.Summary, beats),
		Key:      it.Key,
		State:    attention.EventNew,
		Rule:     attention.RuleHostDiskWarn,
		Handled:  attention.HandledOperatorAction,
		Wake:     true,
	}}
}

// attentionWord is the transition's word in the daemon log. A cleared line
// repeats the raised line's text verbatim, so a bare sign reads as a fresh
// failure (gt-t4n5r).
func attentionWord(s attention.EventState) string {
	if s == attention.EventCleared {
		return "cleared"
	}
	return "raised"
}

// collectAttention runs every collector and returns the items holding now. A
// collector that fails contributes no items of its own, and the previous
// tick's items of its kind instead: an unanswered query must not clear a real
// alarm (townhealth's UNKNOWN rule, gt-s3rec.1). The failure is logged once,
// in the collector's name.
func collectAttention(ctx context.Context, prev attention.State, collectors []attentionCollector, logf func(string, ...any)) []attention.Item {
	var observed []attention.Item
	for _, c := range collectors {
		items, err := c.collect(ctx)
		if err != nil {
			logf("attention: collector %s: %v", c.kind, err)
			observed = append(observed, prevOfKind(prev, c.kind)...)
			continue
		}
		observed = append(observed, items...)
	}
	return observed
}

// prevOfKind is what a failed collector keeps: the items of its kind.
func prevOfKind(prev attention.State, kind attention.Kind) []attention.Item {
	var out []attention.Item
	for _, it := range prev.Items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

// collectors is the queue's collector set, in the order they run. Each later
// bead (gt-vsct7.4, .5, .6) adds its checks by appending entries here.
func (s *attentionSources) collectors() []attentionCollector {
	return append([]attentionCollector{
		{kind: attention.KindRedMain, collect: s.collectRedMain},
		{kind: attention.KindEscalation, collect: s.collectEscalations},
		{kind: attention.KindRejectedTwice, collect: s.collectRejectedTwice},
		{kind: attention.KindSlotHeld, collect: s.collectSlotHeld},
		{kind: attention.KindSlotDeadHolder, collect: s.collectSlotDeadHolder},
		{kind: attention.KindBDSlow, collect: s.collectBDSlow},
		{kind: attention.KindRevertRefused, collect: s.collectRevertRefused},
		{kind: attention.KindLandingStuck, collect: s.collectLandingStuck},
		{kind: attention.KindQueueStuck, collect: s.collectQueueStuck},
		{kind: attention.KindPolecatStall, collect: s.collectPolecatStall},
		{kind: attention.KindDirectPush, collect: s.collectDirectPush},
		{kind: attention.KindRiskPath, collect: s.collectRiskPaths},
		// Reads the count collectRiskPaths just made, so it stays right after it.
		{kind: attention.KindReviewBacklog, collect: s.collectReviewBacklog},
		{kind: attention.KindTierSweepRed, collect: s.collectTierSweepRed},
		{kind: attention.KindOverrunStopped, collect: s.collectOverrunStopped},
		{kind: attention.KindScheduledGate, collect: s.collectScheduledGate},
		{kind: attention.KindHostDisk, collect: s.collectHostDisk},
		{kind: attention.KindHostMem, collect: s.collectHostMem},
		{kind: attention.KindHostDocker, collect: s.collectHostDocker},
		{kind: attention.KindDispatchIdle, collect: s.collectDispatchIdle},
		{kind: attention.KindDeployFailed, collect: s.collectDeployFailures},
	}, s.questionCollectors()...)
}

// attentionSources builds the production reads. The bead lists go through the
// daemon's own read-only work-bead client, so the queue never writes to
// beads; the slot picture and the bd timing are the daemon's own probes.
func (d *Daemon) attentionSources(now time.Time) *attentionSources {
	s := &attentionSources{
		d:           d,
		now:         now,
		report:      d.lastTownHealth,
		reworkNotes: map[string]reworkNote{},
		host:        d.hostProbes,
	}
	if s.host == nil {
		s.host = systemHostProbes{townRoot: d.config.TownRoot}
	}
	s.landingRigs = d.attentionLandingRigs
	overseerCfg := d.loadOperationalConfig().GetOverseerConfig()
	s.backlog = backlogLimits{threshold: overseerCfg.ReviewBacklogThresholdV(), rise: overseerCfg.ReviewBacklogRiseV()}
	s.questions = d.productionQuestionReads()
	s.overseer = d.productionOverseerReads()
	s.dispatchIdle = d.dispatchIdle.Status
	s.dispatchHold = d.specDispatchTownHold
	s.redMain = func(ctx context.Context, rig string) ([]*beads.Issue, error) {
		return d.rigWorkBeads(rig).List(beads.ListOptions{
			Status: "open", Label: landworker.LabelRedMain, Priority: -1, Limit: 0,
		})
	}
	s.escalations = func(ctx context.Context) ([]*beads.Issue, error) {
		return listOpenEscalations(ctx, d.escalationBeads())
	}
	// The escalation records live in the town database; the bead one names as
	// its related_bead belongs to the rig its prefix routes to.
	s.escalationBead = func(ctx context.Context, id string) (*beads.Issue, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rig := beads.GetRigNameForPrefix(d.config.TownRoot, beads.ExtractPrefix(id))
		if rig == "" {
			return nil, nil
		}
		return d.rigWorkBeads(rig).Show(id)
	}
	s.rework = func(ctx context.Context, rig string) ([]*beads.Issue, error) {
		var out []*beads.Issue
		// No status filter: a candidate is anything IsActionable, so a bead
		// parked on a blocker or claimed for the rework still counts, and a
		// closed one is dropped by the collector (healthSources.countOpen
		// reads the same way, gt-tk2xd).
		for _, label := range []string{land.LabelRework, land.LabelNeedsHuman} {
			issues, err := d.rigWorkBeads(rig).List(beads.ListOptions{
				Label: label, Priority: -1, Limit: 0,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, issues...)
		}
		return out, nil
	}
	s.reworkNote = func(ctx context.Context, rig, id string) (string, error) {
		is, err := d.rigWorkBeads(rig).Show(id)
		if err != nil {
			return "", err
		}
		return is.Notes, nil
	}
	s.slots = func() (slot.Report, error) {
		pool := slot.PoolFromConfig(d.loadOperationalConfig().GetContainerGateConfig())
		return slot.StatusPoolLocksOnly(d.config.TownRoot, pool)
	}
	s.pidAlive = attentionPIDAlive
	s.bdLatency = d.measureBDLatency
	s.refusals = func() ([]attention.Refusal, error) {
		return attention.ReadRefusals(d.config.TownRoot)
	}
	s.refusalBead = func(ctx context.Context, rig, id string) (*beads.Issue, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return d.rigWorkBeads(rig).Show(id)
	}
	s.landingState = d.landingStates.get
	s.readyToLand = d.attentionReadyToLand
	s.tierSweep = func(rig string) (tierSweepState, error) {
		return readTierSweepState(d.config.TownRoot, rig)
	}
	s.tierSweepRigs = func() []string {
		if !d.isPatrolActive("tier_sweep") {
			return nil
		}
		return tierSweepRigs(d.patrolConfig, d.getKnownRigs())
	}
	s.seats = func() ([]townhealth.Seat, error) { return d.attentionSeats(now) }
	s.seatWork = d.attentionSeatWork
	s.remoteTip = func(rig string) (string, error) {
		rigPath := filepath.Join(d.config.TownRoot, rig)
		branch, err := rigDefaultBranch(rigPath)
		if err != nil {
			// Fail closed: a config.json that does not decode names no branch,
			// so there is no tip to read (gt-v4r0x).
			return "", fmt.Errorf("%s: %w", filepath.Join(rigPath, "config.json"), err)
		}
		return git.NewGit(filepath.Join(rigPath, ".repo.git")).
			RemoteBranchTip("origin", branch)
	}
	s.landedCommit = d.attentionLandedCommit
	s.commitInfo = d.attentionCommitInfo
	s.riskLandings = d.attentionRiskLandings
	s.riskIssue = func(ctx context.Context, rig, id string) (*beads.Issue, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return d.rigWorkBeads(rig).Show(id)
	}
	d.productionRiskAudit(s)
	return s
}

// attentionRiskLandings reads the rig's landing records landed at or after
// since: the risk-path collector's window over the landings file the landing
// worker writes.
//
// Unparsable lines fail the read rather than being skipped: each record is a
// risk-path item and part of the count the review-backlog rule reads, and a
// count short by a landing can clear a standing backlog item (gt-8mm0g).
// land.Since reports those lines through Logf instead of failing.
func (d *Daemon) attentionRiskLandings(rig string, since time.Time) ([]land.LandingRecord, error) {
	f, err := land.RigLandingsFile(d.config.TownRoot, rig)
	if err != nil {
		return nil, err
	}
	var unreadable string
	f.Logf = func(format string, args ...any) { unreadable = fmt.Sprintf(format, args...) }
	recs, err := f.Since(since)
	if err != nil {
		return nil, err
	}
	if unreadable != "" {
		// land's warning names the file, the count and a snippet of the first
		// line; the error says what that costs the count's reader.
		d.logger.Printf("attention: %s", unreadable)
		return nil, errors.New("the landings file has lines this read could not parse, so its count of open reviews is short")
	}
	return recs, nil
}

// attentionReadyToLand reads the rig's actionable gt:ready-to-land beads: the
// same list the landing worker's pass works, filtered the same way
// (landworker.Worker.pass), so the queue-stuck item and the worker never
// disagree about whether there is anything waiting. The oldest submission
// among them is the age queue-stuck judges, by land.SubmittedAt — the clock
// the worker orders the queue by (gt-m36as).
func (d *Daemon) attentionReadyToLand(ctx context.Context, rig string) (readyQueue, error) {
	if err := ctx.Err(); err != nil {
		return readyQueue{}, err
	}
	issues, err := d.rigWorkBeads(rig).List(beads.ListOptions{
		Label: land.LabelReadyToLand, Priority: -1, Limit: 0,
	})
	if err != nil {
		return readyQueue{}, err
	}
	var q readyQueue
	for _, is := range issues {
		if is == nil || !beads.IssueStatus(strings.TrimSpace(is.Status)).IsActionable() {
			continue
		}
		q.count++
		t := land.SubmittedAt(is)
		if t.IsZero() {
			continue
		}
		if q.oldest.IsZero() || t.Before(q.oldest) {
			q.oldest, q.bead = t, is.ID
		}
	}
	return q, nil
}

// attentionSeats walks the town's seat records the way townhealth does, with
// the same evidence window, so the stall item and the health report judge the
// same samples. It samples nothing itself (gt-vsct7.3): the walk reads the
// intent records the liveness sampler already writes.
func (d *Daemon) attentionSeats(now time.Time) ([]townhealth.Seat, error) {
	th, _, err := d.loadOperationalConfig().GetHealthSettings().Resolve()
	if err != nil {
		th = townhealth.DefaultThresholds()
	}
	return (&healthSources{d: d, evidence: th.SeatEvidence, now: now}).Seats()
}

// attentionLandingLimits is the town's configured landing wait limits, the
// compiled defaults when the health block is broken (the config field reports
// that). The queue-stuck collector scales these with the queue's depth the
// same way the townhealth landing field does, so the two read the same
// limits (gt-cpefw).
func (d *Daemon) attentionLandingLimits() townhealth.Limits {
	th, _, err := d.loadOperationalConfig().GetHealthSettings().Resolve()
	if err != nil {
		th = townhealth.DefaultThresholds()
	}
	return th.Landing
}

// attentionLandingStuckBudget is how long the in-flight landing's stage may
// run before the pass running it is wedged: the stage's own timeout — the CI
// wait, om's review — plus one pass interval of slack. A landing that
// outlives the stage it is running is wedged; one that is merely farther along
// in a healthy pipeline is not (gt-84gcp). A stage the pipeline reports no
// timeout for, the fast work before the CI wait, is judged against the whole
// landing budget instead.
func (d *Daemon) attentionLandingStuckBudget(stage string) time.Duration {
	cfg := landingWorkerConfig(d.patrolConfig)
	switch stage {
	case land.StageCI:
		return landingCIBudget() + townhealth.LandingPassInterval
	case land.StageOM:
		return landingOMBudget(cfg) + townhealth.LandingPassInterval
	}
	return townhealth.LandingWaitBudget
}

// attentionSeatWork reports whether the polecat holds assigned open work: a
// bead assigned to its seat with one of the statuses a sling sets
// (Daemon.hasAssignedOpenWork's set). Work already submitted for landing is
// not open work — that seat's record has no samples to be stalled on anyway.
// The read's error is kept, so an unanswered query is UNKNOWN rather than "no
// work", which would quietly clear a real stall (townhealth's UNKNOWN rule).
func (d *Daemon) attentionSeatWork(rig, name string) (bool, error) {
	assignee := rig + "/polecats/" + name
	for _, status := range []string{"hooked", "in_progress", "open"} {
		issues, err := d.assignedWork(rig, assignee, status)
		if err != nil {
			return false, fmt.Errorf("%s: bd list --status=%s: %w", assignee, status, err)
		}
		if len(issues) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// attentionLandedCommit reports whether the rig's landings file records sha as
// a landed commit: the landing worker's own record that the tip was its doing
// (gt-vsct7.3).
func (d *Daemon) attentionLandedCommit(rig, sha string) (bool, error) {
	f, err := land.RigLandingsFile(d.config.TownRoot, rig)
	if err != nil {
		return false, err
	}
	recs, err := f.Recent(attentionLandingRecords)
	if err != nil {
		return false, err
	}
	for _, rec := range recs {
		if rec.LandedCommit == sha {
			return true, nil
		}
	}
	return false, nil
}

// attentionCommitInfo describes a commit in the rig repo. An unreadable commit
// is empty, not an error: the tip of a direct push is usually a commit the
// local repo has never seen, and the item is still worth raising without the
// author and subject.
func (d *Daemon) attentionCommitInfo(rig, sha string) (string, string) {
	rigPath := filepath.Join(d.config.TownRoot, rig)
	author, subject, err := git.NewGit(filepath.Join(rigPath, ".repo.git")).CommitAuthorSubject(sha)
	if err != nil {
		return "", ""
	}
	return author, subject
}

// attentionLandingRigs are the rigs whose landing beads the collectors read;
// none when the landing worker is off. It is the same set townhealth's
// landing sources walk (healthSources.landingRigs).
func (d *Daemon) attentionLandingRigs() []string {
	if !d.isPatrolActive("landing_worker") {
		return nil
	}
	return landingWorkerRigs(d.patrolConfig, d.getKnownRigs())
}

func (d *Daemon) rigWorkBeads(rig string) workBeadReader {
	return d.workBeads(d.workBeadsEnv(rig), attentionBeadTimeout)
}

// measureBDLatency times one bd read of the town's open beads: the canary the
// bd-slow collector watches. The clock is the daemon's, so a test with a fake
// clock measures zero.
func (d *Daemon) measureBDLatency(ctx context.Context) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	start := d.clk().Now()
	_, err := d.workBeads(bdReadOnlyRoutingEnv(d.config.TownRoot), attentionBeadTimeout).
		List(beads.ListOptions{Status: "open", Priority: -1, Limit: 1})
	return d.clk().Now().Sub(start), err
}

// collectRedMain raises one item per open red-main bead, the beads the
// red-main owner files for a package that stayed red on main (gt-v4ssj.4).
// An item clears when the bead closes: the read lists open beads only.
func (s *attentionSources) collectRedMain(ctx context.Context) ([]attention.Item, error) {
	var out []attention.Item
	for _, rig := range s.landingRigs() {
		issues, err := s.redMain(ctx, rig)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rig, err)
		}
		prefix := landworker.RedMainTitle(rig, "")
		for _, is := range issues {
			if is == nil {
				continue
			}
			key, ok := strings.CutPrefix(is.Title, prefix)
			if !ok || key == "" {
				key = is.ID
			}
			out = append(out, attention.Item{
				Key:      "red-main:" + rig + ":" + key,
				Kind:     attention.KindRedMain,
				Severity: attention.SeverityHigh,
				Rig:      rig,
				Bead:     is.ID,
				Summary:  is.Title,
			})
		}
	}
	return out, nil
}

// collectEscalations raises one item per open escalation record, read through
// the same pinned query as the town-health escalation field (gt-9k2bx).
func (s *attentionSources) collectEscalations(ctx context.Context) ([]attention.Item, error) {
	issues, err := s.escalations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]attention.Item, 0, len(issues))
	for _, is := range issues {
		if is == nil {
			continue
		}
		summary := is.Title
		if summary == "" {
			summary = "escalation " + is.ID
		}
		owner, waiting := s.escalationOwner(ctx, is)
		out = append(out, attention.Item{
			Key:       "esc:" + is.ID,
			Kind:      attention.KindEscalation,
			Severity:  attention.SeverityHigh,
			Bead:      is.ID,
			Owner:     owner,
			WaitingOn: waiting,
			Summary:   summary,
		})
	}
	return out, nil
}

// escalationOwner names the owner of the bead an escalation record relates to,
// and what the escalation waits on: that bead's assignee. No related bead, no
// assignee, or a failed read of the bead leaves both empty, and the escalation
// is still raised (gt-sxmqs).
func (s *attentionSources) escalationOwner(ctx context.Context, esc *beads.Issue) (owner, waiting string) {
	related := strings.TrimSpace(beads.ParseEscalationFields(esc.Description).RelatedBead)
	if related == "" || s.escalationBead == nil {
		return "", ""
	}
	is, err := s.escalationBead(ctx, related)
	if err != nil || is == nil {
		// The escalation is the alarm; naming its bead's owner is what is lost
		// when that read fails, so the item is raised without the owner line.
		return "", ""
	}
	owner = strings.TrimSpace(is.Assignee)
	if owner == "" {
		return "", ""
	}
	return owner, "escalated, owned by " + owner
}

// collectRejectedTwice raises one item per actionable rework bead whose notes
// record at least attentionRejectedTwice MERGE REJECTION blocks: the landing
// loop has refused it twice, so the next round is a polecat session and a gate
// run spent on a bead that is not converging (gt-28ibg). It clears when the
// bead lands or closes, because the read lists open, actionable beads only.
//
// The notes are read with one bd show per candidate, cached by updated_at:
// the list carries no notes, and a bead that has not changed since the last
// tick cannot have gained a rejection.
func (s *attentionSources) collectRejectedTwice(ctx context.Context) ([]attention.Item, error) {
	var out []attention.Item
	for _, rig := range s.landingRigs() {
		issues, err := s.rework(ctx, rig)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rig, err)
		}
		for _, is := range issues {
			if is == nil || !beads.IssueStatus(strings.TrimSpace(is.Status)).IsActionable() {
				continue
			}
			n, err := s.rejectionCount(ctx, rig, is)
			if err != nil {
				return nil, err
			}
			if n < attentionRejectedTwice {
				continue
			}
			// The waiting-on line names an owner's rework. A bead nobody owns
			// waits on a person, so it stays empty and classifies as act.
			owner := strings.TrimSpace(is.Assignee)
			waiting := ""
			if owner != "" {
				waiting = fmt.Sprintf("rework by %s, attempt %d", owner, n)
			}
			out = append(out, attention.Item{
				Key:       "rework:" + is.ID,
				Kind:      attention.KindRejectedTwice,
				Severity:  attention.SeverityHigh,
				Rig:       rig,
				Bead:      is.ID,
				Owner:     owner,
				WaitingOn: waiting,
				Summary:   fmt.Sprintf("%s rejected %d times", is.ID, n),
			})
		}
	}
	return out, nil
}

// rejectionCount is one candidate's rejection count, from notes the list
// already carried when it has them, else from the cache, else from one bd
// show recorded in the cache.
func (s *attentionSources) rejectionCount(ctx context.Context, rig string, is *beads.Issue) (int, error) {
	if is.Notes != "" {
		return land.RejectionBlocks(is.Notes), nil
	}
	if c, ok := s.reworkNotes[is.ID]; ok && c.updatedAt == is.UpdatedAt {
		return c.count, nil
	}
	notes, err := s.reworkNote(ctx, rig, is.ID)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", is.ID, err)
	}
	n := land.RejectionBlocks(notes)
	if s.reworkNotes == nil {
		s.reworkNotes = map[string]reworkNote{}
	}
	s.reworkNotes[is.ID] = reworkNote{updatedAt: is.UpdatedAt, count: n}
	return n, nil
}

// collectSlotHeld raises one item per container-build slot held longer than
// attentionSlotHeld: the read is the same slot picture townhealth's slot
// field reads (gt-a8kx), so a held slot is one condition reported in two
// places, never two different pictures.
func (s *attentionSources) collectSlotHeld(ctx context.Context) ([]attention.Item, error) {
	rep, err := s.slots()
	if err != nil {
		return nil, err
	}
	var out []attention.Item
	for _, st := range rep.Slots {
		if !st.Held || st.Owner == nil || st.Owner.AcquiredAt.IsZero() {
			continue
		}
		held := s.now.Sub(st.Owner.AcquiredAt)
		if held < attentionSlotHeld {
			continue
		}
		name := slotName(st)
		out = append(out, attention.Item{
			Key:      "slot:" + name,
			Kind:     attention.KindSlotHeld,
			Severity: attention.SeverityHigh,
			Summary:  fmt.Sprintf("slot %s held %s", name, townhealth.Short(held)),
		})
	}
	return out, nil
}

// collectSlotDeadHolder raises one item per held slot whose owner process is
// gone: the flock is stale, so the slot is held by nobody and every suite
// waits on it.
func (s *attentionSources) collectSlotDeadHolder(ctx context.Context) ([]attention.Item, error) {
	rep, err := s.slots()
	if err != nil {
		return nil, err
	}
	var out []attention.Item
	for _, st := range rep.Slots {
		if !st.Held || st.Owner == nil || st.Owner.PID <= 0 || s.pidAlive(st.Owner.PID) {
			continue
		}
		name := slotName(st)
		out = append(out, attention.Item{
			Key:      "slot-dead:" + name,
			Kind:     attention.KindSlotDeadHolder,
			Severity: attention.SeverityHigh,
			Summary:  fmt.Sprintf("slot %s held by dead pid %d", name, st.Owner.PID),
		})
	}
	return out, nil
}

// slotName is a slot row's display name.
func slotName(st slot.SlotState) string {
	if st.Name != "" {
		return st.Name
	}
	return fmt.Sprintf("slot%d", st.Index)
}

// collectBDSlow raises one item when the town's bead reads are slow: this
// tick's health report judged Dolt red, the collector's own bd read took
// longer than attentionBDSlow, or the read did not answer at all. It is the
// one collector whose subject is the beads plane itself, so a town whose reads
// have stopped answering shows up in the queue before its alarms do.
//
// The report is read before the timing probe, and a failed read raises the
// item rather than returning the error: that failure is the condition this
// collector reports — bd, or the Dolt under it, is down — and an error would
// leave the queue silent in exactly the case it exists for (gt-cuzjj).
func (s *attentionSources) collectBDSlow(ctx context.Context) ([]attention.Item, error) {
	f, red := doltRed(s.report)
	slow, err := s.bdLatency(ctx)
	var summary string
	switch {
	case red:
		summary = "dolt " + f.Value
		if f.Detail != "" {
			summary += ": " + f.Detail
		}
	case err != nil:
		// The error's text is not the summary: bd repeats its stderr, and a
		// queue row is one line.
		summary = "bd read failed"
	case slow >= attentionBDSlow:
		summary = "bd read took " + townhealth.Short(slow)
	}
	if summary == "" {
		return nil, nil
	}
	return []attention.Item{{
		Key:      "bd-slow",
		Kind:     attention.KindBDSlow,
		Severity: attention.SeverityHigh,
		Summary:  summary,
	}}, nil
}

// collectRevertRefused raises one item per gt done refusal still standing: a
// refusal recorded in the last attentionRefusalWindow whose bead is not closed
// and has not since been submitted to land at another head. A refusal is the
// strongest signal a polecat is destroying work other people merged (gt-63sz),
// and until this it lived only in the refused pane.
func (s *attentionSources) collectRevertRefused(ctx context.Context) ([]attention.Item, error) {
	refusals, err := s.refusals()
	if err != nil {
		return nil, err
	}
	var out []attention.Item
	for _, ref := range refusals {
		if ref.Bead == "" || ref.Head == "" || s.now.Sub(ref.TS) >= attentionRefusalWindow {
			continue
		}
		issue, err := s.refusalBead(ctx, ref.Rig, ref.Bead)
		if errors.Is(err, beads.ErrNotFound) {
			// A bead purged since the refusal is gone, not an unanswered read:
			// clear the item rather than stall the kind on a dead id.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref.Bead, err)
		}
		// The bead closing, or a later submission at a different head, is the
		// clear: either the work landed another way or was redone after the
		// refusal, so the reverted-branch alarm no longer holds.
		if issue == nil || beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
			continue
		}
		if w, ok := land.ParseReadyNote(issue.Notes); ok && w.Head != "" && w.Head != ref.Head {
			continue
		}
		out = append(out, attention.Item{
			Key:      "refused:" + ref.Bead + ":" + head12(ref.Head),
			Kind:     attention.KindRevertRefused,
			Severity: attention.SeverityHigh,
			Rig:      ref.Rig,
			Bead:     ref.Bead,
			SHA:      ref.Head,
			Summary:  ref.Summary,
		})
	}
	return out, nil
}

// head12 is the 12-character head a refusal item is keyed by.
func head12(head string) string {
	if len(head) > 12 {
		return head[:12]
	}
	return head
}

// doltRed returns the report's Dolt field when the report judged it red.
func doltRed(r *townhealth.Report) (townhealth.Field, bool) {
	if r == nil {
		return townhealth.Field{}, false
	}
	for _, f := range r.Fields {
		if f.Name == townhealth.FieldDolt && f.Verdict == townhealth.Red {
			return f, true
		}
	}
	return townhealth.Field{}, false
}

// attentionPIDAlive reports whether pid names a live process. A pid this
// process may not signal still exists, so it counts as alive; only a pid
// whose probe says no such process is gone.
func attentionPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return isProcessAlive(p)
}

// collectLandingStuck raises one item per rig whose in-flight landing has
// outlived the stage it is running: the daemon's copy of the landing-worker
// STUCK-INFLIGHT check, read from the Active and Stage callbacks the worker
// already makes rather than by tailing the log (gt-vsct7.3). A pass is judged
// against its own stage's timeout plus one pass interval of slack, so a
// landing that is legitimately gating — lint, then the gate, then om — is
// never an item, however long the pipeline as a whole takes (gt-84gcp). A
// pass on no reported stage, the fast work before the gate, is judged against
// the whole landing budget instead. The item clears on its own: the next tick
// returns no item once the pass moves to another bead or ends.
func (s *attentionSources) collectLandingStuck(ctx context.Context) ([]attention.Item, error) {
	var out []attention.Item
	for _, rig := range s.landingRigs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := s.landingState(rig)
		if p.bead == "" || p.since.IsZero() {
			continue
		}
		// A reported stage is judged by its own clock, so the gate's minutes
		// do not count against the review that follows it; the bead's clock
		// covers the work before the gate, where no stage has been reported.
		age, stage := s.now.Sub(p.since), ""
		if p.stage != "" && !p.stageSince.IsZero() {
			age, stage = s.now.Sub(p.stageSince), p.stage
		}
		if age <= s.d.attentionLandingStuckBudget(stage) {
			continue
		}
		summary := fmt.Sprintf("%s in flight %s", p.bead, townhealth.Short(age))
		if stage != "" {
			summary = fmt.Sprintf("%s in the %s stage %s", p.bead, stage, townhealth.Short(age))
		}
		out = append(out, attention.Item{
			Key:      "landing-stuck:" + rig + ":" + p.bead,
			Kind:     attention.KindLandingStuck,
			Severity: attention.SeverityHigh,
			Rig:      rig,
			Bead:     p.bead,
			Summary:  summary,
		})
	}
	return out, nil
}

// collectQueueStuck raises one item per rig whose oldest ready-to-land
// submission has waited past its queue's wait allowance: the daemon's copy of
// queue-watch STUCK-QUEUE. The allowance is townhealth.LandingWaitLimits, the
// landing wait budget scaled by the queue's depth, so a healthy serial queue
// of two or three behind a loaded gate is not an item (gt-cpefw); it is the
// same limits the health field judges, so the alert and the health line
// cannot disagree. It judges the waiting bead's own age, not the time since
// the rig last landed, so it does not measure an idle town (gt-m36as).
func (s *attentionSources) collectQueueStuck(ctx context.Context) ([]attention.Item, error) {
	base := s.d.attentionLandingLimits()
	var out []attention.Item
	for _, rig := range s.landingRigs() {
		q, err := s.readyToLand(ctx, rig)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rig, err)
		}
		if q.count == 0 || q.oldest.IsZero() {
			// Nothing waiting, or no submission time to judge one by: an
			// unreadable stamp is the landing field's to report, not this
			// collector's to guess at.
			continue
		}
		waited := s.now.Sub(q.oldest)
		allowance := townhealth.LandingWaitLimits(base, q.count).Red
		if allowance <= 0 || waited <= allowance {
			continue
		}
		out = append(out, attention.Item{
			Key:      "queue-stuck:" + rig,
			Kind:     attention.KindQueueStuck,
			Severity: attention.SeverityHigh,
			Rig:      rig,
			Bead:     q.bead,
			Summary:  fmt.Sprintf("%s waiting %s with %d ready-to-land", q.bead, townhealth.Short(waited), q.count),
		})
	}
	return out, nil
}

// readyQueue is a rig's waiting landing work: how many actionable
// ready-to-land beads wait, and when the oldest of them was submitted.
type readyQueue struct {
	count int
	// oldest is the oldest submission time; zero when nothing waits, or when
	// no waiting bead carries a readable one.
	oldest time.Time
	// bead is the bead oldest belongs to; "" when oldest is zero.
	bead string
}

// collectPolecatStall raises one item per running polecat whose progress
// evidence has not changed for attentionPolecatStall and which holds assigned
// open work (gt-vsct7.3): the daemon's copy of the polecat-stall monitor,
// read from the intent record the liveness sampler already writes rather than
// from a fresh pane capture. It is the queue's one low-severity item: a
// stalled seat costs a session, not a town.
//
// The walk has already dropped every seat whose sample is missing or older
// than the evidence window, so this reads evidence and holds back nothing.
func (s *attentionSources) collectPolecatStall(ctx context.Context) ([]attention.Item, error) {
	seats, err := s.seats()
	if err != nil {
		return nil, err
	}
	var out []attention.Item
	for _, seat := range seats {
		if !seat.Run || seat.Frozen {
			continue
		}
		role, name, named := strings.Cut(seat.Name, "/")
		if !named || role != constants.RolePolecat || seat.Rig == "" {
			continue
		}
		if seat.Changed.IsZero() {
			continue
		}
		quiet := s.now.Sub(seat.Changed)
		if quiet <= attentionPolecatStall {
			continue
		}
		holds, err := s.seatWork(seat.Rig, name)
		if err != nil {
			return nil, err
		}
		if !holds {
			continue
		}
		out = append(out, attention.Item{
			Key:      "stall:" + seat.Rig + "/" + name,
			Kind:     attention.KindPolecatStall,
			Severity: attention.SeverityLow,
			Rig:      seat.Rig,
			Summary:  fmt.Sprintf("%s silent %s with work in hand", name, townhealth.Short(quiet)),
		})
	}
	return out, nil
}

// collectRiskPaths raises risk:<bead>:<head12> for each landing in the last
// attentionRiskPathWindow whose record names a risk path, unless the work
// bead's notes carry an "OVERSEER REVIEW <that head> PASS" line, or a
// WAIVED or AUDITED line with its reason (land.HasOverseerReviewNote). It is the label land.Land writes as
// gt:overseer-review-wanted made visible as something to do: a landed bead is
// closed, and the review it asks for comes after, so this item outlives the
// close.
//
// A FAIL does not clear the item: the review found a defect, so the item
// stays, with a summary that says so and names the finding beads the FAIL line
// covers, until a later PASS for the same head (gt-86uzs).
//
// Its severity is review debt's, judged by attention.RiskPathSeverity from the
// verdict and the work bead's own priority: the queue holds dozens of these at
// once, and only a failed review of a P1 landing is an incident (gt-zevjx).
//
// The bead read is cached per rig and bead for the tick, so a bead landed twice
// in the window costs one bd show. The cache lives for the one collector call:
// a review note written between ticks must be seen on the next, so nothing
// carries over.
//
// Like the other landing collectors it reads only the rigs whose landing
// worker is on (attentionLandingRigs): a town with landings disabled has no
// landing picture to report, and this item is part of that picture.
//
// A bead read that fails (a slow or failed bd show) skips that bead alone: the
// rest of the window is still judged, and the landing whose read failed keeps
// the item it already had, so a failed read never clears a review that is owed
// (gt-pqpq8). The collector returns an error only when every bead read failed or
// the context is done, which is what collectAttention carries forward. A bead
// that keeps failing is logged when it starts failing, not on every tick.
func (s *attentionSources) collectRiskPaths(ctx context.Context) ([]attention.Item, error) {
	s.riskOpen, s.riskCounted = 0, false
	var out []attention.Item
	reads := map[string]riskRead{}
	skipped := map[string]bool{}
	var firstErr error
	beadReads, beadFails := 0, 0
	since := s.now.Add(-attentionRiskPathWindow)
	for _, rig := range s.landingRigs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		recs, err := s.riskLandings(rig, since)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rig, err)
		}
		for _, rec := range recs {
			if rec.BeadID == "" || rec.Head == "" || len(rec.RiskPaths) == 0 {
				continue
			}
			key := "risk:" + rec.BeadID + ":" + sha12(rec.Head)
			cacheKey := rig + "\x00" + rec.BeadID
			read, ok := reads[cacheKey]
			if !ok {
				issue, err := s.riskIssue(ctx, rig, rec.BeadID)
				read = riskRead{issue: issue, err: err}
				reads[cacheKey] = read
				beadReads++
				if err != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return nil, ctxErr
					}
					beadFails++
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", rec.BeadID, err)
					}
					skipped[cacheKey] = true
					if !s.riskSkipped[cacheKey] {
						s.d.logger.Printf("attention: collector risk-path: skipping %s this tick: %v", rec.BeadID, err)
					}
				}
			}
			if read.err != nil {
				if it, ok := attention.Find(s.prev, key); ok {
					out = append(out, it)
				}
				continue
			}
			issue := read.issue
			if issue == nil {
				continue
			}
			notes := issue.Notes
			if land.HasOverseerReviewNote(notes, rec.Head) {
				continue
			}
			verdict, _ := land.OverseerReviewVerdict(notes, rec.Head)
			wanted := "overseer review wanted"
			if verdict == land.OverseerReviewFail {
				wanted = "overseer review FAILED; rework needed, then a PASS"
				if f := land.OverseerReviewFindings(notes, rec.Head); len(f) > 0 {
					wanted = fmt.Sprintf("overseer review FAILED (findings %s); rework needed, then a PASS", strings.Join(f, ","))
				}
			}
			out = append(out, attention.Item{
				Key:  key,
				Kind: attention.KindRiskPath,
				Severity: attention.RiskPathSeverity(attention.RiskPathEvidence{
					Failed:   verdict == land.OverseerReviewFail,
					Priority: issue.Priority,
				}),
				Rig:      rig,
				Bead:     rec.BeadID,
				SHA:      rec.Head,
				LandedAt: rec.LandedAt,
				Summary:  fmt.Sprintf("%s landed %s touching %s; %s", rec.BeadID, sha12(rec.Head), riskPathList(rec.RiskPaths), wanted),
			})
		}
	}
	// Recorded only when the pass ran to its end: an early return leaves last
	// tick's skips in place, so the next tick logs only what it has not logged.
	s.riskSkipped = skipped
	if beadReads > 0 && beadFails == beadReads {
		return nil, fmt.Errorf("every bead read failed, first: %w", firstErr)
	}
	// Published only when every rig and every bead read: a failed rig returned
	// above, and a count short by a bead whose read failed is not one the
	// backlog rule may clear a standing item on (gt-8mm0g).
	s.riskOpen, s.riskCounted = len(out), beadFails == 0
	return out, nil
}

// riskRead is one work bead's read as collectRiskPaths holds it for the tick:
// the issue, or the error that failed the read.
type riskRead struct {
	issue *beads.Issue
	err   error
}

// collectTierSweepRed raises one item per rig and tier whose last sweep
// verdict is RED (gt-vsct7.5), read from the sweep job's own record and dated
// at the tip that verdict was taken at. The item clears on the next GREEN of
// that tier, because that is what rewrites the record's verdict; nothing
// clears it by hand, and a Partial cycle that never reached the tier leaves the
// RED standing (gt-euxf1).
func (s *attentionSources) collectTierSweepRed(ctx context.Context) ([]attention.Item, error) {
	var out []attention.Item
	for _, rig := range s.tierSweepRigs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := s.tierSweep(rig)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rig, err)
		}
		for tier, res := range st.Tiers {
			if res.Verdict != tierSweepRed {
				continue
			}
			out = append(out, attention.Item{
				Key:      "sweep:" + rig + ":" + tier,
				Kind:     attention.KindTierSweepRed,
				Severity: attention.SeverityHigh,
				Rig:      rig,
				SHA:      res.SHA,
				Summary:  fmt.Sprintf("tier sweep %s %s RED (%d failed%s)", rig, tier, res.Failed, tierSweepFailureNames(res.FailedNames)),
			})
		}
	}
	return out, nil
}

// riskPathList is a landing's risk paths as one line, capped so a landing that
// swept a whole directory does not fill the queue row.
func riskPathList(paths []string) string {
	const shown = 3
	if len(paths) <= shown {
		return strings.Join(paths, " ")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(paths[:shown], " "), len(paths)-shown)
}

// tierSweepFailureNames renders a RED tier's failing units for an item
// summary: the first few by name, with a count for the rest.
func tierSweepFailureNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	const shown = 3
	head := names
	rest := ""
	if len(head) > shown {
		head, rest = head[:shown], fmt.Sprintf(" and %d more", len(names)-shown)
	}
	return ": " + strings.Join(head, ", ") + rest
}
