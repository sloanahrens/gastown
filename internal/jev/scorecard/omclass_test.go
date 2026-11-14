package scorecard

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/omclass"
)

// omclassFixture is one finding's line: its keyword class and the class Jev
// picked for it.
func omclassFixture(bead string, finding int, keyword, pick string) omclass.Line {
	return omclass.Line{
		TS: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), At: time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC),
		Rig: "gastown", Bead: bead, Attempt: 1, Finding: finding,
		Keyword:         keyword,
		Answers:         map[string]omclass.Answer{omclass.QRuleClass: {Choice: pick}},
		Model:           "jev-test",
		QuestionVersion: omclass.Version,
	}
}

func TestOMClassBatteryIsRegistered(t *testing.T) {
	t.Parallel()
	a, err := Open(OMClassBattery, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if a.Battery() != OMClassBattery {
		t.Errorf("Battery = %q, want %q", a.Battery(), OMClassBattery)
	}
	if !slices.Contains(Registered(), OMClassBattery) {
		t.Errorf("Registered() = %v lacks %q", Registered(), OMClassBattery)
	}
}

// TestOMClassContextReportsAgreementPerKeyword reads four findings: two whose
// pick agrees with the keyword class and two whose pick does not. The report
// names the agreement overall and per keyword class, and what Jev picks instead.
func TestOMClassContextReportsAgreementPerKeyword(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	lines := []omclass.Line{
		omclassFixture("gt-a", 0, "fail-open", "fail-open"),
		omclassFixture("gt-a", 1, "fail-open", "silent-drop"),
		omclassFixture("gt-b", 0, omclass.NoneLabel, "fail-open"),
		omclassFixture("gt-b", 1, omclass.NoneLabel, omclass.NoneLabel),
	}
	for _, l := range lines {
		if err := battery.Append(omclass.LogPath(town), l); err != nil {
			t.Fatal(err)
		}
	}
	a, err := Open(OMClassBattery, town)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.Labeled != 0 {
		t.Errorf("Labeled = %d, want 0: the keyword class is not a yes or a no", rep.Labeled)
	}
	want := []string{
		"4 findings, Jev agrees with the keyword class on 2 (50%)",
		"keyword fail-open: 2 findings, Jev agrees 1 (50%); Jev picks instead: silent-drop 1",
		"keyword none: 2 findings, Jev agrees 1 (50%); Jev picks instead: fail-open 1",
	}
	if !slices.Equal(rep.Context, want) {
		t.Errorf("Context =\n%q\nwant\n%q", rep.Context, want)
	}
}

// TestOMClassParseRefusesLinesItCannotCompare names each line Parse skips: one
// with no keyword to compare with, a question set this scorecard cannot read,
// or no class answer.
func TestOMClassParseRefusesLinesItCannotCompare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*omclass.Line)
	}{
		{"no keyword", func(l *omclass.Line) { l.Keyword = "" }},
		{"question set newer than the scorecard", func(l *omclass.Line) { l.QuestionVersion = omclass.Version + 1 }},
		{"question set unset", func(l *omclass.Line) { l.QuestionVersion = 0 }},
		{"no class answer", func(l *omclass.Line) { l.Answers = map[string]omclass.Answer{} }},
		{"class answer with no choice", func(l *omclass.Line) { l.Answers[omclass.QRuleClass] = omclass.Answer{} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l := omclassFixture("gt-a", 0, "fail-open", "fail-open")
			c.mutate(&l)
			raw, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			a := newOMClassAdapter(omclass.LogPath(t.TempDir()))
			if _, err := a.Parse(raw); err == nil {
				t.Error("Parse accepted a line it cannot compare")
			}
		})
	}
}
