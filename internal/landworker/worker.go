package landworker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
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

// Remote answers the two questions the worker asks origin before a landing.
type Remote interface {
	// BranchTip returns origin's tip of branch, or "" when origin has no
	// such branch.
	BranchTip(branch string) (string, error)
	// Contains fetches target and reports whether commit is reachable from
	// origin/<target>.
	Contains(target, commit string) (bool, error)
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
	Logf   func(format string, args ...any)
	Now    func() time.Time

	state map[string]*beadState
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
}

// Report counts one pass's outcomes.
type Report struct {
	Landed, Repaired, Rejected, Skipped, Failed int
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

// Pass lands every ready bead once, oldest first, then checks WatchTarget for
// a direct push, and returns what happened. It stops early only when ctx is
// done.
func (w *Worker) Pass(ctx context.Context) Report {
	rep := w.landReady(ctx)
	if !w.draining() {
		w.watchTarget(ctx)
	}
	return rep
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
	sortOldestFirst(issues)
	for _, issue := range issues {
		if ctx.Err() != nil {
			return rep
		}
		// Checked before each claim, never mid-landing.
		if w.draining() {
			return rep
		}
		if issue == nil || beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() || !beads.HasLabel(issue, land.LabelReadyToLand) {
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
	switch classify(ctx, err) {
	case outLanded:
		delete(w.pendingRepair, work.BeadID)
		delete(w.state, work.BeadID)
		if wasRepair {
			rep.Repaired++
		} else {
			rep.Landed++
		}
		w.logf("%s: landed %s on %s (patch-id %s)", work.BeadID, short(res.LandedCommit), work.Target, short(res.PatchID))
		w.clearIntent(work)
		if !wasRepair {
			w.afterLanding(ctx, work, res)
		}
	case outRecordIncomplete:
		// Landed: the push was read back. Only the record is unfinished.
		w.pendingRepair[work.BeadID] = work
		rep.Landed++
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
			st.until = w.now().Add(rejectRecordBackoff)
			return
		}
		delete(w.state, work.BeadID)
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
		if rej.RecordErr != nil {
			st.until = w.now().Add(rejectRecordBackoff)
			return
		}
		delete(w.state, work.BeadID)
		w.clearIntent(work)
	case outRace:
		// Target moved during the gate: nothing written; land again next pass.
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
	}
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

func (w *Worker) clearIntent(work land.Work) {
	if w.ClearIntent == nil || work.Worker == "" {
		return
	}
	if err := w.ClearIntent(work); err != nil {
		w.logf("%s: clearing %s's submitted intent: %v", work.BeadID, work.Worker, err)
	}
}

// sortOldestFirst orders beads by last update, then ID, so the bead that has
// waited longest lands first.
func sortOldestFirst(issues []*beads.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a == nil || b == nil {
			return b == nil && a != nil
		}
		if a.UpdatedAt != b.UpdatedAt {
			return a.UpdatedAt < b.UpdatedAt
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
