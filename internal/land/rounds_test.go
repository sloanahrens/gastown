package land

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// ledgerIn is a ledger over a fresh temp dir and a fixed clock.
func ledgerIn(t *testing.T, bead string) *RoundsLedger {
	t.Helper()
	return &RoundsLedger{OutDir: t.TempDir(), Bead: bead, Now: func() time.Time {
		return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	}}
}

// rendered is Render's output as the test reads it: the schema gt-3l5qy hands
// om through --rounds-file.
type rendered []struct {
	Round      int    `json:"round"`
	Head       string `json:"head"`
	OpenAtLand int    `json:"open_at_land"`
	Findings   []struct {
		ID      string `json:"id"`
		File    string `json:"file"`
		Text    string `json:"text"`
		Outcome string `json:"outcome"`
	} `json:"findings"`
}

func renderedOf(t *testing.T, l *RoundsLedger, maxBytes int) rendered {
	t.Helper()
	data, err := l.Render(maxBytes)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var out rendered
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Render wrote %q, not a JSON array of rounds (%v)", data, err)
	}
	return out
}

// TestRoundsLedgerRoundTrip: append, settle and render a round through a temp
// dir, and the round comes back with each finding's text and outcome and the
// count still open (gt-yphcl).
func TestRoundsLedgerRoundTrip(t *testing.T) {
	t.Parallel()
	l := ledgerIn(t, "gt-abc")
	findings := []Finding{
		{ID: "F1", Severity: "major", Path: "internal/land/a.go", Line: 3, Title: "not atomic"},
		{ID: "F2", Severity: "minor", Path: "internal/land/b.go", Title: "name"},
	}
	if err := l.Append(1, "head1", findings); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Settle(FindingKey{File: "internal/land/a.go", ID: "F1"}, OutcomeFixed); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	rounds := renderedOf(t, l, 0)
	if len(rounds) != 1 {
		t.Fatalf("Render returned %d round(s), want 1", len(rounds))
	}
	r := rounds[0]
	if r.Round != 1 || r.Head != "head1" {
		t.Errorf("round = %d at head %q, want 1 at head1", r.Round, r.Head)
	}
	if r.OpenAtLand != 1 {
		t.Errorf("open_at_land = %d, want 1 (F1 is fixed)", r.OpenAtLand)
	}
	if len(r.Findings) != 2 {
		t.Fatalf("round carries %d finding(s), want 2", len(r.Findings))
	}
	first, second := r.Findings[0], r.Findings[1]
	if first.ID != "F1" || first.File != "internal/land/a.go" || first.Text != "not atomic" || first.Outcome != string(OutcomeFixed) {
		t.Errorf("first finding = %+v, want F1 in internal/land/a.go, text %q, fixed", first, "not atomic")
	}
	if second.ID != "F2" || second.Outcome != string(OutcomeOpen) {
		t.Errorf("second finding = %+v, want F2 open", second)
	}
	// The file lives where the bead's name says, and the injected clock
	// stamped the round.
	data, err := os.ReadFile(filepath.Join(l.OutDir, "rounds-gt-abc.json"))
	if err != nil {
		t.Fatalf("ledger file: %v", err)
	}
	var doc ledgerDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("ledger file is not JSON: %v", err)
	}
	if len(doc.Rounds) != 1 || !doc.Rounds[0].At.Equal(time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("stored rounds = %+v, want one stamped with the injected clock", doc.Rounds)
	}
}

// TestRoundsLedgerRecordsARoundWithoutFindings: an approval that raised
// nothing is still a round, and om reads an empty array rather than null.
func TestRoundsLedgerRecordsARoundWithoutFindings(t *testing.T) {
	t.Parallel()
	l := ledgerIn(t, "gt-abc")
	if err := l.Append(2, "head2", nil); err != nil {
		t.Fatalf("Append: %v", err)
	}
	rounds := renderedOf(t, l, 0)
	if len(rounds) != 1 || rounds[0].Round != 2 || len(rounds[0].Findings) != 0 || rounds[0].OpenAtLand != 0 {
		t.Errorf("Render = %+v, want round 2 with no findings and nothing open", rounds)
	}
	data, err := l.Render(0)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(data), `"findings":[]`) {
		t.Errorf("Render wrote %s, want an empty findings array", data)
	}
}

// TestRoundsLedgerSettleReachesEveryRoundAndReportsAnUnknownFinding: a finding
// an earlier round raised is settled in every round that carries it, and a
// key no round carries is an error rather than a silent no-op.
func TestRoundsLedgerSettleReachesEveryRoundAndReportsAnUnknownFinding(t *testing.T) {
	t.Parallel()
	l := ledgerIn(t, "gt-abc")
	key := FindingKey{File: "a.go", ID: "F1"}
	if err := l.Append(1, "head1", []Finding{{ID: "F1", Path: "a.go", Title: "raised"}}); err != nil {
		t.Fatalf("Append round 1: %v", err)
	}
	if err := l.Append(2, "head2", []Finding{{ID: "F1", Path: "a.go", Title: "raised again"}}); err != nil {
		t.Fatalf("Append round 2: %v", err)
	}
	if err := l.Settle(key, OutcomeAccepted); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	for _, r := range renderedOf(t, l, 0) {
		if got := r.Findings[0].Outcome; got != string(OutcomeAccepted) {
			t.Errorf("round %d records %q, want accepted in every round", r.Round, got)
		}
		if r.OpenAtLand != 0 {
			t.Errorf("round %d reports %d open, want 0", r.Round, r.OpenAtLand)
		}
	}
	if err := l.Settle(FindingKey{ID: "F9"}, OutcomeFixed); err == nil {
		t.Error("Settle of a finding no round carries reported no error")
	}
	if err := l.Settle(key, Outcome("ignored")); err == nil {
		t.Error("Settle of an outcome the ledger never writes reported no error")
	}
	if err := l.Settle(FindingKey{File: "a.go"}, OutcomeFixed); err == nil {
		t.Error("Settle of a key without an id reported no error")
	}
}

// TestRoundsLedgerSettleByName: a key naming only the id settles the finding in
// every file that raises it, the way a bare id in a comment does, and a key
// naming a file settles only that file's (gt-yphcl).
func TestRoundsLedgerSettleByName(t *testing.T) {
	t.Parallel()
	l := ledgerIn(t, "gt-abc")
	if err := l.Append(1, "head1", []Finding{
		{ID: "F1", Path: "a.go", Title: "in a"},
		{ID: "F1", Path: "b.go", Title: "in b"},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Settle(FindingKey{File: "b.go", ID: "F1"}, OutcomeRebutted); err != nil {
		t.Fatalf("Settle by file: %v", err)
	}
	byFile := renderedOf(t, l, 0)[0].Findings
	if byFile[0].Outcome != string(OutcomeOpen) || byFile[1].Outcome != string(OutcomeRebutted) {
		t.Errorf("settling b.go's F1 left %q and %q, want open and rebutted", byFile[0].Outcome, byFile[1].Outcome)
	}
	if err := l.Settle(FindingKey{ID: "F1"}, OutcomeFixed); err != nil {
		t.Fatalf("Settle by id: %v", err)
	}
	for _, f := range renderedOf(t, l, 0)[0].Findings {
		if f.Outcome != string(OutcomeFixed) {
			t.Errorf("settling a bare id left %s at %q, want fixed in every file", f.File, f.Outcome)
		}
	}
}

// TestRoundsLedgerReappendDoesNotSettleIdlessFindings: om findings need not
// have ids, two of them in one file share a key, and neither inherits its
// neighbor's outcome on a re-append (gt-yphcl).
func TestRoundsLedgerReappendDoesNotSettleIdlessFindings(t *testing.T) {
	t.Parallel()
	l := ledgerIn(t, "gt-abc")
	// A ledger that records one id-less finding settled and its neighbor not:
	// the two share a key, so the re-append must settle neither from the other.
	doc := `{"bead":"gt-abc","rounds":[{"round":1,"head":"head1","findings":[` +
		`{"id":"","file":"a.go","text":"first","outcome":"fixed"},` +
		`{"id":"","file":"a.go","text":"second","outcome":"open"}]}]}`
	if err := os.WriteFile(filepath.Join(l.OutDir, "rounds-gt-abc.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	idless := []Finding{{Path: "a.go", Title: "first"}, {Path: "a.go", Title: "second"}}
	if err := l.Append(1, "head1", idless); err != nil {
		t.Fatalf("re-append: %v", err)
	}
	findings := renderedOf(t, l, 0)[0].Findings
	if findings[0].Outcome != string(OutcomeOpen) || findings[1].Outcome != string(OutcomeOpen) {
		t.Errorf("re-appended outcomes = %q and %q, want both open: an id-less finding cannot be told from its neighbor",
			findings[0].Outcome, findings[1].Outcome)
	}
}

// TestRoundsLedgerReappendKeepsSettledOutcomes: a retry of the same attempt
// re-appends the round, and the outcomes the bead already settled survive it
// (gt-yphcl).
func TestRoundsLedgerReappendKeepsSettledOutcomes(t *testing.T) {
	t.Parallel()
	l := ledgerIn(t, "gt-abc")
	key := FindingKey{File: "a.go", ID: "F1"}
	if err := l.Append(1, "head1", []Finding{{ID: "F1", Path: "a.go", Title: "raised"}, {ID: "F2", Path: "a.go", Title: "other"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Settle(key, OutcomeAccepted); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if err := l.Append(1, "head1", []Finding{{ID: "F1", Path: "a.go", Title: "raised"}, {ID: "F2", Path: "a.go", Title: "other"}}); err != nil {
		t.Fatalf("re-append: %v", err)
	}
	rounds := renderedOf(t, l, 0)
	if len(rounds) != 1 {
		t.Fatalf("re-appending round 1 left %d round(s), want 1", len(rounds))
	}
	if got := rounds[0].Findings[0].Outcome; got != string(OutcomeAccepted) {
		t.Errorf("the re-appended round records F1 as %q, want the settled accepted", got)
	}
}

// TestRoundsLedgerRenderDropsTheOldestRoundsOverTheCap: an output over the cap
// keeps the newest rounds whole, and the newest round stays even alone over it
// (gt-yphcl).
func TestRoundsLedgerRenderDropsTheOldestRoundsOverTheCap(t *testing.T) {
	t.Parallel()
	l := ledgerIn(t, "gt-abc")
	text := strings.Repeat("x", 300)
	for round := 1; round <= 3; round++ {
		if err := l.Append(round, "head", []Finding{{ID: "F1", Path: "a.go", Title: text}}); err != nil {
			t.Fatalf("Append round %d: %v", round, err)
		}
	}
	if got := len(renderedOf(t, l, 0)); got != 3 {
		t.Errorf("Render at the default cap kept %d round(s), want all 3", got)
	}
	capped := renderedOf(t, l, 500)
	if len(capped) != 1 {
		t.Fatalf("Render at 500 bytes kept %d round(s), want the newest only", len(capped))
	}
	if capped[0].Round != 3 {
		t.Errorf("the kept round is %d, want the newest, 3", capped[0].Round)
	}
	if lone := renderedOf(t, l, 10); len(lone) != 1 || lone[0].Round != 3 {
		t.Errorf("Render under the newest round's own size = %+v, want round 3 kept whole", lone)
	}
	if empty := renderedOf(t, ledgerIn(t, "gt-empty"), 0); len(empty) != 0 {
		t.Errorf("an empty ledger rendered %+v, want no rounds", empty)
	}
}

// TestRoundsLedgerReportsACorruptFile: a ledger file that does not parse, or
// records one round twice, is an error the caller treats as no history, and
// the file is left for a human rather than overwritten (gt-yphcl).
func TestRoundsLedgerReportsACorruptFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "not json", body: "{\"bead\": \"gt-abc\", "},
		{name: "two entries for one round", body: `{"bead":"gt-abc","rounds":[{"round":1},{"round":1}]}`},
		{name: "an outcome the ledger never writes", body: `{"bead":"gt-abc","rounds":[{"round":1,"findings":[{"id":"F1","outcome":"maybe"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := ledgerIn(t, "gt-abc")
			name := filepath.Join(l.OutDir, "rounds-gt-abc.json")
			if err := os.WriteFile(name, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Render(0); !errors.Is(err, ErrRoundsCorrupt) {
				t.Errorf("Render on a corrupt ledger = %v, want ErrRoundsCorrupt", err)
			}
			if err := l.Append(1, "head", nil); !errors.Is(err, ErrRoundsCorrupt) {
				t.Errorf("Append on a corrupt ledger = %v, want ErrRoundsCorrupt", err)
			}
			if err := l.Settle(FindingKey{ID: "F1"}, OutcomeFixed); !errors.Is(err, ErrRoundsCorrupt) {
				t.Errorf("Settle on a corrupt ledger = %v, want ErrRoundsCorrupt", err)
			}
			after, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != tc.body {
				t.Errorf("the corrupt ledger was rewritten to %q", after)
			}
		})
	}
}

// failingFS is an fs whose write always fails: the seam a test uses to reach
// the save path without a broken host.
type failingFS struct {
	osLedgerFS
	err error
}

func (f failingFS) WriteFile(string, []byte, os.FileMode) error { return f.err }

// TestRoundsLedgerReportsAFailedWrite: a ledger that cannot be written is an
// error the landing logs and survives.
func TestRoundsLedgerReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	boom := errors.New("no space")
	l := ledgerIn(t, "gt-abc")
	l.FS = failingFS{err: boom}
	if err := l.Append(1, "head", nil); !errors.Is(err, boom) {
		t.Errorf("Append over a failing write = %v, want %v", err, boom)
	}
}

// TestFindingOutcome: the mapping from the old outcome, the later round's
// classification and the bead's comments to a finding's outcome (gt-yphcl).
// om's classification decides a finding nobody settled; a comment from the
// allowlist settles it whatever the round said; omission settles nothing.
func TestFindingOutcome(t *testing.T) {
	t.Parallel()
	const at = "2026-10-26T12:00:00Z"
	comments := []beads.Comment{
		{Author: "Sloan Ahrens", Text: "ACCEPTED-RISK: F2 the mount is approved by design 5.5", CreatedAt: at},
		{Author: "town/crew/overseer", Text: "REBUTTAL: F3 the guard runs on the write path too", CreatedAt: at},
		{Author: "town/crew/overseer", Text: "ACCEPTED-RISK: internal/land/rounds.go:F4 approved there too", CreatedAt: at},
		{Author: "Sloan Ahrens", Text: "ACCEPTED-RISK:F7 approved with no space after the marker", CreatedAt: at},
		{Author: "Sloan Ahrens", Text: "ACCEPTED-RISK: internal/land/other.go:F1 approved there", CreatedAt: at},
		{Author: "gastown/polecats/immortan", Text: "ACCEPTED-RISK: F1 my own work is fine", CreatedAt: at},
		{Author: "gastown/polecats/immortan", Text: "REBUTTAL: F6 not my problem", CreatedAt: at},
		{Author: "Sloan Ahrens", Text: "ACCEPTED-RISK: F5 approved\nACCEPTED-RISK: F10 approved too\nthe F8 mention here is prose", CreatedAt: at},
	}
	inFile := FindingKey{File: "internal/land/rounds.go", ID: "F1"}
	tests := []struct {
		name  string
		key   FindingKey
		prev  Outcome
		class Classification
		want  Outcome
	}{
		{name: "om classifies it resolved", key: inFile, class: ClassificationResolved, want: OutcomeFixed},
		{name: "om does not mention it", key: inFile, class: ClassificationUnresolved, want: OutcomeOpen},
		{name: "om regresses a fixed finding", key: inFile, prev: OutcomeFixed, class: ClassificationRegressed, want: OutcomeOpen},
		{name: "om does not mention a fixed finding", key: inFile, prev: OutcomeFixed, class: ClassificationUnresolved, want: OutcomeFixed},
		{name: "an accepted risk names it", key: FindingKey{ID: "F2"}, want: OutcomeAccepted},
		{name: "a rebuttal names it", key: FindingKey{ID: "F3"}, want: OutcomeRebutted},
		{name: "a file and id form settles nothing: a comment names an id", key: FindingKey{File: "internal/land/rounds.go", ID: "F4"}, want: OutcomeOpen},
		{name: "one comment with two id lines settles each id", key: FindingKey{ID: "F5"}, want: OutcomeAccepted},
		{name: "the second id line of one comment settles its id", key: FindingKey{ID: "F10"}, want: OutcomeAccepted},
		{name: "a prose mention of an id settles nothing", key: FindingKey{ID: "F8"}, want: OutcomeOpen},
		{name: "an id names one finding exactly: F10 does not settle F1", key: inFile, class: ClassificationUnresolved, want: OutcomeOpen},
		{name: "an accepted risk names a bare id with no space after the marker", key: FindingKey{ID: "F7"}, want: OutcomeAccepted},
		{name: "a file scoped comment does not settle the id in another file", key: inFile, class: ClassificationUnresolved, want: OutcomeOpen},
		{name: "an accepted risk is not reopened by omission", key: FindingKey{ID: "F2"}, prev: OutcomeAccepted, class: ClassificationUnresolved, want: OutcomeAccepted},
		{name: "an unmatched finding stays open", key: FindingKey{ID: "F9"}, class: ClassificationUnresolved, want: OutcomeOpen},
		{name: "a polecat's accepted risk is ignored", key: inFile, class: ClassificationUnresolved, want: OutcomeOpen},
		{name: "a polecat's rebuttal is ignored", key: FindingKey{ID: "F6"}, class: ClassificationUnresolved, want: OutcomeOpen},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FindingOutcome(tc.prev, tc.class, comments, tc.key); got != tc.want {
				t.Errorf("FindingOutcome(%q, %q, %s) = %q, want %q", tc.prev, tc.class, tc.key, got, tc.want)
			}
		})
	}
}

// TestLandRoundsLedgerRoundIsTheReviewsLogAttempt: the landing path records
// each round through the same read of the bead the reviews log counts, so a
// round and the attempt it names cannot drift apart (gt-yphcl).
func TestLandRoundsLedgerRoundIsTheReviewsLogAttempt(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	reviews := reviewsFileIn(t)
	l := f.lander()
	l.Reviews = reviews
	l.RoundsDir = t.TempDir()
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictRequestChanges, Score: 0.5, Findings: []Finding{
			{ID: "F1", Severity: "major", Path: "b.txt", Line: 1, Title: "not atomic"}}}, nil
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first landing passed; want a review rejection")
	}
	f.requeue(1)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictRequestChanges, Score: 0.6, Findings: []Finding{
			{ID: "F2", Severity: "major", Path: "b.txt", Line: 2, Title: "still not atomic"}}}, nil
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the second landing passed; want a review rejection")
	}
	recs, err := reviews.Since(time.Time{})
	if err != nil {
		t.Fatalf("reviews log: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("reviews log holds %d record(s), want 2", len(recs))
	}
	ledger := RoundsLedger{OutDir: l.RoundsDir, Bead: f.work.BeadID}
	rounds := renderedOf(t, &ledger, 0)
	if len(rounds) != len(recs) {
		t.Fatalf("ledger holds %d round(s) for %d review record(s)", len(rounds), len(recs))
	}
	for i, rec := range recs {
		if rounds[i].Round != rec.Attempt {
			t.Errorf("ledger round %d names attempt %d, but reviews record %d is attempt %d", i+1, rounds[i].Round, i+1, rec.Attempt)
		}
		if rounds[i].Head != f.work.Head {
			t.Errorf("round %d records head %q, want the submitted head %q", rounds[i].Round, rounds[i].Head, f.work.Head)
		}
		if rounds[i].OpenAtLand != 1 {
			t.Errorf("round %d reports open_at_land %d, want its one unsettled finding", rounds[i].Round, rounds[i].OpenAtLand)
		}
	}
}

// TestLandSurvivesACorruptRoundsLedger: the ledger is the landing's history,
// not its precondition, so a file that does not parse costs a log line and the
// landing rejects exactly as it did before (gt-yphcl).
func TestLandSurvivesACorruptRoundsLedger(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.RoundsDir = t.TempDir()
	l.Reviews = reviewsFileIn(t)
	if err := os.WriteFile(filepath.Join(l.RoundsDir, "rounds-"+f.work.BeadID+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	l.Out = &out
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictRequestChanges, Score: 0.4, Findings: []Finding{
			{ID: "F1", Severity: "major", Path: "b.txt", Line: 1, Title: "broken"}}}, nil
	}
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectReview, LabelRework)
	if !strings.Contains(out.String(), "could not record the round in the rounds ledger") {
		t.Errorf("the failed ledger write is not in the landing log:\n%s", out.String())
	}
}

// An unreadable rounds ledger is named once for its bead, not on every round:
// the review runs without rounds and the log says so once (gt-yphcl).
func TestLogRoundsNamesAnUnreadableLedgerOncePerBead(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "landing-gt-yphcl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rounds-gt-yphcl.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	l := &Lander{RoundsDir: dir, Out: &out}
	issue := &beads.Issue{ID: "gt-yphcl"}
	w := Work{BeadID: "gt-yphcl", Head: "abc123"}
	v := Verdict{Verdict: VerdictRequestChanges}
	for i := 0; i < 3; i++ {
		l.logRounds(issue, w, v)
	}
	if n := strings.Count(out.String(), "could not record the round"); n != 1 {
		t.Errorf("the unreadable ledger was named %d times, want once:\n%s", n, out.String())
	}
	if !l.firstRoundsWarning("other-bead") {
		t.Error("a different bead is not named yet, but its warning was suppressed")
	}
}
