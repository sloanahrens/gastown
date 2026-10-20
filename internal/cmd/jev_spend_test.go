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
