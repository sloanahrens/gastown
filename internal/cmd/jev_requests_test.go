package cmd

import (
	"bytes"
	"encoding/json"
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

// TestJevRequestsCommand runs the cobra command, not the reader, so the flag
// wiring and the town lookup are covered too. Parsing the flags and then
// calling runJevRequests stands in for Execute, which would also run the root
// command's pre-run hooks.
//
//testpolicy:allow parallel — the requests command's flag variables are process-wide
func TestJevRequestsCommand(t *testing.T) {
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(town, "mayor"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0o600))
	//testpolicy:allow no-chdir — the cwd is the input under test: runJevRequests finds the town from the working directory
	t.Chdir(town)

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	writeJevRequestsLog(t, town,
		jevRequestLine(now.Add(-3*time.Minute), "dispatch", "aaa"),
		jevRequestLine(now.Add(-2*time.Minute), "landing", "bbb"),
		jevRequestLine(now.Add(-time.Minute), "dispatch", "ccc"),
	)

	// print runs the command with args and returns its output lines.
	print := func(t *testing.T, args ...string) []jev.RequestRecord {
		t.Helper()
		var buf bytes.Buffer
		jevRequestsCmd.SetOut(&buf)
		require.NoError(t, jevRequestsCmd.ParseFlags(args))
		require.NoError(t, runJevRequests(jevRequestsCmd, nil))
		var out []jev.RequestRecord
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var rec jev.RequestRecord
			require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
			out = append(out, rec)
		}
		return out
	}

	t.Run("recent keeps the last lines oldest first", func(t *testing.T) {
		got := print(t, "--recent", "2")

		require.Len(t, got, 2)
		assert.Equal(t, "bbb", got[0].BodySHA256)
		assert.Equal(t, "ccc", got[1].BodySHA256)
	})

	t.Run("caller keeps that caller's last lines", func(t *testing.T) {
		got := print(t, "--caller", "dispatch", "--recent", "1")

		require.Len(t, got, 1)
		assert.Equal(t, "dispatch", got[0].Caller)
		assert.Equal(t, "ccc", got[0].BodySHA256)
	})

	t.Run("zero recent is refused", func(t *testing.T) {
		jevRequestsCmd.SetOut(&bytes.Buffer{})
		require.NoError(t, jevRequestsCmd.ParseFlags([]string{"--recent", "0"}))
		err := runJevRequests(jevRequestsCmd, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--recent")
	})

	t.Run("a missing log prints no lines", func(t *testing.T) {
		var buf bytes.Buffer
		jevRequestsCmd.SetOut(&buf)
		require.NoError(t, writeJevRequests(&buf, filepath.Join(t.TempDir(), "missing.jsonl"), "", 20))
		assert.Equal(t, "no requests logged yet\n", buf.String())
	})

	t.Run("an unusable log path is reported", func(t *testing.T) {
		// A regular file where the log's directory should be fails the open.
		blocker := filepath.Join(t.TempDir(), "blocker")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

		err := writeJevRequests(&bytes.Buffer{}, filepath.Join(blocker, "requests.jsonl"), "", 20)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "request log")
	})

	t.Run("a log that cannot be read is reported", func(t *testing.T) {
		// Opening a directory succeeds; reading it does not.
		err := writeJevRequests(&bytes.Buffer{}, t.TempDir(), "", 20)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "request log")
	})

	t.Run("a writer that fails is reported", func(t *testing.T) {
		err := writeJevRequests(failingWriter{}, jev.RequestsPath(town), "", 20)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "printing a request line")
	})
}

// TestJevRequestsOutsideATown drives the town lookup's failure: the walk from
// an empty directory finds no marker and an empty GT_TOWN_ROOT leaves the
// environment fallback with nothing to return.
//
//testpolicy:allow parallel — GT_TOWN_ROOT and the cwd are process-wide
func TestJevRequestsOutsideATown(t *testing.T) {
	//testpolicy:allow no-env — the empty town root is the input under test: it leaves the fallback nothing to resolve
	t.Setenv("GT_TOWN_ROOT", "")
	//testpolicy:allow no-chdir — the cwd is the input under test: no town marker sits above it
	t.Chdir(t.TempDir())

	var buf bytes.Buffer
	jevRequestsCmd.SetOut(&buf)
	err := runJevRequests(jevRequestsCmd, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "finding the town root")
}
