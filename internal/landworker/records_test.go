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

// queueDir is a private directory for a record queue: t.TempDir is 0777 minus
// the umask, and the queue refuses a group- or world-writable one (gt-qa5ge8).
func queueDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	//testpolicy:allow no-exec-files — a directory, not a file: its owner search bit lets the queue reach its file
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// drainNoteRecord is the queued note the drain tests write: a gate rejection of noteHead.
func drainNoteRecord() land.PendingRecord {
	note := land.RejectionNote{Attempt: 1, Kind: string(land.RejectGate), Reason: "gate failed", Branch: branch, Target: "main", MR: "gt-abc", Head: noteHead}
	return land.PendingRecord{Bead: "gt-abc", Kind: land.RecordKindNotes, Key: noteHead + " gate", Text: land.FormatRejectionNote(note)}
}

// seedRecord puts rec in q, hand-editing the line for a kind Enqueue refuses (gt-qa5ge8).
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

// TestDrainRecordQueue: a queued record is written and dropped, or kept with one
// log line while bd refuses; one the bead already carries is dropped unwritten
// (gt-qa5ge8).
func TestDrainRecordQueue(t *testing.T) {
	t.Parallel()
	noteRec := drainNoteRecord()
	newer := land.FormatRejectionNote(land.RejectionNote{Attempt: 2, Kind: "rebase", Reason: "rebase conflicted", Branch: branch, Target: "main", MR: "gt-abc", Head: noteHead})
	commentRec := land.PendingRecord{Bead: "gt-abc", Kind: land.RecordKindComment, Key: "[head 99999999]", Text: "om approve 0.90 (om v1.0.0, 2026-11-19T00:00:00Z–2026-11-19T00:01:00Z) [head 99999999]"}
	cases := []struct {
		name                  string
		rec                   land.PendingRecord
		notes                 string
		comment, fail, queued bool
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
			h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal", Notes: tc.notes})
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
			q := land.RecordQueue{Path: filepath.Join(queueDir(t), "records.jsonl")}
			if err := seedRecord(q, tc.rec); err != nil {
				t.Fatal(err)
			}
			h.w.Records = &q
			h.w.Pass(context.Background())
			recs, err := q.Pending()
			if err != nil {
				t.Fatal(err)
			}
			once := strings.Count(strings.Join(logs, "\n"), "still waiting for bd") == 1
			if (len(recs) > 0) != tc.queued || once != tc.queued {
				t.Fatalf("queue holds %d record(s), still-waiting logged once=%v; want queued=%v: %v", len(recs), once, tc.queued, logs)
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

// TestDrainRecordQueueFaults: an unreadable queue is logged and the pass goes
// on; a refused removal leaves the record queued, its note already on the bead
// (gt-qa5ge8).
func TestDrainRecordQueueFaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name               string
		unreadable, refuse bool
		wantNote           bool
		wantLog            string
	}{
		{"read fails, the pass goes on", true, false, false, "reading the landing record queue"},
		{"removal fails, the entry stays", false, true, true, "removing a landing record its bead now carries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal"})
			var logs []string
			h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			rec := drainNoteRecord()
			q := &land.RecordQueue{Path: filepath.Join(queueDir(t), "records.jsonl")}
			if err := q.Enqueue(rec); err != nil {
				t.Fatal(err)
			}
			h.w.Records = q
			switch {
			case tc.refuse:
				h.w.Records = removeFails{q}
			case tc.unreadable:
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
