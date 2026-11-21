package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// seat.finished_lingering (gt-klpqb3): a seat whose last turn ended and whose
// bead is closed, still in the walk past the linger bound, is a finished seat
// nobody ended.

// finishedSeat is one seat the walk lists: its bare name, the work bead its
// record names, that bead's status, the agent state, and how long ago the
// agent bead was last written (the done time the rule ages from).
type finishedSeat struct {
	name   string
	bead   string
	status string
	state  string
	age    time.Duration
}

// finishedSeatTown points the walk at seats and answers their agent and work
// bead reads from memory, dated against keptSeatAttentionNow. It returns the
// reader so a test can break one read and keep the rest.
func finishedSeatTown(t *testing.T, f *attentionFixture, seats ...finishedSeat) keptSeatHookReader {
	t.Helper()
	r := keptSeatHookReader{issues: map[string]*beads.Issue{}}
	var walk []townhealth.Seat
	for _, s := range seats {
		id := keptSeatAgentID(f, s.name)
		r.issues[id] = &beads.Issue{
			ID:          id,
			UpdatedAt:   keptSeatAttentionNow.Add(-s.age).Format(time.RFC3339),
			Description: "agent_state: " + s.state + "\nlast_source_issue: " + s.bead,
		}
		if s.bead != "" {
			r.issues[s.bead] = &beads.Issue{ID: s.bead, Status: s.status}
		}
		walk = append(walk, townhealth.Seat{Rig: attentionRig, Name: "polecat/" + s.name, Run: true})
	}
	f.d.openWorkBeads = r.open
	f.src.seats = func() ([]townhealth.Seat, error) { return walk, nil }
	return r
}

// A done seat whose bead is closed raises one item once it has lingered past
// thirty minutes, naming the seat, the bead and the age; the same seat at
// twenty-nine minutes raises none.
func TestSeatFinishedLingeringRaisesAfterTheWindow(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	finishedSeatTown(t, f,
		finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
		finishedSeat{name: "nux", bead: "gt-abc13", status: string(beads.StatusClosed), state: "done", age: 29 * time.Minute},
	)
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one (the 29m seat has not lingered past the bound)", items)
	}
	it := items[0]
	if it.Key != "seat-finished:gastown/capable" || it.Kind != attention.KindSeatFinishedLingering ||
		it.Severity != attention.SeverityLow || it.Rig != attentionRig || it.Bead != "gt-abc12" {
		t.Errorf("item = %+v, want a low seat-finished item on gastown/capable and gt-abc12", it)
	}
	if want := "gastown/capable done 31m on gt-abc12"; it.Summary != want {
		t.Errorf("summary = %q, want %q", it.Summary, want)
	}
}

// A seat that raised the item and has left the walk raises nothing next tick:
// the item is keyed by the seat, and a key the walk no longer shows is one the
// queue clears. The previous tick's item is carried in prev, as the daemon
// carries it across heartbeats.
func TestSeatFinishedLingeringClearsWhenTheSeatLeavesTheWalk(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	finishedSeatTown(t, f,
		finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 31 * time.Minute},
	)
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	first, err := c.collect(context.Background())
	if err != nil || len(first) != 1 {
		t.Fatalf("first tick = %+v, %v; want the one item for the walking seat", first, err)
	}

	f.src.seats = func() ([]townhealth.Seat, error) { return nil, nil }
	got := collectAttention(context.Background(), attention.State{Items: first},
		[]attentionCollector{c}, func(string, ...any) {})
	if len(got) != 0 {
		t.Errorf("items after the seat left the walk = %+v, want none", got)
	}
}

// A done seat whose bead is still open is rework with its branch preserved,
// not a seat left behind, so it raises nothing however long it has lingered.
func TestSeatFinishedLingeringIgnoresOpenWork(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	finishedSeatTown(t, f,
		finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusOpen), state: "done", age: 3 * time.Hour},
	)
	c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none for a done seat whose bead is open", items)
	}
}

// A seat the rule cannot judge raises nothing: one that is not a polecat, one
// whose record is not done, and one whose record names no bead.
func TestSeatFinishedLingeringSkipsSeatsItCannotJudge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, f *attentionFixture)
	}{
		{
			name: "the seat is not a polecat",
			setup: func(t *testing.T, f *attentionFixture) {
				f.d.openWorkBeads = func([]string, time.Duration) workBeadReader {
					t.Error("a non-polecat seat was read")
					return keptSeatHookReader{issues: map[string]*beads.Issue{}}
				}
				f.src.seats = func() ([]townhealth.Seat, error) {
					return []townhealth.Seat{{Rig: attentionRig, Name: "witness/capable", Run: true}}, nil
				}
			},
		},
		{
			name: "the record is not done",
			setup: func(t *testing.T, f *attentionFixture) {
				finishedSeatTown(t, f, finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "working", age: 3 * time.Hour})
			},
		},
		{
			name: "the record names no bead",
			setup: func(t *testing.T, f *attentionFixture) {
				finishedSeatTown(t, f, finishedSeat{name: "capable", state: "done", age: 3 * time.Hour})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			tc.setup(t, f)
			c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

			items, err := c.collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 0 {
				t.Fatalf("items = %+v, want none for a seat the rule cannot judge", items)
			}
		})
	}
}

// The agent bead is a read the rule cannot guess at: one that reads as no bead
// fails the collection with no item, and the caller keeps the previous tick's
// seat-finished items rather than clearing them. A failed read is covered in
// TestSeatFinishedLingeringFailedReadsAreErrors.
func TestSeatFinishedLingeringUnreadableAgentBeadKeepsPreviousItems(t *testing.T) {
	t.Parallel()
	seat := finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 3 * time.Hour}
	cases := []struct {
		name string
		read func(t *testing.T, f *attentionFixture)
	}{
		{
			name: "the agent bead reads as no bead",
			read: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.noBead = map[string]bool{keptSeatAgentID(f, seat.name): true}
				f.d.openWorkBeads = r.open
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			tc.read(t, f)
			c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)
			prev := attention.State{Items: []attention.Item{
				{Key: "seat-finished:gastown/capable", Kind: attention.KindSeatFinishedLingering, Severity: attention.SeverityLow, Rig: attentionRig, Bead: seat.bead},
				{Key: "stall:gastown/opal", Kind: attention.KindPolecatStall, Severity: attention.SeverityHigh, Summary: "another collector's item"},
			}}

			items, err := c.collect(context.Background())
			if err == nil {
				t.Fatalf("collect = %+v, nil; want the read's error", items)
			}
			if len(items) != 0 {
				t.Errorf("items = %+v, want none beside the error", items)
			}

			got := collectAttention(context.Background(), prev, []attentionCollector{c}, func(string, ...any) {})
			if len(got) != 1 || got[0].Key != "seat-finished:gastown/capable" {
				t.Errorf("collectAttention = %+v, want only the previous seat-finished item kept", got)
			}
		})
	}
}

// Every read the rule makes is the error when it fails, never a guess at
// "finished" or "not finished": a walk, record or bead that cannot be read
// raises nothing and fails the collection, so the queue keeps the items it had.
func TestSeatFinishedLingeringFailedReadsAreErrors(t *testing.T) {
	t.Parallel()
	readFails := errors.New("bedrock: bd unavailable")
	seat := finishedSeat{name: "capable", bead: "gt-abc12", status: string(beads.StatusClosed), state: "done", age: 3 * time.Hour}
	cases := []struct {
		name  string
		setup func(t *testing.T, f *attentionFixture)
	}{
		{
			name: "the seat walk fails",
			setup: func(t *testing.T, f *attentionFixture) {
				f.src.seats = func() ([]townhealth.Seat, error) { return nil, readFails }
			},
		},
		{
			name: "the agent bead read fails",
			setup: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.fail = map[string]error{keptSeatAgentID(f, seat.name): readFails}
				f.d.openWorkBeads = r.open
			},
		},
		{
			name: "the work bead read fails",
			setup: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.fail = map[string]error{seat.bead: readFails}
				f.d.openWorkBeads = r.open
			},
		},
		{
			name: "the work bead reads as no bead",
			setup: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.noBead = map[string]bool{seat.bead: true}
				f.d.openWorkBeads = r.open
			},
		},
		{
			name: "the record carries no update time",
			setup: func(t *testing.T, f *attentionFixture) {
				r := finishedSeatTown(t, f, seat)
				r.issues[keptSeatAgentID(f, seat.name)].UpdatedAt = ""
				f.d.openWorkBeads = r.open
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			tc.setup(t, f)
			c := collectorFor(t, f.src, attention.KindSeatFinishedLingering)

			items, err := c.collect(context.Background())
			if err == nil {
				t.Fatalf("collect = %+v, nil; want the read's error", items)
			}
			if len(items) != 0 {
				t.Errorf("items = %+v, want none beside the error", items)
			}

			prev := attention.State{Items: []attention.Item{
				{Key: "seat-finished:gastown/capable", Kind: attention.KindSeatFinishedLingering, Severity: attention.SeverityLow, Rig: attentionRig, Bead: seat.bead},
			}}
			got := collectAttention(context.Background(), prev, []attentionCollector{c}, func(string, ...any) {})
			if len(got) != 1 || got[0].Key != "seat-finished:gastown/capable" {
				t.Errorf("collectAttention = %+v, want the previous seat-finished item kept", got)
			}
		})
	}
}
