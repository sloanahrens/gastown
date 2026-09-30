// Package patrolscan is the deterministic half of the witness patrol, run by
// the daemon as the patrol_scan tick (ADR 0005, gt-4k3fj.6). One tick scans one
// rig and does three things:
//
//   - restart: a polecat whose session is confirmed dead while it holds
//     unfinished, unheld work is restarted through the supervisor, which
//     enforces the park/freeze, e-stop, submitted and 3-per-hour budget
//     guards in one place;
//   - orphaned molecules: a polecat that is gone (no session and no
//     directory) leaves its work bead's bonded mol-polecat-work wisp and step
//     wisps open; they are closed so the base bead is not blocked by them;
//   - stranded work: a hooked bead whose polecat is gone gets ONE comment per
//     report window naming the surviving branch (or saying none survives). The
//     tick never re-slings, resets or reassigns the bead.
//
// Every read that fails makes the answer Unknown, and Unknown is never acted
// on: a failed bd read is not "no work", "not held" or "bead gone". The tick
// never reads or drains mail.
//
// Nothing here touches tmux, bd or the filesystem directly. The host (the
// daemon) implements Env; the tests implement it with fakes.
package patrolscan

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/liveness"
)

// ReadyToLandLabel marks work gt done submitted to the landing worker. It is
// land.LabelReadyToLand, repeated here so this package stays out of the land
// package's dependency tree; a daemon test pins the two together.
const ReadyToLandLabel = "gt:ready-to-land"

// Defaults.
const (
	// DefaultDeadSamples is how many consecutive dead samples a seat needs
	// before it is restarted. The count lives in the intent record, so a
	// daemon restart does not reset it.
	DefaultDeadSamples = 2
	// DefaultSpawnGrace: gt sling hooks the work bead before the session
	// exists, so work hooked this recently with no session is a polecat
	// starting up.
	DefaultSpawnGrace = 5 * time.Minute
	// DefaultReportWindow is how often one stranded bead may be commented on.
	DefaultReportWindow = 24 * time.Hour
	// DefaultHeartbeatFresh is how recent an "exiting" heartbeat must be for
	// a dead-looking session to be taken as a polecat inside gt done.
	DefaultHeartbeatFresh = 3 * time.Minute
)

// Actor is the supervisor actor and comment author of every tick action.
const Actor = "daemon/patrol-scan"

// Work is the slice of a work bead the tick decides on.
type Work struct {
	ID       string
	Status   string
	Assignee string
	Labels   []string
	Design   string
	Notes    string
	// UpdatedAt is when the bead last changed; zero when unknown.
	UpdatedAt time.Time
	// AttachedMolecule is the bonded mol-polecat-work root, "" for none.
	AttachedMolecule string
}

// HasLabel reports whether the bead carries label (case-insensitive).
func (w Work) HasLabel(label string) bool {
	for _, l := range w.Labels {
		if strings.EqualFold(strings.TrimSpace(l), label) {
			return true
		}
	}
	return false
}

// Heartbeat is a polecat's self-reported session heartbeat (heartbeat v2).
type Heartbeat struct {
	State string // working, idle, exiting, stuck
	At    time.Time
}

// Env is everything a tick reads and does. Every read returns an error when
// it could not answer; the tick turns that into Unknown.
type Env interface {
	// Polecats lists the polecat directories of the rig.
	Polecats(rig string) ([]string, error)
	// Intent reads the seat's intent record (fail-closed: an unreadable
	// record comes back held, with an error).
	Intent(rig, polecat string) (intent.Record, error)
	// Assess runs the liveness function for the polecat's session and
	// persists the sample.
	Assess(rig, polecat string) liveness.Result
	// AssignedWork returns the polecat's hooked or in_progress work bead, or
	// nil when it has none.
	AssignedWork(rig, polecat string) (*Work, error)
	// AgentState returns the agent_state the polecat's agent bead records
	// (stuck, awaiting-gate, paused, done, ...). It is read only to refuse a
	// restart, never to cause one.
	AgentState(rig, polecat string) (string, error)
	// Heartbeat returns the session heartbeat, nil when there is none.
	Heartbeat(rig, polecat string) *Heartbeat
	// Restart restarts the seat through the supervisor.
	Restart(rig, polecat, reason string) error

	// ActiveWork lists hooked and in_progress beads assigned to polecats of
	// the rig (assignee "<rig>/polecats/<name>").
	ActiveWork(rig string) ([]Work, error)
	// PolecatDirExists reports whether the polecat's directory exists.
	PolecatDirExists(rig, polecat string) (bool, error)
	// SessionExists reports whether the polecat's tmux session exists.
	SessionExists(rig, polecat string) (bool, error)
	// MoleculeStatus returns a molecule root's status; "" with no error
	// means the root is gone (reaped).
	MoleculeStatus(id string) (string, error)
	// CloseMolecule closes a molecule root and its step wisps, returning how
	// many closed.
	CloseMolecule(id, reason string) (int, error)
	// SurvivingBranch returns the branch carrying the bead's unlanded work,
	// "" when none does; an error means unknown.
	SurvivingBranch(rig, beadID string) (string, error)
	// Comment appends a comment to a bead.
	Comment(beadID, text string) error
}

// Ledger remembers when a bead was last reported, so a report is written
// once per window across ticks and daemon restarts.
type Ledger interface {
	LastReported(key string) (time.Time, bool)
	MarkReported(key string, at time.Time) error
}

// Options tunes a Scanner.
type Options struct {
	DeadSamples    int
	SpawnGrace     time.Duration
	ReportWindow   time.Duration
	HeartbeatFresh time.Duration
	// HoldReason returns why a work bead is held from dispatch ("" when it
	// is not): the convoy dispatchers' hold rule. Nil means no hold rule.
	HoldReason func(Work) string
	// IsRefusal reports whether a Restart error is the supervisor refusing
	// (parked, frozen, e-stop, budget) rather than failing.
	IsRefusal func(error) bool
	Now       func() time.Time
}

// Scanner runs ticks.
type Scanner struct {
	env    Env
	ledger Ledger
	o      Options
}

// New returns a Scanner with defaults filled in.
func New(env Env, ledger Ledger, o Options) *Scanner {
	if o.DeadSamples <= 0 {
		o.DeadSamples = DefaultDeadSamples
	}
	if o.SpawnGrace <= 0 {
		o.SpawnGrace = DefaultSpawnGrace
	}
	if o.ReportWindow <= 0 {
		o.ReportWindow = DefaultReportWindow
	}
	if o.HeartbeatFresh <= 0 {
		o.HeartbeatFresh = DefaultHeartbeatFresh
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.IsRefusal == nil {
		o.IsRefusal = func(error) bool { return false }
	}
	return &Scanner{env: env, ledger: ledger, o: o}
}

// Outcome names what the tick did about one seat or bead.
type Outcome string

const (
	OutcomeRestarted  Outcome = "restarted"
	OutcomeRefused    Outcome = "refused"    // the supervisor said no
	OutcomeFailed     Outcome = "failed"     // an action ran and failed
	OutcomeSkipped    Outcome = "skipped"    // a guard said leave it
	OutcomeUnknown    Outcome = "unknown"    // a read failed; nothing done
	OutcomeWaiting    Outcome = "waiting"    // dead, but not confirmed yet
	OutcomeStalled    Outcome = "stalled"    // reported only
	OutcomeClosed     Outcome = "closed"     // orphaned molecule closed
	OutcomeReported   Outcome = "reported"   // stranded comment written
	OutcomeSuppressed Outcome = "suppressed" // already reported this window
)

// Finding is one line of a tick's report.
type Finding struct {
	Kind    string // "seat", "molecule", "stranded"
	Subject string // polecat name or bead ID
	Outcome Outcome
	Detail  string
}

func (f Finding) String() string {
	if f.Detail == "" {
		return fmt.Sprintf("%s %s: %s", f.Kind, f.Subject, f.Outcome)
	}
	return fmt.Sprintf("%s %s: %s (%s)", f.Kind, f.Subject, f.Outcome, f.Detail)
}

// Report is one tick of one rig.
type Report struct {
	Rig      string
	Checked  int
	Findings []Finding
	// Errors are reads that failed for the whole rig (listing polecats or
	// active work); the affected pass did nothing.
	Errors []string
}

// Count returns how many findings have outcome o.
func (r Report) Count(o Outcome) int {
	n := 0
	for _, f := range r.Findings {
		if f.Outcome == o {
			n++
		}
	}
	return n
}

// Lines renders the report for the daemon log: one summary line, then one
// line per finding that is not a quiet skip.
func (r Report) Lines() []string {
	lines := []string{fmt.Sprintf("%s: checked %d polecat(s): %d restarted, %d refused, %d unknown, %d molecule(s) closed, %d stranded reported, %d error(s)",
		r.Rig, r.Checked, r.Count(OutcomeRestarted), r.Count(OutcomeRefused), r.Count(OutcomeUnknown),
		r.Count(OutcomeClosed), r.Count(OutcomeReported), len(r.Errors))}
	for _, f := range r.Findings {
		lines = append(lines, r.Rig+": "+f.String())
	}
	for _, e := range r.Errors {
		lines = append(lines, r.Rig+": error: "+e)
	}
	return lines
}

// Tick scans one rig once.
func (s *Scanner) Tick(rig string) Report {
	r := Report{Rig: rig}
	polecats, err := s.env.Polecats(rig)
	if err != nil {
		r.Errors = append(r.Errors, "listing polecats: "+err.Error())
	} else {
		sort.Strings(polecats)
		for _, name := range polecats {
			r.Checked++
			if f, ok := s.seat(rig, name); ok {
				r.Findings = append(r.Findings, f)
			}
		}
	}
	s.orphans(rig, &r)
	return r
}

// seat decides one polecat. It reports a finding only when there is something
// to say beyond "alive" or "nothing to do".
func (s *Scanner) seat(rig, name string) (Finding, bool) {
	f := Finding{Kind: "seat", Subject: name}
	skip := func(why string) (Finding, bool) {
		f.Outcome, f.Detail = OutcomeSkipped, why
		return f, true
	}
	unknown := func(why string, err error) (Finding, bool) {
		f.Outcome, f.Detail = OutcomeUnknown, why+": "+err.Error()
		return f, true
	}

	// The intent record first: it never depends on Dolt, and a held or
	// submitted seat needs no liveness sample to be left alone.
	rec, err := s.env.Intent(rig, name)
	if err != nil {
		return unknown("intent record unreadable", err)
	}
	if rec.Held() {
		return Finding{}, false // parked or frozen: the operator's, silently
	}
	if rec.Submitted() {
		return Finding{}, false // gt done handed it to the landing worker
	}

	res := s.env.Assess(rig, name)
	switch res.Verdict {
	case liveness.Unknown:
		if res.Err == nil {
			res.Err = errors.New(res.Reason)
		}
		return unknown("liveness", res.Err)
	case liveness.Alive:
		return Finding{}, false
	case liveness.Stalled:
		// Reported only: a long turn is the usual explanation, and the
		// stall threshold is tuned after a measured week (ADR 0005).
		f.Outcome, f.Detail = OutcomeStalled, res.Reason
		return f, true
	}

	// Dead. Confirm it across ticks before acting.
	samples := 0
	if res.Sample != nil {
		samples = res.Sample.DeadSamples
	}

	work, err := s.env.AssignedWork(rig, name)
	if err != nil {
		return unknown("assigned work unreadable", err)
	}
	if work == nil {
		return Finding{}, false // idle or finished: a dead session is not a crash
	}
	if strings.EqualFold(work.Status, "closed") {
		return Finding{}, false
	}
	if work.HasLabel(ReadyToLandLabel) {
		return skip(work.ID + " is submitted for landing (" + ReadyToLandLabel + "); the landing worker owns it")
	}
	if s.o.HoldReason != nil {
		if why := s.o.HoldReason(*work); why != "" {
			return skip(work.ID + " is held: " + why)
		}
	}
	now := s.o.Now()
	if !work.UpdatedAt.IsZero() && now.Sub(work.UpdatedAt) < s.o.SpawnGrace {
		return skip(fmt.Sprintf("%s hooked %s ago; polecat may be spawning", work.ID, now.Sub(work.UpdatedAt).Round(time.Second)))
	}

	// Hazard 1 (gt-x45us): a polecat that parked itself is not a crash,
	// whatever else the record says. The agent bead is read only here, only
	// to refuse, and an unreadable one is Unknown.
	state, err := s.env.AgentState(rig, name)
	if err != nil {
		return unknown("agent state unreadable", err)
	}
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "stuck", "awaiting-gate", "paused":
		return skip("agent_state " + state + ": held by the polecat")
	case "done", "nuked":
		return skip("agent_state " + state + ": stopped on purpose")
	}
	if hb := s.env.Heartbeat(rig, name); hb != nil {
		switch strings.ToLower(hb.State) {
		case "stuck":
			return skip("heartbeat says stuck: held by the polecat")
		case "exiting":
			if now.Sub(hb.At) < s.o.HeartbeatFresh {
				return skip("heartbeat says exiting: inside gt done")
			}
		}
	}

	if samples < s.o.DeadSamples {
		f.Outcome = OutcomeWaiting
		f.Detail = fmt.Sprintf("%s with %s hooked; dead sample %d of %d", res.Reason, work.ID, samples, s.o.DeadSamples)
		return f, true
	}

	reason := fmt.Sprintf("patrol scan: %s with %s hooked (%d dead samples)", res.Reason, work.ID, samples)
	if err := s.env.Restart(rig, name, reason); err != nil {
		if s.o.IsRefusal(err) {
			f.Outcome, f.Detail = OutcomeRefused, err.Error()
			return f, true
		}
		f.Outcome, f.Detail = OutcomeFailed, err.Error()
		return f, true
	}
	f.Outcome, f.Detail = OutcomeRestarted, reason
	return f, true
}

// orphans scans from the beads side: work held by a polecat that no longer
// exists. That polecat is invisible to the seat pass, which walks directories.
func (s *Scanner) orphans(rig string, r *Report) {
	work, err := s.env.ActiveWork(rig)
	if err != nil {
		r.Errors = append(r.Errors, "listing active work: "+err.Error())
		return
	}
	prefix := rig + "/polecats/"
	sort.Slice(work, func(i, j int) bool { return work[i].ID < work[j].ID })
	for _, w := range work {
		if !strings.HasPrefix(w.Assignee, prefix) {
			continue
		}
		name := strings.TrimPrefix(w.Assignee, prefix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		gone, why := s.holderGone(rig, name)
		if why != "" {
			r.Findings = append(r.Findings, Finding{Kind: "stranded", Subject: w.ID, Outcome: OutcomeUnknown, Detail: why})
			continue
		}
		if !gone {
			continue
		}
		if w.HasLabel(ReadyToLandLabel) {
			continue // the landing worker owns it; a dead holder is expected
		}
		if f, ok := s.molecule(w, name); ok {
			r.Findings = append(r.Findings, f)
		}
		r.Findings = append(r.Findings, s.stranded(rig, w, name))
	}
}

// holderGone reports whether the polecat has neither a directory nor a
// session. A non-empty why means a read failed and the answer is unknown.
func (s *Scanner) holderGone(rig, name string) (gone bool, why string) {
	exists, err := s.env.PolecatDirExists(rig, name)
	if err != nil {
		return false, "polecat directory unreadable: " + err.Error()
	}
	if exists {
		return false, "" // the seat pass owns a polecat that still has a directory
	}
	alive, err := s.env.SessionExists(rig, name)
	if err != nil {
		return false, "session query failed: " + err.Error()
	}
	return !alive, ""
}

// molecule closes the orphaned work molecule bonded to w, if one is open.
func (s *Scanner) molecule(w Work, holder string) (Finding, bool) {
	if w.AttachedMolecule == "" {
		return Finding{}, false
	}
	f := Finding{Kind: "molecule", Subject: w.AttachedMolecule}
	status, err := s.env.MoleculeStatus(w.AttachedMolecule)
	if err != nil {
		f.Outcome, f.Detail = OutcomeUnknown, "molecule status unreadable: "+err.Error()
		return f, true
	}
	if status == "" || strings.EqualFold(status, "closed") {
		return Finding{}, false
	}
	reason := fmt.Sprintf("Orphaned mol-polecat-work: owning polecat %s no longer exists (patrol scan)", holder)
	n, err := s.env.CloseMolecule(w.AttachedMolecule, reason)
	if err != nil {
		f.Outcome, f.Detail = OutcomeFailed, fmt.Sprintf("closed %d, then: %v", n, err)
		return f, true
	}
	f.Outcome, f.Detail = OutcomeClosed, fmt.Sprintf("%d wisp(s) closed; base bead %s", n, w.ID)
	return f, true
}

// stranded writes one comment per report window on a bead whose holder is
// gone. It never changes the bead's status, assignee or hook (hazard 2).
func (s *Scanner) stranded(rig string, w Work, holder string) Finding {
	f := Finding{Kind: "stranded", Subject: w.ID}
	now := s.o.Now()
	key := rig + "/" + w.ID
	if last, ok := s.ledger.LastReported(key); ok && now.Sub(last) < s.o.ReportWindow {
		f.Outcome, f.Detail = OutcomeSuppressed, "reported "+now.Sub(last).Round(time.Minute).String()+" ago"
		return f
	}
	branch, err := s.env.SurvivingBranch(rig, w.ID)
	if err != nil {
		f.Outcome, f.Detail = OutcomeUnknown, "surviving-work check failed: "+err.Error()
		return f
	}
	text := StrandedComment(w, rig, holder, branch, s.o.ReportWindow)
	if err := s.env.Comment(w.ID, text); err != nil {
		f.Outcome, f.Detail = OutcomeFailed, "comment: "+err.Error()
		return f
	}
	if err := s.ledger.MarkReported(key, now); err != nil {
		// The comment is written; the next tick may write it again. Say so.
		f.Outcome, f.Detail = OutcomeReported, "ledger not updated: "+err.Error()
		return f
	}
	detail := "no surviving branch"
	if branch != "" {
		detail = "surviving branch " + branch
	}
	f.Outcome, f.Detail = OutcomeReported, detail
	return f
}

// StrandedComment is the text of a stranded-work report.
func StrandedComment(w Work, rig, holder, branch string, window time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "STRANDED (patrol scan): %s is %s and assigned to %s/polecats/%s, which no longer exists (no session, no directory).", w.ID, w.Status, rig, holder)
	if branch != "" {
		fmt.Fprintf(&b, " Unlanded work survives on branch %s.", branch)
		b.WriteString(" Resume it on that branch; do not re-sling with --force, which discards it.")
	} else {
		b.WriteString(" No polecat branch carries unlanded work for it.")
		b.WriteString(" Release it with `bd update " + w.ID + " --status open --assignee \"\"` if it should be redone.")
	}
	fmt.Fprintf(&b, " The patrol scan does not re-dispatch; this report repeats at most once per %s.", window)
	return b.String()
}
