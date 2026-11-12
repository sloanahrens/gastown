package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

var updateLandingGolden = flag.Bool("update-landing-golden", false, "rewrite testdata/landing.golden")

// verdictAt is when the first attempt's om verdict is recorded in the labeling
// tests; a line logged before it is judged on that verdict.
var verdictAt = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

// landingItemAt is the item a landing line for bead, logged at logged, parses to.
func landingItemAt(t *testing.T, a Adapter, bead string, logged time.Time) Item {
	t.Helper()
	raw := `{"ts":"` + logged.Format(time.RFC3339) + `","bead":"` + bead + `","head":"a1a1a1a"}`
	it, err := a.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// wantRiskLabels asserts that lab labels needs_review_now and risk both as
// rejected, by rule, with no source: the landing adapter has one source.
func wantRiskLabels(t *testing.T, lab Labeling, rejected bool, rule string) {
	t.Helper()
	if !lab.Labeled {
		t.Fatalf("labeling = %+v, want labeled by %q", lab, rule)
	}
	if lab.Rule != rule {
		t.Errorf("rule = %q, want %q", lab.Rule, rule)
	}
	want := map[QuestionID]bool{QuestionID(landing.QNeedsReviewNow): rejected, QuestionID(landing.QRisk): rejected}
	if !maps.Equal(lab.Labels, want) {
		t.Errorf("labels = %v, want %v", lab.Labels, want)
	}
	if lab.Source != "" {
		t.Errorf("source = %q, want none", lab.Source)
	}
}

// TestLandingRiskLabeledTrueOnAFirstAttemptRejection labels needs_review_now and
// risk true for a landing logged before a first attempt that rejected the work:
// a request_changes verdict, and an approval the landing refused on its findings.
func TestLandingRiskLabeledTrueOnAFirstAttemptRejection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		row  land.ReviewRecord
	}{
		{
			name: "request_changes",
			row:  review("gt-a", 1, land.VerdictRequestChanges, verdictAt),
		},
		{
			name: "approval the landing rejected",
			row:  land.ReviewRecord{BeadID: "gt-a", Attempt: 1, Verdict: land.VerdictApprove, Rejected: true, At: verdictAt},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &landingAdapter{reviews: &land.ReviewsFile{Path: reviewFile(t, tc.row)}}
			lab, err := a.Label(context.Background(), landingItemAt(t, a, "gt-a", verdictAt.Add(-time.Hour)))
			if err != nil {
				t.Fatal(err)
			}
			wantRiskLabels(t, lab, true, ruleLandingFirstAttemptRejected)
		})
	}
}

// TestLandingRiskLabeledFalseOnAFirstAttemptApproval labels needs_review_now and
// risk false for a landing logged before a first attempt that approved the work
// and was not rejected. A later attempt's rejection does not change that.
func TestLandingRiskLabeledFalseOnAFirstAttemptApproval(t *testing.T) {
	t.Parallel()
	a := &landingAdapter{reviews: &land.ReviewsFile{Path: reviewFile(t,
		review("gt-a", 1, land.VerdictApprove, verdictAt),
		review("gt-a", 2, land.VerdictRequestChanges, verdictAt.Add(time.Hour)),
	)}}
	lab, err := a.Label(context.Background(), landingItemAt(t, a, "gt-a", verdictAt.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	wantRiskLabels(t, lab, false, ruleLandingFirstAttemptApproved)
}

// TestRiskLabelIgnoresALineLoggedAfterTheVerdict leaves a line unlabeled for
// risk when it was logged at or after the first attempt's verdict: the line
// cannot have been judged on that verdict. The landing and the submission
// adapters both refuse it.
func TestRiskLabelIgnoresALineLoggedAfterTheVerdict(t *testing.T) {
	t.Parallel()
	rows := []land.ReviewRecord{review("gt-a", 1, land.VerdictRequestChanges, verdictAt)}
	for _, logged := range []time.Time{verdictAt, verdictAt.Add(time.Hour)} {
		t.Run("landing at "+logged.Format(time.RFC3339), func(t *testing.T) {
			t.Parallel()
			a := &landingAdapter{reviews: &land.ReviewsFile{Path: reviewFile(t, rows...)}}
			lab, err := a.Label(context.Background(), landingItemAt(t, a, "gt-a", logged))
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled {
				t.Errorf("labeling = %+v, want unlabeled for a line logged at or after the verdict", lab)
			}
		})
		t.Run("submission at "+logged.Format(time.RFC3339), func(t *testing.T) {
			t.Parallel()
			a := &submissionAdapter{reviews: &land.ReviewsFile{Path: reviewFile(t, rows...)}, notes: &fakeLandingNotes{byBead: map[string]string{"gt-a": ""}}}
			lab, err := a.Label(context.Background(), submissionItem(t, a, "gt-a", logged))
			if err != nil {
				t.Fatal(err)
			}
			// The window join may still label will_pass_review from the same row,
			// so the check is on risk alone.
			if _, ok := lab.Labels[QuestionID(submission.QRisk)]; ok {
				t.Errorf("labels = %v, want no risk label for a submission logged at or after the verdict", lab.Labels)
			}
		})
	}
}

// TestLandingRiskUnlabeledWithoutAFirstAttemptVerdict leaves a landing unlabeled
// when the review log holds no first attempt for its bead, or the first attempt
// is neither an approval nor a rejection.
func TestLandingRiskUnlabeledWithoutAFirstAttemptVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rows []land.ReviewRecord
	}{
		{name: "no review row for the bead", rows: nil},
		{name: "only a later attempt", rows: []land.ReviewRecord{review("gt-a", 2, land.VerdictRequestChanges, verdictAt)}},
		{name: "a first attempt for another bead", rows: []land.ReviewRecord{review("gt-b", 1, land.VerdictApprove, verdictAt)}},
		{name: "a first attempt with no approval or rejection", rows: []land.ReviewRecord{review("gt-a", 1, "skipped", verdictAt)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &landingAdapter{reviews: &land.ReviewsFile{Path: reviewFile(t, tc.rows...)}}
			lab, err := a.Label(context.Background(), landingItemAt(t, a, "gt-a", verdictAt.Add(-time.Hour)))
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled {
				t.Errorf("labeling = %+v, want unlabeled", lab)
			}
		})
	}
}

// TestLandingLabelFailsWhenTheReviewLogCannotBeRead fails the label rather than
// reading an unreadable review log as no verdict, which would pass as unlabeled.
func TestLandingLabelFailsWhenTheReviewLogCannotBeRead(t *testing.T) {
	t.Parallel()
	// A directory where the log file should be opens, but its read fails.
	path := filepath.Join(t.TempDir(), land.ReviewsFileName)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &landingAdapter{reviews: &land.ReviewsFile{Path: path}}
	if _, err := a.Label(context.Background(), landingItemAt(t, a, "gt-a", verdictAt)); err == nil {
		t.Error("Label succeeded over an unreadable review log, want an error")
	}
}

// fakeLandingNotes returns scripted notes per bead and records every read.
type fakeLandingNotes struct {
	byBead map[string]string
	err    error
	reads  []string
}

func (f *fakeLandingNotes) Notes(rig, bead string) (string, error) {
	f.reads = append(f.reads, rig+"/"+bead)
	if f.err != nil {
		return "", f.err
	}
	notes, ok := f.byBead[bead]
	if !ok {
		return "", errors.New("no bead " + bead)
	}
	return notes, nil
}

// TestTownNotesCommentsReadsOneQuery decodes bd sql's CSV answer into comments
// by bead, quoted fields included. A bead the issues table holds with no
// comments is an entry with none, so the rules adapter never reads it again; an
// id the table does not hold is absent, and only that one is read on its own.
func TestTownNotesCommentsReadsOneQuery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var calls [][]string
	run := func(_ context.Context, argv []string) (stdout, stderr []byte, err error) {
		calls = append(calls, argv)
		csv := "issue_id,commented,author,text,created_at\n" +
			"gt-a,1,gastown/polecats/furiosa,DONE_CLOSE_SKIPPED: head unchanged since the rejection,2026-10-21T17:02:00.500000Z\n" +
			"gt-a,1,daemon,\"a second comment, with a comma\",2026-10-21T17:03:00.000000Z\n" +
			"gt-b,0,,,\n"
		return []byte(csv), nil, nil
	}
	tn := &townNotes{town: dir, stores: map[string]*beads.Beads{
		"gastown": beads.New(dir, beads.WithRunner(run)),
	}}

	got, err := tn.Comments("gastown", []string{"gt-a", "gt-b", "gt-missing"})
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	sqlCalls := 0
	for _, argv := range calls {
		if slices.Contains(argv, "sql") {
			sqlCalls++
		}
	}
	if sqlCalls != 1 {
		t.Fatalf("bd calls = %v, want one bd sql query", calls)
	}
	if c, ok := got["gt-b"]; !ok || len(c) != 0 {
		t.Errorf("gt-b = %+v, %v; want an empty entry: the issues table holds it", c, ok)
	}
	if _, ok := got["gt-missing"]; ok {
		t.Errorf("gt-missing has an entry, want absent: the issues table does not hold it")
	}
	want := []beads.Comment{
		{IssueID: "gt-a", Author: "gastown/polecats/furiosa",
			Text: "DONE_CLOSE_SKIPPED: head unchanged since the rejection", CreatedAt: "2026-10-21T17:02:00.500000Z"},
		{IssueID: "gt-a", Author: "daemon",
			Text: "a second comment, with a comma", CreatedAt: "2026-10-21T17:03:00.000000Z"},
	}
	if !slices.Equal(got["gt-a"], want) {
		t.Errorf("gt-a's comments = %+v, want %+v", got["gt-a"], want)
	}
}

// mysqlDateFormat renders at the way MySQL's DATE_FORMAT renders format, for
// the specifiers CommentsOf may use. The batch stub answers with it, so the
// batch time is whatever the format CommentsOf really builds would print, not a
// string the test chose. A specifier it does not know fails the test rather
// than printing something the server would not.
func mysqlDateFormat(t *testing.T, at time.Time, format string) string {
	t.Helper()
	at = at.UTC()
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		i++
		if i == len(format) {
			t.Fatalf("DATE_FORMAT format %q ends in a bare %%", format)
		}
		switch format[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", at.Year())
		case 'm':
			fmt.Fprintf(&b, "%02d", int(at.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", at.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", at.Hour())
		case 'i':
			fmt.Fprintf(&b, "%02d", at.Minute())
		case 's':
			fmt.Fprintf(&b, "%02d", at.Second())
		case 'f':
			fmt.Fprintf(&b, "%06d", at.Nanosecond()/1000)
		case '%':
			b.WriteByte('%')
		default:
			t.Fatalf("DATE_FORMAT specifier %%%c is not modelled by mysqlDateFormat", format[i])
		}
	}
	return b.String()
}

// createdAtFormat finds the DATE_FORMAT format the comments query applies to
// created_at, so the stub prints with the format the code under test built.
var createdAtFormat = regexp.MustCompile(`DATE_FORMAT\(c\.created_at, '([^']*)'\)`)

// TestTownNotesCommentsKeepsSubSecondTimes: the time the batch read reports for
// a comment equals the time bd's per-bead comments --json reports for it, for a
// comment with sub-second digits and for one without. A time truncated to whole
// seconds could move a stand-down out of its rule window. The per-bead stub
// prints the instant the way bd does (a Go time marshalled as RFC3339, the shape
// the bdshim comments golden holds); the batch stub prints it with the DATE_FORMAT
// format the query really carries, so dropping the fraction or adding a stray
// character to that format fails here.
func TestTownNotesCommentsKeepsSubSecondTimes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"with microseconds", time.Date(2026, 10, 21, 17, 2, 0, 123456000, time.UTC)},
		{"on a whole second", time.Date(2026, 11, 6, 17, 31, 26, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			perBeadTime, err := json.Marshal(tc.at)
			if err != nil {
				t.Fatalf("marshalling the instant: %v", err)
			}
			run := func(_ context.Context, argv []string) (stdout, stderr []byte, err error) {
				if slices.Contains(argv, "sql") {
					m := createdAtFormat.FindStringSubmatch(argv[len(argv)-1])
					if m == nil {
						t.Errorf("the batch query has no DATE_FORMAT of c.created_at: %q", argv[len(argv)-1])
						return nil, nil, errors.New("unmodelled batch query")
					}
					return []byte("issue_id,commented,author,text,created_at\n" +
						"gt-a,1,someone,DONE_CLOSE_SKIPPED: went ahead," + mysqlDateFormat(t, tc.at, m[1]) + "\n"), nil, nil
				}
				return []byte(`[{"issue_id":"gt-a","author":"someone","text":"DONE_CLOSE_SKIPPED: went ahead","created_at":` + string(perBeadTime) + `}]`), nil, nil
			}
			b := beads.New(dir, beads.WithRunner(run))
			tn := &townNotes{town: dir, stores: map[string]*beads.Beads{"gastown": b}}

			batch, err := tn.Comments("gastown", []string{"gt-a"})
			if err != nil {
				t.Fatalf("Comments: %v", err)
			}
			if len(batch["gt-a"]) != 1 {
				t.Fatalf("batch comments = %+v, want gt-a's one comment", batch)
			}
			perBead, err := b.Comments("gt-a")
			if err != nil {
				t.Fatalf("per-bead Comments: %v", err)
			}
			if len(perBead) != 1 {
				t.Fatalf("per-bead comments = %+v, want gt-a's one comment", perBead)
			}
			got, err := time.Parse(time.RFC3339Nano, batch["gt-a"][0].CreatedAt)
			if err != nil {
				t.Fatalf("parsing the batch time %q: %v", batch["gt-a"][0].CreatedAt, err)
			}
			want, err := time.Parse(time.RFC3339Nano, perBead[0].CreatedAt)
			if err != nil {
				t.Fatalf("parsing the per-bead time %q: %v", perBead[0].CreatedAt, err)
			}
			if !got.Equal(want) {
				t.Errorf("batch time %v (%q), want the per-bead read's %v (%q)", got, batch["gt-a"][0].CreatedAt, want, perBead[0].CreatedAt)
			}
			if !want.Equal(tc.at) {
				t.Errorf("per-bead time %v, want the instant %v the stub was given", want, tc.at)
			}
		})
	}
}

// TestTownNotesCommentsReadsAMissingRigAsAnError: a rig with no beads
// directory fails the read as not found, the way its single-bead reads do, and
// opens no store. An empty comment set would leave every bead of the rig
// unlabeled with a clean pass.
func TestTownNotesCommentsReadsAMissingRigAsAnError(t *testing.T) {
	t.Parallel()
	tn := &townNotes{town: t.TempDir(), stores: map[string]*beads.Beads{}}
	if _, err := tn.Comments("gastown", []string{"gt-a"}); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Comments of a rig with no beads directory = %v, want not found", err)
	}
	if len(tn.stores) != 0 {
		t.Errorf("a rig with no beads directory opened %d store(s)", len(tn.stores))
	}
}

// TestTownNotesCommentsRefusesAnUnexpectedAnswer: an answer the query was not
// written for is an error, never an empty comment set, so no bead can read as
// having no stand-down record because the read was misparsed.
func TestTownNotesCommentsRefusesAnUnexpectedAnswer(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]string{
		"no header":      "",
		"missing column": "issue_id,commented,author,text\n",
		"short row":      "issue_id,commented,author,text,created_at\ngt-a,1,someone,text\n",
		"bad commented":  "issue_id,commented,author,text,created_at\ngt-a,maybe,,,\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tn := &townNotes{town: dir, stores: map[string]*beads.Beads{
				"gastown": beads.New(dir, beads.WithRunner(func(context.Context, []string) ([]byte, []byte, error) {
					return []byte(answer), nil, nil
				})),
			}}
			if _, err := tn.Comments("gastown", []string{"gt-a"}); err == nil {
				t.Errorf("Comments accepted %q", answer)
			}
		})
	}
}

func TestLandingParseReadsTheLogLine(t *testing.T) {
	t.Parallel()
	a := &landingAdapter{}
	raw := []byte(`{"ts":"2026-10-19T09:00:00Z","bead":"gt-a","rig":"gastown","head":"a1a1a1a","landed_at":"2026-10-19T08:59:00Z","answers":{"needs_review_now":{"noul":0.9},"risk":{"score":3,"confidence":0.6}},"question_version":1,"model":"jev-1.13.0"}`)
	it, err := a.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if it.ID != "gt-a" || it.Model != "jev-1.13.0" {
		t.Errorf("item id %q model %q, want gt-a and jev-1.13.0", it.ID, it.Model)
	}
	if it.Refs[refHead] != "a1a1a1a" || it.Refs[refRig] != "gastown" {
		t.Errorf("refs = %v, want head a1a1a1a on gastown", it.Refs)
	}
	if q := it.Answers[QuestionID(landing.QNeedsReviewNow)]; q.Noul == nil || *q.Noul != 0.9 {
		t.Errorf("needs_review_now answer = %+v, want noul 0.9", q)
	}
	if q := it.Answers[QuestionID(landing.QRisk)]; q.Score == nil || *q.Score != 3 {
		t.Errorf("risk answer = %+v, want score 3", q)
	}
}

func TestLandingParseDefaultsTheRigAndRejectsIncompleteLines(t *testing.T) {
	t.Parallel()
	a := &landingAdapter{}
	it, err := a.Parse([]byte(`{"ts":"2026-10-19T09:00:00Z","bead":"gt-a","head":"a1a1a1a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := it.Refs[refRig]; got != landing.RigName {
		t.Errorf("rig = %q, want the landing rig %q for a line without one", got, landing.RigName)
	}
	for _, raw := range []string{
		`not json`,
		`{"ts":"2026-10-19T09:00:00Z","bead":"gt-a"}`,
		`{"ts":"2026-10-19T09:00:00Z","head":"a1a1a1a"}`,
		`{"bead":"gt-a","head":"a1a1a1a"}`,
	} {
		if _, err := a.Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) succeeded, want an error", raw)
		}
	}
}

// landingLog is the fixture log: four landings, and one line that does not parse.
var landingLog = []string{
	`{"ts":"2026-10-19T09:00:00Z","bead":"gt-a","rig":"gastown","head":"a1a1a1a","answers":{"needs_review_now":{"noul":0.9},"touches_town_machinery":{"noul":0.8},"beyond_goal":{"noul":0.1},"tests_cover_change":{"noul":0.7},"risk":{"score":3,"confidence":0.6}},"question_version":1,"model":"jev-1.13.0"}`,
	`{"ts":"2026-10-19T10:00:00Z","bead":"gt-b","rig":"gastown","head":"b2b2b2b","answers":{"needs_review_now":{"noul":0.8},"touches_town_machinery":{"noul":0.9},"beyond_goal":{"noul":0.2},"tests_cover_change":{"noul":0.4},"risk":{"score":2,"confidence":0.5}},"question_version":1,"model":"jev-1.13.0"}`,
	`{"ts":"2026-10-19T11:00:00Z","bead":"gt-c","rig":"gastown","head":"c3c3c3c","answers":{"needs_review_now":{"noul":0.3},"touches_town_machinery":{"noul":0.2},"beyond_goal":{"noul":0.9},"tests_cover_change":{"noul":0.6},"risk":{"score":1,"confidence":0.7}},"question_version":1,"model":"jev-1.13.0"}`,
	`{"ts":"2026-10-19T12:00:00Z","bead":"gt-d","rig":"gastown","head":"d4d4d4d","answers":{"needs_review_now":{"noul":0.5},"touches_town_machinery":{"noul":0.1},"beyond_goal":{"noul":0.3},"tests_cover_change":{"noul":0.8},"risk":{"score":2,"confidence":0.6}},"question_version":1,"model":"jev-1.13.0"}`,
	`not json`,
}

// landingReviews is the fixture review log the landings are labeled from: gt-a
// was approved on its first attempt before it landed, gt-b's first attempt was
// rejected, gt-c has no review, and gt-d's approval came after it landed, so
// gt-d is unlabeled.
func landingReviews() []land.ReviewRecord {
	return []land.ReviewRecord{
		review("gt-a", 1, land.VerdictApprove, time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)),
		review("gt-b", 1, land.VerdictRequestChanges, time.Date(2026, 10, 19, 11, 0, 0, 0, time.UTC)),
		review("gt-d", 1, land.VerdictApprove, time.Date(2026, 10, 19, 11, 30, 0, 0, time.UTC)),
	}
}

func TestLandingBatteryIsRegistered(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a, err := Open(LandingBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	if a.Battery() != LandingBattery {
		t.Errorf("battery = %q, want %q", a.Battery(), LandingBattery)
	}
	if got := a.LogPath(); got != landing.LogPath(town) {
		t.Errorf("log = %q, want the landing log %q", got, landing.LogPath(town))
	}
}

// TestLandingScorecardPrintsPerQuestionBlocks builds the report over the fixture
// log, labeled from the fixture review log, and compares the text with its golden.
func TestLandingScorecardPrintsPerQuestionBlocks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := landing.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(landingLog, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &landingAdapter{
		log:     logPath,
		reviews: &land.ReviewsFile{Path: reviewFile(t, landingReviews()...)},
	}

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 4 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 4 and 1", rep.Items, rep.LinesSkipped)
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	goldenPath := filepath.Join("testdata", "landing.golden")
	if *updateLandingGolden {
		if err := os.MkdirAll("testdata", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-landing-golden):\n%s\nwant:\n%s", goldenPath, got, want)
	}
}
