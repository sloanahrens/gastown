package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// TestChoosePoolAgent pins the whole routing table: the agent AND the exact
// one-line reason, because the line is the only thing a sling prints and
// "local pool 2/2" vs "local pool full (2/2)" used to read the same (gt-ipk7).
func TestChoosePoolAgent(t *testing.T) {
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}
	s := func(agent string, age time.Duration) poolSession {
		return poolSession{name: "gt-x", agent: agent, created: now.Add(-age)}
	}
	// One local seat taken 10m ago: room for more, and no stagger (the gap is 4m).
	busy := []poolSession{s("local-coder-polecat", 10*time.Minute)}
	// One local seat taken 1m ago: room, but inside the stagger gap.
	recent := []poolSession{s("local-coder-polecat", time.Minute)}
	// Every local seat taken.
	full := []poolSession{s("local-coder-polecat", 30*time.Minute), s("local-coder-polecat", 20*time.Minute), s("local-coder-polecat", 10*time.Minute)}

	cases := []struct {
		name     string
		pool     *config.PolecatPool
		bead     poolBead
		sessions []poolSession
		want     string
		wantWhy  string
	}{
		{"no pool", nil, poolBead{}, nil, "", "pool: no polecat pool configured"},
		{"pool without local agent", &config.PolecatPool{MaxLocal: 2}, poolBead{}, nil, "", "pool: polecat_pool has no local_agent; using the role default"},
		{"pool with max 0", &config.PolecatPool{LocalAgent: "l", MaxLocal: 0}, poolBead{}, nil, "", "pool: polecat_pool max_local is 0; using the role default"},

		{"label route:local wins over type", pool, poolBead{Type: "bug", Labels: []string{"route:local"}}, nil, "local-coder-polecat", "pool: local seat 1/3 -> local-coder-polecat (label route:local)"},
		{"label route:flash wins over type", pool, poolBead{Type: "task", Labels: []string{"route:flash"}}, nil, "deepseek-flash", "pool: overflow -> deepseek-flash (label route:flash)"},
		{"spent local attempt goes overflow", pool, poolBead{Type: "task", Labels: []string{"local-attempt:1"}}, nil, "deepseek-flash", "pool: overflow -> deepseek-flash (local-attempt:1 failed)"},
		{"task takes a free seat", pool, poolBead{Type: "task"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (type=task)"},
		{"rework beats the bead type", pool, poolBead{Type: "feature", Labels: []string{"rework"}}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (rework)"},
		{"bug overflows", pool, poolBead{Type: "bug"}, full, "deepseek-flash", "pool: overflow -> deepseek-flash (type=bug)"},
		{"feature inside the stagger overflows", pool, poolBead{Type: "feature"}, recent, "deepseek-flash", "pool: overflow -> deepseek-flash (type=feature)"},
		{"bug fills an idle seat", pool, poolBead{Type: "bug"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (idle-seat fill, local-attempt:1)"},

		{"task with no seat left", pool, poolBead{Type: "chore"}, full, "deepseek-flash", "pool: local full (3/3) -> deepseek-flash"},
		{"docs inside the stagger", pool, poolBead{Type: "docs"}, recent, "deepseek-flash", "pool: stagger 1m0s since last local spawn -> deepseek-flash"},
		{"unknown type follows the seat count", pool, poolBead{Type: "epic"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (type=epic)"},
		{"no bead shape at all", pool, poolBead{}, nil, "local-coder-polecat", "pool: local seat 1/3 -> local-coder-polecat (type=unknown)"},

		{"empty town -> local", pool, poolBead{}, nil, "local-coder-polecat", "pool: local seat 1/3 -> local-coder-polecat (type=unknown)"},
		{"one local, old enough -> local", pool, poolBead{Type: "task"}, []poolSession{s("local-coder-polecat", 10*time.Minute)}, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (type=task)"},
		{"flash sessions do not count", pool, poolBead{Type: "task"}, []poolSession{s("deepseek-flash", time.Minute), s("deepseek-flash", time.Minute)}, "local-coder-polecat", "pool: local seat 1/3 -> local-coder-polecat (type=task)"},
		{"no gap configured -> local while room", &config.PolecatPool{LocalAgent: "l", MaxLocal: 3}, poolBead{Type: "task"}, []poolSession{s("l", time.Second)}, "l", "pool: local seat 2/3 -> l (type=task)"},
		{"full with no overflow agent -> role default", &config.PolecatPool{LocalAgent: "l", MaxLocal: 1}, poolBead{Type: "task"}, []poolSession{s("l", time.Hour)}, "", "pool: local full (1/1) -> the role default"},
		// MaxLocal 1 with the seat taken: the idle-seat fill cannot fire, so a
		// bug bead falls through to the shape branch and names the empty
		// overflow_agent as the role default rather than printing "-> ".
		{"overflow with no overflow agent -> role default", &config.PolecatPool{LocalAgent: "l", MaxLocal: 1}, poolBead{Type: "bug"}, []poolSession{s("l", time.Hour)}, "", "pool: overflow -> the role default (type=bug)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := choosePoolAgent(c.pool, c.bead, c.sessions, now)
			if got != c.want {
				t.Errorf("agent = %q, want %q (reason: %s)", got, c.want, reason)
			}
			if reason != c.wantWhy {
				t.Errorf("reason = %q, want %q", reason, c.wantWhy)
			}
		})
	}
}

// A bead carrying route:flash still has to name its agent when the pool is
// full: the label, not the seat count, is why it went where it went.
func TestChoosePoolAgentRouteLabelVsFullPool(t *testing.T) {
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 2, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}
	full := []poolSession{
		{name: "gt-a", agent: "local-coder-polecat", created: now.Add(-time.Hour)},
		{name: "gt-b", agent: "local-coder-polecat", created: now.Add(-30 * time.Minute)},
	}
	agent, reason := choosePoolAgent(pool, poolBead{Type: "task", Labels: []string{"route:local"}}, full, now)
	if agent != "deepseek-flash" {
		t.Errorf("route:local with no seat free must overflow, got %q", agent)
	}
	if want := "pool: local full (2/2) -> deepseek-flash"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	// A spent local attempt outranks the bead's type but not an explicit label.
	agent, reason = choosePoolAgent(pool, poolBead{Type: "bug", Labels: []string{"local-attempt:1", "route:local"}}, nil, now)
	if agent != "local-coder-polecat" {
		t.Errorf("route:local beats local-attempt:1, got %q (%s)", agent, reason)
	}
}

type fakeLister struct {
	sessions map[string]map[string]string // name -> env
	created  map[string]time.Time
	err      error
}

func (f *fakeLister) ListSessions() ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for n := range f.sessions {
		out = append(out, n)
	}
	return out, nil
}
func (f *fakeLister) GetEnvironment(session, key string) (string, error) {
	v, ok := f.sessions[session][key]
	if !ok {
		return "", errors.New("unknown variable")
	}
	return v, nil
}
func (f *fakeLister) GetSessionCreatedTime(name string) (time.Time, error) {
	if c, ok := f.created[name]; ok {
		return c, nil
	}
	return time.Time{}, errors.New("no such session")
}

// Only polecat sessions count, identified by GT_ROLE; witnesses, refineries
// and dogs on the same server are ignored; a polecat without GT_AGENT is
// counted with an empty agent (so it never inflates the local count).
func TestListPolecatSessions(t *testing.T) {
	now := time.Now()
	f := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-marble":   {"GT_ROLE": "gastown/polecats/marble", "GT_AGENT": "local-coder-polecat"},
			"gt-slate":    {"GT_ROLE": "gastown/polecats/slate", "GT_AGENT": "deepseek-flash"},
			"gt-opal":     {"GT_ROLE": "gastown/polecats/opal"},
			"gt-witness":  {"GT_ROLE": "gastown/witness", "GT_AGENT": "local-coder-polecat"},
			"gt-refinery": {"GT_ROLE": "gastown/refinery", "GT_AGENT": "local-coder-polecat"},
			"hq-mayor":    {"GT_ROLE": "mayor"},
			"random":      {},
		},
		created: map[string]time.Time{"gt-marble": now.Add(-time.Minute), "gt-slate": now.Add(-time.Hour)},
	}
	got, err := listPolecatSessions(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d polecat sessions, want 3: %+v", len(got), got)
	}
	agents := map[string]string{}
	for _, s := range got {
		agents[s.name] = s.agent
	}
	if agents["gt-marble"] != "local-coder-polecat" || agents["gt-slate"] != "deepseek-flash" || agents["gt-opal"] != "" {
		t.Errorf("agents: %v", agents)
	}
	// A polecat whose creation time tmux cannot report counts as just spawned.
	for _, s := range got {
		if s.name == "gt-opal" && time.Since(s.created) > time.Minute {
			t.Errorf("unknown created time should read as now, got %v", s.created)
		}
	}
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 2, MinSpawnGap: "30s", OverflowAgent: "deepseek-flash"}
	if a, r := choosePoolAgent(pool, poolBead{}, got, now); a != "local-coder-polecat" {
		t.Errorf("one local (marble, 1m ago) with a 30s gap and room for one more: want local, got %q (%s)", a, r)
	}
	pool.MinSpawnGap = "5m"
	if a, r := choosePoolAgent(pool, poolBead{}, got, now); a != "deepseek-flash" {
		t.Errorf("marble 1m ago with a 5m gap: want overflow, got %q (%s)", a, r)
	}
}

// resolvePolecatPoolAgent reads the town settings; no pool -> "", "".
func TestResolvePolecatPoolAgent(t *testing.T) {
	townRoot := t.TempDir()
	ts := config.NewTownSettings()
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatal(err)
	}
	if a, r := resolvePolecatPoolAgent(townRoot, ""); a != "" || r != "" {
		t.Errorf("no pool: got %q %q", a, r)
	}
	ts.PolecatPool = &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 1, OverflowAgent: "deepseek-flash"}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatal(err)
	}
	orig := newPoolSessionLister
	t.Cleanup(func() { newPoolSessionLister = orig })
	newPoolSessionLister = func() sessionLister {
		return &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}}
	}
	if a, _ := resolvePolecatPoolAgent(townRoot, ""); a != "local-coder-polecat" {
		t.Errorf("empty town: got %q", a)
	}
	newPoolSessionLister = func() sessionLister {
		return &fakeLister{sessions: map[string]map[string]string{"gt-a": {"GT_ROLE": "gastown/polecats/a", "GT_AGENT": "local-coder-polecat"}}, created: map[string]time.Time{"gt-a": time.Now().Add(-time.Hour)}}
	}
	if a, _ := resolvePolecatPoolAgent(townRoot, ""); a != "deepseek-flash" {
		t.Errorf("full pool: got %q", a)
	}
	newPoolSessionLister = func() sessionLister { return &fakeLister{err: errors.New("no server")} }
	if a, r := resolvePolecatPoolAgent(townRoot, ""); a != "deepseek-flash" || !strings.Contains(r, "cannot list sessions") {
		t.Errorf("lister failure must fall back to overflow: got %q %q", a, r)
	}
	// Misconfigured pool (max_local 0) says so instead of "no pool".
	ts.PolecatPool = &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 0}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatal(err)
	}
	newPoolSessionLister = func() sessionLister {
		return &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}}
	}
	if a, r := resolvePolecatPoolAgent(townRoot, ""); a != "" || !strings.Contains(r, "max_local") {
		t.Errorf("misconfigured pool: got %q %q", a, r)
	}
}

// fakePoolTown wires a town whose pool has three local seats, one taken 10m
// ago (so a seat is free and the stagger gap has passed), plus recording fakes
// for the bead lookup and the label write. It returns the town root and a
// pointer to the recorded label writes.
func fakePoolTown(t *testing.T, bead poolBead, lookupErr, addErr error) (string, *[]string) {
	t.Helper()
	townRoot := t.TempDir()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatal(err)
	}
	origLister, origLookup, origAdd := newPoolSessionLister, poolBeadLookup, poolBeadLabelAdd
	t.Cleanup(func() { newPoolSessionLister, poolBeadLookup, poolBeadLabelAdd = origLister, origLookup, origAdd })

	now := time.Now()
	newPoolSessionLister = func() sessionLister {
		return &fakeLister{
			sessions: map[string]map[string]string{"gt-a": {"GT_ROLE": "gastown/polecats/a", "GT_AGENT": "local-coder-polecat"}},
			created:  map[string]time.Time{"gt-a": now.Add(-10 * time.Minute)},
		}
	}
	poolBeadLookup = func(_, _ string) (poolBead, error) {
		if lookupErr != nil {
			return poolBead{}, lookupErr
		}
		return bead, nil
	}
	added := &[]string{}
	poolBeadLabelAdd = func(_, beadID, label string) error {
		*added = append(*added, beadID+":"+label)
		return addErr
	}
	return townRoot, added
}

// The idle-seat fill is the one routing branch with a side effect: it attaches
// local-attempt:1 so the bead cannot take a second local seat. A dry run must
// report the same route without writing.
func TestResolvePoolAgentIdleFillLabelsTheBead(t *testing.T) {
	townRoot, added := fakePoolTown(t, poolBead{ID: "gt-b", Type: "bug"}, nil, nil)

	agent, reason := resolvePolecatPoolAgent(townRoot, "gt-b")
	want := "pool: local seat 2/3 -> local-coder-polecat (idle-seat fill, local-attempt:1)"
	if agent != "local-coder-polecat" || reason != want {
		t.Fatalf("got %q %q, want %q %q", agent, reason, "local-coder-polecat", want)
	}
	if len(*added) != 1 || (*added)[0] != "gt-b:"+localAttemptLabel {
		t.Errorf("label writes = %v, want [gt-b:%s]", *added, localAttemptLabel)
	}

	*added = nil
	peekAgent, peekReason := peekPolecatPoolAgent(townRoot, "gt-b")
	if peekAgent != agent || peekReason != reason {
		t.Errorf("peek = %q %q, want the same route as resolve", peekAgent, peekReason)
	}
	if len(*added) != 0 {
		t.Errorf("a dry run must not label the bead, got %v", *added)
	}
}

// A bead that did not go to the idle-seat fill branch is never labeled.
func TestResolvePoolAgentNoLabelForShapedRoute(t *testing.T) {
	townRoot, added := fakePoolTown(t, poolBead{ID: "gt-b", Type: "task"}, nil, nil)
	agent, reason := resolvePolecatPoolAgent(townRoot, "gt-b")
	if agent != "local-coder-polecat" || !strings.HasSuffix(reason, "(type=task)") {
		t.Fatalf("got %q %q", agent, reason)
	}
	if len(*added) != 0 {
		t.Errorf("a task bead must not get local-attempt:1: %v", *added)
	}
}

// A bead whose shape could not be read falls back to the seat count, and the
// reason says why rather than passing the seat-only decision off as a route.
func TestResolvePoolAgentBeadLookupFailure(t *testing.T) {
	townRoot, added := fakePoolTown(t, poolBead{}, errors.New("bd: database not found"), nil)
	agent, reason := resolvePolecatPoolAgent(townRoot, "gt-b")
	if agent != "local-coder-polecat" {
		t.Errorf("seat-only decision should still take the free seat, got %q (%s)", agent, reason)
	}
	if !strings.Contains(reason, "gt-b unreadable") || !strings.Contains(reason, "database not found") {
		t.Errorf("reason must say the bead was unreadable: %q", reason)
	}
	if len(*added) != 0 {
		t.Errorf("no shape, no fill rule, no label: %v", *added)
	}
}

// The label write failing must not swallow the route: the sling still names
// the agent, and says the label did not land.
func TestResolvePoolAgentLabelWriteFailure(t *testing.T) {
	townRoot, _ := fakePoolTown(t, poolBead{ID: "gt-b", Type: "bug"}, nil, errors.New("dolt is down"))
	agent, reason := resolvePolecatPoolAgent(townRoot, "gt-b")
	if agent != "local-coder-polecat" {
		t.Errorf("a failed label write must not change the route, got %q", agent)
	}
	if !strings.Contains(reason, idleFillReason) || !strings.Contains(reason, "local-attempt:1 label failed") {
		t.Errorf("reason should carry both the route and the failure: %q", reason)
	}
}

// ── Seat claims (gt-eoi9) ───────────────────────────────────────────────────

const (
	claimLocal    = "local-coder-polecat"
	claimOverflow = "deepseek-flash"
)

// fakeRacingPoolTown wires a town with local seats and, unless a gap is given,
// no stagger at all — so the seat count alone decides. The tmux server has not
// heard of any of the slings racing each other, which is the moment the cap
// used to break.
func fakeRacingPoolTown(t *testing.T, maxLocal int, minSpawnGap string, liveLocal int) string {
	t.Helper()
	townRoot := t.TempDir()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{
		LocalAgent:    claimLocal,
		MaxLocal:      maxLocal,
		MinSpawnGap:   minSpawnGap,
		OverflowAgent: claimOverflow,
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatal(err)
	}
	origLister, origLookup := newPoolSessionLister, poolBeadLookup
	origClaims := processPoolSeatClaims
	t.Cleanup(func() {
		newPoolSessionLister, poolBeadLookup = origLister, origLookup
		processPoolSeatClaims = origClaims
	})

	sessions := map[string]map[string]string{}
	created := map[string]time.Time{}
	for i := 0; i < liveLocal; i++ {
		name := fmt.Sprintf("gt-live-%d", i)
		sessions[name] = map[string]string{"GT_ROLE": "gastown/polecats/live", "GT_AGENT": claimLocal}
		created[name] = time.Now().Add(-time.Hour)
	}
	newPoolSessionLister = func() sessionLister { return &fakeLister{sessions: sessions, created: created} }
	poolBeadLookup = func(_, beadID string) (poolBead, error) {
		return poolBead{ID: beadID, Type: "task"}, nil
	}
	return townRoot
}

// slingFromAnotherProcess decides as a fresh `gt sling` would: an empty claim
// store of its own, while the claims other slings wrote are on disk.
func slingFromAnotherProcess(t *testing.T, townRoot, beadID string) (string, string) {
	t.Helper()
	processPoolSeatClaims = &poolSeatClaimStore{}
	return resolvePolecatPoolAgent(townRoot, beadID)
}

// The gt-eoi9 bug: three slings started in parallel each counted the same live
// sessions, so each read "local pool 1/2" and spawned locally, putting three
// polecats on a two-seat pool (opal, shale and agate all local). The seat claim
// the first sling leaves behind is what the second counts instead.
func TestPoolSeatClaimsHoldTheCapForConcurrentSlings(t *testing.T) {
	townRoot := fakeRacingPoolTown(t, 2, "", 0)

	a1, r1 := slingFromAnotherProcess(t, townRoot, "gt-a")
	a2, r2 := slingFromAnotherProcess(t, townRoot, "gt-b")
	a3, r3 := slingFromAnotherProcess(t, townRoot, "gt-c")

	if a1 != claimLocal || a2 != claimLocal {
		t.Fatalf("the two seats go to the first two slings: got %q (%s) and %q (%s)", a1, r1, a2, r2)
	}
	if !strings.Contains(r1, "local seat 1/2") || !strings.Contains(r2, "local seat 2/2") {
		t.Errorf("each sling must see the seat it took: %q, %q", r1, r2)
	}
	if a3 != claimOverflow || !strings.Contains(r3, "local full (2/2)") {
		t.Errorf("the third sling must overflow with the cap full, got %q (%s)", a3, r3)
	}
}

// A claim also moves the stagger clock: a second sling inside min_spawn_gap
// overflows even though the pool has room, which is the same prefill guard a
// second spawn seconds after the first would get from a live session.
func TestPoolSeatClaimFeedsTheStagger(t *testing.T) {
	townRoot := fakeRacingPoolTown(t, 3, "4m", 1)

	if a, r := slingFromAnotherProcess(t, townRoot, "gt-a"); a != claimLocal {
		t.Fatalf("one live local, one seat free, gap long past: want local, got %q (%s)", a, r)
	}
	a, r := slingFromAnotherProcess(t, townRoot, "gt-b")
	if a != claimOverflow || !strings.Contains(r, "stagger") {
		t.Errorf("a sling racing the spawn it cannot see yet is a stagger, got %q (%s)", a, r)
	}
}

// The claim lives exactly as long as the seat is invisible to tmux: StartSession
// drops it, and the live session counts instead — one seat either way, never
// both and never neither.
func TestPoolSeatClaimIsHandedOverToTheSession(t *testing.T) {
	townRoot := fakeRacingPoolTown(t, 2, "", 0)
	if a, r := slingFromAnotherProcess(t, townRoot, "gt-a"); a != claimLocal {
		t.Fatalf("empty pool: want the local seat, got %q (%s)", a, r)
	}
	if claims := readPoolSeatClaims(townRoot); len(claims) != 1 {
		t.Fatalf("a local route must claim a seat, got %d claims", len(claims))
	}

	// StartSession: the tmux session is now the record of that seat.
	releasePoolSeatClaim()
	if claims := readPoolSeatClaims(townRoot); len(claims) != 0 {
		t.Fatalf("StartSession must drop the claim, %d left", len(claims))
	}

	// A second sling in a fresh process counts the live session, not a phantom
	// claim: seat 2 of 2, which is what a double-count would call "full".
	newPoolSessionLister = func() sessionLister {
		return &fakeLister{
			sessions: map[string]map[string]string{"gt-a": {"GT_ROLE": "gastown/polecats/a", "GT_AGENT": claimLocal}},
			created:  map[string]time.Time{"gt-a": time.Now().Add(-time.Hour)},
		}
	}
	a, r := slingFromAnotherProcess(t, townRoot, "gt-b")
	if a != claimLocal || !strings.Contains(r, "local seat 2/2") {
		t.Errorf("the live session must count as one seat, got %q (%s)", a, r)
	}
}

// A dry run reads the claims — so it prints the route a real sling would take —
// but claims nothing and drops nothing.
func TestPeekPolecatPoolAgentNeitherClaimsNorDropsSeats(t *testing.T) {
	townRoot := fakeRacingPoolTown(t, 2, "", 0)

	if a, _ := peekPolecatPoolAgent(townRoot, "gt-a"); a != claimLocal {
		t.Fatalf("empty pool: the preview route should be the local seat, got %q", a)
	}
	if claims := readPoolSeatClaims(townRoot); len(claims) != 0 {
		t.Fatalf("a dry run must not claim a seat, got %d claims", len(claims))
	}

	if a, _ := slingFromAnotherProcess(t, townRoot, "gt-a"); a != claimLocal {
		t.Fatalf("live sling: want the local seat, got %q", a)
	}
	// Seen from another process, that claim is the second seat taken.
	processPoolSeatClaims = &poolSeatClaimStore{}
	if _, r := peekPolecatPoolAgent(townRoot, "gt-b"); !strings.Contains(r, "local seat 2/2") {
		t.Errorf("the preview must count the seat another sling claimed: %q", r)
	}
	if claims := readPoolSeatClaims(townRoot); len(claims) != 1 {
		t.Errorf("a dry run must not drop another sling's claim, %d left", len(claims))
	}
}

// A claim whose sling is gone cannot become a session, so the seat is free: a
// crashed sling must not shadow a seat for the TTL, and a claim held too long
// by a process that lingers must not shadow one forever.
func TestPoolSeatClaimCleanupDropsCrashedAndStaleSlingers(t *testing.T) {
	townRoot := fakeRacingPoolTown(t, 2, "", 0)
	crashed, err := publishPoolSeatClaim(townRoot, poolSeatClaim{
		ID: "crashed-1", PID: reapedPID(t), Agent: claimLocal, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := publishPoolSeatClaim(townRoot, poolSeatClaim{
		ID: "stale-1", PID: os.Getpid(), Agent: claimLocal, CreatedAt: time.Now().Add(-poolSeatClaimTTL - time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	held, err := publishPoolSeatClaim(townRoot, poolSeatClaim{
		ID: "held-1", PID: os.Getpid(), Agent: claimLocal, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	cleanupStalePoolSeatClaims(townRoot, time.Now())

	got := readPoolSeatClaims(townRoot)
	if len(got) != 1 || got[0].ID != held.ID {
		t.Fatalf("only the live, fresh claim survives, got %v (crashed=%s stale=%s held=%s)",
			got, crashed.ID, stale.ID, held.ID)
	}
}

// reapedPID returns a PID that is certainly not alive: a child that has exited
// and been waited for. processAlive reads it as a slinger that is gone.
func reapedPID(t *testing.T) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("reaping a child PID is unix-specific")
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot reap a child process: %v", err)
	}
	return cmd.Process.Pid
}
