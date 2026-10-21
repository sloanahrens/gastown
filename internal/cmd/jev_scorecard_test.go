package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

var updateJevScorecardGolden = flag.Bool("update-jev-scorecard-golden", false, "rewrite testdata/jev_scorecard_fixture.golden")

const jevScorecardGoldenPath = "jev_scorecard_fixture.golden"

// writeScorecardFixture puts the scorecard package's fixture under town, the
// layout the fixture adapter reads.
func writeScorecardFixture(t *testing.T, town string) {
	t.Helper()
	if err := scorecard.WriteFixture(town, scorecard.FixtureLines, scorecard.FixtureLabels); err != nil {
		t.Fatal(err)
	}
}

// The text report prints one block per question in a fixed layout.
func TestScorecardFixtureGolden(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)

	var out bytes.Buffer
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", false, now); err != nil {
		t.Fatal(err)
	}

	golden := filepath.Join("testdata", jevScorecardGoldenPath)
	if *updateJevScorecardGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-jev-scorecard-golden):\n%s\nwant:\n%s", golden, out.String(), want)
	}
}

// --json prints the same report as the text, as a decodable object.
func TestScorecardFixtureJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)

	var out bytes.Buffer
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", true, now); err != nil {
		t.Fatal(err)
	}

	var rep scorecard.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decoding the JSON report: %v\n%s", err, out.String())
	}
	if rep.Battery != scorecard.FixtureBattery || rep.Items != 4 || rep.LinesSkipped != 1 {
		t.Errorf("report = battery %q items %d skipped %d, want fixture, 4 and 1", rep.Battery, rep.Items, rep.LinesSkipped)
	}
	if len(rep.Questions) != 3 {
		t.Fatalf("report has %d questions, want 3", len(rep.Questions))
	}
	for _, q := range rep.Questions {
		if q.Logged != 4 || q.Labeled != 3 {
			t.Errorf("%s logged %d labeled %d, want 4 and 3", q.ID, q.Logged, q.Labeled)
		}
		if q.Verdict != scorecard.VerdictNotYet {
			t.Errorf("%s verdict %s, want not-yet on 3 labeled lines", q.ID, q.Verdict)
		}
	}
}

// --since keeps only the items stamped inside the window.
func TestScorecardSinceNarrowsItems(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)

	var out bytes.Buffer
	// Ending 2026-10-18 12:30 with a 3h window starts at 09:30, so fx-1 is out.
	now := time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "3h", true, now); err != nil {
		t.Fatal(err)
	}
	var rep scorecard.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Items != 3 {
		t.Errorf("items = %d inside the window, want 3 (fx-2, fx-3, fx-4)", rep.Items)
	}
}

func TestScorecardUnknownBatteryListsRegistered(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := jevScorecardWithFlags(context.Background(), &out, t.TempDir(), "no-such-battery", "", false, time.Now())
	if err == nil {
		t.Fatal("an unknown battery exited cleanly")
	}
	if !strings.Contains(err.Error(), `"no-such-battery"`) || !strings.Contains(err.Error(), "finding, landing") {
		t.Errorf("error %q does not name the battery and list the registered ones", err)
	}
	// The test-only fixture battery is not offered to the operator.
	if strings.Contains(err.Error(), scorecard.FixtureBattery) {
		t.Errorf("error %q lists the test-only %q battery", err, scorecard.FixtureBattery)
	}
}

func TestScorecardParseSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	good := []struct {
		in   string
		want time.Time
	}{
		{"", time.Time{}},
		{"14d", now.Add(-14 * 24 * time.Hour)},
		{"1d", now.Add(-24 * time.Hour)},
		{"36h", now.Add(-36 * time.Hour)},
	}
	for _, tc := range good {
		got, err := parseSince(tc.in, now)
		if err != nil {
			t.Errorf("parseSince(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseSince(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{"0d", "-1d", "soon", "1.5d", "0h"} {
		if _, err := parseSince(in, now); err == nil {
			t.Errorf("parseSince(%q) accepted a bad window", in)
		}
	}
}
