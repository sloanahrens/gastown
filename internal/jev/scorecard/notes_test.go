package scorecard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/notes"
)

// notesLine is a logged read of one memory note: the label it earned, the
// missing tokens behind it, and the three answers.
func notesLine(file, label string, missing []notes.Token, still, removed float64, kind string) string {
	l := notes.Line{
		TS:      time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC),
		Source:  notes.SourceMemory,
		File:    file,
		Hash:    "h-" + file,
		Label:   label,
		Missing: missing,
		Model:   "jev-test",
		Answers: map[string]notes.Answer{
			notes.QStillDescribes: {Noul: &still},
			notes.QNamesRemoved:   {Noul: &removed},
			notes.QKind:           {Choice: kind, Probabilities: map[string]float64{kind: 0.9}},
		},
		QuestionVersion: notes.Version,
	}
	raw, err := json.Marshal(l)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func writeNotesLog(t *testing.T, town string, lines ...string) {
	t.Helper()
	path := notes.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The adapter reports the three questions against the mechanical label: stale
// sets still_describes false and names_something_removed true; current sets
// them the other way round; an unlabeled file counts as logged only.
func TestNotesScorecardReportsTheThreeQuestionsWithTheLabel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	gone := []notes.Token{{Kind: notes.KindPath, Text: "internal/jev/stale/old.go"}}
	writeNotesLog(t, town,
		notesLine("stale-right.md", notes.LabelStale, gone, 0.1, 0.9, notes.KindPath),
		notesLine("stale-wrong.md", notes.LabelStale, gone, 0.8, 0.2, notes.KindNone),
		notesLine("current-right.md", notes.LabelCurrent, nil, 0.9, 0.1, notes.KindNone),
		notesLine("current-wrong.md", notes.LabelCurrent, nil, 0.2, 0.7, notes.KindFlag),
		notesLine("prose.md", "", nil, 0.5, 0.5, notes.KindUnclear),
	)

	a, err := Open(NotesBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Battery != NotesBattery || rep.Items != 5 || rep.Labeled != 4 {
		t.Fatalf("report = battery %q, %d items, %d labeled; want notes, 5 items, 4 labeled", rep.Battery, rep.Items, rep.Labeled)
	}
	want := map[string]struct{ logged, labeled, positives, negatives int }{
		notes.QStillDescribes: {5, 4, 2, 2},
		notes.QNamesRemoved:   {5, 4, 2, 2},
		notes.QKind:           {5, 4, 2, 2},
	}
	if len(rep.Questions) != len(want) {
		t.Fatalf("report has %d questions, want %d: %+v", len(rep.Questions), len(want), rep.Questions)
	}
	for _, q := range rep.Questions {
		w, ok := want[q.ID]
		if !ok {
			t.Errorf("unexpected question %q", q.ID)
			continue
		}
		if q.Logged != w.logged || q.Labeled != w.labeled || q.Positives != w.positives || q.Negatives != w.negatives {
			t.Errorf("%s = logged %d, labeled %d, +%d/-%d; want %+v", q.ID, q.Logged, q.Labeled, q.Positives, q.Negatives, w)
		}
	}
	for _, q := range rep.Questions {
		if q.ID == notes.QKind && q.Kind != KindChoice {
			t.Errorf("kind is scored as %q, want a choice", q.Kind)
		}
	}
}

// kind is labeled true when the option Jev picked is the kind of a missing
// token on a stale file, and true only for none on a current one.
func TestNotesLabelRules(t *testing.T) {
	t.Parallel()
	gone := []notes.Token{{Kind: notes.KindPath, Text: "docs/gone.md"}, {Kind: notes.KindFlag, Text: "gt done --gone"}}
	tests := []struct {
		name    string
		label   string
		missing []notes.Token
		picked  string
		want    map[QuestionID]bool
		rule    string
	}{
		{"stale, picked a missing kind", notes.LabelStale, gone, notes.KindFlag,
			map[QuestionID]bool{qStillDescribes: false, qNamesRemoved: true, qNotesKind: true}, ruleNotesStale},
		{"stale, picked another kind", notes.LabelStale, gone, notes.KindCommand,
			map[QuestionID]bool{qStillDescribes: false, qNamesRemoved: true, qNotesKind: false}, ruleNotesStale},
		{"stale, picked none", notes.LabelStale, gone, notes.KindNone,
			map[QuestionID]bool{qStillDescribes: false, qNamesRemoved: true, qNotesKind: false}, ruleNotesStale},
		{"current, picked none", notes.LabelCurrent, nil, notes.KindNone,
			map[QuestionID]bool{qStillDescribes: true, qNamesRemoved: false, qNotesKind: true}, ruleNotesCurrent},
		{"current, picked a kind", notes.LabelCurrent, nil, notes.KindPath,
			map[QuestionID]bool{qStillDescribes: true, qNamesRemoved: false, qNotesKind: false}, ruleNotesCurrent},
		{"current, picked unclear", notes.LabelCurrent, nil, notes.KindUnclear,
			map[QuestionID]bool{qStillDescribes: true, qNamesRemoved: false, qNotesKind: false}, ruleNotesCurrent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &notesAdapter{lines: map[string]notes.Line{}}
			item, err := a.Parse([]byte(notesLine("n.md", tt.label, tt.missing, 0.5, 0.5, tt.picked)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := a.Label(context.Background(), item)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Labeled || got.Rule != tt.rule {
				t.Fatalf("labeling = %+v, want labeled by %s", got, tt.rule)
			}
			for id, want := range tt.want {
				if v, ok := got.Labels[id]; !ok || v != want {
					t.Errorf("%s = %v (set %v), want %v", id, v, ok, want)
				}
			}
		})
	}
}

func TestNotesUnlabeledFileIsLoggedButNotLabeled(t *testing.T) {
	t.Parallel()
	a := &notesAdapter{lines: map[string]notes.Line{}}
	item, err := a.Parse([]byte(notesLine("n.md", "", nil, 0.5, 0.5, notes.KindNone)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Label(context.Background(), item)
	if err != nil || got.Labeled {
		t.Errorf("labeling = %+v, %v; want unlabeled", got, err)
	}
}

// A line with no answers, one from another question set and one with no file
// are skipped and counted.
func TestNotesParseSkipsUnusableLines(t *testing.T) {
	t.Parallel()
	a := &notesAdapter{lines: map[string]notes.Line{}}
	for name, raw := range map[string]string{
		"no answers":    `{"ts":"2026-10-21T09:00:00Z","source":"memory","file":"n.md","hash":"h","answers":null,"question_version":1,"reason":"ask_failed"}`,
		"other version": `{"ts":"2026-10-21T09:00:00Z","source":"memory","file":"n.md","hash":"h","answers":{"kind":{"choice":"none"}},"question_version":0}`,
		"no file":       `{"ts":"2026-10-21T09:00:00Z","source":"memory","hash":"h","answers":{"kind":{"choice":"none"}},"question_version":1}`,
		"not json":      `{`,
	} {
		if _, err := a.Parse([]byte(raw)); err == nil {
			t.Errorf("%s: Parse accepted the line", name)
		}
	}
	if _, err := a.Label(context.Background(), Item{ID: "never-parsed"}); err == nil {
		t.Error("Label accepted an item the adapter did not parse")
	}
}

func TestNotesIsRegistered(t *testing.T) {
	t.Parallel()
	found := false
	for _, name := range Registered() {
		found = found || name == NotesBattery
	}
	if !found {
		t.Errorf("Registered() = %v, want notes among them", Registered())
	}
}
