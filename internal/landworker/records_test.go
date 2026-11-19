package landworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// failNotes is the beads fake whose note writes all fail.
type failNotes struct {
	*beadsfake.Fake
	err error
}

func (f failNotes) AppendNotes(string, string) error { return f.err }

// TestDrainRecordQueue: a pass writes the records an earlier pass queued and
// drops each one its bead already carries without a second write; one that
// still cannot be written stays queued and is counted in one log line
// (gt-qa5ge8).
func TestDrainRecordQueue(t *testing.T) {
	t.Parallel()
	note := land.RejectionNote{Attempt: 1, Kind: string(land.RejectGate), Reason: "gate failed",
		Branch: branch, Target: "main", MR: "gt-abc", Head: noteHead}
	noteRec := land.PendingRecord{Bead: "gt-abc", Kind: land.RecordKindNotes, Key: noteHead + " gate",
		Text: land.FormatRejectionNote(note)}
	commentRec := land.PendingRecord{Bead: "gt-abc", Kind: land.RecordKindComment, Key: "[head 99999999]",
		Text: "om approve 0.90 (om v1.0.0, 2026-11-19T00:00:00Z–2026-11-19T00:01:00Z) [head 99999999]"}
	cases := []struct {
		name    string
		rec     land.PendingRecord
		notes   string
		comment bool
		fail    bool
		queued  bool
	}{
		{"note written", noteRec, "", false, false, false},
		{"note already present", noteRec, noteRec.Text, false, false, false},
		{"note write fails", noteRec, "", false, true, true},
		{"comment written", commentRec, "", false, false, false},
		{"comment already present", commentRec, "", true, false, false},
		{"unknown kind stays queued", land.PendingRecord{Bead: "gt-abc", Kind: "other", Key: "k", Text: "t"}, "", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task",
				Assignee: "gastown/polecats/opal", Notes: tc.notes})
			if tc.comment {
				if err := h.bd.AddComment("gt-abc", tc.rec.Text); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fail {
				h.w.Beads = failNotes{Fake: h.bd, err: errors.New("bd down")}
			}
			var logs []string
			h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			q := land.RecordQueue{Path: filepath.Join(t.TempDir(), "records.jsonl")}
			//testpolicy:allow no-exec-files — a directory, not a file: its owner search bit lets the queue reach its record file
			if err := os.Chmod(filepath.Dir(q.Path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := seedRecord(q, tc.rec); err != nil {
				t.Fatal(err)
			}
			h.w.Records = &q
			h.w.Pass(context.Background())
			recs, err := q.Pending()
			if err != nil {
				t.Fatal(err)
			}
			if (len(recs) > 0) != tc.queued {
				t.Fatalf("queue holds %d record(s); want queued=%v", len(recs), tc.queued)
			}
			if once := strings.Count(strings.Join(logs, "\n"), "still waiting for bd") == 1; once != tc.queued {
				t.Errorf("queue still-waiting line logged %v; want once=%v: %v", once, tc.queued, logs)
			}
			if tc.queued {
				return
			}
			issue, err := h.bd.Show("gt-abc")
			if err != nil {
				t.Fatal(err)
			}
			if tc.rec.Kind == land.RecordKindNotes && issue.Notes != tc.rec.Text {
				t.Errorf("notes %q; want exactly the one queued note", issue.Notes)
			}
			if tc.rec.Kind == land.RecordKindComment && len(h.comments(t, "gt-abc")) != 1 {
				t.Errorf("comments %v; want exactly the one the record names", h.comments(t, "gt-abc"))
			}
		})
	}
}

// seedRecord puts rec in q: through Enqueue for the kinds it accepts, and as a
// hand-edited queue file for one it refuses.
func seedRecord(q land.RecordQueue, rec land.PendingRecord) error {
	if rec.Kind != "other" {
		return q.Enqueue(rec)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(q.Path, append(line, '\n'), 0o600)
}
