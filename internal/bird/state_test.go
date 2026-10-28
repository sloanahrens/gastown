package bird

import (
	"testing"
	"time"
)

func at(minute int) time.Time {
	return time.Date(2026, 10, 28, 12, minute, 0, 0, time.UTC)
}

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
	dropped := st.PruneDead(func(a Agent) bool { return a.PID != 11 })
	if dropped != 1 {
		t.Errorf("PruneDead dropped %d agents, want 1", dropped)
	}
	if len(st.Agents) != 1 || st.Agents[0].Name != "wren" {
		t.Errorf("agents = %+v, want only wren", st.Agents)
	}
}
