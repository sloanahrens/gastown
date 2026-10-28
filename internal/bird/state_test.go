package bird

import (
	"errors"
	"testing"
	"time"
)

func at(minute int) time.Time {
	return time.Date(2026, 10, 28, 12, minute, 0, 0, time.UTC)
}

// nobodyIsLive is a liveness read that answers for a machine with no
// processes at all.
func nobodyIsLive(Agent) (bool, error) { return false, nil }

func TestBindStoresOneBindingPerNameAndBead(t *testing.T) {
	t.Parallel()
	var st State
	if !st.Bind("wren", "gt-a", at(1)) {
		t.Fatal("first Bind reported no binding added")
	}
	if st.Bind("wren", "gt-a", at(2)) {
		t.Fatal("second Bind added the same name and bead pair again")
	}
	if got := len(st.Bindings); got != 1 {
		t.Fatalf("bindings = %+v, want exactly one for the pair", st.Bindings)
	}
}

func TestBindDropsTheReservationForItsName(t *testing.T) {
	t.Parallel()
	var st State
	st.Reserve("wren", at(1))
	st.Reserve("robin", at(1))
	st.Bind("wren", "gt-a", at(2))
	if len(st.Reservations) != 1 || st.Reservations[0].Name != "robin" {
		t.Errorf("reservations = %+v, want only the one no binding claimed", st.Reservations)
	}
}

func TestBoundReturnsNewestNameForABead(t *testing.T) {
	t.Parallel()
	var st State
	st.Bind("lark", "gt-a", at(1))
	st.Bind("wren", "gt-a", at(3))
	st.Bind("robin", "gt-a", at(2))
	if name, ok := st.Bound("gt-a"); !ok || name != "wren" {
		t.Errorf("Bound(gt-a) = %q, %v, want the newest binding wren", name, ok)
	}
	if name, ok := st.Bound("gt-b"); ok {
		t.Errorf("Bound(gt-b) = %q, true, want no binding for an unbound bead", name)
	}
}

func TestBoundLetsTheLaterBindingWinATie(t *testing.T) {
	t.Parallel()
	var st State
	st.Bind("lark", "gt-a", at(1))
	st.Bind("wren", "gt-a", at(1))
	if name, ok := st.Bound("gt-a"); !ok || name != "wren" {
		t.Errorf("Bound(gt-a) = %q, %v, want the later of two bindings at the same time", name, ok)
	}
}

func TestPruneDeadDropsOnlyRejectedAgents(t *testing.T) {
	t.Parallel()
	st := State{Agents: []Agent{
		{Name: "wren", PID: 10, Start: "tok"},
		{Name: "robin", PID: 11, Start: "tok"},
	}}
	dropped, err := st.PruneDead(func(a Agent) (bool, error) { return a.PID != 11, nil })
	if err != nil {
		t.Fatalf("PruneDead: %v", err)
	}
	if dropped != 1 {
		t.Errorf("PruneDead dropped %d agents, want 1", dropped)
	}
	if len(st.Agents) != 1 || st.Agents[0].Name != "wren" {
		t.Errorf("agents = %+v, want only wren", st.Agents)
	}
}

func TestPruneDeadKeepsEverythingWhenALivenessIsUnreadable(t *testing.T) {
	t.Parallel()
	st := State{Agents: []Agent{
		{Name: "wren", PID: 10, Start: "tok"},
		{Name: "robin", PID: 11, Start: "tok"},
	}}
	unreadable := errors.New("no process table")
	read := func(a Agent) (bool, error) {
		if a.Name == "robin" {
			return false, unreadable
		}
		return false, nil
	}
	if _, err := st.PruneDead(read); !errors.Is(err, unreadable) {
		t.Fatalf("PruneDead = %v, want the read error", err)
	}
	if len(st.Agents) != 2 {
		t.Errorf("agents = %+v, want both kept: an unreadable process is not a dead one", st.Agents)
	}
}

func TestPruneReservationsDropsTheOldOnes(t *testing.T) {
	t.Parallel()
	st := State{Reservations: []Reservation{
		{Name: "wren", Time: at(0)},
		{Name: "robin", Time: at(9)},
	}}
	dropped := st.PruneReservations(at(10), 5*time.Minute)
	if dropped != 1 {
		t.Errorf("PruneReservations dropped %d, want the one past its ttl", dropped)
	}
	if len(st.Reservations) != 1 || st.Reservations[0].Name != "robin" {
		t.Errorf("reservations = %+v, want the fresh one kept", st.Reservations)
	}
}

func TestTakenHoldsBoundLiveAndFreshlyReservedNames(t *testing.T) {
	t.Parallel()
	st := State{
		Agents: []Agent{
			{Name: "heron", PID: 10, Start: "tok"},
			{Name: "egret", PID: 11, Start: "tok"},
		},
		Bindings: []Binding{{Name: "wren", Bead: "gt-a", Time: at(0)}},
		Reservations: []Reservation{
			{Name: "lark", Time: at(9)},
			{Name: "robin", Time: at(0)},
		},
	}
	live := func(a Agent) (bool, error) { return a.Name == "heron", nil }
	taken, err := st.Taken(at(10), 5*time.Minute, live)
	if err != nil {
		t.Fatalf("Taken: %v", err)
	}
	want := map[string]bool{"wren": true, "heron": true, "lark": true}
	for name := range want {
		if !taken[name] {
			t.Errorf("taken[%q] = false, want the name held", name)
		}
	}
	for _, name := range []string{"egret", "robin"} {
		if taken[name] {
			t.Errorf("taken[%q] = true, want a dead agent and an expired reservation free", name)
		}
	}
}

func TestTakenReportsALivenessItCannotRead(t *testing.T) {
	t.Parallel()
	st := State{Agents: []Agent{{Name: "heron", PID: 10, Start: "tok"}}}
	unreadable := errors.New("no process table")
	read := func(Agent) (bool, error) { return false, unreadable }
	if _, err := st.Taken(at(0), 5*time.Minute, read); !errors.Is(err, unreadable) {
		t.Fatalf("Taken = %v, want the read error", err)
	}
}

func TestReserveReplacesTheEarlierReservationForAName(t *testing.T) {
	t.Parallel()
	var st State
	st.Reserve("wren", at(0))
	st.Reserve("wren", at(4))
	if len(st.Reservations) != 1 || !st.Reservations[0].Time.Equal(at(4)) {
		t.Errorf("reservations = %+v, want one for wren stamped at(4)", st.Reservations)
	}
}
