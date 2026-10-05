// Package patrolscan is the deterministic half of the witness patrol, run by
// the daemon as the patrol_scan tick (ADR 0005, gt-4k3fj.6). One tick scans one
// rig and does four things:
//
//   - restart: a polecat whose session is confirmed dead while it holds
//     unfinished, unheld work is restarted through the supervisor, which
//     enforces the park/freeze, e-stop, submitted and 3-per-hour budget
//     guards in one place. A submitted seat is restarted once its work bead
//     stops reading as submitted, which is how a landing pulled for rework
//     comes back to a session it no longer has (gt-xs1ni). A seat whose
//     agent bead reads stuck after a DEFERRED exit is restarted the same
//     way, on its preserved branch: that bead is a finished turn left behind
//     by gt done, not a polecat holding the seat (gt-ks62m);
//   - orphaned molecules: a polecat that is gone (no session and no
//     directory) leaves its work bead's bonded mol-polecat-work wisp and step
//     wisps open; they are closed so the base bead is not blocked by them;
//   - dead-holder recovery: a hooked or in_progress bead whose polecat is gone
//     (no session, no directory) is released to the ready queue — open and
//     unassigned — with the branch its work survives on recorded as a
//     `resume_branch:` notes line, so the dispatcher resumes that branch
//     instead of starting fresh from main (gt-gzhin.2). A bead carrying
//     gt:ready-to-land, or held by a parked seat, is left alone;
//   - idle seats: a polecat whose session is confirmed gone and which holds
//     no work has its record retired to stop, so nothing keeps reporting the
//     seat dead (gt-613vw). Dispatching work to it sets the record back to run.
//     A seat whose last turn exited ESCALATED is first cleared to idle once
//     its blocker is provably resolved, so a spent escalation stops reading
//     as recovery-needed work (gt-fn9e6.33); see resolvedEscalationEvidence.
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

// ExitDeferred is the gt done exit type that hands a finished turn on. A
// polecat that exits DEFERRED signaled neither completion nor an escalation:
// its worktree keeps the commits it made and a successor is expected to carry
// the bead on. It is done.ExitDeferred, repeated here so this package stays
// out of the done package's dependency tree; a daemon test pins the two
// together.
const ExitDeferred = "DEFERRED"

// ExitEscalated is the gt done exit type of a polecat that stopped to raise a
// blocker for a human instead of finishing. It is done.ExitEscalated, repeated
// here so this package stays out of the done package's dependency tree; a
// daemon test pins the two together.
const ExitEscalated = "ESCALATED"

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

// ResumeBranchKey prefixes the notes line recording the branch a gone
// polecat's unlanded work survives on. The spec dispatcher reads the line
// back (gt-gzhin.3), so the key is part of a bead's contract, not a log
// format: `resume_branch: <branch>`, one per line.
const ResumeBranchKey = "resume_branch:"

// ResumeBranchNote renders one ResumeBranchKey line.
func ResumeBranchNote(branch string) string { return ResumeBranchKey + " " + branch }

// ResumeBranchFromNotes returns the branch the last ResumeBranchKey line in
// notes names, or "" when notes carry none: a bead recovered more than once
// resumes its newest branch.
func ResumeBranchFromNotes(notes string) string {
	branch := ""
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, ResumeBranchKey) {
			continue
		}
		if b := strings.TrimSpace(strings.TrimPrefix(line, ResumeBranchKey)); b != "" {
			branch = b
		}
	}
	return branch
}

// hasResumeBranch reports whether notes already carry the line for branch, so
// a retry after a failed release does not append it a second time.
func hasResumeBranch(notes, branch string) bool {
	for _, line := range strings.Split(notes, "\n") {
		if strings.TrimSpace(line) == ResumeBranchNote(branch) {
			return true
		}
	}
	return false
}

// AgentRecord is the slice of a polecat's agent bead the tick reads to tell a
// seat the polecat still holds from the record of a finished turn: gt done
// writes the state, the exit type and the cleanup status together when the
// turn ends.
type AgentRecord struct {
	// State is agent_state (stuck, awaiting-gate, paused, done, nuked, ...).
	State string
	// ExitType is the gt done exit type the bead recorded (DEFERRED,
	// ESCALATED, COMPLETED), "" when the bead carries none.
	ExitType string
	// CleanupStatus is the polecat's self-reported git state (clean,
	// has_uncommitted, has_stash, has_unpushed), "" when the bead carries none.
	CleanupStatus string
	// HookBead is hook_bead: the work bead the record says the seat still
	// holds, "" when it holds none.
	HookBead string
	// LastSourceIssue is last_source_issue: the work bead the last turn ran
	// on, which outlives the hook_bead clear gt done writes.
	LastSourceIssue string
}

// GitState is one polecat worktree's live git evidence. The tick measures it
// now rather than reading back the cleanup_status gt done recorded, because
// that record describes the turn that ended: only the worktree as it is today
// can prove nothing is at risk in it (gt-fn9e6.33).
type GitState struct {
	// Branch is the worktree's branch, "" when the probe did not resolve one.
	Branch string
	// Dirty is true when the worktree holds uncommitted work.
	Dirty bool
	// StashCount is the number of stash entries.
	StashCount int
	// UnpushedCommits is the number of commits the probe found preserved
	// nowhere on the rig's origin remote.
	UnpushedCommits int
}

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

// IsSubmitted reports whether the bead is submitted for landing: it carries
// gt:ready-to-land and is not finished. It is the rule the daemon's crash
// detector and the polecat state readers each apply, named once here so the
// tick cannot answer it differently — gt-xs1ni was the daemon and gt polecat
// list disagreeing about the same seat.
// TestPatrolScanIsSubmittedMatchesPolecat pins this copy to the original.
func (w Work) IsSubmitted() bool {
	if w.IsTerminal() {
		return false
	}
	return w.HasLabel(ReadyToLandLabel)
}

// IsTerminal reports whether the work is over for good: closed or tombstone.
// It repeats beads.IssueStatus.IsTerminal to keep this package out of the
// beads dependency tree; TestPatrolScanIsSubmittedMatchesPolecat pins the two
// to one answer on every status a work bead can be in.
func (w Work) IsTerminal() bool {
	switch strings.TrimSpace(w.Status) {
	case "closed", "tombstone":
		return true
	}
	return false
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
	// WorkBead returns the work bead with the given ID, or nil when the bead
	// is gone. It reads one bead a submitted seat's record names, to tell a
	// real landing wait from a record its landing outlived (gt-xs1ni).
	WorkBead(rig, id string) (*Work, error)
	// ClearSubmission ends a submitted seat's wait whose work bead is no
	// longer submitted for landing, so the ordinary path can decide what the
	// seat needs. It reports whether the record changed.
	ClearSubmission(rig, polecat, workBead string) (bool, error)
	// AgentRecord returns the agent_state, gt done exit type and cleanup
	// status the polecat's agent bead records. It is read only to refuse a
	// restart, never to cause one.
	AgentRecord(rig, polecat string) (AgentRecord, error)
	// GitState measures the polecat's worktree live. A probe that could not
	// answer returns an error, never a zero state: the tick reads a failed
	// measurement as Unknown and clears nothing on it.
	GitState(rig, polecat string) (GitState, error)
	// Heartbeat returns the session heartbeat, nil when there is none.
	Heartbeat(rig, polecat string) *Heartbeat
	// Restart restarts the seat through the supervisor.
	Restart(rig, polecat, reason string) error
	// MarkIdle retires the seat's intent record to stop: its session is gone
	// and it holds no work, so nothing needs it running. It reports whether
	// the record changed.
	MarkIdle(rig, polecat string) (bool, error)
	// ClearEscalation resets the agent bead of a seat whose ESCALATED blocker
	// is resolved: agent_state to idle and the exit type removed, the state
	// and the field gt done's exit wrote together. It leaves the seat's
	// branch and directory alone, and reports whether the bead changed.
	ClearEscalation(rig, polecat string) (bool, error)
	// MarkSubmitted records the seat's work bead as submitted for landing, so
	// a seat the tick found mid-landing is read from its record on later
	// ticks rather than from liveness.
	MarkSubmitted(rig, polecat, workBead string) error

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
	// SurvivingBranch returns the polecat branch for beadID that is present
	// on the rig's origin remote, "" when none is; an error means unknown.
	SurvivingBranch(rig, beadID string) (string, error)
	// RecordResumeBranch appends the resume_branch notes line naming branch.
	RecordResumeBranch(rig, beadID, branch string) error
	// Reopen releases a gone polecat's work bead to the ready queue, open and
	// unassigned, but only while it is still assigned to assignee: the guard
	// is what makes the release happen once however many ticks see the bead.
	// It reports whether the bead changed.
	Reopen(rig, beadID, assignee string) (bool, error)
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
	// is not): the dispatchers' hold rule. Nil means no hold rule.
	HoldReason func(Work) string
	// IsRefusal reports whether a Restart error is the supervisor refusing
	// (parked, frozen, e-stop, budget) rather than failing.
	IsRefusal func(error) bool
	// Reap enables the worktree reap pass. Nil leaves it off, and it does
	// nothing unless the Env also implements ReapEnv.
	Reap *ReapOptions
	Now  func() time.Time
}

// Scanner runs ticks.
type Scanner struct {
	env     Env
	ledger  Ledger
	o       Options
	reapEnv ReapEnv
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
	s := &Scanner{env: env, ledger: ledger, o: o}
	if o.Reap != nil {
		r := *o.Reap
		if r.Grace <= 0 {
			r.Grace = DefaultReapGrace
		}
		if r.ParkedGrace <= 0 {
			r.ParkedGrace = DefaultReapParkedGrace
		}
		if r.MaxPerTick <= 0 {
			r.MaxPerTick = DefaultReapMaxPerTick
		}
		s.o.Reap = &r
		s.reapEnv, _ = env.(ReapEnv)
	}
	return s
}

// Outcome names what the tick did about one seat or bead.
type Outcome string

const (
	OutcomeRestarted Outcome = "restarted"
	OutcomeRefused   Outcome = "refused"    // the supervisor said no
	OutcomeFailed    Outcome = "failed"     // an action ran and failed
	OutcomeSkipped   Outcome = "skipped"    // a guard said leave it
	OutcomeUnknown   Outcome = "unknown"    // a read failed; nothing done
	OutcomeWaiting   Outcome = "waiting"    // dead, but not confirmed yet
	OutcomeStalled   Outcome = "stalled"    // reported only
	OutcomeClosed    Outcome = "closed"     // orphaned molecule closed
	OutcomeReopened  Outcome = "reopened"   // dead holder's bead returned to the queue
	OutcomeIdled     Outcome = "idled"      // idle seat's record retired to stop
	OutcomeCleared   Outcome = "cleared"    // resolved escalation's record reset to idle
	OutcomeReaped    Outcome = "reaped"     // finished seat's worktree removed
	OutcomeWouldReap Outcome = "would-reap" // dry-run: a seat a real run would remove
	OutcomeBlocked   Outcome = "blocked"    // not safe to remove, or nuke refused
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
	lines := []string{fmt.Sprintf("%s: checked %d polecat(s): %d restarted, %d refused, %d unknown, %d molecule(s) closed, %d reopened, %d error(s), %d reaped, %d would-reap, %d blocked",
		r.Rig, r.Checked, r.Count(OutcomeRestarted), r.Count(OutcomeRefused), r.Count(OutcomeUnknown),
		r.Count(OutcomeClosed), r.Count(OutcomeReopened), len(r.Errors),
		r.Count(OutcomeReaped), r.Count(OutcomeWouldReap), r.Count(OutcomeBlocked))}
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
		// The reap pass runs after seat() and before orphans(): removing a
		// directory makes a dead holder's bead recoverable, and the reap
		// vetoes keep any seat with a live bead out of the reaper, so recovery
		// never sees a reaped seat's work.
		left := 0
		if s.reapEnv != nil {
			left = s.o.Reap.MaxPerTick
		}
		for _, name := range polecats {
			if f, ok := s.reap(rig, name, &left); ok {
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

	// The intent record first: it never depends on Dolt, and a held seat
	// needs no liveness sample to be left alone.
	rec, err := s.env.Intent(rig, name)
	if err != nil {
		return unknown("intent record unreadable", err)
	}
	if rec.Held() {
		return Finding{}, false // parked or frozen: the operator's, silently
	}
	if rec.Submitted() {
		// The record is a wait for the landing worker, and the wait is real
		// only while the bead it names is still submitted. gt done writes the
		// label before the record, so a bead that no longer reads as
		// submitted was pulled after the submission — for rework, or by a
		// human — and only the record outlived it. End the wait and fall
		// through to the ordinary dead-session path below: nothing else ever
		// restarts the seat (gt-xs1ni). An unreadable bead is Unknown, never
		// a fall-through — restarting on a failed read is the double-spawn
		// gt-obbx2 closed.
		waiting, err := s.submittedWait(rig, rec)
		if err != nil {
			return unknown("submitted work unreadable", err)
		}
		if waiting {
			return Finding{}, false // gt done handed it to the landing worker
		}
		if _, err := s.env.ClearSubmission(rig, name, rec.WorkBead); err != nil {
			return unknown("ending the stale submission", err)
		}
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
		// Idle or finished: a dead session is not a crash. The record still
		// saying run would keep townhealth reporting the seat dead forever
		// (gt-613vw), so the seat is retired to stop once the death is
		// confirmed across ticks, exactly as a restart is.
		if samples < s.o.DeadSamples {
			return Finding{}, false
		}
		// A seat the last turn left stuck after an ESCALATED exit is cleared
		// here, before it is retired: a resolved escalation has no work left,
		// so the agent bead is the last record still asking for recovery
		// (gt-fn9e6.33). Reading it only once the death is confirmed keeps an
		// alive or spawning seat free of the Dolt read.
		agent, err := s.env.AgentRecord(rig, name)
		if err != nil {
			return unknown("agent state unreadable", err)
		}
		if f, ok := s.clearEscalation(rig, name, agent); ok {
			return f, true
		}
		changed, err := s.env.MarkIdle(rig, name)
		if err != nil {
			f.Outcome, f.Detail = OutcomeFailed, "retiring idle seat: "+err.Error()
			return f, true
		}
		if !changed {
			return Finding{}, false // already stopped on an earlier tick
		}
		f.Outcome, f.Detail = OutcomeIdled, "no session and no work; record retired to stop"
		return f, true
	}
	if strings.EqualFold(work.Status, "closed") {
		return Finding{}, false
	}
	if work.IsSubmitted() {
		// gt done writes this label before the record, and that record write is
		// best-effort: a seat whose write was lost still holds the label while
		// the record says run. The label is the authoritative half, so the
		// record is brought up to it. MarkSubmitted also drops the dead sample
		// Assess has just taken, which is what townhealth reads to report a
		// mid-landing seat dead (gt-2z8k1), and later ticks take the record
		// check above before Dolt is read at all.
		if err := s.env.MarkSubmitted(rig, name, work.ID); err != nil {
			f.Outcome, f.Detail = OutcomeFailed, "recording "+work.ID+" as submitted: "+err.Error()
			return f, true
		}
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
	agent, err := s.env.AgentRecord(rig, name)
	if err != nil {
		return unknown("agent state unreadable", err)
	}
	// deferredExit records that the bead reads stuck after a DEFERRED exit:
	// the turn ended without completing or escalating, so a successor is
	// expected to carry the bead on. It is not a hold, and the restart below
	// says so.
	deferredExit := false
	switch state := strings.ToLower(strings.TrimSpace(agent.State)); state {
	case "stuck":
		// agent_state=stuck is not a self-park. The only writer is gt done's
		// own exit path, which sets it for every non-COMPLETED exit
		// (internal/cmd/done_agent_state.go); a live polecat that is stuck
		// holds the seat through its heartbeat instead, which is checked
		// below. A stale stuck bead left by a DEFERRED exit is a finished
		// turn whose worktree still holds unpushed commits and whose bead is
		// still hooked, so it falls through to the ordinary dead-session path
		// and the supervisor restarts the seat on its preserved branch rather
		// than leaving the work dead until an operator notices (gt-ks62m). An
		// ESCALATED exit is the operator's: the polecat stopped on purpose to
		// raise a blocker, and a restart would fight it. A resolved one never
		// arrives here — its bead is closed, so the tick reaches it with no
		// work and clears it there (resolvedEscalationEvidence).
		if !strings.EqualFold(strings.TrimSpace(agent.ExitType), ExitDeferred) {
			detail := "agent_state stuck: stopped on purpose"
			if e := strings.TrimSpace(agent.ExitType); e != "" {
				detail += " (exit " + e + ")"
			}
			return skip(detail)
		}
		deferredExit = true
	case "awaiting-gate", "paused":
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
	if deferredExit {
		// A restart here is a recovery, not a crash relaunch: name the turn
		// that ended and what the worktree holds, so the reason line reads as
		// the recovery it is (gt-ks62m).
		cleanup := strings.TrimSpace(agent.CleanupStatus)
		if cleanup == "" {
			cleanup = "unknown"
		}
		reason = fmt.Sprintf("patrol scan: %s with %s hooked after a DEFERRED exit (cleanup_status %s, %d dead samples)",
			res.Reason, work.ID, cleanup, samples)
	}
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

// resolvedEscalationEvidence returns the evidence that the seat the last turn
// left stuck after an ESCALATED exit has had its blocker resolved, or "" when
// it may not be cleared. Every condition is measured now, so a seat that fails
// any one of them is left exactly as the tick leaves it today — the operator's
// escalation, or an unmeasured one. An error is a condition the tick could not
// measure, and nothing is cleared on an unknown.
//
// A resolved escalation also means the seat has no work: its work bead is
// closed, so the tick reaches this from the no-work path. A seat whose bead is
// still open is skipped further up, which is the "still the operator's" case
// (gt-fn9e6.33).
func (s *Scanner) resolvedEscalationEvidence(rig, name string, agent AgentRecord) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(agent.State), "stuck") ||
		!strings.EqualFold(strings.TrimSpace(agent.ExitType), ExitEscalated) {
		return "", nil
	}
	// A record still pointing at a hooked bead has not finished the turn that
	// escalated, whatever its state says.
	if strings.TrimSpace(agent.HookBead) != "" {
		return "", nil
	}
	live, err := s.env.SessionExists(rig, name)
	if err != nil {
		return "", fmt.Errorf("session query: %w", err)
	}
	if live {
		return "", nil
	}
	// The bead the turn ran on is the escalation's subject: while it is open
	// the blocker stands, and a record naming none proves nothing.
	source := strings.TrimSpace(agent.LastSourceIssue)
	if source == "" {
		return "", nil
	}
	work, err := s.env.WorkBead(rig, source)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", source, err)
	}
	if work == nil || !work.IsTerminal() {
		return "", nil
	}
	git, err := s.env.GitState(rig, name)
	if err != nil {
		return "", fmt.Errorf("measuring the worktree: %w", err)
	}
	if git.Dirty || git.StashCount > 0 || git.UnpushedCommits > 0 {
		return "", nil
	}
	if git.Branch != "" {
		return fmt.Sprintf("%s is closed; no session, no hook, worktree clean on %s", source, git.Branch), nil
	}
	return source + " is closed; no session, no hook, worktree clean", nil
}

// clearEscalation clears one seat's resolved ESCALATED record and reports the
// finding to log. ok is false when the seat is left exactly as the tick would
// have left it without this pass: no resolution proven, or a record an earlier
// tick already cleared.
func (s *Scanner) clearEscalation(rig, name string, agent AgentRecord) (Finding, bool) {
	f := Finding{Kind: "seat", Subject: name}
	evidence, err := s.resolvedEscalationEvidence(rig, name, agent)
	if err != nil {
		f.Outcome, f.Detail = OutcomeUnknown, "checking a resolved escalation: "+err.Error()
		return f, true
	}
	if evidence == "" {
		return Finding{}, false
	}
	changed, err := s.env.ClearEscalation(rig, name)
	if err != nil {
		f.Outcome, f.Detail = OutcomeFailed, "clearing a resolved escalation: "+err.Error()
		return f, true
	}
	if !changed {
		return Finding{}, false
	}
	f.Outcome, f.Detail = OutcomeCleared, "exit ESCALATED, blocker resolved ("+evidence+"); record reset to idle"
	return f, true
}

// submittedWait reports whether a submitted seat's record still describes a
// landing that is waiting: the bead the record names carries
// gt:ready-to-land and has not closed. A record with no work_bead keeps the
// wait — it was written before the field existed and names nothing to check.
func (s *Scanner) submittedWait(rig string, rec intent.Record) (bool, error) {
	if rec.WorkBead == "" {
		return true, nil
	}
	work, err := s.env.WorkBead(rig, rec.WorkBead)
	if err != nil {
		return false, err
	}
	if work == nil {
		return false, nil // the bead is gone: nothing is waiting to land
	}
	return work.IsSubmitted(), nil
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
		if w.IsSubmitted() {
			continue // the landing worker owns it; a dead holder is expected
		}
		if f, ok := s.molecule(w, name); ok {
			r.Findings = append(r.Findings, f)
		}
		if f, ok := s.recover(rig, w, name); ok {
			r.Findings = append(r.Findings, f)
		}
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

// recover returns a gone polecat's work bead to the ready queue — open,
// unassigned — with the branch its work survives on recorded as a notes line,
// so the spec dispatcher resumes that branch rather than starting fresh from
// main (gt-gzhin.2). It reports a finding only when there is something to say.
//
// Every read and write fails closed. An unreadable seat record, a branch
// state that cannot be determined, or a notes write that failed leaves the
// bead hooked where it is; a parked seat is the operator's. The guarded
// release makes the bead change once, so a later tick that still sees it
// (the release itself failed) retries instead of racing.
func (s *Scanner) recover(rig string, w Work, holder string) (Finding, bool) {
	f := Finding{Kind: "stranded", Subject: w.ID}
	rec, err := s.env.Intent(rig, holder)
	if err != nil {
		f.Outcome, f.Detail = OutcomeUnknown, "holder record unreadable: "+err.Error()
		return f, true
	}
	if rec.Held() {
		return Finding{}, false // parked or frozen: the operator's, silently
	}

	branch, err := s.env.SurvivingBranch(rig, w.ID)
	if err != nil {
		f.Outcome, f.Detail = OutcomeUnknown, "surviving-work check failed: "+err.Error()
		return f, true
	}
	// The notes line is written first: a release whose branch record was lost
	// sends the dispatcher to main on work that exists on a branch, which is
	// the discard this recovery exists to prevent. A retry finds the line
	// already there and writes only the release.
	if branch != "" && !hasResumeBranch(w.Notes, branch) {
		if err := s.env.RecordResumeBranch(rig, w.ID, branch); err != nil {
			f.Outcome, f.Detail = OutcomeFailed, "recording "+ResumeBranchNote(branch)+": "+err.Error()
			return f, true
		}
	}
	reopened, err := s.env.Reopen(rig, w.ID, w.Assignee)
	if err != nil {
		f.Outcome, f.Detail = OutcomeFailed, "reopening "+w.ID+": "+err.Error()
		return f, true
	}
	if !reopened {
		return Finding{}, false // released or reassigned since the list read
	}

	f.Outcome = OutcomeReopened
	f.Detail = "no surviving branch"
	if branch != "" {
		f.Detail = ResumeBranchNote(branch)
	}
	s.reopenComment(rig, w, holder, branch, &f)
	return f, true
}

// reopenComment leaves the durable human-readable record of a recovery, once
// per report window: the status change is a bead event nothing reads back,
// and the notes line is machine contract, not narrative.
func (s *Scanner) reopenComment(rig string, w Work, holder, branch string, f *Finding) {
	now := s.o.Now()
	key := rig + "/" + w.ID
	if last, ok := s.ledger.LastReported(key); ok && now.Sub(last) < s.o.ReportWindow {
		return
	}
	if err := s.env.Comment(w.ID, ReopenedComment(w, rig, holder, branch)); err != nil {
		// The bead is reopened; only its record is missing. Say so rather than
		// claim a clean recovery.
		f.Detail += "; comment failed: " + err.Error()
		return
	}
	if err := s.ledger.MarkReported(key, now); err != nil {
		f.Detail += "; ledger not updated: " + err.Error()
	}
}

// ReopenedComment is the text of the record left on a bead the tick returned
// to the ready queue.
func ReopenedComment(w Work, rig, holder, branch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REOPENED (patrol scan): %s was %s and assigned to %s/polecats/%s, which no longer exists (no session, no directory).", w.ID, w.Status, rig, holder)
	b.WriteString(" Returned to the ready queue, unassigned.")
	if branch != "" {
		fmt.Fprintf(&b, " Unlanded work survives on branch %s (%s); resume it rather than starting from main.", branch, ResumeBranchNote(branch))
	} else {
		b.WriteString(" No polecat branch carries unlanded work for it.")
	}
	return b.String()
}
