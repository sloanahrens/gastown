package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/mail"
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
	// attentionRefusalWindow is how long a gt done refusal stays an item. A
	// refusal older than this has either been acted on or outlived its
	// branch; keeping it forever would turn the queue into a history file.
	attentionRefusalWindow = 48 * time.Hour
)

// mayorAddress is the mailbox the polecat template sends BLOCKED reports to
// (internal/templates/roles/polecat.md.tmpl).
const mayorAddress = "mayor/"

// attentionCache is what the daemon carries between attention ticks: the
// state it wrote last, so an item keeps its first_seen and a failed collector
// can keep its kind's items, and the rejection-note cache.
//
// It is read and written only on the heartbeat goroutine.
type attentionCache struct {
	state       attention.State
	reworkNotes map[string]reworkNote
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

	// refusals is gt done's refusal ledger; refusalBead reads one refused
	// bead in its own rig; blockedMail is the mayor's unread mail.
	refusals    func() ([]attention.Refusal, error)
	refusalBead func(ctx context.Context, rig, id string) (*beads.Issue, error)
	blockedMail func(ctx context.Context) ([]*mail.Message, error)

	// reworkNotes is the rejection-count cache, carried across ticks by the
	// daemon.
	reworkNotes map[string]reworkNote
}

// writeAttention is the heartbeat step after townhealth (gt-vsct7.2): it runs
// every collector, folds the items holding now into the queue and writes
// state.json. It is a non-lifecycle step, so it runs under the town E-stop
// too: the queue is a report about the town, and an E-stopped town still
// wants one.
func (d *Daemon) writeAttention() {
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
	ctx, cancel := context.WithTimeout(context.Background(), attentionTimeout)
	defer cancel()
	now := d.clk().Now()
	src := d.attentionSources(now)
	if cache.reworkNotes != nil {
		src.reworkNotes = cache.reworkNotes
	}
	state := d.attentionTick(ctx, src, prev, acks, now)
	d.lastAttention = &attentionCache{state: state, reworkNotes: src.reworkNotes}
}

// attentionTick runs the collectors, reconciles what they found against prev
// and the acks, and writes the queue. It returns the state it wrote, so the
// daemon can carry it into the next tick.
func (d *Daemon) attentionTick(ctx context.Context, src *attentionSources, prev attention.State, acks attention.Acks, now time.Time) attention.State {
	observed := collectAttention(ctx, prev, src.collectors(), d.logger.Printf)
	res := attention.Reconcile(prev, observed, acks, now)
	if err := attention.WriteState(d.config.TownRoot, res.State); err != nil {
		d.logger.Printf("attention: writing %s: %v", attention.StateFileName, err)
		return res.State
	}
	if err := attention.AppendEvents(d.config.TownRoot, res.Events); err != nil {
		d.logger.Printf("attention: appending %s: %v", attention.EventsFileName, err)
	}
	// One line per transition, and nothing on a tick with no change: the line
	// is the signal, not a per-beat heartbeat.
	for _, e := range res.Events {
		d.logger.Printf("attention: %s%s %s", attentionSign(e.State), e.Key, e.Text)
	}
	return res.State
}

// attentionSign is the transition's sign in the daemon log.
func attentionSign(s attention.EventState) string {
	if s == attention.EventCleared {
		return "-"
	}
	return "+"
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
// bead (gt-vsct7.3, .4, .5, .6) adds its checks by appending entries here.
func (s *attentionSources) collectors() []attentionCollector {
	return []attentionCollector{
		{kind: attention.KindRedMain, collect: s.collectRedMain},
		{kind: attention.KindEscalation, collect: s.collectEscalations},
		{kind: attention.KindRejectedTwice, collect: s.collectRejectedTwice},
		{kind: attention.KindSlotHeld, collect: s.collectSlotHeld},
		{kind: attention.KindSlotDeadHolder, collect: s.collectSlotDeadHolder},
		{kind: attention.KindBDSlow, collect: s.collectBDSlow},
		{kind: attention.KindRevertRefused, collect: s.collectRevertRefused},
		{kind: attention.KindBlockedMail, collect: s.collectBlockedMail},
	}
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
	}
	s.landingRigs = d.attentionLandingRigs
	s.redMain = func(ctx context.Context, rig string) ([]*beads.Issue, error) {
		return d.rigWorkBeads(rig).List(beads.ListOptions{
			Status: "open", Label: landworker.LabelRedMain, Priority: -1, Limit: 0,
		})
	}
	s.escalations = func(ctx context.Context) ([]*beads.Issue, error) {
		return listOpenEscalations(ctx, d.escalationBeads())
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
	s.blockedMail = func(ctx context.Context) ([]*mail.Message, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return mail.NewMailboxFromAddress(mayorAddress, d.config.TownRoot).ListUnread()
	}
	return s
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
		out = append(out, attention.Item{
			Key:      "esc:" + is.ID,
			Kind:     attention.KindEscalation,
			Severity: attention.SeverityHigh,
			Bead:     is.ID,
			Summary:  summary,
		})
	}
	return out, nil
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
			out = append(out, attention.Item{
				Key:      "rework:" + is.ID,
				Kind:     attention.KindRejectedTwice,
				Severity: attention.SeverityHigh,
				Rig:      rig,
				Bead:     is.ID,
				Summary:  fmt.Sprintf("%s rejected %d times", is.ID, n),
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
		return land.CountRejections(is.Notes), nil
	}
	if c, ok := s.reworkNotes[is.ID]; ok && c.updatedAt == is.UpdatedAt {
		return c.count, nil
	}
	notes, err := s.reworkNote(ctx, rig, is.ID)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", is.ID, err)
	}
	n := land.CountRejections(notes)
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

// collectBDSlow raises one item when the town's bead reads are slow: the
// collector's own bd read took longer than attentionBDSlow, or this tick's
// health report judged Dolt red. It is the one collector whose subject is the
// beads plane itself, so a town whose reads have stopped answering shows up
// in the queue before its alarms do.
func (s *attentionSources) collectBDSlow(ctx context.Context) ([]attention.Item, error) {
	slow, err := s.bdLatency(ctx)
	if err != nil {
		return nil, err
	}
	var summary string
	if f, ok := doltRed(s.report); ok {
		summary = "dolt " + f.Value
		if f.Detail != "" {
			summary += ": " + f.Detail
		}
	} else if slow >= attentionBDSlow {
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

// collectBlockedMail raises one item per unread message to the mayor whose
// subject starts with BLOCKED: — a polecat saying it cannot proceed. The mayor
// is stopped, so these sit unread; the read never marks one read, and the item
// clears when its message is read or closed.
func (s *attentionSources) collectBlockedMail(ctx context.Context) ([]attention.Item, error) {
	messages, err := s.blockedMail(ctx)
	if err != nil {
		return nil, err
	}
	var out []attention.Item
	for _, m := range messages {
		if m == nil || !blockedSubject(m.Subject) {
			continue
		}
		out = append(out, attention.Item{
			Key:      "blocked:" + m.ID,
			Kind:     attention.KindBlockedMail,
			Severity: attention.SeverityHigh,
			Summary:  fmt.Sprintf("blocked mail from %s: %s", m.From, m.Subject),
		})
	}
	return out, nil
}

// blockedSubject reports whether subject is a polecat's BLOCKED report.
func blockedSubject(subject string) bool {
	return strings.HasPrefix(strings.TrimSpace(subject), "BLOCKED:")
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
