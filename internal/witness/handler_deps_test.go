package witness

import (
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
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

func TestHandlersNotifierKeepsAnInjectedOne(t *testing.T) {
	t.Parallel()
	rec := notifyfake.New()
	h := &handlers{notifier: rec}
	if got := h.notify("/town"); got != rec {
		t.Fatalf("notify() = %T, want the injected recorder", got)
	}
}

// TestHandlersNotifierDefaultsToGtFromTheTownRoot pins the invocation the
// witness's slot-open mail and dialog-blocked escalation always used: gt from
// PATH, run from the town root, with the witness's own environment.
func TestHandlersNotifierDefaultsToGtFromTheTownRoot(t *testing.T) {
	t.Parallel()
	got := newHandlers().notify("/town")
	cli, ok := got.(*notify.CLI)
	if !ok {
		t.Fatalf("notify() = %T, want *notify.CLI", got)
	}
	if cli.Dir != "/town" || cli.Bin != "" || cli.Env != nil {
		t.Fatalf("CLI = %+v, want gt from PATH run from /town with the inherited env", cli)
	}
}
