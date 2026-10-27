package beadsfake

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// AdminClient is a database offering both surfaces the contracts pin.
type AdminClient interface {
	beads.Client
	beads.Admin
}

// RunAdminContract checks the maintenance surface (beads.Admin) every
// implementation must share. newImpl returns one database, whose issue
// prefix is "gt", for the whole run; the cases run one after another on it
// and assert on changes, not absolute counts, so they need not start empty.
// SQLCSV and InitDatabase are scripted or recorded by the fake, not
// modeled, so they are not pinned here.
func RunAdminContract(t *testing.T, newImpl func(t *testing.T) AdminClient) {
	t.Helper()
	c := newImpl(t)
	for _, tc := range adminCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, &adminScope{AdminClient: c, scope: newScope(t, c)})
		})
	}
}

type adminScope struct {
	AdminClient
	*scope
}

// RunVersionContract checks the version pair (Version and SchemaLevel) every
// store that serves a bd must share: the report names a fork build, and the
// database sits at the level that build migrates to — the invariant the
// startup handshake refuses a town over.
//
// It is separate from RunAdminContract because a store with no bd behind it
// refuses these verbs rather than answering a zero value a caller could read
// as an answer (see trackerclient.ErrUnsupported), so such a store does not
// run this suite. A new store runs it unless it can say why not.
func RunVersionContract(t *testing.T, newImpl func(t *testing.T) AdminClient) {
	t.Helper()
	t.Run("version", func(t *testing.T) {
		t.Parallel()
		c := newImpl(t)
		info, err := c.Version()
		if err != nil {
			t.Fatalf("Version: %v", err)
		}
		if info.Version == "" || info.BuildID == "" || info.ContractVersion == 0 {
			t.Errorf("Version = %+v; want a fork build's report naming version, build_id and contract_version", info)
		}
		level, err := c.SchemaLevel()
		if err != nil {
			t.Fatalf("SchemaLevel: %v", err)
		}
		if level <= 0 || level != info.DBSchemaVersion {
			t.Errorf("SchemaLevel = %d, Version.DBSchemaVersion = %d; want the database at the level bd migrates to", level, info.DBSchemaVersion)
		}
	})
}

var adminCases = []struct {
	name string
	run  func(t *testing.T, s *adminScope)
}{
	{"config", adminConfig},
	{"kv store", adminKV},
	{"counts and stats", adminCounts},
	{"wisp list", adminWispList},
	{"wisp gc candidates", adminWispGCCandidates},
	{"events journal", adminEventsJournal},
}

// adminEventsJournal pins EventsTail over the mutations an events-journal
// reader sees: a close is op close with status closed, and a reopen is an update
// whose status is no longer closed. The database is shared with the other
// cases, so it asserts only on its own issue's records.
func adminEventsJournal(t *testing.T, s *adminScope) {
	start, err := s.EventsTail(0, 0)
	if err != nil {
		t.Fatalf("EventsTail(0): %v", err)
	}
	head := start.NextSince
	is := s.mustCreate(t, beads.CreateOptions{Title: "journaled", Priority: -1})
	mustDo(t, "close", s.Close(is.ID))
	mustDo(t, "reopen", s.Update(is.ID, beads.UpdateOptions{Status: ptr("open")}))

	page, err := s.EventsTail(head, 0)
	if err != nil {
		t.Fatalf("EventsTail(%d): %v", head, err)
	}
	var got []string
	for _, r := range page.Records {
		if r.Seq <= head {
			t.Errorf("record seq %d is not after since %d", r.Seq, head)
		}
		if r.IssueID == is.ID {
			got = append(got, r.Op+":"+r.Status)
		}
	}
	if want := []string{"create:open", "close:closed", "update:open"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("journal for %s = %q, want %q", is.ID, got, want)
	}
	if page.More || page.NextSince <= head {
		t.Errorf("full read = next %d more %v, want next past %d and no more", page.NextSince, page.More, head)
	}
	if again, err := s.EventsTail(page.NextSince, 0); err != nil || len(again.Records) != 0 || again.NextSince != page.NextSince {
		t.Errorf("EventsTail(next) = %+v, %v; want nothing new and the same cursor", again, err)
	}
	one, err := s.EventsTail(head, 1)
	if err != nil || len(one.Records) != 1 || !one.More || one.NextSince != one.Records[0].Seq {
		t.Errorf("EventsTail(head, 1) = %+v, %v; want one record and more", one, err)
	}
}

func adminConfig(t *testing.T, s *adminScope) {
	if v, err := s.ConfigGet("custom.contract_probe"); err != nil || v != "" {
		t.Errorf("ConfigGet(unset) = %q, %v; want \"\", nil", v, err)
	}
	if v, err := s.ConfigGet("issue_prefix"); err != nil || v != "gt" {
		t.Errorf("ConfigGet(issue_prefix) = %q, %v; want gt", v, err)
	}
	mustDo(t, "ConfigSet", s.ConfigSet("types.custom", "agent,rig"))
	if v, err := s.ConfigGet("types.custom"); err != nil || v != "agent,rig" {
		t.Errorf("ConfigGet after set = %q, %v", v, err)
	}
	mustDo(t, "ConfigSet again", s.ConfigSet("types.custom", "agent"))
	if v, err := s.ConfigGet("types.custom"); err != nil || v != "agent" {
		t.Errorf("ConfigGet after overwrite = %q, %v", v, err)
	}
}

// adminKV pins the kv store's two answers against each other: every key KVList
// reports is a key KVGet finds with the same value, and a key the store does
// not hold is ErrNotFound rather than an empty value a caller cannot tell from
// a stored "". The databases this runs on hold no memories yet, so the list
// loop pins the read pair without seeding a store Admin cannot write.
func adminKV(t *testing.T, s *adminScope) {
	all, err := s.KVList()
	if err != nil {
		t.Fatalf("KVList: %v", err)
	}
	for k, want := range all {
		got, err := s.KVGet(k)
		if err != nil {
			t.Errorf("KVGet(%q), listed by KVList: %v", k, err)
			continue
		}
		if got != want {
			t.Errorf("KVGet(%q) = %q, KVList says %q", k, got, want)
		}
	}
	if _, err := s.KVGet("kv.contract_probe"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("KVGet(unset) = %v, want beads.ErrNotFound", err)
	}
}

type statsSummary struct {
	Total  int `json:"total_issues"`
	Open   int `json:"open_issues"`
	Closed int `json:"closed_issues"`
}

func stats(t *testing.T, s *adminScope) statsSummary {
	t.Helper()
	raw, err := s.StatsJSON()
	if err != nil {
		t.Fatalf("StatsJSON: %v", err)
	}
	var st struct {
		Summary statsSummary `json:"summary"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("StatsJSON is not the stats object: %v\n%s", err, raw)
	}
	return st.Summary
}

func count(t *testing.T, s *adminScope) int {
	t.Helper()
	n, err := s.CountIssues()
	if err != nil {
		t.Fatalf("CountIssues: %v", err)
	}
	return n
}

func adminCounts(t *testing.T, s *adminScope) {
	n0, st0 := count(t, s), stats(t, s)
	s.mustCreate(t, beads.CreateOptions{Title: "one", Priority: -1})
	two := s.mustCreate(t, beads.CreateOptions{Title: "two", Priority: -1})
	mustDo(t, "close", s.Close(two.ID))
	s.mustCreate(t, beads.CreateOptions{Title: "wisp", Priority: -1, Ephemeral: true})
	if n := count(t, s); n-n0 != 2 {
		t.Errorf("CountIssues grew by %d, want 2 (closed counted, wisp not)", n-n0)
	}
	st := stats(t, s)
	if d := (statsSummary{st.Total - st0.Total, st.Open - st0.Open, st.Closed - st0.Closed}); d != (statsSummary{2, 1, 1}) {
		t.Errorf("stats summary grew by %+v, want total 2 open 1 closed 1", d)
	}
}

func adminWispList(t *testing.T, s *adminScope) {
	open := s.mustCreate(t, beads.CreateOptions{Title: "open wisp", Priority: -1, Ephemeral: true})
	shut := s.mustCreate(t, beads.CreateOptions{Title: "closed wisp", Priority: -1, Ephemeral: true})
	mustDo(t, "close wisp", s.Close(shut.ID))
	s.mustCreate(t, beads.CreateOptions{Title: "durable", Priority: -1})
	got, err := s.MolWispList()
	s.want(t, "MolWispList", got, err, open.ID)
	for _, w := range s.mine(got) {
		if w.Status != "open" || !w.Ephemeral || w.Title != "open wisp" {
			t.Errorf("wisp = %+v", w)
		}
		if _, err := time.Parse(time.RFC3339, w.UpdatedAt); err != nil {
			t.Errorf("wisp updated_at %q is not RFC 3339: %v", w.UpdatedAt, err)
		}
	}
}

// adminWispGCCandidates pins which wisps age gc would delete: plain open
// ones past the threshold. Work in progress, closed wisps, wisps an open
// issue blocks, and durable issues are never candidates.
func adminWispGCCandidates(t *testing.T, s *adminScope) {
	idle := s.mustCreate(t, beads.CreateOptions{Title: "idle open wisp", Priority: -1, Ephemeral: true})
	busy := s.mustCreate(t, beads.CreateOptions{Title: "wisp in progress", Priority: -1, Ephemeral: true})
	mustDo(t, "start", s.Update(busy.ID, beads.UpdateOptions{Status: ptr("in_progress")}))
	shut := s.mustCreate(t, beads.CreateOptions{Title: "closed wisp", Priority: -1, Ephemeral: true})
	mustDo(t, "close", s.Close(shut.ID))
	durable := s.mustCreate(t, beads.CreateOptions{Title: "durable", Priority: -1})
	held := s.mustCreate(t, beads.CreateOptions{Title: "blocked wisp", Priority: -1, Ephemeral: true})
	mustDo(t, "block", s.AddDependency(held.ID, durable.ID))

	mine := func(got []string) []string {
		out := []string{}
		for _, id := range got {
			if s.created[id] {
				out = append(out, id)
			}
		}
		return sorted(out...)
	}
	got, err := s.WispGCCandidates(0)
	if err != nil {
		t.Fatalf("WispGCCandidates(0): %v", err)
	}
	if g := mine(got); len(g) != 1 || g[0] != idle.ID {
		t.Errorf("WispGCCandidates(0) = %v (of this case's), want only %s", g, idle.ID)
	}
	got, err = s.WispGCCandidates(24 * time.Hour)
	if err != nil {
		t.Fatalf("WispGCCandidates(24h): %v", err)
	}
	if g := mine(got); len(g) != 0 {
		t.Errorf("WispGCCandidates(24h) = %v, want none of this case's fresh wisps", g)
	}
	// A dry run deletes nothing.
	if _, err := s.Show(idle.ID); err != nil {
		t.Errorf("the dry run removed %s: %v", idle.ID, err)
	}
}
