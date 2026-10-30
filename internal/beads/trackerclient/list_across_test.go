package trackerclient

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// acrossServer answers the list route with issues and records every filter it
// was sent.
func acrossServer(t *testing.T, issues ...api.Issue) (*Client, func() []api.ListFilter) {
	t.Helper()
	var mu sync.Mutex
	var asked []api.ListFilter
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f api.ListFilter
		_ = json.NewDecoder(r.Body).Decode(&f)
		mu.Lock()
		asked = append(asked, f)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.IssuePage{Issues: issues})
	})
	tc, err := client.New(client.Config{BaseURL: "http://127.0.0.1:1", Token: "t", SchemaLevel: api.SchemaLevel, MaxRetries: -1, HTTP: &http.Client{Transport: roundTrip(handler)}})
	if err != nil {
		t.Fatal(err)
	}
	return New(tc, "gt"), func() []api.ListFilter {
		mu.Lock()
		defer mu.Unlock()
		return append([]api.ListFilter(nil), asked...)
	}
}

// Three rigs, one request: the scope list names them all and each issue comes
// back under the rig its id starts with.
func TestListAcrossAsksForEveryRigOnce(t *testing.T) {
	t.Parallel()
	c, asked := acrossServer(t,
		api.Issue{ID: "gt-a", Title: "a"},
		api.Issue{ID: "hq-b", Title: "b"},
		api.Issue{ID: "hq-c", Title: "c"},
	)
	res, err := c.ListAcross([]string{"gt", "hq", "be"}, beads.ListOptions{Label: "gt:ready-to-land", Limit: 0})
	if err != nil {
		t.Fatalf("ListAcross: %v", err)
	}
	if calls := asked(); len(calls) != 1 {
		t.Fatalf("requests = %d, want one for all three rigs", len(calls))
	}
	f := asked()[0]
	if strings.Join(f.Rigs, ",") != "gt,hq,be" {
		t.Errorf("rigs = %v, want the scope list gt hq be", f.Rigs)
	}
	if strings.Join(f.Labels, ",") != "gt:ready-to-land" || f.Limit != 0 {
		t.Errorf("filter = %+v, want the column's label and no page cap", f)
	}
	if got := len(res.Rigs["gt"]); got != 1 {
		t.Errorf("gt holds %d issues, want 1", got)
	}
	if got := len(res.Rigs["hq"]); got != 2 {
		t.Errorf("hq holds %d issues, want 2", got)
	}
	if got, ok := res.Rigs["be"]; !ok || len(got) != 0 {
		t.Errorf("be = %v (present %v), want an empty answer rather than none", got, ok)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v, want none", res.Errors)
	}
}

// An issue filed under a rig the caller did not ask for is a broken answer,
// not an issue to file under a rig nobody listed.
func TestListAcrossRefusesAnIssueFromARigItDidNotAskFor(t *testing.T) {
	t.Parallel()
	c, _ := acrossServer(t, api.Issue{ID: "nope-a", Title: "elsewhere"})
	if _, err := c.ListAcross([]string{"gt"}, beads.ListOptions{}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("ListAcross = %v, want an error naming the rig it was not asked for", err)
	}
}

// A filter the list query cannot express refuses the call, and no request is
// sent for it.
func TestListAcrossRefusesAFilterTheTrackerCannotExpress(t *testing.T) {
	t.Parallel()
	c, asked := acrossServer(t)
	_, err := c.ListAcross([]string{"gt"}, beads.ListOptions{UpdatedAfter: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if err == nil || !strings.Contains(err.Error(), "UpdatedAfter") {
		t.Errorf("ListAcross = %v, want the refusal naming the filter", err)
	}
	if calls := asked(); len(calls) != 0 {
		t.Errorf("requests = %d, want none for a refused filter", len(calls))
	}
}
