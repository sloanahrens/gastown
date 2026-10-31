package contract

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "dependencies add, list by type, and remove", Run: depsRoundTrip})
	register(Case{Name: "dependencies refuse self links, missing ends, bad types and bad external refs", Run: depsRefusals})
	register(Case{Name: "adding a link twice is a no-op and a different type is a conflict", Run: depsIdempotent})
	register(Case{Name: "a cycle of blocking links is refused and other types may cycle", Run: depsCycles})
	register(Case{Name: "opposite blocking links added at the same instant never both succeed", Run: depsCycleRace})
	register(Case{Name: "an external target needs no row and is left out of the list", Run: depsExternal})
	register(Case{Name: "a dependent of an external:<rig>:<id> target is listed by its id once, and a near-miss suffix is not", Fake: true, Run: depsExternalDependents})
	register(Case{Name: "a parent-child link sets and clears the parent", Run: depsParentChild})
	register(Case{Name: "an open blocker stops close unless forced", Run: depsBlockerFence})
	register(Case{Name: "delete removes links to the deleted issue and list counts links", Run: depsDeleteAndCounts})
}

func mustDep(t *testing.T, s store.Store, issue, on, typ string) {
	t.Helper()
	if err := s.AddDependency(ctx(), issue, on, typ); err != nil {
		t.Fatalf("AddDependency(%s, %s, %s): %v", issue, on, typ, err)
	}
}

func depsRoundTrip(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b", Priority: 1})
	c := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "c"})
	mustDep(t, s, a.ID, b.ID, "blocks")
	mustDep(t, s, a.ID, c.ID, "tracks")
	all, err := s.Dependencies(ctx(), a.ID, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("Dependencies = %v, %v; want 2", all, err)
	}
	byID := map[string]api.IssueDep{}
	for _, d := range all {
		byID[d.ID] = d
	}
	if d := byID[b.ID]; d.DependencyType != "blocks" || d.Title != "b" || d.Status != "open" || d.Priority != 1 {
		t.Errorf("link to b = %+v, want type blocks with b's title, status and priority", d)
	}
	if only, _ := s.Dependencies(ctx(), a.ID, "tracks"); len(only) != 1 || only[0].ID != c.ID {
		t.Errorf("Dependencies by type tracks = %v, want just %s", only, c.ID)
	}
	if none, err := s.Dependencies(ctx(), b.ID, ""); err != nil || none == nil || len(none) != 0 {
		t.Errorf("Dependencies of b = %#v, %v; want an empty non-nil slice (links point one way)", none, err)
	}
	if err := s.RemoveDependency(ctx(), a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.Dependencies(ctx(), a.ID, ""); len(left) != 1 {
		t.Errorf("after remove: %v, want one link left", left)
	}
	if err := s.RemoveDependency(ctx(), a.ID, b.ID); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("removing a missing link = %v, want api.ErrNotFound", err)
	}
	if _, err := s.Dependencies(ctx(), "gt-nope", ""); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Dependencies of a missing issue = %v, want api.ErrNotFound", err)
	}
}

func depsRefusals(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b"})
	for name, tc := range map[string]struct {
		issue, on, typ string
		want           *api.Error
	}{
		"self link":          {a.ID, a.ID, "blocks", api.ErrInvalid},
		"missing source":     {"gt-nope", b.ID, "blocks", api.ErrNotFound},
		"missing target":     {a.ID, "gt-nope", "blocks", api.ErrNotFound},
		"bad type":           {a.ID, b.ID, "Not A Type", api.ErrInvalid},
		"empty type":         {a.ID, b.ID, "", api.ErrInvalid},
		"malformed external": {a.ID, "external:om", "tracks", api.ErrInvalid},
	} {
		if err := s.AddDependency(ctx(), tc.issue, tc.on, tc.typ); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want kind %s", name, err, tc.want.Kind)
		}
	}
}

func depsIdempotent(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b"})
	mustDep(t, s, a.ID, b.ID, "blocks")
	mustDep(t, s, a.ID, b.ID, "blocks")
	if got, _ := s.Dependencies(ctx(), a.ID, ""); len(got) != 1 {
		t.Errorf("after adding twice: %v, want one link", got)
	}
	if err := s.AddDependency(ctx(), a.ID, b.ID, "tracks"); !errors.Is(err, api.ErrConflict) {
		t.Errorf("same pair, other type = %v, want api.ErrConflict", err)
	}
}

func depsCycles(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b"})
	c := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "c"})
	mustDep(t, s, a.ID, b.ID, "blocks")
	mustDep(t, s, b.ID, c.ID, "blocks")
	if err := s.AddDependency(ctx(), c.ID, a.ID, "blocks"); !errors.Is(err, api.ErrConflict) {
		t.Errorf("closing a blocking cycle = %v, want api.ErrConflict", err)
	}
	if err := s.AddDependency(ctx(), c.ID, c.ID, "blocks"); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("self cycle = %v, want api.ErrInvalid", err)
	}
	mustDep(t, s, c.ID, a.ID, "relates-to") // informational links may loop
	mustDep(t, s, a.ID, c.ID, "relates-to")
}

func depsExternal(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	mustDep(t, s, a.ID, "external:om:om-7", "tracks")
	if got, err := s.Dependencies(ctx(), a.ID, ""); err != nil || len(got) != 0 {
		t.Errorf("Dependencies = %v, %v; want the external target left out", got, err)
	}
	page := mustList(t, s, api.ListFilter{})
	if len(page.Issues) != 1 || page.Issues[0].DependencyCount != 1 {
		t.Errorf("listed %+v, want DependencyCount 1 (the external link still counts)", page.Issues)
	}
	if err := s.CloseIssues(ctx(), []string{a.ID}, "r", false); err != nil {
		t.Errorf("an external link must not block close: %v", err)
	}
}

// depsExternalDependents pins the reverse read for an external target: a
// dependent whose link names the id as external:<rig>:<id> is listed under
// that id, once even when it also links plainly, and a target whose last part
// merely ends with the id's characters is not named by it.
func depsExternalDependents(t *testing.T, s store.Store) {
	target := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "target"})
	plain := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "plain"})
	both := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "both"})
	ext := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "external"})
	near := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "near miss"})
	mustDep(t, s, plain.ID, target.ID, "blocks")
	mustDep(t, s, both.ID, target.ID, "blocks")
	mustDep(t, s, both.ID, "external:om:"+target.ID, "tracks")
	mustDep(t, s, ext.ID, "external:om:"+target.ID, "blocks")
	mustDep(t, s, near.ID, "external:om:wisp-"+target.ID, "blocks")

	got := mustGet(t, s, target.ID)
	rows := make([]string, 0, len(got.Dependents))
	for _, d := range got.Dependents {
		rows = append(rows, d.ID+" "+d.DependencyType)
	}
	want := []string{plain.ID + " blocks", both.ID + " blocks", ext.ID + " blocks"}
	slices.Sort(rows)
	slices.Sort(want)
	if !slices.Equal(rows, want) {
		t.Errorf("Dependents of %s = %v, want %v (both listed once, at the plain relation; near miss absent)", target.ID, rows, want)
	}
}

func depsParentChild(t *testing.T, s store.Store) {
	p := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "p"})
	p2 := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "p2"})
	k := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "k"})
	mustDep(t, s, k.ID, p.ID, "parent-child")
	if got := mustGet(t, s, k.ID); got.Parent != p.ID {
		t.Errorf("Parent = %q, want %s", got.Parent, p.ID)
	}
	if err := s.AddDependency(ctx(), k.ID, p2.ID, "parent-child"); !errors.Is(err, api.ErrConflict) {
		t.Errorf("a second parent = %v, want api.ErrConflict", err)
	}
	if err := s.RemoveDependency(ctx(), k.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, k.ID); got.Parent != "" {
		t.Errorf("Parent = %q after the link was removed, want none", got.Parent)
	}
}

func depsBlockerFence(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b"})
	mustDep(t, s, a.ID, b.ID, "blocks")
	err := s.CloseIssues(ctx(), []string{a.ID}, "r", false)
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), b.ID) {
		t.Fatalf("closing a blocked issue = %v, want a conflict that names %s", err, b.ID)
	}
	if err := s.CloseIssues(ctx(), []string{b.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIssues(ctx(), []string{a.ID}, "r", false); err != nil {
		t.Errorf("closing after the blocker closed: %v", err)
	}
	c := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "c"})
	d := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "d"})
	mustDep(t, s, c.ID, d.ID, "blocks")
	if err := s.CloseIssues(ctx(), []string{c.ID}, "forced", true); err != nil {
		t.Errorf("a forced close past an open blocker: %v", err)
	}
}

func depsDeleteAndCounts(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b"})
	mustDep(t, s, a.ID, b.ID, "blocks")
	byID := map[string]api.Issue{}
	for _, is := range mustList(t, s, api.ListFilter{}).Issues {
		byID[is.ID] = is
	}
	if byID[a.ID].DependencyCount != 1 || byID[b.ID].DependentCount != 1 {
		t.Errorf("counts: a %+v, b %+v; want a.DependencyCount 1 and b.DependentCount 1", byID[a.ID], byID[b.ID])
	}
	if err := s.DeleteIssues(ctx(), []string{b.ID}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Dependencies(ctx(), a.ID, ""); err != nil || len(got) != 0 {
		t.Errorf("after deleting the target: %v, %v; want no links", got, err)
	}
	// No dangling row may linger: it would still be counted.
	for _, is := range mustList(t, s, api.ListFilter{}).Issues {
		if is.ID == a.ID && is.DependencyCount != 0 {
			t.Errorf("DependencyCount = %d after the target was deleted, want 0: its link row must go with it", is.DependencyCount)
		}
	}
	if err := s.CloseIssues(ctx(), []string{a.ID}, "r", false); err != nil {
		t.Errorf("a deleted blocker must not block close: %v", err)
	}
}

func depsCycleRace(t *testing.T, s store.Store) {
	// Many rounds, both directions released together: a cycle check made
	// outside any shared lock shows up as a round where both links land.
	const rounds = 25
	for round := 0; round < rounds; round++ {
		a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: fmt.Sprintf("a%d", round)})
		b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: fmt.Sprintf("b%d", round)})
		pairs := [][2]string{{a.ID, b.ID}, {b.ID, a.ID}}
		errs := make([]error, len(pairs))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, pr := range pairs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = s.AddDependency(ctx(), pr[0], pr[1], "blocks")
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, api.ErrConflict):
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d of the two opposite links were added, want exactly 1 (a cycle otherwise)", round, wins)
		}
	}
}
