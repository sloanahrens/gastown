package metrics

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installReceiptsEnv is an Env over town: files are served by town-relative
// path, fail names the paths whose read fails, and any other path is read from
// the town on disk, so the record the writer appends is read back.
func installReceiptsEnv(town string, files map[string]string, fail map[string]error) Env {
	return Env{
		Town: town,
		ReadFile: func(path string) ([]byte, error) {
			rel, err := filepath.Rel(town, path)
			if err != nil {
				return nil, err
			}
			rel = filepath.ToSlash(rel)
			if ferr, ok := fail[rel]; ok {
				return nil, ferr
			}
			if content, ok := files[rel]; ok {
				return []byte(content), nil
			}
			return os.ReadFile(path)
		},
	}
}

// installReceiptLine is one receipt the installer writes: the fields a regime
// event is made from, and the ones it is not.
const installReceiptLine = `{"commit":"abc123","duration_s":12,"event":"installed","merged_at":null,"prev_commit":"def456","reason":null,"source":"manual","ts":"2026-11-10T12:00:00Z"}`

// omReceiptLine is one receipt the om installer writes.
const omReceiptLine = `{"commit":"om987","duration_s":3,"event":"noop","prev_commit":null,"reason":"already-installed","source":"manual","ts":"2026-11-11T01:30:00Z"}`

// recordedInstallRecords reads the town's runtime record back as records.
func recordedInstallRecords(t *testing.T, town string) []RegimeEventRecord {
	t.Helper()
	data, err := os.ReadFile(RegimeEventsPath(town))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	records, err := parseRegimeRecords(data)
	if err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	return records
}

// A sync writes one install event per receipt, from both files, with the
// receipt's commits as the refs either side of the change; a second run writes
// nothing, because the log already holds every event.
func TestRecordInstallReceiptsWritesOneEventPerReceiptAndNoneOnSecondRun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	files := map[string]string{
		"daemon/install-receipts.jsonl": `{"commit":"aaa111","event":"installed","prev_commit":"","ts":"2026-11-09T08:00:00Z"}` + "\n" +
			`{"commit":"bbb222","event":"installed","prev_commit":"aaa111","ts":"2026-11-10T12:00:00Z"}` + "\n",
		"daemon/install-om-receipts.jsonl": omReceiptLine + "\n",
	}
	env := installReceiptsEnv(town, files, nil)
	var logged []string
	logLine := func(line string) { logged = append(logged, line) }

	res, err := RecordInstallReceipts(env, "ace", logLine)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.Recorded != 3 || res.Skipped != 0 {
		t.Fatalf("first run = %+v, want 3 recorded and 0 skipped", res)
	}
	if len(logged) != 0 {
		t.Fatalf("first run logged %q, want nothing", logged)
	}
	records := recordedInstallRecords(t, town)
	if len(records) != 3 {
		t.Fatalf("%d records, want 3: %#v", len(records), records)
	}
	want := map[string]struct {
		before, after, reason, source string
	}{
		"2026-11-09T08:00:00Z": {before: "", after: "aaa111", reason: "installed aaa111", source: "daemon/install-receipts.jsonl"},
		"2026-11-10T12:00:00Z": {before: "aaa111", after: "bbb222", reason: "installed bbb222", source: "daemon/install-receipts.jsonl"},
		"2026-11-11T01:30:00Z": {before: "", after: "om987", reason: "noop om987 (already-installed)", source: "daemon/install-om-receipts.jsonl"},
	}
	for _, rec := range records {
		key := rec.TS.Format(time.RFC3339)
		expect, ok := want[key]
		if !ok {
			t.Fatalf("record at %s was not in the receipts: %#v", key, rec)
		}
		if rec.Kind != "install" || rec.BeforeRef != expect.before || rec.AfterRef != expect.after ||
			rec.Reason != expect.reason || rec.Source != expect.source || rec.Actor != "ace" {
			t.Errorf("record at %s = %#v, want kind install, refs %q -> %q, reason %q, source %q",
				key, rec, expect.before, expect.after, expect.reason, expect.source)
		}
	}

	logged = nil
	res, err = RecordInstallReceipts(env, "ace", logLine)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Recorded != 0 || res.Skipped != 3 {
		t.Fatalf("second run = %+v, want 0 recorded and 3 skipped", res)
	}
	if got := recordedInstallRecords(t, town); len(got) != 3 {
		t.Fatalf("second run left %d records, want the 3 the first wrote", len(got))
	}
}

// A receipt the sync cannot use is reported on one line and the receipts beside
// it still record, in both the file and the line.
func TestRecordInstallReceiptsReportsAnUnusableReceiptAndRecordsTheRest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   map[string]string
		fail    map[string]error
		wantMsg []string
	}{
		{
			name:    "a receipts file that cannot be read",
			files:   map[string]string{"daemon/install-om-receipts.jsonl": omReceiptLine + "\n"},
			fail:    map[string]error{"daemon/install-receipts.jsonl": errors.New("permission denied")},
			wantMsg: []string{"install-receipts.jsonl", "permission denied"},
		},
		{
			name: "a line that is not a JSON object",
			files: map[string]string{
				"daemon/install-receipts.jsonl": "not json at all\n" + `{"commit":"ccc333","event":"installed","ts":"2026-11-10T12:00:00Z"}` + "\n",
			},
			wantMsg: []string{"install-receipts.jsonl line 1", "decoding"},
		},
		{
			name: "a line with no event",
			files: map[string]string{
				"daemon/install-receipts.jsonl": `{"commit":"ccc333","ts":"2026-11-10T12:00:00Z"}` + "\n" +
					`{"commit":"ddd444","event":"installed","ts":"2026-11-10T13:00:00Z"}` + "\n",
			},
			wantMsg: []string{"install-receipts.jsonl line 1", "no event"},
		},
		{
			name: "a line with no timestamp",
			files: map[string]string{
				"daemon/install-receipts.jsonl": `{"commit":"ccc333","event":"installed"}` + "\n" +
					`{"commit":"ddd444","event":"installed","ts":"2026-11-10T13:00:00Z"}` + "\n",
			},
			wantMsg: []string{"install-receipts.jsonl line 1", "no timestamp"},
		},
		{
			name: "a line whose timestamp does not parse",
			files: map[string]string{
				"daemon/install-receipts.jsonl": `{"commit":"ccc333","event":"installed","ts":"yesterday"}` + "\n" +
					`{"commit":"ddd444","event":"installed","ts":"2026-11-10T13:00:00Z"}` + "\n",
			},
			wantMsg: []string{"install-receipts.jsonl line 1", "invalid ISO timestamp"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			var logged []string
			res, err := RecordInstallReceipts(installReceiptsEnv(town, tc.files, tc.fail), "ace",
				func(line string) { logged = append(logged, line) })
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if res.Recorded != 1 {
				t.Fatalf("recorded = %d, want the one receipt that could be used", res.Recorded)
			}
			if len(logged) != 1 {
				t.Fatalf("logged %q, want exactly one line", logged)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(logged[0], want) {
					t.Errorf("logged %q, want it to name %q", logged[0], want)
				}
			}
			if got := recordedInstallRecords(t, town); len(got) != 1 {
				t.Fatalf("%d records, want the one usable receipt", len(got))
			}
		})
	}
}

// Two receipts that make the same event are recorded once: the reader counts
// the duplicate once, so the log holds it once.
func TestRecordInstallReceiptsCountsARepeatedReceiptOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	line := `{"commit":"aaa111","event":"installed","ts":"2026-11-09T08:00:00Z"}` + "\n"
	files := map[string]string{
		"daemon/install-receipts.jsonl":    line,
		"daemon/install-om-receipts.jsonl": line,
	}
	res, err := RecordInstallReceipts(installReceiptsEnv(town, files, nil), "ace", nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Recorded != 1 || res.Skipped != 1 {
		t.Fatalf("sync = %+v, want 1 recorded and 1 skipped", res)
	}
	if got := recordedInstallRecords(t, town); len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
}

// A record the log already holds but cannot be read fails the run and appends
// nothing: a log read as empty would replay every receipt.
func TestRecordInstallReceiptsRefusesAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := RegimeEventsPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"daemon/install-receipts.jsonl": installReceiptLine + "\n"}
	_, err := RecordInstallReceipts(installReceiptsEnv(town, files, nil), "ace", nil)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want the unreadable record named", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "not a record\n" {
		t.Fatalf("record = %q, want it left as it was", data)
	}
}

// A record the log refuses to take ends the run with the writer's error: the
// receipts it already wrote keep their lines, and a later run picks up from
// there.
func TestRecordInstallReceiptsReturnsTheWriterError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	files := map[string]string{"daemon/install-receipts.jsonl": installReceiptLine + "\n"}
	_, err := recordInstallReceipts(installReceiptsEnv(town, files, nil), RegimeEventsPath(town), "ace", nil,
		func(RegimeEventRecord) error { return errors.New("disk full") })
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the writer's error", err)
	}
}

// A record the run cannot read for a reason other than being absent is an
// error, not an empty record it replays into.
func TestRecordInstallReceiptsRefusesAnUnreadableRecordRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	files := map[string]string{"daemon/install-receipts.jsonl": installReceiptLine + "\n"}
	fail := map[string]error{".runtime/metrics/regime-events.jsonl": errors.New("input/output error")}
	_, err := RecordInstallReceipts(installReceiptsEnv(town, files, fail), "ace", nil)
	if err == nil || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("err = %v, want the unreadable record's error", err)
	}
}

// A sync with no actor to record under is refused before it reads anything.
func TestRecordInstallReceiptsRefusesNoActor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	files := map[string]string{"daemon/install-receipts.jsonl": installReceiptLine + "\n"}
	_, err := RecordInstallReceipts(installReceiptsEnv(town, files, nil), "  ", nil)
	if err == nil || !strings.Contains(err.Error(), "no actor") {
		t.Fatalf("err = %v, want the missing-actor refusal", err)
	}
	if _, statErr := os.Stat(RegimeEventsPath(town)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a refused sync left %s behind (%v)", RegimeEventsPath(town), statErr)
	}
}

// The receipts read through Env.ReadFile, and an Env without one is a wiring
// error rather than a town with no receipts.
func TestRecordInstallReceiptsRequiresAReader(t *testing.T) {
	t.Parallel()
	_, err := RecordInstallReceipts(Env{Town: t.TempDir()}, "ace", nil)
	if err == nil || !strings.Contains(err.Error(), "ReadFile") {
		t.Fatalf("err = %v, want the unwired-reader refusal", err)
	}
}
