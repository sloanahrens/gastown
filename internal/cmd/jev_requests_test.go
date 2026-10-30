package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev"
)

// writeJevRequestsLog writes lines to the town's Jev request log.
func writeJevRequestsLog(t *testing.T, townRoot string, lines ...string) {
	t.Helper()
	path := jev.RequestsPath(townRoot)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

// jevRequestLine is one request log line for caller whose body hashed to
// bodySHA.
func jevRequestLine(ts time.Time, caller, bodySHA string) string {
	rec := jev.RequestRecord{
		TS:          ts,
		Caller:      caller,
		QuestionIDs: []string{"q"},
		StateBytes:  10,
		BodyBytes:   20,
		BodySHA256:  bodySHA,
		Status:      200,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// runRequests runs one freshly built requests command against town and returns
// its output, its notes and its error. A command per run keeps one run's flag
// values out of the next, and the deps fake keeps the town lookup off the host
// filesystem.
func runRequests(t *testing.T, town string, args ...string) (out, notes string, err error) {
	t.Helper()
	deps := jevRequestsDeps{findTownRoot: func() (string, error) { return town, nil }}
	cmd := newJevRequestsCmd(deps)
	var outBuf, notesBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&notesBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return outBuf.String(), notesBuf.String(), err
}

// requestLines parses the records a requests run printed.
func requestLines(t *testing.T, out string) []jev.RequestRecord {
	t.Helper()
	var records []jev.RequestRecord
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec jev.RequestRecord
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		records = append(records, rec)
	}
	return records
}

// TestJevRequestsCommand runs the cobra command over a fixture log.
func TestJevRequestsCommand(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	writeJevRequestsLog(t, town,
		jevRequestLine(now.Add(-3*time.Minute), "dispatch", "aaa"),
		jevRequestLine(now.Add(-2*time.Minute), "landing", "bbb"),
		jevRequestLine(now.Add(-time.Minute), "dispatch", "ccc"),
	)

	t.Run("recent keeps the last lines oldest first", func(t *testing.T) {
		out, _, err := runRequests(t, town, "--recent", "2")

		require.NoError(t, err)
		got := requestLines(t, out)
		require.Len(t, got, 2)
		assert.Equal(t, "bbb", got[0].BodySHA256)
		assert.Equal(t, "ccc", got[1].BodySHA256)
	})

	t.Run("caller keeps that caller's last lines", func(t *testing.T) {
		out, _, err := runRequests(t, town, "--caller", "dispatch", "--recent", "1")

		require.NoError(t, err)
		got := requestLines(t, out)
		require.Len(t, got, 1)
		assert.Equal(t, "dispatch", got[0].Caller)
		assert.Equal(t, "ccc", got[0].BodySHA256)
	})

	t.Run("a run without flags does not inherit the last run's flags", func(t *testing.T) {
		_, _, err := runRequests(t, town, "--caller", "dispatch", "--recent", "1")
		require.NoError(t, err)

		out, _, err := runRequests(t, town)

		require.NoError(t, err)
		assert.Len(t, requestLines(t, out), 3, "a fresh command keeps every caller and the default window")
	})

	t.Run("zero recent is refused", func(t *testing.T) {
		_, _, err := runRequests(t, town, "--recent", "0")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "--recent")
	})

	t.Run("a missing log prints no lines", func(t *testing.T) {
		out, notes, err := runRequests(t, t.TempDir())

		require.NoError(t, err)
		assert.Equal(t, "no requests logged yet\n", out)
		assert.Empty(t, notes)
	})
}

// TestJevRequestsOutsideATown covers the town lookup failing, with the lookup
// itself faked: the test never needs a directory the walk could find a town
// in.
func TestJevRequestsOutsideATown(t *testing.T) {
	t.Parallel()
	deps := jevRequestsDeps{findTownRoot: func() (string, error) { return "", errors.New("no town here") }}
	cmd := newJevRequestsCmd(deps)
	cmd.SilenceUsage = true
	var out, notes bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&notes)

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "finding the town root")
	assert.Empty(t, out)
}

// TestWriteJevRequestsOverFixtures drives the reader through the command's
// writer: the paths it reports and the lines it refuses.
func TestWriteJevRequestsOverFixtures(t *testing.T) {
	t.Parallel()
	t.Run("an unusable log path is reported", func(t *testing.T) {
		// A regular file where the log's directory should be fails the open.
		blocker := filepath.Join(t.TempDir(), "blocker")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

		err := writeJevRequests(&bytes.Buffer{}, &bytes.Buffer{}, filepath.Join(blocker, "requests.jsonl"), "", 20)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "request log")
	})

	t.Run("a log that cannot be read is reported", func(t *testing.T) {
		// Opening a directory succeeds; reading it does not.
		err := writeJevRequests(&bytes.Buffer{}, &bytes.Buffer{}, t.TempDir(), "", 20)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "request log")
	})

	t.Run("a writer that fails is reported", func(t *testing.T) {
		town := t.TempDir()
		writeJevRequestsLog(t, town, jevRequestLine(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), "dispatch", "aaa"))

		err := writeJevRequests(failingWriter{}, &bytes.Buffer{}, jev.RequestsPath(town), "", 20)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "printing a request line")
	})

	t.Run("the count of unreadable lines goes to the notes", func(t *testing.T) {
		town := t.TempDir()
		now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
		writeJevRequestsLog(t, town,
			jevRequestLine(now, "dispatch", "aaa"),
			"{not json",
			jevRequestLine(now, "dispatch", "ccc"),
		)
		var out, notes bytes.Buffer

		require.NoError(t, writeJevRequests(&out, &notes, jev.RequestsPath(town), "", 20))

		assert.Len(t, requestLines(t, out.String()), 2)
		assert.Contains(t, notes.String(), "skipped 1 unreadable line(s)")
	})
}
