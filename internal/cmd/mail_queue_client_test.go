package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

func createMessage(t *testing.T, bd beads.Client, title string, labels ...string) string {
	t.Helper()
	is, err := bd.Create(beads.CreateOptions{Title: title, Labels: labels})
	if err != nil {
		t.Fatalf("create %s: %v", title, err)
	}
	return is.ID
}

// A queue lists only its unclaimed gt:message issues, oldest first, and a
// claim then release round-trips through the claim labels.
func TestQueueMessages_ClaimAndRelease(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	first := createMessage(t, bd, "first", "gt:message", "queue:work", "from:mayor/")
	second := createMessage(t, bd, "second", "gt:message", "queue:work")
	createMessage(t, bd, "not a message", "queue:work")
	createMessage(t, bd, "other queue", "gt:message", "queue:other")
	createMessage(t, bd, "claimed", "gt:message", "queue:work", "claimed-by:gastown/crew/joe")

	msgs, err := listUnclaimedQueueMessages(bd, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != first || msgs[1].ID != second {
		t.Fatalf("unclaimed = %+v, want [%s %s] oldest first", msgs, first, second)
	}
	if msgs[0].From != "mayor/" || msgs[0].Created.IsZero() {
		t.Fatalf("first message = %+v, want from mayor/ with a created time", msgs[0])
	}

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := claimQueueMessage(bd, first, "gastown/crew/ann", now); err != nil {
		t.Fatal(err)
	}
	info, err := getQueueMessageInfo(bd, first)
	if err != nil {
		t.Fatal(err)
	}
	if info.QueueName != "work" || info.ClaimedBy != "gastown/crew/ann" || info.ClaimedAt == nil || !info.ClaimedAt.Equal(now) {
		t.Fatalf("info after claim = %+v", info)
	}

	if err := releaseQueueMessage(bd, first); err != nil {
		t.Fatal(err)
	}
	info, err = getQueueMessageInfo(bd, first)
	if err != nil {
		t.Fatal(err)
	}
	if info.ClaimedBy != "" || info.ClaimedAt != nil {
		t.Fatalf("info after release = %+v, want no claim", info)
	}
}

func TestGetQueueMessageInfo_NotFound(t *testing.T) {
	t.Parallel()
	_, err := getQueueMessageInfo(beadsfake.New(), "gt-missing")
	if err == nil || !strings.Contains(err.Error(), "message not found: gt-missing") {
		t.Fatalf("err = %v, want message not found", err)
	}
}

// Channel and announce reads keep only the channel's gt:message issues,
// newest first.
func TestChannelAndAnnounceMessages(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	old := createMessage(t, bd, "old", "gt:message", "channel:ops", "announce_channel:ops", "from:deacon/")
	createMessage(t, bd, "unlabeled", "channel:ops", "announce_channel:ops")
	createMessage(t, bd, "elsewhere", "gt:message", "channel:dev", "announce_channel:dev")
	newer := createMessage(t, bd, "new", "gt:message", "channel:ops", "announce_channel:ops")

	ch, err := listChannelMessages(bd, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 2 || ch[0].ID != newer || ch[1].ID != old || ch[1].From != "deacon/" {
		t.Fatalf("channel messages = %+v, want [%s %s] newest first", ch, newer, old)
	}

	ann, err := listAnnounceMessages(bd, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if len(ann) != 2 || ann[0].ID != newer || ann[1].ID != old || ann[1].From != "deacon/" {
		t.Fatalf("announce messages = %+v, want [%s %s] newest first", ann, newer, old)
	}
}
