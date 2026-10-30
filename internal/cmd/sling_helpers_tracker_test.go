package cmd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads/trackerclient"
	"github.com/steveyegge/gastown/internal/beadsql"
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

// trackerSlingStore is a tracker-backed sling store: the tracker client with
// the SQLCSV every sling store carries. The tracker keeps no SQL, so sling
// reaching SQLCSV is a failure.
type trackerSlingStore struct {
	*trackerclient.Client
	t *testing.T
}

func (s trackerSlingStore) SQLCSV(query beadsql.Query) ([][]string, error) {
	s.t.Errorf("sling ran SQL against a tracker store: %s", query)
	return nil, errors.New("tracker store has no SQL")
}

// TestMoleculeDepsOnATrackerStoreNeverReadsSQL: a bead in a tracker rig is
// slung through the tracker's dependents. Its open molecules come back, and
// sling never asks the tracker for SQL.
func TestMoleculeDepsOnATrackerStoreNeverReadsSQL(t *testing.T) {
	t.Parallel()
	tc, err := client.New(client.Config{
		BaseURL:     "http://127.0.0.1:1",
		Token:       "t",
		SchemaLevel: api.SchemaLevel,
		MaxRetries:  -1,
		HTTP: &http.Client{Transport: trackerRoundTrip{issues: map[string]api.Issue{
			"gt-work": {ID: "gt-work", Title: "work", Status: "open", Type: "task", Dependents: []api.IssueDep{
				{ID: "gt-wisp-live", Title: "live", Status: "open", DependencyType: "blocks"},
				{ID: "gt-wisp-done", Title: "done", Status: "closed", DependencyType: "parent-child"},
				{ID: "gt-wisp-track", Title: "track", Status: "open", DependencyType: "tracks"},
			}},
			"gt-wisp-live":  {ID: "gt-wisp-live", Title: "live", Status: "open", Type: "molecule"},
			"gt-wisp-done":  {ID: "gt-wisp-done", Title: "done", Status: "closed", Type: "molecule"},
			"gt-wisp-track": {ID: "gt-wisp-track", Title: "track", Status: "open", Type: "molecule"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := trackerSlingStore{trackerclient.New(tc, "gt"), t}
	stores := slingStores{pinned: func(string) slingStore { return store }}

	got, err := stores.moleculeDeps("gt-work", t.TempDir())
	if err != nil {
		t.Fatalf("moleculeDeps: %v", err)
	}
	if want := []string{"gt-wisp-live"}; !slices.Equal(got, want) {
		t.Errorf("moleculeDeps() = %v, want %v", got, want)
	}
}
