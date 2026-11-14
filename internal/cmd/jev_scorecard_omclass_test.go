package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/omclass"
)

// omclassLine is one asked finding's log line: its keyword class and Jev's pick.
func omclassLine(bead string, finding int, keyword, pick string) omclass.Line {
	return omclass.Line{
		TS: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), At: time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC),
		Rig: "gastown", Bead: bead, Attempt: 1, Finding: finding,
		Keyword:         keyword,
		Answers:         map[string]omclass.Answer{omclass.QRuleClass: {Choice: pick}},
		Model:           "jev-test",
		QuestionVersion: omclass.Version,
	}
}

// TestJevScorecardOMClass scores a fixture log through the command: the output
// names the agreement with the keyword class overall and per class, and what
// Jev picks where it differs.
func TestJevScorecardOMClass(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, l := range []omclass.Line{
		omclassLine("gt-a", 0, "fail-open", "fail-open"),
		omclassLine("gt-a", 1, "fail-open", "silent-drop"),
		omclassLine("gt-b", 0, omclass.NoneLabel, "fail-open"),
		omclassLine("gt-b", 1, omclass.NoneLabel, omclass.NoneLabel),
	} {
		if err := battery.Append(omclass.LogPath(town), l); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, "omclass", "", false, false, now); err != nil {
		t.Fatalf("scorecard: %v", err)
	}
	for _, want := range []string{
		"4 findings, Jev agrees with the keyword class on 2 (50%)",
		"keyword fail-open: 2 findings, Jev agrees 1 (50%); Jev picks instead: silent-drop 1",
		"keyword none: 2 findings, Jev agrees 1 (50%); Jev picks instead: fail-open 1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
