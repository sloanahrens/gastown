package beadsfake

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
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
		// The bd behind every store offers --if-status on bd update (the
		// guard the tick's --if-acceptance joined); a store that reads bd's
		// help answers so, and one with no bd behind it is not in this suite.
		offers, err := c.UpdateOffersFlag("--if-status")
		if err != nil {
			t.Fatalf("UpdateOffersFlag(--if-status): %v", err)
		}
		if !offers {
			t.Errorf("UpdateOffersFlag(--if-status) = false; want the bd behind the store to offer it")
		}
	})
}

// RunCostSamplesContract checks the cost-sample pair, WriteCostSample and
// ReadCostSamples, that every store with a samples table must share: a row
// read back carries the values it was written with, each kind is its own
// series, since and limit narrow the read newest first, and a write with no
// time or an unencodable reading, an unknown kind and a negative limit are
// each refused — the refused writes leaving no row behind.
//
// It is separate from RunAdminContract because the bd on PATH predates the
// samples table (beads be-1qqu): a store backed by that bd refuses the write by
// name and cannot answer the case, so only a store whose bd offers the verb
// runs it. beadsfake's Fake runs it in the unit tier.
func RunCostSamplesContract(t *testing.T, newClient func(t *testing.T) beads.Client) {
	t.Helper()
	t.Run("cost samples", func(t *testing.T) {
		t.Parallel()
		contractCostSamples(t, newScope(t, newClient(t)))
	})
}

// contractCostSamples pins the cost-sample pair; see RunCostSamplesContract.
// The database may be shared with other cases, so each read passes a since
// just before base, the time this case writes at.
func contractCostSamples(t *testing.T, s *scope) {
	ctx := context.Background()
	base := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	floor := base.Add(-time.Second)
	deepseek := beads.Sample{
		Kind: beads.SampleKindDeepSeekBalance, At: base,
		Total: ptr(18.25), Granted: ptr(5.0), ToppedUp: ptr(13.25), Note: "topped up",
	}
	first := beads.Sample{
		Kind: beads.SampleKindClaudePlan, At: base.Add(time.Minute),
		FiveHourPct: ptr(41.0), FiveHourResetsAt: ptr(base.Add(3 * time.Hour)),
		SevenDayPct: ptr(23.5), SevenDayResetsAt: ptr(base.Add(48 * time.Hour)),
		Note: "first reading",
	}
	second := beads.Sample{
		Kind: beads.SampleKindClaudePlan, At: base.Add(5 * time.Minute),
		FiveHourPct: ptr(47.0), FiveHourResetsAt: ptr(base.Add(3 * time.Hour)),
		SevenDayPct: ptr(24.5), SevenDayResetsAt: ptr(base.Add(48 * time.Hour)),
		Note: "second reading",
	}
	for _, w := range []struct {
		what   string
		sample beads.Sample
	}{
		{"deepseek_balance", deepseek},
		{"first claude_plan", first},
		{"second claude_plan", second},
	} {
		mustDo(t, "WriteCostSample("+w.what+")", s.WriteCostSample(ctx, w.sample))
	}

	// Each kind is its own series, the newest row listed first.
	claude, err := s.ReadCostSamples(ctx, beads.SampleKindClaudePlan, floor, 0)
	if err != nil {
		t.Fatalf("ReadCostSamples(claude_plan): %v", err)
	}
	if len(claude) != 2 {
		t.Fatalf("ReadCostSamples(claude_plan) = %v, want the two rows written here", claude)
	}
	if !claude[0].At.Equal(second.At) || !claude[1].At.Equal(first.At) {
		t.Errorf("claude_plan rows = %v, want newest first (%v then %v)", claude, second.At, first.At)
	}
	if !reflect.DeepEqual(claude[0], second) {
		t.Errorf("second claude_plan row = %+v, want the sample written: %+v", claude[0], second)
	}
	if !reflect.DeepEqual(claude[1], first) {
		t.Errorf("first claude_plan row = %+v, want the sample written: %+v", claude[1], first)
	}
	balance, err := s.ReadCostSamples(ctx, beads.SampleKindDeepSeekBalance, floor, 0)
	if err != nil || len(balance) != 1 || !reflect.DeepEqual(balance[0], deepseek) {
		t.Fatalf("ReadCostSamples(deepseek_balance) = %v, %v; want the one balance row written: %+v", balance, err, deepseek)
	}

	// A since between the two claude_plan rows keeps only the newer one, and
	// a limit caps the answer after the filter, not the series.
	between := first.At.Add(30 * time.Second)
	recent, err := s.ReadCostSamples(ctx, beads.SampleKindClaudePlan, between, 0)
	if err != nil || len(recent) != 1 || !recent[0].At.Equal(second.At) {
		t.Errorf("ReadCostSamples(claude_plan, since %v) = %v, %v; want only the newer row", between, recent, err)
	}
	newest, err := s.ReadCostSamples(ctx, beads.SampleKindClaudePlan, floor, 1)
	if err != nil || len(newest) != 1 || !newest[0].At.Equal(second.At) {
		t.Errorf("ReadCostSamples(claude_plan, limit 1) = %v, %v; want the newest row", newest, err)
	}

	// A refused write must not leave a row behind.
	if err := s.WriteCostSample(ctx, beads.Sample{Kind: "usage_record", At: base}); err == nil {
		t.Error("WriteCostSample(unknown kind) succeeded, want it refused")
	}
	if err := s.WriteCostSample(ctx, beads.Sample{Kind: beads.SampleKindClaudePlan}); err == nil {
		t.Error("WriteCostSample(no time) succeeded, want it refused")
	}
	nan := math.NaN()
	if err := s.WriteCostSample(ctx, beads.Sample{Kind: beads.SampleKindClaudePlan, At: base, FiveHourPct: &nan}); err == nil {
		t.Error("WriteCostSample(NaN reading) succeeded, want it refused: the payload cannot carry one")
	}
	if _, err := s.ReadCostSamples(ctx, "usage_record", base, 1); err == nil {
		t.Error("ReadCostSamples(unknown kind) succeeded, want it refused rather than an empty series")
	}
	if _, err := s.ReadCostSamples(ctx, beads.SampleKindClaudePlan, base, -1); err == nil {
		t.Error("ReadCostSamples(limit -1) succeeded, want it refused")
	}
	after, err := s.ReadCostSamples(ctx, beads.SampleKindClaudePlan, floor, 0)
	if err != nil || len(after) != 2 {
		t.Errorf("claude_plan rows after the refused writes = %v, %v; want the same two rows", after, err)
	}
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

// adminKV pins the kv store's write and reads against each other: a key set is
// what KVGet finds with the value set last and what KVList reports, and a key
// the store does not hold is ErrNotFound rather than an empty value a caller
// cannot tell from a stored "". The key sits outside bd's reserved "kv."
// namespace, which bd refuses to write, so the same case holds for the bd
// store as for the fake and the tracker.
func adminKV(t *testing.T, s *adminScope) {
	mustDo(t, "KVSet", s.KVSet("contract.probe", "first"))
	if got, err := s.KVGet("contract.probe"); err != nil || got != "first" {
		t.Errorf("KVGet after set = %q, %v; want first", got, err)
	}
	mustDo(t, "KVSet overwrite", s.KVSet("contract.probe", "second"))
	if got, err := s.KVGet("contract.probe"); err != nil || got != "second" {
		t.Errorf("KVGet after overwrite = %q, %v; want second", got, err)
	}
	all, err := s.KVList()
	if err != nil {
		t.Fatalf("KVList: %v", err)
	}
	if got, ok := all["contract.probe"]; !ok || got != "second" {
		t.Errorf("KVList[contract.probe] = %q, present %v; want second", got, ok)
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
	if _, err := s.KVGet("contract.unset"); !errors.Is(err, beads.ErrNotFound) {
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
