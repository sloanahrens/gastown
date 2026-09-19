package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

func TestChoosePoolAgent(t *testing.T) {
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 2, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}
	s := func(agent string, age time.Duration) poolSession {
		return poolSession{name: "gt-x", agent: agent, created: now.Add(-age)}
	}
	cases := []struct {
		name     string
		pool     *config.PolecatPool
		sessions []poolSession
		want     string
		wantLine string // exact reason; "" means only check it is non-empty
	}{
		{"no pool", nil, nil, "", "no polecat pool configured"},
		{"pool without local agent", &config.PolecatPool{MaxLocal: 2}, nil, "", "polecat_pool has no local_agent; using the role default"},
		{"pool with max 0", &config.PolecatPool{LocalAgent: "l", MaxLocal: 0}, nil, "", "polecat_pool max_local is 0; using the role default"},
		{"empty town -> local", pool, nil, "local-coder-polecat", "local seat 1/2 -> local-coder-polecat"},
		{"one local, old enough -> local", pool, []poolSession{s("local-coder-polecat", 10*time.Minute)}, "local-coder-polecat", "local seat 2/2 -> local-coder-polecat"},
		{"one local, too recent -> overflow", pool, []poolSession{s("local-coder-polecat", 90*time.Second)}, "deepseek-flash", "local stagger (last spawn 1m30s ago < 4m0s gap, 1/2) -> overflow deepseek-flash"},
		{"pool full -> overflow", pool, []poolSession{s("local-coder-polecat", time.Hour), s("local-coder-polecat", time.Hour)}, "deepseek-flash", "local full (2/2) -> overflow deepseek-flash"},
		{"flash sessions do not count", pool, []poolSession{s("deepseek-flash", time.Minute), s("deepseek-flash", time.Minute), s("deepseek-flash", time.Minute)}, "local-coder-polecat", ""},
		{"newest local decides the gap", &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}, []poolSession{s("local-coder-polecat", time.Hour), s("local-coder-polecat", time.Minute)}, "deepseek-flash", ""},
		{"oldest local alone would allow", &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}, []poolSession{s("local-coder-polecat", time.Hour)}, "local-coder-polecat", ""},
		{"no gap configured -> local while room", &config.PolecatPool{LocalAgent: "l", MaxLocal: 3}, []poolSession{s("l", time.Second)}, "l", ""},
		{"full with no overflow agent -> role default", &config.PolecatPool{LocalAgent: "l", MaxLocal: 1}, []poolSession{s("l", time.Hour)}, "", "local full (1/1) -> overflow role default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := choosePoolAgent(c.pool, c.sessions, now)
			if got != c.want {
				t.Errorf("agent = %q (%s), want %q", got, reason, c.want)
			}
			if reason == "" {
				t.Error("reason must never be empty")
			}
			if c.wantLine != "" && reason != c.wantLine {
				t.Errorf("reason = %q, want %q", reason, c.wantLine)
			}
		})
	}
}

// The seat-taken line and the overflow line must not read alike: telling them
// apart by one word ("local pool 2/2" vs "local pool full (2/2)") cost a flash
// seat on 2026-09-18. Both lines name the agent they picked.
func TestChoosePoolAgentLinesAreUnambiguous(t *testing.T) {
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 2, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}
	local := poolSession{name: "gt-a", agent: "local-coder-polecat", created: now.Add(-10 * time.Minute)}

	_, seatLine := choosePoolAgent(pool, []poolSession{local}, now)
	_, fullLine := choosePoolAgent(pool, []poolSession{local, {name: "gt-b", agent: "local-coder-polecat", created: now.Add(-time.Hour)}}, now)
	_, staggerLine := choosePoolAgent(pool, []poolSession{{name: "gt-c", agent: "local-coder-polecat", created: now.Add(-30 * time.Second)}}, now)

	if seatLine == fullLine || seatLine == staggerLine || fullLine == staggerLine {
		t.Fatalf("routing lines must be distinct:\n  seat:    %s\n  full:    %s\n  stagger: %s", seatLine, fullLine, staggerLine)
	}
	if strings.Contains(seatLine, "full") || strings.Contains(seatLine, "overflow") {
		t.Errorf("a taken local seat must not read as an overflow: %s", seatLine)
	}
	for _, line := range []string{seatLine, fullLine, staggerLine} {
		agent := "local-coder-polecat"
		if line != seatLine {
			agent = "deepseek-flash"
		}
		fields := strings.Fields(line)
		if fields[len(fields)-1] != agent {
			t.Errorf("routing line must end with the chosen agent %q: %s", agent, line)
		}
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
	if a, r := choosePoolAgent(pool, got, now); a != "local-coder-polecat" {
		t.Errorf("one local (marble, 1m ago) with a 30s gap and room for one more: want local, got %q (%s)", a, r)
	}
	pool.MinSpawnGap = "5m"
	if a, r := choosePoolAgent(pool, got, now); a != "deepseek-flash" {
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
	if a, r := resolvePolecatPoolAgent(townRoot); a != "" || r != "" {
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
	if a, _ := resolvePolecatPoolAgent(townRoot); a != "local-coder-polecat" {
		t.Errorf("empty town: got %q", a)
	}
	newPoolSessionLister = func() sessionLister {
		return &fakeLister{sessions: map[string]map[string]string{"gt-a": {"GT_ROLE": "gastown/polecats/a", "GT_AGENT": "local-coder-polecat"}}, created: map[string]time.Time{"gt-a": time.Now().Add(-time.Hour)}}
	}
	if a, _ := resolvePolecatPoolAgent(townRoot); a != "deepseek-flash" {
		t.Errorf("full pool: got %q", a)
	}
	newPoolSessionLister = func() sessionLister { return &fakeLister{err: errors.New("no server")} }
	if a, r := resolvePolecatPoolAgent(townRoot); a != "deepseek-flash" || !strings.Contains(r, "cannot list sessions") {
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
	if a, r := resolvePolecatPoolAgent(townRoot); a != "" || !strings.Contains(r, "max_local") {
		t.Errorf("misconfigured pool: got %q %q", a, r)
	}
}
