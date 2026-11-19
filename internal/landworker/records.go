package landworker

import (
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// drainRecordQueue writes the landing records an earlier pass queued because bd
// would not take them, and drops each one its bead already carries. A record
// that still cannot be written waits for a later pass, and one line says how
// many do. Nil skips it (gt-qa5ge8).
func (w *Worker) drainRecordQueue() {
	if w.Records == nil {
		return
	}
	recs, err := w.Records.Pending()
	if err != nil {
		w.logf("reading the landing record queue: %v", err)
	}
	var remain int
	for _, rec := range recs {
		if !w.writeQueuedRecord(rec) {
			remain++
		} else if err := w.Records.Remove(rec); err != nil {
			w.logf("%s: removing a landing record its bead now carries: %v", rec.Bead, err)
			remain++
		}
	}
	if remain > 0 {
		w.logf("landing record queue: %d record(s) still waiting for bd", remain)
	}
}

// writeQueuedRecord writes rec through bd and reports whether the queue may
// drop it: false means bd could not be read or written, so it waits.
func (w *Worker) writeQueuedRecord(rec land.PendingRecord) bool {
	var err error
	switch rec.Kind {
	case land.RecordKindNotes:
		var issue *beads.Issue
		if issue, err = w.Beads.Show(rec.Bead); err == nil {
			// reject keys the record "<head> <kind>", so a block already holding
			// both means the note is on the bead.
			last, ok := land.ParseRejectionNote(issue.Notes)
			if !ok || last.Head+" "+last.Kind != rec.Key {
				err = w.Beads.AppendNotes(rec.Bead, rec.Text)
			}
		}
	case land.RecordKindComment:
		var comments []beads.Comment
		if comments, err = w.Beads.Comments(rec.Bead); err == nil && !land.VerdictCommentQueued(comments, rec) {
			err = w.Beads.AddComment(rec.Bead, rec.Text)
		}
	default:
		return false
	}
	if err != nil {
		w.logf("%s: draining a queued %s record: %v", rec.Bead, rec.Kind, err)
		return false
	}
	return true
}
