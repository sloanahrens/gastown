package trackerclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	})
	if got.CreatedAt != "2026-10-01T09:00:00Z" || got.ClosedAt != "2026-10-01T10:00:00Z" {
		t.Errorf("times = %q / %q, want RFC3339 in UTC", got.CreatedAt, got.ClosedAt)
	}
	if got.BlockedByCount != 2 || len(got.Dependencies) != 1 || got.Dependencies[0].DependencyType != "blocks" || got.Dependencies[0].Title != "b" {
		t.Errorf("BlockedByCount = %d, Dependencies = %+v", got.BlockedByCount, got.Dependencies)
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

// serve returns a Client whose service answers every request with handler.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	tc, err := client.New(client.Config{BaseURL: "http://127.0.0.1:1", Token: "t", SchemaLevel: api.SchemaLevel, MaxRetries: -1, HTTP: &http.Client{Transport: roundTrip(handler)}})
	if err != nil {
		t.Fatal(err)
	}
	return New(tc, "gt")
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

func TestStatsJSONAndCountAndTables(t *testing.T) {
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
	if !c.TableExists("wisps") || !c.TableExists("issues") || c.TableExists("no_such_table") {
		t.Error("TableExists: wisps and issues have counterparts, no_such_table does not")
	}
}

func TestExportWritesDurableIssuesAndWispsOneJSONObjectPerLine(t *testing.T) {
	t.Parallel()
	var lists []bool // the Ephemeral flag of each list request
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/issues:list":
			var f api.ListFilter
			_ = json.Unmarshal(b, &f)
			lists = append(lists, f.Ephemeral)
			id := "gt-durable"
			if f.Ephemeral {
				id = "gt-wisp-1"
			}
			_ = json.NewEncoder(w).Encode(api.IssuePage{Issues: []api.Issue{{ID: id}}})
		case "/v1/issues:show-many":
			var req api.ShowManyRequest
			_ = json.Unmarshal(b, &req)
			out := map[string]api.Issue{}
			for _, id := range req.IDs {
				out[id] = api.Issue{ID: id, Title: "t " + id, Dependencies: []api.IssueDep{{ID: "gt-x", DependencyType: "blocks"}}}
			}
			_ = json.NewEncoder(w).Encode(api.ShowManyResponse{Issues: out})
		}
	})
	path := t.TempDir() + "/issues.jsonl"
	if err := c.Export(path); err != nil {
		t.Fatal(err)
	}
	if len(lists) != 2 || lists[0] || !lists[1] {
		t.Errorf("list requests asked ephemeral = %v, want durable issues then wisps", lists)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("export has %d lines, want 2:\n%s", len(lines), data)
	}
	var is beads.Issue
	if err := json.Unmarshal([]byte(lines[1]), &is); err != nil || is.ID != "gt-wisp-1" || len(is.Dependencies) != 1 {
		t.Errorf("second line = %q (%v), want the wisp with its dependencies", lines[1], err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d files after export, want only the export (no temp file left)", len(entries))
	}
}
