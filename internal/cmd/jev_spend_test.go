package cmd

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev"
)

// writeJevSpendLedger writes lines to the town's Jev usage ledger.
func writeJevSpendLedger(t *testing.T, townRoot string, lines ...string) {
	t.Helper()
	path := jev.UsageLedgerPath(townRoot)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

func jevSpendLine(ts time.Time, caller string, in, out int) string {
	rec := jev.LedgerRecord{TS: ts, Caller: caller, InputTokens: in, OutputTokens: out}
	data, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestJevSpendJSONSumsTheWindowByCaller(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	writeJevSpendLedger(t, town,
		jevSpendLine(now.Add(-2*time.Hour), "attention jev-replay", 1000, 500),
		jevSpendLine(now.Add(-3*time.Hour), "attention jev-replay", 100, 50),
		jevSpendLine(now.Add(-48*time.Hour), "attention jev-replay", 7000, 7000), // outside
		`{"ts":"2026-10-19T11:00:00Z","caller":"cut`,                             // malformed
	)

	var buf bytes.Buffer
	require.NoError(t, jevSpendWithFlags(&buf, town, 24, 1e9, true, now))

	var got jev.SpendReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, 2, got.Calls)
	assert.Equal(t, 1100, got.InputTokens)
	assert.Equal(t, 550, got.OutputTokens)
	assert.Equal(t, float64(1650), got.USD)
	assert.Equal(t, 1, got.LinesSkipped)
	assert.Equal(t, map[string]jev.CallerSpend{
		"attention jev-replay": {Calls: 2, USD: 1650},
	}, got.ByCaller)
}

// The JSON also carries the periods the dashboard's Jev row fills its other
// columns from, in the same pass over the same ledger.
func TestJevSpendJSONCarriesThePeriods(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	writeJevSpendLedger(t, town,
		jevSpendLine(now.Add(-30*time.Minute), "a", 1000, 0),
		jevSpendLine(now.Add(-26*time.Hour), "a", 4000, 0),
	)
	var buf bytes.Buffer
	require.NoError(t, jevSpendWithFlags(&buf, town, 24, 1e9, true, now))
	var got struct {
		Periods jev.SpendPeriods `json:"periods"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, jev.SpendPeriods{Last1hUSD: 1000, TodayUSD: 1000, YesterdayUSD: 4000, Last7dUSD: 5000}, got.Periods)
}

func TestJevSpendMissingLedgerPrintsZeros(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	require.NoError(t, jevSpendWithFlags(&buf, t.TempDir(), 24, 1e9, true, now))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &raw))
	assert.Equal(t, float64(0), raw["calls"])
	assert.Equal(t, float64(0), raw["input_tokens"])
	assert.Equal(t, float64(0), raw["output_tokens"])
	assert.Equal(t, float64(0), raw["usd"])
	assert.Equal(t, float64(0), raw["lines_skipped"])
	assert.Equal(t, map[string]any{}, raw["by_caller"], "an empty report still names its callers as an object")
}

func TestJevSpendRefusesAnUnusablePrice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for name, price := range map[string]float64{
		"omitted":  0,
		"negative": -1,
		"NaN":      math.NaN(),
		"infinite": math.Inf(1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := jevSpendWithFlags(&bytes.Buffer{}, t.TempDir(), 24, price, true, now)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--usd-per-billion-tokens")
		})
	}
}

func TestJevSpendRefusesANonPositiveWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	err := jevSpendWithFlags(&bytes.Buffer{}, t.TempDir(), 0, 1e9, true, now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--hours")
}

func TestJevSpendTableNamesEveryCaller(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	writeJevSpendLedger(t, town,
		jevSpendLine(now.Add(-time.Hour), "attention jev-replay", 10, 10),
		jevSpendLine(now.Add(-time.Hour), "triage", 5, 5),
	)

	var buf bytes.Buffer
	require.NoError(t, jevSpendWithFlags(&buf, town, 24, 1e9, false, now))

	out := buf.String()
	assert.Contains(t, out, "Calls")
	assert.Contains(t, out, "attention jev-replay")
	assert.Contains(t, out, "triage")
}

// TestJevSpendCommandRefusesBadFlags runs the cobra command, not the helper, so
// the flag wiring and the town lookup are covered too: a price or window the
// helper refuses must come back from "gt jev spend" naming the flag. Parsing
// the flags and then calling runJevSpend stands in for Execute, which would
// also run the root command's pre-run hooks.
//
//testpolicy:allow parallel — the spend command's flag variables are process-wide
func TestJevSpendCommandRefusesBadFlags(t *testing.T) {
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(town, "mayor"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0o600))
	//testpolicy:allow no-chdir — the cwd is the input under test: runJevSpend finds the town from the working directory
	t.Chdir(town)

	cases := []struct {
		name string
		args []string
		flag string
	}{
		{"zero price", []string{"--hours", "24", "--usd-per-billion-tokens", "0"}, "--usd-per-billion-tokens"},
		{"negative price", []string{"--hours", "24", "--usd-per-billion-tokens", "-1"}, "--usd-per-billion-tokens"},
		{"NaN price", []string{"--hours", "24", "--usd-per-billion-tokens", "NaN"}, "--usd-per-billion-tokens"},
		{"infinite price", []string{"--hours", "24", "--usd-per-billion-tokens", "+Inf"}, "--usd-per-billion-tokens"},
		{"zero window", []string{"--hours", "0", "--usd-per-billion-tokens", "3"}, "--hours"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, jevSpendCmd.ParseFlags(tc.args))
			err := runJevSpend(jevSpendCmd, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.flag)
		})
	}

	t.Run("valid flags price the ledger", func(t *testing.T) {
		writeJevSpendLedger(t, town,
			jevSpendLine(time.Now().Add(-30*time.Minute), "triage", 1000, 500),
		)
		var buf bytes.Buffer
		jevSpendCmd.SetOut(&buf)
		require.NoError(t, jevSpendCmd.ParseFlags([]string{"--hours", "1", "--usd-per-billion-tokens", "3", "--json"}))
		require.NoError(t, runJevSpend(jevSpendCmd, nil))

		var got jev.SpendReport
		require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
		assert.Equal(t, 1, got.Calls)
		assert.Equal(t, 1000, got.InputTokens)
		assert.InDelta(t, 4.5e-6, got.USD, 1e-12, "1500 tokens at $3 per billion")
	})
}

// TestJevSpendPrintsTheLogCopyAge prints the newest write among the town's Jev
// files above the table, the same line health prints, so a reader of 0.12 USD
// can tell which copy it came from (gt-bgimh9).
func TestJevSpendPrintsTheLogCopyAge(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	writeJevSpendLedger(t, town, jevSpendLine(now.Add(-time.Hour), "attention", 10, 5))
	ledger := jev.UsageLedgerPath(town)
	jevSetModTime(t, ledger, now.Add(-10*time.Minute))

	var out bytes.Buffer
	require.NoError(t, jevSpendWithFlags(&out, town, 24, 1e9, false, now))
	assert.Contains(t, out.String(), "log copy: newest write 2026-10-19 11:50Z (10m ago) at "+ledger)
	assert.Less(t, strings.Index(out.String(), "log copy:"), strings.Index(out.String(), "Calls"),
		"the line sits above the table")
}

// TestJevSpendJSONWarnsInsteadOfFailingOnABadPatrolConfigOrLogPath covers the
// copy read's advisory path. Spend's numbers come from the ledger, so a patrol
// config or a log path that cannot be read is named as copy_warning and the
// command still prints the spend it printed before the copy line existed.
func TestJevSpendJSONWarnsInsteadOfFailingOnABadPatrolConfigOrLogPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		breakTown func(t *testing.T, town string)
		reason    string
	}{
		"patrol config does not parse": {
			breakTown: func(t *testing.T, town string) {
				path := daemon.PatrolConfigFile(town)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("{"), 0o644))
			},
			reason: "reading the patrol config",
		},
		"log path is a directory": {
			breakTown: func(t *testing.T, town string) {
				require.NoError(t, os.MkdirAll(filepath.Join(town, ".runtime", "jev", "attention.jsonl"), 0o700))
			},
			reason: "not a regular file",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeJevSpendLedger(t, town, jevSpendLine(now.Add(-time.Hour), "attention", 10, 5))
			tc.breakTown(t, town)

			var out bytes.Buffer
			require.NoError(t, jevSpendWithFlags(&out, town, 24, 1e9, true, now))
			var doc jevSpendDoc
			require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
			assert.Equal(t, 1, doc.Calls, "the spend is printed as before")
			assert.Contains(t, doc.CopyWarning, tc.reason)
			assert.True(t, doc.CopyStale, "a copy that cannot be read is not called fresh")

			var text bytes.Buffer
			require.NoError(t, jevSpendWithFlags(&text, town, 24, 1e9, false, now))
			assert.Contains(t, text.String(), "log copy: WARNING: ")
			assert.Contains(t, text.String(), "Calls", "the table is printed as before")
		})
	}
}

// TestJevSpendReportsAWriteFailure covers the copy line's write: a writer that
// refuses is an error, not a table printed without the copy it came from.
func TestJevSpendReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeJevSpendLedger(t, town, jevSpendLine(time.Date(2026, 10, 19, 11, 0, 0, 0, time.UTC), "attention", 10, 5))

	err := jevSpendWithFlags(jevHealthFailingWriter{}, town, 24, 1e9, false,
		time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	require.Error(t, err)
}
