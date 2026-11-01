package jev

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSpendLedger writes lines to a ledger in a fresh subdirectory of dir.
func writeSpendLedger(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "jev", "usage.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

func ledgerLine(ts time.Time, caller string, in, out int) string {
	rec := LedgerRecord{TS: ts, Caller: caller, InputTokens: in, OutputTokens: out}
	data, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestSpendLedgerSumsOnlyLinesInsideTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	path := writeSpendLedger(t, t.TempDir(),
		ledgerLine(now.Add(-1*time.Hour), "a", 1000, 500),
		ledgerLine(from, "b", 10, 20), // the window's first instant counts
		ledgerLine(now, "b", 5, 5),    // and its last
		ledgerLine(from.Add(-time.Nanosecond), "c", 7, 7),
		ledgerLine(now.Add(time.Second), "c", 9, 9),
	)

	rep, err := ReadSpend(path, from, now, 1e9)
	require.NoError(t, err)

	assert.Equal(t, 3, rep.Calls)
	assert.Equal(t, 1015, rep.InputTokens)
	assert.Equal(t, 525, rep.OutputTokens)
	// At one dollar per billion tokens, a token is a dollar.
	assert.Equal(t, float64(1540), rep.USD)
	assert.Equal(t, 0, rep.LinesSkipped)
	assert.Equal(t, map[string]CallerSpend{
		"a": {Calls: 1, USD: 1500},
		"b": {Calls: 2, USD: 40},
	}, rep.ByCaller)
}

func TestSpendLedgerSkipsMalformedLines(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	path := writeSpendLedger(t, t.TempDir(),
		ledgerLine(now, "a", 1, 1),
		`{"ts":"2026-10-19T11:00:00Z","caller":"b","input_tok`, // a write cut short
		`not json`,
		`{"caller":"c","input_tokens":5,"output_tokens":5}`, // no stamp to place it
		``, // a blank line is not a record, so it is not a loss
	)

	rep, err := ReadSpend(path, now.Add(-time.Hour), now, 1e9)
	require.NoError(t, err)

	assert.Equal(t, 1, rep.Calls)
	assert.Equal(t, 3, rep.LinesSkipped)
	assert.Equal(t, float64(2), rep.USD)
}

func TestSpendLedgerSkipsNegativeTokenCounts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	path := writeSpendLedger(t, t.TempDir(),
		ledgerLine(now, "a", 3, 4),
		ledgerLine(now, "b", -10, 5),
		ledgerLine(now, "c", 5, -10),
		ledgerLine(now, "d", -1, -1),
	)

	rep, err := ReadSpend(path, now.Add(-time.Hour), now, 1e9)
	require.NoError(t, err)

	assert.Equal(t, 1, rep.Calls)
	assert.Equal(t, 3, rep.InputTokens)
	assert.Equal(t, 4, rep.OutputTokens)
	assert.Equal(t, float64(7), rep.USD)
	assert.Equal(t, 3, rep.LinesSkipped)
	assert.Equal(t, map[string]CallerSpend{"a": {Calls: 1, USD: 7}}, rep.ByCaller)
}

func TestSpendLedgerGroupsAnEmptyCaller(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	path := writeSpendLedger(t, t.TempDir(),
		`{"ts":"2026-10-19T11:00:00Z","input_tokens":3,"output_tokens":4}`,
	)

	rep, err := ReadSpend(path, now.Add(-time.Hour), now, 1e9)
	require.NoError(t, err)

	assert.Equal(t, map[string]CallerSpend{defaultCaller: {Calls: 1, USD: 7}}, rep.ByCaller)
}

func TestSpendLedgerMissingIsEmpty(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "jev", "usage.jsonl")

	rep, err := ReadSpend(path, now.Add(-time.Hour), now, 1e9)
	require.NoError(t, err)

	assert.Equal(t, 0, rep.Calls)
	assert.Equal(t, 0, rep.InputTokens)
	assert.Equal(t, 0, rep.OutputTokens)
	assert.Equal(t, 0, rep.LinesSkipped)
	assert.Equal(t, float64(0), rep.USD)
	assert.NotNil(t, rep.ByCaller)
	assert.Empty(t, rep.ByCaller)
}

// The dashboard's Jev row shows the last hour, today, yesterday and the last
// week beside the day: one pass of the
// ledger, days cut at the clock's local midnight.
func TestReadSpendPeriodsCutsHourTodayYesterdayAndWeek(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("test", -5*3600)
	now := time.Date(2026, 10, 31, 20, 0, 0, 0, loc)
	midnight := time.Date(2026, 10, 31, 0, 0, 0, 0, loc)
	path := writeSpendLedger(t, t.TempDir(),
		ledgerLine(now.Add(-30*time.Minute), "a", 1000, 0),    // hour, today, week
		ledgerLine(now.Add(-5*time.Hour), "a", 2000, 0),       // today, week
		ledgerLine(midnight.Add(-time.Hour), "a", 4000, 0),    // yesterday, week
		ledgerLine(midnight.Add(-30*time.Hour), "a", 8000, 0), // before yesterday, week
		ledgerLine(now.Add(-200*time.Hour), "a", 16000, 0),    // outside the week
		ledgerLine(now.Add(time.Hour), "a", 32000, 0),         // after now
	)
	got, err := ReadSpendPeriods(path, now, 1e9)
	require.NoError(t, err)
	assert.Equal(t, SpendPeriods{Last1hUSD: 1000, TodayUSD: 3000, YesterdayUSD: 4000, Last7dUSD: 15000}, got)
}

func TestReadSpendPeriodsMissingLedgerIsZeros(t *testing.T) {
	t.Parallel()
	got, err := ReadSpendPeriods(filepath.Join(t.TempDir(), "none.jsonl"), time.Now(), 1e9)
	require.NoError(t, err)
	assert.Equal(t, SpendPeriods{}, got)
}
