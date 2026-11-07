package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jev/health"
)

// newJevHealthTown writes a fixture town whose only enabled battery is
// dedupe: a cursor written cursorAge ago, one answered line a minute old and a
// day of ledger calls from two callers.
func newJevHealthTown(t *testing.T, now time.Time, cursorAge time.Duration) string {
	t.Helper()
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(town, "mayor"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(town, "settings"), 0o755))
	cfg := `{"daemon":{"patrols":{"jev_dedupe":{"enabled":true,"max_usd_per_day":1,"usd_per_billion_tokens":42}}}}`
	require.NoError(t, os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(cfg), 0o644))

	jevDir := filepath.Join(town, ".runtime", "jev")
	require.NoError(t, os.MkdirAll(jevDir, 0o700))
	cursor := filepath.Join(jevDir, "dedupe.cursor.json")
	require.NoError(t, os.WriteFile(cursor, []byte(`{"time":"2026-10-30T11:00:00Z","id":"gt-x"}`), 0o600))
	at := now.Add(-cursorAge)
	require.NoError(t, os.Chtimes(cursor, at, at))

	line := fmt.Sprintf(`{"ts":%q,"answers":{"q":"a"}}`, now.Add(-time.Minute).UTC().Format(time.RFC3339))
	require.NoError(t, os.WriteFile(filepath.Join(jevDir, "dedupe.jsonl"), []byte(line+"\n"), 0o600))

	// The daemon's last-run record: dedupe's patrol ran as often as the cursor
	// was last written, so a stale cursor here is also a stale patrol.
	writeJevPatrolRuns(t, town, map[string]time.Time{"jev_dedupe": now.Add(-cursorAge)})

	writeJevSpendLedger(t, town,
		jevSpendLine(now.Add(-30*time.Minute), "jev-dedupe", 100, 50),
		jevSpendLine(now.Add(-20*time.Minute), "jev-thrash", 100, 50),
	)
	return town
}

// writeJevPatrolRuns writes the daemon's last-run record with each patrol's
// last completion time.
func writeJevPatrolRuns(t *testing.T, town string, runs map[string]time.Time) {
	t.Helper()
	dir := filepath.Join(town, "daemon")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data, err := json.Marshal(map[string]any{"last_run": runs})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, patrolLastRunFile), data, 0o644))
}

// jevHealthFailingWriter refuses every write, for the report's write failures.
type jevHealthFailingWriter struct{}

func (jevHealthFailingWriter) Write([]byte) (int, error) { return 0, errors.New("the writer refused") }

// townFinder resolves the fixture town the test names.
func townFinder(town string) func() (string, error) {
	return func() (string, error) { return town, nil }
}

// jevHealthDir is the town's Jev runtime directory, created.
func jevHealthDir(t *testing.T, town string) string {
	t.Helper()
	dir := filepath.Join(town, ".runtime", "jev")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	return dir
}

// runJevHealthCommand drives the subcommand it builds for the fixture town and
// clocks it at now, as gt would.
func runJevHealthCommand(t *testing.T, town string, now time.Time, asJSON bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newJevHealthCmd(townFinder(town), func() time.Time { return now })
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if asJSON {
		cmd.SetArgs([]string{"--json"})
	} else {
		cmd.SetArgs([]string{})
	}
	err := cmd.Execute()
	return out.String(), err
}

// runJevHealthError drives the subcommand its constructor builds for findTown,
// writing to out, so every failure path is reached through the command the way
// gt reaches it, on the fixed clock and never on the wall clock.
func runJevHealthError(t *testing.T, findTown func() (string, error), out io.Writer, args ...string) error {
	t.Helper()
	if out == nil {
		out = &bytes.Buffer{}
	}
	cmd := newJevHealthCmd(findTown, jevHealthFixedNow)
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	return cmd.Execute()
}

// jevHealthTableRow finds a battery's table line.
func jevHealthTableRow(t *testing.T, table, name string) string {
	t.Helper()
	for _, line := range strings.Split(table, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", name, table)
	return ""
}

// jevHealthRow finds a battery's row in the --json document.
func jevHealthRow(t *testing.T, rows []health.Row, name string) health.Row {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row for %s", name)
	return health.Row{}
}

// TestJevHealthCommand runs gt jev health against a fixture town: a healthy
// town prints an ok dedupe row and exits 0, and a town whose cursor is two
// hours old prints stale and exits 1. The command reads the fixed now the
// fixture's timestamps are built from, so no wall clock reaches the verdict.
func TestJevHealthCommand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	healthy := newJevHealthTown(t, now, 5*time.Minute)
	stale := newJevHealthTown(t, now, 2*time.Hour)

	t.Run("healthy table", func(t *testing.T) {
		out, err := runJevHealthCommand(t, healthy, now, false)
		require.NoError(t, err)
		assert.Contains(t, out, "BATTERY")
		row := jevHealthTableRow(t, out, "dedupe")
		assert.Contains(t, row, " ok")
		assert.NotContains(t, row, "stale")
	})

	t.Run("healthy json rows", func(t *testing.T) {
		out, err := runJevHealthCommand(t, healthy, now, true)
		require.NoError(t, err)

		var doc jevHealthDoc
		require.NoError(t, json.Unmarshal([]byte(out), &doc))
		require.Len(t, doc.Batteries, len(health.Batteries))

		dedupe := jevHealthRow(t, doc.Batteries, "dedupe")
		assert.True(t, dedupe.Enabled)
		assert.Equal(t, health.OK, dedupe.Status)
		assert.Equal(t, 1, dedupe.Lines24)
		assert.Equal(t, 1, dedupe.LedgerCalls, "only the dedupe caller's call counts")

		targets := jevHealthRow(t, doc.Batteries, "targets")
		assert.False(t, targets.Enabled, "a battery the config does not turn on is disabled")
		assert.Equal(t, health.Disabled, targets.Status)
	})

	t.Run("stale fixture exits 1", func(t *testing.T) {
		out, err := runJevHealthCommand(t, stale, now, false)

		code, ok := IsSilentExit(err)
		require.True(t, ok, "want a silent coded exit, got %v", err)
		assert.Equal(t, 1, code)
		assert.Contains(t, jevHealthTableRow(t, out, "dedupe"), " stale")
	})
}

// jevHealthFixedNow is the instant every fixture is judged at, so no test
// reaches the wall clock.
func jevHealthFixedNow() time.Time {
	return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
}

// TestJevHealthRefusesAMissingTown covers the town lookup's failure path.
func TestJevHealthRefusesAMissingTown(t *testing.T) {
	t.Parallel()
	err := runJevHealthError(t, func() (string, error) { return "", errors.New("no town") }, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finding the town root")
}

// TestJevHealthRefusesABrokenPatrolConfig covers the config read's failure
// path: a config that does not parse must fail the command, not read as a town
// with every battery off. The fixture is written where the loader reads it and
// the error must name that path, so a fixture that lands elsewhere fails here.
func TestJevHealthRefusesABrokenPatrolConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := daemon.PatrolConfigFile(town)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o644))

	err := runJevHealthError(t, townFinder(town), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the patrol config")
	assert.Contains(t, err.Error(), path, "the error names the file the fixture wrote")
}

// TestJevHealthRefusesAnUnreadableLedger covers the ledger read's failure
// path: a ledger that is a directory cannot be read, and must not read as no
// calls. The cause stays wrapped, so a caller can still name the file.
func TestJevHealthRefusesAnUnreadableLedger(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(jevHealthDir(t, town), "usage.jsonl"), 0o700))

	err := runJevHealthError(t, townFinder(town), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errJevHealthLedger)
	var pathErr *fs.PathError
	assert.ErrorAs(t, err, &pathErr, "the ledger failure keeps its cause")
}

// TestJevHealthRefusesAnUnreadableLog covers the log read's failure path: a
// log that is a directory cannot be read, and must not read as a quiet
// battery.
func TestJevHealthRefusesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(jevHealthDir(t, town), "attention.jsonl"), 0o700))

	err := runJevHealthError(t, townFinder(town), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attention.jsonl")
}

// TestJevHealthRefusesAnUnreadableCursor covers the cursor stat's failure
// path: a cursor that cannot be statted must not read as a battery with no
// pass.
func TestJevHealthRefusesAnUnreadableCursor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A symlink to itself: the stat fails with ELOOP, not with "not there".
	require.NoError(t, os.Symlink("attention.cursor.json", filepath.Join(jevHealthDir(t, town), "attention.cursor.json")))

	err := runJevHealthError(t, townFinder(town), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attention.cursor.json")
}

// TestJevHealthReportsAWriteFailure covers both write paths: the table's flush
// and the JSON encoder's write.
func TestJevHealthReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for name, args := range map[string][]string{"table": nil, "json": {"--json"}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := runJevHealthError(t, townFinder(town), jevHealthFailingWriter{}, args...)
			require.Error(t, err)
		})
	}
}

// TestJevHealthJSON reads the --json rows for a quiet battery: its cursor is
// nine hours old, but the daemon recorded its patrol's last run three minutes
// ago. The row carries both times, names the quiet stretch, and reads ok.
func TestJevHealthJSON(t *testing.T) {
	t.Parallel()
	now := jevHealthFixedNow()
	town := newJevHealthTown(t, now, 9*time.Hour)
	writeJevPatrolRuns(t, town, map[string]time.Time{"jev_dedupe": now.Add(-3 * time.Minute)})

	out, err := runJevHealthCommand(t, town, now, true)
	require.NoError(t, err)
	assert.Contains(t, out, `"last_event"`)
	assert.Contains(t, out, `"last_pass"`)

	var doc jevHealthDoc
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	dedupe := jevHealthRow(t, doc.Batteries, "dedupe")
	assert.Equal(t, health.OK, dedupe.Status, "reason %q", dedupe.Reason)
	assert.Equal(t, "no events since 03:00Z; last pass 3m ago", dedupe.Reason)
	require.NotNil(t, dedupe.LastEvent)
	require.NotNil(t, dedupe.LastPass)
	assert.WithinDuration(t, now.Add(-9*time.Hour), *dedupe.LastEvent, time.Second)
	assert.WithinDuration(t, now.Add(-3*time.Minute), *dedupe.LastPass, time.Second)
}

// TestJevHealthWeeklyReadsPatrolRecord judges the weekly notes battery by its
// patrol's last run: its newest log line is nine days old, but the daemon
// recorded the patrol's last run a day ago, so the row reads ok.
func TestJevHealthWeeklyReadsPatrolRecord(t *testing.T) {
	t.Parallel()
	now := jevHealthFixedNow()
	town := newJevHealthTown(t, now, 5*time.Minute)
	cfg := `{"daemon":{"patrols":{"jev_dedupe":{"enabled":true,"max_usd_per_day":1,"usd_per_billion_tokens":42},"jev_notes":{"enabled":true}}}}`
	require.NoError(t, os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(cfg), 0o644))
	old := fmt.Sprintf(`{"ts":%q,"answers":{"q":"a"}}`, now.Add(-9*24*time.Hour).UTC().Format(time.RFC3339))
	require.NoError(t, os.WriteFile(filepath.Join(town, ".runtime", "jev", "notes.jsonl"), []byte(old+"\n"), 0o600))
	writeJevPatrolRuns(t, town, map[string]time.Time{
		"jev_dedupe": now.Add(-5 * time.Minute),
		"jev_notes":  now.Add(-24 * time.Hour),
	})

	out, err := runJevHealthCommand(t, town, now, true)
	require.NoError(t, err)

	var doc jevHealthDoc
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	notes := jevHealthRow(t, doc.Batteries, "notes")
	assert.Equal(t, health.OK, notes.Status, "reason %q", notes.Reason)
	require.NotNil(t, notes.LastPass)
	assert.WithinDuration(t, now.Add(-24*time.Hour), *notes.LastPass, time.Second)
}

// TestJevHealthWeeklyUnreadablePatrolRecord judges a weekly battery when the
// daemon's last-run record cannot be read: the command hands notes the read
// error, so notes falls back to its log, reads unknown rather than ok, names the
// file, and the command exits 1. The record is a directory, so the read fails
// without being mistaken for a missing record.
func TestJevHealthWeeklyUnreadablePatrolRecord(t *testing.T) {
	t.Parallel()
	now := jevHealthFixedNow()
	town := newJevHealthTown(t, now, 5*time.Minute)
	cfg := `{"daemon":{"patrols":{"jev_dedupe":{"enabled":true,"max_usd_per_day":1,"usd_per_billion_tokens":42},"jev_notes":{"enabled":true}}}}`
	require.NoError(t, os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(cfg), 0o644))
	line := fmt.Sprintf(`{"ts":%q,"answers":{"q":"a"}}`, now.Add(-24*time.Hour).UTC().Format(time.RFC3339))
	require.NoError(t, os.WriteFile(filepath.Join(town, ".runtime", "jev", "notes.jsonl"), []byte(line+"\n"), 0o600))
	record := filepath.Join(town, "daemon", patrolLastRunFile)
	require.NoError(t, os.Remove(record))
	require.NoError(t, os.MkdirAll(record, 0o755))

	out, err := runJevHealthCommand(t, town, now, true)
	code, ok := IsSilentExit(err)
	require.True(t, ok, "want a silent coded exit, got %v", err)
	assert.Equal(t, 1, code)

	var doc jevHealthDoc
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	notes := jevHealthRow(t, doc.Batteries, "notes")
	assert.Equal(t, health.Unknown, notes.Status, "reason %q", notes.Reason)
	assert.Contains(t, notes.Reason, "last-run file unreadable: reading patrol_last_run.json")
	require.NotNil(t, notes.LastPass)
	assert.WithinDuration(t, now.Add(-24*time.Hour), *notes.LastPass, time.Second)
}

// TestJevHealthMissingPatrolRecordIsNotOK removes the daemon's last-run record
// from an otherwise healthy town: dedupe reads unknown, not ok, and the command
// exits 1.
func TestJevHealthMissingPatrolRecordIsNotOK(t *testing.T) {
	t.Parallel()
	now := jevHealthFixedNow()
	town := newJevHealthTown(t, now, 5*time.Minute)
	require.NoError(t, os.Remove(filepath.Join(town, "daemon", patrolLastRunFile)))

	out, err := runJevHealthCommand(t, town, now, false)
	code, ok := IsSilentExit(err)
	require.True(t, ok, "want a silent coded exit, got %v", err)
	assert.Equal(t, 1, code)
	row := jevHealthTableRow(t, out, "dedupe")
	assert.Contains(t, row, " unknown")
	assert.Contains(t, row, "last-run file unreadable: patrol_last_run.json is missing")
}

// TestJevHealthPatrolRuns reads the daemon's last-run record: a valid record
// gives each patrol's time, and a missing or corrupt one is an error that names
// the file and not its contents.
func TestJevHealthPatrolRuns(t *testing.T) {
	t.Parallel()
	now := jevHealthFixedNow()

	cases := []struct {
		name    string
		write   func(t *testing.T, town string)
		wantErr string
	}{
		{name: "missing", wantErr: "patrol_last_run.json is missing"},
		{
			name: "unreadable",
			write: func(t *testing.T, town string) {
				// A directory where the record should be: the read fails, and the file is not missing.
				require.NoError(t, os.MkdirAll(filepath.Join(town, "daemon", patrolLastRunFile), 0o755))
			},
			wantErr: "reading patrol_last_run.json",
		},
		{
			name: "corrupt",
			write: func(t *testing.T, town string) {
				dir := filepath.Join(town, "daemon")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, patrolLastRunFile), []byte(`{"last_run": {not json`), 0o644))
			},
			wantErr: "patrol_last_run.json is not valid JSON",
		},
		{
			name: "valid",
			write: func(t *testing.T, town string) {
				writeJevPatrolRuns(t, town, map[string]time.Time{"jev_dedupe": now.Add(-3 * time.Minute)})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.write != nil {
				tc.write(t, town)
			}
			runs := jevHealthPatrolRuns(town)
			if tc.wantErr != "" {
				require.Error(t, runs.err)
				assert.Contains(t, runs.err.Error(), tc.wantErr)
				assert.NotContains(t, runs.err.Error(), "not json", "the error must not quote the file")
				return
			}
			require.NoError(t, runs.err)
			assert.WithinDuration(t, now.Add(-3*time.Minute), runs.at["jev_dedupe"], time.Second)
		})
	}
}
