package beads

import (
	"context"
	"errors"

	beadsdk "github.com/steveyegge/beads"
)

// ErrStoreReadOnly is what every write through an in-process store returns.
// gastown reads beads through these stores and writes through bd; a write that
// reaches the store is a bug in gastown, not a user error, and it is refused
// loudly rather than committed with the library's pre-fork semantics
// (gt-fcxe9.11, deep review B2-26/B5-22).
var ErrStoreReadOnly = errors.New("in-process beads store is read-only: write to beads through bd")

// readOnlyStore is the beadsdk.Storage OpenStoreFromConfig hands back: reads
// pass through to the opened store, every mutation is refused with
// ErrStoreReadOnly.
//
// A facade rather than a read-only open because the pinned library offers no
// such open — its two exported constructors build a writable dolt.Config, and
// the Config that carries ReadOnly is in the library's internal/ tree
// (gt-fcxe9.11). A write that reaches a store commits with the library's
// pre-fork semantics against a fork database, so gastown has none.
//
// MergeSlotCheck and MergeSlotAcquire pass through unrefused: they are the two
// writes whose result types the library does not re-export, so a wrapper
// outside the library cannot declare them. gastown calls neither on a store —
// its merge slot is a bead read and written through Client
// (beads_merge_slot.go) — and readOnlyStore's test pins that.
type readOnlyStore struct {
	beadsdk.Storage
}

func (s readOnlyStore) CreateIssue(context.Context, *beadsdk.Issue, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) CreateIssues(context.Context, []*beadsdk.Issue, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) UpdateIssue(context.Context, string, map[string]interface{}, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) ReopenIssue(context.Context, string, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) UpdateIssueType(context.Context, string, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) CloseIssue(context.Context, string, string, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) DeleteIssue(context.Context, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) AddDependency(context.Context, *beadsdk.Dependency, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) RemoveDependency(context.Context, string, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) AddLabel(context.Context, string, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) RemoveLabel(context.Context, string, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) AddIssueComment(context.Context, string, string, string) (*beadsdk.Comment, error) {
	return nil, ErrStoreReadOnly
}

func (s readOnlyStore) SetConfig(context.Context, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) SetLocalMetadata(context.Context, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) RunInTransaction(context.Context, string, func(tx beadsdk.Transaction) error) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) MergeSlotCreate(context.Context, string) (*beadsdk.Issue, error) {
	return nil, ErrStoreReadOnly
}

func (s readOnlyStore) MergeSlotRelease(context.Context, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) SlotSet(context.Context, string, string, string, string) error {
	return ErrStoreReadOnly
}

func (s readOnlyStore) SlotClear(context.Context, string, string, string) error {
	return ErrStoreReadOnly
}
