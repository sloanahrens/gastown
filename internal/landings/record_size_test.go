package landings

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// TestRunawayBeadRecordSizeRoundTripsThroughTheLedgerFile: the added lines and
// changed files a landing record carries survive a write and a read of the
// ledger file, so a reader answering how big a bead was gets the written
// numbers back (gt-9bkhff).
func TestRunawayBeadRecordSizeRoundTripsThroughTheLedgerFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                string
		added, filesChanged int
	}{
		{name: "one line, one file", added: 1, filesChanged: 1},
		{name: "a runaway bead", added: 812, filesChanged: 24},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "gastown.jsonl")
			rec := Record{
				Bead:         "gt-abc",
				Rig:          "gastown",
				LandedAt:     time.Date(2026, 9, 30, 14, 5, 6, 0, time.UTC),
				AddedLines:   tc.added,
				FilesChanged: tc.filesChanged,
			}
			line, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			// The written keys are the ones a ledger reader looks for.
			var written struct {
				AddedLines   int `json:"added_lines"`
				FilesChanged int `json:"files_changed"`
			}
			if err := json.Unmarshal(line, &written); err != nil {
				t.Fatal(err)
			}
			if written.AddedLines != tc.added || written.FilesChanged != tc.filesChanged {
				t.Fatalf("written size keys = %d added lines, %d files; want %d and %d",
					written.AddedLines, written.FilesChanged, tc.added, tc.filesChanged)
			}
			appendFile(t, path, string(line)+"\n")

			recs, bad, err := (&Reader{Path: path}).ReadNew()
			if err != nil {
				t.Fatal(err)
			}
			if len(bad) != 0 || len(recs) != 1 {
				t.Fatalf("read %d records, %d bad; want 1 and 0", len(recs), len(bad))
			}
			if recs[0].AddedLines != tc.added || recs[0].FilesChanged != tc.filesChanged {
				t.Fatalf("size = %d added lines, %d files; want %d and %d",
					recs[0].AddedLines, recs[0].FilesChanged, tc.added, tc.filesChanged)
			}
			// The rewrite keeps both keys, so a second read of the same record
			// still answers the size.
			out, err := json.Marshal(recs[0])
			if err != nil {
				t.Fatal(err)
			}
			if keys(t, out) != keys(t, line) {
				t.Fatalf("key set drifted:\n got %s\nwant %s", keys(t, out), keys(t, line))
			}
		})
	}
}

// TestRunawayBeadRecordSizeAbsentInAnOldLineReadsZeroAndRewritesWithoutKeys: a
// record written before the size fields existed reads with zeros and rewrites
// without the keys, so an old line is not given a size it never had
// (gt-9bkhff).
func TestRunawayBeadRecordSizeAbsentInAnOldLineReadsZeroAndRewritesWithoutKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gastown.jsonl")
	appendFile(t, path, d2Line+"\n")

	recs, bad, err := (&Reader{Path: path}).ReadNew()
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 || len(recs) != 1 {
		t.Fatalf("read %d records, %d bad; want 1 and 0", len(recs), len(bad))
	}
	if recs[0].AddedLines != 0 || recs[0].FilesChanged != 0 {
		t.Fatalf("old line size = %d added lines, %d files; want zeros",
			recs[0].AddedLines, recs[0].FilesChanged)
	}
	out, err := json.Marshal(recs[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"added_lines", "files_changed"} {
		if _, ok := m[key]; ok {
			t.Fatalf("a zero-size rewrite wrote %q: %s", key, out)
		}
	}
	if keys(t, out) != keys(t, []byte(d2Line)) {
		t.Fatalf("key set drifted from an old line's:\n got %s\nwant %s", keys(t, out), keys(t, []byte(d2Line)))
	}
}
