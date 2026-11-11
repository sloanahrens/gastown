package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/jev/testsel"
)

var updateJevScorecardGolden = flag.Bool("update-jev-scorecard-golden", false, "rewrite testdata/jev_scorecard_fixture.golden")

const jevScorecardGoldenPath = "jev_scorecard_fixture.golden"

// writeScorecardFixture puts the scorecard package's fixture under town, the
// layout the fixture adapter reads.
// scorecardFixtureWritten stamps the fixture log five minutes before the clock
// the scorecard tests judge it at, so its copy line never carries the wall-clock
// time the file was written.
var scorecardFixtureWritten = time.Date(2026, 10, 20, 11, 55, 0, 0, time.UTC)

// writeScorecardFixture writes the fixture battery's log and stamps it at
// scorecardFixtureWritten.
func writeScorecardFixture(t *testing.T, town string) {
	t.Helper()
	if err := scorecard.WriteFixture(town, scorecard.FixtureLines, scorecard.FixtureLabels); err != nil {
		t.Fatal(err)
	}
	jevSetModTime(t, scorecard.FixtureLogPath(town), scorecardFixtureWritten)
}

// The text report prints one block per question in a fixed layout.
func TestScorecardFixtureGolden(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)

	var out bytes.Buffer
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", false, false, now); err != nil {
		t.Fatal(err)
	}
	// The copy line names the absolute fixture log path, and the town is a temp
	// directory, so the golden holds the town as <town>.
	got := strings.ReplaceAll(out.String(), town, "<town>")

	golden := filepath.Join("testdata", jevScorecardGoldenPath)
	if *updateJevScorecardGolden {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-jev-scorecard-golden):\n%s\nwant:\n%s", golden, got, want)
	}
}

// --json prints the same report as the text, as a decodable object.
func TestScorecardFixtureJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)

	var out bytes.Buffer
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", true, false, now); err != nil {
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
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "3h", true, false, now); err != nil {
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
	err := jevScorecardWithFlags(context.Background(), &out, t.TempDir(), "no-such-battery", "", false, false, time.Now())
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

// writeTestselLog writes a testsel log of n changes, each asking about
// pkgs packages, so the report holds n*pkgs items.
func writeTestselLog(t *testing.T, town string, n, pkgs int) {
	t.Helper()
	var lines []string
	for i := range n {
		noul, gate := map[string]float64{}, map[string]bool{}
		for p := range pkgs {
			dir := "internal/pkg" + strings.Repeat("x", p%5) + string(rune('a'+p%26)) + strconv.Itoa(p)
			noul[dir] = float64(p%10) / 10
			if p%3 != 0 {
				gate[dir] = p%7 != 0
			}
		}
		data, err := json.Marshal(map[string]any{
			"ts": time.Date(2026, 10, 18, 0, i, 0, 0, time.UTC), "bead": "gt-t" + strconv.Itoa(i), "rig": "gastown",
			"noul": noul, "gate": gate, "question_version": 1, "model": "m1",
		})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	path := testsel.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The testsel report is one row per asked package, thousands a week. Its --json
// leaves the rows out and keeps the totals; --lines brings them back.
func TestScorecardTestselJSONLeavesItemRowsOutUnlessAsked(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// The live log: 142 changes of about 63 packages each, 8946 items in a week.
	writeTestselLog(t, town, 142, 63)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	var lean, full bytes.Buffer
	if err := jevScorecardWithFlags(context.Background(), &lean, town, scorecard.TestselBattery, "", true, false, now); err != nil {
		t.Fatal(err)
	}
	if err := jevScorecardWithFlags(context.Background(), &full, town, scorecard.TestselBattery, "", true, true, now); err != nil {
		t.Fatal(err)
	}
	if lean.Len() > 100*1024 {
		t.Errorf("the testsel --json report is %d bytes for %d items, want under 100 KB", lean.Len(), 142*63)
	}

	var leanRep, fullRep scorecard.Report
	if err := json.Unmarshal(lean.Bytes(), &leanRep); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(full.Bytes(), &fullRep); err != nil {
		t.Fatal(err)
	}
	if len(leanRep.Lines) != 0 || !leanRep.LinesOmitted {
		t.Errorf("lean report has %d rows, lines_omitted %v; want none and true", len(leanRep.Lines), leanRep.LinesOmitted)
	}
	if len(fullRep.Lines) != fullRep.Items || fullRep.LinesOmitted {
		t.Errorf("--lines report has %d rows for %d items, lines_omitted %v", len(fullRep.Lines), fullRep.Items, fullRep.LinesOmitted)
	}
	labeled := 0
	for _, l := range fullRep.Lines {
		if l.Labeled {
			labeled++
		}
	}
	if leanRep.Items != 142*63 || leanRep.Labeled != labeled || fullRep.Labeled != labeled || labeled == 0 {
		t.Errorf("totals: lean items %d labeled %d, full labeled %d, rows labeled %d; want 8946 items and one labeled count",
			leanRep.Items, leanRep.Labeled, fullRep.Labeled, labeled)
	}
	if len(leanRep.Questions) != len(fullRep.Questions) || len(leanRep.Context) != len(fullRep.Context) {
		t.Errorf("the lean report lost questions or context")
	}
}

// A battery that is not bulky keeps its rows, and the report's labeled total
// is the count of its labeled rows.
func TestScorecardJSONLabeledTotalMatchesRows(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)
	var out bytes.Buffer
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", true, false, now); err != nil {
		t.Fatal(err)
	}
	var rep scorecard.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	labeled := 0
	for _, l := range rep.Lines {
		if l.Labeled {
			labeled++
		}
	}
	if len(rep.Lines) != rep.Items || rep.LinesOmitted || rep.Labeled != labeled || labeled == 0 {
		t.Errorf("rows %d of %d items, omitted %v, labeled total %d, labeled rows %d", len(rep.Lines), rep.Items, rep.LinesOmitted, rep.Labeled, labeled)
	}
}

// TestJevScorecardPrintsTheLogCopyAge names the one log the report read, not the
// newest write across the town's logs, so a score read from a copy other than
// the daemon's says which file it came from (gt-bgimh9).
func TestJevScorecardPrintsTheLogCopyAge(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// A newer write in another battery's log must not become the copy line.
	other := filepath.Join(town, ".runtime", "jev", "dedupe.jsonl")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	jevSetModTime(t, other, now.Add(-time.Minute))

	var out bytes.Buffer
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", false, false, now); err != nil {
		t.Fatal(err)
	}
	want := "log copy: newest write 2026-10-20 11:55Z (5m ago) at " + scorecard.FixtureLogPath(town)
	if !strings.Contains(out.String(), want) {
		t.Errorf("output does not carry %q:\n%s", want, out.String())
	}
}

// TestJevScorecardWarnsWhenItsLogHasNeverBeenWritten covers the battery that has
// written nothing: no file is the warning form, with copy_newest_write null.
func TestJevScorecardWarnsWhenItsLogHasNeverBeenWritten(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	var out bytes.Buffer
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", true, false, now); err != nil {
		t.Fatal(err)
	}
	var doc jevScorecardDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.CopyNewestWrite != nil || !doc.CopyStale {
		t.Errorf("copy_newest_write = %+v, copy_stale = %v; want null and true", doc.CopyNewestWrite, doc.CopyStale)
	}
}

// TestJevScorecardWarnsWhenTheCopyHasGoneQuiet warns when the battery's log is
// older than three passes of an enabled quarter-hour battery. The score is
// printed as it is, and --json names the copy as stale (gt-bgimh9).
func TestJevScorecardWarnsWhenTheCopyHasGoneQuiet(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	town := newJevHealthTown(t, now, 5*time.Minute)
	writeScorecardFixture(t, town)

	var out bytes.Buffer
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", false, false, now); err != nil {
		t.Fatal(err)
	}
	want := "log copy: WARNING: newest write 2026-10-20 11:55Z (24h ago) at " + scorecard.FixtureLogPath(town)
	if !strings.Contains(out.String(), want) {
		t.Errorf("output does not carry %q:\n%s", want, out.String())
	}

	var js bytes.Buffer
	if err := jevScorecardWithFlags(context.Background(), &js, town, scorecard.FixtureBattery, "", true, false, now); err != nil {
		t.Fatal(err)
	}
	var doc jevScorecardDoc
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.CopyStale || doc.CopyWarning != "" {
		t.Errorf("copy_stale = %v, copy_warning = %q; want true and no read warning", doc.CopyStale, doc.CopyWarning)
	}
}

// TestJevScorecardStillPrintsNormalOutputWhenTheAgeReadFails covers the copy's
// advisory path: a patrol config that does not parse is the line's warning and
// the copy_warning field, and the report prints as it does with a good config.
func TestJevScorecardStillPrintsNormalOutputWhenTheAgeReadFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clean := t.TempDir()
	writeScorecardFixture(t, clean)
	broken := t.TempDir()
	writeScorecardFixture(t, broken)
	path := daemon.PatrolConfigFile(broken)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	var want, got bytes.Buffer
	if err := jevScorecardWithFlags(context.Background(), &want, clean, scorecard.FixtureBattery, "", false, false, now); err != nil {
		t.Fatal(err)
	}
	if err := jevScorecardWithFlags(context.Background(), &got, broken, scorecard.FixtureBattery, "", false, false, now); err != nil {
		t.Fatalf("the report must still print: %v", err)
	}
	firstLine, rest, _ := strings.Cut(got.String(), "\n")
	if !strings.Contains(firstLine, "log copy: WARNING: cannot be read: reading the patrol config") {
		t.Errorf("first line = %q, want the copy warning", firstLine)
	}
	_, wantRest, _ := strings.Cut(want.String(), "\n")
	if rest != wantRest {
		t.Errorf("report differs from the clean run beneath the copy line:\n--- clean\n%s\n--- broken\n%s", wantRest, rest)
	}

	var js bytes.Buffer
	if err := jevScorecardWithFlags(context.Background(), &js, broken, scorecard.FixtureBattery, "", true, false, now); err != nil {
		t.Fatal(err)
	}
	var doc jevScorecardDoc
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.CopyNewestWrite != nil || !doc.CopyStale || !strings.Contains(doc.CopyWarning, "reading the patrol config") {
		t.Errorf("copy fields = %+v, stale %v, warning %q; want null, true and the config warning", doc.CopyNewestWrite, doc.CopyStale, doc.CopyWarning)
	}
	if doc.Battery != scorecard.FixtureBattery {
		t.Errorf("battery = %q, want the report's own field", doc.Battery)
	}
}

// TestJevScorecardRefusesAnUnreadableBatteryLog covers the battery's own log
// being unreadable: the report is built from that log, so the report fails. The
// copy line does not decide this; the score does.
func TestJevScorecardRefusesAnUnreadableBatteryLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)
	// The fixture's own log becomes a symlink to itself: the stat fails with
	// ELOOP, not with "not there".
	logPath := scorecard.FixtureLogPath(town)
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(logPath), logPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", false, false,
		time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), filepath.Base(logPath)) {
		t.Fatalf("err = %v, want the copy read's failure", err)
	}
}

// TestJevScorecardReportsAWriteFailure covers the copy line's write: a writer
// that refuses is an error, not a report printed without the copy it came from.
func TestJevScorecardReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)

	err := jevScorecardWithFlags(context.Background(), jevHealthFailingWriter{}, town, scorecard.FixtureBattery, "", false, false,
		time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("want the writer's failure")
	}
}
