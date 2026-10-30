package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	writeJevSpendLedger(t, town,
		jevSpendLine(now.Add(-30*time.Minute), "jev-dedupe", 100, 50),
		jevSpendLine(now.Add(-20*time.Minute), "jev-thrash", 100, 50),
	)
	return town
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

// runJevHealthCommand drives the subcommand from inside town, as gt would.
func runJevHealthCommand(t *testing.T, town string, asJSON bool) (string, error) {
	t.Helper()
	//testpolicy:allow no-chdir — the cwd is the input under test: runJevHealth finds the town from the working directory
	t.Chdir(town)
	var out bytes.Buffer
	jevHealthCmd.SetOut(&out)
	jevHealthCmd.SetErr(&bytes.Buffer{})
	args := []string{"--json=false"}
	if asJSON {
		args = []string{"--json=true"}
	}
	if err := jevHealthCmd.ParseFlags(args); err != nil {
		t.Fatalf("parsing the flags: %v", err)
	}
	err := runJevHealth(jevHealthCmd, nil)
	return out.String(), err
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
// hours old prints stale and exits 1.
//
//testpolicy:allow parallel — the command reads the process working directory, which t.Chdir sets for the whole process
func TestJevHealthCommand(t *testing.T) {
	now := time.Now()
	healthy := newJevHealthTown(t, now, 5*time.Minute)
	stale := newJevHealthTown(t, now, 2*time.Hour)

	t.Run("healthy table", func(t *testing.T) {
		out, err := runJevHealthCommand(t, healthy, false)
		require.NoError(t, err)
		assert.Contains(t, out, "BATTERY")
		row := jevHealthTableRow(t, out, "dedupe")
		assert.Contains(t, row, " ok")
		assert.NotContains(t, row, "stale")
	})

	t.Run("healthy json rows", func(t *testing.T) {
		out, err := runJevHealthCommand(t, healthy, true)
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
		assert.False(t, targets.Enabled, "a patrol key the config does not set is off")
		assert.Equal(t, health.Disabled, targets.Status)
	})

	t.Run("stale fixture exits 1", func(t *testing.T) {
		out, err := runJevHealthCommand(t, stale, false)

		code, ok := IsSilentExit(err)
		require.True(t, ok, "want a silent coded exit, got %v", err)
		assert.Equal(t, 1, code)
		assert.Contains(t, jevHealthTableRow(t, out, "dedupe"), " stale")
	})
}

// TestJevHealthRefusesAMissingTown covers the town lookup's failure path.
func TestJevHealthRefusesAMissingTown(t *testing.T) {
	t.Parallel()
	err := jevHealthInTown(&bytes.Buffer{}, func() (string, error) { return "", errors.New("no town") }, false, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finding the town root")
}

// TestJevHealthRefusesABrokenPatrolConfig covers the config read's failure
// path: a config that does not parse must fail the command, not read as a town
// with every battery off.
func TestJevHealthRefusesABrokenPatrolConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(town, "mayor"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(town, "mayor", "daemon.json"), []byte("{"), 0o644))

	err := jevHealthInTown(&bytes.Buffer{}, townFinder(town), false, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the patrol config")
}

// TestJevHealthRefusesAnUnreadableLedger covers the ledger read's failure
// path: a ledger that is a directory cannot be read, and must not read as no
// calls.
func TestJevHealthRefusesAnUnreadableLedger(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(jevHealthDir(t, town), "usage.jsonl"), 0o700))

	err := jevHealthInTown(&bytes.Buffer{}, townFinder(town), false, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage ledger")
}

// TestJevHealthRefusesAnUnreadableLog covers the log read's failure path: a
// log that is a directory cannot be read, and must not read as a quiet
// battery.
func TestJevHealthRefusesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(jevHealthDir(t, town), "attention.jsonl"), 0o700))

	err := jevHealthInTown(&bytes.Buffer{}, townFinder(town), false, time.Now())
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

	err := jevHealthInTown(&bytes.Buffer{}, townFinder(town), false, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attention.cursor.json")
}

// TestJevHealthReportsAWriteFailure covers both write paths: the table's flush
// and the JSON encoder's write.
func TestJevHealthReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for name, asJSON := range map[string]bool{"table": false, "json": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := jevHealthInTown(jevHealthFailingWriter{}, townFinder(town), asJSON, time.Now())
			require.Error(t, err)
		})
	}
}
