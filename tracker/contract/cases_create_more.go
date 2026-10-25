package contract

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// These cases pin behavior only Postgres models (rigs are rows there, ids
// come from counters and constraints); the in-memory fake does not run them.
func init() {
	register(Case{Name: "create refuses a rig that does not exist", Run: createUnknownRig})
	register(Case{Name: "a child gets the next number under its parent and records the parent", Run: createChildIDs})
	register(Case{Name: "create under a missing parent is not found", Run: createMissingParent})
	register(Case{Name: "an ephemeral issue gets a wisp id", Run: createWispID})
	register(Case{Name: "labels are de-duplicated and empty ones dropped", Run: createLabelDedupe})
	register(Case{Name: "an event payload must be JSON and round-trips", Run: createPayload})
	register(Case{Name: "racing creates of one explicit id leave exactly one winner", Run: createRace})
}

func createUnknownRig(t *testing.T, s store.Store) {
	if _, err := s.CreateIssue(ctx(), api.CreateParams{Rig: "zz", Title: "x"}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("unknown rig error = %v, want api.ErrInvalid", err)
	}
}

func createChildIDs(t *testing.T, s store.Store) {
	parent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "epic"})
	c1 := mustCreate(t, s, api.CreateParams{Parent: parent.ID, Title: "one"})
	c2 := mustCreate(t, s, api.CreateParams{Parent: parent.ID, Title: "two"})
	if c1.ID != parent.ID+".1" || c2.ID != parent.ID+".2" {
		t.Errorf("child ids = %s, %s; want %s.1 and %s.2", c1.ID, c2.ID, parent.ID, parent.ID)
	}
	got, err := s.GetIssues(ctx(), []string{c1.ID})
	if err != nil || got[c1.ID].Parent != parent.ID {
		t.Errorf("child %s Parent = %q, %v; want %s", c1.ID, got[c1.ID].Parent, err, parent.ID)
	}
	// An explicit id under a parent is kept and still linked.
	c3 := mustCreate(t, s, api.CreateParams{ID: "gt-explicit", Parent: parent.ID, Title: "three"})
	if c3.ID != "gt-explicit" || c3.Parent != parent.ID {
		t.Errorf("explicit child = %+v, want id gt-explicit under %s", c3, parent.ID)
	}
}

func createMissingParent(t *testing.T, s store.Store) {
	if _, err := s.CreateIssue(ctx(), api.CreateParams{Parent: "gt-nope", Title: "orphan"}); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("missing parent error = %v, want api.ErrNotFound", err)
	}
}

func createWispID(t *testing.T, s store.Store) {
	w := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "w", Ephemeral: true})
	if !w.Ephemeral || !regexp.MustCompile(`^hq-wisp-[0-9a-z]{6}$`).MatchString(w.ID) {
		t.Errorf("wisp = %+v, want ephemeral with an hq-wisp-xxxxxx id", w)
	}
}

func createLabelDedupe(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "l", Labels: []string{"b", "a", "a", ""}})
	if !slices.Equal(is.Labels, []string{"a", "b"}) {
		t.Errorf("Labels = %v, want [a b]", is.Labels)
	}
	got, _ := s.GetIssues(ctx(), []string{is.ID})
	if !slices.Equal(got[is.ID].Labels, []string{"a", "b"}) {
		t.Errorf("stored Labels = %v, want [a b]", got[is.ID].Labels)
	}
}

func createPayload(t *testing.T, s store.Store) {
	if _, err := s.CreateIssue(ctx(), api.CreateParams{Rig: "gt", Title: "e", Type: "event", EventKind: "k", EventPayload: "{not json"}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("bad payload error = %v, want api.ErrInvalid", err)
	}
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "e", Type: "event", EventKind: "wisp.compaction", EventPayload: `{"a":1,"b":[true]}`})
	var want, got any
	_ = json.Unmarshal([]byte(`{"a":1,"b":[true]}`), &want)
	if err := json.Unmarshal([]byte(is.Payload), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Payload = %q, want JSON equal to the input (%v)", is.Payload, err)
	}
	if is.EventKind != "wisp.compaction" {
		t.Errorf("EventKind = %q, want wisp.compaction", is.EventKind)
	}
}

func createRace(t *testing.T, s store.Store) {
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.CreateIssue(ctx(), api.CreateParams{ID: "gt-race1", Title: "r"})
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, api.ErrConflict):
			conflicts++
		default:
			t.Errorf("unexpected error %v", err)
		}
	}
	if wins != 1 || conflicts != n-1 {
		t.Errorf("%d winners and %d conflicts among %d racers, want 1 and %d", wins, conflicts, n, n-1)
	}
}
