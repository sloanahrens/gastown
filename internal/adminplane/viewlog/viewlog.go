// Package viewlog is the admin plane's record of an operator's view of a
// customer-shared item (plan row V3): ViewShared asks the ShareChecker whether
// the workspace shared the item, and, when it did, appends one Entry naming the
// actor, the workspace, the item and the time before the caller releases the
// item.
//
// Nothing calls it yet. Its caller is the console handler for shared items (a
// later bead), which supplies a ShareChecker over the admin plane's sharing
// records and a Logger over the view log table; Memory is the in-memory Logger
// those callers and these tests use. The package holds no database and no HTTP.
// The Entry holds ids and a time, never the item's content.
package viewlog

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// View is one operator's view of an item: who looked, in which workspace, at
// which item, and when. Actor is a role such as "operator" or a staff id, never
// a person's name.
type View struct {
	Actor     string
	Workspace string
	ItemID    string
	At        time.Time
}

// Entry is what ViewShared appends for a view: the actor, workspace, item and
// time, with no field that could hold the item's content.
type Entry struct {
	Actor     string
	Workspace string
	ItemID    string
	At        time.Time
}

// ShareChecker answers whether a workspace shared one of its items with the
// operator console. An error means the answer is unknown, so the view is
// neither recorded nor released.
type ShareChecker interface {
	// Shared reports whether workspace shared itemID with the operator console.
	Shared(ctx context.Context, workspace, itemID string) (bool, error)
}

// Logger is where a view's entry is appended. The admin plane's implementation
// is its view log table (a later bead); Memory is the in-memory fake.
type Logger interface {
	// Append stores one entry. An error means it was not stored, so the view it
	// records was not released.
	Append(ctx context.Context, e Entry) error
}

var (
	// ErrNoChecker refuses a call with no ShareChecker: without one the package
	// cannot tell a shared item from an unshared one, and must not guess.
	ErrNoChecker = errors.New("viewlog: a share checker is required")
	// ErrNoLog refuses a call with no Logger: nothing could record the view.
	ErrNoLog = errors.New("viewlog: a logger is required")
	// ErrNoActor refuses a view with no actor: an entry naming nobody records
	// nothing.
	ErrNoActor = errors.New("viewlog: an actor is required")
	// ErrEmptyWorkspace refuses a view with no workspace, which names nothing.
	ErrEmptyWorkspace = errors.New("viewlog: a workspace is required")
	// ErrEmptyItem refuses a view with no item, which names nothing.
	ErrEmptyItem = errors.New("viewlog: an item is required")
	// ErrNotShared refuses a view of an item the workspace did not share with the
	// operator console, which may not be shown and must not be recorded.
	ErrNotShared = errors.New("viewlog: item is not shared with the operator console")
)

// ViewShared records one operator view of an item the workspace shared with the
// operator console. It appends exactly one Entry, and only after the
// ShareChecker confirms the item is shared; an append failure is returned and
// stores nothing, so a caller that appends before releasing the item never
// shows one unrecorded.
//
// An item that is not shared is refused with ErrNotShared and appends nothing.
// A ShareChecker error is returned wrapped: an unknown answer is not a refusal.
// A nil ShareChecker or Logger, including a nil pointer held in the interface,
// is refused with ErrNoChecker or ErrNoLog.
func ViewShared(ctx context.Context, shares ShareChecker, log Logger, v View) error {
	switch {
	case isNil(shares):
		return ErrNoChecker
	case isNil(log):
		return ErrNoLog
	case v.Actor == "":
		return ErrNoActor
	case v.Workspace == "":
		return ErrEmptyWorkspace
	case v.ItemID == "":
		return ErrEmptyItem
	}
	shared, err := shares.Shared(ctx, v.Workspace, v.ItemID)
	if err != nil {
		return fmt.Errorf("viewlog: checking whether item %q is shared: %w", v.ItemID, err)
	}
	if !shared {
		return fmt.Errorf("%w: item %q of workspace %q", ErrNotShared, v.ItemID, v.Workspace)
	}
	if err := log.Append(ctx, Entry{Actor: v.Actor, Workspace: v.Workspace, ItemID: v.ItemID, At: v.At}); err != nil {
		return fmt.Errorf("viewlog: appending the view of item %q: %w", v.ItemID, err)
	}
	return nil
}

// isNil reports whether v is nil, including an interface that holds a nil
// pointer, map, slice, func or channel. Such an interface compares unequal to
// nil, so a plain nil check would let a call reach a method that dereferences it.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

// Memory is an in-memory Logger for tests and for callers before the view log
// table exists. Its zero value is ready to use. It is not safe for concurrent
// use.
type Memory struct {
	entries []Entry
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory { return &Memory{} }

// Append stores e.
func (m *Memory) Append(_ context.Context, e Entry) error {
	m.entries = append(m.entries, e)
	return nil
}

// Entries returns a copy of the entries the log holds, so a caller can read
// what was recorded without holding the log's own slice.
func (m *Memory) Entries() []Entry {
	out := make([]Entry, len(m.entries))
	copy(out, m.entries)
	return out
}
