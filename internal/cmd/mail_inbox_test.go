package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/mail"
)

type fakeInboxLister struct {
	calls    int
	messages []*mail.Message
	err      error
}

func (f *fakeInboxLister) List() ([]*mail.Message, error) {
	f.calls++
	return f.messages, f.err
}

func TestLoadInboxSnapshotListsOnceAndCounts(t *testing.T) {
	t.Parallel()
	box := &fakeInboxLister{
		messages: []*mail.Message{
			{ID: "msg-1", Read: false},
			{ID: "msg-2", Read: true},
			{ID: "msg-3", Read: false},
		},
	}

	messages, total, unread, err := loadInboxSnapshot(box, false)
	if err != nil {
		t.Fatalf("loadInboxSnapshot returned error: %v", err)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
	if total != 3 || unread != 2 {
		t.Fatalf("counts = (%d total, %d unread), want (3, 2)", total, unread)
	}
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
}

func TestLoadInboxSnapshotUnreadOnlyFiltersAfterSingleList(t *testing.T) {
	t.Parallel()
	box := &fakeInboxLister{
		messages: []*mail.Message{
			{ID: "msg-1", Read: false},
			{ID: "msg-2", Read: true},
			{ID: "msg-3", Read: false},
		},
	}

	messages, total, unread, err := loadInboxSnapshot(box, true)
	if err != nil {
		t.Fatalf("loadInboxSnapshot returned error: %v", err)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
	if total != 3 || unread != 2 {
		t.Fatalf("counts = (%d total, %d unread), want (3, 2)", total, unread)
	}
	if len(messages) != 2 {
		t.Fatalf("filtered messages len = %d, want 2", len(messages))
	}
	if messages[0].ID != "msg-1" || messages[1].ID != "msg-3" {
		t.Fatalf("filtered messages = [%s %s], want [msg-1 msg-3]", messages[0].ID, messages[1].ID)
	}
}

func TestLoadInboxSnapshotPropagatesListError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("list failed")
	box := &fakeInboxLister{err: wantErr}

	_, _, _, err := loadInboxSnapshot(box, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
}

func TestMailReadIndexMatchesUnreadListing(t *testing.T) {
	t.Parallel()
	box := &fakeInboxLister{
		messages: []*mail.Message{
			{ID: "m1", Read: true},
			{ID: "m2", Read: false},
		},
	}

	listed, _, _, err := loadInboxSnapshot(box, true)
	if err != nil {
		t.Fatalf("loadInboxSnapshot returned error: %v", err)
	}
	if len(listed) == 0 || listed[0].ID != "m2" {
		t.Fatalf("inbox --unread row 1 = %v, want m2", listed)
	}

	got, err := resolveInboxIndex(box, 1, true)
	if err != nil {
		t.Fatalf("resolveInboxIndex(unread) returned error: %v", err)
	}
	if got != "m2" {
		t.Fatalf("read --unread 1 selected %q, want m2", got)
	}

	got, err = resolveInboxIndex(box, 1, false)
	if err != nil {
		t.Fatalf("resolveInboxIndex(all) returned error: %v", err)
	}
	if got != "m1" {
		t.Fatalf("read 1 selected %q, want m1 (full mailbox)", got)
	}

	if _, err := resolveInboxIndex(box, 2, true); err == nil {
		t.Fatal("index 2 with one unread message: want out-of-range error")
	}
}
