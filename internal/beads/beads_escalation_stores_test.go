package beads

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestListEscalationsAcrossRigsMergesStoresSortedByID holds gt-lqlupr: the
// escalation list reads the town store and every Dolt rig store the town's
// routes name, so an escalation filed in a rig's own store is not dropped from
// `gt escalate list`, `gt status` and the dashboard. The per-store read is the
// package seam escalationStoreRead, replaced here so no bd runs, which is why
// the test does not run in parallel.
//
// The town has a gt- rig store, a tracker-backed rig (not a Dolt store) and a
// route whose beads directory was never checked out; only the first two Dolt
// stores are read. An id present in two stores comes back once.
func TestListEscalationsAcrossRigsMergesStoresSortedByID(t *testing.T) {
	townRoot, _ := townWithRoutes(t)
	routes := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n" +
		`{"prefix":"tr-","path":"tracker/mayor/rig","backend":"tracker"}` + "\n" +
		`{"prefix":"zz-","path":"gone/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	// The tracker rig has a beads directory, so only its backend keeps it out.
	if err := os.MkdirAll(filepath.Join(townRoot, "tracker", "mayor", "rig", ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	esc := func(id string) *Issue { return &Issue{ID: id, Labels: []string{"gt:escalation"}} }
	var read []string
	prev := escalationStoreRead
	t.Cleanup(func() { escalationStoreRead = prev })
	escalationStoreRead = func(_ *Beads, s escalationStore, _ string) ([]*Issue, error) {
		read = append(read, s.BeadsDir)
		if s.Town {
			return []*Issue{esc("hq-wisp2"), esc("hq-wisp1")}, nil
		}
		return []*Issue{esc("gt-esc1"), esc("hq-wisp1")}, nil
	}

	got, err := ListEscalationsAcrossRigs(New(townRoot))
	if err != nil {
		t.Fatalf("ListEscalationsAcrossRigs: %v", err)
	}
	var ids []string
	for _, i := range got {
		ids = append(ids, i.ID)
	}
	if want := []string{"gt-esc1", "hq-wisp1", "hq-wisp2"}; !slices.Equal(ids, want) {
		t.Errorf("escalation ids = %q, want %q: both stores, sorted by id, the shared id once", ids, want)
	}
	wantStores := []string{
		filepath.Join(townRoot, ".beads"),
		filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"),
	}
	if !slices.Equal(read, wantStores) {
		t.Errorf("stores read = %q, want %q (not the tracker rig, not the rig with no beads directory)", read, wantStores)
	}
	for _, s := range read {
		if strings.Contains(s, "tracker") || strings.Contains(s, "gone") {
			t.Errorf("read the store %s, which is not a Dolt store on disk", s)
		}
	}
}

// A route whose beads directory exists but cannot be examined is not a store
// that is absent: dropping it would hide its escalations from `gt escalate
// list`, `gt status` and the dashboard with no error. The route here runs
// through a regular file, so the stat fails with ENOTDIR whoever runs the test;
// only fs.ErrNotExist is skipped (gt-lqlupr).
func TestListEscalationsAcrossRigsRefusesAStoreItCannotStat(t *testing.T) {
	townRoot, _ := townWithRoutes(t)
	if err := os.WriteFile(filepath.Join(townRoot, "afile"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"hq-","path":"."}` + "\n" + `{"prefix":"zz-","path":"afile/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := escalationStoreRead
	t.Cleanup(func() { escalationStoreRead = prev })
	escalationStoreRead = func(*Beads, escalationStore, string) ([]*Issue, error) {
		return []*Issue{{ID: "hq-wisp1", Labels: []string{"gt:escalation"}}}, nil
	}

	got, err := ListEscalationsAcrossRigs(New(townRoot))
	if err == nil {
		t.Fatalf("ListEscalationsAcrossRigs = %v, want an error naming the store it could not examine", got)
	}
	if !strings.Contains(err.Error(), "afile") || got != nil {
		t.Errorf("got %v, %v; want no partial list and an error naming afile/mayor/rig", got, err)
	}
}

// TestListEscalationsAcrossRigsRefusesARoutesFileItCannotRead pins the routes
// read: a routes.jsonl that cannot be read (here a directory of that name) is an
// error naming the routes, never a town read as having only its own store.
func TestListEscalationsAcrossRigsRefusesARoutesFileItCannotRead(t *testing.T) {
	townRoot, _ := townWithRoutes(t)
	routesPath := filepath.Join(townRoot, ".beads", "routes.jsonl")
	if err := os.RemoveAll(routesPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(routesPath, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := escalationStoreRead
	t.Cleanup(func() { escalationStoreRead = prev })
	escalationStoreRead = func(*Beads, escalationStore, string) ([]*Issue, error) {
		return []*Issue{{ID: "hq-wisp1", Labels: []string{"gt:escalation"}}}, nil
	}

	got, err := ListEscalationsAcrossRigs(New(townRoot))
	if err == nil {
		t.Fatalf("ListEscalationsAcrossRigs = %v, want an error naming the routes it could not read", got)
	}
	if !strings.Contains(err.Error(), "reading the routes") || got != nil {
		t.Errorf("got %v, %v; want no list and an error saying it could not read the routes", got, err)
	}
}

// TestListEscalationsAcrossRigsFailedStoreNamesIt pins that one store that
// cannot be read fails the whole read: the town store answered, the gt rig
// store did not, and the list is not the town's alone with the rig's
// escalations silently missing (gt-3zz0el). The error names the rig store's
// beads directory and wraps the read's own error.
func TestListEscalationsAcrossRigsFailedStoreNamesIt(t *testing.T) {
	townRoot, _ := townWithRoutes(t)
	rigStore := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	errDolt := errors.New("dolt: connection refused")
	prev := escalationStoreRead
	t.Cleanup(func() { escalationStoreRead = prev })
	escalationStoreRead = func(_ *Beads, s escalationStore, _ string) ([]*Issue, error) {
		if s.Town {
			return []*Issue{{ID: "hq-wisp1", Labels: []string{"gt:escalation"}}}, nil
		}
		return nil, errDolt
	}

	got, err := ListEscalationsAcrossRigs(New(townRoot))
	if err == nil {
		t.Fatalf("ListEscalationsAcrossRigs = %v, want an error: the rig store could not be read", got)
	}
	if !strings.Contains(err.Error(), rigStore) {
		t.Errorf("error = %q, want it to name the store %s", err, rigStore)
	}
	if !errors.Is(err, errDolt) {
		t.Errorf("error = %q, want it to wrap the read's own error", err)
	}
	if got != nil {
		t.Errorf("got the partial list %v, want none", got)
	}
}
