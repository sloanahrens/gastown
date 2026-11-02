package done

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// failingNotesAppender is a bead store whose note write fails, the narrowest
// store RunEndAppender accepts.
type failingNotesAppender struct{ err error }

func (f failingNotesAppender) AppendNotes(id, note string) error { return f.err }

// A failed ESCALATED RUN END write leaves the next seat to be stopped on the
// old clock, so it warns with that consequence rather than passing silently
// (gt-ntvac).
func TestRecordEscalatedRunEndWriteFailureIsLogged(t *testing.T) {
	t.Parallel()
	var warn bytes.Buffer
	at := time.Date(2026, 11, 2, 1, 34, 0, 0, time.UTC)
	RecordEscalatedRunEnd(failingNotesAppender{err: errors.New("dolt unavailable")}, "gt-blocked", "gastown/polecats/nux", at, &warn)
	got := warn.String()
	for _, want := range []string{"gt-blocked", "the next seat may be stopped on this run's clock"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q does not name %q", got, want)
		}
	}
}
