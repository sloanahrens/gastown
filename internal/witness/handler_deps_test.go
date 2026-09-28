package witness

import (
	"testing"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// newTestHandlers returns a handlers value a single test owns. Every
// collaborator starts real — and the destructive ones (session restart, tmux
// kill, `gt polecat nuke`) panic under a test binary (gt-5itbt) — so a test
// fakes exactly the fields its path reaches, on its own instance. Nothing is
// swapped process-wide, which is what lets these tests run in parallel.
//
// The notifier is the exception: it starts as a recorder, so no test mails or
// escalates through a live gt. Read it with recorderOf.
func newTestHandlers() *handlers { return &handlers{notifier: notifyfake.New()} }

// recorderOf returns the recorder newTestHandlers installed on h.
func recorderOf(t *testing.T, h *handlers) *notifyfake.Recorder {
	t.Helper()
	rec, ok := h.notifier.(*notifyfake.Recorder)
	if !ok {
		t.Fatalf("handlers notifier is %T, want *notifyfake.Recorder", h.notifier)
	}
	return rec
}
