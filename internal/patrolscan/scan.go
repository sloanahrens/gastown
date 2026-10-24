// Package patrolscan is the deterministic half of the witness patrol, run by
// the daemon as the patrol_scan tick (ADR 0005, gt-4k3fj.6). One tick scans one
// rig and does five things:
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
//     as recovery-needed work (gt-fn9e6.33); a seat whose last turn exited
//     DEFERRED with its work bead closed (a report-only job) is cleared the
//     same way (gt-oa6du); see finishedExitEvidence.
//     A seat still holding the bead it escalated on is restarted on its
//     preserved branch once the escalation that turn raised for the bead is
//     closed, and stays skipped while it is open or cannot be read
//     (gt-4hduq); see SeatEscalationVerdict. A LIVE seat stalled on work that
//     is already closed, or on none, is stopped and retired like an idle one:
//     it has nothing left to progress on and only holds its seat (gt-9p99w);
//   - leaked wisps: a wisp a gone polecat still holds, whether it is pinned,
//     open or in_progress, is closed once it is a day old and its root is
//     gone or closed too; see leakedWisps. An open molecule root a day old
//     whose work bead is closed is closed with its steps unless a polecat
//     still alive holds it (closedWorkRoots).
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

	"github.com/steveyegge/gastown/internal/deadseats"
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

// SpawningAgentState is the agent_state gt sling writes before the session and
// the worktree exist, and the only state that reads as mid-dispatch. It is
// beads.AgentStateSpawning, repeated here so this package stays out of the
// beads dependency tree; a daemon test pins the two together.
const SpawningAgentState = "spawning"

// SpawnGrace reports whether a seat whose session is not up is still coming
// up rather than crashed: its agent record says spawning, and the hook that
// dispatched it was written inside the window.
//
// It is polecat.SpawnGrace's rule (gt-yteq), repeated here to keep this
// package out of the polecat dependency tree;
// TestPatrolScanSpawnGraceMatchesPolecat pins the two to one answer.
//
// Only an explicit spawning state earns the window: a running polecat keeps
// writing its own work bead, so that write cannot stand in for a dispatch
// (gt-6hby3). A missing fact (no window, no timestamp, no clock) returns
// false, so a bead nobody can date never earns the grace.
func SpawnGrace(agentState string, updatedAt, now time.Time, grace time.Duration) bool {
	if strings.TrimSpace(agentState) != SpawningAgentState {
		return false
	}
	if grace <= 0 || updatedAt.IsZero() || now.IsZero() {
		return false
	}
	return now.Sub(updatedAt) < grace
}

// Defaults.
const (
	// DefaultDeadSamples is how many consecutive dead samples a seat needs
	// before it is restarted. The count lives in the intent record, so a
	// daemon restart does not reset it. deadseats holds the one value, since
	// the roster and the health line confirm a death at the same count
	// (gt-4rxdi).
	DefaultDeadSamples = deadseats.DefaultSamples
	// DefaultSpawnGrace is the window a dispatched seat is read as starting
	// up rather than crashed in (SpawnGrace).
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
	// UpdatedAt is the record's last write, zero when it carries none. Nothing
	// writes the agent bead between the spawn and gt done, so while the state
	// still says spawning this is the spawn time (gt-i3ky2).
	UpdatedAt time.Time
}

// StoppedOnEscalated reports whether the record is the one gt done's ESCALATED
// exit wrote: agent_state stuck beside exit_type ESCALATED. It is the state a
// polecat stopped in on purpose, holding a bead it raised a blocker for, and
// the one both the tick and the daemon's crash detector read before deciding
// whether anything may restart the seat (gt-4hduq).
func (r AgentRecord) StoppedOnEscalated() bool {
	return strings.EqualFold(strings.TrimSpace(r.State), "stuck") &&
		strings.EqualFold(strings.TrimSpace(r.ExitType), ExitEscalated)
}

// Escalation is the slice of an escalation bead the tick matches a stopped
// seat's turn against: who raised it, which bead it names, and whether it is
// still open.
type Escalation struct {
	// ID is the escalation bead's ID, named in the tick's report.
	ID string
	// EscalatedBy is the agent address that raised it, as gt escalate wrote it
	// ("beads/polecats/guzzle").
	EscalatedBy string
	// Title is the escalation's title. The raiser's convention, and the tie
	// that names the bead, is to prefix it with the work bead the turn ran on
	// ("be-01x: <the decision being raised>").
	Title string
	// RelatedBead is the escalation's related_bead field, "" when the raiser
	// set none.
	RelatedBead string
	// Open is true while the escalation has not been closed.
	Open bool
}

// EscalationVerdict is what the escalations a seat raised for one work bead
// say about restarting the seat that stopped on them.
type EscalationVerdict string

const (
	// EscalationPending: the seat's escalation for the bead is still open, so
	// the decision it stopped for is still pending and the seat is left
	// skipped, exactly as before this rule existed (gt-fn9e6.33).
	EscalationPending EscalationVerdict = "pending"
	// EscalationResolved: the seat's escalation for the bead is closed and none
	// of its escalations for that bead is open, so the turn it stopped can be
	// resumed on its preserved branch (gt-4hduq).
	EscalationResolved EscalationVerdict = "resolved"
	// EscalationUnnamed: no escalation the seat raised names the bead. Nothing
	// proves the blocker spent, so the seat is left skipped.
	EscalationUnnamed EscalationVerdict = "unnamed"
	// EscalationNotStopped: the seat's record is not the ESCALATED stop, so it
	// is waiting on no escalation whatever it has raised (SeatEscalationHold).
	// SeatEscalationVerdict never answers it.
	EscalationNotStopped EscalationVerdict = "not-stopped"
)

// SeatEscalation is the verdict on the escalations one seat raised for one
// work bead.
type SeatEscalation struct {
	Verdict EscalationVerdict
	// ID is the escalation the verdict turns on, "" when none names the bead.
	ID string
	// Detail is why the seat is held, "" when the verdict is resolved.
	Detail string
}

// SeatEscalationVerdict applies the tie between a stopped seat, its work bead
// and the escalations the seat raised, and says whether the seat may be
// restarted. The tie is measured, never assumed (gt-4hduq): the raiser is the
// escalation's escalated_by, which gt escalate writes as the seat's own
// address (rig/polecats/name), and the bead the turn ran on is the one the
// title prefixes ("be-01x: ...") or related_bead names. An escalation someone
// else raised about the same bead is not the seat's and proves nothing about
// its turn; an escalation of the seat's that names no bead proves nothing
// about this one, and either way the seat is held.
//
// One open escalation of the seat's for the bead holds the seat whatever else
// exists — a decision is still pending. Only when none is open does a closed
// one release it.
func SeatEscalationVerdict(rig, name, bead string, raised []Escalation) SeatEscalation {
	seat := rig + "/polecats/" + name
	matches := make([]Escalation, 0, len(raised))
	for _, e := range raised {
		if !strings.EqualFold(strings.TrimSpace(e.EscalatedBy), seat) {
			continue
		}
		if !escalationNamesBead(e, bead) {
			continue
		}
		matches = append(matches, e)
	}
	// The listing order is bd's, so the verdict is taken over a sorted copy:
	// the same set of escalations must answer the same way on every tick.
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	for _, e := range matches {
		if e.Open {
			return SeatEscalation{Verdict: EscalationPending, ID: e.ID,
				Detail: "agent_state stuck: exit ESCALATED and " + e.ID + " is still open for " + bead}
		}
	}
	if len(matches) > 0 {
		return SeatEscalation{Verdict: EscalationResolved, ID: matches[0].ID}
	}
	return SeatEscalation{Verdict: EscalationUnnamed,
		Detail: "agent_state stuck: exit ESCALATED and no escalation of the seat's names " + bead}
}

// SeatEscalationHold is the one answer to "is this seat waiting on an
// escalation": the seat's record says it stopped on one (StoppedOnEscalated),
// and the verdict over the escalations it raised for bead says what became of
// it. The tick, the daemon's crash detector and the overrun guard all read it,
// so the three cannot disagree. A live seat that raised an escalation and kept
// working is not held, and its escalations are not read: an open escalation
// alone does not make a seat a waiter.
//
// A record that is not the ESCALATED stop answers EscalationNotStopped. The
// escalations are read only for a stopped seat, so an error means a stopped
// seat whose escalations could not be read, and each caller picks its own
// fail-closed direction.
func SeatEscalationHold(rec AgentRecord, rig, name, bead string, readRaised func() ([]Escalation, error)) (SeatEscalation, error) {
	if !rec.StoppedOnEscalated() {
		return SeatEscalation{Verdict: EscalationNotStopped}, nil
	}
	raised, err := readRaised()
	if err != nil {
		return SeatEscalation{}, err
	}
	return SeatEscalationVerdict(rig, name, bead, raised), nil
}

// escalationNamesBead reports whether one escalation names bead: its
// related_bead field, or the work bead the raiser prefixed onto the title. A
// title that merely mentions the bead ("escalating about be-01x") does not
// name it, and the seat stays held.
func escalationNamesBead(e Escalation, bead string) bool {
	if strings.TrimSpace(bead) == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(e.RelatedBead), bead) {
		return true
	}
	title := strings.TrimSpace(e.Title)
	if len(title) <= len(bead) || !strings.EqualFold(title[:len(bead)], bead) {
		return false
	}
	return strings.HasPrefix(title[len(bead):], ":")
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

// SeatCap is the polecat_pool cap a crash restart answers to and the live
// census behind it (gt-02tcp).
//
// The census counts live polecats, not the seats the pool calls occupied, and
// it leaves out the session of the seat being restarted: a polecat whose agent
// died with its tmux session kept is restarted in that session, one for one,
// so the restart adds no live polecat and must not wait on itself (gt-zwiww).
//
// The zero value is no cap holding the restart: a pool with none, or a cap
// with room. A seat the operator closed with a cap of 0 is not that zero
// value: Closed holds its restart.
type SeatCap struct {
	// Live is how many polecats are live now.
	Live int
	// Limit is the cap they are counted against: the seat's cap for the
	// agent's own seat, or polecat_pool.max_total across every seat. Zero
	// is no cap, which is the answer when the pool has none.
	Limit int
	// Closed is set for a seat the operator closed with a cap of 0 (gt pool
	// set <agent>=0) or one inside its closed window (gt-her01): no restart
	// goes onto it however few polecats are live. Limit stays 0 there, since
	// zero is also how a pool states no cap.
	Closed bool
	// ClosedDetail, when Closed is set, says what shuts the seat, in place of
	// the default "<Key> is 0": a seat inside its closed window names the
	// window and the hour it opens.
	ClosedDetail string
	// Key names the polecat_pool key Limit came from — the key the operator
	// raises — so the wait names what holds it: "polecat_pool.seats.<agent>.cap"
	// or "polecat_pool.max_total". Empty when there is no cap.
	Key string
}

// Full reports whether the cap has no room for one more live polecat. A Limit
// of zero is no cap, so it is never full, unless Closed says the zero is a seat
// the operator closed.
func (c SeatCap) Full() bool { return c.Closed || (c.Limit > 0 && c.Live >= c.Limit) }

// Wisp is the slice of a non-closed wisp the leaked-wisp pass decides on.
type Wisp struct {
	ID       string
	Status   string
	Assignee string
	// Parent is the root or step this wisp hangs under, "" for none.
	Parent string
	// CreatedAt is when the wisp was created; zero when the host could not
	// read it, which the pass reads as an unknown age.
	CreatedAt time.Time
}

// IsClosed reports whether the wisp is over for good: closed or tombstone.
func (w Wisp) IsClosed() bool { return Work{Status: w.Status}.IsTerminal() }

// WorkRef is the work bead a molecule root's blocks edge points at, with the
// status the edge reports for it ("" when that could not be read).
type WorkRef struct {
	ID     string
	Status string
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
	// AssignedWork returns the polecat's hooked, in_progress or open work
	// bead, or nil when it has none.
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
	// PolecatEscalations returns the escalation records raised by the rig's
	// polecat seats, open and closed: the ones a seat that stopped on an
	// escalation is matched against (SeatEscalationVerdict). An error means
	// the read could not answer, and the tick holds the seat on it.
	PolecatEscalations(rig string) ([]Escalation, error)
	// GitState measures the polecat's worktree live. A probe that could not
	// answer returns an error, never a zero state: the tick reads a failed
	// measurement as Unknown and clears nothing on it.
	GitState(rig, polecat string) (GitState, error)
	// Heartbeat returns the session heartbeat, nil when there is none.
	Heartbeat(rig, polecat string) *Heartbeat
	// Restart restarts the seat through the supervisor.
	Restart(rig, polecat, reason string) error
	// Kill stops the seat's live session through the supervisor, which
	// refuses it for a held or e-stopped seat exactly as it refuses Restart.
	// It is the automatic-path kill, so a stall can never outrun an operator's
	// park or e-stop (gt-9p99w).
	Kill(rig, polecat, reason string) error
	// StalledReleaseOwed reports whether a stalled seat's release, whose kill
	// stopped the session, still owes its sandbox removal. An unreadable seat
	// record is an error: the debt is never assumed absent.
	StalledReleaseOwed(rig, polecat string) (bool, error)
	// FinishStalledRelease completes such a release: it retries the removal and
	// retires the seat to stop. Called only for a seat StalledReleaseOwed reports.
	FinishStalledRelease(rig, polecat string) error
	// MarkIdle retires the seat's intent record to stop: its session is gone
	// and it holds no work, so nothing needs it running. It reports whether
	// the record changed.
	MarkIdle(rig, polecat string) (bool, error)
	// ClearStuckExit resets the agent bead of a seat whose last turn is
	// finished and left behind by gt done: agent_state to idle and the exit
	// type removed, the state and the field gt done's exit wrote together.
	// exit is the exit type the tick decided on (ESCALATED or DEFERRED); a
	// record that no longer reads stuck with that exit is left alone. It
	// leaves the seat's branch and directory alone, and reports whether the
	// bead changed.
	ClearStuckExit(rig, polecat, exit string) (bool, error)
	// MarkSubmitted records the seat's work bead as submitted for landing, so
	// a seat the tick found mid-landing is read from its record on later
	// ticks rather than from liveness.
	MarkSubmitted(rig, polecat, workBead string) error

	// ActiveWork lists hooked, in_progress and open beads assigned to polecats
	// of the rig (assignee "<rig>/polecats/<name>"). All three are assigned
	// statuses; a host that drops one hides that work from the orphan and
	// held-wisp passes (gt-gnqmu).
	ActiveWork(rig string) ([]Work, error)
	// PolecatDirExists reports whether the polecat's directory exists.
	PolecatDirExists(rig, polecat string) (bool, error)
	// SessionExists reports whether the polecat's tmux session exists.
	SessionExists(rig, polecat string) (bool, error)
	// MoleculeStatus returns a molecule root's status; "" with no error
	// means the root is gone (reaped).
	MoleculeStatus(id string) (string, error)
	// CloseMolecule closes a molecule root and its step wisps, returning how
	// many closed. It closes a pinned wisp too.
	CloseMolecule(id, reason string) (int, error)
	// LeakedWisps lists the rig's wisps that are not closed: open,
	// in_progress, pinned and hooked. A status that cannot be listed fails
	// the whole read, since it may be the one holding the leak.
	LeakedWisps(rig string) ([]Wisp, error)
	// WispStatus returns the status of the wisp or bead id names; "" with no
	// error means it is gone (reaped). It reads the parent a listed wisp
	// names.
	WispStatus(id string) (string, error)
	// RootWork returns the work beads a molecule root's blocks edges point
	// at, each with its status as the edge reads it. Empty with no error
	// means the root has no such edge (a patrol root); a root that cannot be
	// read, or an edge whose bead has no readable status, is an error or an
	// empty Status, and the pass treats either as Unknown.
	RootWork(root string) ([]WorkRef, error)
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
	DeadSamples int
	// SpawnGrace overrides DefaultSpawnGrace.
	SpawnGrace     time.Duration
	ReportWindow   time.Duration
	HeartbeatFresh time.Duration
	// HoldReason returns why a work bead is held from dispatch ("" when it
	// is not): the dispatchers' hold rule. Nil means no hold rule.
	HoldReason func(Work) string
	// IsRefusal reports whether a Restart or Kill error is the supervisor
	// refusing (parked, frozen, e-stop, budget) rather than failing.
	IsRefusal func(error) bool
	// SeatCap reads the polecat_pool cap a crash restart of rig/name answers
	// to, counting every live polecat session but name's own: that session,
	// if it still exists, is the one the restart replaces. Nil leaves the
	// tick uncapped, the behavior before gt-02tcp. An error means the cap
	// could not be read, which is Unknown: a restart taken on an unread cap
	// is a session raised past a limit nobody measured.
	SeatCap func(rig, name string) (SeatCap, error)
	// DoneAge reads how long before now the seat's own most recent event was
	// a gt done for workBead, and false when it was not (no such event, a
	// newer event of the seat's, or a log that cannot be read). With
	// DoneCloseGrace it keeps the DEFERRED recovery from restarting a seat
	// whose bead has had no time to close behind its done (gt-iwgbd). Nil
	// leaves the recovery restarting at once, as before.
	DoneAge func(rig, name, workBead string, now time.Time) (time.Duration, bool)
	// DoneCloseGrace is how long after its done a seat holding an unclosed
	// bead is read as a finished turn. Zero or less turns the wait off. The
	// daemon sets it to the window its crash detector uses (gt-0p9pv).
	DoneCloseGrace time.Duration
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
	OutcomeReleased  Outcome = "released"   // stalled seat on finished work stopped and retired
	OutcomeClosed    Outcome = "closed"     // orphaned molecule closed
	OutcomeReopened  Outcome = "reopened"   // dead holder's bead returned to the queue
	OutcomeIdled     Outcome = "idled"      // idle seat's record retired to stop
	OutcomeCleared   Outcome = "cleared"    // finished exit's record reset to idle
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
	lines := []string{fmt.Sprintf("%s: checked %d polecat(s): %d restarted, %d refused, %d unknown, %d released, %d molecule(s) closed, %d reopened, %d error(s), %d reaped, %d would-reap, %d blocked",
		r.Rig, r.Checked, r.Count(OutcomeRestarted), r.Count(OutcomeRefused), r.Count(OutcomeUnknown), r.Count(OutcomeReleased),
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
	s.leakedWisps(rig, &r)
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
		if f, ok := s.releaseStalled(rig, name, res); ok {
			return f, true
		}
		// Reported only: a stall on work that is not over is a long turn,
		// the usual explanation, and the stall threshold is tuned after a
		// measured week (ADR 0005).
		f.Outcome, f.Detail = OutcomeStalled, res.Reason
		return f, true
	}

	// Dead. Confirm it across ticks before acting.
	samples := 0
	if res.Sample != nil {
		samples = res.Sample.DeadSamples
	}

	// A stalled seat whose kill stopped the session but whose sandbox removal
	// failed still holds its record, and nothing else reaches that sandbox. The
	// release is finished here, as the stall path would have finished it.
	owed, err := s.env.StalledReleaseOwed(rig, name)
	if err != nil {
		return unknown("stalled release unreadable", err)
	}
	if owed {
		if err := s.env.FinishStalledRelease(rig, name); err != nil {
			f.Outcome, f.Detail = OutcomeFailed, err.Error()
			return f, true
		}
		f.Outcome, f.Detail = OutcomeReleased, "sandbox removal finished after a stalled kill; session stopped and seat retired to stop"
		return f, true
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
		if !deadseats.Confirmed(samples, s.o.DeadSamples) {
			return Finding{}, false
		}
		// A seat the last turn left stuck after an ESCALATED or DEFERRED exit
		// is cleared here, before it is retired: a resolved escalation or a
		// closed report-only bead has no work left, so the agent bead is the
		// last record still asking for recovery (gt-fn9e6.33, gt-oa6du).
		// Reading it only once the death is confirmed keeps an
		// alive or spawning seat free of the Dolt read.
		agent, err := s.env.AgentRecord(rig, name)
		if err != nil {
			return unknown("agent state unreadable", err)
		}
		if f, ok := s.clearFinishedExit(rig, name, agent); ok {
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

	// Hazard 1 (gt-x45us): a polecat that parked itself is not a crash,
	// whatever else the record says. The agent bead is read only here, only
	// to refuse, and an unreadable one is Unknown.
	agent, err := s.env.AgentRecord(rig, name)
	if err != nil {
		return unknown("agent state unreadable", err)
	}
	// Spawn grace (gt-6hby3): gt sling hooks the work bead before the tmux
	// session exists, so work hooked moments ago may be a polecat starting up
	// rather than a crash (issue #1752). Only the agent record can say that —
	// a running polecat keeps writing its work bead, so a recent write there
	// is no evidence of a dispatch — so the check sits after the record read.
	// The record's own write dates the spawn: a seat that says spawning but was
	// last written longer ago than the window never left that state, and the
	// dispatch it names is over (gt-i3ky2).
	if SpawnGrace(agent.State, work.UpdatedAt, now, s.o.SpawnGrace) &&
		SpawnGrace(agent.State, agent.UpdatedAt, now, s.o.SpawnGrace) {
		return skip(fmt.Sprintf("%s hooked %s ago and agent_state=%s; polecat may be spawning",
			work.ID, now.Sub(work.UpdatedAt).Round(time.Second), SpawningAgentState))
	}

	// recovery names the finished turn the restart below resumes — a polecat
	// that ended its turn without completing (gt done's DEFERRED exit), or one
	// that stopped on an escalation whose decision has since been made — and
	// "" is an ordinary crash. released names the escalation that released
	// such a seat, "" when none did.
	recovery, released := "", ""
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
		// than leaving the work dead until an operator notices (gt-ks62m).
		switch exit := strings.TrimSpace(agent.ExitType); {
		case strings.EqualFold(exit, ExitDeferred):
			recovery = ExitDeferred
		case strings.EqualFold(exit, ExitEscalated):
			// An ESCALATED exit is the operator's while its blocker stands:
			// the polecat stopped on purpose to raise a blocker, and a restart
			// would fight it. The blocker is the escalation that turn raised
			// for this bead, so the live question is what became of it: still
			// open, the seat is skipped as it always was; closed, the decision
			// was made and the seat has work waiting on it that nothing else
			// will pick up (gt-4hduq). A seat whose bead closed never arrives
			// here — the tick reaches it with no work and clears it there
			// (finishedExitEvidence).
			v, err := SeatEscalationHold(agent, rig, name, work.ID, func() ([]Escalation, error) {
				return s.env.PolecatEscalations(rig)
			})
			if err != nil {
				return unknown("escalations unreadable", err)
			}
			if v.Verdict != EscalationResolved {
				// Detail is "" for not-stopped. This branch already read the
				// record as the ESCALATED stop, so that answer should not come
				// back, but a held seat must still say why it was skipped.
				if v.Detail == "" {
					return skip("agent_state stuck: exit ESCALATED, escalation verdict " + string(v.Verdict))
				}
				return skip(v.Detail)
			}
			recovery, released = ExitEscalated, v.ID
		default:
			detail := "agent_state stuck: stopped on purpose"
			if exit != "" {
				detail += " (exit " + exit + ")"
			}
			return skip(detail)
		}
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

	if !deadseats.Confirmed(samples, s.o.DeadSamples) {
		f.Outcome = OutcomeWaiting
		f.Detail = fmt.Sprintf("%s with %s hooked; dead sample %d of %d", res.Reason, work.ID, samples, s.o.DeadSamples)
		return f, true
	}

	// A DEFERRED seat whose own last event is a done for this bead has ended
	// its turn, and the bead's close trails that event: a report-only run that
	// skipped the close leaves the bead hooked over a dead session, and a
	// restart here starts its agent again for nothing (gt-iwgbd). The event log
	// is read only here, for a dead session over open work already confirmed
	// across ticks, never for every seat. The wait is bounded: a done whose
	// bead never closes, and a DEFERRED exit with unpushed commits that the
	// town needs resumed (gt-ks62m), end in the same restart once the grace
	// passes. The dead samples are kept, so that restart goes ahead on the
	// first tick after it.
	if recovery == ExitDeferred && s.o.DoneAge != nil && s.o.DoneCloseGrace > 0 {
		if age, ok := s.o.DoneAge(rig, name, work.ID, now); ok && age < s.o.DoneCloseGrace {
			return skip(fmt.Sprintf("%s: the seat ran gt done for it %s ago and the bead has not closed yet",
				work.ID, age.Round(time.Second)))
		}
	}

	// The restart below raises a live session, so it spends a seat as a sling
	// does and waits for a free one rather than pushing the town over its cap.
	// Only the crash relaunch answers to the cap: the switch below is the
	// DEFERRED and ESCALATED turns, recoveries of work the town already holds,
	// which keep the handling their own beads gave them. The dead samples
	// accrued are untouched, so the restart goes ahead on the first later tick
	// that finds a free seat (gt-02tcp).
	if recovery == "" && s.o.SeatCap != nil {
		cap, err := s.o.SeatCap(rig, name)
		if err != nil {
			return unknown("seat cap unreadable", err)
		}
		if cap.Full() {
			if cap.Closed {
				if cap.ClosedDetail != "" {
					return skip(fmt.Sprintf("%s stays hooked: %s", work.ID, cap.ClosedDetail))
				}
				return skip(fmt.Sprintf("%s stays hooked: %s is 0, the seat is closed", work.ID, cap.Key))
			}
			return skip(fmt.Sprintf("%s stays hooked: %s full (%d/%d polecats live), waiting for a free seat",
				work.ID, cap.Key, cap.Live, cap.Limit))
		}
	}

	reason := fmt.Sprintf("patrol scan: %s with %s hooked (%d dead samples)", res.Reason, work.ID, samples)
	switch recovery {
	case ExitDeferred:
		// A restart here is a recovery, not a crash relaunch: name the turn
		// that ended and what the worktree holds, so the reason line reads as
		// the recovery it is (gt-ks62m).
		cleanup := strings.TrimSpace(agent.CleanupStatus)
		if cleanup == "" {
			cleanup = "unknown"
		}
		reason = fmt.Sprintf("patrol scan: %s with %s hooked after a DEFERRED exit (cleanup_status %s, %d dead samples)",
			res.Reason, work.ID, cleanup, samples)
	case ExitEscalated:
		// The release, in one line: the escalation that stopped the turn is
		// closed, so the seat is resumed on its preserved branch (gt-4hduq).
		reason = fmt.Sprintf("patrol scan: %s with %s hooked after an ESCALATED exit (escalation %s closed, %d dead samples)",
			res.Reason, work.ID, released, samples)
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

// releaseStalled frees a LIVE but stalled seat that has no work left to make
// progress on: it holds nothing, or a work bead that is closed or tombstoned.
// A live session sitting on a bead that already landed is a zombie — it never
// stops, and it keeps counting toward its seat cap — so the supervisor stops
// the session, and the seat is then retired to stop the way the dead-session
// path retires an idle one (gt-9p99w).
//
// It reports ok=false, leaving the seat exactly as it was, while the seat
// holds work that is not over — open, in_progress, submitted or held — because
// a long turn on live work is the usual explanation for a stall. The stall
// threshold needs no check here: liveness.Assess answers Stalled only once the
// quiet reaches the configured threshold, so a second reading of it could only
// disagree. A read that fails is Unknown: a release is never taken on an
// unanswered read.
func (s *Scanner) releaseStalled(rig, name string, res liveness.Result) (Finding, bool) {
	f := Finding{Kind: "seat", Subject: name}
	work, err := s.env.AssignedWork(rig, name)
	if err != nil {
		f.Outcome, f.Detail = OutcomeUnknown, "stalled seat's work unreadable: "+err.Error()
		return f, true
	}
	if work != nil && !work.IsTerminal() {
		return Finding{}, false
	}
	quiet := res.QuietFor.Round(time.Second)
	what := "no work"
	if work != nil {
		what = work.ID + " is " + strings.TrimSpace(work.Status)
	}
	// The session is stopped first: the supervisor refuses a held, parked or
	// e-stopped seat, and a refused stop is not a release — the record must
	// keep asking for the session the seat still has. Only a stop that went
	// through retires the record, and if that write then fails the next tick
	// retires the dead session the ordinary way.
	reason := fmt.Sprintf("patrol scan: stalled seat holding %s, quiet for %s", what, quiet)
	if err := s.env.Kill(rig, name, reason); err != nil {
		if s.o.IsRefusal(err) {
			f.Outcome, f.Detail = OutcomeRefused, err.Error()
			return f, true
		}
		f.Outcome, f.Detail = OutcomeFailed, err.Error()
		return f, true
	}
	if _, err := s.env.MarkIdle(rig, name); err != nil {
		f.Outcome, f.Detail = OutcomeFailed, "session stopped, retiring the stalled seat: "+err.Error()
		return f, true
	}
	f.Outcome, f.Detail = OutcomeReleased, what+"; session stopped after "+quiet.String()+" quiet"
	return f, true
}

// finishedExitEvidence returns the evidence that the seat the last turn left
// stuck after an ESCALATED or DEFERRED exit has nothing left to recover, or ""
// when it may not be cleared. Every condition is measured now, so a seat that
// fails any one of them is left exactly as the tick leaves it today — the
// operator's escalation, a turn a successor is expected to resume, or an
// unmeasured one. An error is a condition the tick could not measure, and
// nothing is cleared on an unknown.
//
// For ESCALATED a resolved escalation means the seat has no work: its work
// bead is closed. For DEFERRED the same holds of a report-only job, which
// closes its bead and then exits DEFERRED as mol-deep-review tells it
// (gt-oa6du). Either way the tick reaches this from the no-work path. A seat
// whose bead is still open is skipped or restarted further up, which is the
// "still the operator's" and "work to resume" case (gt-fn9e6.33).
func (s *Scanner) finishedExitEvidence(rig, name string, agent AgentRecord) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(agent.State), "stuck") {
		return "", nil
	}
	if exit := strings.TrimSpace(agent.ExitType); !strings.EqualFold(exit, ExitEscalated) && !strings.EqualFold(exit, ExitDeferred) {
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

// clearFinishedExit clears one seat's finished ESCALATED or DEFERRED record
// and reports the finding to log. ok is false when the seat is left exactly as
// the tick would have left it without this pass: nothing proven finished, or a
// record an earlier tick already cleared.
func (s *Scanner) clearFinishedExit(rig, name string, agent AgentRecord) (Finding, bool) {
	f := Finding{Kind: "seat", Subject: name}
	evidence, err := s.finishedExitEvidence(rig, name, agent)
	if err != nil {
		f.Outcome, f.Detail = OutcomeUnknown, "checking a finished exit: "+err.Error()
		return f, true
	}
	if evidence == "" {
		return Finding{}, false
	}
	exit := strings.ToUpper(strings.TrimSpace(agent.ExitType))
	changed, err := s.env.ClearStuckExit(rig, name, exit)
	if err != nil {
		f.Outcome, f.Detail = OutcomeFailed, "clearing a finished exit: "+err.Error()
		return f, true
	}
	if !changed {
		return Finding{}, false
	}
	why := "blocker resolved"
	if exit == ExitDeferred {
		why = "turn finished"
	}
	f.Outcome, f.Detail = OutcomeCleared, "exit "+exit+", "+why+" ("+evidence+"); record reset to idle"
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

const (
	// leakedWispMinAge is how old a wisp must be before the leaked-wisp pass
	// closes it: a seat that was just torn down may still be finishing.
	leakedWispMinAge = 24 * time.Hour
	// leakedWispMaxPerTick caps the closes one tick makes; the rest wait for
	// the next tick.
	leakedWispMaxPerTick = 20
)

// leakedWisps closes wisps held by a polecat seat that no longer exists.
// orphans reaches only the wisps bonded to a hooked or in_progress work bead,
// so a pinned wisp, or a step whose root is gone, stays open for good.
//
// A wisp is closed only when all of these hold:
//   - its assignee is <rig>/polecats/<name>, holderGone says the seat has no
//     directory and no session, and its intent record is not held;
//   - no hooked or in_progress work bead is assigned to that seat, because
//     molecule and recover own that work;
//   - it is older than leakedWispMinAge;
//   - its parent is absent, closed, a listed wisp of the same seat that this
//     rule closes too, or an open molecule root with no holder whose listed
//     wisps all pass the rule; the root is closed with them.
//
// A wisp is also left open while any listed wisp under it fails the rule,
// because CloseMolecule closes the whole tree under the wisp it is given.
// Every read that fails yields Unknown and no write.
//
// Unless a failed read or the cap on closes ends the tick first, the pass then
// closes the open molecule roots whose work bead is over (closedWorkRoots).
func (s *Scanner) leakedWisps(rig string, r *Report) {
	unknown := func(subject, why string) {
		r.Findings = append(r.Findings, Finding{Kind: "wisp", Subject: subject, Outcome: OutcomeUnknown, Detail: why})
	}
	prefix := rig + "/polecats/"
	wisps, err := s.env.LeakedWisps(rig)
	if err != nil {
		unknown(rig, "listing wisps: "+err.Error())
		return
	}
	closed := map[string]bool{}
	calls := 0 // failed closes count: a bd that errors is not retried 20 times a tick
	for _, w := range wisps {
		// The usual tick: no polecat holds a wisp, so the holder rules read
		// nothing.
		if strings.HasPrefix(w.Assignee, prefix) {
			if !s.heldWisps(rig, r, wisps, closed, &calls) {
				return
			}
			break
		}
	}
	s.closedWorkRoots(r, wisps, closed, &calls)
}

// heldWisps is the holder half of the leaked-wisp pass, run only when some
// polecat of the rig holds a wisp. It adds the molecules it closes to closed
// and counts its close calls in calls. It reports false when the pass is over
// for the tick: a failed read, or the cap on closes.
func (s *Scanner) heldWisps(rig string, r *Report, wisps []Wisp, closed map[string]bool, calls *int) bool {
	unknown := func(subject, why string) {
		r.Findings = append(r.Findings, Finding{Kind: "wisp", Subject: subject, Outcome: OutcomeUnknown, Detail: why})
	}
	prefix := rig + "/polecats/"
	active, err := s.env.ActiveWork(rig)
	if err != nil {
		unknown(rig, "listing active work: "+err.Error())
		return false
	}
	p := &wispPass{
		s: s, rig: rig, prefix: prefix, now: s.o.Now(), unknown: unknown,
		byID: map[string]Wisp{}, children: map[string][]string{},
		busy: map[string]bool{}, seats: map[string]bool{},
		eligible: map[string]bool{}, visiting: map[string]bool{},
		tree: map[string]bool{}, parents: map[string]bool{},
	}
	for _, a := range active {
		if strings.HasPrefix(a.Assignee, prefix) {
			p.busy[strings.TrimPrefix(a.Assignee, prefix)] = true
		}
	}
	for _, w := range wisps {
		if w.ID == "" || w.IsClosed() {
			continue
		}
		p.byID[w.ID] = w
		if w.Parent != "" {
			p.children[w.Parent] = append(p.children[w.Parent], w.ID)
		}
	}

	// Roots before the steps under them: closing a root closes its steps, so
	// a step whose ancestor was just closed is not closed a second time.
	ids := make([]string, 0, len(p.byID))
	for id := range p.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if di, dj := p.depth(ids[i]), p.depth(ids[j]); di != dj {
			return di < dj
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		w := p.byID[id]
		if !p.closable(id) || p.coveredBy(w, closed) {
			continue
		}
		// A wisp under an unassigned root goes with the root or not at all:
		// the root is what a leaked tree is closed from, and a root left
		// open (a live or a young wisp under it) leaves its steps alone.
		if p.unassignedAncestor(id) != "" {
			continue
		}
		if *calls >= leakedWispMaxPerTick {
			r.Findings = append(r.Findings, capFinding(id))
			return false
		}
		name := strings.TrimPrefix(w.Assignee, prefix)
		reason := fmt.Sprintf("Leaked wisp: holder %s no longer exists (patrol scan)", name)
		if w.Assignee == "" {
			reason = "Leaked wisp tree: molecule root has no holder (patrol scan)"
		}
		*calls++
		n, err := s.env.CloseMolecule(id, reason)
		f := Finding{Kind: "wisp", Subject: id}
		if err != nil {
			f.Outcome, f.Detail = OutcomeFailed, fmt.Sprintf("closed %d, then: %v", n, err)
			r.Findings = append(r.Findings, f)
			continue
		}
		closed[id] = true
		detail := fmt.Sprintf("%d wisp(s) closed; %s, held by %s", n, w.Status, name)
		if w.Assignee == "" {
			detail = fmt.Sprintf("%d wisp(s) closed; %s molecule root, no holder", n, w.Status)
		}
		f.Outcome, f.Detail = OutcomeClosed, detail
		r.Findings = append(r.Findings, f)
	}
	return true
}

// capFinding is the report line for a wisp left to the next tick because the
// tick has made leakedWispMaxPerTick close calls.
func capFinding(id string) Finding {
	return Finding{Kind: "wisp", Subject: id, Outcome: OutcomeSkipped,
		Detail: fmt.Sprintf("closed %d this tick; the rest wait for the next", leakedWispMaxPerTick)}
}

// closedWorkRoots closes open molecule roots whose work bead is over, held by
// a gone polecat or by nobody. The holder rules reach only wisps a gone
// polecat holds, so a root that leaked by some other path (a work bead closed
// by hand, a landing that outlived its seat) stays open for good; the blocks
// edge from the root to its work bead is the one record that does not depend
// on the path.
//
// A root is closed only when all of these hold:
//   - it is a listed, open wisp with no parent, older than leakedWispMinAge,
//     that the holder rules did not already close this tick;
//   - no polecat that is still alive holds it: that polecat may be finishing
//     its steps;
//   - it has at least one blocks edge to a work bead, and every such bead
//     reads closed or tombstone. An edge to another wisp is not a work bead.
//
// A root with no blocks edge (a patrol root), a young root, a root a live
// polecat holds, and a root whose work bead is anything but over are left
// alone. A root, an edge, a holder or a work bead that cannot be read yields
// Unknown and no write. The reads are one RootWork per candidate root and a
// holder check for a candidate a polecat holds, so a tick with no candidate
// makes none.
func (s *Scanner) closedWorkRoots(r *Report, wisps []Wisp, closed map[string]bool, calls *int) {
	now := s.o.Now()
	for _, w := range wisps {
		if w.ID == "" || w.Parent != "" || closed[w.ID] || !strings.EqualFold(strings.TrimSpace(w.Status), "open") {
			continue
		}
		unknown := func(why string) {
			r.Findings = append(r.Findings, Finding{Kind: "wisp", Subject: w.ID, Outcome: OutcomeUnknown, Detail: why})
		}
		if w.CreatedAt.IsZero() {
			unknown("created_at unreadable")
			continue
		}
		if now.Sub(w.CreatedAt) <= leakedWispMinAge {
			continue
		}
		if rig, name, held := strings.Cut(w.Assignee, "/polecats/"); held {
			gone, why := s.holderGone(rig, name)
			if why != "" {
				unknown("holder " + name + ": " + why)
				continue
			}
			if !gone {
				continue
			}
		}
		refs, err := s.env.RootWork(w.ID)
		if err != nil {
			unknown("work bead edge unreadable: " + err.Error())
			continue
		}
		var work []WorkRef
		for _, ref := range refs {
			if !strings.Contains(ref.ID, "-wisp-") {
				work = append(work, ref)
			}
		}
		if len(work) == 0 {
			continue
		}
		over := true
		for _, ref := range work {
			if strings.TrimSpace(ref.Status) == "" {
				unknown("status of work bead " + ref.ID + " unreadable")
				over = false
				break
			}
			if !(Work{Status: ref.Status}).IsTerminal() {
				over = false
				break
			}
		}
		if !over {
			continue
		}
		if *calls >= leakedWispMaxPerTick {
			r.Findings = append(r.Findings, capFinding(w.ID))
			return
		}
		*calls++
		n, err := s.env.CloseMolecule(w.ID, "Leaked wisp tree: work bead "+work[0].ID+" is "+strings.TrimSpace(work[0].Status)+" (patrol scan)")
		f := Finding{Kind: "wisp", Subject: w.ID}
		if err != nil {
			f.Outcome, f.Detail = OutcomeFailed, fmt.Sprintf("closed %d, then: %v", n, err)
		} else {
			closed[w.ID] = true
			f.Outcome, f.Detail = OutcomeClosed, fmt.Sprintf("%d wisp(s) closed; molecule root whose work bead %s is %s", n, work[0].ID, strings.TrimSpace(work[0].Status))
		}
		r.Findings = append(r.Findings, f)
	}
}

// wispPass is the state of one leakedWisps run: the listing, and the answers
// each rule has already given, so a seat or a parent is read once.
type wispPass struct {
	s       *Scanner
	rig     string
	prefix  string
	now     time.Time
	unknown func(subject, why string)

	byID     map[string]Wisp
	children map[string][]string // listed wisps under each parent ID
	busy     map[string]bool     // seats with a hooked or in_progress work bead
	seats    map[string]bool     // seat -> its wisps may be closed
	eligible map[string]bool     // wisp -> the rule holds for it alone
	visiting map[string]bool     // wisps on the parent chain being walked
	tree     map[string]bool     // wisp -> eligible, as is every listed wisp under it
	parents  map[string]bool     // unlisted parent -> absent or closed
}

// seatGone reports whether the seat's wisps may be closed: it holds no work
// bead, has neither directory nor session, and is not parked or frozen. A
// read that fails is reported once per seat.
func (p *wispPass) seatGone(name string) bool {
	if ok, seen := p.seats[name]; seen {
		return ok
	}
	ok := p.readSeat(name)
	p.seats[name] = ok
	return ok
}

func (p *wispPass) readSeat(name string) bool {
	if name == "" || strings.Contains(name, "/") || p.busy[name] {
		return false
	}
	gone, why := p.s.holderGone(p.rig, name)
	if why != "" {
		p.unknown(name, "wisps held by "+name+": "+why)
		return false
	}
	if !gone {
		return false
	}
	rec, err := p.s.env.Intent(p.rig, name)
	if err != nil {
		p.unknown(name, "wisps held by "+name+": holder record unreadable: "+err.Error())
		return false
	}
	return !rec.Held() // parked or frozen: the operator's, silently
}

// ok reports whether the rule holds for the wisp alone.
func (p *wispPass) ok(id string) bool {
	if v, seen := p.eligible[id]; seen {
		return v
	}
	if p.visiting[id] {
		return false // a parent cycle has no root to close from
	}
	p.visiting[id] = true
	defer delete(p.visiting, id)
	v := p.check(p.byID[id])
	p.eligible[id] = v
	return v
}

func (p *wispPass) check(w Wisp) bool {
	if w.Assignee == "" {
		return p.orphanRoot(w)
	}
	if !strings.HasPrefix(w.Assignee, p.prefix) || !p.seatGone(strings.TrimPrefix(w.Assignee, p.prefix)) {
		return false
	}
	if w.CreatedAt.IsZero() {
		p.unknown(w.ID, "created_at unreadable")
		return false
	}
	if p.now.Sub(w.CreatedAt) <= leakedWispMinAge {
		return false
	}
	return p.parentOK(w)
}

// orphanRoot reports whether w is an open molecule root and nothing holds it.
// The steps of a molecule whose root was never assigned hang there, so the
// root is the wisp that carries the leak; closable adds the condition that
// every listed wisp under it passes the rule, because closing the root
// closes that whole tree.
func (p *wispPass) orphanRoot(w Wisp) bool {
	if w.Parent != "" || !strings.EqualFold(strings.TrimSpace(w.Status), "open") {
		return false
	}
	if w.CreatedAt.IsZero() {
		p.unknown(w.ID, "created_at unreadable")
		return false
	}
	return p.now.Sub(w.CreatedAt) > leakedWispMinAge
}

// parentOK reports whether the wisp's parent is absent, closed, a listed
// wisp of the same seat that the rule closes too, or a listed wisp with no
// assignee that the rule closes too (an unassigned root: the loop never
// closes a step under one on its own).
func (p *wispPass) parentOK(w Wisp) bool {
	if w.Parent == "" {
		return true
	}
	if parent, listed := p.byID[w.Parent]; listed {
		return (parent.Assignee == w.Assignee || parent.Assignee == "") && p.ok(parent.ID)
	}
	if v, seen := p.parents[w.Parent]; seen {
		return v
	}
	status, err := p.s.env.WispStatus(w.Parent)
	if err != nil {
		p.unknown(w.ID, "parent "+w.Parent+" unreadable: "+err.Error())
		return false // not cached: the next wisp under it asks again
	}
	v := status == "" || strings.EqualFold(strings.TrimSpace(status), "closed")
	p.parents[w.Parent] = v
	return v
}

// closable reports whether the wisp passes the rule and so does every listed
// wisp under it.
func (p *wispPass) closable(id string) bool {
	if v, seen := p.tree[id]; seen {
		return v
	}
	v := p.ok(id)
	for _, c := range p.children[id] {
		if !v {
			break
		}
		v = p.closable(c)
	}
	p.tree[id] = v
	return v
}

// depth counts the listed wisps above id, stopping at a cycle.
func (p *wispPass) depth(id string) int {
	seen := map[string]bool{id: true}
	n := 0
	for {
		parent, listed := p.byID[p.byID[id].Parent]
		if !listed || seen[parent.ID] {
			return n
		}
		seen[parent.ID] = true
		id = parent.ID
		n++
	}
}

// unassignedAncestor returns the nearest listed ancestor of id that has no
// assignee, "" when the chain above id has none. A wisp under one is closed
// with that ancestor, which the loop reaches first: closing a step on its
// own would clear the very wisp the next tick reads the tree from.
func (p *wispPass) unassignedAncestor(id string) string {
	seen := map[string]bool{id: true}
	for {
		parent, listed := p.byID[p.byID[id].Parent]
		if !listed || seen[parent.ID] {
			return ""
		}
		if parent.Assignee == "" {
			return parent.ID
		}
		seen[parent.ID] = true
		id = parent.ID
	}
}

// coveredBy reports whether a listed ancestor of w was closed this tick.
func (p *wispPass) coveredBy(w Wisp, closed map[string]bool) bool {
	seen := map[string]bool{w.ID: true}
	for {
		parent, listed := p.byID[w.Parent]
		if !listed || seen[parent.ID] {
			return false
		}
		if closed[parent.ID] {
			return true
		}
		seen[parent.ID] = true
		w = parent
	}
}
