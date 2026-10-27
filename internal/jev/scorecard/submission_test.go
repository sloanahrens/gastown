package scorecard

import (
	"context"
	"errors"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

var updateSubmissionGolden = flag.Bool("update-submission-golden", false, "rewrite testdata/submission.golden")

// submissionAt is a 2026-10-20 time on the hour and minute, in UTC.
func submissionAt(hour, min int) time.Time {
	return time.Date(2026, 10, 20, hour, min, 0, 0, time.UTC)
}

// submissionItem is the item a submission line for bead, submitted at at, parses to.
func submissionItem(t *testing.T, a Adapter, bead string, at time.Time) Item {
	t.Helper()
	raw := `{"ts":"` + at.Format(time.RFC3339) + `","bead":"` + bead + `","submitted":"` + at.Format(time.RFC3339) + `"}`
	it, err := a.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// reviewFile writes rows to a fresh review log through the real writer and
// returns the file's path.
func reviewFile(t *testing.T, rows ...land.ReviewRecord) string {
	t.Helper()
	// The landings dir must be private: the review log refuses one any other
	// account can write to, and t.TempDir's numbered subdirectory is 0777 less umask.
	dir := filepath.Join(t.TempDir(), "landings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, land.ReviewsFileName)
	f := &land.ReviewsFile{Path: path}
	for _, row := range rows {
		if err := f.Append(row); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func review(bead string, attempt int, verdict string, at time.Time) land.ReviewRecord {
	return land.ReviewRecord{BeadID: bead, Rig: submission.RigName, Attempt: attempt, Verdict: verdict, At: at}
}

func TestSubmissionLabelJoinsTheReviewInItsWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rows    []land.ReviewRecord
		labeled bool
		value   bool
	}{
		{
			name:    "an approve after the submission labels will_pass_review true",
			rows:    []land.ReviewRecord{review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30))},
			labeled: true,
			value:   true,
		},
		{
			name:    "a request_changes after the submission labels it false",
			rows:    []land.ReviewRecord{review("gt-a", 1, land.VerdictRequestChanges, submissionAt(10, 30))},
			labeled: true,
		},
		{
			name:    "a review exactly at the submitted time joins it",
			rows:    []land.ReviewRecord{review("gt-a", 1, land.VerdictApprove, submissionAt(10, 0))},
			labeled: true,
			value:   true,
		},
		{
			name:    "no review row is unlabeled",
			labeled: false,
		},
		{
			name:    "a review before the submission is unlabeled",
			rows:    []land.ReviewRecord{review("gt-a", 1, land.VerdictApprove, submissionAt(9, 30))},
			labeled: false,
		},
		{
			name:    "a review for another bead is unlabeled",
			rows:    []land.ReviewRecord{review("gt-b", 1, land.VerdictApprove, submissionAt(10, 30))},
			labeled: false,
		},
		{
			name:    "a verdict that is neither approve nor request_changes is unlabeled",
			rows:    []land.ReviewRecord{review("gt-a", 1, "waived", submissionAt(10, 30))},
			labeled: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &submissionAdapter{reviews: &land.ReviewsFile{Path: reviewFile(t, tc.rows...)}, notes: &fakeLandingNotes{byBead: map[string]string{"gt-a": ""}}}
			it := submissionItem(t, a, "gt-a", submissionAt(10, 0))
			lab, err := a.Label(context.Background(), it)
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled = %v, want %v (labels %v)", lab.Labeled, tc.labeled, lab.Labels)
			}
			if !tc.labeled {
				return
			}
			want := map[QuestionID]bool{QuestionID(submission.QWillPassReview): tc.value}
			if !maps.Equal(lab.Labels, want) {
				t.Errorf("labels = %v, want %v", lab.Labels, want)
			}
		})
	}
}

// submissionVerdict is what one submission's will_pass_review label must be:
// labeled says whether a review joined it, and value is the label when it did.
type submissionVerdict struct {
	labeled bool
	value   bool
}

func TestSubmissionLabelJoinsEachSubmissionOfABeadToItsOwnReview(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		rows          []land.ReviewRecord
		first, second submissionVerdict
	}{
		{
			name: "each submission takes the review between it and the next",
			rows: []land.ReviewRecord{
				review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30)),
				review("gt-a", 2, land.VerdictRequestChanges, submissionAt(12, 30)),
			},
			first:  submissionVerdict{labeled: true, value: true},
			second: submissionVerdict{labeled: true, value: false},
		},
		{
			name: "a review after the next submission does not join the first",
			rows: []land.ReviewRecord{
				review("gt-a", 2, land.VerdictApprove, submissionAt(13, 30)),
			},
			first:  submissionVerdict{labeled: false},
			second: submissionVerdict{labeled: true, value: true},
		},
		{
			name: "the first review in the window is the one that counts",
			rows: []land.ReviewRecord{
				review("gt-a", 1, land.VerdictRequestChanges, submissionAt(10, 20)),
				review("gt-a", 1, land.VerdictApprove, submissionAt(10, 40)),
			},
			first:  submissionVerdict{labeled: true, value: false},
			second: submissionVerdict{labeled: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &submissionAdapter{reviews: &land.ReviewsFile{Path: reviewFile(t, tc.rows...)}, notes: &fakeLandingNotes{byBead: map[string]string{"gt-a": ""}}}
			first := submissionItem(t, a, "gt-a", submissionAt(10, 0))
			second := submissionItem(t, a, "gt-a", submissionAt(12, 0))
			for i, c := range []struct {
				it   Item
				want submissionVerdict
			}{{first, tc.first}, {second, tc.second}} {
				lab, err := a.Label(context.Background(), c.it)
				if err != nil {
					t.Fatal(err)
				}
				if lab.Labeled != c.want.labeled {
					t.Fatalf("submission %d labeled = %v, want %v", i+1, lab.Labeled, c.want.labeled)
				}
				if !c.want.labeled {
					continue
				}
				if got := lab.Labels[QuestionID(submission.QWillPassReview)]; got != c.want.value {
					t.Errorf("submission %d will_pass_review = %v, want %v", i+1, got, c.want.value)
				}
			}
		})
	}
}

func TestSubmissionLabelTakesTheRejectionForTheReviewAttempt(t *testing.T) {
	t.Parallel()
	attempt1 := land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Reason: "review found defects", Branch: "polecat/x/gt-a+1", Target: "main", MR: "gt-mr1",
		Findings: []land.Finding{{ID: "gt-f1", Severity: "high", Path: "a.go", Line: 3, Title: "The new branch has no test"}},
	})
	attempt2 := land.FormatRejectionNote(land.RejectionNote{
		Attempt: 2, Reason: "review found defects", Branch: "polecat/x/gt-a+1", Target: "main", MR: "gt-mr1",
		Findings: []land.Finding{{ID: "gt-f2", Severity: "medium", Path: "b.go", Line: 9, Title: "Reshapes the daemon beyond the goal"}},
	})
	notes := attempt1 + "\n" + attempt2
	a := &submissionAdapter{
		reviews: &land.ReviewsFile{Path: reviewFile(t, review("gt-a", 2, land.VerdictRequestChanges, submissionAt(10, 30)))},
		notes:   &fakeLandingNotes{byBead: map[string]string{"gt-a": notes}},
	}
	lab, err := a.Label(context.Background(), submissionItem(t, a, "gt-a", submissionAt(10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	// Attempt 2 is the one the review judged, so its beyond-goal finding labels
	// the item and attempt 1's missing-test finding does not.
	want := map[QuestionID]bool{
		QuestionID(submission.QWillPassReview): false,
		QuestionID(submission.QBeyondGoal):     true,
	}
	if !maps.Equal(lab.Labels, want) {
		t.Errorf("labels = %v, want %v", lab.Labels, want)
	}
	if lab.Rule != ruleSubmissionRequestChanges {
		t.Errorf("rule = %q, want %q", lab.Rule, ruleSubmissionRequestChanges)
	}
}

func TestSubmissionLabelWithoutARejectionBlockLabelsOnlyTheVerdict(t *testing.T) {
	t.Parallel()
	a := &submissionAdapter{
		reviews: &land.ReviewsFile{Path: reviewFile(t, review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30)))},
		notes:   &fakeLandingNotes{byBead: map[string]string{"gt-a": "a note with no rejection block"}},
	}
	lab, err := a.Label(context.Background(), submissionItem(t, a, "gt-a", submissionAt(10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	want := map[QuestionID]bool{QuestionID(submission.QWillPassReview): true}
	if !maps.Equal(lab.Labels, want) {
		t.Errorf("labels = %v, want %v", lab.Labels, want)
	}
}

func TestSubmissionLabelFailsWhenTheNotesCannotBeRead(t *testing.T) {
	t.Parallel()
	a := &submissionAdapter{
		reviews: &land.ReviewsFile{Path: reviewFile(t, review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30)))},
		notes:   &fakeLandingNotes{err: errors.New("bead store unavailable")},
	}
	_, err := a.Label(context.Background(), submissionItem(t, a, "gt-a", submissionAt(10, 0)))
	if err == nil || !strings.Contains(err.Error(), "bead store unavailable") {
		t.Fatalf("Label error = %v, want the store failure", err)
	}
}

// TestSubmissionFindingLabelsReadsBothPhraseLists checks every entry of both
// phrase lists, matched in upper case, and a title that names neither.
func TestSubmissionFindingLabelsReadsBothPhraseLists(t *testing.T) {
	t.Parallel()
	testsFalse := map[QuestionID]bool{QuestionID(submission.QTestsCoverChange): false}
	beyondTrue := map[QuestionID]bool{QuestionID(submission.QBeyondGoal): true}
	cases := []struct {
		name  string
		title string
		want  map[QuestionID]bool
	}{
		{name: "neither list", title: "Variable name is unclear", want: map[QuestionID]bool{}},
		{name: "an empty title", title: "", want: map[QuestionID]bool{}},
	}
	for _, p := range missingTestPhrases {
		cases = append(cases, struct {
			name  string
			title string
			want  map[QuestionID]bool
		}{"missing test: " + p, "Change has " + strings.ToUpper(p) + " here", testsFalse})
	}
	for _, p := range beyondGoalPhrases {
		cases = append(cases, struct {
			name  string
			title string
			want  map[QuestionID]bool
		}{"beyond goal: " + p, "Edits code " + strings.ToUpper(p) + " of this bead", beyondTrue})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := submissionFindingLabels([]land.Finding{{Title: tc.title}})
			if !maps.Equal(got, tc.want) {
				t.Errorf("labels for %q = %v, want %v", tc.title, got, tc.want)
			}
		})
	}
}

// TestSubmissionFindingLabelsMatchesWordsButNotPartsOfWords checks that a
// phrase inside a longer word does not label and that its inflection does.
func TestSubmissionFindingLabelsMatchesWordsButNotPartsOfWords(t *testing.T) {
	t.Parallel()
	for _, title := range []string{
		"Reads the unrequested_fields column",
		"Fixes the piano test helper",
		"Sorts the untesteddata rows",
	} {
		if got := submissionFindingLabels([]land.Finding{{Title: title}}); len(got) != 0 {
			t.Errorf("labels for %q = %v, want none", title, got)
		}
	}
	inflected := []struct {
		title string
		want  map[QuestionID]bool
	}{
		{"Adds no testing for the parser", map[QuestionID]bool{QuestionID(submission.QTestsCoverChange): false}},
		{"The new branch is untested", map[QuestionID]bool{QuestionID(submission.QTestsCoverChange): false}},
		{"Covers the out of scopes table", map[QuestionID]bool{QuestionID(submission.QBeyondGoal): true}},
		{
			"No tests, and out of scope: reshapes the daemon.",
			map[QuestionID]bool{
				QuestionID(submission.QTestsCoverChange): false,
				QuestionID(submission.QBeyondGoal):       true,
			},
		},
	}
	for _, tc := range inflected {
		if got := submissionFindingLabels([]land.Finding{{Title: tc.title}}); !maps.Equal(got, tc.want) {
			t.Errorf("labels for %q = %v, want %v", tc.title, got, tc.want)
		}
	}
}

// TestSubmissionLabelNamesARequestChangesWithNoRejectionBlock checks that a
// request_changes verdict whose block is missing carries its own rule, and an
// approval with no block does not.
func TestSubmissionLabelNamesARequestChangesWithNoRejectionBlock(t *testing.T) {
	t.Parallel()
	otherAttempt := land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Kind: string(land.RejectReview), Reason: "refused", Branch: "b", Target: "main", MR: "gt-a",
	})
	cases := []struct {
		name    string
		verdict string
		attempt int
		notes   string
		rule    string
	}{
		{"no block at all", land.VerdictRequestChanges, 1, "a note with no block", ruleSubmissionRequestChanges + " " + ruleSubmissionNoBlock},
		{"only another attempt's block", land.VerdictRequestChanges, 2, otherAttempt, ruleSubmissionRequestChanges + " " + ruleSubmissionNoBlock},
		{"the block of its own attempt", land.VerdictRequestChanges, 1, otherAttempt, ruleSubmissionRequestChanges},
		{"an approval needs no block", land.VerdictApprove, 2, "a note with no block", ruleSubmissionApprove},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &submissionAdapter{
				reviews: &land.ReviewsFile{Path: reviewFile(t, review("gt-a", tc.attempt, tc.verdict, submissionAt(10, 30)))},
				notes:   &fakeLandingNotes{byBead: map[string]string{"gt-a": tc.notes}},
			}
			lab, err := a.Label(context.Background(), submissionItem(t, a, "gt-a", submissionAt(10, 0)))
			if err != nil {
				t.Fatal(err)
			}
			if lab.Rule != tc.rule {
				t.Errorf("rule = %q, want %q", lab.Rule, tc.rule)
			}
		})
	}
}

// TestSubmissionLabelReadsABeadsNotesOnce checks that two labeled submissions
// of one bead read its notes from the store a single time.
func TestSubmissionLabelReadsABeadsNotesOnce(t *testing.T) {
	t.Parallel()
	notes := &fakeLandingNotes{byBead: map[string]string{"gt-a": "", "gt-b": ""}}
	a := &submissionAdapter{
		reviews: &land.ReviewsFile{Path: reviewFile(t,
			review("gt-a", 1, land.VerdictRequestChanges, submissionAt(10, 30)),
			review("gt-a", 2, land.VerdictApprove, submissionAt(12, 30)),
			review("gt-b", 1, land.VerdictApprove, submissionAt(10, 30)),
		)},
		notes: notes,
	}
	items := []Item{
		submissionItem(t, a, "gt-a", submissionAt(10, 0)),
		submissionItem(t, a, "gt-a", submissionAt(12, 0)),
		submissionItem(t, a, "gt-b", submissionAt(10, 0)),
	}
	for _, it := range items {
		if _, err := a.Label(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{submission.RigName + "/gt-a", submission.RigName + "/gt-b"}
	if !slices.Equal(notes.reads, want) {
		t.Errorf("notes reads = %v, want %v", notes.reads, want)
	}
}

// TestSubmissionContextDropsTheCachedNotes checks that the report boundary
// clears the notes cache, so the next build reads them again.
func TestSubmissionContextDropsTheCachedNotes(t *testing.T) {
	t.Parallel()
	notes := &fakeLandingNotes{byBead: map[string]string{"gt-a": ""}}
	a := &submissionAdapter{
		reviews: &land.ReviewsFile{Path: reviewFile(t, review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30)))},
		notes:   notes,
	}
	for range 2 {
		if _, err := a.Label(context.Background(), submissionItem(t, a, "gt-a", submissionAt(10, 0))); err != nil {
			t.Fatal(err)
		}
		a.Context()
	}
	want := []string{submission.RigName + "/gt-a", submission.RigName + "/gt-a"}
	if !slices.Equal(notes.reads, want) {
		t.Errorf("notes reads = %v, want one per report, %v", notes.reads, want)
	}
}

func TestSubmissionFindingLabelsIsAnUnlabeledQuestionWhenNoTitleNamesIt(t *testing.T) {
	t.Parallel()
	got := submissionFindingLabels([]land.Finding{
		{Title: "Missing test for the parser"},
		{Title: "Unrelated style nit"},
	})
	want := map[QuestionID]bool{QuestionID(submission.QTestsCoverChange): false}
	if !maps.Equal(got, want) {
		t.Errorf("labels = %v, want %v: beyond_goal has no title and must stay unlabeled", got, want)
	}
}

func TestSubmissionRejectionForAttemptPicksTheBlockOfThatAttempt(t *testing.T) {
	t.Parallel()
	notes := land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Reason: "one", Branch: "b", Target: "main", MR: "m",
		Findings: []land.Finding{{ID: "gt-f1", Severity: "low", Path: "a.go", Line: 1, Title: "first"}}}) + "\n" +
		land.FormatRejectionNote(land.RejectionNote{Attempt: 2, Reason: "two", Branch: "b", Target: "main", MR: "m",
			Findings: []land.Finding{{ID: "gt-f2", Severity: "low", Path: "b.go", Line: 2, Title: "second"}}})

	rej, ok := land.RejectionForAttempt(notes, 2)
	if !ok || rej.Attempt != 2 || len(rej.Findings) != 1 || rej.Findings[0].Title != "second" {
		t.Errorf("attempt 2 = %+v, %v; want its own block with the second finding", rej, ok)
	}
	if _, ok := land.RejectionForAttempt(notes, 3); ok {
		t.Error("attempt 3 has no block and was found")
	}
	if _, ok := land.RejectionForAttempt("", 1); ok {
		t.Error("empty notes hold no block and one was found")
	}
}

// TestSubmissionRejectionForAttemptPrefersTheReviewBlock is gt-l3zak: a gate
// block numbers itself among the gates, so it can carry the same count as a
// later review round, and the review block is the one that opened it.
func TestSubmissionRejectionForAttemptPrefersTheReviewBlock(t *testing.T) {
	t.Parallel()
	notes := land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Kind: string(land.RejectReview), Reason: "one",
		Branch: "b", Target: "main", MR: "m"}) + "\n" +
		land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Kind: string(land.RejectGate), Reason: "two",
			Branch: "b", Target: "main", MR: "m"})

	rej, ok := land.RejectionForAttempt(notes, 1)
	if !ok || rej.Kind != string(land.RejectReview) || rej.Reason != "one" {
		t.Errorf("attempt 1 = %+v, %v; want the review block that opened it", rej, ok)
	}
	only := land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Kind: string(land.RejectGate), Reason: "two",
		Branch: "b", Target: "main", MR: "m"})
	if rej, ok := land.RejectionForAttempt(only, 1); !ok || rej.Kind != string(land.RejectGate) {
		t.Errorf("attempt 1 = %+v, %v; want the gate block when no review block carries it", rej, ok)
	}
}

func TestSubmissionParseReadsTheLogLine(t *testing.T) {
	t.Parallel()
	a := &submissionAdapter{}
	raw := []byte(`{"ts":"2026-10-20T10:05:00Z","bead":"gt-a","branch":"polecat/x/gt-a+1","head":"a1a1a1a","target":"main","submitted":"2026-10-20T10:00:00Z","agent":"cheedo","answers":{"will_pass_review":{"noul":0.9,"confidence":0.7},"risk":{"score":2,"confidence":0.6}},"question_version":1,"model":"jev-1.0.0"}`)
	it, err := a.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if it.ID != "gt-a" || it.Model != "jev-1.0.0" || !it.Time.Equal(submissionAt(10, 5)) {
		t.Errorf("item = %+v, want gt-a at 10:05 on jev-1.0.0", it)
	}
	if got := it.Refs[refSubmitted]; got != "2026-10-20T10:00:00Z" {
		t.Errorf("submitted ref = %q, want 10:00", got)
	}
	if q := it.Answers[QuestionID(submission.QWillPassReview)]; q.Noul == nil || *q.Noul != 0.9 {
		t.Errorf("will_pass_review answer = %+v, want noul 0.9", q)
	}
	if q := it.Answers[QuestionID(submission.QRisk)]; q.Score == nil || *q.Score != 2 {
		t.Errorf("risk answer = %+v, want score 2", q)
	}
}

func TestSubmissionParseRejectsIncompleteLines(t *testing.T) {
	t.Parallel()
	a := &submissionAdapter{}
	for _, raw := range []string{
		`not json`,
		`{"ts":"2026-10-20T10:05:00Z","submitted":"2026-10-20T10:00:00Z"}`,
		`{"bead":"gt-a","submitted":"2026-10-20T10:00:00Z"}`,
		`{"ts":"2026-10-20T10:05:00Z","bead":"gt-a"}`,
	} {
		if _, err := a.Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) succeeded, want an error", raw)
		}
	}
}

func TestSubmissionBatteryIsRegistered(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a, err := Open(SubmissionBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	if a.Battery() != SubmissionBattery {
		t.Errorf("battery = %q, want %q", a.Battery(), SubmissionBattery)
	}
	if got := a.LogPath(); got != submission.LogPath(town) {
		t.Errorf("log = %q, want the submission log %q", got, submission.LogPath(town))
	}
	if _, ok := a.(Contextual); !ok {
		t.Error("the submission adapter does not report context")
	}
}

// submissionLog is the fixture submission log: two submissions of gt-b, one of
// gt-a and one of gt-c, and a line that does not parse.
var submissionLog = []string{
	`{"ts":"2026-10-20T10:00:00Z","bead":"gt-a","branch":"polecat/x/gt-a+1","head":"a1a1a1a","target":"main","submitted":"2026-10-20T10:00:00Z","agent":"cheedo","answers":{"tests_cover_change":{"noul":0.8,"confidence":0.7},"beyond_goal":{"noul":0.1,"confidence":0.6},"will_pass_review":{"noul":0.9,"confidence":0.7},"commits_match_change":{"noul":0.9,"confidence":0.8},"touches_town_machinery":{"noul":0.2,"confidence":0.6},"risk":{"score":1,"confidence":0.6}},"question_version":1,"model":"jev-1.0.0"}`,
	`{"ts":"2026-10-20T11:00:00Z","bead":"gt-b","branch":"polecat/y/gt-b+1","head":"b2b2b2b","target":"main","submitted":"2026-10-20T11:00:00Z","agent":"dementus","answers":{"tests_cover_change":{"noul":0.4,"confidence":0.5},"beyond_goal":{"noul":0.7,"confidence":0.6},"will_pass_review":{"noul":0.3,"confidence":0.6},"commits_match_change":{"noul":0.8,"confidence":0.7},"touches_town_machinery":{"noul":0.1,"confidence":0.8},"risk":{"score":2,"confidence":0.5}},"question_version":1,"model":"jev-1.0.0"}`,
	`{"ts":"2026-10-20T12:00:00Z","bead":"gt-c","branch":"polecat/z/gt-c+1","head":"c3c3c3c","target":"main","submitted":"2026-10-20T12:00:00Z","agent":"cheedo","answers":{"tests_cover_change":{"noul":0.6,"confidence":0.6},"beyond_goal":{"noul":0.3,"confidence":0.5},"will_pass_review":{"noul":0.5,"confidence":0.5},"commits_match_change":{"noul":0.7,"confidence":0.6},"touches_town_machinery":{"noul":0.3,"confidence":0.7},"risk":{"score":0,"confidence":0.8}},"question_version":1,"model":"jev-1.0.0"}`,
	`{"ts":"2026-10-20T13:00:00Z","bead":"gt-b","branch":"polecat/y/gt-b+2","head":"b4b4b4b","target":"main","submitted":"2026-10-20T13:00:00Z","agent":"dementus","answers":{"tests_cover_change":{"noul":0.9,"confidence":0.8},"beyond_goal":{"noul":0.1,"confidence":0.7},"will_pass_review":{"noul":0.8,"confidence":0.7},"commits_match_change":{"noul":0.9,"confidence":0.9},"touches_town_machinery":{"noul":0.1,"confidence":0.9},"risk":{"score":1,"confidence":0.7}},"question_version":1,"model":"jev-1.0.0"}`,
	`not json`,
}

// submissionNotesByBead is the fake notes the fixture log is labeled from:
// gt-b's first attempt was rejected for a missing test and for scope beyond the
// goal. gt-a was never rejected and gt-c has no review yet.
var submissionNotesByBead = map[string]string{
	"gt-a": "",
	"gt-b": land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Reason: "review found defects", Branch: "polecat/y/gt-b+1", Target: "main", MR: "gt-mr-b1",
		Findings: []land.Finding{
			{ID: "gt-f1", Severity: "high", Path: "internal/x.go", Line: 12, Title: "No test covers the new branch"},
			{ID: "gt-f2", Severity: "medium", Path: "internal/y.go", Line: 4, Title: "Edits the daemon beyond the goal of this bead"},
		},
	}),
	"gt-c": "",
}

// submissionReviews is the fixture review log: gt-a is approved, gt-b's first
// submission is sent back with two findings and its second is approved, and
// gt-c has no review.
var submissionReviews = []land.ReviewRecord{
	review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30)),
	{BeadID: "gt-b", Rig: submission.RigName, Attempt: 1, Verdict: land.VerdictRequestChanges, Findings: 2, At: submissionAt(11, 30)},
	{BeadID: "gt-b", Rig: submission.RigName, Attempt: 2, Verdict: land.VerdictApprove, At: submissionAt(13, 30)},
}

// TestSubmissionScorecardPrintsPerQuestionBlocks drives the submission adapter
// over the fixture log, review log and notes, and checks the printed scorecard
// against the golden file. The registered factory builds a live beads store, so
// the adapter is built with the fake notes its labeling reads instead.
func TestSubmissionScorecardPrintsPerQuestionBlocks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := submission.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(submissionLog, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &submissionAdapter{
		log:     logPath,
		reviews: &land.ReviewsFile{Path: reviewFile(t, submissionReviews...)},
		notes:   &fakeLandingNotes{byBead: submissionNotesByBead},
	}

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 4 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 4 and 1", rep.Items, rep.LinesSkipped)
	}
	wantContext := "om reviews joined 3 submission(s), carrying 2 finding(s)"
	if len(rep.Context) != 1 || rep.Context[0] != wantContext {
		t.Errorf("context = %q, want %q", rep.Context, wantContext)
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	goldenPath := filepath.Join("testdata", "submission.golden")
	if *updateSubmissionGolden {
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-submission-golden):\n%s\nwant:\n%s", goldenPath, got, want)
	}
}

// submissionItemAtHead is submissionItem for a line that names head.
func submissionItemAtHead(t *testing.T, a Adapter, bead, head string, at time.Time) Item {
	t.Helper()
	raw := `{"ts":"` + at.Format(time.RFC3339) + `","bead":"` + bead + `","head":"` + head +
		`","submitted":"` + at.Format(time.RFC3339) + `"}`
	it, err := a.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// TestSubmissionLabelRulesForTheClaimsBoxesAndOutcome checks which of the four
// new questions each review and bead-note state labels, and that the rule
// names every rule that set a label.
func TestSubmissionLabelRulesForTheClaimsBoxesAndOutcome(t *testing.T) {
	t.Parallel()
	const head = "head-a"
	landed := func(landedHead string) string {
		return land.LandingRecord{
			BeadID: "gt-a", Branch: "polecat/x/gt-a+1", Head: landedHead, Target: "main",
			LandedCommit: "c0ffee", LandedAt: submissionAt(10, 40),
		}.NoteBlock()
	}
	rejection := func(attempt int, kind string, severity string) string {
		n := land.RejectionNote{Attempt: attempt, Kind: kind, Reason: "refused", Branch: "polecat/x/gt-a+1", Target: "main", MR: "gt-a"}
		if severity != "" {
			n.Findings = []land.Finding{{ID: "gt-f1", Severity: severity, Path: "a.go", Line: 3, Title: "Wrong branch"}}
		}
		return land.FormatRejectionNote(n)
	}
	gateAttempt1 := rejection(1, string(land.RejectGate), "")
	conflictAttempt1 := rejection(1, string(land.RejectConflict), "")

	cases := []struct {
		name  string
		rows  []land.ReviewRecord
		notes string
		want  map[string]bool
		rule  string
	}{
		{
			name:  "a first-try landing labels the claims, the boxes and the outcome",
			rows:  []land.ReviewRecord{review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30))},
			notes: landed(head),
			want: map[string]bool{
				submission.QWillPassReview: true, submission.QClaimsVerified: true,
				submission.QBoxesCredible: true, submission.QOutcome: true,
			},
			rule: ruleSubmissionApprove + " " + ruleSubmissionFirstTry,
		},
		{
			name:  "a landing of another head is not this submission's first try",
			rows:  []land.ReviewRecord{review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30))},
			notes: landed("head-other"),
			want:  map[string]bool{submission.QWillPassReview: true},
			rule:  ruleSubmissionApprove,
		},
		{
			name:  "an approval with no landing record is not a first try",
			rows:  []land.ReviewRecord{review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30))},
			notes: "a note with no landing",
			want:  map[string]bool{submission.QWillPassReview: true},
			rule:  ruleSubmissionApprove,
		},
		{
			name:  "a landing on a later attempt is not a first try",
			rows:  []land.ReviewRecord{review("gt-a", 2, land.VerdictApprove, submissionAt(10, 30))},
			notes: gateAttempt1 + "\n" + landed(head),
			want:  map[string]bool{submission.QWillPassReview: true},
			rule:  ruleSubmissionApprove,
		},
		{
			name:  "a gate refusal labels the boxes not credible",
			rows:  []land.ReviewRecord{review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30))},
			notes: gateAttempt1,
			want:  map[string]bool{submission.QWillPassReview: true, submission.QBoxesCredible: false},
			rule:  ruleSubmissionApprove + " " + ruleSubmissionGateRejected,
		},
		{
			name:  "an om refusal on a major labels the claims unverified",
			rows:  []land.ReviewRecord{review("gt-a", 2, land.VerdictRequestChanges, submissionAt(10, 30))},
			notes: rejection(2, string(land.RejectReview), "major"),
			want:  map[string]bool{submission.QWillPassReview: false, submission.QClaimsVerified: false},
			rule:  ruleSubmissionRequestChanges + " " + ruleSubmissionOmMajor,
		},
		{
			name:  "an om refusal on minors alone labels no claim",
			rows:  []land.ReviewRecord{review("gt-a", 2, land.VerdictRequestChanges, submissionAt(10, 30))},
			notes: rejection(2, string(land.RejectReview), "minor"),
			want:  map[string]bool{submission.QWillPassReview: false},
			rule:  ruleSubmissionRequestChanges,
		},
		{
			name:  "a conflict refusal leaves the claims, the boxes and the outcome unlabeled",
			rows:  []land.ReviewRecord{review("gt-a", 1, land.VerdictApprove, submissionAt(10, 30))},
			notes: conflictAttempt1 + "\n" + landed(head),
			want:  map[string]bool{submission.QWillPassReview: true},
			rule:  ruleSubmissionApprove,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &submissionAdapter{
				reviews: &land.ReviewsFile{Path: reviewFile(t, tc.rows...)},
				notes:   &fakeLandingNotes{byBead: map[string]string{"gt-a": tc.notes}},
			}
			lab, err := a.Label(context.Background(), submissionItemAtHead(t, a, "gt-a", head, submissionAt(10, 0)))
			if err != nil {
				t.Fatal(err)
			}
			want := map[QuestionID]bool{}
			for id, v := range tc.want {
				want[QuestionID(id)] = v
			}
			if !maps.Equal(lab.Labels, want) {
				t.Errorf("labels = %v, want %v", lab.Labels, want)
			}
			if lab.Rule != tc.rule {
				t.Errorf("rule = %q, want %q", lab.Rule, tc.rule)
			}
		})
	}
}
