package deepseekspend

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the tests under the hermetic harness, as every unit-tier package
// does: the environment is scrubbed and HOME is sandboxed.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// loc is a fixed offset, so the tests do not depend on the host's zone database.
var loc = time.FixedZone("CST", -6*60*60)

// fixtureInput is testdata/balance.log read at 01:20 on 2026-11-09, with the
// top-up moved to 23:00 on 2026-11-08 so the spend since it is not the ledger's.
func fixtureInput(t *testing.T) Input {
	t.Helper()
	text, err := os.ReadFile("testdata/balance.log")
	if err != nil {
		t.Fatal(err)
	}
	return Input{
		Constants: Constants{
			TopUpAt: time.Date(2026, time.November, 8, 23, 0, 0, 0, loc),
			TopUp:   114.05,
		},
		Rows:   ParseLedger(string(text), loc),
		Events: []string{"e1", "e2", "e3", "e4", "e5", "e6  "},
		Now:    time.Date(2026, time.November, 9, 1, 20, 0, 0, loc),
		Days:   DefaultDays,
	}
}

// The fixture's figures, worked out by hand from the tool's definitions:
//   - spent_ledger: the falls over all 11 samples are 1.00 + 1.00 + 1.50 +
//     1.50 + 1.00 + 0.50 + 1.50 + 1.00 + 0.50 = 9.50. The 10.00 rise at 23:50 is
//     a credit, not spend.
//   - spent_total: from 23:00 on the 8th the falls are 8.50; the 1.00 fall from
//     17:00 to 23:20 is before the top-up.
//   - pace_1h: samples from 00:20 are 00:22 to 01:12, 50 minutes, falls 3.00,
//     so 3.00 / (50/60) = 3.600 an hour.
//   - pace_6h: samples from 19:20 are 23:20 to 01:12, 112 minutes, falls 8.50,
//     so 8.50 / (112/60) = 4.554 an hour (4.5535...).
//   - pace_24h: every sample, 17:00 to 01:12, 492 minutes, falls 9.50, so
//     9.50 / 8.2 = 1.159 an hour (1.1585...).
//   - runway: 51.50 / (1.159 * 24) = 1.85..., 1.9 days.
//   - per_day: the 8th falls 1.00 + 1.00 + 1.50 = 3.50; the 9th falls 1.00 +
//     0.50 + 1.50 + 1.00 + 0.50 = 4.50. The 1.50 fall from 23:50 to 00:02 spans
//     midnight and is in no day.
//   - gaps: 17:00 to 23:20 (380 minutes, 1.00 spent) and 00:32 to 01:02 (30
//     minutes, 1.00 spent). The other pairs are 10 or 12 minutes apart.
func TestBuildReportFromFixture(t *testing.T) {
	t.Parallel()
	got := Build(fixtureInput(t))
	want := Report{
		Live:        Live{OK: false, Error: "no live balance in the dashboard"},
		Balance:     ptr(51.5),
		TopUp:       114.05,
		SpentTotal:  8.5,
		SpentLedger: 9.5,
		LedgerSince: ptr("2026-11-08T17:00"),
		Credits: []Credit{
			{At: "2026-11-08T23:50", Amount: 10, Kind: "credit"},
		},
		Samples:    11,
		LastSample: ptr("2026-11-09T01:12"),
		Pace1h:     &Pace{Spent: 3, PerHour: ptr(3.6)},
		Pace6h:     &Pace{Spent: 8.5, PerHour: ptr(4.554)},
		Pace24h:    &Pace{Spent: 9.5, PerHour: ptr(1.159)},
		RunwayDays: ptr(1.9),
		PerDay: map[string]Day{
			"2026-11-08": {Open: 51, Close: 57.5, Spent: 3.5},
			"2026-11-09": {Open: 56, Close: 51.5, Spent: 4.5},
		},
		Gaps: []Gap{
			{From: "2026-11-08T17:00", To: "2026-11-08T23:20", Minutes: 380, Spent: 1},
			{From: "2026-11-09T00:32", To: "2026-11-09T01:02", Minutes: 30, Spent: 1},
		},
		Alerts: []string{"e2", "e3", "e4", "e5", "e6"},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("report differs from the hand-computed fixture:\n%s", gotJSON)
	}
}

func TestLedgerParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		text      string
		wantRows  int
		wantBal   float64
		wantSplit bool
	}{
		{
			name:     "a row without the tail keeps its balance",
			text:     "2026-11-09T01:12:00 balance=51.50\n",
			wantRows: 1, wantBal: 51.5,
		},
		{
			name:     "a bad split keeps the row's balance and drops the split",
			text:     "2026-11-09T01:12:00 balance=51.50 granted=x topped=15.00\n",
			wantRows: 1, wantBal: 51.5,
		},
		{
			name:     "a malformed line is skipped and the next row is kept",
			text:     "garbage line\n2026-11-09T01:12:00 balance=abc\n2026-11-09T01:13:00 balance=51.50 granted=15 topped=45\n",
			wantRows: 1, wantBal: 51.5, wantSplit: true,
		},
		{
			name:     "a line with a bad timestamp is skipped",
			text:     "garbage balance=51.50\n2026-11-09T01:12:00 balance=51.50\n",
			wantRows: 1, wantBal: 51.5,
		},
		{
			name:     "a line with an empty balance is skipped",
			text:     "2026-11-09T01:11:00 balance=\n2026-11-09T01:12:00 balance=51.50\n",
			wantRows: 1, wantBal: 51.5,
		},
		{
			name: "an empty ledger has no rows",
			text: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := ParseLedger(tc.text, loc)
			if len(rows) != tc.wantRows {
				t.Fatalf("rows = %d, want %d", len(rows), tc.wantRows)
			}
			if tc.wantRows == 0 {
				report := Build(Input{Rows: rows})
				if report.Samples != 0 || report.Balance != nil || report.LastSample != nil || report.Live.OK {
					t.Errorf("an empty ledger gave %+v, want a zero report with no balance", report)
				}
				return
			}
			if rows[0].Balance != tc.wantBal {
				t.Errorf("balance = %v, want %v", rows[0].Balance, tc.wantBal)
			}
			if got := rows[0].Granted != nil && rows[0].Topped != nil; got != tc.wantSplit {
				t.Errorf("split present = %v, want %v", got, tc.wantSplit)
			}
		})
	}
}

func TestLedgerParsingReadsTimesInTheLocation(t *testing.T) {
	t.Parallel()
	rows := ParseLedger("2026-11-09T01:12:00 balance=51.50\n", loc)
	want := time.Date(2026, time.November, 9, 1, 12, 0, 0, loc)
	if !rows[0].At.Equal(want) {
		t.Errorf("at = %v, want %v (naive local time in the ledger's location)", rows[0].At, want)
	}
}

// The live read is never made in the pod, so the report says so and carries no
// cash, credit or watcher pid.
func TestReportHasNoLiveBalance(t *testing.T) {
	t.Parallel()
	report := Build(fixtureInput(t))
	out, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	live, ok := m["live"].(map[string]any)
	if !ok {
		t.Fatalf("live = %v, want an object", m["live"])
	}
	if live["ok"] != false || live["error"] != "no live balance in the dashboard" {
		t.Errorf("live = %v, want ok false with the no-live-balance error", live)
	}
	for _, key := range []string{"cash", "credit", "credit_runway_days_at_24h_pace", "watcher_pid"} {
		if v, present := m[key]; !present || v != nil {
			t.Errorf("%s = %v (present %v), want null", key, v, present)
		}
	}
}

// per_day keeps the latest days the window asks for, and a window of zero lists
// every day.
func TestPerDayKeepsTheLatestDays(t *testing.T) {
	t.Parallel()
	in := fixtureInput(t)
	in.Days = 1
	if got := Build(in).PerDay; len(got) != 1 || got["2026-11-09"].Open != 56 {
		t.Errorf("per_day with a one-day window = %v, want only 2026-11-09", got)
	}
	in.Days = 0
	if got := Build(in).PerDay; len(got) != 2 {
		t.Errorf("per_day with no window = %v, want both days", got)
	}
}

// A window needs two samples and a rate needs a span: without them the figure is
// null, not zero.
func TestPaceIsNullWithoutTwoSamplesOrASpan(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.November, 9, 1, 12, 0, 0, loc)
	one := Build(Input{Rows: []Row{{At: at, Balance: 51.5}}, Now: at.Add(time.Minute)})
	if one.Pace1h != nil || one.Pace24h != nil || one.RunwayDays != nil {
		t.Errorf("one sample: pace_1h %v, runway %v; want null", one.Pace1h, one.RunwayDays)
	}
	same := Build(Input{Rows: []Row{{At: at, Balance: 52}, {At: at, Balance: 51.5}}, Now: at})
	if same.Pace1h == nil || same.Pace1h.Spent != 0.5 || same.Pace1h.PerHour != nil {
		t.Errorf("zero span: pace_1h %+v; want the spend and no rate", same.Pace1h)
	}
}

// A gap is more than fifteen minutes: a pair exactly fifteen minutes apart is
// not one, and a pair a minute past it is.
func TestGapIsMoreThanFifteenMinutes(t *testing.T) {
	t.Parallel()
	ledger := "2026-11-09T01:00:00 balance=52.00\n" +
		"2026-11-09T01:15:00 balance=51.50\n" +
		"2026-11-09T01:31:00 balance=51.00\n"
	got := Build(Input{Rows: ParseLedger(ledger, loc), Now: time.Date(2026, time.November, 9, 1, 31, 0, 0, loc)}).Gaps
	want := []Gap{{From: "2026-11-09T01:15", To: "2026-11-09T01:31", Minutes: 16, Spent: 0.5}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gaps = %+v, want only the 16-minute pair", got)
	}
}

// countingFile counts the bytes a poll reads through it.
type countingFile struct {
	File
	bytes *int64
}

func (f countingFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	*f.bytes += int64(n)
	return n, err
}

// appendText adds text to the end of the file at path.
func appendText(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// TestIncrementalRead drives one reader through appends, a partial line, a file
// that shrank and a file that was replaced. Each poll's report equals a fresh
// reader's full read, and an append costs only its own bytes.
func TestIncrementalRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ledger := filepath.Join(dir, "balance.log")
	events := filepath.Join(dir, "events.log")
	now := time.Date(2026, time.November, 9, 1, 30, 0, 0, loc)
	constants := Constants{TopUpAt: time.Date(2026, time.November, 8, 23, 0, 0, 0, loc), TopUp: 114.05}

	var read int64
	open := func(name string) (File, error) {
		f, err := OpenFile(name)
		if err != nil {
			return nil, err
		}
		return countingFile{File: f, bytes: &read}, nil
	}
	reader := NewReader(ledger, events, loc, constants, DefaultDays, open)
	fullRead := func() Report {
		t.Helper()
		report, err := NewReader(ledger, events, loc, constants, DefaultDays, OpenFile).Report(now)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	poll := func(step string) Report {
		t.Helper()
		report, err := reader.Report(now)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if want := fullRead(); !reflect.DeepEqual(report, want) {
			t.Fatalf("%s: report differs from a full re-read", step)
		}
		return report
	}

	if err := os.WriteFile(ledger, []byte("2026-11-09T01:00:00 balance=52.00\n2026-11-09T01:10:00 balance=51.50\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(events, []byte("e1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := poll("first read").Samples; got != 2 {
		t.Fatalf("first read samples = %d, want 2", got)
	}

	appended := "2026-11-09T01:20:00 balance=51.00\n2026-11-09T01:25:00 balance=50.50\n"
	appendText(t, ledger, appended)
	read = 0
	if got := poll("two appended rows").Samples; got != 4 {
		t.Errorf("after appending two rows: samples = %d, want 4", got)
	}
	if read != int64(len(appended)) {
		t.Errorf("second poll read %d bytes, want only the %d appended", read, len(appended))
	}

	appendText(t, ledger, "2026-11-09T01:30:00 balance=5")
	if got := poll("partial line").Samples; got != 4 {
		t.Errorf("a line with no newline yet: samples = %d, want 4", got)
	}
	appendText(t, ledger, "0.00\n")
	if report := poll("completed line"); report.Samples != 5 || *report.Balance != 50 {
		t.Errorf("after the newline: samples %d, balance %v; want 5 and 50", report.Samples, *report.Balance)
	}

	shrunk := "2026-11-09T01:40:00 balance=49.00\n"
	if err := os.WriteFile(ledger, []byte(shrunk), 0o644); err != nil {
		t.Fatal(err)
	}
	read = 0
	if got := poll("shrunk file").Samples; got != 1 {
		t.Errorf("after the file shrank: samples = %d, want 1 (re-read from the start)", got)
	}
	if read != int64(len(shrunk)) {
		t.Errorf("shrunk file: read %d bytes, want the whole %d", read, len(shrunk))
	}

	replacement := "2026-11-09T02:00:00 balance=48.00\n2026-11-09T02:10:00 balance=47.00\n2026-11-09T02:20:00 balance=46.50\n"
	next := filepath.Join(dir, "balance.log.next")
	if err := os.WriteFile(next, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, ledger); err != nil {
		t.Fatal(err)
	}
	read = 0
	if got := poll("replaced file").Samples; got != 3 {
		t.Errorf("after the file was replaced: samples = %d, want 3", got)
	}
	if read != int64(len(replacement)) {
		t.Errorf("replaced file: read %d bytes, want the whole %d", read, len(replacement))
	}
}

// A ledger that cannot be opened is a failed read, not an empty report.
func TestReadFailsOnMissingLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reader := NewReader(filepath.Join(dir, "balance.log"), filepath.Join(dir, "events.log"), loc, Constants{}, DefaultDays, OpenFile)
	if _, err := reader.Report(time.Now()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing ledger: err = %v, want a not-exist error", err)
	}
}
