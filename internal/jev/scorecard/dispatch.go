package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/land"
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
const (
	qLandsFirstTry      QuestionID = "lands_first_try"
	qFitsTwentyMinutes  QuestionID = "fits_twenty_minutes"
	qAcceptanceTickable QuestionID = "acceptance_tickable"
	qNeedsStrongModel   QuestionID = "needs_strong_model"
	qTouchesDaemon      QuestionID = "touches_daemon"
	qSize               QuestionID = "size"
)

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
	// overrunNone: the bead has no record.
	overrunNone overrunRecord = iota
	// overrunPending: a record whose stop has not finished, so the outcome is
	// not known yet.
	overrunPending
	// overrunStopped: a record with stopped_at, so the bead overran.
	overrunStopped
)

// dispatchAdapter scores the dispatch log. Its labels come from three sources:
// the first attempt's om verdict in reviews.jsonl, the overrun guard's record
// per bead, and the bead itself (its close reason, labels and status).
type dispatchAdapter struct {
	town      string
	openBeads func() (beadReader, error)

	beads         beadReader
	reviews       map[string]land.ReviewRecord
	reviewsLoaded bool
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
			qLandsFirstTry:      noul(ans.LandsFirstTry),
			qFitsTwentyMinutes:  noul(ans.FitsTwentyMinutes),
			qAcceptanceTickable: noul(ans.AcceptanceTickable),
			qNeedsStrongModel:   noul(ans.NeedsStrongModel),
			qTouchesDaemon:      noul(ans.TouchesDaemon),
			qSize:               {Score: &size},
		},
	}
	if l.Agent != "" {
		item.Context = map[QuestionID]string{qNeedsStrongModel: l.Agent}
	}
	return item, nil
}

// Label joins one placement to its three label sources. A source that cannot
// be read is an error, so the report fails rather than reading as unlabeled.
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
	labels, rules := dispatchLabels(review, issue, overrun)
	if len(labels) == 0 {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// loadReviews reads the town's om verdicts and keeps each bead's first
// attempt, the verdict the battery asks about.
func (a *dispatchAdapter) loadReviews() error {
	recs, err := land.TownReviewsFile(a.town).Since(time.Time{})
	if err != nil {
		return fmt.Errorf("reading the review log: %w", err)
	}
	a.reviews = map[string]land.ReviewRecord{}
	for _, r := range recs {
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

// showBead returns the bead, or nil when the store no longer has it. The
// store is opened on the first call, so a scorecard over an empty log never
// touches it.
func (a *dispatchAdapter) showBead(id string) (*beads.Issue, error) {
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

// readOverrun reads the overrun guard's record for bead. A missing record is
// overrunNone; a record that cannot be read is an error.
func readOverrun(town, bead string) (overrunRecord, error) {
	path := filepath.Join(constants.TownRuntimePath(town), "overrun-guard", bead+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return overrunNone, nil
	}
	if err != nil {
		return overrunNone, fmt.Errorf("reading the overrun record for %s: %w", bead, err)
	}
	var st struct {
		StoppedAt time.Time `json:"stopped_at"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return overrunNone, fmt.Errorf("parsing the overrun record for %s: %w", bead, err)
	}
	if st.StoppedAt.IsZero() {
		return overrunPending, nil
	}
	return overrunStopped, nil
}

// dispatchLabels applies the join rules to one placement, and names each rule
// that fired, in the order they fired. A question with no rule for the
// placement is left out, so it counts as unlabeled.
//
// lands_first_try: the first attempt's verdict. approve is true unless the
// landing refused it anyway (Rejected), and request_changes is false. With no
// review, a bead closed as landed is true.
//
// fits_twenty_minutes: a stopped overrun record, or a bead closed as superseded
// or labeled needs-split, is false. A pending overrun record leaves it
// unlabeled. With neither, a bead closed as landed is true.
func dispatchLabels(review *land.ReviewRecord, issue *beads.Issue, overrun overrunRecord) (map[QuestionID]bool, []string) {
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
	case review == nil && landed:
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
	case landed:
		labels[qFitsTwentyMinutes] = true
		rules = append(rules, "landed")
	}
	return labels, rules
}

// isLanded is a bead closed by a landing: the landing worker's close carries
// the landed commit in its reason.
func isLanded(issue *beads.Issue) bool {
	if issue == nil || issue.Status != closedStatus {
		return false
	}
	reason := strings.TrimSpace(issue.CloseReason)
	return strings.HasPrefix(reason, "Landed on") || strings.Contains(reason, "landed_commit:")
}

// isSuperseded is a bead closed with a superseded close reason.
func isSuperseded(issue *beads.Issue) bool {
	if issue == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(issue.CloseReason)), "superseded")
}

// hasNeedsSplit is a bead carrying the needs-split label.
func hasNeedsSplit(issue *beads.Issue) bool {
	return issue != nil && beads.HasLabel(issue, needsSplitLabel)
}
