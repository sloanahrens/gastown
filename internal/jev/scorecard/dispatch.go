package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	gtdispatch "github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/jev/stale"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// DispatchBattery is the dispatch battery's name, the --battery value that
// scores the log `gt jev dispatch` writes.
const DispatchBattery = "dispatch"

func init() {
	Register(DispatchBattery, func(townRoot string) Adapter {
		return newDispatchAdapter(townRoot, func() (beadReader, error) { return openRigBeads(townRoot) })
	})
}

// The question ids the dispatch battery logs. They must match the ids in
// internal/jev/dispatch/questions.go, which does not export them.
// TestScorecardDispatchQuestionIDsMatchLogKeys fails when they drift from the
// answer keys the log writes.
const (
	qLandsFirstTry      QuestionID = "lands_first_try"
	qFitsTwentyMinutes  QuestionID = "fits_twenty_minutes"
	qAcceptanceTickable QuestionID = "acceptance_tickable"
	qNeedsStrongModel   QuestionID = "needs_strong_model"
	qTouchesDaemon      QuestionID = "touches_daemon"
	qSize               QuestionID = "size"
)

// sizeLargePoints is the points at which a landing's size reads large. It is
// 34 points on the story-points scale (internal/storypoints/points.go), which
// is over 27.5 minutes of polecat time. The cut follows the scale rather than a
// minutes figure of its own, so Jev, story points and the dashboard agree on
// one definition (gt-eele9q).
const sizeLargePoints = 34

// Verdicts om writes to reviews.jsonl that judged a tree.
const (
	reviewApprove        = "approve"
	reviewRequestChanges = "request_changes"
)

// closedStatus is the bead status a closed bead carries.
const closedStatus = string(beads.StatusClosed)

// needsSplitLabel is the label HandBack puts on a bead it hands back for a
// split (internal/polecat, NeedsSplitLabel).
const needsSplitLabel = "needs-split"

// beadReader reads one bead. *beads.Beads satisfies it.
type beadReader interface {
	Show(id string) (*beads.Issue, error)
}

// overrunRecord is what the overrun guard left in <bead>.json.
type overrunRecord int

const (
	// overrunNone: the bead has no record, or a record carrying only sling
	// bookkeeping and no stop stamp.
	overrunNone overrunRecord = iota
	// overrunPending: a stop that began and has not finished, so the outcome is
	// not known yet.
	overrunPending
	// overrunStopped: a record with stopped_at, so the bead overran.
	overrunStopped
)

// dispatchAdapter scores the dispatch log. Its labels come from four sources:
// the first attempt's om verdict in reviews.jsonl, the overrun guard's record
// per bead, the bead itself (its close reason, labels and status), and the
// bead's latest record in the rig's landings file, which gives its size.
type dispatchAdapter struct {
	town      string
	openBeads func() (beadReader, error)

	beads   beadReader
	reviews map[string]land.ReviewRecord
	// reworked is a bead with a verdict from a rework attempt (attempt 2 or
	// later). A bead in it with no first-attempt verdict has an unknown first try.
	reworked      map[string]bool
	reviewsLoaded bool
	// shown holds the beads Prefetch read in one batch, by id. showBead reads a
	// bead missing from it the one-at-a-time way.
	shown map[string]*beads.Issue
	// latest holds each bead's latest landing record in the rig's landings
	// file, read once by loadLandings. latestErr is that read's failure.
	latest       map[string]landings.Record
	latestLoaded bool
	latestErr    error
}

// Prefetch reads every placed bead in one batch. A scorecard over an empty log
// never opens the store, so it opens nothing here either.
func (a *dispatchAdapter) Prefetch(_ context.Context, items []Item) {
	a.shown = nil
	if len(items) == 0 {
		return
	}
	if a.beads == nil {
		b, err := a.openBeads()
		if err != nil {
			return
		}
		a.beads = b
	}
	shown, err := showAll(a.beads, itemIDs(items))
	if err != nil {
		warnf("reading the placed beads in a batch: %v", err)
	}
	a.shown = shown
}

func newDispatchAdapter(town string, openBeads func() (beadReader, error)) *dispatchAdapter {
	return &dispatchAdapter{town: town, openBeads: openBeads}
}

// openRigBeads opens the rig's beads read-only; the adapter never writes.
func openRigBeads(townRoot string) (beadReader, error) {
	dir := doltserver.FindRigBeadsDir(townRoot, dispatch.RigName)
	if dir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", dispatch.RigName)
	}
	return beads.New(dir), nil
}

func (a *dispatchAdapter) Battery() string { return DispatchBattery }
func (a *dispatchAdapter) LogPath() string { return dispatch.LogPath(a.town) }

// Parse reads one dispatch line. The item is the bead, stamped with its
// placement, and the agent rides beside needs_strong_model as context.
func (a *dispatchAdapter) Parse(raw []byte) (Item, error) {
	var l dispatch.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Bead == "" || strings.ContainsAny(l.Bead, `/\`) || l.AttachedAt.IsZero() {
		return Item{}, errors.New("dispatch line needs a bead id and an attached_at stamp")
	}
	ans := l.Answers
	noul := func(p float64) Answer { return Answer{Noul: &p} }
	size := ans.Size.Score
	item := Item{
		ID:    l.Bead,
		Time:  l.AttachedAt,
		Model: l.Model,
		Answers: map[QuestionID]Answer{
			qFitsTwentyMinutes:  noul(ans.FitsTwentyMinutes),
			qAcceptanceTickable: noul(ans.AcceptanceTickable),
			qNeedsStrongModel:   noul(ans.NeedsStrongModel),
			qTouchesDaemon:      noul(ans.TouchesDaemon),
			qSize:               {Score: &size},
		},
	}
	// Lines logged after lands_first_try left the battery carry no answer for
	// it, so only a line that has one is scored on it.
	if ans.LandsFirstTry != nil {
		item.Answers[qLandsFirstTry] = noul(*ans.LandsFirstTry)
	}
	if l.Agent != "" {
		item.Context = map[QuestionID]string{qNeedsStrongModel: l.Agent}
	}
	return item, nil
}

// Label joins one placement to its label sources. A source that cannot be
// read is an error, so the report fails rather than reading as unlabeled.
func (a *dispatchAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	if !a.reviewsLoaded {
		if err := a.loadReviews(); err != nil {
			return Labeling{}, err
		}
	}
	issue, err := a.showBead(item.ID)
	if err != nil {
		return Labeling{}, err
	}
	overrun, err := readOverrun(a.town, item.ID)
	if err != nil {
		return Labeling{}, err
	}
	var review *land.ReviewRecord
	if r, ok := a.reviews[item.ID]; ok {
		review = &r
	}
	labels, rules := dispatchLabels(review, a.reworked[item.ID], issue, overrun)
	size, sizeRule, sized, err := a.sizeLabel(item.ID)
	if err != nil {
		return Labeling{}, err
	}
	if sized {
		labels[qSize] = size
		rules = append(rules, sizeRule)
	}
	if len(labels) == 0 {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// loadReviews reads the town's om verdicts and keeps each bead's first
// attempt, the verdict the battery asks about. It also marks each bead with a
// rework verdict, so a missing first attempt is not read as a first-try landing.
func (a *dispatchAdapter) loadReviews() error {
	recs, err := land.TownReviewsFile(a.town).Since(time.Time{})
	if err != nil {
		return fmt.Errorf("reading the review log: %w", err)
	}
	a.reviews = map[string]land.ReviewRecord{}
	a.reworked = map[string]bool{}
	for _, r := range recs {
		if r.Attempt > 1 {
			a.reworked[r.BeadID] = true
		}
		if r.Attempt != 1 {
			continue
		}
		if _, seen := a.reviews[r.BeadID]; !seen {
			a.reviews[r.BeadID] = r
		}
	}
	a.reviewsLoaded = true
	return nil
}

// sizeLabel labels a placement's size from the measured points of its latest
// landing record: large at sizeLargePoints or more, small below it. The points
// come from storypoints.Actual, so a bound time, a zero time and a time the
// scale refuses are unlabeled, and so is a crew landing, which has no seat and
// leaves Agent empty. It reports whether it labeled the item, and the rule that
// fired.
func (a *dispatchAdapter) sizeLabel(bead string) (bool, string, bool, error) {
	if err := a.loadLandings(); err != nil {
		return false, "", false, err
	}
	rec, ok := a.latest[bead]
	if !ok || rec.Agent == "" {
		return false, "", false, nil
	}
	points, weighted := storypoints.Actual(rec.PolecatSecs, rec.PolecatLowerBound, rec.PolecatUpperBound)
	if !weighted {
		return false, "", false, nil
	}
	return points >= sizeLargePoints, "landing:points", true, nil
}

// loadLandings reads the rig's landings file once and keeps each bead's latest
// record, the one the landing's sp-actual is read from. A line that is not a
// record is an error, not a skipped line: it could be the bead's own record,
// and a corrupt record must not read as a bead that never landed.
func (a *dispatchAdapter) loadLandings() error {
	if a.latestLoaded {
		return a.latestErr
	}
	a.latestLoaded = true
	a.latest = map[string]landings.Record{}
	a.latestErr = a.readLandings()
	return a.latestErr
}

// readLandings fills latest from the rig's landings file. landings.Path refuses
// only a rig name that could leave the directory, and the rig is a constant, so
// its error shares the read's return rather than getting one of its own.
func (a *dispatchAdapter) readLandings() error {
	path, err := landings.Path(a.town, dispatch.RigName)
	var recs []landings.Record
	var bad []string
	if err == nil {
		recs, bad, err = (&landings.Reader{Path: path}).ReadNew()
	}
	if err != nil {
		return fmt.Errorf("reading the landings file: %w", err)
	}
	if len(bad) > 0 {
		return fmt.Errorf("reading the landings file: %d line(s) are not landing records", len(bad))
	}
	for _, r := range recs {
		a.latest[r.Bead] = r
	}
	return nil
}

// showBead returns the bead, or nil when the store no longer has it. The
// store is opened on the first call, so a scorecard over an empty log never
// touches it.
func (a *dispatchAdapter) showBead(id string) (*beads.Issue, error) {
	// shown holds only beads the batch found, never a nil.
	if issue := a.shown[id]; issue != nil {
		return issue, nil
	}
	if a.beads == nil {
		b, err := a.openBeads()
		if err != nil {
			return nil, err
		}
		a.beads = b
	}
	issue, err := a.beads.Show(id)
	if errors.Is(err, beads.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("showing bead %s: %w", id, err)
	}
	return issue, nil
}

// readOverrun reads the overrun guard's record for bead through the rule
// thrash.go's reader states: a stop in progress is stop_began_at set and
// stopped_at not. A missing record, or one carrying only sling bookkeeping, is
// overrunNone; a stop that began and has not finished is overrunPending; a
// record with stopped_at is overrunStopped. A record that cannot be read is an
// error.
func readOverrun(town, bead string) (overrunRecord, error) {
	stop, err := overrunGuardRecord(town, bead)
	if err != nil {
		return overrunNone, err
	}
	switch stop {
	case stopDone:
		return overrunStopped, nil
	case stopInProgress:
		return overrunPending, nil
	}
	return overrunNone, nil
}

// dispatchLabels applies the join rules to one placement, and names each rule
// that fired, in the order they fired. A question with no rule for the
// placement is left out, so it counts as unlabeled.
//
// lands_first_try: the first attempt's verdict. approve is true unless the
// landing refused it anyway (Rejected), and request_changes is false. With no
// review, a bead closed as landed is true, unless reworked says the bead has a
// verdict from a later attempt, so its first try is unknown.
//
// fits_twenty_minutes: a stopped overrun record, or a bead closed as superseded
// or labeled needs-split, is false. A pending overrun record (stop_began_at set,
// stopped_at not) leaves it unlabeled. With no stop in the record, a bead closed
// as landed is true.
func dispatchLabels(review *land.ReviewRecord, reworked bool, issue *beads.Issue, overrun overrunRecord) (map[QuestionID]bool, []string) {
	labels := map[QuestionID]bool{}
	var rules []string
	landed := isLanded(issue)

	switch {
	case review != nil && review.Verdict == reviewApprove && review.Rejected:
		labels[qLandsFirstTry] = false
		rules = append(rules, "review:refused")
	case review != nil && review.Verdict == reviewApprove:
		labels[qLandsFirstTry] = true
		rules = append(rules, "review:approve")
	case review != nil && review.Verdict == reviewRequestChanges:
		labels[qLandsFirstTry] = false
		rules = append(rules, "review:request-changes")
	case review == nil && landed && !reworked:
		labels[qLandsFirstTry] = true
		rules = append(rules, "landed:no-review")
	}

	switch {
	case overrun == overrunStopped:
		labels[qFitsTwentyMinutes] = false
		rules = append(rules, "overrun:stopped")
	case isSuperseded(issue):
		labels[qFitsTwentyMinutes] = false
		rules = append(rules, "closed:superseded")
	case hasNeedsSplit(issue):
		labels[qFitsTwentyMinutes] = false
		rules = append(rules, "label:needs-split")
	case overrun == overrunPending:
		// A stop that began and has not finished is not an outcome yet.
	case landed:
		labels[qFitsTwentyMinutes] = true
		rules = append(rules, "landed")
	}
	return labels, rules
}

// isLanded is a bead closed by a landing, read through the verdict the stale
// pass reads so both agree on what landed.
// TestScorecardDispatchCloseReasonPredicates pins the reasons it answers to,
// and TestScorecardDispatchLandedMatchesLandingCloseReason fails when the
// landing worker's wording stops reading as one.
func isLanded(issue *beads.Issue) bool {
	if issue == nil || issue.Status != closedStatus {
		return false
	}
	return stale.ClosingVerdict(issue.CloseReason) == stale.VerdictLanded
}

// isSuperseded is a bead closed with a superseded close reason, read through
// the verdict the stale pass reads.
// TestScorecardDispatchCloseReasonPredicates pins the reasons it answers to.
func isSuperseded(issue *beads.Issue) bool {
	if issue == nil {
		return false
	}
	return stale.ClosingVerdict(issue.CloseReason) == stale.VerdictSuperseded
}

// hasNeedsSplit is a bead carrying the needs-split label, typed by hand in any
// case. It reads the dispatcher's HasLabel so the hold and the score agree.
func hasNeedsSplit(issue *beads.Issue) bool {
	return issue != nil && gtdispatch.HasLabel(issue.Labels, needsSplitLabel)
}
