package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// deepSeekBalanceRow is one deepseek_balance row the way the balance patrol
// writes it: a total, the granted/topped split, and the alert lines as its note.
func deepSeekBalanceRow(at time.Time, balance float64, note string) beads.Sample {
	granted := 5.0
	topped := balance - granted
	return beads.Sample{
		Kind:     beads.SampleKindDeepSeekBalance,
		At:       at.UTC(),
		Total:    &balance,
		Granted:  &granted,
		ToppedUp: &topped,
		Note:     note,
	}
}

// deepSeekReport is the part of the spend report these tests read back.
type deepSeekReport struct {
	Samples    int      `json:"samples"`
	Balance    *float64 `json:"balance"`
	LastSample string   `json:"last_sample"`
	Alerts     []string `json:"alerts"`
}

// decodeDeepSeekReport decodes out as a spend report.
func decodeDeepSeekReport(t *testing.T, out json.RawMessage) deepSeekReport {
	t.Helper()
	if out == nil {
		t.Fatal("the spend read gave no report")
	}
	var r deepSeekReport
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("the spend report %q is not JSON: %v", out, err)
	}
	return r
}

// TestResolveSpendServesDeepSeekRowsBeforeTheLedger: the town's deepseek_balance
// rows answer the Cost panel's spend before the Mac's balance ledger, so a pod
// with no Mac file still draws the panel (gt-u3fp8i). The poll takes the whole
// series - the report's spent_ledger, credits and gaps are all-time figures -
// and folds it oldest first, so the newest row is the balance and the ledger's
// stamp. The why-log names the rows as the source that answered.
func TestResolveSpendServesDeepSeekRowsBeforeTheLedger(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home) // stands at 51.00 at 2026-11-09T01:12
	homeFn := func() (string, error) { return home, nil }

	newest := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	older := newest.Add(-10 * time.Minute)
	var kinds []string
	var sinces []time.Time
	var limits []int
	rows := func(kind string, since time.Time, limit int) ([]beads.Sample, error) {
		kinds = append(kinds, kind)
		sinces = append(sinces, since)
		limits = append(limits, limit)
		// Newest first, as bd lists them.
		return []beads.Sample{
			deepSeekBalanceRow(newest, 40.25, "balance low: $40.25"),
			deepSeekBalanceRow(older, 40.75, ""),
		}, nil
	}

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	spend, reason := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rows, whyLog)
	if spend == nil || reason != "" {
		t.Fatalf("rows wired: spend nil %v, reason %q; want a source and no reason", spend == nil, reason)
	}
	r := decodeDeepSeekReport(t, spend())

	if r.Samples != 2 || r.Balance == nil || *r.Balance != 40.25 {
		t.Errorf("the report reads %d samples with balance %v, want the rows' 2 and 40.25, not the ledger's 2 and 51.00", r.Samples, r.Balance)
	}
	if r.LastSample != "2026-11-10T09:30" {
		t.Errorf("last sample = %q, want the newest row's 2026-11-10T09:30, so the series is folded oldest first", r.LastSample)
	}
	if len(r.Alerts) != 1 || r.Alerts[0] != "balance low: $40.25" {
		t.Errorf("alerts = %q, want the newest row's note", r.Alerts)
	}
	if len(kinds) != 1 || kinds[0] != beads.SampleKindDeepSeekBalance {
		t.Errorf("the poll read rows of kind %v, want one %s read", kinds, beads.SampleKindDeepSeekBalance)
	}
	if len(sinces) != 1 || !sinces[0].IsZero() {
		t.Errorf("the poll read rows since %v, want the whole series", sinces)
	}
	if len(limits) != 1 || limits[0] != deepSeekRowScanLimit {
		t.Errorf("the poll read rows with limits %v, want the package's own cap of %d", limits, deepSeekRowScanLimit)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], deepSeekRowSource) {
		t.Errorf("the why-log = %q, want one line naming %q", logged, deepSeekRowSource)
	}
}

// TestResolveSpendServesTheLedgerWhileNoDeepSeekRowExists: while the town has no
// deepseek_balance row the Mac's ledger answers, and the why-log names the
// ledger beside the reason the rows did not, so a pane that fell back is not
// read as a town with no rows (gt-u3fp8i).
func TestResolveSpendServesTheLedgerWhileNoDeepSeekRowExists(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home)
	homeFn := func() (string, error) { return home, nil }

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	spend, reason := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rowsOf(), whyLog)
	if spend == nil || reason != "" {
		t.Fatalf("no rows: spend nil %v, reason %q; want the ledger and no reason", spend == nil, reason)
	}
	r := decodeDeepSeekReport(t, spend())

	if r.Samples != 2 || r.Balance == nil || *r.Balance != 51.00 {
		t.Errorf("the report reads %d samples with balance %v, want the ledger's 2 and 51.00", r.Samples, r.Balance)
	}
	want := "deepseek spend source: " + spendFileSourceLedger + " (no deepseek_balance rows)"
	if len(logged) != 1 || logged[0] != want {
		t.Errorf("the why-log = %q, want [%q]", logged, want)
	}
}

// TestResolveSpendReadsTheRowsOnEveryPoll: the source is chosen on every poll,
// never once at build, so the first row the patrol writes is read without the
// hub restarting.
func TestResolveSpendReadsTheRowsOnEveryPoll(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home)
	homeFn := func() (string, error) { return home, nil }

	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	answers := [][]beads.Sample{{}, {deepSeekBalanceRow(at, 40.25, "")}}
	calls := 0
	rows := func(string, time.Time, int) ([]beads.Sample, error) {
		if calls >= len(answers) {
			return nil, fmt.Errorf("poll %d has no scripted answer", calls)
		}
		answer := answers[calls]
		calls++
		return answer, nil
	}

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	spend, _ := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rows, whyLog)

	if r := decodeDeepSeekReport(t, spend()); r.Balance == nil || *r.Balance != 51.00 {
		t.Fatalf("first poll: balance %v, want the ledger's 51.00 before any row exists", r.Balance)
	}
	if r := decodeDeepSeekReport(t, spend()); r.Balance == nil || *r.Balance != 40.25 {
		t.Errorf("second poll: balance %v, want the row's 40.25: the source must be chosen on every poll", r.Balance)
	}
	if calls != 2 {
		t.Errorf("row reads = %d, want one per poll", calls)
	}
	if len(logged) != 2 || !strings.Contains(logged[0], spendFileSourceLedger) || !strings.Contains(logged[1], deepSeekRowSource) {
		t.Errorf("the why-log = %q, want the ledger then the rows named", logged)
	}
}

// TestResolveSpendFallsBackWhenTheDeepSeekRowsFail: a rows read that fails
// answers the ledger with the reason logged once, so a bd hiccup does not blank
// a panel whose ledger is still there.
func TestResolveSpendFallsBackWhenTheDeepSeekRowsFail(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home)
	homeFn := func() (string, error) { return home, nil }
	rows := func(string, time.Time, int) ([]beads.Sample, error) {
		return nil, errors.New("bd sample list: connection refused")
	}

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	spend, _ := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rows, whyLog)

	if r := decodeDeepSeekReport(t, spend()); r.Balance == nil || *r.Balance != 51.00 {
		t.Errorf("failed rows: balance %v, want the ledger's 51.00", r.Balance)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], spendFileSourceLedger) || !strings.Contains(logged[0], "connection refused") {
		t.Errorf("the why-log = %q, want one line naming %s and the failed rows read", logged, spendFileSourceLedger)
	}
}

// TestResolveSpendFallsBackWhenTheRowsCannotBeEncoded: a row whose balance JSON
// cannot carry (a NaN another writer stored) gives no report, so the poll
// answers the ledger with the reason logged rather than serve a broken report.
func TestResolveSpendFallsBackWhenTheRowsCannotBeEncoded(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home)
	homeFn := func() (string, error) { return home, nil }
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	rows := rowsOf(deepSeekBalanceRow(at, math.NaN(), ""))

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	spend, _ := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rows, whyLog)

	if r := decodeDeepSeekReport(t, spend()); r.Balance == nil || *r.Balance != 51.00 {
		t.Errorf("unencodable row: balance %v, want the ledger's 51.00", r.Balance)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], spendFileSourceLedger) || !strings.Contains(logged[0], "reading the deepseek_balance rows") {
		t.Errorf("the why-log = %q, want one line naming the ledger and the failed rows report", logged)
	}
}

// TestResolveSpendRefusesAFullDeepSeekRowCap: a rows read that comes back with
// as many rows as it asked for has dropped the series' start, so the poll
// refuses that series and answers from the ledger with the reason logged rather
// than fold a short series as the whole one.
func TestResolveSpendRefusesAFullDeepSeekRowCap(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home)
	homeFn := func() (string, error) { return home, nil }
	rows := func(string, time.Time, int) ([]beads.Sample, error) {
		return make([]beads.Sample, deepSeekRowScanLimit), nil
	}

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	spend, _ := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rows, whyLog)

	if r := decodeDeepSeekReport(t, spend()); r.Balance == nil || *r.Balance != 51.00 {
		t.Errorf("full cap: balance %v, want the ledger's 51.00", r.Balance)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], spendFileSourceLedger) || !strings.Contains(logged[0], "filled its cap") {
		t.Errorf("the why-log = %q, want one line naming the cap", logged)
	}
}

// TestResolveSpendSkipsDeepSeekRowsWithNoTotal: a row another writer left
// without a total is not a balance reading, so it is skipped rather than folded
// as zero, and the unusable count is named beside the source that answered. A
// town whose rows are all unusable falls back to the ledger, and the log says
// why.
func TestResolveSpendSkipsDeepSeekRowsWithNoTotal(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home)
	homeFn := func() (string, error) { return home, nil }
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	partial := beads.Sample{Kind: beads.SampleKindDeepSeekBalance, At: at}

	t.Run("one usable row answers and the partial row is counted", func(t *testing.T) {
		t.Parallel()
		var logged []string
		whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
		spend, _ := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rowsOf(deepSeekBalanceRow(at, 40.25, ""), partial), whyLog)

		r := decodeDeepSeekReport(t, spend())
		if r.Samples != 1 || r.Balance == nil || *r.Balance != 40.25 {
			t.Errorf("the report reads %d samples with balance %v, want the usable row's 1 and 40.25", r.Samples, r.Balance)
		}
		if len(logged) != 1 || !strings.Contains(logged[0], deepSeekRowSource) || !strings.Contains(logged[0], "unusable deepseek_balance rows: 1") {
			t.Errorf("the why-log = %q, want one line naming the rows and the unusable count", logged)
		}
	})

	t.Run("all rows unusable answers the ledger", func(t *testing.T) {
		t.Parallel()
		var logged []string
		whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
		spend, _ := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rowsOf(partial), whyLog)

		if r := decodeDeepSeekReport(t, spend()); r.Balance == nil || *r.Balance != 51.00 {
			t.Errorf("all unusable: balance %v, want the ledger's 51.00", r.Balance)
		}
		if len(logged) != 1 || !strings.Contains(logged[0], spendFileSourceLedger) || !strings.Contains(logged[0], "unusable deepseek_balance rows: 1") {
			t.Errorf("the why-log = %q, want one line naming the ledger and the unusable count", logged)
		}
	})
}

// TestResolveSpendWiredWithNoRowAndNoFileHasNoReport: a hub with the rows wired
// and no file source still polls; a town with neither gives no report, never a
// zero report, and the pane keeps the cluster's absence as its reason.
func TestResolveSpendWiredWithNoRowAndNoFileHasNoReport(t *testing.T) {
	t.Parallel()

	home := t.TempDir() // no ledger, no tool
	homeFn := func() (string, error) { return home, nil }
	spend, reason := resolveSpendIn(false, "", homeFn, os.Stat, spendUnused, rowsOf(), nil)
	if spend == nil || reason != dashboard.NotInClusterReason {
		t.Fatalf("no rows and no file: spend nil %v, reason %q; want the wired rows and %q", spend == nil, reason, dashboard.NotInClusterReason)
	}
	if out := spend(); out != nil {
		t.Errorf("no rows and no file: served %s, want no report", out)
	}
}

// TestResolveSpendFlagWinsOverDeepSeekRows: --spend-cmd is an explicit source
// the operator named, not a file ledger, so it answers alone and the rows are
// not read.
func TestResolveSpendFlagWinsOverDeepSeekRows(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home)
	homeFn := func() (string, error) { return home, nil }
	rows := func(string, time.Time, int) ([]beads.Sample, error) {
		return nil, errors.New("the rows were read, want the flag's command alone")
	}

	var calls [][]string
	spend, reason := resolveSpendIn(false, "/usr/bin/spend-report --json", homeFn, os.Stat, spendFake(&calls, `{"from":"flag"}`), rows, nil)
	if spend == nil || reason != "" {
		t.Fatalf("flag: spend nil %v, reason %q; want the flag's command and no reason", spend == nil, reason)
	}
	if got := string(spend()); got != `{"from":"flag"}` {
		t.Errorf("flag: served %q, want the flag's JSON", got)
	}
}

// TestResolveSpendInClusterReadsDeepSeekRowsWithNoLedger: the pod after the
// Mac's ~/.claude is unmounted has no ledger and no tool, so the town's rows are
// the panel's only source. The rows answer, and nothing is run.
func TestResolveSpendInClusterReadsDeepSeekRowsWithNoLedger(t *testing.T) {
	t.Parallel()

	home := t.TempDir() // the pod: neither the tool nor the ledger
	homeFn := func() (string, error) { return home, nil }
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	spend, _ := resolveSpendIn(true, "", homeFn, os.Stat, spendUnused, rowsOf(deepSeekBalanceRow(at, 40.25, "")), whyLog)
	if spend == nil {
		t.Fatal("in cluster with rows: spend nil, want the rows source")
	}
	if r := decodeDeepSeekReport(t, spend()); r.Balance == nil || *r.Balance != 40.25 {
		t.Errorf("in cluster: balance %v, want the row's 40.25", r.Balance)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], deepSeekRowSource) {
		t.Errorf("the why-log = %q, want one line naming %q", logged, deepSeekRowSource)
	}
}

// TestDashboardSpendLedgerStillReadsWithNoRowsWired guards the nil-rows path:
// a hub with no row store keeps the file source's own behavior and logging.
func TestDashboardSpendLedgerStillReadsWithNoRowsWired(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	writeSpendLedger(t, home)
	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	spend, _ := resolveSpendIn(false, "", func() (string, error) { return home, nil }, os.Stat, spendUnused, nil, whyLog)

	if r := decodeDeepSeekReport(t, spend()); r.Balance == nil || *r.Balance != 51.00 {
		t.Errorf("nil rows: balance %v, want the ledger's 51.00", r.Balance)
	}
	for _, line := range logged {
		if strings.Contains(line, "deepseek spend source") {
			t.Errorf("nil rows logged %q, want no spend-source line: the file source is unchanged", line)
		}
	}
}

// TestResolveSpendKeepsTheLedgerWhenItCannotBeRead pins that a wired rows source
// does not swallow the ledger's failed-read state: rows that fail and a ledger
// that is a directory still serve the unavailable state with its reason.
func TestResolveSpendKeepsTheLedgerWhenItCannotBeRead(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	if err := os.MkdirAll(spendDirIn(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(spendDirIn(home), spendLedgerName), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := func(string, time.Time, int) ([]beads.Sample, error) {
		return nil, errors.New("bd sample list: connection refused")
	}
	spend, _ := resolveSpendIn(false, "", func() (string, error) { return home, nil }, os.Stat, spendUnused, rows, nil)

	out := spend()
	var state struct {
		Error   string `json:"error"`
		Samples *int   `json:"samples"`
	}
	if err := json.Unmarshal(out, &state); out == nil || err != nil {
		t.Fatalf("state %q (err %v), want a non-nil unavailable state", out, err)
	}
	if state.Samples != nil || state.Error == "" {
		t.Errorf("state %s, want the ledger's failed read with its reason and no report", out)
	}
}
