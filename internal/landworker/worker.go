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
)

// Beads is what the worker reads and writes work beads through (ADR 0001:
// bd only). The daemon passes a beads client wrapped in RetryBeads.
type Beads interface {
	land.Beads
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Comments(id string) ([]beads.Comment, error)
	AddComment(id, text string) error
}

// Remote answers the worker's questions about origin: before a landing, where
// a branch points; after one, which polecat branches remain for the bead and
// the hash-guarded way to delete them.
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
	// DeleteRemoteBranchIfAt deletes branch on origin only while it still
	// points at expectedHash.
	DeleteRemoteBranchIfAt(branch, expectedHash string) error
}

// Lander lands one piece of work; *land.Lander is the production one.
type Lander interface {
	Land(ctx context.Context, w land.Work) (land.Result, error)
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

// Defaults for the worker's timing.
const (
	DefaultLandTimeout = 90 * time.Minute
	// recentRepairWindow is how many of the landings file's last records the
	// first pass checks for a landed bead whose record is incomplete.
	recentRepairWindow = 50
	infraBackoffBase   = time.Minute
	infraBackoffMax    = 30 * time.Minute
	// humanWaitBackoff spaces re-checks of a bead only a human can unblock
	// (branch missing, no landing request): cheap to re-check, noisy to log.
	humanWaitBackoff = 15 * time.Minute
	readBackBackoff  = 30 * time.Minute
	// rejectRecordBackoff spaces re-landing a bead whose rejection could not
	// be fully written, so a bd outage does not become a rejection loop.
	rejectRecordBackoff = 10 * time.Minute
	// infraAnnounceAfter is how many consecutive infrastructure failures on
	// one bead earn it a comment saying why it is not landing.
	infraAnnounceAfter = 3
	// DefaultLintTimeoutEscalateAfter is the run of lint-stage timeouts on one
	// bead that earns an escalation. A lint timeout is an infrastructure
	// failure retried every pass, so a hung lint or a stuck lock holder would
	// otherwise idle the bead unseen (gt-j8ade).
	DefaultLintTimeoutEscalateAfter = 3
	// DefaultCISilenceEscalateAfter is the run of candidate-gate silences on
	// one bead that earns an escalation, for the same reason: the worker backs
	// off and retries a runner that is down or hung, and only an escalation
	// says so out loud (gt-fn9e6.5).
	DefaultCISilenceEscalateAfter = 3
	// DefaultFailingEscalateAfter is the run of failures at any landing stage
	// on one bead that earns an escalation: a landing failing repeatedly is
	// otherwise visible only in daemon.log (gt-fn9e6.44).
	DefaultFailingEscalateAfter = 3
	// maxErrorLine bounds the error text a backoff record and a health detail
	// carry, so a failure whose message is a wall of output stays one phrase.
	maxErrorLine = 160
)

// Worker lands one rig's ready work, serially.
type Worker struct {
	Rig      string
	Beads    Beads
	Remote   Remote
	Lander   Lander
	Landings Landings
	// ClearIntent returns the author polecat's seat from submitted to stop
	// once the worker has finished with its bead. nil skips it.
	ClearIntent func(w land.Work) error
	// PostLand, when set, runs the rig's post-landing command after each
	// new landing (never after a record repair).
	PostLand PostLandTrigger
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
	// WatchTarget is the branch the worker watches for commits that reached
	// it without a landing (a direct push); each pass that finds one runs
	// the post-landing command for it. "" turns the watch off. MainState
	// seeds the tip it last saw across restarts.
	WatchTarget string
	MainState   MainStateStore
	// LandTimeout bounds one landing (gate included); 0 means
	// DefaultLandTimeout.
	LandTimeout time.Duration
	// Draining, when set and true, stops the worker from starting anything
	// new: the landing in flight finishes, no further ready bead is claimed
	// and the watch is skipped. The daemon sets it while an upgrade restart
	// is pending, so the restart finds an idle moment (gt-nxvpe).
	Draining func() bool
	// Active is told which bead is being landed (and "" when it ends), so
	// the daemon can say what a pending restart is waiting for.
	Active func(beadID string)
	// Escalate, when set, raises a rejection left for a human (gt:needs-human)
	// to the operator, so it is not waiting unseen on a label: an om review
	// that returned no verdict, a stage timeout, a policy refusal (gt-is0ep).
	// It also raises a bead whose lint stage timed out LintTimeoutEscalateAfter
	// times in a row (gt-j8ade). It runs in its own goroutine, which recovers
	// and logs a panic; the pass does not wait.
	Escalate func(beadID, message string)
	// LintTimeoutEscalateAfter is how many consecutive lint-stage timeouts on
	// one bead raise a single escalation; 0 means DefaultLintTimeoutEscalateAfter.
	LintTimeoutEscalateAfter int
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
	Logf    func(format string, args ...any)
	Now     func() time.Time

	state map[string]*beadState
	// escWG lets a test wait for Escalate goroutines.
	escWG sync.WaitGroup
	// pendingRepair holds landings whose record was left incomplete; the
	// next pass finishes them before landing anything new.
	pendingRepair map[string]land.Work
	startupDone   bool
	// lastSeen is the WatchTarget tip the worker last saw or landed.
	lastSeen string
}

// RevertHooks is the red-main owner's side of an automatic revert.
type RevertHooks interface {
	// RevertLanded is called after every new landing.
	RevertLanded(ctx context.Context, work land.Work, res land.Result)
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
	// lintTimeouts counts consecutive lint-stage timeouts; any other outcome
	// resets it.
	lintTimeouts int
	// ciSilences counts consecutive candidate-gate silences; any other outcome
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

// Pass lands every ready bead once, highest priority first and, within a
// priority, oldest submission first, then checks WatchTarget for a direct
// push, and returns what happened. It stops early only when ctx is done.
func (w *Worker) Pass(ctx context.Context) Report {
	rep := w.landReady(ctx)
	if !w.draining() {
		w.watchTarget(ctx)
	}
	// After the pass, so every next-try time in the snapshot is the one the
	// following pass will act on.
	w.writeBackoff()
	return rep
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

func (w *Worker) landReady(ctx context.Context) Report {
	var rep Report
	if w.pendingRepair == nil {
		w.pendingRepair = map[string]land.Work{}
	}
	if !w.startupDone {
		w.startupDone = true
		w.queueRecentRepairs()
	}
	for id, work := range w.pendingRepair {
		if ctx.Err() != nil {
			return rep
		}
		if w.now().Before(w.bead(id).until) {
			rep.Skipped++
			continue
		}
		w.logf("%s: finishing the record of an earlier landing", id)
		w.landOne(ctx, work, &rep)
	}

	issues, err := w.Beads.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1})
	if err != nil {
		w.logf("listing %s beads: %v", land.LabelReadyToLand, err)
		rep.Failed++
		return rep
	}
	sortByLandingOrder(issues)
	for _, issue := range issues {
		if ctx.Err() != nil {
			return rep
		}
		// Checked before each claim, never mid-landing.
		if w.draining() {
			return rep
		}
		// IsActionable, not IsTerminal: deferring a submitted bead is the
		// operator's only hold on it (gt bead reset refuses a ready bead), so
		// the worker must read the same park the health count reports, or a
		// "0 pending" field sits over a queue that still lands (gt-y7n1u).
		if issue == nil || !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() || !beads.HasLabel(issue, land.LabelReadyToLand) {
			continue
		}
		if _, pending := w.pendingRepair[issue.ID]; pending {
			continue
		}
		if st := w.bead(issue.ID); w.now().Before(st.until) {
			rep.Skipped++
			continue
		}
		w.process(ctx, issue, &rep)
	}
	return rep
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
	for _, rec := range recs {
		// Only the worker's own landings, and only while the bead still
		// waits on them: a bead a human reopened after its landing record
		// was written is theirs, not a record to finish.
		if rec.Route != "" && rec.Route != "daemon" {
			continue
		}
		issue, err := w.Beads.Show(rec.BeadID)
		if err != nil || issue == nil || beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
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

func workFromRecord(rec land.LandingRecord, issue *beads.Issue) land.Work {
	work := land.Work{BeadID: rec.BeadID, Rig: rec.Rig, Branch: rec.Branch, Head: rec.Head, Target: rec.Target}
	if ready, ok := land.ParseReadyNote(issue.Notes); ok {
		work.Worker = ready.Worker
	} else {
		work.Worker = polecatFromAssignee(issue.Assignee)
	}
	return work
}

// process resolves what to land for issue and lands it.
func (w *Worker) process(ctx context.Context, issue *beads.Issue, rep *Report) {
	if w.Active != nil {
		w.Active(issue.ID)
		defer w.Active("")
	}
	// Read from the bead this pass loaded: a landed bead carrying the label
	// arms the install request, and a rejected or skipped one never reaches
	// the outcome that reads it (gt-3qmv4.2).
	w.bead(issue.ID).installNow = beads.HasLabel(issue, LabelInstallNow)

	work, ok := w.resolveWork(issue)
	if !ok {
		w.waitForHuman(issue.ID, "no-request", fmt.Sprintf("Landing worker: %s carries %s but neither a READY TO LAND block nor a \"Submitted for landing: <branch> @ <sha>\" comment says what to land. Leaving the label for a human.", issue.ID, land.LabelReadyToLand))
		rep.Skipped++
		return
	}

	// A landing already on the target with an incomplete record is repaired,
	// never landed twice, whatever the branch holds now.
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
	}
	work.Head = tip
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
// failing that, the latest submission comment.
func (w *Worker) resolveWork(issue *beads.Issue) (land.Work, bool) {
	if work, err := land.WorkFromBead(issue, w.Rig); err == nil {
		return work, true
	}
	comments, err := w.Beads.Comments(issue.ID)
	if err != nil {
		w.logf("%s: reading comments: %v", issue.ID, err)
		return land.Work{}, false
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
		return land.Work{BeadID: issue.ID, Rig: w.Rig, Branch: m[1], Head: m[2], Target: target, Worker: polecatFromAssignee(issue.Assignee)}, true
	}
	return land.Work{}, false
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
	if oc != outInfra {
		st.lintTimeouts = 0
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
		w.refreshAuthorSeats(work, w.reapBeadBranches(work.BeadID))
		w.clearIntent(work)
		if !wasRepair {
			w.afterLanding(ctx, work, res)
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
		if cerr := w.Beads.AddComment(work.BeadID, msg); cerr != nil {
			w.logf("%s: adding the rework comment: %v", work.BeadID, cerr)
		}
		w.clearIntent(work)
	case outRejectedHuman:
		var rej *land.Rejection
		errors.As(err, &rej)
		rep.Rejected++
		w.logf("%s: %v (left for a human: %s)", work.BeadID, err, land.LabelNeedsHuman)
		w.escalate(work.BeadID, fmt.Sprintf("Landing of %s (%s @ %s) left for a human (%s): %s",
			work.BeadID, work.Branch, work.Head, rej.Kind, land.NoteField(rej.Reason)))
		if rej.RecordErr != nil {
			w.closeFailureRun(work.BeadID)
			st.until = w.now().Add(rejectRecordBackoff)
			return
		}
		w.endFailureRun(work.BeadID)
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
		w.logf("%s: %v", work.BeadID, err)
	case outReadBack:
		rep.Failed++
		st.until = w.now().Add(readBackBackoff)
		w.logf("%s: %v", work.BeadID, err)
		w.announce(work.BeadID, "read-back", fmt.Sprintf("Landing worker: the push of %s reported success but %s's tip was not the landed commit on read-back (%v). Whether it landed is unknown; not retrying for %s. A human should compare origin/%s with the landings file.", work.Branch, work.Target, err, readBackBackoff, work.Target))
	case outCanceled:
		w.logf("%s: stopped: %v", work.BeadID, err)
	default:
		w.infraFailure(work.BeadID, "landing", err, rep)
		w.countLintTimeout(work, err)
		w.countCISilence(work, err)
		w.countFailingLanding(work, err)
	}
}

// escalate calls Escalate in its own goroutine. A panic in it is logged, not
// fatal: the daemon outlives a broken alert path.
func (w *Worker) escalate(beadID, message string) {
	if w.Escalate == nil {
		return
	}
	w.alertAsync(beadID, func() { w.Escalate(beadID, message) })
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
// the run raised closes.
func (w *Worker) endFailureRun(beadID string) {
	w.closeFailureRun(beadID)
	delete(w.state, beadID)
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

// countLintTimeout tracks consecutive lint-stage timeouts on one bead and
// escalates once, on reaching LintTimeoutEscalateAfter. The next landing
// outcome that is not a lint timeout clears the count (landing or rejecting
// deletes the bead's state), so a fresh run escalates again.
func (w *Worker) countLintTimeout(work land.Work, err error) {
	st := w.bead(work.BeadID)
	if !errors.Is(err, land.ErrLintTimeout) {
		st.lintTimeouts = 0
		return
	}
	st.lintTimeouts++
	limit := w.LintTimeoutEscalateAfter
	if limit <= 0 {
		limit = DefaultLintTimeoutEscalateAfter
	}
	if st.lintTimeouts != limit {
		return
	}
	st.escalated = true
	w.escalate(work.BeadID, fmt.Sprintf("Landing of %s (%s @ %s) stuck at the lint stage: it timed out %d times in a row (%v). The worker keeps retrying and no rework is needed; look for a hung lint or a golangci-lint lock holder.",
		work.BeadID, work.Branch, work.Head, st.lintTimeouts, err))
}

// countCISilence tracks consecutive candidate-gate silences on one bead and
// escalates once, on reaching CISilenceEscalateAfter. The design gives CI
// silence the infra backoff, and a forced runner outage would otherwise idle
// the bead until the queue is read by hand.
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
	w.escalate(work.BeadID, fmt.Sprintf("Landing of %s (%s @ %s) has waited on the candidate gate %d times in a row with no verdict (%v). The worker keeps retrying with backoff and no rework is needed; look for a down, busy or hung Forgejo runner.",
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
			"Landing of %s (%s @ %s) has failed %d times in a row at the %s stage (%v). The worker keeps retrying with backoff and no rework is needed; read the landing worker's pass log for it.",
			work.BeadID, work.Branch, work.Head, st.failures, st.stage, err))
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

// maxCommentFindings bounds the om findings one rework comment lists.
const maxCommentFindings = 15

// reworkMessage is the comment a rework rejection leaves on the work bead.
// An om rejection carries om's verdict text and findings in the same
// comment: one comment on the work bead, never a bead per finding.
func reworkMessage(rej *land.Rejection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Landing rejected (%s): %s. Back to the polecat as rework: %s.", rej.Kind, land.NoteField(rej.Reason), ReworkComment)
	if rej.Kind == land.RejectReview {
		fmt.Fprintf(&b, "\n\nom verdict: request_changes, score %.2f", rej.ReviewScore)
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

// infraFailure backs a bead off exponentially after a failure that says
// nothing about the work, and says so on the bead once it keeps happening.
func (w *Worker) infraFailure(id, stage string, err error, rep *Report) {
	rep.Failed++
	st := w.bead(id)
	st.failures++
	st.stage = failureStage(stage, err)
	st.lastErr = errorLine(err)
	wait := infraBackoffBase << (st.failures - 1)
	if st.failures > 6 || wait > infraBackoffMax {
		wait = infraBackoffMax
	}
	st.until = w.now().Add(wait)
	w.logf("%s: %s failed (%d in a row, next try in %s): %v", id, stage, st.failures, wait, err)
	if st.failures == infraAnnounceAfter {
		w.announce(id, "infra", fmt.Sprintf("Landing worker: %s has failed %d times in a row on infrastructure, not on the work (last: %s: %v). Still retrying with backoff; no rework needed.", id, st.failures, stage, err))
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
	st := w.bead(id)
	if st.announced == key {
		return
	}
	if err := w.Beads.AddComment(id, msg); err != nil {
		w.logf("%s: annotating: %v", id, err)
		return
	}
	st.announced = key
}

// afterLanding runs the post-landing command for a new landing and tells
// the red-main owner, which finishes the landing if it was a revert.
func (w *Worker) afterLanding(ctx context.Context, work land.Work, res land.Result) {
	if res.LandedCommit != "" && work.Target == w.WatchTarget {
		w.lastSeen = res.LandedCommit
	}
	if w.PostLand != nil && res.LandedCommit != "" {
		w.PostLand.Trigger(ctx, PostLand{BeadID: work.BeadID, Commit: res.LandedCommit, Target: work.Target})
	}
	if w.Reverts != nil {
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

// sortByLandingOrder orders the ready queue the way the worker lands it:
// highest priority first, then the bead submitted for landing earliest (by
// land.SubmittedAt, the clock the alarms read too), then ID. Priority leads
// because a P1 fix pays for every bead landed ahead of it.
func sortByLandingOrder(issues []*beads.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a == nil || b == nil {
			return b == nil && a != nil
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		as, bs := land.SubmittedAt(a), land.SubmittedAt(b)
		if !as.Equal(bs) {
			return as.Before(bs)
		}
		return a.ID < b.ID
	})
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
