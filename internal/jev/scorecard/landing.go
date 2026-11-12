package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/land"
)

// errNoRigBeads is a rig whose beads directory does not exist. It wraps
// beads.ErrNotFound, so Show reads it as no bead to find, and Label tells it
// from a bead missing in a rig that has a directory.
var errNoRigBeads = fmt.Errorf("no beads directory: %w", beads.ErrNotFound)

// LandingBattery is the landing adapter's name: the --battery value.
const LandingBattery = "landing"

// Refs the landing adapter keeps in Item.Refs.
const (
	refRig  = "rig"
	refHead = "head"
)

// Rules the landing adapter names in the --json report's lines. Each names the
// first attempt's om verdict that set needs_review_now and risk.
const (
	ruleLandingFirstAttemptRejected = "landing:first-attempt-rejected"
	ruleLandingFirstAttemptApproved = "landing:first-attempt-approved"
)

func init() {
	Register(LandingBattery, func(townRoot string) Adapter {
		return newLandingAdapter(townRoot)
	})
}

// newLandingAdapter builds the landing adapter for the town at townRoot. Its
// labels come from the town's om review log.
func newLandingAdapter(townRoot string) *landingAdapter {
	return &landingAdapter{
		log:     landing.LogPath(townRoot),
		reviews: land.TownReviewsFile(townRoot),
	}
}

// landingNotes reads one bead's notes from its rig's beads store.
type landingNotes interface {
	Notes(rig, bead string) (string, error)
}

// townNotes reads each rig through a bd-backed store, one per rig per adapter.
type townNotes struct {
	town   string
	stores map[string]*beads.Beads
}

func (t *townNotes) Notes(rig, bead string) (string, error) {
	issue, err := t.Show(rig, bead)
	if err != nil {
		return "", err
	}
	return issue.Notes, nil
}

// Show reads one bead from its rig's beads store. A rig whose beads directory
// does not exist has no bead to find, so it reads as not found.
func (t *townNotes) Show(rig, bead string) (*beads.Issue, error) {
	b, err := t.store(rig)
	if err != nil {
		return nil, err
	}
	return b.Show(bead)
}

// ShowAll reads several of a rig's beads in as few calls as it can. A rig whose
// store cannot be opened, a bead it does not hold and a failed call all leave
// the bead out of the result, for the caller to read one at a time. The error
// says which batch read failed; the result still holds what was read.
func (t *townNotes) ShowAll(rig string, ids []string) (map[string]*beads.Issue, error) {
	b, err := t.store(rig)
	if err != nil {
		return nil, fmt.Errorf("opening %s's beads: %w", rig, err)
	}
	return showAll(b, ids)
}

// rigShowAller reads several of a rig's beads at once. townNotes is one.
type rigShowAller interface {
	ShowAll(rig string, ids []string) (map[string]*beads.Issue, error)
}

// commentColumns are the columns Comments reads, in the order the query
// selects them.
var commentColumns = []string{"issue_id", "commented", "author", "text", "created_at"}

// Comments reads the comments of several of a rig's beads in one query, and
// returns them by bead. ids is one chunk; the caller keeps it inside
// commentChunk. Every bead the rig's issues table holds has an entry, empty
// when the bead has no comments, so the caller reads none of those again. An id
// the table does not hold (a wisp, say) is absent, and the caller reads that
// one on its own.
func (t *townNotes) Comments(rig string, ids []string) (map[string][]beads.Comment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	b, err := t.store(rig)
	if err != nil {
		return nil, fmt.Errorf("opening %s's beads: %w", rig, err)
	}
	records, err := b.SQLCSV(beadsql.CommentsOf(ids))
	if err != nil {
		return nil, fmt.Errorf("reading %s's comments: %w", rig, err)
	}
	// An answer the read was not written for must not come back as "no
	// comments": every bead would then be read as having no stand-down record.
	if len(records) == 0 {
		return nil, fmt.Errorf("reading %s's comments: no header row", rig)
	}
	if !slices.Equal(records[0], commentColumns) {
		return nil, fmt.Errorf("reading %s's comments: columns %q, want %q", rig, records[0], commentColumns)
	}
	// SQLCSV reads the rows with encoding/csv's default field count, so every
	// row has the header's five fields or ReadAll has already failed.
	comments := make(map[string][]beads.Comment, len(ids))
	for _, row := range records[1:] {
		id := row[0]
		if _, ok := comments[id]; !ok {
			comments[id] = nil
		}
		// The left join answers a bead with no comments with commented 0 and
		// empty comment columns. Any other value is an answer this read was not
		// written for, so it fails rather than pass the bead as silent.
		switch row[1] {
		case "0":
			continue
		case "1":
		default:
			return nil, fmt.Errorf("reading %s's comments: bead %s answered commented %q, want 0 or 1", rig, id, row[1])
		}
		comments[id] = append(comments[id], beads.Comment{
			IssueID:   id,
			Author:    row[2],
			Text:      row[3],
			CreatedAt: row[4],
		})
	}
	return comments, nil
}

// List reads the rig's beads matching opts. A rig with no beads directory fails
// the listing rather than listing none: a rule that reads an empty list would
// label as if it had looked.
func (t *townNotes) List(rig string, opts beads.ListOptions) ([]*beads.Issue, error) {
	b, err := t.store(rig)
	if err != nil {
		return nil, err
	}
	return b.List(opts)
}

// store returns the rig's beads store, opened on first use.
func (t *townNotes) store(rig string) (*beads.Beads, error) {
	if b, ok := t.stores[rig]; ok {
		return b, nil
	}
	dir := doltserver.FindRigBeadsDir(t.town, rig)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("rig %s: %w", rig, errNoRigBeads)
	} else if err != nil {
		return nil, fmt.Errorf("reading the beads directory for rig %s: %w", rig, err)
	}
	b := beads.New(dir)
	t.stores[rig] = b
	return b, nil
}

// landingAdapter labels each logged landing from the om verdict its bead's first
// attempt drew: needs_review_now and risk are both true when that attempt
// rejected the work and both false when it approved it. The landing log writes
// landing.Line, whose answers carry the fields Answer reads; its confidence is
// not scored.
type landingAdapter struct {
	log string
	// reviews is the town's om review log, which Label reads once.
	reviews *land.ReviewsFile
	// rows is the review log grouped by bead, in log order. It is loaded on the
	// first label.
	rows map[string][]land.ReviewRecord
}

func (a *landingAdapter) Battery() string { return LandingBattery }
func (a *landingAdapter) LogPath() string { return a.log }

// landingLine is the part of a landing.Line the scorecard reads.
type landingLine struct {
	TS      time.Time             `json:"ts"`
	Bead    string                `json:"bead"`
	Rig     string                `json:"rig"`
	Head    string                `json:"head"`
	Model   string                `json:"model"`
	Answers map[QuestionID]Answer `json:"answers"`
}

func (a *landingAdapter) Parse(raw []byte) (Item, error) {
	var line landingLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return Item{}, err
	}
	if line.Bead == "" || line.Head == "" || line.TS.IsZero() {
		return Item{}, errors.New("landing line needs a bead, a head and a ts")
	}
	rig := line.Rig
	if rig == "" {
		rig = landing.RigName
	}
	return Item{
		ID:      line.Bead,
		Time:    line.TS,
		Model:   line.Model,
		Answers: line.Answers,
		Refs:    map[string]string{refRig: rig, refHead: line.Head},
	}, nil
}

// Label labels needs_review_now and risk from the om verdict of the bead's
// first attempt, as firstAttemptRejected reads it: a rejection labels both
// true, an approval both false. A landing logged at or after that verdict, a
// bead with no first attempt in the review log and a first attempt that is
// neither an approval nor a rejection are unlabeled. A review log that cannot
// be read fails the report, which would otherwise read every landing as
// unlabeled with a clean exit.
func (a *landingAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	if err := a.loadReviews(); err != nil {
		return Labeling{}, err
	}
	rejected, ok := firstAttemptRejected(a.rows[item.ID], item.Time)
	if !ok {
		return Labeling{}, nil
	}
	rule := ruleLandingFirstAttemptApproved
	if rejected {
		rule = ruleLandingFirstAttemptRejected
	}
	return Labeling{
		Labeled: true,
		Labels: map[QuestionID]bool{
			QuestionID(landing.QNeedsReviewNow): rejected,
			QuestionID(landing.QRisk):           rejected,
		},
		Rule: rule,
	}, nil
}

// loadReviews reads the review log once, grouped by bead.
func (a *landingAdapter) loadReviews() error {
	if a.rows != nil {
		return nil
	}
	rows, err := reviewsByBead(a.reviews)
	if err != nil {
		return err
	}
	a.rows = rows
	return nil
}

// reviewsByBead reads the whole review log, grouped by bead in log order.
func reviewsByBead(reviews *land.ReviewsFile) (map[string][]land.ReviewRecord, error) {
	recs, err := reviews.Since(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("reading the om reviews: %w", err)
	}
	rows := make(map[string][]land.ReviewRecord, len(recs))
	for _, rec := range recs {
		rows[rec.BeadID] = append(rows[rec.BeadID], rec)
	}
	return rows, nil
}

// firstAttemptRejected reports whether the bead's first attempt rejected the
// work, for a line logged at logged. rows are the bead's review records in log
// order. ok is false when the line is unlabeled: the bead has no first attempt,
// the line was logged at or after that verdict (so the line cannot have been
// judged on it), or the verdict is neither an approval nor a rejection.
func firstAttemptRejected(rows []land.ReviewRecord, logged time.Time) (rejected, ok bool) {
	for _, r := range rows {
		if r.Attempt != 1 {
			continue
		}
		if !logged.Before(r.At) {
			return false, false
		}
		if r.IsRejection() {
			return true, true
		}
		if r.Verdict == land.VerdictApprove {
			return false, true
		}
		return false, false
	}
	return false, false
}
