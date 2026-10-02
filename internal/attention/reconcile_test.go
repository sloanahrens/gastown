package attention

import (
	"testing"
	"time"
)

var reconcileNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// item is a collector observation with the fields a collector fills in.
func item(key string, kind Kind, summary string) Item {
	return Item{Key: key, Kind: kind, Severity: SeverityHigh, Summary: summary}
}

func TestReconcile(t *testing.T) {
	t.Parallel()
	older := reconcileNow.Add(-30 * time.Minute)

	tests := []struct {
		name         string
		prev         State
		observed     []Item
		acks         Acks
		wantItems    int
		wantNew      int
		wantCleared  int
		wantFirstSee time.Time
		wantLastSee  time.Time
		wantAcked    bool
		wantAcks     int
	}{
		{
			name:         "a new item gets first_seen=now and one new transition",
			observed:     []Item{item("esc:hq-1", KindEscalation, "escalation open")},
			wantItems:    1,
			wantNew:      1,
			wantFirstSee: reconcileNow,
			wantLastSee:  reconcileNow,
		},
		{
			name: "a re-observed item keeps first_seen and updates last_seen",
			prev: State{Updated: older, Items: []Item{
				{Key: "esc:hq-1", Kind: KindEscalation, Severity: SeverityHigh, Summary: "escalation open", FirstSeen: older, LastSeen: older},
			}},
			observed:     []Item{item("esc:hq-1", KindEscalation, "escalation open")},
			wantItems:    1,
			wantFirstSee: older,
			wantLastSee:  reconcileNow,
		},
		{
			name: "an item that is not observed is dropped with one cleared transition",
			prev: State{Updated: older, Items: []Item{
				{Key: "esc:hq-1", Kind: KindEscalation, Severity: SeverityHigh, Summary: "escalation open", FirstSeen: older, LastSeen: older},
			}},
			wantItems:   0,
			wantCleared: 1,
		},
		{
			name:         "an acked item stays in state but is marked acked",
			observed:     []Item{item("stall:gastown/opal", KindPolecatStall, "opal silent 12m")},
			acks:         Acks{Acks: []Ack{{Key: "stall:gastown/opal", At: reconcileNow}}},
			wantItems:    1,
			wantNew:      1,
			wantAcked:    true,
			wantFirstSee: reconcileNow,
			wantLastSee:  reconcileNow,
			wantAcks:     1,
		},
		{
			name: "an ack whose item cleared is dropped",
			prev: State{Updated: older, Items: []Item{
				{Key: "esc:hq-1", Kind: KindEscalation, Severity: SeverityHigh, Summary: "escalation open", FirstSeen: older, LastSeen: older},
			}},
			acks:        Acks{Acks: []Ack{{Key: "esc:hq-1", At: older}}},
			wantItems:   0,
			wantCleared: 1,
			wantAcks:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Reconcile(tt.prev, tt.observed, tt.acks, reconcileNow)
			if len(got.State.Items) != tt.wantItems {
				t.Fatalf("items = %d, want %d", len(got.State.Items), tt.wantItems)
			}
			if !got.State.Updated.Equal(reconcileNow) {
				t.Errorf("updated = %v, want %v", got.State.Updated, reconcileNow)
			}
			var newN, clearedN int
			for _, e := range got.Events {
				switch e.State {
				case EventNew:
					newN++
				case EventCleared:
					clearedN++
				default:
					t.Errorf("event state = %q", e.State)
				}
			}
			if newN != tt.wantNew {
				t.Errorf("new transitions = %d, want %d", newN, tt.wantNew)
			}
			if clearedN != tt.wantCleared {
				t.Errorf("cleared transitions = %d, want %d", clearedN, tt.wantCleared)
			}
			if tt.wantItems > 0 {
				got0 := got.State.Items[0]
				if !tt.wantFirstSee.IsZero() && !got0.FirstSeen.Equal(tt.wantFirstSee) {
					t.Errorf("first_seen = %v, want %v", got0.FirstSeen, tt.wantFirstSee)
				}
				if !tt.wantLastSee.IsZero() && !got0.LastSeen.Equal(tt.wantLastSee) {
					t.Errorf("last_seen = %v, want %v", got0.LastSeen, tt.wantLastSee)
				}
				if (got0.AckedAt != nil) != tt.wantAcked {
					t.Errorf("acked = %v, want %v", got0.AckedAt != nil, tt.wantAcked)
				}
			}
			if len(got.Acks.Acks) != tt.wantAcks {
				t.Errorf("acks kept = %d, want %d", len(got.Acks.Acks), tt.wantAcks)
			}
		})
	}
}

func TestReconcileSameSetTwiceHasNoTransitions(t *testing.T) {
	t.Parallel()
	observed := []Item{
		item("red-main:gastown", KindRedMain, "main red"),
		item("esc:hq-1", KindEscalation, "escalation open"),
	}
	first := Reconcile(State{}, observed, Acks{}, reconcileNow)
	if len(first.Events) != 2 {
		t.Fatalf("first reconcile events = %d, want 2", len(first.Events))
	}
	second := Reconcile(first.State, observed, first.Acks, reconcileNow.Add(time.Minute))
	if len(second.Events) != 0 {
		t.Errorf("second reconcile events = %d, want 0: %+v", len(second.Events), second.Events)
	}
	if len(second.State.Items) != 2 {
		t.Errorf("second reconcile items = %d, want 2", len(second.State.Items))
	}
}

func TestReconcileNewTransitionCarriesLineSchema(t *testing.T) {
	t.Parallel()
	got := Reconcile(State{}, []Item{item("esc:hq-1", KindEscalation, "escalation open")}, Acks{}, reconcileNow)
	if len(got.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(got.Events))
	}
	e := got.Events[0]
	if e.Class != KindEscalation || e.Severity != SeverityHigh || e.Text != "escalation open" || e.Key != "esc:hq-1" || e.State != EventNew {
		t.Errorf("event = %+v", e)
	}
	if !e.TS.Equal(reconcileNow) {
		t.Errorf("ts = %v, want %v", e.TS, reconcileNow)
	}
}

func TestKindsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[Kind]bool{}
	for _, k := range Kinds() {
		if k == "" {
			t.Error("empty kind")
		}
		if seen[k] {
			t.Errorf("duplicate kind %q", k)
		}
		seen[k] = true
	}
}
