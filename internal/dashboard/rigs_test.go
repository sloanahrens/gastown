package dashboard

import (
	"encoding/json"
	"testing"
)

func intp(n int) *int { return &n }

// The panel is a join: the queue names the rigs and carries their work, and the
// polecat seats say which of them are free.
func TestRigRowsJoinTheQueueAndTheSeats(t *testing.T) {
	t.Parallel()
	q := &Queue{Rigs: []Rig{
		{Name: "gastown", Ready: intp(4), Landing: intp(1)},
		{Name: "mango", Parked: true, Ready: intp(2), Landing: intp(3)},
	}}
	polecats := []Polecat{
		{Rig: "gastown", Name: "a", State: StateIdle},
		{Rig: "gastown", Name: "b", State: StateWorking},
		{Rig: "gastown", Name: "c", State: StateStalled},
		{Rig: "mango", Name: "d", State: StateIdle},
		{Rig: "mango", Name: "e", State: StateIdle},
		{Rig: "om", Name: "f", State: StateIdle}, // a rig the queue did not name gets no row
	}
	rows := RigRows(q, polecats)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want one per queue rig", rows)
	}
	g, m := rows[0], rows[1]
	if g.Name != "gastown" || g.Parked || *g.Seats != 1 || *g.Ready != 4 || *g.Landing != 1 {
		t.Errorf("gastown = %+v (only the idle polecat is an open seat)", g)
	}
	if m.Name != "mango" || !m.Parked || *m.Seats != 2 || *m.Ready != 2 || *m.Landing != 3 {
		t.Errorf("mango = %+v (a parked rig keeps its row and its numbers)", m)
	}
	if q.Rigs[0].Seats != nil {
		t.Error("the queue's own rows must not be written through")
	}
}

// A count the reader could not make is absent from the payload, so the page can
// tell it from a zero it did read.
func TestRigRowsCountsAreAbsentWhenUnreadableAndZeroWhenRead(t *testing.T) {
	t.Parallel()
	q := &Queue{Rigs: []Rig{
		{Name: "gastown", Ready: intp(0), Landing: intp(0)},
		{Name: "beads"}, // its store could not be read
	}}
	b, err := json.Marshal(RigRows(q, []Polecat{}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"gastown","seats":0,"ready":0,"landing":0},{"name":"beads","seats":0}]`
	if string(b) != want {
		t.Errorf("payload = %s\nwant       %s", b, want)
	}
}

// A failed seat read is not a town of no polecats: every seat count is unknown.
func TestRigRowsLeaveSeatsUnknownWhenTheSeatReadFailed(t *testing.T) {
	t.Parallel()
	q := &Queue{Rigs: []Rig{{Name: "gastown", Ready: intp(1), Landing: intp(0)}}}
	b, err := json.Marshal(RigRows(q, nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"name":"gastown","ready":1,"landing":0}]`; string(b) != want {
		t.Errorf("payload = %s, want %s", b, want)
	}
}

func TestRigRowsWithoutAQueueReading(t *testing.T) {
	t.Parallel()
	if rows := RigRows(nil, []Polecat{{Rig: "gastown", State: StateIdle}}); rows != nil {
		t.Errorf("rows = %+v, want none before the queue reports", rows)
	}
}
