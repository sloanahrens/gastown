package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// listAnnounceMessages must find the messages the router writes. The router
// labels an announce with exactly gt:message, from:<sender>, and
// announce:<channel> (see sendToAnnounce), so the reader has to query that
// label rather than a label nothing writes.
func TestListAnnounceMessagesReadsRouterLabels(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	id := createMessage(t, bd, "disk full", "gt:message", "from:x", "announce:ops")

	msgs, err := listAnnounceMessages(bd, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("announce messages = %+v, want 1 message", msgs)
	}
	if msgs[0].ID != id || msgs[0].From != "x" {
		t.Fatalf("announce message = %+v, want id %s from x", msgs[0], id)
	}
}
