package viewlog

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// at is the time every test's view is stamped with, far enough from the wall
// clock that a call reading time.Now instead is visible.
var at = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// validView is a complete, valid view of the shared item item-1 in workspace
// ws-1. Every test uses it whole except the field rows of
// TestRefusedViewAppendsNothing, which spell out their own View.
func validView() View {
	return View{Actor: "operator", Workspace: "ws-1", ItemID: "item-1", At: at}
}

// fakeShares is ShareChecker in memory: the workspace/item pairs it calls
// shared, an error it returns instead of answering, and how many times it was
// asked, so a test can tell a refused view from a checked one.
type fakeShares struct {
	shared map[string]bool
	err    error
	calls  int
}

func (s *fakeShares) Shared(_ context.Context, workspace, itemID string) (bool, error) {
	s.calls++
	if s.err != nil {
		return false, s.err
	}
	return s.shared[workspace+"/"+itemID], nil
}

// fakeLog is Logger in memory, failing instead of storing while err is set.
type fakeLog struct {
	entries []Entry
	err     error
}

func (l *fakeLog) Append(_ context.Context, e Entry) error {
	if l.err != nil {
		return l.err
	}
	l.entries = append(l.entries, e)
	return nil
}

func TestSharedViewAppendsOneEntry(t *testing.T) {
	t.Parallel()
	shares := &fakeShares{shared: map[string]bool{"ws-1/item-1": true}}
	log := NewMemory()
	v := validView()

	if err := ViewShared(context.Background(), shares, log, v); err != nil {
		t.Fatalf("ViewShared returned %v, want nil", err)
	}
	entries := log.Entries()
	if len(entries) != 1 {
		t.Fatalf("logged %d entries, want 1: %+v", len(entries), entries)
	}
	got := entries[0]
	if got.Actor != v.Actor || got.Workspace != v.Workspace || got.ItemID != v.ItemID || !got.At.Equal(v.At) {
		t.Errorf("entry = %+v, want actor %q, workspace %q, item %q at %v", got, v.Actor, v.Workspace, v.ItemID, v.At)
	}

	// The entry's shape is checked rather than assumed: a content, title or body
	// field added later fails here, because no such field may carry the item.
	var fields []string
	typ := reflect.TypeOf(Entry{})
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Name)
	}
	if want := []string{"Actor", "Workspace", "ItemID", "At"}; !reflect.DeepEqual(fields, want) {
		t.Errorf("Entry fields = %v, want %v", fields, want)
	}
}

func TestUnsharedViewRefusedAndNotLogged(t *testing.T) {
	t.Parallel()
	shares := &fakeShares{shared: map[string]bool{}}
	log := NewMemory()

	err := ViewShared(context.Background(), shares, log, validView())
	if !errors.Is(err, ErrNotShared) {
		t.Fatalf("ViewShared returned %v, want ErrNotShared", err)
	}
	if entries := log.Entries(); len(entries) != 0 {
		t.Errorf("logged %+v, want no entry", entries)
	}
}

func TestAppendFailureReleasesNothing(t *testing.T) {
	t.Parallel()
	appendErr := errors.New("view log unavailable")
	shares := &fakeShares{shared: map[string]bool{"ws-1/item-1": true}}
	log := &fakeLog{err: appendErr}

	err := ViewShared(context.Background(), shares, log, validView())
	if !errors.Is(err, appendErr) {
		t.Fatalf("ViewShared returned %v, want the append error", err)
	}
	if len(log.entries) != 0 {
		t.Errorf("logged %+v, want no entry", log.entries)
	}
}

func TestRefusedViewAppendsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		shares ShareChecker
		log    Logger
		view   View
		want   error
	}{
		{"no share checker", nil, NewMemory(), validView(), ErrNoChecker},
		{"no logger", &fakeShares{}, nil, validView(), ErrNoLog},
		{"typed nil share checker", (*fakeShares)(nil), NewMemory(), validView(), ErrNoChecker},
		{"typed nil logger", &fakeShares{}, (*Memory)(nil), validView(), ErrNoLog},
		{"no actor", &fakeShares{}, NewMemory(), View{Workspace: "ws-1", ItemID: "item-1", At: at}, ErrNoActor},
		{"no workspace", &fakeShares{}, NewMemory(), View{Actor: "operator", ItemID: "item-1", At: at}, ErrEmptyWorkspace},
		{"no item", &fakeShares{}, NewMemory(), View{Actor: "operator", Workspace: "ws-1", At: at}, ErrEmptyItem},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ViewShared(context.Background(), tc.shares, tc.log, tc.view); !errors.Is(err, tc.want) {
				t.Fatalf("ViewShared returned %v, want %v", err, tc.want)
			}
			if shares, ok := tc.shares.(*fakeShares); ok && shares != nil && shares.calls != 0 {
				t.Errorf("share checker consulted %d times, want 0", shares.calls)
			}
			if log, ok := tc.log.(*Memory); ok && log != nil && len(log.Entries()) != 0 {
				t.Errorf("logged %+v, want no entry", log.Entries())
			}
		})
	}
}

func TestCheckerErrorAppendsNothing(t *testing.T) {
	t.Parallel()
	checkErr := errors.New("sharing store unavailable")
	shares := &fakeShares{err: checkErr}
	log := NewMemory()

	err := ViewShared(context.Background(), shares, log, validView())
	if !errors.Is(err, checkErr) {
		t.Fatalf("ViewShared returned %v, want the checker error", err)
	}
	if errors.Is(err, ErrNotShared) {
		t.Errorf("ViewShared returned %v, which reads as a refusal, want an unknown answer", err)
	}
	if entries := log.Entries(); len(entries) != 0 {
		t.Errorf("logged %+v, want no entry", entries)
	}
}
