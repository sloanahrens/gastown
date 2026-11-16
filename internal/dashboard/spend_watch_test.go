package dashboard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spendWatchNow is the instant every watch-file fixture is written at.
var spendWatchNow = time.Date(2026, 11, 1, 17, 4, 5, 0, time.UTC)

// spendSampleStamp is the last_sample a file report names, in the ledger's own
// minute-stamped local form, and spendSampleAt is the instant it denotes.
const spendSampleStamp = "2026-11-01T16:04"

var spendSampleAt = time.Date(2026, 11, 1, 16, 4, 0, 0, time.Local)

// spendReport is the spend tool's report with a balance, its two paces, the
// newest sample it read and the fields the watch file does not carry. The
// sample stamp is the report's own as-of instant; a report that names none is
// spendUndatedReport.
func spendReport(t *testing.T, balance, perHour1h, perHour24h any) json.RawMessage {
	t.Helper()
	return spendReportJSON(t, balance, perHour1h, perHour24h, spendSampleStamp)
}

// spendUndatedReport is a report with a balance and no instant naming where it
// is from, as a tool that only prints a live balance gives.
func spendUndatedReport(t *testing.T, balance, perHour1h float64) json.RawMessage {
	t.Helper()
	return spendReportJSON(t, balance, perHour1h, 2.5, "")
}

// spendReportJSON builds the report with last_sample set to stamp, or left out
// when stamp is empty.
func spendReportJSON(t *testing.T, balance, perHour1h, perHour24h any, stamp string) json.RawMessage {
	t.Helper()
	fields := map[string]any{
		"balance":                 balance,
		"pace_1h":                 map[string]any{"per_hour": perHour1h},
		"pace_24h":                map[string]any{"per_hour": perHour24h},
		"cash":                    60.0,
		"credit":                  14.55,
		"per_day":                 map[string]any{},
		"runway_days_at_24h_pace": 20.0,
	}
	if stamp != "" {
		fields["last_sample"] = stamp
	}
	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	return raw
}

// spendRowsReport is the report the dashboard builds from the town's
// deepseek_balance rows: a balance, its pace, and the instant the balance was
// read.
func spendRowsReport(t *testing.T, balance, perHour1h float64, readAt time.Time) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"balance":    balance,
		"pace_1h":    map[string]any{"per_hour": perHour1h},
		"balance_at": readAt,
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
// took: the dashboard's own report is the reading the file was missing, stamped
// with the newest sample that report names rather than the poll's instant
// (gt-jqtw9, gt-u3fp8i).
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
	assert.True(t, spendSampleAt.Equal(doc.TS), "ts = %s, want the newest sample %s", doc.TS, spendSampleAt)
	assert.Contains(t, string(raw), `"ts":"`+spendSampleAt.UTC().Format(time.RFC3339)+`"`, "ts is not the newest sample's RFC 3339 UTC instant")
}

// A report built from the town's cost rows stamps the watch file with the row's
// own instant, not the poll's: the balance is that row's reading, so a patrol
// that stopped must not pass its last figure off as fresh to the snapshot, gt
// tail and gt report (gt-u3fp8i).
func TestPollSpendStampsARowReportWithTheRowsInstant(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "watch")
	readAt := spendWatchNow.Add(-2 * time.Hour)
	h := NewHub(Config{
		Now:      func() time.Time { return spendWatchNow },
		Spend:    func() json.RawMessage { return spendRowsReport(t, 74.55, 1.25, readAt) },
		WatchDir: dir,
	})

	h.pollSpend()

	doc, raw := readSpendWatch(t, dir)
	require.NotNil(t, doc.Balance, "the watch file carries no balance")
	assert.InDelta(t, 74.55, *doc.Balance, 1e-9)
	assert.True(t, readAt.Equal(doc.TS), "ts = %s, want the row's instant %s", doc.TS, readAt)
	assert.Contains(t, string(raw), `"ts":"2026-11-01T15:04:05Z"`, "ts is not the row's RFC 3339 UTC instant")
}

// A file report names the newest sample it read in last_sample, and that is the
// instant its balance is from: the watch file is stamped with it, never with
// the poll's (gt-u3fp8i).
func TestSpendFileStampsALedgerReportWithItsSampleInstant(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, spendWatchName)
	require.NoError(t, os.WriteFile(path, []byte(`{"ts":"2026-10-14T21:06:00Z","per_hour":0.12,"balance":5.2}`), 0o644))

	require.NoError(t, writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5)))

	doc, _ := readSpendWatch(t, dir)
	require.NotNil(t, doc.Balance)
	assert.InDelta(t, 74.55, *doc.Balance, 1e-9)
	assert.True(t, spendSampleAt.Equal(doc.TS), "ts = %s, want the ledger's newest sample %s", doc.TS, spendSampleAt)
}

// A file report with no balance_at and no last_sample dates nothing. When the
// rows read falls back to it, writing it would restamp the newer row-stamped
// reading with the poll's instant and pass the old balance off as fresh, so the
// writer leaves the file alone (gt-u3fp8i).
func TestSpendFileKeepsANewerRowStampedReadingOverAnUndatedLedgerReport(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, spendWatchName)
	require.NoError(t, writeSpendWatch(dir, spendRowsReport(t, 74.55, 1.25, spendWatchNow)))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, writeSpendWatch(dir, spendUndatedReport(t, 12.5, 0.5)))

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "an undated ledger report replaced the newer row-stamped reading or its stamp")
	doc, _ := readSpendWatch(t, dir)
	require.NotNil(t, doc.Balance)
	assert.InDelta(t, 74.55, *doc.Balance, 1e-9, "the watch file took the undated report's balance")
	assert.True(t, spendWatchNow.Equal(doc.TS), "ts = %s, want the row reading's instant %s", doc.TS, spendWatchNow)
	assert.Empty(t, spendTemps(t, dir), "an undated report left a temporary file behind")
}

// A dated ledger report older than the reading in the file leaves the file
// alone: when the rows read fails the ledger answers with its older sample, and
// writing it would move the watch file back to that balance and stamp (gt-u3fp8i).
func TestSpendFileKeepsANewerRowStampedReadingOverAnOlderDatedLedgerReport(t *testing.T) {
	t.Parallel()

	newer := spendSampleAt.Add(time.Hour)
	dir := t.TempDir()
	path := filepath.Join(dir, spendWatchName)
	require.NoError(t, writeSpendWatch(dir, spendRowsReport(t, 74.55, 1.25, newer)))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, writeSpendWatch(dir, spendReport(t, 12.5, 0.5, 2.5)))

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "an older dated ledger report replaced the newer row-stamped reading")
	assert.Empty(t, spendTemps(t, dir), "an older report left a temporary file behind")
}

// A dated ledger report as new as the reading in the file, or newer, is written.
func TestSpendFileWritesALedgerReportNotOlderThanTheReadingInTheFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, writeSpendWatch(dir, spendRowsReport(t, 74.55, 1.25, spendSampleAt.Add(-time.Hour))))

	require.NoError(t, writeSpendWatch(dir, spendReport(t, 12.5, 0.5, 2.5)))

	doc, _ := readSpendWatch(t, dir)
	require.NotNil(t, doc.Balance)
	assert.InDelta(t, 12.5, *doc.Balance, 1e-9, "a newer ledger report was not written")
	assert.True(t, spendSampleAt.Equal(doc.TS), "ts = %s, want %s", doc.TS, spendSampleAt)
}

// A report with no balance is no reading: the file keeps its own age instead of
// being restamped with a figure the report does not have. A report that will
// not decode is a failure the poll reports, not a silent no-write.
func TestSpendFileWriteIsAtomic(t *testing.T) {
	t.Parallel()

	oldDoc := `{"ts":"2026-10-14T21:06:00Z","per_hour":0.12,"balance":5.2}`
	newDoc := `{"ts":"2026-11-01T17:04:05Z","per_hour":1.25,"balance":74.55}`

	t.Run("a good report replaces the file by rename", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, spendWatchName)
		require.NoError(t, os.WriteFile(path, []byte(oldDoc), 0o644))
		before, err := os.Stat(path)
		require.NoError(t, err)

		require.NoError(t, writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5)))

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

		require.NoError(t, writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5)))

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

		require.NoError(t, writeSpendWatch(dir, spendReport(t, nil, 1.25, 2.5)))

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, oldDoc, string(data), "a report with no balance rewrote the reading")
	})

	t.Run("a report that will not decode is a failure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, spendWatchName)
		require.NoError(t, os.WriteFile(path, []byte(oldDoc), 0o644))

		err := writeSpendWatch(dir, json.RawMessage(`{"balance":"not a number"}`))

		require.Error(t, err, "a report the writer cannot read is not a no-write")
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, oldDoc, string(data), "a failed write left the reading altered")
	})

	t.Run("a balance_at of the zero instant is a failure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, spendWatchName)
		require.NoError(t, os.WriteFile(path, []byte(oldDoc), 0o644))

		report := json.RawMessage(`{"balance":74.55,"balance_at":"0001-01-01T00:00:00Z"}`)
		err := writeSpendWatch(dir, report)

		require.Error(t, err, "a balance with no instant is not a reading")
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, oldDoc, string(data), "a refused write left the reading altered")
	})

	// The temporary file's creation, write and close each refuse in turn. The
	// refusals are scripted rather than made by the directory's mode, which root
	// does not respect, so every row runs under any user.
	t.Run("a failed temporary file leaves the old file intact", func(t *testing.T) {
		t.Parallel()
		refused := errors.New("the directory refuses the write")
		for _, tc := range []struct {
			name   string
			create func(dir string) (spendTempFile, error)
		}{
			{"creation refused", scriptedTemp(refused, nil, nil)},
			{"write refused", scriptedTemp(nil, refused, nil)},
			{"close refused", scriptedTemp(nil, nil, refused)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				path := filepath.Join(dir, spendWatchName)
				require.NoError(t, os.WriteFile(path, []byte(oldDoc), 0o644))

				err := writeSpendWatchVia(dir, []byte(newDoc), tc.create)

				require.ErrorIs(t, err, refused, "a refused temporary file reported no error")
				data, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Equal(t, oldDoc, string(data), "a refused temporary file did not leave the old reading in place")
				assert.Empty(t, spendTemps(t, dir), "a refused temporary file left a temporary file behind")
			})
		}
	})

	t.Run("a temporary file in a missing directory is a failure", func(t *testing.T) {
		t.Parallel()
		// No directory to create the file in refuses it for any user, so this row
		// runs the production creator on a refusal the OS gives under root too.
		dir := filepath.Join(t.TempDir(), "missing")

		err := writeSpendWatchVia(dir, []byte(newDoc), createSpendTemp)

		require.ErrorIs(t, err, os.ErrNotExist, "a temporary file in a missing directory reported no error")
	})

	t.Run("the watch file takes the mode a plain write gives it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// The umask applies to a plain 0644 write, so that file's mode is the one
		// the reading must match whatever the umask is.
		plain := filepath.Join(dir, "plain")
		require.NoError(t, os.WriteFile(plain, nil, 0o644))
		want, err := os.Stat(plain)
		require.NoError(t, err)

		require.NoError(t, writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5)))

		got, err := os.Stat(filepath.Join(dir, spendWatchName))
		require.NoError(t, err)
		assert.Equal(t, want.Mode().Perm(), got.Mode().Perm(), "the watch file's mode is not the one a plain 0644 write gives")
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

		err := writeSpendWatch(dir, spendReport(t, 74.55, 1.25, 2.5))

		require.Error(t, err, "a rename that the target refused reported no error")
		assert.ErrorContains(t, err, "replacing", "a refused rename is not reported as a replacement")
		data, readErr := os.ReadFile(held)
		require.NoError(t, readErr)
		assert.Equal(t, "kept", string(data), "a refused rename changed what was there")
		assert.Empty(t, spendTemps(t, dir), "a refused rename left the temporary file behind")
	})

	t.Run("a last_sample that is not a ledger stamp is a failure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// The writer dates a file report from last_sample, so a value the
		// ledger would not have written is a broken report, not a reading; the
		// writer reports it rather than leaving a file it cannot date.
		report := json.RawMessage(`{"balance":74.55,"last_sample":"10000-01-01T00:00:00"}`)
		err := writeSpendWatch(dir, report)

		require.Error(t, err, "a report whose last_sample is not a stamp reported no error")
		_, statErr := os.Stat(filepath.Join(dir, spendWatchName))
		assert.True(t, os.IsNotExist(statErr), "a reading the writer cannot date was written anyway")
	})
}

// spendTemps is the writer's temporary files still in dir.
func spendTemps(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	require.NoError(t, err)
	return found
}

// scriptedTemp is a temporary file creator that refuses the creation, the write
// or the close as its arguments say. Each file it hands out is a real temporary
// file from createSpendTemp, so a refusal after creation is cleaned up as a
// real one would be.
func scriptedTemp(createErr, writeErr, closeErr error) func(dir string) (spendTempFile, error) {
	return func(dir string) (spendTempFile, error) {
		if createErr != nil {
			return nil, createErr
		}
		f, err := createSpendTemp(dir)
		if err != nil {
			return nil, err
		}
		return &scriptedFile{spendTempFile: f, writeErr: writeErr, closeErr: closeErr}, nil
	}
}

// scriptedFile is a temporary file whose write or close is refused as scripted.
// The real file is always closed, so no descriptor is left open by a refusal.
type scriptedFile struct {
	spendTempFile
	writeErr, closeErr error
}

func (f *scriptedFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.spendTempFile.Write(p)
}

func (f *scriptedFile) Close() error {
	err := f.spendTempFile.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
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

// A spend tool or ledger absent where the dashboard runs gives the pane its
// reason and is not a failed read. A tool that is there and gives no report is
// the failed read, with no reason: the two are never confused.
func TestSpendAbsentIsNotAFailedRead(t *testing.T) {
	t.Parallel()

	absent := NewHub(Config{
		SpendReason: NotInClusterReason,
		SpendHours:  func() SpendRead { return SpendRead{Reason: NotInClusterReason} },
	})
	absent.pollSpend()
	st := absent.State()
	assert.Equal(t, NotInClusterReason, st.SpendReason, "the spend pane has no reason")
	assert.Equal(t, NotInClusterReason, st.SpendHoursReason, "the spend-by-hour pane has no reason")
	assert.False(t, st.Reads[SourceSpend].Error, "an absent tool is marked as a failed read")

	failed := NewHub(Config{Spend: func() json.RawMessage { return nil }})
	failed.pollSpend()
	st = failed.State()
	assert.True(t, st.Reads[SourceSpend].Error, "a tool that gives no report lost its failed-read mark")
	assert.Empty(t, st.SpendReason, "a tool that is there and fails was given the not-in-cluster reason")
}
