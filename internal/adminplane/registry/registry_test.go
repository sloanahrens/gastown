package registry

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// at is the time every test's clock reports, far enough from the wall clock
// that a call the package makes to time.Now instead is visible.
var at = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fixedClock always reports at.
func fixedClock() time.Time { return at }

// allowed is the move table the plan states, written out here so that a change
// to the package's own table cannot vouch for itself.
var allowed = map[[2]State]bool{
	{Active, Paused}:    true,
	{Active, Suspended}: true,
	{Active, Closed}:    true,
	{Paused, Active}:    true,
	{Paused, Suspended}: true,
	{Paused, Closed}:    true,
	{Suspended, Active}: true,
	{Suspended, Closed}: true,
}

// states are the four states plus one this package does not know, so the
// refusal of an unknown state is exercised alongside the known ones.
var states = []State{Active, Paused, Suspended, Closed, "archived"}

// newWorkspace returns a workspace in state from.
func newWorkspace(from State) Workspace {
	return Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1", State: from, CreatedAt: at}
}

func TestLegalTransitions(t *testing.T) {
	t.Parallel()
	for pair := range allowed {
		from, to := pair[0], pair[1]
		t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
			t.Parallel()
			w := newWorkspace(from)
			got, ev, err := Transition(fixedClock, w, to, "operator")
			if err != nil {
				t.Fatalf("Transition(%s, %s) returned %v, want nil", from, to, err)
			}
			if got.State != to {
				t.Errorf("workspace state = %q, want %q", got.State, to)
			}
			if got.ID != w.ID || got.OrgID != w.OrgID || got.OwnerSub != w.OwnerSub {
				t.Errorf("workspace identity changed: got %+v, want %+v", got, w)
			}
			if ev.WorkspaceID != w.ID || ev.Actor != "operator" || ev.From != from || ev.To != to {
				t.Errorf("event = %+v, want workspace %q, actor %q, from %q, to %q", ev, w.ID, "operator", from, to)
			}
		})
	}
}

func TestIllegalTransitionsRefused(t *testing.T) {
	t.Parallel()
	for _, from := range states {
		for _, to := range states {
			if allowed[[2]State{from, to}] {
				continue
			}
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				t.Parallel()
				w := newWorkspace(from)
				got, ev, err := Transition(fixedClock, w, to, "operator")
				var refusal *TransitionError
				if !errors.As(err, &refusal) {
					t.Fatalf("Transition(%s, %s) returned error %v, want *TransitionError", from, to, err)
				}
				if refusal.From != from || refusal.To != to {
					t.Errorf("error names %q to %q, want %q to %q", refusal.From, refusal.To, from, to)
				}
				for _, want := range []string{string(from), string(to)} {
					if !strings.Contains(refusal.Error(), `"`+want+`"`) {
						t.Errorf("error %q does not name %q", refusal.Error(), want)
					}
				}
				if got != w {
					t.Errorf("refused move changed the workspace: got %+v, want %+v", got, w)
				}
				if ev != (Event{}) {
					t.Errorf("refused move returned event %+v, want none", ev)
				}
			})
		}
	}
}

func TestTransitionUsesInjectedClock(t *testing.T) {
	t.Parallel()
	calls := 0
	clock := func() time.Time {
		calls++
		return at
	}
	_, ev, err := Transition(clock, newWorkspace(Active), Paused, "operator")
	if err != nil {
		t.Fatalf("Transition returned %v, want nil", err)
	}
	if !ev.At.Equal(at) {
		t.Errorf("event time = %v, want the clock's %v", ev.At, at)
	}
	if calls != 1 {
		t.Errorf("clock called %d times, want 1", calls)
	}
}

func TestTransitionRefusesNilClock(t *testing.T) {
	t.Parallel()
	w := newWorkspace(Active)
	got, ev, err := Transition(nil, w, Paused, "operator")
	if !errors.Is(err, ErrNoClock) {
		t.Fatalf("Transition with no clock returned %v, want ErrNoClock", err)
	}
	if got != w || ev != (Event{}) {
		t.Errorf("refused move returned workspace %+v and event %+v, want the workspace unchanged and no event", got, ev)
	}
}
