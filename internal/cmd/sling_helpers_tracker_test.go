package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/beads/trackerclient"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// trackerRoundTrip answers the tracker's issue reads in-process from issues,
// the way the tracker's HTTP API does: Show is a GET of the id, ShowMany a
// POST of the ids. Nothing leaves the test.
type trackerRoundTrip struct {
	issues map[string]api.Issue
}

func (tr trackerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/issues/"):
		issue, ok := tr.issues[strings.TrimPrefix(r.URL.Path, "/v1/issues/")]
		if !ok {
			rec.WriteHeader(http.StatusNotFound)
			break
		}
		_ = json.NewEncoder(rec).Encode(issue)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/issues:show-many":
		var req api.ShowManyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			rec.WriteHeader(http.StatusBadRequest)
			break
		}
		out := api.ShowManyResponse{Issues: map[string]api.Issue{}}
		for _, id := range req.IDs {
			if issue, ok := tr.issues[id]; ok {
				out.Issues[id] = issue
			}
		}
		_ = json.NewEncoder(rec).Encode(out)
	default:
		rec.WriteHeader(http.StatusNotFound)
	}
	return rec.Result(), nil
}

// TestMoleculeDepsOnATrackerStoreReadsEveryBondThroughDepListUp: the tracker
// answers a bead's dependents, external-target ones included, through
// DepListUp, so sling reads every bond through the seam, runs no SQL, and
// skips nothing (gt-etvxc). The tracks relation is not a bond.
func TestMoleculeDepsOnATrackerStoreReadsEveryBondThroughDepListUp(t *testing.T) {
	t.Parallel()
	tc, err := client.New(client.Config{
		BaseURL:     "http://127.0.0.1:1",
		Token:       "t",
		SchemaLevel: api.SchemaLevel,
		MaxRetries:  -1,
		HTTP: &http.Client{Transport: trackerRoundTrip{issues: map[string]api.Issue{
			"gt-work": {ID: "gt-work", Title: "work", Status: "open", Type: "task", Dependents: []api.IssueDep{
				{ID: "gt-wisp-live", Title: "live", Status: "open", DependencyType: "blocks"},
				{ID: "gt-wisp-ext", Title: "ext", Status: "open", DependencyType: "blocks"},
				{ID: "gt-wisp-done", Title: "done", Status: "closed", DependencyType: "parent-child"},
				{ID: "gt-wisp-track", Title: "track", Status: "open", DependencyType: "tracks"},
			}},
			"gt-wisp-live":  {ID: "gt-wisp-live", Title: "live", Status: "open", Type: "molecule", Ephemeral: true},
			"gt-wisp-ext":   {ID: "gt-wisp-ext", Title: "ext", Status: "open", Type: "molecule", Ephemeral: true},
			"gt-wisp-done":  {ID: "gt-wisp-done", Title: "done", Status: "closed", Type: "molecule", Ephemeral: true},
			"gt-wisp-track": {ID: "gt-wisp-track", Title: "track", Status: "open", Type: "molecule", Ephemeral: true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := trackerclient.New(tc, "gt")
	if _, ok := any(store).(sqlStore); ok {
		t.Fatal("the tracker client answers SQL; this test no longer models a tracker store")
	}
	stores := slingStores{pinned: func(string) slingStore { return store }}

	got, err := stores.moleculeDeps("gt-work", t.TempDir())
	if err != nil {
		t.Fatalf("moleculeDeps: %v", err)
	}
	if want := []string{"gt-wisp-live", "gt-wisp-ext"}; !slices.Equal(got, want) {
		t.Errorf("moleculeDeps() = %v, want every open molecule bond %v", got, want)
	}
}

// noSQLFake is a store with the slingStore surface and no SQL, so it fails the
// sqlStore assertion as the tracker does.
type noSQLFake struct {
	slingStore
}

// TestMoleculeDepsOnANoSQLStoreReadsAnExternalBondThroughDepListUp: a molecule
// whose dependency row names the bead as external:<rig>:<id> is a bond on a
// store with no SQL. The seam's DepListUp lists it, so sling keeps it and runs
// no external-target statement.
func TestMoleculeDepsOnANoSQLStoreReadsAnExternalBondThroughDepListUp(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-work", Title: "work", Status: "open", Type: "task"},
		beads.Issue{ID: "gt-wisp-ext", Title: "ext", Status: "open", Type: "molecule", Ephemeral: true},
		beads.Issue{ID: "gt-wisp-loose", Title: "loose", Status: "open", Type: "molecule", Ephemeral: true})
	if err := db.AddTypedDependency("gt-wisp-ext", "external:om:gt-work", "blocks"); err != nil {
		t.Fatalf("AddTypedDependency: %v", err)
	}
	store := noSQLFake{db}
	if _, ok := any(store).(sqlStore); ok {
		t.Fatal("noSQLFake answers SQL; this test no longer models a store with no SQL")
	}
	stores := slingStores{pinned: func(string) slingStore { return store }}

	got, err := stores.moleculeDeps("gt-work", t.TempDir())
	if err != nil {
		t.Fatalf("moleculeDeps: %v", err)
	}
	if want := []string{"gt-wisp-ext"}; !slices.Equal(got, want) {
		t.Errorf("moleculeDeps() = %v, want the external-target molecule %v", got, want)
	}
}
