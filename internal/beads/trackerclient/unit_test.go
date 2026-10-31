package trackerclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// Unit tier: pure mapping and a canned loopback server; milliseconds per case.
func TestMapErrMakesAMissingIssueErrNotFoundAndKeepsTheAPIError(t *testing.T) {
	t.Parallel()
	err := mapErr(api.NotFound("gt-x"))
	if !errors.Is(err, beads.ErrNotFound) || !errors.Is(err, api.ErrNotFound) {
		t.Errorf("mapErr(not found) = %v, want errors.Is both beads.ErrNotFound and api.ErrNotFound", err)
	}
	for name, e := range map[string]error{"guard": api.GuardFailed("held"), "conflict": api.Conflict("fence"), "invalid": api.Invalid("bad"), "unavailable": api.Unavailable("down")} {
		got := mapErr(e)
		if errors.Is(got, beads.ErrNotFound) {
			t.Errorf("%s refusal %v must not look like a missing issue", name, got)
		}
		if got != e {
			t.Errorf("%s: mapErr changed the error: %v", name, got)
		}
	}
	if mapErr(nil) != nil {
		t.Error("mapErr(nil) must be nil")
	}
}

func TestIssueConversionCarriesFieldsTimesAndDependencies(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 10, 1, 10, 0, 0, 0, time.FixedZone("x", 3600))
	closed := created.Add(time.Hour)
	got := issue(api.Issue{
		ID: "gt-a", Title: "t", Status: "closed", Priority: 1, Type: "task", CreatedAt: created, UpdatedAt: created, ClosedAt: &closed,
		BlockedBy: []string{"gt-b", "gt-c"}, Payload: `{"n": 3, "a": 1}`,
		Dependencies: []api.IssueDep{{ID: "gt-b", Title: "b", Status: "open", DependencyType: "blocks"}},
		Dependents:   []api.IssueDep{{ID: "gt-w", Title: "w", Status: "open", DependencyType: "tracks"}},
	})
	if got.CreatedAt != "2026-10-01T09:00:00Z" || got.ClosedAt != "2026-10-01T10:00:00Z" {
		t.Errorf("times = %q / %q, want RFC3339 in UTC", got.CreatedAt, got.ClosedAt)
	}
	if got.BlockedByCount != 2 || len(got.Dependencies) != 1 || got.Dependencies[0].DependencyType != "blocks" || got.Dependencies[0].Title != "b" {
		t.Errorf("BlockedByCount = %d, Dependencies = %+v", got.BlockedByCount, got.Dependencies)
	}
	if len(got.Dependents) != 1 || got.Dependents[0].ID != "gt-w" || got.Dependents[0].DependencyType != "tracks" {
		t.Errorf("Dependents = %+v, want the issue that depends on this one, with its relation", got.Dependents)
	}
	if got.Payload != `{"a":1,"n":3}` {
		t.Errorf("Payload = %q, want it compact with sorted keys, as bd returns it", got.Payload)
	}
	if issue(api.Issue{ID: "gt-z"}).ClosedAt != "" {
		t.Error("an open issue must have an empty ClosedAt")
	}
}

func TestCompactJSONLeavesNonJSONAlone(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": "", "plain text": "plain text", `{"b": 2,"a":1}`: `{"a":1,"b":2}`, `[1, 2]`: `[1,2]`} {
		if got := compactJSON(in); got != want {
			t.Errorf("compactJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListFilterMapsStatusLabelsPriorityAndLeavesRigAlone(t *testing.T) {
	t.Parallel()
	f := listFilter(beads.ListOptions{Status: "open,closed", Label: "gt:a", Labels: []string{"b"}, Type: "bug", Priority: beads.PriorityP3, Rig: "testrig", Limit: 4, Ephemeral: true})
	if len(f.Statuses) != 2 || f.Statuses[0] != "open" || f.Statuses[1] != "closed" {
		t.Errorf("Statuses = %v, want open and closed", f.Statuses)
	}
	if len(f.Labels) != 2 || f.Labels[0] != "gt:a" || f.Labels[1] != "b" {
		t.Errorf("Labels = %v, want the single Label first (Type is ignored when Label is set)", f.Labels)
	}
	if f.Priority == nil || *f.Priority != 3 || f.Limit != 4 || !f.Ephemeral || f.Rig != "" {
		t.Errorf("filter = %+v, want priority 3, limit 4, ephemeral and no rig (ListOptions.Rig filters merge requests, not the database)", f)
	}
	if all := listFilter(beads.ListOptions{Status: "all"}); len(all.Statuses) != 1 || all.Statuses[0] != "all" {
		t.Errorf("Status all = %v, want [all]", all.Statuses)
	}
	if def := listFilter(beads.ListOptions{}); len(def.Statuses) != 0 || def.Priority != nil {
		t.Errorf("zero options = %+v, want no status and no priority filter", def)
	}
	if typed := listFilter(beads.ListOptions{Type: "bug"}); len(typed.Labels) != 1 || typed.Labels[0] != "gt:bug" {
		t.Errorf("deprecated Type = %v, want the gt:bug label", typed.Labels)
	}
}

// TestListRefusesFiltersTheTrackerCannotExpress: a filter the tracker's list
// query has no field for is an error, not a silently wider answer, and List
// sends no request for it.
func TestListRefusesFiltersTheTrackerCannotExpress(t *testing.T) {
	t.Parallel()
	tc, err := client.New(client.Config{
		BaseURL: "http://127.0.0.1:1", Token: "t", SchemaLevel: api.SchemaLevel, MaxRetries: -1,
		HTTP: &http.Client{Transport: roundTrip(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("List sent %s?%s for a filter it must refuse", r.URL.Path, r.URL.RawQuery)
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := New(tc, "gt")
	for name, opts := range map[string]beads.ListOptions{
		"NotesContains": {NotesContains: "MERGE REJECTION"},
		"UpdatedAfter":  {UpdatedAfter: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if _, err := c.List(opts); !errors.Is(err, ErrListFilter) || !strings.Contains(err.Error(), name) {
			t.Errorf("List(%s) = %v, want an ErrListFilter naming the filter", name, err)
		}
	}
	if err := refusedListFilter(beads.ListOptions{Status: "all"}); err != nil {
		t.Errorf("a filter the tracker expresses was refused: %v", err)
	}
}

// roundTrip serves requests from handler in process: no listener, no network.
type roundTrip http.HandlerFunc

func (h roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	http.HandlerFunc(h).ServeHTTP(rec, r)
	return rec.Result(), nil
}

// closeServer answers POST /v1/issues:close from a table of id -> status.
func closeServer(t *testing.T, verdict map[string]int) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req api.CloseRequest
		_ = json.Unmarshal(b, &req)
		id := strings.Join(req.IDs, ",")
		mu.Lock()
		asked = append(asked, id+"/"+req.Reason)
		mu.Unlock()
		switch verdict[id] {
		case 404:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"kind":"not_found","message":"no such issue"}`))
		case 409:
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"kind":"conflict","message":"has open children"}`))
		case 503:
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"kind":"unavailable","message":"database does not answer"}`))
		default:
			w.WriteHeader(204)
		}
	})
	tc, err := client.New(client.Config{BaseURL: "http://127.0.0.1:1", Token: "t", SchemaLevel: api.SchemaLevel, MaxRetries: -1, HTTP: &http.Client{Transport: roundTrip(handler)}})
	if err != nil {
		t.Fatal(err)
	}
	return New(tc, "gt"), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), asked...) }
}

func TestCloseClosesOneAtATimeInOrderWithBdsDefaultReason(t *testing.T) {
	t.Parallel()
	c, asked := closeServer(t, nil)
	if err := c.Close("gt-a", "gt-b"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(asked(), " "); got != "gt-a/Closed gt-b/Closed" {
		t.Errorf("close calls = %q, want one per id in argument order, each with bd's default reason", got)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

func TestCloseReportsPartialRefusalsMissingAndAllRefused(t *testing.T) {
	t.Parallel()
	c, _ := closeServer(t, map[string]int{"gt-refused": 409, "gt-missing": 404, "gt-down": 503})

	var pe *beads.PartialCloseError
	err := c.CloseWithReason("r", "gt-ok", "gt-refused", "gt-ok2")
	if !errors.As(err, &pe) || strings.Join(pe.Closed, ",") != "gt-ok,gt-ok2" || strings.Join(pe.NotClosed, ",") != "gt-refused" {
		t.Errorf("some refused = %v, want a PartialCloseError closing gt-ok and gt-ok2 and not gt-refused", err)
	}
	if err := c.Close("gt-refused"); err == nil || errors.As(err, &pe) || errors.Is(err, beads.ErrNotFound) {
		t.Errorf("every issue refused = %v, want the refusal itself, not a partial and not a not-found", err)
	}
	if err := c.Close("gt-ok", "gt-missing"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("a missing id = %v, want beads.ErrNotFound", err)
	}
	if err := c.Close("gt-ok", "gt-down", "gt-ok2"); !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("an unavailable service = %v, want api.ErrUnavailable straight away", err)
	}
}

func TestCreateRefusesAFlagLikeTitleBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	c, asked := closeServer(t, nil)
	if _, err := c.Create(beads.CreateOptions{Title: "--help"}); !errors.Is(err, beads.ErrFlagTitle) {
		t.Errorf("Create(--help) = %v, want beads.ErrFlagTitle", err)
	}
	if len(asked()) != 0 {
		t.Errorf("a flag-like title reached the service: %v", asked())
	}
}

func TestCreateRefusesAnActorOptionLoudlyInsteadOfIgnoringIt(t *testing.T) {
	t.Parallel()
	c, asked := closeServer(t, nil)
	if _, err := c.Create(beads.CreateOptions{Title: "t", Actor: "someone-else"}); !errors.Is(err, ErrActorOption) {
		t.Errorf("Create with Actor = %v, want ErrActorOption", err)
	}
	if len(asked()) != 0 {
		t.Errorf("a refused option still reached the service: %v", asked())
	}
}

// The version verbs have no tracker answer, so they refuse by name without
// asking: an empty report or a zero level would read as a real answer.
func TestVersionVerbsRefuseByNameWithoutARequest(t *testing.T) {
	t.Parallel()
	c, asked := closeServer(t, nil)
	if _, err := c.Version(); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "Version") {
		t.Errorf("Version = %v, want ErrUnsupported naming the verb", err)
	}
	if _, err := c.SchemaLevel(); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "SchemaLevel") {
		t.Errorf("SchemaLevel = %v, want ErrUnsupported naming the verb", err)
	}
	if len(asked()) != 0 {
		t.Errorf("a refused verb still reached the service: %v", asked())
	}
}

// serve returns a Client whose service answers every request with handler.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	tc, err := client.New(client.Config{BaseURL: "http://127.0.0.1:1", Token: "t", SchemaLevel: api.SchemaLevel, MaxRetries: -1, HTTP: &http.Client{Transport: roundTrip(handler)}})
	if err != nil {
		t.Fatal(err)
	}
	return New(tc, "gt")
}

// ShowText renders the issue's id and title from one Show, as the shared
// contract requires of every store (gt-trackershow).
func TestShowTextRendersTheIssueLine(t *testing.T) {
	t.Parallel()
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gt-x","title":"showable","priority":1,"status":"open"}`))
	})
	text, err := c.ShowText("gt-x", []string{"gt-x"})
	if err != nil {
		t.Fatalf("ShowText: %v", err)
	}
	if !strings.Contains(text, "gt-x") || !strings.Contains(text, "showable") || !strings.Contains(text, "P1") {
		t.Errorf("ShowText = %q, want the id, title and priority", text)
	}
}

func TestEventsTailMapsOpsStatusAndPaging(t *testing.T) {
	t.Parallel()
	status := func(s string) json.RawMessage { return json.RawMessage(`{"status":"` + s + `"}`) }
	all := []api.Event{
		{Seq: 5, Op: "created", IssueID: "gt-a", Actor: "me", Payload: status("open")},
		{Seq: 6, Op: "closed", IssueID: "gt-a", Payload: status("closed")},
		{Seq: 7, Op: "reopened", IssueID: "gt-a", Payload: status("open")},
		{Seq: 8, Op: "label_added", IssueID: "gt-a", Payload: status("open")},
		{Seq: 9, Op: "deleted", IssueID: "gt-a"},
	}
	var asked []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RawQuery)
		_ = json.NewEncoder(w).Encode(api.EventsPage{Events: all, NextAfter: 9, FirstSeq: 5})
	})
	page, err := c.EventsTail(4, 0)
	if err != nil || page.More || page.NextSince != 9 {
		t.Fatalf("EventsTail(4, 0) = %+v, %v; want next 9 and no more", page, err)
	}
	var got []string
	for _, r := range page.Records {
		got = append(got, r.Op+":"+r.Status)
	}
	if want := "create:open close:closed update:open update:open delete:"; strings.Join(got, " ") != want {
		t.Errorf("records = %q, want %q (a reopen and a label are updates; a delete has no status)", strings.Join(got, " "), want)
	}
	two, err := c.EventsTail(4, 2)
	if err != nil || len(two.Records) != 2 || !two.More || two.NextSince != 6 {
		t.Errorf("EventsTail(4, 2) = %+v, %v; want two records, more, next 6", two, err)
	}
	if asked[1] != "after=4&limit=3" {
		t.Errorf("a limit of 2 asked %q, want one more than wanted (limit=3) so More is known", asked[1])
	}
	empty := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.EventsPage{NextAfter: 9, FirstSeq: 5})
	})
	if p, err := empty.EventsTail(9, 0); err != nil || len(p.Records) != 0 || p.NextSince != 9 || p.More {
		t.Errorf("nothing new = %+v, %v; want the same cursor back", p, err)
	}
}

func TestEventsTailReportsAPrunedPast(t *testing.T) {
	t.Parallel()
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.EventsPage{NextAfter: 90, FirstSeq: 50})
	})
	var te *beads.EventsTruncatedError
	if _, err := c.EventsTail(10, 0); !errors.As(err, &te) || te.Since != 10 || te.Floor != 50 {
		t.Errorf("EventsTail below the oldest retained = %v, want *EventsTruncatedError{Since:10, Floor:50}", err)
	}
	if _, err := c.EventsTail(49, 0); err != nil {
		t.Errorf("EventsTail(49) with the oldest record 50 = %v, want none: nothing is missing", err)
	}
}

func TestConfigIsNamespacedByRigAndIssuePrefixIsTheRig(t *testing.T) {
	t.Parallel()
	var keys []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"value":"v","set":true}`))
	})
	if v, err := c.ConfigGet("issue_prefix"); err != nil || v != "gt" {
		t.Errorf("ConfigGet(issue_prefix) = %q, %v; want the rig", v, err)
	}
	if v, err := c.ConfigGet("types.custom"); err != nil || v != "v" {
		t.Errorf("ConfigGet = %q, %v", v, err)
	}
	_ = c.ConfigSet("types.custom", "x")
	if len(keys) != 2 || keys[0] != "GET /v1/config/rig.gt.types.custom" || keys[1] != "PUT /v1/config/rig.gt.types.custom" {
		t.Errorf("requests = %v, want the key under rig.gt. and issue_prefix answered without a request", keys)
	}
}

func TestStatsJSONAndCount(t *testing.T) {
	t.Parallel()
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("rig") != "gt" {
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(api.Stats{Total: 6, ByStatus: map[string]int{"open": 3, "in_progress": 1, "closed": 2}})
	})
	raw, err := c.StatsJSON()
	var st struct {
		Summary map[string]int `json:"summary"`
	}
	if err != nil || json.Unmarshal(raw, &st) != nil || st.Summary["total_issues"] != 6 || st.Summary["open_issues"] != 3 || st.Summary["closed_issues"] != 2 || st.Summary["in_progress_issues"] != 1 {
		t.Errorf("StatsJSON = %s, %v; want the summary counts", raw, err)
	}
	if n, err := c.CountIssues(); err != nil || n != 6 {
		t.Errorf("CountIssues = %d, %v; want 6", n, err)
	}
}

// TestReadyFilteredCarriesTheFilterAndSignalsACutPage pins the adapter's side
// of the Client contract (gt-p85pw.7): the caller's filters cross the wire, and
// a positive limit whose page has a next cursor comes back with
// beads.ErrReadyTruncated — the tracker's way of saying more rows remain.
func TestReadyFilteredCarriesTheFilterAndSignalsACutPage(t *testing.T) {
	t.Parallel()
	var filters []api.ReadyFilter
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var f api.ReadyFilter
		_ = json.NewDecoder(r.Body).Decode(&f)
		filters = append(filters, f)
		_ = json.NewEncoder(w).Encode(api.IssuePage{
			Issues:     []api.Issue{{ID: "gt-a"}, {ID: "gt-b"}},
			NextCursor: "next",
		})
	})
	got, err := c.ReadyFiltered(beads.ReadyOptions{
		Unassigned:    true,
		ExcludeLabels: []string{"gt:skipped"},
		ExcludeTypes:  []string{"epic"},
		Limit:         2,
	})
	var capped *beads.ErrReadyTruncated
	if !errors.As(err, &capped) {
		t.Fatalf("ReadyFiltered(limit 2) err = %v, want beads.ErrReadyTruncated for a page with a next cursor", err)
	}
	if len(got) != 2 || capped.Cap != 2 || capped.TrueCount <= capped.Found {
		t.Errorf("ReadyFiltered = %d issues, sentinel %+v; want the two-row page and more behind it", len(got), *capped)
	}
	if len(filters) != 1 {
		t.Fatalf("requests = %d, want one ready page", len(filters))
	}
	f := filters[0]
	if !f.Unassigned || f.Limit != 2 || strings.Join(f.ExcludeLabels, ",") != "gt:skipped" || strings.Join(f.ExcludeTypes, ",") != "epic" {
		t.Errorf("filter = %+v, want the caller's filters carried across", f)
	}
}

// Decision A (gt-rtxfy): the tracker's key-value store is empty and answers
// every key as unset, which the admin contract requires of a store that holds
// nothing. This test pins that decision. It starts no tracker server.
func TestTrackerKVIsEmptyAndAnswersUnset(t *testing.T) {
	t.Parallel()
	c := &Client{}
	kvs, err := c.KVList()
	if err != nil || len(kvs) != 0 {
		t.Errorf("KVList = %v, %v; want an empty map and no error", kvs, err)
	}
	if _, err := c.KVGet("kv.contract_probe"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("KVGet(unset) = %v, want beads.ErrNotFound", err)
	}
}

// A missing issue is an error from ShowText, not an empty render (gt-trackershow).
func TestShowTextMissingIssueIsAnError(t *testing.T) {
	t.Parallel()
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such issue"}}`))
	})
	text, err := c.ShowText("gt-missing", []string{"gt-missing"})
	if err == nil || text != "" {
		t.Errorf("ShowText of a missing issue = %q, %v; want \"\" and an error", text, err)
	}
}

// A bd flag passed to ShowText is refused before any request (gt-rtxfy).
func TestShowTextRefusesAFlagWithoutARequest(t *testing.T) {
	t.Parallel()
	var asked int
	c := serve(t, func(http.ResponseWriter, *http.Request) { asked++ })
	if _, err := c.ShowText("gt-x", []string{"gt-x", "--json"}); err == nil {
		t.Error("ShowText with --json returned no error")
	}
	if asked != 0 {
		t.Errorf("ShowText with a flag sent %d requests, want none", asked)
	}
}

// DepListUp answers from one Show of the issue: the dependents the tracker
// carries, narrowed to one relation when depType names one.
func TestDepListUpReadsTheDependentsOfOneShowFilteredByRelation(t *testing.T) {
	t.Parallel()
	// The handler runs on the goroutine that sends the request, so the count is
	// atomic: a plain int would be a data race the moment a request moves.
	var asked atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gt-x","title":"target","status":"open","priority":2,"dependents":[` +
			`{"id":"gt-b","title":"blocked","status":"open","priority":2,"issue_type":"task","dependency_type":"blocks"},` +
			`{"id":"gt-k","title":"kid","status":"open","priority":2,"issue_type":"task","dependency_type":"parent-child"}]}`))
	})
	any, err := c.DepListUp("gt-x", "")
	if err != nil || len(any) != 2 || any[0].ID != "gt-b" || any[0].DependencyType != "blocks" || any[1].ID != "gt-k" || any[1].DependencyType != "parent-child" {
		t.Errorf("DepListUp(any) = %+v, %v; want gt-b blocks and gt-k parent-child", any, err)
	}
	blocks, err := c.DepListUp("gt-x", "blocks")
	if err != nil || len(blocks) != 1 || blocks[0].ID != "gt-b" {
		t.Errorf("DepListUp(blocks) = %+v, %v; want only gt-b", blocks, err)
	}
	if n := asked.Load(); n != 2 {
		t.Errorf("DepListUp sent %d requests for two reads, want one Show each", n)
	}
}

// A wisp is an issue on the wire, so a wisp that depends on an issue is one of
// its dependents and must come through the Show mapping with its relation, for
// Show and for DepListUp alike.
func TestWispDependentsSurviveTheShowMapping(t *testing.T) {
	t.Parallel()
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gt-x","title":"target","status":"open","priority":2,"dependents":[` +
			`{"id":"gt-wisp-w1","title":"wisp","status":"open","priority":2,"issue_type":"task","dependency_type":"blocks"}]}`))
	})
	is, err := c.Show("gt-x")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(is.Dependents) != 1 || is.Dependents[0].ID != "gt-wisp-w1" || is.Dependents[0].DependencyType != "blocks" {
		t.Errorf("Show(gt-x).Dependents = %+v, want the wisp gt-wisp-w1 with relation blocks", is.Dependents)
	}
	wisps, err := c.DepListUp("gt-x", "blocks")
	if err != nil || len(wisps) != 1 || wisps[0].ID != "gt-wisp-w1" {
		t.Errorf("DepListUp(blocks) = %+v, %v; want the wisp gt-wisp-w1", wisps, err)
	}
}

// A missing issue is ErrNotFound, and a failed read is an error: neither may
// come back as an empty list, which a caller reads as "no dependents".
func TestDepListUpRefusesAMissingIssueAndAFailedRead(t *testing.T) {
	t.Parallel()
	missing := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"not_found","message":"no such issue"}`))
	})
	if deps, err := missing.DepListUp("gt-missing", ""); !errors.Is(err, beads.ErrNotFound) || deps != nil {
		t.Errorf("DepListUp of a missing issue = %+v, %v; want nil and ErrNotFound", deps, err)
	}
	down := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"kind":"unavailable","message":"database does not answer"}`))
	})
	if deps, err := down.DepListUp("gt-x", ""); err == nil || deps != nil {
		t.Errorf("DepListUp on a failed read = %+v, %v; want nil and an error", deps, err)
	}
}
