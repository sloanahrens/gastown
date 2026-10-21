package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

// SubmissionBattery is the submission adapter's name: the --battery value.
const SubmissionBattery = "submission"

// The phrase lists the submission adapter reads a rejection's findings with.
// Each finding title is matched case-insensitively against the lists, an entry
// matching only as a whole word or its inflection: "no test" matches "no
// testing" and "no tests", but not the "no test" inside "piano test". A title
// that holds a missingTestPhrases entry labels tests_cover_change false, and a
// title that holds a beyondGoalPhrases entry labels beyond_goal true. Every
// entry is checked by TestSubmissionFindingLabelsReadsBothPhraseLists.
var (
	missingTestPhrases = []string{
		"no test",
		"no tests",
		"without a test",
		"without tests",
		"missing test",
		"untested",
		"add a test",
		"add tests",
		"test coverage",
	}
	beyondGoalPhrases = []string{
		"beyond the goal",
		"beyond the bead",
		"beyond what the goal",
		"beyond scope",
		"out of scope",
		"outside the bead",
		"outside the goal",
		"scope creep",
		"not asked for",
		"unrequested",
	}
)

// Rules the submission adapter names in the --json report's lines. The first
// two name the om verdict that set will_pass_review. The rest name the
// rejection or landing that set the other labels, or, for
// submission:no-rejection-block, the missing block a request_changes verdict
// should have left; a line can carry several, space-separated.
const (
	ruleSubmissionApprove        = "submission:om-approve"
	ruleSubmissionRequestChanges = "submission:om-request-changes"
	ruleSubmissionGateRejected   = "submission:gate-rejected"
	ruleSubmissionOmMajor        = "submission:om-major"
	ruleSubmissionFirstTry       = "submission:first-try-landing"
	ruleSubmissionNoBlock        = "submission:no-rejection-block"
)

var (
	missingTestRE = phraseRE(missingTestPhrases)
	beyondGoalRE  = phraseRE(beyondGoalPhrases)
)

// phraseRE compiles phrases into one case-insensitive expression that matches
// any of them at the start of a word, with the inflectional ending its last
// word may take. A phrase therefore matches its plural and its past and
// continuous forms — "no testing" and "no tests" as well as "no test" — but
// never inside a longer word: "piano test" and
// "unrequested_fields" match neither list.
func phraseRE(phrases []string) *regexp.Regexp {
	quoted := make([]string, len(phrases))
	for i, p := range phrases {
		quoted[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)(?:s|es|ed|d|ing)?\b`)
}

// refSubmitted is the Item ref that holds a submission's submitted time.
const refSubmitted = "submitted"

// rejectionHeaderRE finds the header line of each MERGE REJECTION block, so the
// notes can be cut into one block per attempt.
var rejectionHeaderRE = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(land.MergeRejectionNoteMarker) + ` \(attempt [0-9]+\): `)

func init() {
	Register(SubmissionBattery, func(townRoot string) Adapter {
		return &submissionAdapter{
			log:     submission.LogPath(townRoot),
			reviews: land.TownReviewsFile(townRoot),
			notes:   &townNotes{town: townRoot, stores: map[string]*beads.Beads{}},
		}
	})
}

// submissionAdapter labels each logged submission from the om review that
// judged it: the first review row for the bead at or after the logged
// submitted time and before the bead's next submission. A row's approve or
// request_changes verdict sets will_pass_review. The MERGE REJECTION block for
// the row's attempt, read from the bead's notes, sets tests_cover_change and
// beyond_goal from its findings, boxes_credible false when the gate refused it,
// and claims_verified false when om refused it on a major. An approval the
// first attempt landed, with its LANDING RECORD on the bead, sets claims_verified,
// boxes_credible and the outcome choice's positive option.
type submissionAdapter struct {
	log     string
	reviews *land.ReviewsFile
	notes   landingNotes

	// submitted holds each parsed submission's time, by bead. Parse sees every
	// line before the --since window drops any, so a submission's next one is
	// known even when that one falls outside the window.
	submitted map[string][]time.Time
	// rows is the review log, loaded on the first label and kept per bead in
	// file order.
	rows map[string][]land.ReviewRecord
	// beadNotes caches each bead's notes, so a bead with several submissions is
	// read from the beads store once per report.
	beadNotes map[string]string
	// joined and findings tally the labeled reviews for Context.
	joined, findings int
}

func (a *submissionAdapter) Battery() string { return SubmissionBattery }
func (a *submissionAdapter) LogPath() string { return a.log }

// submissionLine is the part of a submission.Line the scorecard reads.
type submissionLine struct {
	TS        time.Time             `json:"ts"`
	Bead      string                `json:"bead"`
	Head      string                `json:"head"`
	Submitted time.Time             `json:"submitted"`
	Model     string                `json:"model"`
	Answers   map[QuestionID]Answer `json:"answers"`
}

func (a *submissionAdapter) Parse(raw []byte) (Item, error) {
	var line submissionLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return Item{}, err
	}
	if line.Bead == "" || line.TS.IsZero() || line.Submitted.IsZero() {
		return Item{}, errors.New("submission line needs a bead, a ts and a submitted time")
	}
	if a.submitted == nil {
		a.submitted = map[string][]time.Time{}
	}
	a.submitted[line.Bead] = append(a.submitted[line.Bead], line.Submitted)
	return Item{
		ID:      line.Bead,
		Time:    line.TS,
		Model:   line.Model,
		Answers: line.Answers,
		Refs: map[string]string{
			refSubmitted: line.Submitted.Format(time.RFC3339Nano),
			refHead:      line.Head,
		},
	}, nil
}

// Label joins item to its om review and labels it from the verdict and the
// rejection block the review's attempt names. No review row for the submission
// leaves it unlabeled, and so does a row whose verdict is neither approve nor
// request_changes. An error reading the review log or the bead's notes fails
// the whole report.
func (a *submissionAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	submitted, err := time.Parse(time.RFC3339Nano, item.Refs[refSubmitted])
	if err != nil {
		return Labeling{}, fmt.Errorf("reading %s's submitted time: %w", item.ID, err)
	}
	if err := a.loadReviews(); err != nil {
		return Labeling{}, err
	}
	row, ok := a.joinReview(item.ID, submitted)
	if !ok {
		return Labeling{}, nil
	}

	labels := map[QuestionID]bool{}
	var rules []string
	switch row.Verdict {
	case land.VerdictApprove:
		labels[QuestionID(submission.QWillPassReview)] = true
		rules = append(rules, ruleSubmissionApprove)
	case land.VerdictRequestChanges:
		labels[QuestionID(submission.QWillPassReview)] = false
		rules = append(rules, ruleSubmissionRequestChanges)
	default:
		return Labeling{}, nil
	}
	a.joined++
	a.findings += row.Findings

	notes, err := a.notesFor(item.ID)
	if err != nil {
		return Labeling{}, err
	}
	if rej, ok := rejectionForAttempt(notes, row.Attempt); ok {
		for id, v := range submissionFindingLabels(rej.Findings) {
			labels[id] = v
		}
		// A conflict refusal says nothing about the claims or the boxes, so it
		// leaves them unlabeled.
		switch rej.Kind {
		case string(land.RejectGate):
			labels[QuestionID(submission.QBoxesCredible)] = false
			rules = append(rules, ruleSubmissionGateRejected)
		case string(land.RejectReview):
			if land.BlocksLanding(rej.Findings) {
				labels[QuestionID(submission.QClaimsVerified)] = false
				rules = append(rules, ruleSubmissionOmMajor)
			}
		}
	} else {
		switch row.Verdict {
		case land.VerdictRequestChanges:
			// A request_changes verdict leaves a block on its attempt. Without
			// one the findings could not be read, which is not the same as none.
			rules = append(rules, ruleSubmissionNoBlock)
		case land.VerdictApprove:
			if row.Attempt == 1 && landedAs(notes, item.Refs[refHead]) {
				labels[QuestionID(submission.QClaimsVerified)] = true
				labels[QuestionID(submission.QBoxesCredible)] = true
				labels[QuestionID(submission.QOutcome)] = true
				rules = append(rules, ruleSubmissionFirstTry)
			}
		}
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, " ")}, nil
}

// Prefetch reads the notes of every submitted bead in one call, so notesFor
// finds them cached. Context drops them with the rest of the build's state.
func (a *submissionAdapter) Prefetch(_ context.Context, items []Item) {
	all, ok := a.notes.(rigShowAller)
	if !ok {
		return
	}
	for id, issue := range all.ShowAll(submission.RigName, itemIDs(items)) {
		if a.beadNotes == nil {
			a.beadNotes = map[string]string{}
		}
		a.beadNotes[id] = issue.Notes
	}
}

// notesFor reads bead's notes once and keeps them for the bead's other
// submissions.
func (a *submissionAdapter) notesFor(bead string) (string, error) {
	if notes, ok := a.beadNotes[bead]; ok {
		return notes, nil
	}
	notes, err := a.notes.Notes(submission.RigName, bead)
	if err != nil {
		return "", fmt.Errorf("reading %s's notes: %w", bead, err)
	}
	if a.beadNotes == nil {
		a.beadNotes = map[string]string{}
	}
	a.beadNotes[bead] = notes
	return notes, nil
}

// landedAs reports whether notes carry the LANDING RECORD of head, the newest
// record naming head and a landed commit. A submission nothing landed has no
// record for its head.
func landedAs(notes, head string) bool {
	rec, ok := land.ParseLandingRecord(notes)
	return ok && head != "" && rec.Head == head
}

// loadReviews reads the whole review log once, grouped by bead.
func (a *submissionAdapter) loadReviews() error {
	if a.rows != nil {
		return nil
	}
	recs, err := a.reviews.Since(time.Time{})
	if err != nil {
		return fmt.Errorf("reading the om reviews: %w", err)
	}
	rows := make(map[string][]land.ReviewRecord, len(recs))
	for _, rec := range recs {
		rows[rec.BeadID] = append(rows[rec.BeadID], rec)
	}
	a.rows = rows
	return nil
}

// joinReview returns the first review row for bead that is at or after
// submitted and before the bead's next submission. ok is false when none is.
func (a *submissionAdapter) joinReview(bead string, submitted time.Time) (land.ReviewRecord, bool) {
	next, hasNext := a.nextSubmission(bead, submitted)
	for _, row := range a.rows[bead] {
		if row.At.Before(submitted) || (hasNext && !row.At.Before(next)) {
			continue
		}
		return row, true
	}
	return land.ReviewRecord{}, false
}

// nextSubmission is the earliest submission of bead after the one at after.
func (a *submissionAdapter) nextSubmission(bead string, after time.Time) (time.Time, bool) {
	var next time.Time
	found := false
	for _, t := range a.submitted[bead] {
		if !t.After(after) {
			continue
		}
		if !found || t.Before(next) {
			next, found = t, true
		}
	}
	return next, found
}

// rejectionForAttempt reads the MERGE REJECTION block that opened attempt in
// notes. land's parser reads the last block of the text it is given, so each
// block is cut out up to the next header before it is parsed.
func rejectionForAttempt(notes string, attempt int) (land.RejectionNote, bool) {
	starts := rejectionHeaderRE.FindAllStringIndex(notes, -1)
	var found land.RejectionNote
	ok := false
	for i, s := range starts {
		end := len(notes)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		rej, parsed := land.ParseRejectionNote(notes[s[0]:end])
		if parsed && rej.Attempt == attempt {
			found, ok = rej, true
		}
	}
	return found, ok
}

// submissionFindingLabels labels tests_cover_change and beyond_goal from the
// titles of one rejection's findings. A question no title names is absent from
// the result, so it stays unlabeled.
func submissionFindingLabels(findings []land.Finding) map[QuestionID]bool {
	labels := map[QuestionID]bool{}
	for _, f := range findings {
		if missingTestRE.MatchString(f.Title) {
			labels[QuestionID(submission.QTestsCoverChange)] = false
		}
		if beyondGoalRE.MatchString(f.Title) {
			labels[QuestionID(submission.QBeyondGoal)] = true
		}
	}
	return labels
}

// Context reports how many labeled reviews the submissions joined and the
// findings those reviews carried, and clears the tally and the notes cached for
// it so the next build starts fresh.
func (a *submissionAdapter) Context() []string {
	joined, findings := a.joined, a.findings
	a.joined, a.findings = 0, 0
	a.beadNotes = nil
	if joined == 0 {
		return nil
	}
	return []string{fmt.Sprintf("om reviews joined %d submission(s), carrying %d finding(s)", joined, findings)}
}
