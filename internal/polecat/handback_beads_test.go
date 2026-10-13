package polecat

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// recordingStore records the bd writes the adapter makes.
type recordingStore struct {
	updates  []beads.UpdateOptions
	ids      []string
	comments []string
	err      error
}

func (r *recordingStore) Update(id string, opts beads.UpdateOptions) error {
	r.ids = append(r.ids, id)
	r.updates = append(r.updates, opts)
	return r.err
}

func (r *recordingStore) AddCommentAs(id, author, comment string) error {
	r.comments = append(r.comments, id+"|"+author+"|"+comment)
	return r.err
}

func TestBeadWriter_HandBackWritesTheReleaseCalls(t *testing.T) {
	t.Parallel()
	store := &recordingStore{}
	w := NewBeadWriter(store, "overrun_guard")

	if err := HandBack(context.Background(), w, "gt-abc", "polecat/nux/gt-abc+x1", 21*time.Minute); err != nil {
		t.Fatalf("HandBack: %v", err)
	}

	if len(store.updates) != 3 {
		t.Fatalf("got %d bd updates, want 3 (status, assignee, label)", len(store.updates))
	}
	if got := store.updates[0].Status; got == nil || *got != "open" {
		t.Errorf("first update status = %v, want open", got)
	}
	if got := store.updates[1].Assignee; got == nil || *got != "" {
		t.Errorf("second update assignee = %v, want a pointer to the empty string", got)
	}
	if got := store.updates[2].AddLabels; !reflect.DeepEqual(got, []string{NeedsSplitLabel}) {
		t.Errorf("third update labels = %v, want [%s]", got, NeedsSplitLabel)
	}
	for i, u := range store.updates {
		if store.ids[i] != "gt-abc" {
			t.Errorf("update %d wrote %s, want gt-abc", i, store.ids[i])
		}
		if u.Force {
			t.Errorf("update %d forced past bd's fences", i)
		}
	}
	if len(store.comments) != 1 || store.comments[0][:len("gt-abc|overrun_guard|")] != "gt-abc|overrun_guard|" {
		t.Errorf("comments = %q, want one on gt-abc by overrun_guard", store.comments)
	}
}

func TestBeadWriter_ErrorsAndCancelledContext(t *testing.T) {
	t.Parallel()
	boom := errors.New("bd down")
	store := &recordingStore{err: boom}
	w := NewBeadWriter(store, "overrun_guard")
	if err := w.SetStatus(context.Background(), "gt-abc", "open"); !errors.Is(err, boom) {
		t.Errorf("SetStatus error = %v, want the store's", err)
	}
	if err := w.Comment(context.Background(), "gt-abc", "x"); !errors.Is(err, boom) {
		t.Errorf("Comment error = %v, want the store's", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fresh := &recordingStore{}
	w = NewBeadWriter(fresh, "overrun_guard")
	if err := w.AddLabel(ctx, "gt-abc", NeedsSplitLabel); !errors.Is(err, context.Canceled) {
		t.Errorf("AddLabel on a cancelled context = %v, want context.Canceled", err)
	}
	if len(fresh.updates) != 0 {
		t.Errorf("a cancelled context still wrote: %v", fresh.updates)
	}
}
