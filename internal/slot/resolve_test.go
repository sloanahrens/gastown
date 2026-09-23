package slot

import (
	"reflect"
	"testing"
)

func heldSlot(index int, owner *Owner) SlotState {
	return SlotState{Index: index, Held: true, Owner: owner}
}

// TestResolveHolds is the classification half of gt-2tqe: an entry whose hold
// was never closed is named against the pool, and every other entry keeps to
// what its own fields already say.
func TestResolveHolds(t *testing.T) {
	open := HistoryEntry{TS: "2026-09-22T15:16:25Z", Role: "gastown/refinery-batch", Slot: 0, PID: 4242, WaitedS: 0}
	heldFor := 5.0
	released := HistoryEntry{Role: "gastown/amber", Slot: 0, PID: 7, HeldS: &heldFor}
	gaveUp := HistoryEntry{Role: "gastown/opal", Slot: 0, PID: 8, TimedOut: true}

	tests := []struct {
		name  string
		entry HistoryEntry
		rep   Report
		want  HoldResolution
		taken *Owner
	}{
		{
			name:  "an open hold the pool still counts is live",
			entry: open,
			rep:   Report{Total: 1, HeldCount: 1, Slots: []SlotState{heldSlot(0, &Owner{Role: "gastown/refinery-batch", PID: 4242})}},
			want:  HoldLive,
		},
		{
			name:  "an open hold on a free slot is abandoned",
			entry: open,
			rep:   Report{Total: 1, Slots: []SlotState{{Index: 0}}},
			want:  HoldAbandoned,
		},
		{
			name:  "an open hold the pool no longer knows the slot for is abandoned",
			entry: HistoryEntry{Role: "gastown/refinery-batch", Slot: 3, PID: 4242},
			rep:   Report{Total: 1, Slots: []SlotState{{Index: 0}}},
			want:  HoldAbandoned,
		},
		{
			name:  "an open hold whose slot somebody else took is abandoned, naming them",
			entry: open,
			rep:   Report{Total: 1, HeldCount: 1, Slots: []SlotState{heldSlot(0, &Owner{Role: "gastown/refinery", PID: 99})}},
			want:  HoldAbandoned,
			taken: &Owner{Role: "gastown/refinery", PID: 99},
		},
		{
			name:  "an open hold against a holder the pool cannot name is unmatched",
			entry: open,
			rep:   Report{Total: 1, HeldCount: 1, Slots: []SlotState{heldSlot(0, nil)}},
			want:  HoldUnmatched,
		},
		{
			name:  "a released hold needs no verdict",
			entry: released,
			rep:   Report{Total: 1, Slots: []SlotState{{Index: 0}}},
		},
		{
			name:  "a caller that gave up needs no verdict",
			entry: gaveUp,
			rep:   Report{Total: 1, Slots: []SlotState{{Index: 0}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveHolds([]HistoryEntry{tt.entry}, tt.rep)
			if len(got) != 1 {
				t.Fatalf("ResolveHolds returned %d holds, want 1", len(got))
			}
			if got[0].Resolution != tt.want {
				t.Errorf("resolution = %q, want %q", got[0].Resolution, tt.want)
			}
			switch {
			case tt.taken == nil && got[0].ReclaimedBy != nil:
				t.Errorf("reclaimed_by = %+v, want none", got[0].ReclaimedBy)
			case tt.taken != nil && got[0].ReclaimedBy == nil:
				t.Errorf("reclaimed_by is nil, want %+v", tt.taken)
			case tt.taken != nil && *got[0].ReclaimedBy != *tt.taken:
				t.Errorf("reclaimed_by = %+v, want %+v", got[0].ReclaimedBy, tt.taken)
			}
			if !reflect.DeepEqual(got[0].HistoryEntry, tt.entry) {
				t.Errorf("the entry lost facts in resolution: %+v, want %+v", got[0].HistoryEntry, tt.entry)
			}
		})
	}
}

// TestResolveHolds_LeavesAnEmptyHistoryEmpty keeps a town that has never run a
// container-backed suite from reporting a history of nothing.
func TestResolveHolds_LeavesAnEmptyHistoryEmpty(t *testing.T) {
	if got := ResolveHolds(nil, Report{Total: 1}); len(got) != 0 {
		t.Errorf("ResolveHolds(nil) = %+v, want nothing", got)
	}
}

// TestResolveHolds_IgnoresMarkerRows: a marker's Index continues the pool's
// numbering rather than naming a real slot, and can coincide with a history
// entry's slot from a pool that used to be larger. That must read as an
// ordinary abandoned hold, never as "reclaimed by" the marker's om-review
// holder (gt-97cm finding 5).
func TestResolveHolds_IgnoresMarkerRows(t *testing.T) {
	entry := HistoryEntry{Role: "gastown/refinery-batch", Slot: 1, PID: 4242}
	rep := Report{
		Total: 1,
		Slots: []SlotState{
			{Index: 0},
			{
				Index:  1,
				Held:   true,
				Marker: true,
				Name:   "om-review-gt-mr-1",
				Owner:  &Owner{Role: "gastown/om-review", PID: 55},
			},
		},
	}

	got := ResolveHolds([]HistoryEntry{entry}, rep)
	if len(got) != 1 {
		t.Fatalf("ResolveHolds returned %d holds, want 1", len(got))
	}
	if got[0].Resolution != HoldAbandoned {
		t.Errorf("resolution = %q, want %q", got[0].Resolution, HoldAbandoned)
	}
	if got[0].ReclaimedBy != nil {
		t.Errorf("reclaimed_by = %+v, want none — the marker at the same index is not this entry's slot", got[0].ReclaimedBy)
	}
}
