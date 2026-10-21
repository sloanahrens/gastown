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
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/progress"
	"github.com/steveyegge/gastown/internal/land"
)

var updateProgressScorecardGolden = flag.Bool("update-progress-scorecard-golden", false, "rewrite testdata/progress_scorecard.golden")

// progressBase is when the first test nudge was delivered. The scorecard runs
// three hours later, so every nudge is old enough to be labeled unless a case
// says otherwise.
var progressBase = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
var progressNow = progressBase.Add(3 * time.Hour)

// fakeProgressNotes reads each bead's notes from a map. A bead the map does not
// hold reads as not found, and err fails every read.
type fakeProgressNotes struct {
	notes map[string]string
	err   error
}

func (f fakeProgressNotes) Notes(_, bead string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	n, ok := f.notes[bead]
	if !ok {
		return "", beads.ErrNotFound
	}
	return n, nil
}

func newProgressTestAdapter(town string, notes landingNotes, now time.Time) *progressAdapter {
	return &progressAdapter{
		town:  town,
		notes: notes,
		lines: map[string]progress.Line{},
		now:   func() time.Time { return now },
	}
}

// progressCase is one nudge in the test town, and what the adapter must say
// about it.
type progressCase struct {
	name  string
	bead  string
	nudge time.Time
	// landed is when the bead's landing record was written; zero means the
	// bead has none.
	landed time.Time
	// stop is the overrun guard's record for the bead; the zero value means no
	// record.
	stop progressStop
	// logged is when the line was written; zero means a minute after the
	// nudge, so the line is old enough to label at progressNow.
	logged time.Time
	// want is the labels the adapter must set, nil when it must leave the item
	// unlabeled; wantRule names the rule that sets them.
	want     map[QuestionID]bool
	wantRule string
}

func progressLine(c progressCase) progress.Line {
	logged := c.logged
	if logged.IsZero() {
		logged = c.nudge.Add(time.Minute)
	}
	return progress.Line{
		TS:              logged,
		Rig:             "gastown",
		Polecat:         "capable",
		Bead:            c.bead,
		Session:         "gt-capable",
		NudgeAt:         c.nudge,
		Model:           "jev-test",
		Answers:         progressAnswers(0.8, 0.1, 0.1, 0.1, 0.5),
		QuestionVersion: progress.Version,
	}
}

// progressAnswers is one line's answers: the four noul probabilities and the
// remaining score.
func progressAnswers(making, looping, blocked, drifted, remaining float64) map[string]progress.Answer {
	noul := func(p float64) progress.Answer { return progress.Answer{Noul: &p} }
	score := remaining
	return map[string]progress.Answer{
		progress.QMakingProgress:       noul(making),
		progress.QLooping:              noul(looping),
		progress.QBlockedOnEnvironment: noul(blocked),
		progress.QDriftedFromBead:      noul(drifted),
		progress.QRemaining:            {Score: &score},
	}
}

// writeProgressTown writes the cases' landing notes, overrun records and log
// lines into a town, and returns the notes reader that serves the landings.
func writeProgressTown(t *testing.T, town string, cases []progressCase) fakeProgressNotes {
	t.Helper()
	notes := fakeProgressNotes{notes: map[string]string{}}
	var logLines []byte
	for _, c := range cases {
		if !c.landed.IsZero() {
			notes.notes[c.bead] = land.LandingRecord{LandedCommit: "abc123", LandedAt: c.landed}.NoteBlock()
		}
		if !c.stop.StoppedAt.IsZero() || c.stop.GateSpent > 0 {
			writeOverrunStop(t, town, c.bead, c.stop)
		}
		raw, err := json.Marshal(progressLine(c))
		if err != nil {
			t.Fatal(err)
		}
		logLines = append(append(logLines, raw...), '\n')
	}
	path := progress.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, logLines, 0o600); err != nil {
		t.Fatal(err)
	}
	return notes
}

func writeOverrunStop(t *testing.T, town, bead string, st progressStop) {
	t.Helper()
	dir := filepath.Join(constants.TownRuntimePath(town), "overrun-guard")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bead+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The label rules, one nudge per case, each on its own bead.
func TestProgressLabelRules(t *testing.T) {
	t.Parallel()
	at := func(d time.Duration) time.Time { return progressBase.Add(d) }
	cases := []progressCase{
		{
			name:     "landed inside the window is progress and done or nearly done",
			bead:     "gt-p01",
			nudge:    progressBase,
			landed:   at(15 * time.Minute),
			want:     map[QuestionID]bool{qMakingProgress: true, qRemaining: true},
			wantRule: ruleProgressLanded,
		},
		{
			name:     "a landing at the window's end still counts",
			bead:     "gt-p02",
			nudge:    progressBase,
			landed:   at(20 * time.Minute),
			want:     map[QuestionID]bool{qMakingProgress: true, qRemaining: true},
			wantRule: ruleProgressLanded,
		},
		{
			name:   "a landing after the window does not count as progress",
			bead:   "gt-p03",
			nudge:  progressBase,
			landed: at(21 * time.Minute),
		},
		{
			name:   "a landing before the nudge is an earlier sling",
			bead:   "gt-p04",
			nudge:  progressBase,
			landed: at(-5 * time.Minute),
		},
		{
			name:     "a stop at or after the nudge is no progress",
			bead:     "gt-p05",
			nudge:    progressBase,
			stop:     progressStop{StoppedAt: at(20 * time.Minute)},
			want:     map[QuestionID]bool{qMakingProgress: false},
			wantRule: ruleProgressStopped,
		},
		{
			name:  "a stop from an earlier sling does not label this nudge",
			bead:  "gt-p06",
			nudge: progressBase,
			stop:  progressStop{StoppedAt: at(-time.Hour)},
		},
		{
			name:     "gate time charged since the nudge is blocked on the environment",
			bead:     "gt-p07",
			nudge:    progressBase,
			stop:     progressStop{GateSpent: 4 * time.Minute, GateSeenAt: at(5 * time.Minute)},
			want:     map[QuestionID]bool{qBlockedOnEnvironment: true},
			wantRule: ruleProgressGateWait,
		},
		{
			name:  "gate time seen only before the nudge does not label",
			bead:  "gt-p08",
			nudge: progressBase,
			stop:  progressStop{GateSpent: 4 * time.Minute, GateSeenAt: at(-time.Minute)},
		},
		{
			name:     "a landing and a gate wait both label, joined in order",
			bead:     "gt-p09",
			nudge:    progressBase,
			landed:   at(10 * time.Minute),
			stop:     progressStop{GateSpent: time.Minute, GateSeenAt: at(2 * time.Minute)},
			want:     map[QuestionID]bool{qMakingProgress: true, qRemaining: true, qBlockedOnEnvironment: true},
			wantRule: ruleProgressLanded + "+" + ruleProgressGateWait,
		},
		{
			name:  "a bead with no landing and no record is unlabeled",
			bead:  "gt-p10",
			nudge: progressBase,
		},
		{
			name:  "a line younger than 30 minutes stays unlabeled",
			bead:  "gt-p11",
			nudge: progressBase,
			// The line is logged 10 minutes before the scorecard runs, while
			// its landing is already on the bead.
			landed: at(15 * time.Minute),
			logged: progressNow.Add(-10 * time.Minute),
		},
	}

	town := t.TempDir()
	notes := writeProgressTown(t, town, cases)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newProgressTestAdapter(town, notes, progressNow)
			item, err := a.Parse(mustMarshal(t, progressLine(c)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			lab, err := a.Label(context.Background(), item)
			if err != nil {
				t.Fatalf("Label: %v", err)
			}
			if c.want == nil {
				if lab.Labeled || lab.Labels != nil {
					t.Errorf("labeled %v by %q, want unlabeled", lab.Labels, lab.Rule)
				}
				return
			}
			if !lab.Labeled || !reflect.DeepEqual(lab.Labels, c.want) || lab.Rule != c.wantRule {
				t.Errorf("label = %+v, want labels %v by rule %q", lab, c.want, c.wantRule)
			}
		})
	}
}

// A bead the store no longer has is unlabeled, not an error.
func TestProgressBeadNotFoundIsUnlabeled(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c := progressCase{bead: "gt-gone", nudge: progressBase}
	notes := writeProgressTown(t, town, []progressCase{c})
	a := newProgressTestAdapter(town, notes, progressNow)
	item, err := a.Parse(mustMarshal(t, progressLine(c)))
	if err != nil {
		t.Fatal(err)
	}
	lab, err := a.Label(context.Background(), item)
	if err != nil || lab.Labeled {
		t.Errorf("Label = %+v, %v; want unlabeled and no error", lab, err)
	}
}

// A notes read that fails for any reason other than a missing bead fails the
// report, so a broken source cannot pass as no labels yet.
func TestProgressNotesErrorFailsLabel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c := progressCase{bead: "gt-p20", nudge: progressBase}
	writeProgressTown(t, town, []progressCase{c})
	a := newProgressTestAdapter(town, fakeProgressNotes{err: errors.New("bead store unavailable")}, progressNow)
	item, err := a.Parse(mustMarshal(t, progressLine(c)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Label(context.Background(), item); err == nil {
		t.Error("Label returned no error for a failed notes read")
	}
}

// Lines the scorecard cannot compare are skipped at Parse: a nudge that was not
// asked, an older question set, and a bead id that could leave the runtime
// directory.
func TestProgressParseSkipsUnusableLines(t *testing.T) {
	t.Parallel()
	good := progressLine(progressCase{bead: "gt-p30", nudge: progressBase})
	cases := map[string]func(l *progress.Line){
		"not asked":       func(l *progress.Line) { l.Answers = nil; l.Reason = progress.ReasonAskFailed },
		"older questions": func(l *progress.Line) { l.QuestionVersion = progress.Version - 1 },
		"bead escapes":    func(l *progress.Line) { l.Bead = "../gt-p30" },
		"no nudge time":   func(l *progress.Line) { l.NudgeAt = time.Time{} },
	}
	a := newProgressTestAdapter(t.TempDir(), fakeProgressNotes{}, progressNow)
	for name, mutate := range cases {
		l := good
		mutate(&l)
		if _, err := a.Parse(mustMarshal(t, l)); err == nil {
			t.Errorf("%s: Parse accepted the line, want an error", name)
		}
	}
}

// The text scorecard for the progress battery, one block per question, over a
// fixture log of nudges whose beads have landed, stopped, gated or nothing.
func TestProgressScorecardGolden(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	at := func(d time.Duration) time.Time { return progressBase.Add(d) }
	cases := []progressCase{
		{name: "landed", bead: "gt-g1", nudge: progressBase, landed: at(12 * time.Minute)},
		{name: "landed late", bead: "gt-g2", nudge: progressBase, landed: at(40 * time.Minute)},
		{name: "stopped", bead: "gt-g3", nudge: progressBase, stop: progressStop{StoppedAt: at(20 * time.Minute)}},
		{name: "gated", bead: "gt-g4", nudge: progressBase, stop: progressStop{GateSpent: 3 * time.Minute, GateSeenAt: at(4 * time.Minute)}},
		{name: "quiet", bead: "gt-g5", nudge: progressBase},
		{name: "young", bead: "gt-g6", nudge: progressBase, logged: progressNow.Add(-5 * time.Minute), landed: at(9 * time.Minute)},
	}
	notes := writeProgressTown(t, town, cases)

	rep, err := Build(context.Background(), newProgressTestAdapter(town, notes, progressNow), time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var out bytes.Buffer
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "progress_scorecard.golden")
	if *updateProgressScorecardGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-progress-scorecard-golden):\n%s", golden, out.String())
	}
}

func TestProgressRegistered(t *testing.T) {
	t.Parallel()
	a, err := Open(ProgressBattery, t.TempDir())
	if err != nil {
		t.Fatalf("Open(%q): %v", ProgressBattery, err)
	}
	if a.Battery() != ProgressBattery || a.LogPath() != progress.LogPath(a.(*progressAdapter).town) {
		t.Errorf("adapter = %q at %q, want the progress battery at its log", a.Battery(), a.LogPath())
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
