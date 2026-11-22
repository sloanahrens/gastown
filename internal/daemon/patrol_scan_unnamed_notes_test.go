package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// notesReader is a work bead reader whose bead carries notes, or whose read fails.
type notesReader struct {
	notes string
	err   error
}

func (n *notesReader) Show(string) (*beads.Issue, error) {
	if n.err != nil {
		return nil, n.err
	}
	return &beads.Issue{Notes: n.notes}, nil
}

func (n *notesReader) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, nil }

// TestPatrolScanUnnamedNotesTail: the escalation filed for a seat that stopped on ESCALATED
// carries the last 300 characters of the bead's notes, which is where the 2x size refusal says
// what it refused; a notes read that fails is named in the message and the filing still goes out;
// a second call for the same bead repeats the key and sends its own message (gt-exlzz1).
func TestPatrolScanUnnamedNotesTail(t *testing.T) {
	t.Parallel()
	t.Run("the last 300 characters", func(t *testing.T) {
		t.Parallel()
		h, rec := unnamedHost(t)
		notes := "FIRST-LINE-NOT-KEPT " + strings.Repeat("x", 400) + " gt done refused at 140 added vs 60 declared"
		reader := &notesReader{notes: notes}
		h.d.openWorkBeads = func([]string, time.Duration) workBeadReader { return reader }

		if err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74"); err != nil {
			t.Fatalf("FileUnnamedEscalation: %v", err)
		}

		reason := rec.Escalations()[0].Escalation.Reason
		tail := []rune(notes)
		want := string(tail[len(tail)-300:])
		if !strings.Contains(reason, want) || strings.Contains(reason, "FIRST-LINE-NOT-KEPT") {
			t.Errorf("reason = %q, want the last 300 characters of the notes and not their start", reason)
		}
		if !strings.Contains(reason, "gt done refused at 140 added vs 60 declared") {
			t.Errorf("reason %q lacks the refusal line the notes end with", reason)
		}
	})
	t.Run("short notes are kept whole", func(t *testing.T) {
		t.Parallel()
		h, rec := unnamedHost(t)
		h.d.openWorkBeads = func([]string, time.Duration) workBeadReader { return &notesReader{notes: "short note"} }
		if err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74"); err != nil {
			t.Fatal(err)
		}
		if reason := rec.Escalations()[0].Escalation.Reason; !strings.Contains(reason, "short note") {
			t.Errorf("reason = %q, want the short notes whole", reason)
		}
	})
	t.Run("empty and blank notes say so", func(t *testing.T) {
		t.Parallel()
		for _, notes := range []string{"", "  \n\t "} {
			h, rec := unnamedHost(t)
			h.d.openWorkBeads = func([]string, time.Duration) workBeadReader { return &notesReader{notes: notes} }
			if err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74"); err != nil {
				t.Fatal(err)
			}
			if reason := rec.Escalations()[0].Escalation.Reason; !strings.Contains(reason, "its notes are empty") {
				t.Errorf("notes %q: reason = %q, want it to say the notes are empty", notes, reason)
			}
		}
	})
	t.Run("a failed read is named and the filing still goes out", func(t *testing.T) {
		t.Parallel()
		h, rec := unnamedHost(t)
		h.d.openWorkBeads = func([]string, time.Duration) workBeadReader {
			return &notesReader{err: errors.New("bd show: connection refused")}
		}
		if err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74"); err != nil {
			t.Fatalf("a failed notes read failed the filing: %v", err)
		}
		calls := rec.Escalations()
		if len(calls) != 1 || !strings.Contains(calls[0].Escalation.Reason, "notes unreadable: bd show: connection refused") {
			t.Errorf("escalations = %d, want one naming the failed notes read", len(calls))
		}
	})
	t.Run("a second exit repeats the key with its own message", func(t *testing.T) {
		t.Parallel()
		h, rec := unnamedHost(t)
		reader := &notesReader{notes: "first exit note"}
		h.d.openWorkBeads = func([]string, time.Duration) workBeadReader { return reader }
		if err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74"); err != nil {
			t.Fatal(err)
		}
		reader.notes = "second exit note"
		if err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74"); err != nil {
			t.Fatal(err)
		}
		calls := rec.Escalations()
		if len(calls) != 2 || calls[0].Escalation.Fingerprint != calls[1].Escalation.Fingerprint ||
			!strings.Contains(calls[1].Escalation.Reason, "second exit note") || strings.Contains(calls[1].Escalation.Reason, "first exit note") {
			t.Errorf("escalations = %+v, want the same key and the second message carrying the second notes", calls)
		}
	})
}
