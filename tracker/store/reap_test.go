package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

var reapNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// wispAt is an ephemeral task wisp created at created, in the given status.
func wispAt(id, status string, created time.Time, labels ...string) api.Issue {
	return api.Issue{ID: id, Type: "task", Status: status, Ephemeral: true, CreatedAt: created, UpdatedAt: created, Labels: labels}
}

// closedAt is an ephemeral wisp closed at closed.
func closedAt(id string, closed time.Time) api.Issue {
	w := wispAt(id, "closed", closed.Add(-time.Hour))
	w.ClosedAt = &closed
	return w
}

// durableClosed is a closed durable task issue closed at closed.
func durableClosed(id string, closed time.Time) api.Issue {
	return api.Issue{ID: id, Type: "task", Status: "closed", CreatedAt: closed.Add(-time.Hour), ClosedAt: &closed}
}

// agentBead is a live agent bead naming hook as its hook bead.
func agentBead(id, hook string) api.Issue {
	return api.Issue{ID: id, Type: "agent", Status: "open", Ephemeral: true, HookBead: hook}
}

// closedAgentWisp is an expired-looking closed agent bead, the kind a purge
// must keep.
func closedAgentWisp(id string, closed time.Time) api.Issue {
	w := closedAt(id, closed)
	w.Type = "agent"
	return w
}

func TestClassifyWisp(t *testing.T) {
	ago := func(d time.Duration) time.Time { return reapNow.Add(-d) }
	stale := ago(25 * time.Hour)
	young := ago(23 * time.Hour)
	oldClosed := ago(169 * time.Hour)
	const staleReason = "reaper: stale wisp past max-age 24h0m0s"
	const purgeReason = "closed past purge-age 168h0m0s"
	const molReason = "reaper: parent molecule closed"

	cases := []struct {
		name   string
		facts  WispFacts
		want   Action
		reason string
	}{
		{"stale open wisp with no parent is closed", WispFacts{Wisp: wispAt("w", "open", stale)}, Close, staleReason},
		{"stale hooked wisp is closed", WispFacts{Wisp: wispAt("w", "hooked", stale)}, Close, staleReason},
		{"stale in_progress wisp is closed", WispFacts{Wisp: wispAt("w", "in_progress", stale)}, Close, staleReason},
		{"wisp younger than max-age is kept", WispFacts{Wisp: wispAt("w", "open", young)}, Keep, ""},
		{"stale wisp with an open parent is kept", WispFacts{Wisp: wispAt("w", "open", stale),
			Parents: []api.IssueDep{{ID: "p", Type: "task", Status: "open"}}}, Keep, ""},
		{"stale wisp with an in_progress parent is kept", WispFacts{Wisp: wispAt("w", "open", stale),
			Parents: []api.IssueDep{{ID: "p", Type: "task", Status: "in_progress"}}}, Keep, ""},
		{"young step of a closed molecule is closed at any age", WispFacts{Wisp: wispAt("w", "open", young),
			Parents: []api.IssueDep{{ID: "m", Type: "molecule", Status: "closed"}}}, Close, molReason},
		{"step of a closed molecule with another open parent is kept", WispFacts{Wisp: wispAt("w", "open", stale),
			Parents: []api.IssueDep{{ID: "m", Type: "molecule", Status: "closed"}, {ID: "p", Type: "task", Status: "open"}}}, Keep, ""},
		{"a closed task parent does not close the wisp", WispFacts{Wisp: wispAt("w", "open", young),
			Parents: []api.IssueDep{{ID: "p", Type: "task", Status: "closed"}}}, Keep, ""},
		{"merge request in flight is kept at any age", WispFacts{Wisp: wispAt("w", "open", ago(100*time.Hour), "gt:merge-request")}, Keep, ""},
		{"cleanup wisp tracking a merge within 24h is kept", WispFacts{Wisp: func() api.Issue {
			w := wispAt("w", "open", ago(100*time.Hour), "cleanup", "state:merge-requested")
			w.UpdatedAt = ago(time.Hour)
			return w
		}()}, Keep, ""},
		{"cleanup wisp whose merge state is older than 24h is closed", WispFacts{Wisp: func() api.Issue {
			w := wispAt("w", "open", ago(100*time.Hour), "cleanup", "state:merge-requested")
			w.UpdatedAt = ago(25 * time.Hour)
			return w
		}()}, Close, staleReason},
		{"wisp a live agent names is kept though stale", WispFacts{Wisp: wispAt("w", "open", stale), Referenced: true}, Keep, ""},
		{"closed wisp past purge-age is purged", WispFacts{Wisp: closedAt("w", oldClosed)}, Purge, purgeReason},
		{"closed wisp just inside purge-age is kept", WispFacts{Wisp: closedAt("w", ago(167*time.Hour))}, Keep, ""},
		{"closed wisp exactly at purge-age is kept", WispFacts{Wisp: closedAt("w", ago(168*time.Hour))}, Keep, ""},
		{"closed wisp a live agent names is kept", WispFacts{Wisp: closedAt("w", oldClosed), Referenced: true}, Keep, ""},
		{"agent bead by type is never reaped", WispFacts{Wisp: agentBead("w", "")}, Keep, ""},
		{"agent bead by gt:agent label is never purged", WispFacts{Wisp: func() api.Issue {
			w := closedAt("w", oldClosed)
			w.Labels = []string{"gt:agent"}
			return w
		}()}, Keep, ""},
		{"durable closed issue is not a wisp", WispFacts{Wisp: api.Issue{ID: "w", Type: "task", Status: "closed", CreatedAt: stale, ClosedAt: &oldClosed}}, Keep, ""},
		{"a deferred wisp is outside the policy", WispFacts{Wisp: wispAt("w", "deferred", stale)}, Keep, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.facts, reapNow, ReapPolicy{})
			if got.Action != c.want {
				t.Fatalf("action = %v (%q), want %v", got.Action, got.Reason, c.want)
			}
			if c.want != Keep && got.Reason != c.reason {
				t.Errorf("reason = %q, want %q", got.Reason, c.reason)
			}
		})
	}
}

func TestLiveAgentRefsNamesWhatALiveAgentHolds(t *testing.T) {
	cases := []struct {
		name   string
		agents []api.Issue
		want   []string
	}{
		{"hook bead field is a live reference", []api.Issue{agentBead("a", "w-hook")}, []string{"w-hook"}},
		{"active_mr and hook_bead in the description are live references", []api.Issue{
			{ID: "a", Type: "agent", Description: "agent_state: working\nactive_mr: w-mr\nhook_bead: w-desc\n"}}, []string{"w-desc", "w-mr"}},
		{"a nuked agent names nothing (agent_state field)", []api.Issue{
			{ID: "a", Type: "agent", AgentState: "nuked", HookBead: "w-hook"}}, nil},
		{"a nuked agent names nothing (agent_state in the description)", []api.Issue{
			{ID: "a", Type: "agent", Description: "agent_state: nuked\nhook_bead: w-hook\n"}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &memStore{issues: c.agents}
			refs, err := liveAgentRefs(context.Background(), s)
			if err != nil {
				t.Fatalf("liveAgentRefs: %v", err)
			}
			var got []string
			for id := range refs {
				got = append(got, id)
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("references = %v, want %v", got, c.want)
			}
		})
	}
}

// TestReapPassClosesStaleAndPurgesExpiredButKeepsProtectedWisps is the store
// check: an expired closed wisp is purged, and an agent bead, a wisp a live
// agent names, a young wisp and a durable issue are all kept. It also walks two
// pages of the open list, so no stale wisp is missed past the first page.
func TestReapPassClosesStaleAndPurgesExpiredButKeepsProtectedWisps(t *testing.T) {
	ago := func(d time.Duration) time.Time { return reapNow.Add(-d) }
	s := &memStore{now: reapNow, pageSize: 2, issues: []api.Issue{
		wispAt("w-stale-1", "open", ago(30*time.Hour)),
		wispAt("w-stale-2", "hooked", ago(30*time.Hour)),
		wispAt("w-stale-3", "open", ago(30*time.Hour)),
		closedAt("w-old", ago(200*time.Hour)),
		closedAt("w-free", ago(200*time.Hour)),
		closedAt("w-held", ago(200*time.Hour)),
		closedAt("w-young", ago(time.Hour)),
		closedAgentWisp("agent-w", ago(200*time.Hour)),
		durableClosed("d-old", ago(200*time.Hour)),
		agentBead("agent-live", "w-held"),
		{ID: "agent-nuked", Type: "agent", Ephemeral: true, Status: "open", Description: "agent_state: nuked\nhook_bead: w-free\n"},
	}}

	res, err := ReapPass(context.Background(), s, reapNow, ReapPolicy{})
	if err != nil {
		t.Fatalf("ReapPass: %v", err)
	}
	if want := (PassResult{Closed: 3, Purged: 2}); res != want {
		t.Errorf("pass = %+v, want %+v", res, want)
	}
	for _, id := range []string{"w-stale-1", "w-stale-2", "w-stale-3"} {
		if got := s.closed[id]; got != "reaper: stale wisp past max-age 24h0m0s" {
			t.Errorf("close reason for %s = %q, want the stale reason", id, got)
		}
	}
	if got := sortedCopy(s.deleted); !slices.Equal(got, []string{"w-free", "w-old"}) {
		t.Errorf("deleted = %v, want [w-free w-old]: the agent wisp, the held wisp, the young wisp and the durable issue are kept", got)
	}
}

func TestPurgeWispsDeletesOnlyWhatThePolicyPurges(t *testing.T) {
	ago := func(d time.Duration) time.Time { return reapNow.Add(-d) }
	labelled := closedAt("agent-w", ago(200*time.Hour))
	labelled.Labels = []string{"gt:agent"}
	s := &memStore{now: reapNow, pageSize: 2, issues: []api.Issue{
		closedAt("w-old", ago(200*time.Hour)),
		closedAt("w-young", ago(time.Hour)),
		closedAt("w-held", ago(200*time.Hour)),
		labelled,
		durableClosed("d-old", ago(200*time.Hour)),
		agentBead("agent-live", "w-held"),
	}}

	res, err := PurgeWisps(context.Background(), s, []string{"w-old", "agent-w", "w-held", "w-young", "d-old", "nope", "w-old"}, reapNow, ReapPolicy{})
	if err != nil {
		t.Fatalf("PurgeWisps: %v", err)
	}
	if !slices.Equal(res.Purged, []string{"w-old"}) {
		t.Errorf("purged = %v, want [w-old]", res.Purged)
	}
	wantKept := []api.KeptWisp{
		{ID: "agent-w", Reason: "agent beads are never reaped"},
		{ID: "w-held", Reason: "a live agent bead names it"},
		{ID: "w-young", Reason: "closed within purge-age"},
		{ID: "d-old", Reason: "a durable issue is not a wisp"},
		{ID: "nope", Reason: "not found"},
	}
	if fmt.Sprint(res.Kept) != fmt.Sprint(wantKept) {
		t.Errorf("kept = %v, want %v", res.Kept, wantKept)
	}
	if got := sortedCopy(s.deleted); !slices.Equal(got, []string{"w-old"}) {
		t.Errorf("deleted = %v, want [w-old]", got)
	}
}

func TestListAllRefusesACursorThatDoesNotAdvance(t *testing.T) {
	s := stuckCursorStore{}
	if _, err := listAll(context.Background(), s, api.ListFilter{}); err == nil {
		t.Fatal("listAll read a cursor that never advances without an error")
	}
}

// stuckCursorStore answers every page with the same cursor, as a broken read
// would.
type stuckCursorStore struct{ Store }

func (stuckCursorStore) ListIssues(context.Context, api.ListFilter) (api.IssuePage, error) {
	return api.IssuePage{Issues: []api.Issue{{ID: "x"}}, NextCursor: "same"}, nil
}

// memStore is the store the reaper tests run against. It filters the way the
// Postgres list does (ephemeral flag, infra types, statuses, labels and the age
// bounds) and pages by pageSize. Any method the tests do not use panics on the
// nil embedded Store, which a test reports as a failure.
type memStore struct {
	Store
	now      time.Time
	pageSize int
	issues   []api.Issue
	parents  map[string][]api.IssueDep
	closed   map[string]string
	deleted  []string
}

func (m *memStore) ListIssues(_ context.Context, f api.ListFilter) (api.IssuePage, error) {
	var matched []api.Issue
	for _, is := range m.issues {
		if !matchesList(is, f) {
			continue
		}
		matched = append(matched, is)
	}
	start := 0
	if f.Cursor != "" {
		start, _ = strconv.Atoi(f.Cursor)
	}
	size := len(matched)
	if m.pageSize > 0 {
		size = m.pageSize
	}
	if f.Limit > 0 {
		size = f.Limit
	}
	end := min(start+size, len(matched))
	page := api.IssuePage{Issues: matched[start:end]}
	if end < len(matched) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

func matchesList(is api.Issue, f api.ListFilter) bool {
	switch {
	case is.Ephemeral != f.Ephemeral:
		return false
	case !f.IncludeInfra && f.IssueType == "" && isInfraType(is.Type):
		return false
	case f.IssueType != "" && is.Type != f.IssueType:
		return false
	}
	switch {
	case len(f.Statuses) == 0:
		if is.Status == "closed" {
			return false
		}
	case slices.Contains(f.Statuses, "all"):
	case !slices.Contains(f.Statuses, is.Status):
		return false
	}
	for _, l := range f.Labels {
		if !slices.Contains(is.Labels, l) {
			return false
		}
	}
	if !f.ClosedBefore.IsZero() && (is.ClosedAt == nil || !is.ClosedAt.Before(f.ClosedBefore)) {
		return false
	}
	if !f.CreatedBefore.IsZero() && !is.CreatedAt.Before(f.CreatedBefore) {
		return false
	}
	return true
}

func (m *memStore) GetIssues(_ context.Context, ids []string) (map[string]api.Issue, error) {
	out := map[string]api.Issue{}
	for _, is := range m.issues {
		if slices.Contains(ids, is.ID) {
			out[is.ID] = is
		}
	}
	return out, nil
}

func (m *memStore) Dependencies(_ context.Context, id, _ string) ([]api.IssueDep, error) {
	return m.parents[id], nil
}

func (m *memStore) CloseIssues(_ context.Context, ids []string, reason string, _ bool) error {
	if m.closed == nil {
		m.closed = map[string]string{}
	}
	for i := range m.issues {
		if slices.Contains(ids, m.issues[i].ID) {
			m.issues[i].Status = "closed"
			at := m.now
			m.issues[i].ClosedAt = &at
			m.closed[m.issues[i].ID] = reason
		}
	}
	return nil
}

func (m *memStore) DeleteIssues(_ context.Context, ids []string) error {
	m.deleted = append(m.deleted, ids...)
	var kept []api.Issue
	for _, is := range m.issues {
		if !slices.Contains(ids, is.ID) {
			kept = append(kept, is)
		}
	}
	m.issues = kept
	return nil
}

func sortedCopy(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}
