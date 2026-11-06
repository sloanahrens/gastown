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
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/land"
)

var updateLandingGolden = flag.Bool("update-landing-golden", false, "rewrite testdata/landing.golden")

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

// fakeReviewDocs returns scripted polecat verdicts per bead and head.
type fakeReviewDocs struct {
	byBead map[string]map[string]string
	err    error
	reads  []string
}

func (f *fakeReviewDocs) Verdict(_ context.Context, bead, head string) (string, bool, error) {
	f.reads = append(f.reads, bead+"/"+head)
	if f.err != nil {
		return "", false, f.err
	}
	verdict, ok := f.byBead[bead][head]
	return verdict, ok, nil
}

func landingItem(bead, head string) Item {
	return Item{
		ID:   bead,
		Time: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC),
		Refs: map[string]string{refRig: landing.RigName, refHead: head},
	}
}

// TestLandingLabelReadsTheReviewVerdicts labels one landing from each source
// the scorecard reads: the overseer's note on the bead and the polecat's
// review document, the overseer's verdict winning when both name the head
// (gt-b9j3o.1).
func TestLandingLabelReadsTheReviewVerdicts(t *testing.T) {
	t.Parallel()
	head := "a1a1a1a"
	other := "b2b2b2b"
	cases := []struct {
		name     string
		notes    string
		docs     map[string]string
		labeled  bool
		failed   bool
		source   string
		findings []string
	}{
		{
			name:    "a PASS for the logged head labels both questions false",
			notes:   land.OverseerReviewMarker + " " + head + " PASS",
			labeled: true,
			source:  landingSourceOverseer,
		},
		{
			name:    "a FAIL labels both true",
			notes:   land.OverseerReviewMarker + " " + head + " FAIL",
			labeled: true,
			failed:  true,
			source:  landingSourceOverseer,
		},
		{
			name:     "a FAIL's finding beads are kept for the context",
			notes:    land.OverseerReviewMarker + " " + head + " FAIL findings=gt-f1,gt-f2",
			labeled:  true,
			failed:   true,
			source:   landingSourceOverseer,
			findings: []string{"gt-f1", "gt-f2"},
		},
		{
			name:    "no review line is unlabeled",
			notes:   "",
			labeled: false,
		},
		{
			name:    "a WAIVED line is unlabeled",
			notes:   land.OverseerReviewMarker + " " + head + " WAIVED: docs only",
			labeled: false,
		},
		{
			name:    "an AUDITED line is unlabeled",
			notes:   land.OverseerReviewMarker + " " + head + " AUDITED: file-level audit",
			labeled: false,
		},
		{
			name:    "a verdict for another head is unlabeled",
			notes:   land.OverseerReviewMarker + " " + other + " PASS",
			labeled: false,
		},
		{
			name:    "a PASS after a FAIL supersedes it",
			notes:   land.OverseerReviewMarker + " " + head + " FAIL findings=gt-f1\n" + land.OverseerReviewMarker + " " + head + " PASS",
			labeled: true,
			source:  landingSourceOverseer,
		},
		{
			name:    "a polecat PASS labels both questions false",
			docs:    map[string]string{head: land.OverseerReviewPass},
			labeled: true,
			source:  landingSourcePolecat,
		},
		{
			name:    "a polecat FAIL labels both true",
			docs:    map[string]string{head: land.OverseerReviewFail},
			labeled: true,
			failed:  true,
			source:  landingSourcePolecat,
		},
		{
			name:    "the overseer's verdict wins over the polecat's document",
			notes:   land.OverseerReviewMarker + " " + head + " PASS",
			docs:    map[string]string{head: land.OverseerReviewFail},
			labeled: true,
			source:  landingSourceOverseer,
		},
		{
			name:    "a WAIVED note falls through to the polecat's document",
			notes:   land.OverseerReviewMarker + " " + head + " WAIVED: docs only",
			docs:    map[string]string{head: land.OverseerReviewPass},
			labeled: true,
			source:  landingSourcePolecat,
		},
		{
			name:    "a polecat review of another head is unlabeled",
			docs:    map[string]string{other: land.OverseerReviewPass},
			labeled: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			notes := &fakeLandingNotes{byBead: map[string]string{"gt-a": tc.notes}}
			docs := &fakeReviewDocs{byBead: map[string]map[string]string{"gt-a": tc.docs}}
			a := &landingAdapter{notes: notes, reviews: docs}
			lab, err := a.Label(context.Background(), landingItem("gt-a", head))
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled = %v, want %v (labels %v)", lab.Labeled, tc.labeled, lab.Labels)
			}
			if !tc.labeled {
				if lab.Labels != nil || lab.Rule != "" || lab.Source != "" {
					t.Errorf("unlabeled item returned labels %v rule %q source %q, want none", lab.Labels, lab.Rule, lab.Source)
				}
				if got := a.Context(); got != nil {
					t.Errorf("unlabeled item reported context %q, want none", got)
				}
				return
			}
			want := map[QuestionID]bool{
				QuestionID(landing.QNeedsReviewNow): tc.failed,
				QuestionID(landing.QRisk):           tc.failed,
			}
			if !maps.Equal(lab.Labels, want) {
				t.Errorf("labels = %v, want %v", lab.Labels, want)
			}
			wantRule := ruleLandingReviewPass
			if tc.failed {
				wantRule = ruleLandingReviewFail
			}
			if lab.Rule != wantRule {
				t.Errorf("rule = %q, want %q", lab.Rule, wantRule)
			}
			if lab.Source != tc.source {
				t.Errorf("source = %q, want %q", lab.Source, tc.source)
			}
			got := a.Context()
			if len(tc.findings) == 0 {
				if got != nil {
					t.Errorf("context = %q, want none without findings", got)
				}
				return
			}
			wantLine := fmt.Sprintf("FAIL review notes named %d finding bead(s): %s",
				len(tc.findings), strings.Join(tc.findings, ", "))
			if len(got) != 1 || got[0] != wantLine {
				t.Errorf("context = %q, want %q", got, wantLine)
			}
		})
	}
}

func TestLandingLabelFailsWhenTheReviewDocsCannotBeRead(t *testing.T) {
	t.Parallel()
	a := &landingAdapter{
		notes:   &fakeLandingNotes{byBead: map[string]string{"gt-a": ""}},
		reviews: &fakeReviewDocs{err: errors.New("clone unavailable")},
	}
	_, err := a.Label(context.Background(), landingItem("gt-a", "a1a1a1a"))
	if err == nil || !strings.Contains(err.Error(), "clone unavailable") {
		t.Fatalf("Label error = %v, want the review document failure", err)
	}
}

func TestLandingLabelFailsWhenTheNotesCannotBeRead(t *testing.T) {
	t.Parallel()
	a := &landingAdapter{notes: &fakeLandingNotes{err: errors.New("bead store unavailable")}}
	_, err := a.Label(context.Background(), landingItem("gt-a", "a1a1a1a"))
	if err == nil || !strings.Contains(err.Error(), "bead store unavailable") {
		t.Fatalf("Label error = %v, want the store failure", err)
	}
}

// TestLandingLabelFailsWithNoReviewSource fails a landing that has no overseer
// verdict and no review source to ask, rather than reading it as unreviewed
// with a clean exit (gt-b9j3o.1).
func TestLandingLabelFailsWithNoReviewSource(t *testing.T) {
	t.Parallel()
	a := &landingAdapter{notes: &fakeLandingNotes{byBead: map[string]string{"gt-a": ""}}}
	_, err := a.Label(context.Background(), landingItem("gt-a", "a1a1a1a"))
	if err == nil || !strings.Contains(err.Error(), "no review source") {
		t.Fatalf("Label error = %v, want the missing review source", err)
	}
}

// TestLandingReviewsReadFromTheTownClone drives the review source the battery
// builds: a landing with no overseer verdict is read from the review documents
// in the town's clone, so a clone that cannot be opened fails the report and
// names the town it was opened for (gt-b9j3o.1).
func TestLandingReviewsReadFromTheTownClone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a := newLandingAdapter(town, &fakeLandingNotes{byBead: map[string]string{"gt-a": ""}})
	_, err := a.Label(context.Background(), landingItem("gt-a", "a1a1a1a"))
	if err == nil {
		t.Fatal("Label succeeded, want the unopenable clone to fail it")
	}
	if want := filepath.Join(town, "mayor", "rigs.json"); !strings.Contains(err.Error(), want) {
		t.Fatalf("Label error = %v, want it to name the town's clone source %s", err, want)
	}
}

// TestLandingLabelLeavesAMissingBeadUnlabeled drives the real notes store over
// a rig that has a beads directory, with bd stubbed on PATH to answer not
// found, so the bead's absence comes through the store rather than a fake.
//
//testpolicy:allow parallel — t.Setenv puts the bd stub first on PATH, and a parallel test would share that PATH
func TestLandingLabelLeavesAMissingBeadUnlabeled(t *testing.T) {
	if runtime.GOOS == "windows" {
		//testpolicy:allow no-skip — the bd stub is a POSIX shell script, which Windows cannot run
		t.Skip("POSIX bd stub")
	}
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, landing.RigName, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	// Exit 20 is bd's own not-found status; beads reads it as ErrNotFound.
	//testpolicy:allow no-exec-files — the test is the process boundary: beads execs bd, and this stub is the bd it finds on PATH
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nexit 20\n"), 0o755); err != nil { //nolint:gosec // G306: the bd stub must be executable
		t.Fatal(err)
	}
	//testpolicy:allow no-env — the stub reaches beads only through PATH, which is process-wide; the test restores it at cleanup
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := &landingAdapter{notes: &townNotes{town: town, stores: map[string]*beads.Beads{}}}
	lab, err := a.Label(context.Background(), landingItem("gt-a", "a1a1a1a"))
	if err != nil {
		t.Fatalf("Label error = %v, want a missing bead to read as unlabeled", err)
	}
	if lab.Labeled || lab.Labels != nil || lab.Rule != "" {
		t.Errorf("labeling = %+v, want none", lab)
	}
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

// TestLandingBuildFailsWhenTheRigHasNoBeadsDirectory drives the real notes
// reader over a town with no rig directories: every landing of such a rig
// would read as unlabeled, so the report fails rather than exit clean.
func TestLandingBuildFailsWhenTheRigHasNoBeadsDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := landing.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(landingLog[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &landingAdapter{log: logPath, notes: &townNotes{town: town, stores: map[string]*beads.Beads{}}}

	_, err := Build(context.Background(), a, time.Time{})
	if err == nil || !errors.Is(err, errNoRigBeads) {
		t.Fatalf("Build error = %v, want the missing rig beads directory", err)
	}
}

func TestLandingContextClearsTheTallyForTheNextBuild(t *testing.T) {
	t.Parallel()
	head := "a1a1a1a"
	notes := &fakeLandingNotes{byBead: map[string]string{
		"gt-a": land.OverseerReviewMarker + " " + head + " FAIL findings=gt-f2,gt-f1",
		"gt-b": land.OverseerReviewMarker + " " + head + " FAIL findings=gt-f1",
	}}
	a := &landingAdapter{notes: notes}
	for _, bead := range []string{"gt-a", "gt-b"} {
		if _, err := a.Label(context.Background(), landingItem(bead, head)); err != nil {
			t.Fatal(err)
		}
	}
	want := "FAIL review notes named 2 finding bead(s): gt-f1, gt-f2"
	if got := a.Context(); len(got) != 1 || got[0] != want {
		t.Fatalf("first context = %q, want %q", got, want)
	}
	if got := a.Context(); got != nil {
		t.Errorf("second context = %q, want the tally cleared", got)
	}
}

// TestLandingScorecardReportsLabelSourcesApart counts each question's labeled
// lines by the source of their label, so a polecat's review and the overseer's
// are never read as one (gt-b9j3o.1).
func TestLandingScorecardReportsLabelSourcesApart(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := landing.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"ts":"2026-10-19T09:00:00Z","bead":"gt-a","head":"a1a1a1a","answers":{"risk":{"score":1}},"question_version":1}`,
		`{"ts":"2026-10-19T10:00:00Z","bead":"gt-b","head":"b2b2b2b","answers":{"risk":{"score":3}},"question_version":1}`,
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &landingAdapter{
		log:     logPath,
		notes:   &fakeLandingNotes{byBead: map[string]string{"gt-a": land.OverseerReviewMarker + " a1a1a1a PASS", "gt-b": ""}},
		reviews: &fakeReviewDocs{byBead: map[string]map[string]string{"gt-b": {"b2b2b2b": land.OverseerReviewFail}}},
	}
	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var risk *QuestionReport
	for i := range rep.Questions {
		if rep.Questions[i].ID == landing.QRisk {
			risk = &rep.Questions[i]
		}
	}
	if risk == nil {
		t.Fatalf("questions = %+v, want the risk question", rep.Questions)
	}
	want := []SourceCount{
		{Source: landingSourceOverseer, Labeled: 1},
		{Source: landingSourcePolecat, Labeled: 1, Positives: 1},
	}
	if !slices.Equal(risk.Sources, want) {
		t.Errorf("sources = %+v, want %+v", risk.Sources, want)
	}
	if rep.Lines[1].Source != landingSourcePolecat {
		t.Errorf("line source = %q, want %q", rep.Lines[1].Source, landingSourcePolecat)
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	if wantLine := "  labels by source: overseer 1 label, 0 positives; polecat 1 label, 1 positive\n"; !strings.Contains(out.String(), wantLine) {
		t.Errorf("text report lacks %q:\n%s", wantLine, out.String())
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

// landingLog is the fixture log: three labeled-or-not landings, one WAIVED and
// one line that does not parse.
var landingLog = []string{
	`{"ts":"2026-10-19T09:00:00Z","bead":"gt-a","rig":"gastown","head":"a1a1a1a","answers":{"needs_review_now":{"noul":0.9},"touches_town_machinery":{"noul":0.8},"beyond_goal":{"noul":0.1},"tests_cover_change":{"noul":0.7},"risk":{"score":3,"confidence":0.6}},"question_version":1,"model":"jev-1.13.0"}`,
	`{"ts":"2026-10-19T10:00:00Z","bead":"gt-b","rig":"gastown","head":"b2b2b2b","answers":{"needs_review_now":{"noul":0.8},"touches_town_machinery":{"noul":0.9},"beyond_goal":{"noul":0.2},"tests_cover_change":{"noul":0.4},"risk":{"score":2,"confidence":0.5}},"question_version":1,"model":"jev-1.13.0"}`,
	`{"ts":"2026-10-19T11:00:00Z","bead":"gt-c","rig":"gastown","head":"c3c3c3c","answers":{"needs_review_now":{"noul":0.3},"touches_town_machinery":{"noul":0.2},"beyond_goal":{"noul":0.9},"tests_cover_change":{"noul":0.6},"risk":{"score":1,"confidence":0.7}},"question_version":1,"model":"jev-1.13.0"}`,
	`{"ts":"2026-10-19T12:00:00Z","bead":"gt-d","rig":"gastown","head":"d4d4d4d","answers":{"needs_review_now":{"noul":0.5},"touches_town_machinery":{"noul":0.1},"beyond_goal":{"noul":0.3},"tests_cover_change":{"noul":0.8},"risk":{"score":2,"confidence":0.6}},"question_version":1,"model":"jev-1.13.0"}`,
	`not json`,
}

// landingNotesByBead is the fake notes the fixture log is labeled from: a
// PASS, a FAIL naming two findings, no line yet, and a WAIVED.
var landingNotesByBead = map[string]string{
	"gt-a": land.OverseerReviewMarker + " a1a1a1a PASS",
	"gt-b": land.OverseerReviewMarker + " b2b2b2b FAIL findings=gt-f1,gt-f2",
	"gt-c": "",
	"gt-d": land.OverseerReviewMarker + " d4d4d4d WAIVED: docs only",
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
	if _, ok := a.(Contextual); !ok {
		t.Error("the landing adapter does not report context")
	}
}

// TestLandingScorecardPrintsPerQuestionBlocks drives the registered battery's
// adapter with fake notes: the registry factory builds a live beads store, so
// the adapter is constructed with the fake its labeling reads instead.
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
		notes:   &fakeLandingNotes{byBead: landingNotesByBead},
		reviews: &fakeReviewDocs{},
	}

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 4 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 4 and 1", rep.Items, rep.LinesSkipped)
	}
	wantContext := "FAIL review notes named 2 finding bead(s): gt-f1, gt-f2"
	if len(rep.Context) != 1 || rep.Context[0] != wantContext {
		t.Errorf("context = %q, want %q", rep.Context, wantContext)
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
