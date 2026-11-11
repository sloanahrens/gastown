package scorecard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
)

var updateDispatchGolden = flag.Bool("update-dispatch-scorecard-golden", false, "rewrite testdata/dispatch_scorecard.golden")

// dispatchBeadStore is a bead store that holds only the beads a test puts in it.
type dispatchBeadStore map[string]*beads.Issue

func (f dispatchBeadStore) Show(id string) (*beads.Issue, error) {
	if is, ok := f[id]; ok {
		return is, nil
	}
	return nil, beads.ErrNotFound
}

func dispatchAnswers(p float64) dispatch.Answers {
	return dispatch.Answers{
		LandsFirstTry:      &p,
		FitsTwentyMinutes:  p,
		AcceptanceTickable: p,
		NeedsStrongModel:   p,
		TouchesDaemon:      p,
		Size:               dispatch.SizeAnswer{Score: 1, Confidence: 0.8},
	}
}

// writeDispatchFixture writes the dispatch log, the review log and the overrun
// records under town. Each line is built from the dispatch package's own
// Line, so the fixture cannot drift from the writer's field names.
func writeDispatchFixture(t *testing.T, town string, lines []dispatch.Line, rawLines []string, reviews []land.ReviewRecord, overruns map[string]string) {
	t.Helper()
	logLines := make([]string, 0, len(lines)+len(rawLines))
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		logLines = append(logLines, string(data))
	}
	logLines = append(logLines, rawLines...)
	writeFile(t, dispatch.LogPath(town), strings.Join(logLines, "\n")+"\n")

	var reviewLines []string
	for _, r := range reviews {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		reviewLines = append(reviewLines, string(data))
	}
	writeFile(t, land.TownReviewsFile(town).Path, strings.Join(reviewLines, "\n")+"\n")

	for bead, body := range overruns {
		writeFile(t, filepath.Join(town, ".runtime", "overrun-guard", bead+".json"), body)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dispatchAdapterFor(town string, store beadReader) *dispatchAdapter {
	return newDispatchAdapter(town, func() (beadReader, error) { return store, nil })
}

func TestScorecardDispatchLabelsJoinRules(t *testing.T) {
	t.Parallel()
	approve := &land.ReviewRecord{Verdict: "approve", Attempt: 1}
	refused := &land.ReviewRecord{Verdict: "approve", Attempt: 1, Rejected: true}
	changes := &land.ReviewRecord{Verdict: "request_changes", Attempt: 1}
	odd := &land.ReviewRecord{Verdict: "errored", Attempt: 1}
	landed := &beads.Issue{Status: "closed", CloseReason: "Landed on main\ntarget_branch: main\nlanded_commit: abc123"}
	open := &beads.Issue{Status: "in_progress"}
	superseded := &beads.Issue{Status: "closed", CloseReason: "Superseded by gt-other"}
	split := &beads.Issue{Status: "open", Labels: []string{"needs-split"}}

	cases := []struct {
		name     string
		review   *land.ReviewRecord
		reworked bool
		issue    *beads.Issue
		overrun  overrunRecord
		want     map[QuestionID]bool
	}{
		{
			name:   "approved on the first attempt lands; open, so fits is unlabeled",
			review: approve, issue: open,
			want: map[QuestionID]bool{qLandsFirstTry: true},
		},
		{
			name:   "an approval the landing refused is not a first-try landing",
			review: refused, issue: landed,
			want: map[QuestionID]bool{qLandsFirstTry: false, qFitsTwentyMinutes: true},
		},
		{
			name:   "request_changes is not a first-try landing, whatever the bead did later",
			review: changes, issue: landed,
			want: map[QuestionID]bool{qLandsFirstTry: false, qFitsTwentyMinutes: true},
		},
		{
			name:   "a verdict the join does not know is unlabeled for lands_first_try",
			review: odd, issue: open,
			want: map[QuestionID]bool{},
		},
		{
			name:  "a bead closed as landed with no review lands first try and fits",
			issue: landed,
			want:  map[QuestionID]bool{qLandsFirstTry: true, qFitsTwentyMinutes: true},
		},
		{
			name:  "no review and still open is unlabeled",
			issue: open,
			want:  map[QuestionID]bool{},
		},
		{
			name:    "a stopped overrun does not fit, even on a bead that landed",
			issue:   landed,
			overrun: overrunStopped,
			want:    map[QuestionID]bool{qLandsFirstTry: true, qFitsTwentyMinutes: false},
		},
		{
			name:    "a pending overrun leaves fits unlabeled",
			issue:   landed,
			overrun: overrunPending,
			want:    map[QuestionID]bool{qLandsFirstTry: true},
		},
		{
			name:  "a bead closed as superseded does not fit and does not land",
			issue: superseded,
			want:  map[QuestionID]bool{qFitsTwentyMinutes: false},
		},
		{
			name:  "a bead labeled needs-split does not fit",
			issue: split,
			want:  map[QuestionID]bool{qFitsTwentyMinutes: false},
		},
		{
			name:   "a review beats the close for lands_first_try",
			review: approve, issue: superseded,
			want: map[QuestionID]bool{qLandsFirstTry: true, qFitsTwentyMinutes: false},
		},
		{
			name: "a bead the store no longer has, with no review or record, is unlabeled",
			want: map[QuestionID]bool{},
		},
		{
			name:     "a bead closed as landed with only a rework verdict has an unknown first try",
			reworked: true, issue: landed,
			want: map[QuestionID]bool{qFitsTwentyMinutes: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := dispatchLabels(tc.review, tc.reworked, tc.issue, tc.overrun)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dispatchLabels = %v, want %v", got, tc.want)
			}
		})
	}
}

// The rule names are what the --json report shows for each line, so each one
// names the join rule that fired, in the order they fired.
func TestScorecardDispatchRuleNames(t *testing.T) {
	t.Parallel()
	approve := &land.ReviewRecord{Verdict: "approve", Attempt: 1}
	refused := &land.ReviewRecord{Verdict: "approve", Attempt: 1, Rejected: true}
	landed := &beads.Issue{Status: "closed", CloseReason: "Landed on main\nlanded_commit: abc123"}
	superseded := &beads.Issue{Status: "closed", CloseReason: "superseded by gt-z"}
	cases := []struct {
		name    string
		review  *land.ReviewRecord
		issue   *beads.Issue
		overrun overrunRecord
		want    string
	}{
		{"an approval names the review", approve, nil, overrunNone, "review:approve"},
		{"a refused approval says so", refused, nil, overrunNone, "review:refused"},
		{"a landed bead with no review names its close", nil, landed, overrunNone, "landed:no-review+landed"},
		{"a review and a landed close name both rules", approve, landed, overrunNone, "review:approve+landed"},
		{"a stopped overrun names the record", nil, landed, overrunStopped, "landed:no-review+overrun:stopped"},
		{"a superseded close names itself", approve, superseded, overrunNone, "review:approve+closed:superseded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, rules := dispatchLabels(tc.review, false, tc.issue, tc.overrun)
			if got := strings.Join(rules, "+"); got != tc.want {
				t.Errorf("rules = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScorecardDispatchParseCarriesAgentBesideStrongModel(t *testing.T) {
	t.Parallel()
	a := dispatchAdapterFor(t.TempDir(), dispatchBeadStore{})
	raw, err := json.Marshal(dispatch.Line{
		Bead:       "gt-x1",
		AttachedAt: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC),
		Agent:      "alpha",
		Answers:    dispatchAnswers(0.25),
		Model:      "model-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if item.ID != "gt-x1" || item.Model != "model-1" || !item.Time.Equal(time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("item = %+v, want the bead, its placement stamp and its model", item)
	}
	if got := item.Answers[qLandsFirstTry]; got.Noul == nil || *got.Noul != 0.25 {
		t.Errorf("lands_first_try = %+v, want noul 0.25", got)
	}
	if got := item.Answers[qSize]; got.Score == nil || *got.Score != 1 {
		t.Errorf("size = %+v, want score 1", got)
	}
	if item.Context[qNeedsStrongModel] != "alpha" || len(item.Context) != 1 {
		t.Errorf("context = %v, want the agent beside needs_strong_model only", item.Context)
	}
}

// A line logged after lands_first_try left the battery has no answer for it,
// so the scorecard leaves the question unanswered rather than scoring 0.
func TestScorecardDispatchParseLeavesLandsUnansweredWhenNotLogged(t *testing.T) {
	t.Parallel()
	a := dispatchAdapterFor(t.TempDir(), dispatchBeadStore{})
	answers := dispatchAnswers(0.25)
	answers.LandsFirstTry = nil
	raw, err := json.Marshal(dispatch.Line{
		Bead:       "gt-x2",
		AttachedAt: time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC),
		Answers:    answers,
		Model:      "model-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, ok := item.Answers[qLandsFirstTry]; ok {
		t.Errorf("lands_first_try = %+v, want no answer: the line never logged one", got)
	}
	if got := item.Answers[qFitsTwentyMinutes]; got.Noul == nil || *got.Noul != 0.25 {
		t.Errorf("fits_twenty_minutes = %+v, want noul 0.25", got)
	}
}

func TestScorecardDispatchParseRejectsUnusableLines(t *testing.T) {
	t.Parallel()
	a := dispatchAdapterFor(t.TempDir(), dispatchBeadStore{})
	for _, raw := range []string{
		`{"bead":"","attached_at":"2026-10-19T09:00:00Z","answers":{}}`,
		`{"bead":"../escape","attached_at":"2026-10-19T09:00:00Z","answers":{}}`,
		`{"bead":"gt-x2","answers":{}}`,
	} {
		if _, err := a.Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) succeeded, want the line skipped", raw)
		}
	}
}

// dispatchFixture writes seven placements of six beads (gt-a1 is placed twice),
// a line that does not parse, an attempt-2 review that must not count, and a
// bead the store no longer has (gt-a5). It returns the bead store.
func dispatchFixture(t *testing.T, town string) dispatchBeadStore {
	t.Helper()
	at := func(h int) time.Time { return time.Date(2026, 10, 19, h, 0, 0, 0, time.UTC) }
	line := func(bead string, h int, agent string, p float64) dispatch.Line {
		return dispatch.Line{Bead: bead, AttachedAt: at(h), Agent: agent, Answers: dispatchAnswers(p), Model: "model-1"}
	}
	writeDispatchFixture(t, town,
		[]dispatch.Line{
			line("gt-a1", 9, "alpha", 0.9),
			line("gt-a2", 10, "beta", 0.4),
			line("gt-a3", 11, "alpha", 0.7),
			line("gt-a4", 12, "beta", 0.2),
			line("gt-a5", 13, "alpha", 0.6),
			line("gt-a6", 14, "beta", 0.3),
			line("gt-a1", 15, "alpha", 0.8),
		},
		[]string{`not json`},
		[]land.ReviewRecord{
			{BeadID: "gt-a1", Attempt: 1, Verdict: "approve", At: at(9)},
			{BeadID: "gt-a2", Attempt: 1, Verdict: "request_changes", At: at(10)},
			{BeadID: "gt-a2", Attempt: 2, Verdict: "approve", At: at(11)},
			{BeadID: "gt-a4", Attempt: 1, Verdict: "approve", Rejected: true, At: at(12)},
		},
		map[string]string{
			"gt-a3": `{"stopped_at":"2026-10-19T11:30:00Z"}`,
			"gt-a6": `{"stop_began_at":"2026-10-19T14:10:00Z"}`,
		},
	)
	return dispatchBeadStore{
		"gt-a1": {ID: "gt-a1", Status: "closed", CloseReason: "Landed on main\ntarget_branch: main\nlanded_commit: 1111"},
		"gt-a2": {ID: "gt-a2", Status: "in_progress"},
		"gt-a3": {ID: "gt-a3", Status: "closed", CloseReason: "Landed on main\ntarget_branch: main\nlanded_commit: 3333"},
		"gt-a4": {ID: "gt-a4", Status: "closed", CloseReason: "Landed on main\ntarget_branch: main\nlanded_commit: 4444"},
		"gt-a6": {ID: "gt-a6", Status: "closed", CloseReason: "Landed on main\ntarget_branch: main\nlanded_commit: 6666"},
	}
}

func TestScorecardDispatchScorecardGolden(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	store := dispatchFixture(t, town)

	rep, err := Build(context.Background(), dispatchAdapterFor(town, store), time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var got bytes.Buffer
	if err := WriteText(&got, rep); err != nil {
		t.Fatal(err)
	}

	golden := filepath.Join("testdata", "dispatch_scorecard.golden")
	if *updateDispatchGolden {
		if err := os.WriteFile(golden, got.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading %s (rewrite with -update-dispatch-scorecard-golden): %v", golden, err)
	}
	if got.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-dispatch-scorecard-golden):\n%s\nwant:\n%s", golden, got.String(), want)
	}
}

func TestScorecardDispatchScorecardCountsLabelsPerQuestion(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	store := dispatchFixture(t, town)

	rep, err := Build(context.Background(), dispatchAdapterFor(town, store), time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.Items != 7 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 7 and 1", rep.Items, rep.LinesSkipped)
	}

	// lands_first_try is labeled on six lines: gt-a1 (twice, approved), gt-a2
	// (its attempt-1 request_changes, not the attempt-2 approve), gt-a3 and
	// gt-a6 (landed with no review), and gt-a4 (a refused approval). gt-a5 is
	// missing from the store and has no review, so it is unlabeled.
	lands := question(t, rep, string(qLandsFirstTry))
	if lands.Labeled != 6 || lands.Positives != 4 || lands.Negatives != 2 || lands.Verdict != VerdictNotYet {
		t.Errorf("lands_first_try labeled %d positives %d negatives %d verdict %s, want 6, 4, 2, not-yet",
			lands.Labeled, lands.Positives, lands.Negatives, lands.Verdict)
	}

	// fits_twenty_minutes is labeled on four lines: gt-a1 (twice, landed), gt-a4
	// (landed) and gt-a3 (stopped overrun). gt-a2 is open, gt-a5 is missing and
	// gt-a6 has a pending overrun, so those three are unlabeled.
	fits := question(t, rep, string(qFitsTwentyMinutes))
	if fits.Labeled != 4 || fits.Positives != 3 || fits.Negatives != 1 {
		t.Errorf("fits_twenty_minutes labeled %d positives %d negatives %d, want 4, 3, 1",
			fits.Labeled, fits.Positives, fits.Negatives)
	}

	needs := question(t, rep, string(qNeedsStrongModel))
	if len(needs.Context) != 2 || needs.Context[0].Value != "alpha" || needs.Context[0].Lines != 4 ||
		needs.Context[1].Value != "beta" || needs.Context[1].Lines != 3 {
		t.Errorf("needs_strong_model context = %+v, want alpha 4 lines and beta 3", needs.Context)
	}
	if needs.Labeled != 0 {
		t.Errorf("needs_strong_model labeled %d, want 0: the agent is context, not a label", needs.Labeled)
	}
}

func TestScorecardDispatchBeadStoreFailureFailsReport(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDispatchFixture(t, town, []dispatch.Line{{
		Bead: "gt-b1", AttachedAt: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC), Answers: dispatchAnswers(0.5),
	}}, nil, nil, nil)

	a := newDispatchAdapter(town, func() (beadReader, error) {
		return nil, errors.New("no beads directory for rig gastown")
	})
	_, err := Build(context.Background(), a, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "no beads directory") {
		t.Fatalf("Build error = %v, want the store failure rather than an unlabeled report", err)
	}
}

func TestScorecardDispatchEmptyLogNeverOpensStore(t *testing.T) {
	t.Parallel()
	opened := false
	a := newDispatchAdapter(t.TempDir(), func() (beadReader, error) {
		opened = true
		return dispatchBeadStore{}, nil
	})
	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.Items != 0 || opened {
		t.Errorf("items %d, store opened %v; want an empty report and no store", rep.Items, opened)
	}
}

// The scorecard's question ids must be the keys the log writes, or the
// battery's answers are read as unlabeled with no error. The keys are the
// JSON names of the writer's Answers. lands_first_try is left out: the writer
// no longer logs it, and the scorecard reads it only from older lines.
func TestScorecardDispatchQuestionIDsMatchLogKeys(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(dispatch.Answers{})
	if err != nil {
		t.Fatal(err)
	}
	var answers map[string]json.RawMessage
	if err := json.Unmarshal(data, &answers); err != nil {
		t.Fatal(err)
	}
	var logKeys []string
	for k := range answers {
		logKeys = append(logKeys, k)
	}
	var ids []string
	for _, id := range []QuestionID{qFitsTwentyMinutes, qAcceptanceTickable, qNeedsStrongModel, qTouchesDaemon, qSize} {
		ids = append(ids, string(id))
	}
	sort.Strings(logKeys)
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, logKeys) {
		t.Errorf("question ids = %v, want the log's answer keys %v", ids, logKeys)
	}
}

// isLanded reads the close reason the landing worker writes. This pins it to
// that producer, so a change to the wording fails here rather than silently
// unlabeling every landed bead.
func TestScorecardDispatchLandedMatchesLandingCloseReason(t *testing.T) {
	t.Parallel()
	reason := land.LandingRecord{Target: "main", LandedCommit: "abc123", PatchID: "p1"}.CloseReason()
	if !isLanded(&beads.Issue{Status: closedStatus, CloseReason: reason}) {
		t.Errorf("isLanded(%q) = false, want true for the landing worker's own close reason", reason)
	}
	if isLanded(&beads.Issue{Status: "open", CloseReason: reason}) {
		t.Errorf("isLanded on an open bead = true, want false")
	}
}

// isLanded and isSuperseded decide the dispatch score, and each takes the
// verdict the stale pass takes. The cases below are the reasons the two used
// to read differently before that verdict was shared: a person's lower-case
// landing, and a superseding close that quotes a landing record.
func TestScorecardDispatchCloseReasonPredicates(t *testing.T) {
	t.Parallel()
	closed := func(reason string) *beads.Issue {
		return &beads.Issue{Status: closedStatus, CloseReason: reason}
	}
	tests := []struct {
		name       string
		issue      *beads.Issue
		landed     bool
		superseded bool
	}{
		{name: "no bead", issue: nil},
		{name: "an open bead with a landing reason", issue: &beads.Issue{Status: "open", CloseReason: "Landed on main"}},
		{name: "the bare landed word", issue: closed("landed"), landed: true},
		{name: "a person's lower-case landing", issue: closed("landed on main"), landed: true},
		{
			name:   "the landing record",
			issue:  closed("Landed on main\ntarget_branch: main\nlanded_commit: c0ffee"),
			landed: true,
		},
		{name: "superseded", issue: closed("superseded by gt-a1"), superseded: true},
		{name: "a duplicate, typed in any case", issue: closed("Duplicate of gt-a1"), superseded: true},
		{
			name:       "a superseding close quoting a landing record",
			issue:      closed("superseded by gt-a1\nlanded_commit: c0ffee"),
			superseded: true,
		},
		{name: "another close reason", issue: closed("wontfix: the rewrite replaced it")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isLanded(tt.issue); got != tt.landed {
				t.Errorf("isLanded(%+v) = %v, want %v", tt.issue, got, tt.landed)
			}
			if got := isSuperseded(tt.issue); got != tt.superseded {
				t.Errorf("isSuperseded(%+v) = %v, want %v", tt.issue, got, tt.superseded)
			}
		})
	}
}

// A bead whose verdicts start at attempt 2 has a first try the log does not
// show, so it must not read as a first-try landing when it lands.
func TestScorecardDispatchReworkedBeadFirstTryIsUnlabeled(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDispatchFixture(t, town, nil, nil, []land.ReviewRecord{
		{BeadID: "gt-r1", Attempt: 2, Verdict: "approve", At: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)},
	}, nil)
	store := dispatchBeadStore{
		"gt-r1": {ID: "gt-r1", Status: "closed", CloseReason: "Landed on main\nlanded_commit: 1111"},
	}
	a := dispatchAdapterFor(town, store)
	got, err := a.Label(context.Background(), Item{ID: "gt-r1"})
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	if _, ok := got.Labels[qLandsFirstTry]; ok {
		t.Errorf("lands_first_try labeled %v on a bead with no attempt-1 verdict, want unlabeled", got.Labels[qLandsFirstTry])
	}
	if got.Labels[qFitsTwentyMinutes] != true {
		t.Errorf("fits_twenty_minutes = %v, want true for a landed bead with no overrun", got.Labels[qFitsTwentyMinutes])
	}
}

// The overrun guard's records in the pod mostly carry sling bookkeeping only —
// gate_sling_at, escalation_sling_at and restart_sling_at — and no stop stamp.
// That is no stop at all, so a landed bead carrying one fits twenty minutes by
// rule landed.
func TestDispatchSlingOnlyOverrunRecordLeavesALandedBeadLabeledTrue(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	at := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	writeDispatchFixture(t, town,
		[]dispatch.Line{{Bead: "gt-s1", AttachedAt: at, Answers: dispatchAnswers(0.5)}},
		nil, nil,
		map[string]string{
			"gt-s1": `{"gate_sling_at":"2026-10-19T09:01:00Z","escalation_sling_at":"2026-10-19T09:02:00Z","restart_sling_at":"2026-10-19T09:03:00Z"}`,
		})
	store := dispatchBeadStore{
		"gt-s1": {ID: "gt-s1", Status: "closed", CloseReason: "Landed on main\nlanded_commit: 1111"},
	}
	got, err := dispatchAdapterFor(town, store).Label(context.Background(), Item{ID: "gt-s1"})
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	fits, ok := got.Labels[qFitsTwentyMinutes]
	if !ok || !fits {
		t.Errorf("fits_twenty_minutes = %v (labeled %v), want true on a landed bead with a sling-only record", fits, ok)
	}
	if got.Rule != "landed:no-review+landed" {
		t.Errorf("rule = %q, want %q", got.Rule, "landed:no-review+landed")
	}
}

// A stop that began and has not finished is the guard's normal form while it
// runs: it writes stopped_at last. Such a record is a stop in progress, not a
// record with no stop.
func TestDispatchStopBeganWithoutStoppedAtIsPending(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFile(t, filepath.Join(constants.TownRuntimePath(town), "overrun-guard", "gt-p1.json"),
		`{"bead":"gt-p1","stop_began_at":"2026-10-19T09:30:00Z"}`)
	got, err := readOverrun(town, "gt-p1")
	if err != nil {
		t.Fatalf("readOverrun: %v", err)
	}
	if got != overrunPending {
		t.Errorf("readOverrun = %v, want overrunPending", got)
	}
}

// A record with stopped_at is a stop that finished, so the bead overran and
// fits_twenty_minutes is false even though the bead landed.
func TestDispatchStoppedOverrunLabelsFalse(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	at := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	writeDispatchFixture(t, town,
		[]dispatch.Line{{Bead: "gt-s2", AttachedAt: at, Answers: dispatchAnswers(0.5)}},
		nil, nil,
		map[string]string{
			"gt-s2": `{"stop_began_at":"2026-10-19T09:10:00Z","stopped_at":"2026-10-19T09:30:00Z"}`,
		})
	store := dispatchBeadStore{
		"gt-s2": {ID: "gt-s2", Status: "closed", CloseReason: "Landed on main\nlanded_commit: 2222"},
	}
	got, err := dispatchAdapterFor(town, store).Label(context.Background(), Item{ID: "gt-s2"})
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	fits, ok := got.Labels[qFitsTwentyMinutes]
	if !ok || fits {
		t.Errorf("fits_twenty_minutes = %v (labeled %v), want false on a stopped overrun", fits, ok)
	}
	if !strings.Contains(got.Rule, "overrun:stopped") {
		t.Errorf("rule = %q, want it to name overrun:stopped", got.Rule)
	}
}

// A record carrying only sling bookkeeping has no stop in it, so it is no
// overrun at all — not a stop still pending.
func TestReadOverrunSlingOnlyRecordIsNone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFile(t, filepath.Join(constants.TownRuntimePath(town), "overrun-guard", "gt-s3.json"),
		`{"bead":"gt-s3","gate_sling_at":"2026-10-19T09:01:00Z","escalation_sling_at":"2026-10-19T09:02:00Z","restart_sling_at":"2026-10-19T09:03:00Z"}`)
	got, err := readOverrun(town, "gt-s3")
	if err != nil {
		t.Fatalf("readOverrun: %v", err)
	}
	if got != overrunNone {
		t.Errorf("readOverrun = %v, want overrunNone", got)
	}
}

// Each read the adapter makes can fail. A failure must fail the report, never
// leave the line unlabeled.
func TestScorecardDispatchReadFailuresFailReport(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	line := dispatch.Line{Bead: "gt-b1", AttachedAt: at, Answers: dispatchAnswers(0.5)}
	cases := []struct {
		name  string
		setup func(t *testing.T, town string) beadReader
		want  string
	}{
		{
			name: "an overrun record that cannot be read",
			setup: func(t *testing.T, town string) beadReader {
				writeDispatchFixture(t, town, []dispatch.Line{line}, nil, nil, nil)
				if err := os.MkdirAll(filepath.Join(town, ".runtime", "overrun-guard", "gt-b1.json"), 0o700); err != nil {
					t.Fatal(err)
				}
				return dispatchBeadStore{}
			},
			want: "reading the overrun record",
		},
		{
			name: "an overrun record that does not parse",
			setup: func(t *testing.T, town string) beadReader {
				writeDispatchFixture(t, town, []dispatch.Line{line}, nil, nil, map[string]string{"gt-b1": `{"stopped_at":`})
				return dispatchBeadStore{}
			},
			want: "parsing the overrun record",
		},
		{
			name: "a bead store that fails on a lookup",
			setup: func(t *testing.T, town string) beadReader {
				writeDispatchFixture(t, town, []dispatch.Line{line}, nil, nil, nil)
				return failingBeadStore{}
			},
			want: "showing bead gt-b1",
		},
		{
			name: "a review log that cannot be read",
			setup: func(t *testing.T, town string) beadReader {
				writeDispatchFixture(t, town, []dispatch.Line{line}, nil, nil, nil)
				path := land.TownReviewsFile(town).Path
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return dispatchBeadStore{}
			},
			want: "reading the review log",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			store := tc.setup(t, town)
			a := newDispatchAdapter(town, func() (beadReader, error) { return store, nil })
			_, err := Build(context.Background(), a, time.Time{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// The dispatcher holds a hand-typed Needs-Split as it holds needs-split, so a
// landed bead carrying that label must not score as fitting twenty minutes.
func TestScorecardDispatchNeedsSplitIgnoresCase(t *testing.T) {
	t.Parallel()
	landed := &beads.Issue{Status: "closed", CloseReason: "Landed on main\nlanded_commit: abc123", Labels: []string{"Needs-Split"}}
	got, rules := dispatchLabels(nil, false, landed, overrunNone)
	want := map[QuestionID]bool{qLandsFirstTry: true, qFitsTwentyMinutes: false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dispatchLabels = %v, want %v", got, want)
	}
	if got, want := strings.Join(rules, "+"), "landed:no-review+label:needs-split"; got != want {
		t.Errorf("rules = %q, want %q", got, want)
	}
}

// writeLandingsFixture writes the rig's landings file under town, one record
// per line, built from the landings package's own Record so the fixture cannot
// drift from the writer's field names.
func writeLandingsFixture(t *testing.T, town string, recs []landings.Record) {
	t.Helper()
	path, err := landings.Path(town, dispatch.RigName)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range recs {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
}

// A polecat landing measured at 30 minutes is 34 points on the story-points
// scale, which is over 27.5 minutes, so its size is labeled large.
func TestDispatchSizeLabeledLargeAtThirtyFourPoints(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeLandingsFixture(t, town, []landings.Record{
		{Bead: "gt-z1", Agent: "sbx-haiku", PolecatSecs: 30 * 60},
	})
	got, err := dispatchAdapterFor(town, dispatchBeadStore{}).Label(context.Background(), Item{ID: "gt-z1"})
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	large, ok := got.Labels[qSize]
	if !ok || !large {
		t.Errorf("size = %v (labeled %v), want large on a 30-minute landing", large, ok)
	}
	if got.Rule != "landing:points" {
		t.Errorf("rule = %q, want %q", got.Rule, "landing:points")
	}
}

// A polecat landing measured at 20 minutes is 21 points, below 34, so its size
// is labeled small.
func TestDispatchSizeLabeledSmallBelowThirtyFourPoints(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeLandingsFixture(t, town, []landings.Record{
		{Bead: "gt-z2", Agent: "sbx-haiku", PolecatSecs: 20 * 60},
	})
	got, err := dispatchAdapterFor(town, dispatchBeadStore{}).Label(context.Background(), Item{ID: "gt-z2"})
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	large, ok := got.Labels[qSize]
	if !ok || large {
		t.Errorf("size = %v (labeled %v), want small on a 20-minute landing", large, ok)
	}
}

// A crew landing has no seat, a zero time or a bound time has no measurement,
// and a bead with no landing record has nothing to label. Each leaves size
// unlabeled rather than reading as small.
func TestDispatchSizeUnlabeledForCrewLandingsAndUnknownBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rec  *landings.Record
	}{
		{name: "crew landing with no time", rec: &landings.Record{Bead: "gt-u", PolecatSecs: 0}},
		{name: "crew landing that carries a time", rec: &landings.Record{Bead: "gt-u", PolecatSecs: 30 * 60}},
		{name: "polecat landing with a zero time", rec: &landings.Record{Bead: "gt-u", Agent: "sbx-haiku"}},
		{name: "polecat landing with a lower bound", rec: &landings.Record{Bead: "gt-u", Agent: "sbx-haiku", PolecatSecs: 30 * 60, PolecatLowerBound: true}},
		{name: "polecat landing with an upper bound", rec: &landings.Record{Bead: "gt-u", Agent: "sbx-haiku", PolecatSecs: 30 * 60, PolecatUpperBound: true}},
		{name: "bead with no landing record", rec: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			var recs []landings.Record
			if tc.rec != nil {
				recs = append(recs, *tc.rec)
			}
			writeLandingsFixture(t, town, recs)
			got, err := dispatchAdapterFor(town, dispatchBeadStore{}).Label(context.Background(), Item{ID: "gt-u"})
			if err != nil {
				t.Fatalf("Label: %v", err)
			}
			if _, ok := got.Labels[qSize]; ok {
				t.Errorf("size labeled %v, want unlabeled", got.Labels[qSize])
			}
		})
	}
}

// Two records for one bead are two landings of it; the later one carries the
// bead's whole polecat time, so its points decide the label.
func TestDispatchSizeUsesTheBeadsLatestLandingRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeLandingsFixture(t, town, []landings.Record{
		{Bead: "gt-l1", Agent: "sbx-haiku", PolecatSecs: 20 * 60},
		{Bead: "gt-l1", Agent: "sbx-haiku", PolecatSecs: 30 * 60},
	})
	got, err := dispatchAdapterFor(town, dispatchBeadStore{}).Label(context.Background(), Item{ID: "gt-l1"})
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	large, ok := got.Labels[qSize]
	if !ok || !large {
		t.Errorf("size = %v (labeled %v), want large from the later record", large, ok)
	}
}

// A landings line that is not a record could be the bead's own record, so the
// label fails rather than reading the bead as unlabeled.
func TestDispatchSizeUnreadableLandingLineFailsLabel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path, err := landings.Path(town, dispatch.RigName)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, `{"bead":"gt-b1","agent":"sbx-haiku","polecat_secs":1800}`+"\n"+`{not json`+"\n")
	if _, err := dispatchAdapterFor(town, dispatchBeadStore{}).Label(context.Background(), Item{ID: "gt-b1"}); err == nil {
		t.Error("Label returned no error for an unreadable landings line, want the report to fail")
	}
}

// A landings file that cannot be read at all fails the label too: the size of a
// bead is unknown, so the report must not read it as unlabeled. A directory at
// the file's path is a file the reader cannot read.
func TestDispatchSizeUnreadableLandingsFileFailsLabel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path, err := landings.Path(town, dispatch.RigName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchAdapterFor(town, dispatchBeadStore{}).Label(context.Background(), Item{ID: "gt-d1"}); err == nil {
		t.Error("Label returned no error for an unreadable landings file, want the report to fail")
	}
}

// failingBeadStore is a bead store whose every lookup fails.
type failingBeadStore struct{}

func (failingBeadStore) Show(string) (*beads.Issue, error) {
	return nil, errors.New("dolt unreachable")
}
