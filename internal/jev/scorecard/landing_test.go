package scorecard

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
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

func landingItem(bead, head string) Item {
	return Item{
		ID:   bead,
		Time: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC),
		Refs: map[string]string{refRig: landing.RigName, refHead: head},
	}
}

func TestLandingLabelReadsTheOverseerVerdict(t *testing.T) {
	t.Parallel()
	head := "a1a1a1a"
	other := "b2b2b2b"
	cases := []struct {
		name     string
		notes    string
		labeled  bool
		failed   bool
		findings []string
	}{
		{
			name:    "a PASS for the logged head labels both questions false",
			notes:   land.OverseerReviewMarker + " " + head + " PASS",
			labeled: true,
		},
		{
			name:    "a FAIL labels both true",
			notes:   land.OverseerReviewMarker + " " + head + " FAIL",
			labeled: true,
			failed:  true,
		},
		{
			name:     "a FAIL's finding beads are kept for the context",
			notes:    land.OverseerReviewMarker + " " + head + " FAIL findings=gt-f1,gt-f2",
			labeled:  true,
			failed:   true,
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
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			notes := &fakeLandingNotes{byBead: map[string]string{"gt-a": tc.notes}}
			a := &landingAdapter{notes: notes}
			lab, err := a.Label(context.Background(), landingItem("gt-a", head))
			if err != nil {
				t.Fatal(err)
			}
			if lab.Labeled != tc.labeled {
				t.Fatalf("labeled = %v, want %v (labels %v)", lab.Labeled, tc.labeled, lab.Labels)
			}
			if !tc.labeled {
				if lab.Labels != nil || lab.Rule != "" {
					t.Errorf("unlabeled item returned labels %v rule %q, want none", lab.Labels, lab.Rule)
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

func TestLandingLabelFailsWhenTheNotesCannotBeRead(t *testing.T) {
	t.Parallel()
	a := &landingAdapter{notes: &fakeLandingNotes{err: errors.New("bead store unavailable")}}
	_, err := a.Label(context.Background(), landingItem("gt-a", "a1a1a1a"))
	if err == nil || !strings.Contains(err.Error(), "bead store unavailable") {
		t.Fatalf("Label error = %v, want the store failure", err)
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
	a := &landingAdapter{log: logPath, notes: &fakeLandingNotes{byBead: landingNotesByBead}}

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
