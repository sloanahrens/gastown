package landworker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// Beads is what the worker reads and writes work beads through (ADR 0001:
// bd only). The daemon passes a beads client wrapped in RetryBeads.
type Beads interface {
	land.Beads
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Comments(id string) ([]beads.Comment, error)
	AddComment(id, text string) error
}

// Remote is the worker's hand on the two repositories it lands through: the
// rig's own, where the leftover local polecat branches live, and origin,
// whose tip it lands and whose polecat branches it reaps.
type Remote interface {
	// BranchTip returns origin's tip of branch, or "" when origin has no
	// such branch.
	BranchTip(branch string) (string, error)
	// Contains fetches target and reports whether commit is reachable from
	// origin/<target>.
	Contains(target, commit string) (bool, error)
	// ListRemoteRefs returns origin's refs whose names start with prefix,
	// each with the commit it points at.
	ListRemoteRefs(prefix string) ([]RemoteRef, error)
	// BranchLandedOn fetches origin's branch and target, then reports whether
	// branch's tip is already on target: reachable from it, or every one of
	// its own commits present there by patch-id (a queue that rebases
	// preserves content, not SHAs). A branch the repository still lacks after
	// the fetch is not proven, never an error.
	BranchLandedOn(target, branch, commit string) (bool, error)
	// CommitTime returns commit's committer time, which ages a leftover
	// branch.
	CommitTime(commit string) (time.Time, error)
	// DeleteRemoteBranchIfAt deletes branch on origin only while it still
	// points at expectedHash.
	DeleteRemoteBranchIfAt(branch, expectedHash string) error
	// LocalBranchTip returns the rig repository's own tip of branch, or ""
	// when it holds no such branch.
	LocalBranchTip(branch string) (string, error)
	// DeleteLocalBranch deletes branch from the rig's own repository — the
	// local half of reaping it on origin — and reports whether a branch was
	// there to delete, an absent one being (false, nil). The delete is pinned
	// at expectedHash, the commit the caller proved the branch to be: a
	// branch that is not at that commit, or that moves onto another one first,
	// is kept and the error says so, so nothing a branch holds past the
	// proven commit is dropped. The repository's HEAD branch and any branch
	// checked out in a worktree are never deleted, and an error says so.
	DeleteLocalBranch(branch, expectedHash string) (bool, error)
}

// FirstSight records a submission the first time the worker sees it, before
// the worker lands anything. *land.SubmissionRecorder is the production one.
type FirstSight interface {
	Record(w land.Work) (bool, error)
}

var _ FirstSight = (*land.SubmissionRecorder)(nil)

// Lander lands one piece of work; *land.Lander is the production one.
type Lander interface {
	Land(ctx context.Context, w land.Work) (land.Result, error)
}

// finisher finishes a deferred landing's record; *land.Lander implements it (gt-k78e8j).
type finisher interface {
	FinishLanding(w land.Work, res land.Result) error
}

// Landings is the rig's landings file, read to repair incomplete records.
type Landings interface {
	LatestForBead(beadID string) (land.LandingRecord, bool, error)
	Recent(n int) ([]land.LandingRecord, error)
}

var (
	_ Lander   = (*land.Lander)(nil)
	_ Landings = (*land.LandingsFile)(nil)
)

// ReworkComment is the comment a rework rejection leaves on the work bead,
// after the rejection's own MERGE REJECTION block.
const ReworkComment = "rebase onto main and re-gate"

// LabelLandingRetry is the operator's label for starting one parked bead on a
// new run without a daemon restart: the pass removes it, forgets the bead's
// park and run, and lands the bead on that same pass (gt-46bh2y).
const LabelLandingRetry = "gt:landing-retry"

// Defaults for the worker's timing.
const (
	DefaultLandTimeout = 90 * time.Minute
	// recentRepairWindow is how many of the landings file's last records the
	// first pass checks for a landed bead whose record is incomplete.
	recentRepairWindow = 50
	// infraParkWait is how long a bead that failed on infrastructure stays
	// parked: no retry on a timer, a failure fails loud and stops (operator
	// 2026-11-20). It is the life of the process, in effect: the park is in
	// memory, so a daemon restart, which is how a fix rolls out, ends it.
	infraParkWait = 10 * 365 * 24 * time.Hour
	// humanWaitBackoff spaces re-checks of a bead only a human can unblock
	// (branch missing, no landing request): cheap to re-check, noisy to log.
	humanWaitBackoff = 15 * time.Minute
	readBackBackoff  = 30 * time.Minute
	// rejectRecordBackoff spaces re-landing a bead whose rejection could not
	// be fully written, so a bd outage does not become a rejection loop.
	rejectRecordBackoff = 10 * time.Minute
	// infraAnnounceAfter is how many consecutive infrastructure failures on
	// one bead earn it a comment saying why it is not landing.
	infraAnnounceAfter = 1
	// DefaultCISilenceEscalateAfter is the run of candidate-gate silences on
	// one bead that earns an escalation: the first, because a silence is an
	// infrastructure failure and parks the bead like any other, so only the
	// escalation says out loud that a runner is down or hung (gt-fn9e6.5).
	DefaultCISilenceEscalateAfter = 1
	// DefaultFailingEscalateAfter is the run of failures at any landing stage
	// on one bead that earns an escalation: a landing failing repeatedly is
	// otherwise visible only in daemon.log (gt-fn9e6.44).
	DefaultFailingEscalateAfter = 1
	// maxErrorLine bounds the error text a failing-landing record and a health detail
	// carry, so a failure whose message is a wall of output stays one phrase.
	maxErrorLine = 160
	// readyListCeiling bounds how long a submission can wait when a pass skipped
	// the list: a label written by hand with bd touches no stamp, so nothing
	// else guarantees a list sooner (gt-flo7a).
	readyListCeiling = 3 * time.Minute
)

// Worker lands one rig's ready work, serially.
type Worker struct {
	Rig string
	// TownRoot is where the rig's ready stamp lives (land.ReadyStamp), which
	// every writer of the ready label touches. "" turns the idle skip off: the
	// pass lists every time.
	TownRoot string
	Beads    Beads
	Remote   Remote
	Lander   Lander
	Landings Landings
	// Records, when set, is the queue of landing records an earlier pass could
	// not write; each pass writes them once bd answers (gt-qa5ge8). nil skips it.
	Records RecordQueue
	// Books, when set, is the queue a deferred bookkeeping job stores work on
	// with Later; each pass runs that work at landReady's start, on this
	// goroutine (gt-rx2kl7). nil skips it.
	Books *BookQueue
	// noBatchSaid is set once the worker has said its beads client cannot batch
	// reads, so the pass that repeats says so once, not on every pass.
	noBatchSaid bool
	// ClearIntent returns the author polecat's seat from submitted to stop
	// once the worker has finished with its bead. nil skips it.
	ClearIntent func(w land.Work) error
	// AfterLanded runs once w's landing is on the target and its record is
	// complete: after a new landing that finished its record, and after the
	// repair pass that finishes a record left unfinished. Not after a rejection
	// or a failure, nor after a landing whose record is still unfinished. The
	// daemon removes a sandbox seat's host directories here (gt-sefzt). nil
	// skips it.
	AfterLanded func(w land.Work)
	// AgentBeadID names the agent bead a polecat's seat wrote its agent on,
	// which the worker reads to record the agent each landing ran on
	// (gt-mukgk.6). The prefix is the town's, so the caller supplies the id;
	// nil, or an id the store has no bead for, leaves the landing without an
	// agent rather than guessing one.
	AgentBeadID func(polecat string) string
	// PostLand, when set, runs the rig's post-landing command after each
	// new landing (never after a record repair).
	PostLand PostLandTrigger
	// HumanActor is the human's recorded identity, the town's overseer name,
	// which the daemon reads from the town config. A submission whose actor is
	// this identity is refused: the human must not be the name the dashboard
	// and the review data read as the submitter (gt-2ge3z). A worker with no
	// identity refuses every submission rather than landing one it could not
	// check.
	HumanActor string
	// RefreshAuthorSeat, when set, is called after a landing, right after the
	// bead's origin branches are reaped, once per polecat that authored one
	// (the landing request's worker included when no branch names it): a
	// Forgejo landing moves the default branch on the remote, and a seat that
	// does not fetch it reads landed work as local-only (gt-fn9e6.55). Best
	// effort — a missing worktree or a failed fetch is logged once and never
	// fails, delays or reorders the landing. nil skips it.
	RefreshAuthorSeat func(polecat string) error
	// Reverts finishes the red-main owner's reverts (*RedMain): told of each
	// new landing and each rework rejection. nil skips it.
	Reverts RevertHooks
	// FirstSight records each waiting submission the first time the worker
	// sees it, so the submission battery still asks about one that lands
	// within its interval and whose branch is then pruned. nil skips it.
	FirstSight FirstSight
	// Pool is the review pool a later bead reads to review submissions ahead
	// of the landing; nil, the only value today, leaves the worker exactly as
	// it was before the field existed (gt-8vfrwi).
	Pool *ReviewPool
	// Stored is where a later bead asks which ready beads have a stored
	// rejecting review, so the worker can reuse a verdict instead of running
	// om inline; nil, the only value today, leaves the worker exactly as it
	// was before the field existed (gt-5q8ayw).
	Stored AheadHints
	// WatchTarget is the branch the worker watches for commits that reached
	// it without a landing (a direct push); each pass that finds one runs
	// the post-landing command for it. The leftover sweep also reads it as
	// the branch work must be on before the sweep deletes anything. ""
	// turns both off. MainState seeds the tip it last saw across restarts.
	WatchTarget string
	MainState   MainStateStore
	// LandTimeout bounds one landing (gate included); 0 means
	// DefaultLandTimeout.
	LandTimeout time.Duration
	// SweepInterval is how often the worker sweeps origin for polecat
	// branches a landing, a restart or an abandoned bead left behind
	// (sweepLeftoverBranches); 0 means DefaultSweepInterval. The first pass
	// of a run sweeps whatever the interval.
	SweepInterval time.Duration
	// Draining, when set and true, stops the worker from starting anything
	// new: the landing in flight finishes, no further ready bead is claimed
	// and the watch and the sweep are skipped. The daemon sets it while any
	// job holds new landing passes off, an upgrade restart or the om rebuild,
	// so that job finds an idle moment (gt-nxvpe, gt-0hsf6).
	Draining func() bool
	// Active is told which bead is being landed (and "" when it ends), so
	// the daemon can say what a pending restart is waiting for.
	Active func(beadID string)
	// Escalate, when set, raises a rejection left for a human (gt:needs-overseer)
	// to the operator, so it is not waiting unseen on a label: an om review
	// that returned no verdict, a stage timeout, a policy refusal (gt-is0ep).
	// It also raises a bead whose candidate gate reported nothing
	// CISilenceEscalateAfter times in a row. lane keys the escalation per lane;
	// "" names none (gt-il3out). It runs in its own goroutine, which recovers
	// and logs a panic; the pass does not wait.
	Escalate func(beadID, lane, message string)
	// EscalationOpen, when set, answers whether beadID's lane already has an
	// open escalation, so a lane whose capped-loop escalation was closed
	// raises one on a new rejection and none while it is open (gt-fjzstx). A
	// check that errors, or no seam at all, raises it again and says so at
	// every check: a silenced alarm is the defect (gt-il3out).
	EscalationOpen func(beadID, lane string) (bool, error)
	// CISilenceEscalateAfter is how many consecutive candidate-gate silences
	// on one bead raise a single escalation; 0 means
	// DefaultCISilenceEscalateAfter.
	CISilenceEscalateAfter int
	// FailingEscalate, when set, raises the escalation for a bead whose
	// landing has failed FailingEscalateAfter times in a row at any stage. It
	// is keyed per rig and bead, so a rerun upserts onto one escalation rather
	// than minting another (gt-fn9e6.44). nil leaves the rule off.
	FailingEscalate func(beadID, message string)
	// FailingClear, when set, closes that escalation: the bead landed or was
	// rejected, so its run of failures is over.
	FailingClear func(beadID string)
	// FailingEscalateAfter is how many consecutive failures at any stage on
	// one bead raise a single escalation; 0 means
	// DefaultFailingEscalateAfter.
	FailingEscalateAfter int
	// Backoff, when set, receives the rig's failing landings after each pass,
	// which is what the dashboard and town health read them from. nil leaves
	// them in the log alone.
	Backoff BackoffWriter
	// Queue, when set, receives this rig's ready-to-land count after each
	// pass's list of the board, which is where a metrics sample reads the
	// queue from instead of listing it again (gt-hw6ih). A list that failed
	// publishes nothing: a count no pass read is not an empty queue. nil
	// skips it.
	Queue *LandingQueueView
	Logf  func(format string, args ...any)
	Now   func() time.Time

	state map[string]*beadState
	// memo holds the beads this run judged already, so a held bead whose row
	// has not moved is not read again before its hold is due (gt-r9pj9).
	memo *outcomeMemos
	// escWG lets a test wait for Escalate goroutines.
	escWG sync.WaitGroup
	// pendingRepair holds landings whose record was left incomplete; the
	// next pass finishes them before landing anything new.
	pendingRepair map[string]land.Work
	startupDone   bool
	// revertChecked is whether this run's check for a revert completion a
	// restart dropped has answered: either the call was made or none is owed.
	// A check that could not read leaves it false, and the next pass asks
	// again (gt-qxm7y3).
	revertChecked bool
	// lastSeen is the WatchTarget tip the worker last saw or landed.
	lastSeen string
	// sweptAt is when the leftover-branch sweep last ran; zero on the run's
	// first pass, which sweeps.
	sweptAt time.Time
	// sweepLogged are the leftover branches the sweep has already reported
	// this run: one line per branch, not one per hour.
	sweepLogged map[string]bool
	// firstSightFailing is true while the last first-sight write failed, so
	// a failing log is reported once, not once per pass.
	firstSightFailing bool
	// lastListAt is when the pass last listed the ready label, taken before the
	// call so a label written during it is not missed; lastListEmpty is whether
	// that list held nothing, and lastListCount is what it held, the count a
	// skipped pass republishes (gt-flo7a).
	lastListAt time.Time
	// lastPollEnd is when the pending post-land verdicts were last read, whether the
	// read finished or ran out of its budget.
	lastPollEnd   time.Time
	lastListEmpty bool
	lastListCount int
	// stampUnreadable is true while the last ready-stamp read failed, so the
	// fallback to listing the board is logged once for the run of failures,
	// not once per idle pass (gt-5obd1).
	stampUnreadable bool
}

// RevertHooks is the red-main owner's side of an automatic revert.
type RevertHooks interface {
	// RevertLanded is called after every new landing that is not deferred; a
	// deferred landing's bookkeeping job calls it at the next pass instead, and
	// a run that starts before that pass asks RevertOwed and calls it there.
	RevertLanded(ctx context.Context, work land.Work, res land.Result)
	// IsRevert reports whether beadID is one of this owner's reverts. It reads
	// only, so a deferred landing's bookkeeping job may call it on the queue
	// goroutine, where RevertLanded must not run.
	IsRevert(beadID string) bool
	// RevertOwed returns the revert whose landing is recorded while it is
	// still in flight, and false for none: the call a restart dropped between
	// finishing the record and telling the owner (gt-qxm7y3). An unreadable
	// state or landings file is an error, never a false.
	RevertOwed() (land.Work, land.Result, bool, error)
	// RevertRejected is called on every rework rejection; true means the
	// work was a revert and has been dealt with, so no rework comment.
	RevertRejected(work land.Work, rej *land.Rejection) bool
}

var _ RevertHooks = (*RedMain)(nil)

type beadState struct {
	failures  int
	until     time.Time
	announced string
	// stage and lastErr are the last infrastructure failure's stage and error
	// line, the facts the backoff snapshot carries to the dashboard and the
	// health field (gt-fn9e6.44).
	stage   string
	lastErr string
	// ciSilences counts consecutive candidate-gate silences; every outcome but
	// an infrastructure failure, and a pass that consumed no stored verdict,
	// resets it.
	ciSilences int
	// escalated marks that this run of failures has raised an escalation
	// through any rule, so a lint timeout or a CI silence that escalated at
	// the same count does not escalate again through the general rule.
	escalated bool
	// failingEscalated marks that this run raised the general failing-landing
	// escalation, the one only a landing or a rejection closes.
	failingEscalated bool
	// installNow is whether the bead carried LabelInstallNow when this pass
	// loaded it; only a landing of it turns the flag into a report field.
	installNow bool
	// annotated are the annotation keys this run has already posted on the
	// bead: an outcome the bead already carries is not written again, and the
	// keys outlive the failure runs they were posted in, so alternating
	// outcomes do not take turns re-posting each other (gt-r9pj9).
	annotated map[string]bool
	// wrote is whether this pass commented on the bead, which moves its row.
	// The pass consumes it when it records the bead's outcome (gt-r9pj9).
	wrote bool
	// parked is whether an infrastructure failure holds the bead until its
	// park runs out. The park lives on this state, not on the bead's row, so
	// no write to the bead lifts it; it ends when the park runs out or the
	// bead's run ends (gt-5phoiv).
	parked bool
}

// BackoffWriter records a rig's failing landings for readers outside the
// worker; *land.BackoffFile is the production one.
type BackoffWriter interface {
	Write(land.BackoffState) error
}

var _ BackoffWriter = (*land.BackoffFile)(nil)

// Report counts one pass's outcomes.
type Report struct {
	Landed, Repaired, Rejected, Skipped, Failed int
	// InstallRequested is set when a bead landed in this pass carries
	// LabelInstallNow: the operator wants gt installed as soon as that
	// landing is on main, not at the next quiet point (gt-3qmv4.2).
	InstallRequested bool
}

func (r Report) String() string {
	return fmt.Sprintf("%d landed, %d repaired, %d rejected, %d skipped, %d failed", r.Landed, r.Repaired, r.Rejected, r.Skipped, r.Failed)
}

func (w *Worker) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf("landing_worker: "+w.Rig+": "+format, args...)
	}
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) bead(id string) *beadState {
	if w.state == nil {
		w.state = map[string]*beadState{}
	}
	s := w.state[id]
	if s == nil {
		s = &beadState{}
		w.state[id] = s
	}
	return s
}

// parked reports whether an infrastructure failure holds id until a time that
// has not yet passed. The pass skips such a bead without reading it, and no
// write to the bead lifts the hold (gt-5phoiv).
func (w *Worker) parked(id string) bool {
	st := w.state[id]
	return st != nil && st.parked && w.now().Before(st.until)
}

func (w *Worker) outcomeMemo() *outcomeMemos {
	if w.memo == nil {
		w.memo = newOutcomeMemos(w.now)
	}
	return w.memo
}

// rememberOutcome records what the pass left the bead on: a bead held until a
// time is not read again before then, and one the pass landed, or that carries
// no hold, is read afresh on the next pass (gt-r9pj9).
func (w *Worker) rememberOutcome(issue *beads.Issue) {
	st := w.state[issue.ID]
	wrote := st != nil && st.wrote
	if st != nil {
		st.wrote = false
	}
	if st == nil || !st.until.After(w.now()) {
		w.outcomeMemo().forget(issue.ID)
		return
	}
	w.outcomeMemo().record(issue.ID, w.outcomeRow(issue, wrote), st.until)
}

// outcomeRow is the bead's list row as the pass leaves it. A pass that
// commented on the bead moved the row itself, and recording the row it listed
// would make the next pass read the bead to learn of the worker's own write: a
// Show, a Children, a Comments and a whole landing attempt to find nothing new.
// One Show in the pass that wrote instead. A failed read leaves the row unknown,
// so the memo holds nothing and the next pass reads the bead: a row nothing can
// prove is never skipped (gt-r9pj9).
func (w *Worker) outcomeRow(issue *beads.Issue, wrote bool) string {
	if !wrote {
		return issue.UpdatedAt
	}
	fresh, err := w.Beads.Show(issue.ID)
	if err != nil || fresh == nil {
		w.logf("%s: asking bd where the pass's comment left the row (%v); the next pass reads the bead again", issue.ID, err)
		return ""
	}
	return fresh.UpdatedAt
}

// Pass lands every ready bead once, highest priority first and, within a
// priority, oldest submission first, then checks WatchTarget for a direct
// push, then sweeps origin's leftover polecat branches, and returns what
// happened. It stops early only when ctx is done.
func (w *Worker) Pass(ctx context.Context) Report {
	// First, so a bd that has answered since the record was queued takes it
	// before this pass claims anything (gt-qa5ge8).
	w.drainRecordQueue()
	rep := w.landReady(ctx)
	if !w.draining() {
		w.watchTarget(ctx)
		// The pending post-land verdicts are read here, once per pass: status
		// mode never waits on CI, so a verdict still open holds no landing,
		// and the reads share one PostLandStatusPassBudget (each also bounded
		// by PostLandStatusReadTimeout), so a stalled remote cannot hold the
		// pass either (gt-gg9hv.7.1).
		if w.PostLand != nil {
			w.pollPending(ctx)
		}
		// Last, so a slow or failing sweep never holds up a landing this
		// pass.
		w.sweepIfDue(ctx)
	}
	// After the pass, so every next-try time in the snapshot is the one the
	// following pass will act on.
	w.writeBackoff()
	return rep
}

// pollPending reads the pending post-land verdicts and records when the read
// ended, however it ended: a read cut off by its budget counts, so a stalled
// remote is not tried again before pollDue says the interval has passed.
func (w *Worker) pollPending(ctx context.Context) {
	w.PostLand.PollPending(ctx)
	w.lastPollEnd = w.now()
}

// pollDue reports whether the pending verdicts are due before a landing: the
// last poll ended at least PostLandStatusPollInterval ago, or none ran yet.
func (w *Worker) pollDue() bool {
	return w.lastPollEnd.IsZero() || w.now().Sub(w.lastPollEnd) >= PostLandStatusPollInterval
}

// writeBackoff records the beads whose landing is failing and waiting for a
// retry. A bead drops out once it lands, is rejected, or its retry comes due,
// so the file holds what the worker is actually backing off from and nothing
// it has moved past (gt-fn9e6.44).
func (w *Worker) writeBackoff() {
	if w.Backoff == nil {
		return
	}
	now := w.now()
	st := land.BackoffState{Rig: w.Rig, At: now}
	for id, s := range w.state {
		if s.failures == 0 || !s.until.After(now) {
			continue
		}
		st.Beads = append(st.Beads, land.BackoffRecord{
			BeadID: id, Stage: s.stage, Failures: s.failures, NextTry: s.until, Error: s.lastErr,
		})
	}
	sort.Slice(st.Beads, func(i, j int) bool { return st.Beads[i].BeadID < st.Beads[j].BeadID })
	if err := w.Backoff.Write(st); err != nil {
		w.logf("writing the backoff snapshot: %v", err)
	}
}

func (w *Worker) draining() bool {
	return w.Draining != nil && w.Draining()
}

// booksHold reports whether beadID's deferred bookkeeping is still queued or
// running: the bead still reads as ready until that job writes its record and
// removes the label, so landing or repairing it again would race the write
// (gt-ppcl1g). False when no queue is set.
func (w *Worker) booksHold(beadID string) bool {
	return w.Books != nil && w.Books.Holds(beadID)
}

func (w *Worker) landReady(ctx context.Context) Report {
	var rep Report
	// First, so the work a job stored touches this goroutine's state before the
	// pass reads any of it (gt-rx2kl7).
	if w.Books != nil {
		w.Books.RunLater()
	}
	if w.pendingRepair == nil {
		w.pendingRepair = map[string]land.Work{}
	}
	if !w.startupDone {
		w.startupDone = true
		w.queueRecentRepairs()
	}
	// Before anything lands: a revert whose landing a restart left incomplete
	// is told first, so its culprit is reopened before a new landing is counted.
	w.recoverRevertLanded(ctx)
	for id, work := range w.pendingRepair {
		if ctx.Err() != nil {
			return rep
		}
		if w.booksHold(id) {
			rep.Skipped++
			continue
		}
		if w.now().Before(w.bead(id).until) {
			rep.Skipped++
			continue
		}
		w.logf("%s: finishing the record of an earlier landing", id)
		w.landOne(ctx, work, &rep)
	}

	if count, skip := w.skipReadyList(); skip {
		// The count the queue reads until this rig's next pass (gt-hw6ih). A
		// skipped pass is not a queue that emptied, so it republishes the last
		// list's count rather than leaving the column unanswered.
		w.Queue.Publish(w.Rig, count)
		return rep
	}
	// Taken before the list: a label written while the list runs must make the
	// stamp newer than this pass's, so the next pass lists and sees it.
	listedAt := w.now()
	issues, err := w.Beads.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1})
	if err != nil {
		// A list that failed read nothing, so no empty list may stand in for it
		// next pass.
		w.lastListAt = time.Time{}
		w.logf("listing %s beads: %v", land.LabelReadyToLand, err)
		rep.Failed++
		return rep
	}
	w.lastListAt = listedAt
	w.lastListEmpty = len(issues) == 0
	w.lastListCount = len(issues)
	// The count the queue reads until this rig's next pass (gt-hw6ih). It is
	// the whole list, not the beads this pass still has to land, because the
	// column it answers counts what waits.
	w.Queue.Publish(w.Rig, len(issues))
	// The pool reviews this list while the pass lands it, so the landing has a
	// verdict to reuse instead of running om inline. It is offered on ctx, the
	// run context the pass was called with, which outlives one landing and one
	// pass: a review must keep running past both (gt-mdv8eq).
	w.offerReview(ctx, issues, "")
	// The submissions this pass has looked at, whatever it left them on: a bead
	// that stays ready after its attempt is not tried twice in one pass, and the
	// loop ends once a fresh list holds nothing outside this set (gt-wgvu6p).
	handled := make(map[string]bool, len(issues))
	// The bead and head pairs this pass has already swept: one stored review is
	// offered at most once per pass, even when the sweep counted nothing for it,
	// so a kept record is not landed stored-only every round (gt-9nco9r). A new
	// head for the bead is its own pair and is swept once (gt-k5s380).
	swept := make(map[string]bool)
	for {
		if ctx.Err() != nil {
			return rep
		}
		// Checked before each claim, never mid-landing.
		if w.draining() {
			return rep
		}
		// The stored rejections are consumed before the next bead is chosen, so
		// a bead's author is told the review refused it without waiting behind
		// the landings queued ahead of it (gt-9nco9r).
		w.sweepStored(ctx, issues, handled, swept, &rep)
		// The sweep may have learned that the run was canceled or a drain
		// started; nothing more is started once either is true.
		if ctx.Err() != nil || w.draining() {
			return rep
		}
		issue := nextReady(issues, handled)
		if issue == nil {
			return rep
		}
		handled[handledKey(issue)] = true
		// IsActionable, not IsTerminal: deferring a submitted bead is the
		// operator's only hold on it (gt bead reset refuses a ready bead), so
		// the worker must read the same park the health count reports, or a
		// "0 pending" field sits over a queue that still lands (gt-y7n1u).
		if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() || !beads.HasLabel(issue, land.LabelReadyToLand) {
			continue
		}
		// The operator's retry label starts a parked bead on a new run
		// without a daemon restart. It comes off first: only a write that
		// succeeded clears the park and the run, so a failed write leaves
		// both as they were for the next pass (gt-46bh2y).
		if beads.HasLabel(issue, LabelLandingRetry) {
			err := w.Beads.Update(issue.ID, beads.UpdateOptions{RemoveLabels: []string{LabelLandingRetry}})
			if err != nil {
				w.logf("%s: removing %s: %v; the park stands and the next pass tries the label again", issue.ID, LabelLandingRetry, err)
				rep.Skipped++
				continue
			}
			// The run is dropped whole, so a field added later starts at
			// zero; the annotations the bead already carries outlive it as
			// they do any other run, except the infrastructure notice, which
			// the new run posts again (gt-46bh2y).
			st := w.state[issue.ID]
			delete(w.state, issue.ID)
			if st != nil && len(st.annotated) > 0 {
				delete(st.annotated, "infra")
				w.bead(issue.ID).annotated = st.annotated
			}
			w.outcomeMemo().forget(issue.ID)
		}
		// A parked bead is skipped before the pass does anything with it:
		// any write to it moves updated_at, so a hold keyed on the row lifts
		// at the next pass and the fail-loud park would last one pass
		// (gt-5phoiv).
		if w.parked(issue.ID) {
			rep.Skipped++
			continue
		}
		// The bead keeps the label until its queued bookkeeping writes the
		// record and removes it, so the listed row is the one already landed
		// (gt-ppcl1g).
		if w.booksHold(issue.ID) {
			rep.Skipped++
			continue
		}
		// Before the landing, which is what removes the label: a submission
		// that lands within the battery's interval must already be recorded.
		w.recordFirstSight(issue)
		if _, pending := w.pendingRepair[issue.ID]; pending {
			continue
		}
		// A bead whose row is the one its last outcome left, and whose hold
		// has not come due, is not read again (gt-r9pj9).
		if w.outcomeMemo().held(issue.ID, issue.UpdatedAt) {
			rep.Skipped++
			continue
		}
		// The pool reviews this bead while the pass lands the beads ahead of it,
		// so the landing reuses that verdict instead of starting a second om run:
		// it waits for the review of the head it is about to land (gt-m9gfv9).
		if !w.waitForReview(ctx, issue) {
			return rep
		}
		w.process(ctx, issue, &rep)
		w.rememberOutcome(issue)
		// A landing takes minutes, and a bead made ready while it ran must land
		// in this pass's order, not after every bead the pass listed first: the
		// fresh list is read after each attempt (gt-wgvu6p). The bookkeeping
		// above stays the first list's; a later list that fails ends the pass.
		issues, err = w.Beads.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1})
		if err != nil {
			w.logf("listing %s beads: %v", land.LabelReadyToLand, err)
			rep.Failed++
			return rep
		}
		// The fresh list is offered again: a bead made ready while the landing
		// ran, and a work that did not fit the pool's cap until a review ended,
		// are reviewed without a queue (gt-20kjw5). The bead about to be landed
		// is left out, since its review would be spent on a landing now due.
		landing := ""
		if next := nextReady(issues, handled); next != nil {
			landing = next.ID
		}
		w.offerReview(ctx, issues, landing)
	}
}

// skipReadyList reports the count to publish and whether this pass can skip
// listing the ready label: the last list was empty, no ready stamp is newer
// than that list, and the list is younger than readyListCeiling.
//
// Only an empty list is skippable: a non-empty one must be listed again to see
// the beads land and the column fall to zero. A stamp that cannot be read lists
// (fail open), because it is not evidence that nothing was submitted
// (gt-flo7a); the read failure is logged once for the run of failures, since
// the pass falls back to listing for as long as it persists (gt-5obd1).
func (w *Worker) skipReadyList() (int, bool) {
	if w.TownRoot == "" || w.lastListAt.IsZero() || !w.lastListEmpty {
		return 0, false
	}
	age := w.now().Sub(w.lastListAt)
	if age < 0 || age >= readyListCeiling {
		return 0, false
	}
	stamp, err := land.ReadyStamp(w.TownRoot, w.Rig)
	if err != nil {
		if !w.stampUnreadable {
			w.logf("reading the ready stamp; listing the board instead: %v", err)
		}
		w.stampUnreadable = true
		return 0, false
	}
	w.stampUnreadable = false
	if stamp.After(w.lastListAt) {
		return 0, false
	}
	return w.lastListCount, true
}

// queueRecentRepairs finds landings the file records whose bead is still
// open: the daemon stopped between the push and the close. Land finishes
// them from the file without landing again.
func (w *Worker) queueRecentRepairs() {
	if w.Landings == nil {
		return
	}
	recs, err := w.Landings.Recent(recentRepairWindow)
	if err != nil {
		w.logf("reading the landings file for incomplete records: %v", err)
		return
	}
	// Only the worker's own landings, and the distinct beads they name: one
	// read answers every record of the same bead.
	owned := make([]land.LandingRecord, 0, len(recs))
	ids := make([]string, 0, len(recs))
	seen := make(map[string]struct{}, len(recs))
	for _, rec := range recs {
		if rec.Route != "" && rec.Route != "daemon" {
			continue
		}
		owned = append(owned, rec)
		if _, ok := seen[rec.BeadID]; !ok {
			seen[rec.BeadID] = struct{}{}
			ids = append(ids, rec.BeadID)
		}
	}
	// One call for every bead, not one per record (gt-xzcjh).
	issues, batched := w.showMultiple(ids)
	for _, rec := range owned {
		// The queued job is writing this record; a repair queued now would
		// race it (gt-ppcl1g).
		if w.booksHold(rec.BeadID) {
			continue
		}
		// Only while the bead still waits on them: a bead a human reopened
		// after its landing record was written is theirs, not a record to
		// finish.
		var issue *beads.Issue
		if batched {
			issue = issues[rec.BeadID]
		} else {
			var err error
			if issue, err = w.Beads.Show(rec.BeadID); err != nil {
				continue
			}
		}
		if issue == nil || beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
			continue
		}
		if hasLandingRecord(issue.Notes, rec.LandedCommit) && !beads.HasLabel(issue, land.LabelReadyToLand) {
			continue
		}
		if resubmittedAfter(issue.Notes, rec.LandedCommit) {
			continue
		}
		on, err := w.Remote.Contains(rec.Target, rec.LandedCommit)
		if err != nil || !on {
			continue
		}
		w.pendingRepair[rec.BeadID] = workFromRecord(rec, issue)
	}
}

// showMultiple reads ids in one call where the beads client can batch them.
// ok is false when the client has no ShowMultiple or the call failed, and the
// caller then reads them one at a time: one bad read never drops the repairs
// of the others.
func (w *Worker) showMultiple(ids []string) (issues map[string]*beads.Issue, ok bool) {
	if len(ids) == 0 {
		return nil, true
	}
	batch, can := w.Beads.(interface {
		ShowMultiple([]string) (map[string]*beads.Issue, error)
	})
	if !can {
		w.saySlowReads()
		return nil, false
	}
	issues, err := batch.ShowMultiple(ids)
	if err != nil {
		// A store without batching answers ErrNoShowMultiple: say so once, and
		// log only a real failure on every pass.
		if errors.Is(err, ErrNoShowMultiple) {
			w.saySlowReads()
		} else {
			w.logf("reading the beads of %d landing records in one call: %v", len(ids), err)
		}
		return nil, false
	}
	return issues, true
}

// saySlowReads logs, once, that the beads client cannot batch and the repair
// pass reads each landing record's bead on its own.
func (w *Worker) saySlowReads() {
	if w.noBatchSaid {
		return
	}
	w.noBatchSaid = true
	w.logf("the beads client cannot batch reads: reading each landing record's bead on its own")
}

func workFromRecord(rec land.LandingRecord, issue *beads.Issue) land.Work {
	// The record being finished already names the agent its landing ran on,
	// so the repair writes it back rather than asking the agent bead again,
	// which a seat reset since may answer differently (gt-mukgk.6).
	// The repair rewrites the record from this work, so the estimate the record
	// holds is carried too, or the repair would drop it (gt-cdhagm), and so are
	// the minutes behind it and the route's reason (gt-5eeqin).
	work := land.Work{BeadID: rec.BeadID, Rig: rec.Rig, Branch: rec.Branch, Head: rec.Head, Target: rec.Target, Agent: rec.Agent,
		SPEstimate: rec.SPEstimate, SPMinutes: rec.SPMinutes, SPSlung: rec.SPSlung}
	if ready, ok := land.ParseReadyNote(issue.Notes); ok {
		work.Worker = ready.Worker
	} else {
		work.Worker = polecatFromAssignee(issue.Assignee)
	}
	return work
}

// recordFirstSight writes the first-sight record of a waiting submission. A
// bead with no READY TO LAND block has no submission to record: process
// reports it. A failed write is logged once for each run of failures and never
// holds up the landing.
func (w *Worker) recordFirstSight(issue *beads.Issue) {
	if w.FirstSight == nil {
		return
	}
	work, err := land.WorkFromBead(issue, w.Rig)
	if err != nil {
		return
	}
	w.recordSubmission(work)
}

// recordSubmission writes the first-sight record of work. A record already on
// file is no write, so it does not end a run of failures.
func (w *Worker) recordSubmission(work land.Work) {
	if w.FirstSight == nil {
		return
	}
	wrote, err := w.FirstSight.Record(work)
	if err != nil {
		if !w.firstSightFailing {
			w.logf("%s: recording the submission for the jev battery: %v", work.BeadID, err)
		}
		w.firstSightFailing = true
		return
	}
	if wrote {
		w.firstSightFailing = false
	}
}

// process resolves what to land for issue and lands it.
func (w *Worker) process(ctx context.Context, issue *beads.Issue, rep *Report) {
	w.processWith(ctx, issue, rep, false)
}

// sweepStored lands, as a stored-only pass, each ready bead's stored rejection
// before the next landing is chosen, so a rejected bead's author is told before
// the beads queued ahead of it run their gates; a rejection merges nothing, so
// no landing order changes. It leaves a bead alone when the landing loop's own
// turn for it is not due yet, and stops between beads once the run is canceled
// or a drain is on. One bead and head is swept at most once per pass: swept
// names the pairs already offered, and a bead the sweep counted an outcome for
// is marked handled so the loop does not attempt it again while it stays ready;
// a bead with nothing stored to consume counts nothing and keeps its ordinary
// turn, so a stored review the pool replaced for a new head while that turn
// waits is swept once for the new head too. Each landing it makes records the
// first-sight record and the outcome memo the loop records around its own. With
// Stored nil nothing is swept (gt-5q8ayw). A hint that cannot be read is
// unknown, never "not rejecting": the error is logged in one line naming the
// bead and the bead is left for its ordinary turn while the sweep goes on with
// the rest (gt-end632).
func (w *Worker) sweepStored(ctx context.Context, issues []*beads.Issue, handled, swept map[string]bool, rep *Report) {
	if w.Stored == nil {
		return
	}
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		// A bead the pass has already looked at keeps its turn: sweeping it again
		// would land it a second time (gt-wgvu6p).
		submission := handledKey(issue)
		if handled[submission] {
			continue
		}
		if ctx.Err() != nil || w.draining() {
			return
		}
		if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() || !beads.HasLabel(issue, land.LabelReadyToLand) {
			continue
		}
		if w.parked(issue.ID) {
			continue
		}
		// The loop clears the operator's retry label and drops the old run
		// before it lands the bead, so the sweep leaves that bead to it.
		if beads.HasLabel(issue, LabelLandingRetry) {
			continue
		}
		if _, pending := w.pendingRepair[issue.ID]; pending {
			continue
		}
		// The loop skips a bead whose landing record is still being written, so
		// the sweep must not land it stored-only in that window (gt-ppcl1g).
		if w.booksHold(issue.ID) {
			continue
		}
		if w.outcomeMemo().held(issue.ID, issue.UpdatedAt) {
			continue
		}
		head, rejecting, err := w.Stored.Hint(issue.ID)
		if err != nil {
			// The hint is unknown, not "not rejecting": the bead keeps its
			// ordinary turn and the sweep goes on with the rest (gt-end632).
			w.logf("%s: reading the stored review: %v; the bead keeps its turn", issue.ID, err)
			continue
		}
		if !rejecting {
			continue
		}
		// The head joins the key after the hint, so a record replaced for a new
		// head while the bead waits its turn is swept once for that head; the
		// same pair is offered only once per pass (gt-k5s380).
		key := sweptKey(issue.ID, head)
		if swept[key] {
			continue
		}
		swept[key] = true
		before := *rep
		// The loop records a submission before it lands and what it left the
		// bead on after, so a swept landing leaves the same trail: the first-sight
		// record the battery reads, and the outcome memo the next pass reads in
		// place of the bead's row (gt-9nco9r).
		w.recordFirstSight(issue)
		w.processWith(ctx, issue, rep, true)
		w.rememberOutcome(issue)
		// An outcome the pass counted — a rejection, a skip or a failure — has
		// spent the bead's turn, so the loop must not attempt it again while it
		// stays ready (gt-9nco9r).
		if *rep != before {
			handled[submission] = true
		}
	}
}

// processWith resolves what to land for issue and lands it. storedOnly marks the
// landing as a stored-only pass: it consumes the bead's stored review in place of
// running om (land.Work.StoredOnly), and the recorded-landing repair is left to
// the ready pass — a stored-only pass lands what the bead asks for now, never
// finishes an earlier landing's record. It touches neither the ready pass's
// handled set nor rememberOutcome, which belong to landReady (gt-t3fgce).
func (w *Worker) processWith(ctx context.Context, issue *beads.Issue, rep *Report, storedOnly bool) {
	if w.Active != nil {
		w.Active(issue.ID)
		defer w.Active("")
	}
	// Read from the bead this pass loaded: a landed bead carrying the label
	// arms the install request, and a rejected or skipped one never reaches
	// the outcome that reads it (gt-3qmv4.2).
	w.bead(issue.ID).installNow = beads.HasLabel(issue, LabelInstallNow)

	work, ok, err := w.resolveWork(issue)
	if err != nil {
		w.infraFailure(issue.ID, "reading what to land", err, rep)
		return
	}
	if !ok {
		w.waitForHuman(issue.ID, "no-request", fmt.Sprintf("Landing worker: %s carries %s but neither a READY TO LAND block nor a \"Submitted for landing: <branch> @ <sha>\" comment says what to land. Leaving the label for a human.", issue.ID, land.LabelReadyToLand))
		rep.Skipped++
		return
	}

	// A landing already on the target with an incomplete record is repaired,
	// never landed twice, whatever the branch holds now. A stored-only pass
	// leaves the repair to the ready pass, and so reads no landing record: it
	// lands the submission the bead carries now (gt-t3fgce).
	if !storedOnly {
		if rec, found, err := w.Landings.LatestForBead(issue.ID); err != nil {
			w.infraFailure(issue.ID, "reading the landings file", err, rep)
			return
		} else if found && !resubmittedAfter(issue.Notes, rec.LandedCommit) {
			on, err := w.Remote.Contains(rec.Target, rec.LandedCommit)
			if err != nil {
				w.infraFailure(issue.ID, "checking a recorded landing", err, rep)
				return
			}
			if on {
				w.logf("%s: already landed as %s; repairing its record only", issue.ID, short(rec.LandedCommit))
				work := workFromRecord(rec, issue)
				w.pendingRepair[issue.ID] = work
				w.landOne(ctx, work, rep)
				return
			}
		}
	}

	// Who the bead records submitting the work, checked before anything
	// lands: a submission under the human's own identity would read as the
	// human's work, and one naming no submitter would pass the check by
	// default (gt-2ge3z). A record whose comments cannot be read is unknown
	// too, and an unknown actor never lands.
	actor, err := w.submissionActor(issue, work)
	if err != nil {
		w.infraFailure(issue.ID, "reading the submission record", err, rep)
		return
	}
	if key, msg := w.actorRefusal(issue.ID, actor); msg != "" {
		w.waitForHuman(issue.ID, key, msg)
		rep.Skipped++
		return
	}

	tip, err := w.Remote.BranchTip(work.Branch)
	if err != nil {
		w.infraFailure(issue.ID, "reading the branch tip", err, rep)
		return
	}
	if tip == "" {
		w.waitForHuman(issue.ID, "branch-missing:"+work.Branch, fmt.Sprintf("Landing worker: branch %s is not on origin, so %s cannot land. Leaving %s for a human.", work.Branch, issue.ID, land.LabelReadyToLand))
		rep.Skipped++
		return
	}
	if tip != work.Head && !strings.HasPrefix(tip, work.Head) {
		w.logf("%s: %s moved from the submitted %s to %s; landing the tip", issue.ID, work.Branch, short(work.Head), short(tip))
		// The battery reads the commits of the head this landing takes, so
		// that head gets its own first-sight record before the landing.
		moved := work
		moved.Head = tip
		w.recordSubmission(moved)
	}
	work.Head = tip
	work.Agent = w.seatAgent(work.Worker)
	work.Priority = w.beadPriority(issue.ID)
	work.SPEstimate, work.SPMinutes, work.SPSlung = w.beadSPLabels(issue.ID)
	work.StoredOnly = storedOnly
	w.landOne(ctx, work, rep)
}

// hasLandingRecord reports whether notes carry the LANDING RECORD block for
// landedCommit.
func hasLandingRecord(notes, landedCommit string) bool {
	return landedCommit != "" && strings.Contains(notes, land.LandingNoteMarker+"\nlanded_commit: "+landedCommit)
}

// resubmittedAfter reports whether a READY TO LAND block follows the LANDING
// RECORD block for landedCommit: the bead landed once, was reopened, and
// carries new work, which lands normally rather than as a repair.
func resubmittedAfter(notes, landedCommit string) bool {
	rec := strings.LastIndex(notes, land.LandingNoteMarker+"\nlanded_commit: "+landedCommit)
	return rec >= 0 && strings.LastIndex(notes, land.ReadyNoteMarker+"\n") > rec
}

// submittedRE matches gt done's submission comment:
// "Submitted for landing: <branch> @ <sha> onto <target> (attempt N)".
var submittedRE = regexp.MustCompile(`Submitted for landing: (\S+) @ ([0-9a-fA-F]{4,40})(?: onto (\S+))?`)

// resolveWork reads the landing request from the READY TO LAND block, or,
// failing that, the latest submission comment. It leaves Agent empty: process
// asks the seat's agent bead only for a request it is about to land. ok is
// false when neither the notes nor the comments name the work; a comments read
// that fails is reported as an error, since what the bead asks for is then
// unknown rather than absent.
func (w *Worker) resolveWork(issue *beads.Issue) (land.Work, bool, error) {
	if work, err := land.WorkFromBead(issue, w.Rig); err == nil {
		return work, true, nil
	}
	comments, err := w.Beads.Comments(issue.ID)
	if err != nil {
		return land.Work{}, false, fmt.Errorf("reading comments on %s: %w", issue.ID, err)
	}
	for i := len(comments) - 1; i >= 0; i-- {
		m := submittedRE.FindStringSubmatch(comments[i].Text)
		if m == nil {
			continue
		}
		target := m[3]
		if target == "" {
			target = "main"
		}
		return land.Work{BeadID: issue.ID, Rig: w.Rig, Branch: m[1], Head: m[2], Target: target, Worker: polecatFromAssignee(issue.Assignee)}, true, nil
	}
	return land.Work{}, false, nil
}

// waitForReview waits for the review pool's review of the head issue's own
// submission names, the head this pass is about to land, so the landing reuses
// that verdict instead of starting a second om run (gt-m9gfv9). It reports
// false when ctx is done once the wait ends, and the caller then returns from
// the pass without landing the bead: ctx is the context Pass was called with,
// so a daemon stop ends the wait and the pass. It reports true without waiting
// when no pool is set, or when the bead names no submission — its comments
// cannot be read, or they name none — since process reports such a bead as it
// did before the pool existed. A review of the bead at another head returns at
// once (ReviewPool.Await), as does one that is not running.
func (w *Worker) waitForReview(ctx context.Context, issue *beads.Issue) bool {
	if w.Pool == nil {
		return true
	}
	work, ok, err := w.resolveWork(issue)
	if err != nil || !ok {
		return true
	}
	w.Pool.Await(ctx, issue.ID, work.Head)
	return ctx.Err() == nil
}

// offerReview offers the works reviewWorks builds for issues, leaving out the
// bead named by landing (empty for none), to the review pool. A nil pool
// offers nothing; an error building the works offers nothing that time and is
// named on one line, since the pool can only save time and a broken offer must
// never stop the pass landing the beads it can (gt-mdv8eq).
func (w *Worker) offerReview(ctx context.Context, issues []*beads.Issue, landing string) {
	if w.Pool == nil {
		return
	}
	works, err := w.reviewWorks(issues, landing)
	if err != nil {
		w.logf("the review pool: building the works: %v; offering none this pass", err)
		return
	}
	w.Pool.Offer(ctx, works)
}

// reviewWorks builds a work for each issue the pool may review ahead of a
// landing: one whose status is still actionable, that carries the ready label,
// that is not the bead named by landing, the one being landed, and that this
// pass is not holding back — one parked on an infrastructure failure, one held
// by its last outcome, one whose landing record is being repaired, or one whose
// deferred bookkeeping is still queued (booksHold). It sorts
// the issues in place into the order the landing will take them
// (sortByLandingOrder), from the same resolveWork process reads, so the pool
// starts its reviews in landing order and reviews the head the submission
// names. An issue left out is never resolved. One the pool would review whose
// comments read cleanly but name no submission is left out of the offer, named
// in one log line, and the other issues are still offered in landing order; one
// whose comments cannot be read is returned as an error with no works at all,
// since what such a bead asks for is unknown rather than absent.
func (w *Worker) reviewWorks(issues []*beads.Issue, landing string) ([]land.Work, error) {
	sortByLandingOrder(issues)
	works := make([]land.Work, 0, len(issues))
	for _, issue := range issues {
		if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() ||
			!beads.HasLabel(issue, land.LabelReadyToLand) ||
			(landing != "" && issue.ID == landing) {
			continue
		}
		// The holds landReady keeps a bead under, read from the same in-memory
		// state: the pass skips such a bead, so a review of it now is spent on
		// a landing this pass will not make (gt-u8s6m7). A bead whose queued
		// bookkeeping has not written its record yet still reads as ready, so
		// the pass skips it too (gt-ppcl1g).
		if w.parked(issue.ID) || w.outcomeMemo().held(issue.ID, issue.UpdatedAt) || w.booksHold(issue.ID) {
			continue
		}
		if _, pending := w.pendingRepair[issue.ID]; pending {
			continue
		}
		work, ok, err := w.resolveWork(issue)
		if err != nil {
			return nil, err
		}
		if !ok {
			w.logf("%s: no READY TO LAND block and no submission comment say what to land; leaving it out of the review pool", issue.ID)
			continue
		}
		works = append(works, work)
	}
	return works, nil
}

// submissionActor is the identity work is recorded as submitted by: the author
// of the newest "Submitted for landing" comment naming work's branch and head,
// else the READY TO LAND block's Worker line. A crew session's gt done writes
// the comment under whatever identity the session runs as; a polecat's writes
// no comment and names itself on the Worker line.
func (w *Worker) submissionActor(issue *beads.Issue, work land.Work) (string, error) {
	comments, err := w.Beads.Comments(issue.ID)
	if err != nil {
		return "", fmt.Errorf("reading comments: %w", err)
	}
	for i := len(comments) - 1; i >= 0; i-- {
		m := submittedRE.FindStringSubmatch(comments[i].Text)
		if m == nil || m[1] != work.Branch || !strings.HasPrefix(work.Head, m[2]) {
			continue
		}
		return strings.TrimSpace(comments[i].Author), nil
	}
	return strings.TrimSpace(work.Worker), nil
}

// actorRefusal is the annotation key and the reason the worker must not land a
// submission recorded by actor; both are empty for one it may land. The bead
// keeps its label: the submission is still the worker's to land once an agent
// re-records it.
func (w *Worker) actorRefusal(id, actor string) (key, msg string) {
	switch {
	case w.HumanActor == "":
		return "no-human-identity", fmt.Sprintf("Landing worker: %s is ready to land, but this landing worker has no human identity configured, so it cannot tell a human's submission from an agent's. Leaving %s for a human.", id, land.LabelReadyToLand)
	case actor == "":
		return "no-actor", fmt.Sprintf("Landing worker: %s is ready to land but records no submitter, so the worker cannot tell whether a human or an agent submitted it. An agent must submit it (for example the overseer, from its crew session). Leaving %s for a human.", id, land.LabelReadyToLand)
	case land.IsHumanActor(actor, w.HumanActor):
		return "human-actor:" + actor, fmt.Sprintf("Landing worker: %s was submitted for landing by %q, the human's own identity, not an agent. An agent must submit it (for example the overseer, from its crew session). Leaving %s for a human.", id, actor, land.LabelReadyToLand)
	}
	return "", ""
}

// seatAgent is the agent a landing's polecat seat ran on: the agent line its
// agent bead carries, which the sling wrote and the session's end leaves in
// place (gt-mukgk.6). A crew landing has no seat, so its worker is empty and
// it asks nobody. A bead the store does not have — a landing request naming a
// seat that never wrote one, or an older agent bead with no agent line — is no
// agent, never an error that would hold the landing.
func (w *Worker) seatAgent(polecat string) string {
	if w.AgentBeadID == nil || polecat == "" {
		return ""
	}
	id := w.AgentBeadID(polecat)
	if id == "" {
		return ""
	}
	issue, err := w.Beads.Show(id)
	if err != nil {
		if !errors.Is(err, beads.ErrNotFound) {
			w.logf("%s: reading the agent bead %s: %v; the landing records no agent", polecat, id, err)
		}
		return ""
	}
	if issue == nil {
		return ""
	}
	return strings.TrimSpace(beads.ParseAgentFields(issue.Description).Agent)
}

// beadPriority is the landed bead's priority, read from the bead the way
// writeSPActual reads it. A failed read is logged and leaves the priority nil:
// the record carries none, and the landing stands (gt-tnml1k).
func (w *Worker) beadPriority(id string) *int {
	issue, err := w.Beads.Show(id)
	if err != nil {
		w.logf("%s: reading the bead's priority: %v; the landing records no priority", id, err)
		return nil
	}
	if issue == nil {
		return nil
	}
	p := issue.Priority
	return &p
}

// beadSPLabels reads the landed bead once for the three sp label values the
// record carries, so the three cost one Show of the bead and not one each
// (gt-fqaj7z): its sp: estimate, its sp-minutes:<n> and the sp-slung:<reason>
// the dispatcher wrote. A failed read is logged once and leaves all three
// empty: the record carries none, and the landing stands, as it did when each
// value read the bead alone. A bead with no such label, or with two that
// disagree, has none and is no failure.
func (w *Worker) beadSPLabels(id string) (estimate *int, minutes *float64, slung string) {
	issue, err := w.Beads.Show(id)
	if err != nil {
		w.logf("%s: reading the bead's sp: estimate, sp-minutes and sp-slung reason: %v; the landing records none", id, err)
		return nil, nil, ""
	}
	if issue == nil {
		return nil, nil, ""
	}
	estimate = beadSPEstimate(issue.Labels)
	if got, ok := spMinutesOf(issue.Labels); ok {
		minutes = &got
	}
	return estimate, minutes, spSlungOf(issue.Labels)
}

// beadSPEstimate is the sp: estimate among labels. A bead with no estimate, or
// with two that conflict, has none and is no failure (gt-cdhagm).
func beadSPEstimate(labels []string) *int {
	points, ok := storypoints.EstimateOf(labels)
	if !ok {
		return nil
	}
	return &points
}

// spSlungPrefix begins the label the dispatcher writes on a bead it slings at
// or over the sp: split trigger, sp-slung:<reason> (gt-l04nj0). The landing
// worker reads it but cannot import the command package that writes it, so the
// prefix is repeated here.
const spSlungPrefix = "sp-slung:"

// spMinutesOf returns the minutes of the one sp-minutes:<n> label among
// labels. A label storypoints.ParseLabel refuses is not an estimate; a bead
// with none, or with two that disagree, has none.
func spMinutesOf(labels []string) (float64, bool) {
	minutes, found := 0.0, false
	for _, l := range labels {
		label, err := storypoints.ParseLabel(l)
		if err != nil || label.Kind != storypoints.KindMinutes {
			continue
		}
		if found && label.Minutes != minutes {
			return 0, false
		}
		minutes, found = label.Minutes, true
	}
	return minutes, found
}

// spSlungOf returns the reason of the one sp-slung:<reason> label among
// labels. A label with an empty reason carries no reason; a bead with none, or
// with two that disagree, has none.
func spSlungOf(labels []string) string {
	reason, found := "", false
	for _, l := range labels {
		rest, ok := strings.CutPrefix(l, spSlungPrefix)
		if !ok || rest == "" {
			continue
		}
		if found && rest != reason {
			return ""
		}
		reason, found = rest, true
	}
	return reason
}

// polecatFromAssignee is the polecat name in "<rig>/polecats/<name>".
func polecatFromAssignee(assignee string) string {
	parts := strings.Split(strings.TrimSpace(assignee), "/")
	if len(parts) == 3 && parts[1] == "polecats" {
		return parts[2]
	}
	return ""
}

// outcome is what one Land call means for the worker.
type outcome int

const (
	outLanded outcome = iota
	outRecordIncomplete
	outRejectedRework
	outRejectedHuman
	outRace
	outNotReady
	outReadBack
	outCanceled
	outNoStored
	outInfra
)

// classify maps Land's result onto the worker's decisions.
func classify(ctx context.Context, err error) outcome {
	var (
		rej   *land.Rejection
		race  *land.RaceError
		recEr *land.RecordError
	)
	switch {
	case err == nil:
		return outLanded
	case errors.As(err, &recEr):
		return outRecordIncomplete
	case errors.As(err, &rej):
		if rej.Rework {
			return outRejectedRework
		}
		return outRejectedHuman
	case errors.As(err, &race):
		return outRace
	case errors.Is(err, land.ErrNotReady):
		return outNotReady
	case errors.Is(err, land.ErrReadBack):
		return outReadBack
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
		return outCanceled
	case errors.Is(err, land.ErrNoStoredVerdict):
		// A stored-only pass found no stored verdict to consume: the pass asked
		// and was answered, so nothing failed and nothing is owed. The bead
		// keeps its ordinary turn rather than being parked on a verdict the
		// pass was never going to find (gt-ixvxt5).
		return outNoStored
	case errors.Is(err, land.ErrCISilence):
		// The candidate gate reported nothing: infrastructure, never a
		// verdict on the work. CI red comes back as a *Rejection, so the two
		// halves of the design's rule split here, and the silence is counted
		// for its own escalation (countCISilence).
		return outInfra
	default:
		return outInfra
	}
}

// landOne calls Land once and acts on the outcome.
func (w *Worker) landOne(ctx context.Context, work land.Work, rep *Report) {
	// A pass lasts as long as all its landings, so the pending verdicts are read
	// before a landing and not only at the pass's end: a red status is acted on, and
	// a green one read, about every interval. The poll is rate-limited here because
	// PollPending's budget is per call and a read it cuts off learns nothing: a
	// stalled remote would otherwise cost the whole budget at every landing start.
	if w.PostLand != nil && !w.draining() && w.pollDue() {
		w.pollPending(ctx)
	}
	timeout := w.LandTimeout
	if timeout <= 0 {
		timeout = DefaultLandTimeout
	}
	lctx, cancel := context.WithTimeout(ctx, timeout)
	res, err := w.Lander.Land(lctx, work)
	cancel()

	st := w.bead(work.BeadID)
	_, wasRepair := w.pendingRepair[work.BeadID]
	// Only a new landing carries the label's request: a repair finishes a
	// landing an earlier pass already accounted for (gt-3qmv4.2).
	installNow := st.installNow && !wasRepair
	oc := classify(ctx, err)
	// A stored-only pass that consumed nothing attempted no landing, so it
	// neither answers nor resets the CI-silence run the ready pass counts.
	if oc != outInfra && oc != outNoStored {
		st.ciSilences = 0
	}
	switch oc {
	case outLanded:
		delete(w.pendingRepair, work.BeadID)
		w.endFailureRun(work.BeadID)
		if wasRepair {
			rep.Repaired++
		} else {
			rep.Landed++
		}
		if installNow {
			rep.InstallRequested = true
		}
		w.logf("%s: landed %s on %s (patch-id %s)", work.BeadID, short(res.LandedCommit), work.Target, short(res.PatchID))
		if res.Deferred != nil {
			// A deferred landing still owes the bead half of its record: the job
			// finishes it and runs the four landing steps off this goroutine when
			// a queue is set, and the bead stays held (booksHold) until the
			// record is written, so no later pass lands it again (gt-c3vl2u).
			w.finishDeferredQueued(work, res, wasRepair)
		} else {
			w.refreshAuthorSeats(work, w.reapBeadBranches(work.BeadID))
		}
		w.clearIntent(work)
		if !wasRepair {
			w.afterLanding(ctx, work, res)
		}
		if res.Deferred == nil {
			// The label needs the landing's complete record, and a repair is the pass
			// that completes a record an earlier pass left unfinished, so this runs
			// on a repair too. The write is idempotent: a stale sp-actual is replaced.
			w.writeSPActual(work.BeadID, res.LandedCommit)
			// The record is complete now, which is the proof the seat's end reads.
			// A repair that finishes a record left unfinished reaches it here, so
			// the seat its earlier pass kept is ended once the record can prove it.
			if w.AfterLanded != nil {
				w.AfterLanded(work)
			}
		}
	case outRecordIncomplete:
		// Landed: the push was read back. Only the record is unfinished.
		w.endFailureRun(work.BeadID)
		w.pendingRepair[work.BeadID] = work
		rep.Landed++
		if installNow {
			rep.InstallRequested = true
		}
		w.logf("%s: %v; the next pass finishes the record", work.BeadID, err)
		w.clearIntent(work)
		if !wasRepair {
			var recEr *land.RecordError
			if errors.As(err, &recEr) {
				res = recEr.Result
			}
			w.afterLanding(ctx, work, res)
		}
	case outRejectedRework:
		var rej *land.Rejection
		errors.As(err, &rej)
		rep.Rejected++
		if rej.Superseded {
			// The bead is another holder's now, so RevertRejected's close and
			// label strip are not this worker's to make.
			w.supersededRejection(work, rej)
			w.clearIntent(work)
			return
		}
		w.logf("%s: %v", work.BeadID, err)
		if rej.RecordErr != nil {
			// The label may still be on the bead: space the next attempt.
			w.closeFailureRun(work.BeadID)
			st.until = w.now().Add(rejectRecordBackoff)
			return
		}
		w.endFailureRun(work.BeadID)
		if w.Reverts != nil && w.Reverts.RevertRejected(work, rej) {
			return
		}
		msg := reworkMessage(rej)
		w.annotate(work.BeadID, "rework:"+work.Head, "adding the rework comment", msg)
		w.clearIntent(work)
	case outRejectedHuman:
		var rej *land.Rejection
		errors.As(err, &rej)
		rep.Rejected++
		if rej.Superseded {
			w.supersededRejection(work, rej)
			w.clearIntent(work)
			return
		}
		w.logf("%s: %v (left for the overseer: %s)", work.BeadID, err, land.LabelNeedsOverseer)
		// NoEscalate is set when an earlier block of the refusal's lane already
		// escalated the loop, so the operator is not alerted again (gt-dtx3ht).
		if w.shouldEscalate(work, rej) {
			w.escalate(work.BeadID, land.LaneOf(work.Branch), escalationText(work, rej))
		}
		if rej.RecordErr != nil {
			w.closeFailureRun(work.BeadID)
			st.until = w.now().Add(rejectRecordBackoff)
			return
		}
		w.endFailureRun(work.BeadID)
		// The bead keeps its ready label until someone acts on the refusal,
		// so it is re-read on the wait a human refusal gets rather than on
		// every pass (gt-r9pj9).
		w.bead(work.BeadID).until = w.now().Add(humanWaitBackoff)
		w.annotate(work.BeadID, "human-rejection:"+work.Head, "adding the human-rejection comment", humanMessage(rej))
		w.clearIntent(work)
	case outRace:
		// Target moved during the gate: nothing written; land again next pass.
		var race *land.RaceError
		errors.As(err, &race)
		if race != nil && race.Rebuild {
			// A cut-over rig merges a candidate cut from the target, so a busy
			// target refuses the merge and the rebuild is the normal case, not
			// a failure the report counts (design, "Risks": "The worker must
			// treat the rebuild as the normal case, not an error").
			w.logf("%s: %v; rebuilding the candidate on the new tip", work.BeadID, err)
			return
		}
		rep.Failed++
		w.logf("%s: %v", work.BeadID, err)
	case outNotReady:
		delete(w.pendingRepair, work.BeadID)
		rep.Skipped++
		// Not ready means the store's bead disagrees with the row the pass
		// listed, so the next list shows the change; until it does, the bead
		// waits the refusal's wait rather than every pass (gt-r9pj9).
		st.until = w.now().Add(humanWaitBackoff)
		w.logf("%s: %v", work.BeadID, err)
	case outReadBack:
		rep.Failed++
		st.until = w.now().Add(readBackBackoff)
		w.logf("%s: %v", work.BeadID, err)
		w.announce(work.BeadID, "read-back", fmt.Sprintf("Landing worker: the push of %s reported success but %s's tip was not the landed commit on read-back (%v). Whether it landed is unknown; not retrying for %s. A human should compare origin/%s with the landings file.", work.Branch, work.Target, err, readBackBackoff, work.Target))
	case outCanceled:
		w.logf("%s: stopped: %v", work.BeadID, err)
	case outNoStored:
		// Nothing was stored to consume, so there is nothing to count, write,
		// wait or hold: no report line either, since a stored-only sweep
		// crosses every ready bead each interval and the ordinary pass owns
		// the bead's turn (gt-ixvxt5).
	default:
		w.infraFailure(work.BeadID, "landing", err, rep)
		w.countCISilence(work, err)
		w.countFailingLanding(work, err)
	}
}

// finishDeferred finishes a deferred landing's record, then reaps its branches,
// refreshes the authors' seats, writes sp-actual and runs the after-landed hook,
// in that order. It runs on BookQueue's goroutine, so any Worker state it must
// write goes through Later to w's own goroutine; a finish it cannot make leaves
// the work in pendingRepair for the next pass (gt-k78e8j, gt-dci9k8).
func (w *Worker) finishDeferred(work land.Work, res land.Result) {
	f, ok := w.Lander.(finisher)
	if !ok {
		w.logf("%s: the lander cannot finish a deferred landing; the next pass repairs its record", work.BeadID)
		w.repairLater(work)
		return
	}
	if err := f.FinishLanding(work, res); err != nil {
		w.logf("%s: finishing the deferred landing record: %v; the next pass repairs it", work.BeadID, err)
		w.repairLater(work)
		return
	}
	w.refreshAuthorSeats(work, w.reapBeadBranches(work.BeadID))
	w.writeSPActual(work.BeadID, res.LandedCommit)
	if w.AfterLanded != nil {
		w.AfterLanded(work)
	}
}

// finishRevert queues a deferred revert landing's RevertLanded for the worker's
// own goroutine. It runs for the new landing whatever its finish returned, and a
// repair pass skips it, so the call happens exactly once. IsRevert reads the
// bead, so the queue goroutine may ask it; RevertLanded writes red-main state
// the pass also writes, so with a queue it goes through Later and runs at the
// next pass. With no queue the steps already run on the worker's goroutine, and
// it runs there directly (gt-dci9k8).
func (w *Worker) finishRevert(work land.Work, res land.Result) {
	if w.Reverts == nil || !w.Reverts.IsRevert(work.BeadID) {
		return
	}
	// RevertLanded takes a context it does not use, and a stored call outlives
	// the pass that queued it, so there is no pass context to hand it.
	run := func() { w.Reverts.RevertLanded(context.Background(), work, res) }
	if w.Books != nil {
		w.Books.Later(run)
		return
	}
	run()
}

// recoverRevertLanded makes the RevertLanded a restart dropped. A deferred
// revert's call waits in the queue for the worker's next pass, and a process
// that stops first leaves it only in the red-main owner's in-flight record on
// disk; the owner answers with the landing it recorded, and the call runs here,
// on the worker's own goroutine, once per run (gt-qxm7y3). A check that could
// not read makes nothing and is asked again on the next pass, since nothing
// else holds the call.
func (w *Worker) recoverRevertLanded(ctx context.Context) {
	if w.revertChecked || w.Reverts == nil {
		return
	}
	work, res, owed, err := w.Reverts.RevertOwed()
	if err != nil {
		w.logf("checking for a revert whose completion a restart dropped: %v; checking again next pass", err)
		return
	}
	w.revertChecked = true
	if owed {
		w.logf("%s: landed before a restart dropped its revert's completion; telling the owner now", work.BeadID)
		w.Reverts.RevertLanded(ctx, work, res)
	}
}

// finishDeferredQueued runs a deferred landing's finishing steps on BookQueue's
// goroutine when a queue is set, so the pass lands the next bead meanwhile; with
// no queue it runs them here, so a deferred result is never dropped (gt-c3vl2u).
// finishRevert follows the finish attempt whatever it returned, and only for a
// new landing: a repair pass made after a failed finish must not call
// RevertLanded a second time (gt-dci9k8).
func (w *Worker) finishDeferredQueued(work land.Work, res land.Result, wasRepair bool) {
	steps := func() {
		w.finishDeferred(work, res)
		if !wasRepair {
			w.finishRevert(work, res)
		}
	}
	if w.Books == nil {
		steps()
		return
	}
	w.Books.Add(work.BeadID, steps)
}

// repairLater puts work in pendingRepair: through Books.Later (the worker's own
// goroutine) when a queue is set, directly when it is nil (gt-k78e8j).
func (w *Worker) repairLater(work land.Work) {
	repair := func() {
		if w.pendingRepair == nil {
			w.pendingRepair = map[string]land.Work{}
		}
		w.pendingRepair[work.BeadID] = work
	}
	if w.Books != nil {
		w.Books.Later(repair)
		return
	}
	repair()
}

// supersededRejection ends a landing whose bead was closed or changed hands
// while the gate ran (land.Rejection.Superseded): nobody waits on the refusal,
// so the pass logs one line, ends the failure run, writes no comment or
// escalation, and gives the bead the human wait (gt-favqpo).
func (w *Worker) supersededRejection(work land.Work, rej *land.Rejection) {
	w.logf("%s: %v; the bead changed during the landing, so no comment and no escalation", work.BeadID, rej.RecordErr)
	w.endFailureRun(work.BeadID)
	w.bead(work.BeadID).until = w.now().Add(humanWaitBackoff)
}

// shouldEscalate reports whether a human refusal raises the operator's
// escalation. A lane that never escalated raises as it always has. A lane
// that escalated raises again once its escalation is closed, and stays silent
// only while the check reads it open; a check that errors or is missing
// raises again and says so every time, since a silenced alarm is the defect
// (gt-dtx3ht, gt-fjzstx, gt-il3out).
func (w *Worker) shouldEscalate(work land.Work, rej *land.Rejection) bool {
	if !rej.NoEscalate {
		return true
	}
	if !rej.PriorEscalated {
		return false
	}
	if w.EscalationOpen == nil {
		w.logf("%s: no check reads the lane's escalations; raising it again", work.BeadID)
		return true
	}
	open, err := w.EscalationOpen(work.BeadID, land.LaneOf(work.Branch))
	if err != nil {
		w.logf("%s: reading the lane's open escalation: %v; raising it again", work.BeadID, err)
		return true
	}
	return !open
}

// escalate calls Escalate in its own goroutine. A panic in it is logged, not
// fatal: the daemon outlives a broken alert path.
func (w *Worker) escalate(beadID, lane, message string) {
	if w.Escalate == nil {
		return
	}
	w.alertAsync(beadID, func() { w.Escalate(beadID, lane, message) })
}

// alertAsync runs one alert call — a raise or a clear — off the pass's
// goroutine, which a slow alert path must never hold up.
func (w *Worker) alertAsync(beadID string, call func()) {
	w.escWG.Add(1)
	go func() {
		defer w.escWG.Done()
		defer func() {
			if r := recover(); r != nil {
				w.logf("%s: escalation panicked: %v", beadID, r)
			}
		}()
		call()
	}()
}

// endFailureRun forgets a bead's run of failures: the bead landed or was
// rejected, so the next failure starts the count again, and the escalation
// the run raised closes. Everything the run accumulated goes — the state is
// dropped whole rather than field by field, so a field added later starts the
// next run at zero like the rest — except the annotations the bead already
// carries, which outlive the run so a refusal is not posted a second time
// (gt-r9pj9).
func (w *Worker) endFailureRun(beadID string) {
	st := w.state[beadID]
	w.closeFailureRun(beadID)
	delete(w.state, beadID)
	if st != nil && len(st.annotated) > 0 {
		w.bead(beadID).annotated = st.annotated
	}
}

// closeFailureRun is endFailureRun for a path that still needs the bead's
// timing: it clears the run's facts and closes its escalation, leaving the
// backoff the caller is about to set.
func (w *Worker) closeFailureRun(beadID string) {
	st := w.state[beadID]
	if st == nil {
		return
	}
	escalated := st.failingEscalated
	st.failures, st.stage, st.lastErr = 0, "", ""
	st.escalated, st.failingEscalated = false, false
	if escalated && w.FailingClear != nil {
		w.alertAsync(beadID, func() { w.FailingClear(beadID) })
	}
}

// countCISilence tracks consecutive candidate-gate silences on one bead and
// escalates once, on reaching CISilenceEscalateAfter. The design gives CI
// silence the same fail-loud park as any infrastructure failure (the first
// silence escalates, naming the candidate gate, and the bead is parked).
func (w *Worker) countCISilence(work land.Work, err error) {
	st := w.bead(work.BeadID)
	if !errors.Is(err, land.ErrCISilence) {
		st.ciSilences = 0
		return
	}
	st.ciSilences++
	limit := w.CISilenceEscalateAfter
	if limit <= 0 {
		limit = DefaultCISilenceEscalateAfter
	}
	if st.ciSilences != limit {
		return
	}
	st.escalated = true
	// The CI-silence alert names no lane: its key stays per bead (gt-il3out).
	w.escalate(work.BeadID, "", fmt.Sprintf("Landing of %s (%s @ %s) has waited on the candidate gate %d times in a row with no verdict (%v). The bead is parked, not retried, until the cause is fixed and the daemon restarts; no rework is needed; look for a down, busy or hung Forgejo runner.",
		work.BeadID, work.Branch, work.Head, st.ciSilences, err))
}

// countFailingLanding escalates once for a bead whose landing has failed
// FailingEscalateAfter times in a row at any stage. A lint timeout and a CI
// silence each escalate on their own count; a bead that already raised one of
// those has raised its escalation for this run, so this rule stays quiet and
// the run raises exactly one (gt-fn9e6.44).
func (w *Worker) countFailingLanding(work land.Work, err error) {
	st := w.bead(work.BeadID)
	if st.escalated {
		return
	}
	limit := w.FailingEscalateAfter
	if limit <= 0 {
		limit = DefaultFailingEscalateAfter
	}
	if st.failures != limit {
		return
	}
	st.escalated = true
	if w.FailingEscalate == nil {
		return
	}
	st.failingEscalated = true
	w.alertAsync(work.BeadID, func() {
		w.FailingEscalate(work.BeadID, fmt.Sprintf(
			"Landing of %s (%s @ %s) FAILED (failure %d in this run) at the %s stage (%v). The bead is parked, not retried, until the cause is fixed and the daemon restarts, or the bead is labeled %s; no rework is needed; read the landing worker's pass log for it.",
			work.BeadID, work.Branch, work.Head, st.failures, st.stage, err, LabelLandingRetry))
	})
}

// failureStage is the stage a landing failure names: the InfraError's own,
// the text after "landing failed at", and fallback for an error carrying
// none.
func failureStage(fallback string, err error) string {
	var ie *land.InfraError
	if errors.As(err, &ie) && ie.Stage != "" {
		return ie.Stage
	}
	return fallback
}

// errorLine is err's first line, bounded: the backoff record and the health
// detail it feeds are one phrase each.
func errorLine(err error) string {
	if err == nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0])
	if r := []rune(line); len(r) > maxErrorLine {
		line = string(r[:maxErrorLine-1]) + "…"
	}
	return line
}

// maxCommentFindings bounds the om findings one rejection comment lists.
const maxCommentFindings = 15

// reworkMessage is the comment a rework rejection leaves on the work bead.
// An om rejection carries om's verdict text and findings in the same
// comment: one comment on the work bead, never a bead per finding.
func reworkMessage(rej *land.Rejection) string {
	return rejectionComment(rej, fmt.Sprintf("Landing rejected (%s): %s. Back to the polecat as rework: %s.",
		rej.Kind, land.NoteField(rej.Reason), ReworkComment))
}

// humanMessage is the comment a rejection left for a human leaves: the same
// verdict word, score, summary and findings a rework rejection gets, with a
// first sentence saying the loop is escalated so a reader of the comments
// sees it is not reworked again (gt-7nidu).
func humanMessage(rej *land.Rejection) string {
	return rejectionComment(rej, fmt.Sprintf("Landing rejected (%s) and escalated to a human, not reworked again: %s.",
		rej.Kind, land.NoteField(rej.Reason)))
}

// maxEscalationFindings caps the findings an escalation carries (gt-yo36hx).
const maxEscalationFindings = 6

// escalationText is the alarm for a rejection left for a human: the lane and
// its findings, so the overseer need not open the branch (gt-yo36hx).
func escalationText(work land.Work, rej *land.Rejection) string {
	text := fmt.Sprintf("Landing of %s (%s @ %s) left for the overseer (%s): %s; lane %s",
		work.BeadID, work.Branch, work.Head, rej.Kind, land.NoteField(rej.Reason), land.NoteField(land.LaneOf(work.Branch)))
	for i, f := range rej.Findings {
		if i == maxEscalationFindings {
			return text + fmt.Sprintf("; %d finding(s) left out", len(rej.Findings)-i)
		}
		loc := f.Path
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		text += fmt.Sprintf("; %s %s %s", land.NoteField(f.Severity), land.NoteField(loc), land.NoteField(f.Title))
	}
	return text
}

// rejectionComment appends an om rejection's verdict word, score, summary and
// findings to the rejection's first sentence.
func rejectionComment(rej *land.Rejection, first string) string {
	var b strings.Builder
	fmt.Fprint(&b, first)
	if rej.Kind == land.RejectReview {
		fmt.Fprintf(&b, "\n\nom verdict: %s, score %.2f", reviewVerdictWord(rej.ReviewVerdict), rej.ReviewScore)
		if s := strings.TrimSpace(rej.ReviewSummary); s != "" {
			fmt.Fprintf(&b, "\n%s", s)
		}
	}
	for i, f := range rej.Findings {
		if i == maxCommentFindings {
			fmt.Fprintf(&b, "\n- ... and %d more (see the MERGE REJECTION block in the notes)", len(rej.Findings)-i)
			break
		}
		loc := f.Path
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		fmt.Fprintf(&b, "\n- [%s] %s %s", land.NoteField(f.Severity), land.NoteField(loc), land.NoteField(f.Title))
	}
	return b.String()
}

// reviewVerdictWord is the verdict the rework comment names. A rejection
// carrying none is om's own refusal, which is what every review rejection was
// before the finding rules could refuse an approve om reached (gt-oi4je).
func reviewVerdictWord(verdict string) string {
	if verdict == "" {
		return land.VerdictRequestChanges
	}
	return verdict
}

// infraFailure fails a bead loud and stops it: a failure that says nothing
// about the work is not retried by backoff, it is announced on the bead at once
// (and escalated by countFailingLanding at the same count) and the bead is
// parked, so someone fixes the cause instead of the worker retrying a
// deterministic failure on a timer (be-4sgb failed six times in a row on a
// missing BEADS_DIR). The park is in memory: a restart, which is how a fix
// rolls out, lets the bead land again. It is held on the bead's state, not on
// its list row, so a comment or a label anyone writes does not lift it
// (gt-5phoiv).
func (w *Worker) infraFailure(id, stage string, err error, rep *Report) {
	rep.Failed++
	st := w.bead(id)
	if !st.until.IsZero() && !w.now().Before(st.until) {
		// The park ran out (only a clock that jumped past it gets here): this
		// failure is a new run and is as loud as the first.
		st.failures, st.ciSilences, st.escalated, st.failingEscalated = 0, 0, false, false
		st.parked = false
		delete(st.annotated, "infra")
	}
	st.failures++
	st.stage = failureStage(stage, err)
	st.lastErr = errorLine(err)
	st.until = w.now().Add(infraParkWait)
	st.parked = true
	w.logf("%s: %s failed, PARKED until the cause is fixed and the daemon restarts, or the bead is labeled %s: %s", id, stage, LabelLandingRetry, st.lastErr)
	if st.failures == infraAnnounceAfter {
		w.announce(id, "infra", fmt.Sprintf("Landing worker: %s FAILED on infrastructure, not on the work (%s: %v). It is parked and will not be retried until the cause is fixed and the daemon restarts, or the bead is labeled %s; no rework needed.", id, stage, err, LabelLandingRetry))
	}
}

// waitForHuman annotates the bead once per distinct reason and re-checks it
// after humanWaitBackoff. The label stays: the bead is still the worker's.
func (w *Worker) waitForHuman(id, key, msg string) {
	w.logf("%s", msg)
	w.bead(id).until = w.now().Add(humanWaitBackoff)
	w.announce(id, key, msg)
}

func (w *Worker) announce(id, key, msg string) {
	w.annotate(id, key, "annotating", msg)
}

// annotate posts msg on id once per key, whatever the pass's outcome was
// repeated from: the bead already carries the annotation, and posting it again
// is a write nothing reads (gt-r9pj9). Every key posted is remembered, not just
// the last, so a bead whose outcome alternates — refused for one reason, then
// another, then the first again — does not re-post each one in turn. A failed
// write is logged as what failed, then queued as an annotation record for the
// drain, and the key is set so neither the queue nor a later pass posts it
// again. With no queue, or a queue that refuses it, the key stays unset and the
// next pass tries again (gt-o021qa).
func (w *Worker) annotate(id, key, what, msg string) {
	st := w.bead(id)
	if st.annotated[key] {
		return
	}
	if err := w.Beads.AddComment(id, msg); err != nil {
		w.logf("%s: %s (key %s): %v", id, what, key, err)
		if w.Records != nil {
			if err := w.Records.Enqueue(land.PendingRecord{Bead: id, Kind: land.RecordKindAnnotation, Key: key, Text: msg, At: w.now()}); err != nil {
				w.logf("%s: %s (key %s): queueing the annotation: %v", id, what, key, err)
				return
			}
		} else {
			return
		}
	}
	if st.annotated == nil {
		st.annotated = map[string]bool{}
	}
	st.annotated[key] = true
	st.wrote = true
}

// writeSPActual writes the landed bead's measured points, sp-actual:N, from the
// polecat time its complete landing record holds (gt-5d9k2f, SP14). It writes
// only an exact measurement: a record with a lower or upper bound, no time, a
// time FromMinutes rejects, or a bead with no sp: estimate is skipped, and each
// skip logs one line naming the reason. A failed read or write is logged and
// changes nothing about the landing or the report.
func (w *Worker) writeSPActual(beadID, landedCommit string) {
	rec, found, err := w.Landings.LatestForBead(beadID)
	if err != nil {
		w.logf("%s: reading the landing record for sp-actual: %v", beadID, err)
		return
	}
	if !found || rec.LandedCommit != landedCommit {
		w.logf("%s: sp-actual skipped: no landing record for this landing", beadID)
		return
	}
	switch {
	case rec.PolecatLowerBound:
		w.logf("%s: sp-actual skipped: polecat time is a lower bound", beadID)
		return
	case rec.PolecatUpperBound:
		w.logf("%s: sp-actual skipped: polecat time is an upper bound", beadID)
		return
	case rec.PolecatSecs == 0 && len(rec.PolecatAttempts) == 0:
		w.logf("%s: sp-actual skipped: polecat time absent", beadID)
		return
	}
	points, err := storypoints.FromMinutes(rec.PolecatSecs / 60)
	if err != nil {
		w.logf("%s: sp-actual skipped: polecat time %g seconds has no points: %v", beadID, rec.PolecatSecs, err)
		return
	}
	want := storypoints.ActualLabel(points)
	issue, err := w.Beads.Show(beadID)
	if err != nil {
		w.logf("%s: reading the bead for sp-actual: %v", beadID, err)
		return
	}
	estimated := false
	var stale []string
	for _, l := range issue.Labels {
		p, err := storypoints.ParseLabel(l)
		if err != nil {
			continue
		}
		switch p.Kind {
		case storypoints.KindEstimate:
			estimated = true
		case storypoints.KindActual:
			if l != want {
				stale = append(stale, l)
			}
		}
	}
	if !estimated {
		w.logf("%s: sp-actual skipped: no sp: estimate label", beadID)
		return
	}
	opts := beads.UpdateOptions{AddLabels: []string{want}, RemoveLabels: stale}
	if err := w.Beads.Update(beadID, opts); err != nil {
		w.logf("%s: writing %s: %v", beadID, want, err)
	}
}

// afterLanding starts the post-landing verdict for a new landing (a command run
// or a pending status wait) and tells the red-main owner, which finishes the
// landing if it was a revert. A deferred landing's revert is told by its
// bookkeeping job instead: RevertLanded writes state the pass also writes, so
// it must not run on the queue goroutine (gt-dci9k8).
func (w *Worker) afterLanding(ctx context.Context, work land.Work, res land.Result) {
	if res.LandedCommit != "" && work.Target == w.WatchTarget {
		w.lastSeen = res.LandedCommit
	}
	if w.PostLand != nil && res.LandedCommit != "" {
		w.PostLand.Trigger(ctx, PostLand{BeadID: work.BeadID, Commit: res.LandedCommit, Target: work.Target})
	}
	if w.Reverts != nil && res.Deferred == nil {
		w.Reverts.RevertLanded(ctx, work, res)
	}
}

// watchTarget runs the post-landing command for a WatchTarget tip that no
// landing put there: a direct push bypasses the worker, and without this
// nothing would test it (gt-v4ssj.4.1). There is no work bead to blame, so
// the red-main owner names the commit range and never reverts.
//
// The first watch after a start also resumes a landing's run that a daemon
// restart cut short (gt-gb4ij): when the tip is a recorded landing but the
// last verdict was at another commit, the landing's run starts again.
func (w *Worker) watchTarget(ctx context.Context) {
	if w.WatchTarget == "" || w.PostLand == nil || ctx.Err() != nil {
		return
	}
	tip, err := w.Remote.BranchTip(w.WatchTarget)
	if err != nil || tip == "" {
		w.logf("watching %s for direct pushes: tip %q: %v", w.WatchTarget, tip, err)
		return
	}
	resume := false
	if w.lastSeen == "" {
		w.lastSeen, resume = w.seedLastSeen()
		if w.lastSeen == "" {
			// Nothing tested yet: watch from here rather than run the whole
			// tier on a main nobody changed.
			w.lastSeen = tip
			return
		}
	}
	if tip == w.lastSeen {
		return
	}
	from := w.lastSeen
	w.lastSeen = tip
	if rec, ok := w.landingAt(tip); ok {
		// The worker's own landing: its run was triggered when it landed,
		// unless that was before a restart and no verdict was reached.
		if resume {
			w.logf("%s tip %s (landed by %s) has no post-landing verdict (last at %s; a restart cut its run short); running it again", w.WatchTarget, short(tip), rec.BeadID, short(from))
			w.PostLand.Trigger(ctx, PostLand{BeadID: rec.BeadID, Commit: tip, Target: w.WatchTarget})
		}
		return
	}
	w.logf("%s moved %s..%s without a landing (a direct push); running the post-landing command at %s", w.WatchTarget, short(from), short(tip), short(tip))
	w.PostLand.Trigger(ctx, PostLand{Commit: tip, Target: w.WatchTarget, Direct: true, From: from})
}

// seedLastSeen is the newest commit a post-landing run reached a verdict at
// (fromRun true), else the newest landing on WatchTarget, else "".
func (w *Worker) seedLastSeen() (seen string, fromRun bool) {
	if w.MainState != nil {
		if st, err := w.MainState.Load(); err != nil {
			w.logf("reading the main state: %v", err)
		} else if st.LastRun != "" {
			return st.LastRun, true
		}
	}
	if w.Landings == nil {
		return "", false
	}
	recs, err := w.Landings.Recent(recentRepairWindow)
	if err != nil {
		w.logf("reading the landings file: %v", err)
		return "", false
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Target == w.WatchTarget {
			return recs[i].LandedCommit, false
		}
	}
	return "", false
}

// landingAt is the landings file's record of commit, if it has one: the
// worker's own landing, whose post-landing run it triggered.
func (w *Worker) landingAt(commit string) (land.LandingRecord, bool) {
	if w.Landings == nil {
		return land.LandingRecord{}, false
	}
	recs, err := w.Landings.Recent(recentRepairWindow)
	if err != nil {
		w.logf("reading the landings file: %v", err)
		return land.LandingRecord{}, false
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].LandedCommit == commit {
			return recs[i], true
		}
	}
	return land.LandingRecord{}, false
}

// refreshAuthorSeats asks for the author seat worktrees of a landing to be
// refreshed, one call per distinct polecat: every author the bead's origin
// branches name (a rework is pushed to a new branch by a new seat), plus the
// landing request's worker, the author left when no branch names one. Best
// effort — one seat's failure is logged and the next is still asked, and none
// of it touches the landing.
func (w *Worker) refreshAuthorSeats(work land.Work, branches []string) {
	if w.RefreshAuthorSeat == nil {
		return
	}
	for _, polecat := range authorSeats(branches, work.Worker) {
		if err := w.RefreshAuthorSeat(polecat); err != nil {
			w.logf("%s: refreshing the seat worktree of %s/%s: %v", work.BeadID, w.Rig, polecat, err)
		}
	}
}

// authorSeats names the polecats a landing's seat refresh covers: the
// distinct authors of the bead's origin branches, then the landing request's
// worker, in that order.
func authorSeats(branches []string, landingWorker string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(polecat string) {
		if polecat == "" || seen[polecat] {
			return
		}
		seen[polecat] = true
		out = append(out, polecat)
	}
	for _, branch := range branches {
		add(authorPolecat(branch))
	}
	add(landingWorker)
	return out
}

func (w *Worker) clearIntent(work land.Work) {
	if w.ClearIntent == nil || work.Worker == "" {
		return
	}
	if err := w.ClearIntent(work); err != nil {
		w.logf("%s: clearing %s's submitted intent: %v", work.BeadID, work.Worker, err)
	}
}

// sortByLandingOrder orders the ready queue the way the worker lands it: a P0
// first, then the bead submitted for landing earliest (by land.SubmittedAt, the
// clock the alarms read too), then the lower priority number, then ID. Oldest
// first means a submission waits at most the queue ahead of it; priority only
// breaks a tie in submission time. A P0 leads because it is the one class the
// queue must not make wait (gt-rrwwet).
func sortByLandingOrder(issues []*beads.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a == nil || b == nil {
			return b == nil && a != nil
		}
		if (a.Priority == 0) != (b.Priority == 0) {
			return a.Priority == 0
		}
		as, bs := land.SubmittedAt(a), land.SubmittedAt(b)
		if !as.Equal(bs) {
			return as.Before(bs)
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.ID < b.ID
	})
}

// handledKey names one submission of a bead for the pass's handled set: the
// bead's id and the time of its last READY TO LAND block. A bead the pass tried
// that is still ready with the same block is the same submission and is not
// tried twice; a bead its seat resubmitted carries a newer block, so it is a new
// entry and is picked in landing order (gt-jx9f8g). The block time, not
// land.SubmittedAt, because SubmittedAt falls back to the bead's UpdatedAt,
// which moves with every write to the bead.
func handledKey(issue *beads.Issue) string {
	return issue.ID + "@" + land.ReadyBlockTime(issue.Notes).Format(time.RFC3339Nano)
}

// sweptKey names one stored review the pass has already offered to the sweep:
// the bead and the head the hint named. A bead whose stored-only attempt found
// nothing stays unhandled, so a record the pool replaced for a new head while
// its ordinary turn waits is swept once for that head too (gt-k5s380).
func sweptKey(bead, head string) string {
	return bead + "@" + head
}

// nextReady returns the first bead of a listed ready board whose submission
// this pass has not looked at yet, in landing order, or nil when it has looked
// at every one: the pass takes the next bead from each fresh list the way it
// lands them, so a P0 made ready mid-pass lands next (gt-wgvu6p).
func nextReady(issues []*beads.Issue, handled map[string]bool) *beads.Issue {
	sortByLandingOrder(issues)
	for _, issue := range issues {
		if issue != nil && !handled[handledKey(issue)] {
			return issue
		}
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
