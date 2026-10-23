package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ledgerLines reads path and decodes its lines.
func ledgerLines(t *testing.T, path string) []LedgerRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []LedgerRecord
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var rec LedgerRecord
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		out = append(out, rec)
	}
	return out
}

func TestChoice_WithLedgerAppendsOneLine(t *testing.T) {
	t.Parallel()
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	dir := filepath.Join(t.TempDir(), "jev")
	path := filepath.Join(dir, "usage.jsonl")
	c := newTestClient(t, rs, WithLedger(path), WithCaller("test caller"), WithWarnings(io.Discard))

	before := time.Now()
	got, err := c.Choice(context.Background(), shoeState, "Which team should handle this?", shoeCriteria)
	require.NoError(t, err)

	lines := ledgerLines(t, path)
	require.Len(t, lines, 1)
	assert.Equal(t, "test caller", lines[0].Caller)
	assert.Equal(t, got.Usage.InputTokens, lines[0].InputTokens)
	assert.Equal(t, got.Usage.OutputTokens, lines[0].OutputTokens)
	assert.False(t, lines[0].TS.IsZero())
	assert.WithinDuration(t, before, lines[0].TS, time.Minute)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the ledger is private")
	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm(), "the ledger directory is private")
}

func TestChoice_WithLedgerKeepsTheDefaultCaller(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	c := newTestClient(t, &respond{body: readTestdata(t, "choice_response.json")}, WithLedger(path))

	_, err := c.Choice(context.Background(), shoeState, "q?", shoeCriteria)
	require.NoError(t, err)

	lines := ledgerLines(t, path)
	require.Len(t, lines, 1)
	assert.Equal(t, defaultCaller, lines[0].Caller)
}

func TestChoice_LedgerUnwritableWarnsAndKeepsTheAnswer(t *testing.T) {
	t.Parallel()
	// A regular file where the ledger's directory should be makes every
	// write fail without touching permissions.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	var warnings strings.Builder
	c := newTestClient(t, &respond{body: readTestdata(t, "choice_response.json")},
		WithLedger(filepath.Join(blocker, "usage.jsonl")), WithWarnings(&warnings))

	got, err := c.Choice(context.Background(), shoeState, "Which team should handle this?", shoeCriteria)

	require.NoError(t, err, "a ledger failure never fails the call")
	assert.Equal(t, "returns", got.Choice)
	assert.EqualValues(t, 328, got.Usage.InputTokens)
	assert.Contains(t, warnings.String(), "usage ledger")
	assert.NotContains(t, warnings.String(), testKey)
}

func TestChoice_LedgerWritesNoPromptAnswerOrKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs, WithLedger(path), WithCaller("test caller"))

	_, err := c.Choice(context.Background(), shoeState, "Which team should handle this?", shoeCriteria)
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	line := string(data)
	require.Contains(t, string(rs.raw), shoeState, "the request did carry the state")
	assert.NotContains(t, line, shoeState)
	assert.NotContains(t, line, "Which team should handle this?")
	assert.NotContains(t, line, testKey)

	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	assert.Len(t, fields, 4, "a line holds the four fields and nothing else")
}

func TestChoice_NoLedgerLineForAFailedCall(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	c := newTestClient(t, &respond{status: http.StatusUnauthorized, body: `{"error":"bad key"}`}, WithLedger(path))

	_, err := c.Choice(context.Background(), shoeState, "q?", shoeCriteria)

	require.ErrorIs(t, err, ErrUnauthorized)
	assert.NoFileExists(t, path)
}

func TestAppendLine_TightensPreexistingModes(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "jev")
	path := filepath.Join(dir, "usage.jsonl")
	require.NoError(t, os.Mkdir(dir, 0o755))
	//testpolicy:allow no-exec-files — the target is a directory, not a file: the test needs a loose ledger directory to tighten
	require.NoError(t, os.Chmod(dir, 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	require.NoError(t, os.Chmod(path, 0o644))

	require.NoError(t, appendLine(path, []byte(`{"n":1}`)))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "an old 0644 ledger is tightened")
	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm(), "an old 0755 ledger directory is tightened")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "{}\n{\"n\":1}\n", string(data), "the line is appended after the old content")
}

func TestAppendLine_ConcurrentWritersKeepLinesWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	const writers, perWriter = 8, 20
	// Lines this long are the ones a single write is most likely to split.
	pad := strings.Repeat("p", 64<<10)

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				line := fmt.Sprintf(`{"w":%d,"i":%d,"pad":%q}`, w, i, pad)
				assert.NoError(t, appendLine(path, []byte(line)))
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	require.Len(t, lines, writers*perWriter)
	seen := make(map[[2]int]bool)
	for _, line := range lines {
		var rec struct {
			W   int    `json:"w"`
			I   int    `json:"i"`
			Pad string `json:"pad"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "a line was split or interleaved")
		assert.Equal(t, pad, rec.Pad)
		seen[[2]int{rec.W, rec.I}] = true
	}
	assert.Len(t, seen, writers*perWriter, "every write landed exactly once")
}

// shortWrite stores only the first half of b and reports err, as a full disk
// does.
func shortWrite(err error) func(*os.File, []byte) (int, error) {
	return func(f *os.File, b []byte) (int, error) {
		n, werr := f.Write(b[:len(b)/2])
		if werr != nil {
			return n, werr
		}
		return n, err
	}
}

func TestAppendLineShortWrite(t *testing.T) {
	t.Parallel()
	for name, werr := range map[string]error{
		"short count with an error": io.ErrClosedPipe,
		"short count with no error": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "usage.jsonl")
			now := time.Now().UTC()
			rec := func(in int) []byte {
				line, err := json.Marshal(LedgerRecord{TS: now, Caller: "c", InputTokens: in, OutputTokens: 1})
				require.NoError(t, err)
				return line
			}

			err := appendLineWith(path, rec(100), shortWrite(werr))
			require.Error(t, err, "a short write is an error, not absorbed")
			if werr == nil {
				require.ErrorIs(t, err, io.ErrShortWrite)
			}
			require.NoError(t, appendLine(path, rec(7)))

			rep, err := ReadSpend(path, now.Add(-time.Hour), now.Add(time.Hour), 1e9)
			require.NoError(t, err)
			assert.Equal(t, 1, rep.Calls, "the good record is counted")
			assert.Equal(t, 7, rep.InputTokens)
			assert.Equal(t, 1, rep.LinesSkipped, "only the partial line is skipped")
		})
	}
}

func TestUsageLedgerPath(t *testing.T) {
	t.Parallel()
	assert.Equal(t, filepath.Join("/town", ".runtime", "jev", "usage.jsonl"), UsageLedgerPath("/town"))
}
