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

// removeFails is a record queue whose removals are refused (gt-qa5ge8).
type removeFails struct{ *land.RecordQueue }

func (removeFails) Remove(land.PendingRecord) error { return errors.New("rewrite refused") }

// drainNoteRecord is the queued note the drain tests write: a gate rejection of noteHead.
func drainNoteRecord() land.PendingRecord {
	note := land.RejectionNote{Attempt: 1, Kind: string(land.RejectGate), Reason: "gate failed",
		Branch: branch, Target: "main", MR: "gt-abc", Head: noteHead}
	return land.PendingRecord{Bead: "gt-abc", Kind: land.RecordKindNotes, Key: noteHead + " gate",
		Text: land.FormatRejectionNote(note)}
}

// TestDrainRecordQueue: a pass writes the records an earlier pass queued and
// drops each one its bead already carries without a second write; one that
// still cannot be written stays queued and is counted in one log line
// (gt-qa5ge8).
func TestDrainRecordQueue(t *testing.T) {
	t.Parallel()
	noteRec := drainNoteRecord()
	newer := land.FormatRejectionNote(land.RejectionNote{Attempt: 2, Kind: "rebase", Reason: "rebase conflicted",
		Branch: branch, Target: "main", MR: "gt-abc", Head: noteHead})
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
		{"note followed by a newer block is not written twice", noteRec, noteRec.Text + "\n" + newer, false, false, false},
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
			if tc.rec.Kind == land.RecordKindNotes && strings.Count(issue.Notes, tc.rec.Text) != 1 {
				t.Errorf("notes %q; want the queued note exactly once", issue.Notes)
			}
			if tc.rec.Kind == land.RecordKindComment && len(h.comments(t, "gt-abc")) != 1 {
				t.Errorf("comments %v; want exactly the one the record names", h.comments(t, "gt-abc"))
			}
		})
	}
}

// TestDrainRecordQueueFaults: a queue that cannot be read is logged and the
// pass goes on; a record whose removal is refused stays queued, its note
// already on the bead (gt-qa5ge8).
func TestDrainRecordQueueFaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		unreadable bool // the queue path is a directory, so its read fails
		refuse     bool // removals fail
		wantNote   bool
		wantLog    string
	}{
		{"read fails, the pass goes on", true, false, false, "reading the landing record queue"},
		{"removal fails, the entry stays", false, true, true, "removing a landing record its bead now carries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task",
				Assignee: "gastown/polecats/opal"})
			var logs []string
			h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			rec := drainNoteRecord()
			q := &land.RecordQueue{Path: filepath.Join(t.TempDir(), "records.jsonl")}
			if err := q.Enqueue(rec); err != nil {
				t.Fatal(err)
			}
			h.w.Records = q
			if tc.refuse {
				h.w.Records = removeFails{q}
			}
			if tc.unreadable {
				h.w.Records = &land.RecordQueue{Path: t.TempDir()}
			}
			h.w.Pass(context.Background())
			recs, err := q.Pending()
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 1 {
				t.Errorf("queue holds %d record(s); want the record to stay", len(recs))
			}
			issue, err := h.bd.Show("gt-abc")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(issue.Notes, rec.Text); got != tc.wantNote {
				t.Errorf("note on the bead = %v; want %v", got, tc.wantNote)
			}
			if !strings.Contains(strings.Join(logs, "\n"), tc.wantLog) {
				t.Errorf("logs %v; want one naming %q", logs, tc.wantLog)
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
