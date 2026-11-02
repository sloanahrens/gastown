package bdshim

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/backend"
)

// stubDepStore answers the two reads bdView wraps. The embedded interface is
// nil: the view promotes every other method and the test calls only these. A
// set err is what both reads answer, so the view's failure path is drivable.
type stubDepStore struct {
	backend.Store
	issues map[string]*beads.Issue
	err    error
}

func (s stubDepStore) Show(id string) (*beads.Issue, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.issues[id], nil
}

func (s stubDepStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		if is, ok := s.issues[id]; ok {
			out[id] = is
		}
	}
	return out, nil
}

// TestBDViewCarriesAReadError pins that a failed read reaches the handler as
// the store answered it, never as a nil issue or an empty list.
func TestBDViewCarriesAReadError(t *testing.T) {
	t.Parallel()
	down := errors.New("tracker down")
	view := bdView{stubDepStore{err: down}}
	if _, err := view.Show("fx-a"); !errors.Is(err, down) {
		t.Errorf("Show error = %v, want tracker down", err)
	}
	if _, err := view.ShowMultiple([]string{"fx-a"}); !errors.Is(err, down) {
		t.Errorf("ShowMultiple error = %v, want tracker down", err)
	}
}

// TestBDViewLeavesOutOnlyTheExternalTarget pins the view the shim's bd-shaped
// verbs render: the id-only link to another rig is left out of Show and
// ShowMultiple, the local links stay in order, and the store's issue is not
// touched, so a later gt read of the same store still sees the whole set.
func TestBDViewLeavesOutOnlyTheExternalTarget(t *testing.T) {
	t.Parallel()
	local := beads.IssueDep{ID: "fx-b", Title: "b", Status: "open", DependencyType: "blocks"}
	external := beads.IssueDep{ID: "external:om:om-7", DependencyType: "tracks"}
	tracked := &beads.Issue{ID: "fx-a", Dependencies: []beads.IssueDep{local, external}}
	plain := &beads.Issue{ID: "fx-c", Dependencies: []beads.IssueDep{local}}
	view := bdView{stubDepStore{issues: map[string]*beads.Issue{"fx-a": tracked, "fx-c": plain}}}

	got, err := view.Show("fx-a")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if want := []beads.IssueDep{local}; !reflect.DeepEqual(got.Dependencies, want) {
		t.Errorf("Show(fx-a).Dependencies = %+v, want %+v", got.Dependencies, want)
	}
	if len(tracked.Dependencies) != 2 {
		t.Errorf("the store's issue now has %+v, want its two links untouched", tracked.Dependencies)
	}

	multi, err := view.ShowMultiple([]string{"fx-a", "fx-c"})
	if err != nil {
		t.Fatalf("ShowMultiple: %v", err)
	}
	if want := []beads.IssueDep{local}; !reflect.DeepEqual(multi["fx-a"].Dependencies, want) {
		t.Errorf("ShowMultiple(fx-a).Dependencies = %+v, want %+v", multi["fx-a"].Dependencies, want)
	}
	if len(multi) != 2 {
		t.Errorf("ShowMultiple answered %d issues, want both", len(multi))
	}
}
