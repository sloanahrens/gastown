package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spendWatchNow is the instant every watch-file fixture is written at.
var spendWatchNow = time.Date(2026, 11, 1, 17, 4, 5, 0, time.UTC)

// spendReport is the spend tool's report with a balance, its two paces and the
// fields the watch file does not carry.
func spendReport(t *testing.T, balance, perHour1h, perHour24h any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"balance":                 balance,
		"pace_1h":                 map[string]any{"per_hour": perHour1h},
		"pace_24h":                map[string]any{"per_hour": perHour24h},
		"cash":                    60.0,
		"credit":                  14.55,
		"per_day":                 map[string]any{},
		"runway_days_at_24h_pace": 20.0,
	})
	require.NoError(t, err)
	return raw
}

// spendWatchDoc reads the watch file back the way the snapshot, gt tail and gt
// report read it.
type spendWatchDoc struct {
	TS      time.Time `json:"ts"`
	PerHour *float64  `json:"per_hour"`
	Balance *float64  `json:"balance"`
}

func readSpendWatch(t *testing.T, dir string) (spendWatchDoc, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, spendWatchName))
	require.NoError(t, err, "the poll wrote no %s", spendWatchName)
	var doc spendWatchDoc
	require.NoError(t, json.Unmarshal(data, &doc), "the watch file does not parse")
	return doc, data
}

// The snapshot's DeepSeek columns and the tail's rate are as old as the watch
// file, so each successful spend poll restamps it from the reading it just
// took: the dashboard's own report is the live reading the file was missing
// (gt-jqtw9).
func TestPollSpendWritesTheWatchFile(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "watch")
	h := NewHub(Config{
		Now:      func() time.Time { return spendWatchNow },
		Spend:    func() json.RawMessage { return spendReport(t, 74.55, 1.25, 2.5) },
		WatchDir: dir,
	})

	h.pollSpend()

	doc, raw := readSpendWatch(t, dir)
	require.NotNil(t, doc.Balance, "the watch file carries no balance")
	assert.InDelta(t, 74.55, *doc.Balance, 1e-9, "the watch file did not take the report's balance")
	require.NotNil(t, doc.PerHour, "the watch file carries no per_hour")
	assert.InDelta(t, 1.25, *doc.PerHour, 1e-9, "the watch file did not take the report's hourly rate")
	assert.True(t, spendWatchNow.Equal(doc.TS), "ts = %s, want the poll's now %s", doc.TS, spendWatchNow)
	assert.Contains(t, string(raw), `"ts":"2026-11-01T17:04:05Z"`, "ts is not RFC 3339 UTC")
}

// A report with no balance is no reading: the file keeps its own age instead of
// being restamped with a figure the report does not have. A report that will
// not decode is a failure the poll reports, not a silent no-write.
func TestSpendFileWriteIsAtomic(t *testing.T) {
	t.Parallel()

	oldDoc := `{"ts":"2026-10-14T21:06:00Z","per_hour":0.12,"balance":5.2}`

	t.Run("a good report replaces the file by rename", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, spendWatchName)
		require.NoError(t, os.WriteFile(path, []byte(oldDoc), 0o644))
		before, err := os.Stat(path)
		require.NoError(t, err)

		require.NoError(t, writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5), spendWatchNow))

		after, err := os.Stat(path)
		require.NoError(t, err)
		assert.False(t, os.SameFile(before, after), "the file was written in place, not renamed over")
		doc, _ := readSpendWatch(t, dir)
		require.NotNil(t, doc.Balance)
		assert.InDelta(t, 74.55, *doc.Balance, 1e-9)
		assert.Empty(t, spendTemps(t, dir), "a temporary file was left beside the reading")
	})

	t.Run("another hub's in-flight temporary file is left alone", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// A second hub writing the same town holds its own temporary file. The
		// name seeded here is the fixed one a writer would otherwise truncate and
		// rename over, which is the collision gt-h7vex fixes.
		other := filepath.Join(dir, spendWatchName+".tmp")
		require.NoError(t, os.WriteFile(other, []byte("the other hub's reading in flight"), 0o644))

		require.NoError(t, writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5), spendWatchNow))

		data, err := os.ReadFile(other)
		require.NoError(t, err, "the writer took the other hub's temporary file")
		assert.Equal(t, "the other hub's reading in flight", string(data), "the writer overwrote the other hub's temporary file")
		doc, _ := readSpendWatch(t, dir)
		require.NotNil(t, doc.Balance)
		assert.InDelta(t, 74.55, *doc.Balance, 1e-9)
	})

	t.Run("a report with no balance writes nothing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, spendWatchName)
		require.NoError(t, os.WriteFile(path, []byte(oldDoc), 0o644))

		require.NoError(t, writeSpendWatch(dir, spendReport(t, nil, 1.25, 2.5), spendWatchNow))

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, oldDoc, string(data), "a report with no balance rewrote the reading")
	})

	t.Run("a report that will not decode is a failure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, spendWatchName)
		require.NoError(t, os.WriteFile(path, []byte(oldDoc), 0o644))

		err := writeSpendWatch(dir, json.RawMessage(`{"balance":"not a number"}`), spendWatchNow)

		require.Error(t, err, "a report the writer cannot read is not a no-write")
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, oldDoc, string(data), "a failed write left the reading altered")
	})

	t.Run("a failed write leaves the old file intact", func(t *testing.T) {
		t.Parallel()
		// Root writes into a directory whatever its mode, so the 0500 fixture
		// below cannot refuse the write for it; the subtest needs a non-root run.
		if os.Geteuid() == 0 {
			//testpolicy:allow no-skip — root bypasses the directory's write bit, so under root there is no refused write for this fixture to assert; the condition is the host's uid, as a platform skip is
			t.Skip("root bypasses the directory's write bit, so the 0500 fixture cannot refuse the write")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, spendWatchName)
		require.NoError(t, os.WriteFile(path, []byte(oldDoc), 0o644))
		// A directory that refuses new entries is the write failing on a town
		// whose watch directory the dashboard cannot write.
		//testpolicy:allow no-exec-files — a directory needs its search bit to be entered, so 0500 is this fixture's read-only state: it drops the write bit
		require.NoError(t, os.Chmod(dir, 0o500))
		//testpolicy:allow no-exec-files — the cleanup restores the fixture directory's write bit so the test's own tree can be removed
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		err := writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5), spendWatchNow)

		require.Error(t, err, "a write into a directory that refuses it reported no error")
		//testpolicy:allow no-exec-files — restores the write bit so the failed write's file can be read back
		require.NoError(t, os.Chmod(dir, 0o700))
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, oldDoc, string(data), "a failed write did not leave the old reading in place")
		assert.Empty(t, spendTemps(t, dir), "a failed write left a temporary file behind")
	})

	t.Run("a rename that is refused is a failure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// A non-empty directory where the reading belongs refuses the rename
		// for any user, which is the target going wrong between the write and
		// the replacement.
		held := filepath.Join(dir, spendWatchName, "held")
		require.NoError(t, os.MkdirAll(filepath.Dir(held), 0o755))
		require.NoError(t, os.WriteFile(held, []byte("kept"), 0o644))

		err := writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5), spendWatchNow)

		require.Error(t, err, "a rename that the target refused reported no error")
		data, readErr := os.ReadFile(held)
		require.NoError(t, readErr)
		assert.Equal(t, "kept", string(data), "a refused rename changed what was there")
		assert.Empty(t, spendTemps(t, dir), "a refused rename left the temporary file behind")
	})

	t.Run("an instant that will not encode is a failure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// time.Time encodes no year outside 0-9999, so this reading cannot be
		// written; the writer reports it rather than leaving a file that parses
		// as an older reading than it is.
		err := writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))

		require.Error(t, err, "a reading that would not encode reported no error")
		_, statErr := os.Stat(filepath.Join(dir, spendWatchName))
		assert.True(t, os.IsNotExist(statErr), "a reading that would not encode was written anyway")
	})
}

// spendTemps is the writer's temporary files still in dir.
func spendTemps(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	require.NoError(t, err)
	return found
}

// A dashboard that cannot write the watch file still draws the report it read:
// the write is the town's copy of the reading, not the reading. The failure is
// recorded on the spend read's status so it is visible, and the next poll that
// can write clears it (gt-jqtw9).
func TestPollSpendWriteFailureDoesNotStopThePoll(t *testing.T) {
	t.Parallel()

	// A regular file where the watch directory should be refuses the write for
	// any user; moving it away is the directory becoming writable again.
	dir := filepath.Join(t.TempDir(), "watch")
	require.NoError(t, os.WriteFile(dir, nil, 0o644))
	report := spendReport(t, 74.55, 1.25, 2.5)
	h := NewHub(Config{
		Now:      func() time.Time { return spendWatchNow },
		Spend:    func() json.RawMessage { return report },
		WatchDir: dir,
	})

	h.pollSpend()

	st := h.State()
	require.NotEmpty(t, st.Spend, "a failed watch write dropped the spend report")
	assert.NotNil(t, st.CostCharts, "a failed watch write stopped the charts")
	assert.True(t, st.Reads[SourceSpend].Error, "a failed watch write is not recorded on the spend read")

	require.NoError(t, os.Remove(dir))
	h.pollSpend()

	st = h.State()
	assert.False(t, st.Reads[SourceSpend].Error, "the next good write did not clear the failure")
	doc, _ := readSpendWatch(t, dir)
	require.NotNil(t, doc.Balance)
	assert.InDelta(t, 74.55, *doc.Balance, 1e-9)
}
