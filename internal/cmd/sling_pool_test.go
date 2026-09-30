package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
)

// boolp takes the address of a bool for the polecat_pool knobs that are
// pointers so that unset and false stay distinguishable.
func boolp(b bool) *bool { return &b }

// TestChoosePoolAgent pins the whole routing table: the agent AND the exact
// one-line reason, because the line is the only thing a sling prints and
// "local pool 2/2" vs "local pool full (2/2)" used to read the same (gt-ipk7).
func TestChoosePoolAgent(t *testing.T) {
	t.Parallel()
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
	// The same pool with the overflow seat capped at two (gt-jzr1), and the
	// session sets that put the cap under pressure: both flash seats taken,
	// both taken with one free, and both taken behind a stagger.
	capped := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	uncapped := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}
	// The idle-seat fill switched off (gt-nn7n), on its own and behind the cap,
	// plus the same pool spelling the default out.
	fillOff := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", IdleFill: boolp(false)}
	fillOn := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", IdleFill: boolp(true)}
	cappedFillOff := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", MaxOverflow: 2, IdleFill: boolp(false)}
	cappedFull := []poolSession{
		s("local-coder-polecat", 30*time.Minute), s("local-coder-polecat", 20*time.Minute), s("local-coder-polecat", 10*time.Minute),
		s("deepseek-flash", 20*time.Minute), s("deepseek-flash", 10*time.Minute),
	}
	oneOverflowFree := []poolSession{
		s("local-coder-polecat", 30*time.Minute), s("local-coder-polecat", 20*time.Minute), s("local-coder-polecat", 10*time.Minute),
		s("deepseek-flash", 20*time.Minute),
	}
	overRecent := []poolSession{s("local-coder-polecat", time.Minute), s("deepseek-flash", 20*time.Minute), s("deepseek-flash", 10*time.Minute)}
	// A free local seat, no stagger, both flash seats taken: the one state
	// where the fill's own branch and the overflow cap meet.
	freeSeatBothFlash := []poolSession{s("local-coder-polecat", 10*time.Minute), s("deepseek-flash", 20*time.Minute), s("deepseek-flash", 10*time.Minute)}

	cases := []struct {
		name        string
		pool        *config.PolecatPool
		bead        poolBead
		sessions    []poolSession
		want        string
		wantWhy     string
		wantRefused bool
	}{
		{"no pool", nil, poolBead{}, nil, "", "pool: no polecat pool configured", false},
		{"pool without local agent", &config.PolecatPool{MaxLocal: 2}, poolBead{}, nil, "", "pool: polecat_pool has no local_agent; using the role default", false},
		{"pool with no local seats", &config.PolecatPool{LocalAgent: "l", MaxLocal: 0}, poolBead{}, nil, "", "pool: no local seats (max_local 0) -> the role default", false},

		{"label route:local wins over type", pool, poolBead{Type: "bug", Labels: []string{"route:local"}}, nil, "local-coder-polecat", "pool: local seat 1/3 -> local-coder-polecat (label route:local)", false},
		{"label route:flash wins over type", pool, poolBead{Type: "task", Labels: []string{"route:flash"}}, nil, "deepseek-flash", "pool: overflow -> deepseek-flash (label route:flash)", false},
		{"spent local attempt goes overflow", pool, poolBead{Type: "task", Labels: []string{"local-attempt:1"}}, nil, "deepseek-flash", "pool: overflow -> deepseek-flash (local-attempt:1 failed)", false},
		{"task takes a free seat", pool, poolBead{Type: "task"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (type=task)", false},
		{"rework beats the bead type", pool, poolBead{Type: "feature", Labels: []string{"rework"}}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (rework)", false},
		{"bug overflows", pool, poolBead{Type: "bug"}, full, "deepseek-flash", "pool: overflow -> deepseek-flash (type=bug)", false},
		{"feature inside the stagger overflows", pool, poolBead{Type: "feature"}, recent, "deepseek-flash", "pool: overflow -> deepseek-flash (type=feature)", false},
		{"bug fills an idle seat", pool, poolBead{Type: "bug"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (idle-seat fill, local-attempt:1)", false},

		{"task with no seat left", pool, poolBead{Type: "chore"}, full, "deepseek-flash", "pool: local full (3/3) -> deepseek-flash", false},
		{"docs inside the stagger", pool, poolBead{Type: "docs"}, recent, "deepseek-flash", "pool: stagger 1m0s since last local spawn -> deepseek-flash", false},
		{"unknown type follows the seat count", pool, poolBead{Type: "epic"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (type=epic)", false},
		{"no bead shape at all", pool, poolBead{}, nil, "local-coder-polecat", "pool: local seat 1/3 -> local-coder-polecat (type=unknown)", false},

		{"empty town -> local", pool, poolBead{}, nil, "local-coder-polecat", "pool: local seat 1/3 -> local-coder-polecat (type=unknown)", false},
		{"one local, old enough -> local", pool, poolBead{Type: "task"}, []poolSession{s("local-coder-polecat", 10*time.Minute)}, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (type=task)", false},
		{"flash sessions do not count", pool, poolBead{Type: "task"}, []poolSession{s("deepseek-flash", time.Minute), s("deepseek-flash", time.Minute)}, "local-coder-polecat", "pool: local seat 1/3 -> local-coder-polecat (type=task)", false},
		{"no gap configured -> local while room", &config.PolecatPool{LocalAgent: "l", MaxLocal: 3}, poolBead{Type: "task"}, []poolSession{s("l", time.Second)}, "l", "pool: local seat 2/3 -> l (type=task)", false},
		{"full with no overflow agent -> role default", &config.PolecatPool{LocalAgent: "l", MaxLocal: 1}, poolBead{Type: "task"}, []poolSession{s("l", time.Hour)}, "", "pool: local full (1/1) -> the role default", false},
		// MaxLocal 1 with the seat taken: the idle-seat fill cannot fire, so a
		// bug bead falls through to the shape branch and names the empty
		// overflow_agent as the role default rather than printing "-> ".
		{"overflow with no overflow agent -> role default", &config.PolecatPool{LocalAgent: "l", MaxLocal: 1}, poolBead{Type: "bug"}, []poolSession{s("l", time.Hour)}, "", "pool: overflow -> the role default (type=bug)", false},

		// The overflow cap (gt-jzr1). Two flash seats of two taken, so every
		// route that lands on the overflow agent has nowhere to go.
		{"capped overflow refuses a bug", capped, poolBead{Type: "bug"}, cappedFull, "deepseek-flash", "pool: overflow full (2/2) -> no seat (type=bug)", true},
		{"capped overflow refuses route:flash", capped, poolBead{Type: "task", Labels: []string{"route:flash"}}, cappedFull, "deepseek-flash", "pool: overflow full (2/2) -> no seat (label route:flash)", true},
		{"capped overflow refuses a spent local attempt", capped, poolBead{Type: "task", Labels: []string{localAttemptLabel}}, cappedFull, "deepseek-flash", "pool: overflow full (2/2) -> no seat (local-attempt:1 failed)", true},
		{"capped overflow refuses when local is full too", capped, poolBead{Type: "task"}, cappedFull, "deepseek-flash", "pool: overflow full (2/2) -> no seat (local full)", true},
		{"capped overflow refuses inside the stagger", capped, poolBead{Type: "docs"}, overRecent, "deepseek-flash", "pool: overflow full (2/2) -> no seat (stagger 1m0s since last local spawn)", true},
		{"one overflow seat free still routes there", capped, poolBead{Type: "bug"}, oneOverflowFree, "deepseek-flash", "pool: overflow -> deepseek-flash (type=bug)", false},
		{"max_overflow 0 leaves the overflow seat uncapped", uncapped, poolBead{Type: "bug"}, cappedFull, "deepseek-flash", "pool: overflow -> deepseek-flash (type=bug)", false},
		{"overflow cap without an overflow agent is off", &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MaxOverflow: 2}, poolBead{Type: "bug"}, cappedFull, "", "pool: overflow -> the role default (type=bug)", false},

		// The idle-seat fill knob (gt-nn7n). Off, an overflow-shaped bead goes
		// to the overflow agent even with a seat free, and the reason names the
		// knob so the empty seat is not read as the bead's shape overflowing.
		{"fill off: a bug with a free seat overflows", fillOff, poolBead{Type: "bug"}, busy, "deepseek-flash", "pool: overflow -> deepseek-flash (type=bug, idle_fill off)", false},
		{"fill off: a feature in an empty town overflows", fillOff, poolBead{Type: "feature"}, nil, "deepseek-flash", "pool: overflow -> deepseek-flash (type=feature, idle_fill off)", false},
		{"fill off: a full local pool reads the same as before", fillOff, poolBead{Type: "bug"}, full, "deepseek-flash", "pool: overflow -> deepseek-flash (type=bug)", false},
		{"fill off: a task still takes the free seat", fillOff, poolBead{Type: "task"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (type=task)", false},
		{"fill off: an unknown shape still follows the seat count", fillOff, poolBead{Type: "epic"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (type=epic)", false},
		{"fill off: route:local still wins over the switch", fillOff, poolBead{Type: "bug", Labels: []string{routeLocalLabel}}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (label route:local)", false},
		{"fill off: a capped overflow seat refuses the freed bead", cappedFillOff, poolBead{Type: "bug"}, freeSeatBothFlash, "deepseek-flash", "pool: overflow full (2/2) -> no seat (type=bug, idle_fill off)", true},
		{"fill on: the knob spelling out the default changes nothing", fillOn, poolBead{Type: "bug"}, busy, "local-coder-polecat", "pool: local seat 2/3 -> local-coder-polecat (idle-seat fill, local-attempt:1)", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason, refused := choosePoolAgent(c.pool, c.bead, "", c.sessions, now)
			if got != c.want {
				t.Errorf("agent = %q, want %q (reason: %s)", got, c.want, reason)
			}
			if reason != c.wantWhy {
				t.Errorf("reason = %q, want %q", reason, c.wantWhy)
			}
			if refused != c.wantRefused {
				t.Errorf("refused = %v, want %v (reason: %s)", refused, c.wantRefused, reason)
			}
		})
	}
}

// An explicit route:* label outranks --agent (gt-4lbz), so the line has to say
// the label did it: 'gt sling <bead> --agent claude-sonnet' on a route:local
// bead was refused as "local full" with no word about the label (gt-sisll).
func TestChoosePoolAgentRouteLabelNamedInRefusalAndOverride(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 1, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", MaxOverflow: 1}
	full := []poolSession{
		{name: "gt-a", agent: "local-coder-polecat", created: now.Add(-time.Hour)},
		{name: "gt-b", agent: "deepseek-flash", created: now.Add(-30 * time.Minute)},
	}
	local := poolBead{Type: "task", Labels: []string{routeLocalLabel}}
	_, reason, refused := choosePoolAgent(pool, local, "claude-sonnet", full, now)
	want := "pool: overflow full (1/1) -> no seat (local full, label route:local) (label route:local outranks requested claude-sonnet)"
	if !refused || reason != want {
		t.Errorf("reason = %q refused=%v, want %q refused", reason, refused, want)
	}
	// No request, no override note: the refusal still names the label.
	_, reason, refused = choosePoolAgent(pool, local, "", full, now)
	if want := "pool: overflow full (1/1) -> no seat (local full, label route:local)"; !refused || reason != want {
		t.Errorf("reason = %q refused=%v, want %q refused", reason, refused, want)
	}
	// A request the label agrees with needs no note.
	_, reason, _ = choosePoolAgent(pool, local, "local-coder-polecat", nil, now)
	if want := "pool: local seat 1/1 -> local-coder-polecat (label route:local)"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	// route:flash overriding a request names itself too.
	flash := poolBead{Type: "task", Labels: []string{routeFlashLabel}}
	_, reason, _ = choosePoolAgent(pool, flash, "claude-sonnet", nil, now)
	if want := "pool: overflow -> deepseek-flash (label route:flash) (label route:flash outranks requested claude-sonnet)"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

// A bead carrying route:flash still has to name its agent when the pool is
// full: the label, not the seat count, is why it went where it went.
func TestChoosePoolAgentRouteLabelVsFullPool(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 2, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}
	full := []poolSession{
		{name: "gt-a", agent: "local-coder-polecat", created: now.Add(-time.Hour)},
		{name: "gt-b", agent: "local-coder-polecat", created: now.Add(-30 * time.Minute)},
	}
	agent, reason, refused := choosePoolAgent(pool, poolBead{Type: "task", Labels: []string{"route:local"}}, "", full, now)
	if agent != "deepseek-flash" || refused {
		t.Errorf("route:local with no seat free must overflow, got %q (refused=%v)", agent, refused)
	}
	if want := "pool: local full (2/2) -> deepseek-flash"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	// A spent local attempt outranks the bead's type but not an explicit label.
	agent, reason, refused = choosePoolAgent(pool, poolBead{Type: "bug", Labels: []string{"local-attempt:1", "route:local"}}, "", nil, now)
	if agent != "local-coder-polecat" || refused {
		t.Errorf("route:local beats local-attempt:1, got %q (refused=%v, %s)", agent, refused, reason)
	}
}

// A caller-named agent is a request for a seat, not a licence to take one
// (gt-4lbz). The agent a convoy recorded at sling time and the agent the deacon
// escalates to both arrive as --agent, and both spawned past a full pool while
// the pool was consulted only when --agent was empty.
func TestChoosePoolAgentRequestedSeatIsAdmittedByItsCap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	capped := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 2, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	sess := func(agent string, age time.Duration) poolSession {
		return poolSession{name: agent + "-" + age.String(), agent: agent, created: now.Add(-age)}
	}
	allFull := []poolSession{
		sess("local-coder-polecat", 30*time.Minute), sess("local-coder-polecat", 20*time.Minute),
		sess("deepseek-flash", 30*time.Minute), sess("deepseek-flash", 20*time.Minute),
	}

	// The seat requested is the seat counted: a capped overflow seat refuses
	// the very agent the request named.
	if a, r, refused := choosePoolAgent(capped, poolBead{Type: "bug"}, "deepseek-flash", allFull, now); !refused || a != "deepseek-flash" {
		t.Errorf("a requested overflow seat at its cap must refuse, got %q refused=%v (%s)", a, refused, r)
	}
	// Every seat full: no seat for the bead, whatever the caller asked for.
	if a, r, refused := choosePoolAgent(capped, poolBead{Type: "bug"}, "local-coder-polecat", allFull, now); !refused || !strings.Contains(r, "local full (2/2)") {
		t.Errorf("a requested local seat in a full town must refuse, got %q refused=%v (%s)", a, refused, r)
	}
	// A free local seat is the request honoured, and the reason says the agent
	// was requested rather than routed.
	if a, r, refused := choosePoolAgent(capped, poolBead{Type: "bug"}, "local-coder-polecat", nil, now); a != "local-coder-polecat" || refused || !strings.Contains(r, "requested") {
		t.Errorf("a requested local seat with room must be taken, got %q refused=%v (%s)", a, refused, r)
	}
	// An agent the pool does not own is not the pool's to admit.
	if a, r, refused := choosePoolAgent(capped, poolBead{Type: "bug"}, "claude", allFull, now); a != "" || r != "" || refused {
		t.Errorf("an agent outside the pool must leave it without an opinion, got %q %q refused=%v", a, r, refused)
	}
	// The bead's own route:* label outranks the recorded agent: the recorded
	// agent is a default, not an override (gt-4lbz).
	if a, _, refused := choosePoolAgent(capped, poolBead{Type: "bug", Labels: []string{routeFlashLabel}}, "local-coder-polecat", nil, now); a != "deepseek-flash" || refused {
		t.Errorf("route:flash must beat a requested local seat, got %q refused=%v", a, refused)
	}
}

// A request the pool can serve is served, and one it cannot serve is refused:
// the pool never answers a request for one seat with the other one, because the
// overflow seat is the paid provider and moving a bead onto it is a spend
// decision the caller did not make (gt-x40u). `gt sling <bead> <rig> --agent
// local-coder-polecat` on a taken local seat spawned a flash polecat instead of
// refusing the sling.
func TestChoosePoolAgentRequestedSeatIsHonoredOrRefused(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 2, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	sess := func(agent string, age time.Duration) poolSession {
		return poolSession{name: agent + "-" + age.String(), agent: agent, created: now.Add(-age)}
	}
	// The state the sling above hit: every local seat taken, the overflow seat
	// free.
	overflowFree := []poolSession{
		sess("local-coder-polecat", 30*time.Minute), sess("local-coder-polecat", 20*time.Minute),
		sess("deepseek-flash", 20*time.Minute),
	}

	// Local full, overflow free: refused, and the line names the seat that was
	// asked for, not the one the pool would have picked for itself.
	a, r, refused := choosePoolAgent(pool, poolBead{Type: "bug"}, "local-coder-polecat", overflowFree, now)
	if !refused || a != "local-coder-polecat" {
		t.Errorf("a requested local seat with no room must refuse, got %q refused=%v (%s)", a, refused, r)
	}
	if want := "pool: local full (2/2) -> no seat (requested local-coder-polecat)"; r != want {
		t.Errorf("reason = %q, want %q", r, want)
	}
	// A closed local seat cannot serve the request either, and the refusal says
	// which rule closed it.
	noLocal := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 0, OverflowAgent: "deepseek-flash"}
	if a, r, refused := choosePoolAgent(noLocal, poolBead{Type: "bug"}, "local-coder-polecat", nil, now); !refused || !strings.Contains(r, "no local seats (max_local 0)") {
		t.Errorf("max_local 0 must refuse a requested local seat, got %q refused=%v (%s)", a, refused, r)
	}
	// Room, but inside the stagger gap: the seat's own pacing rule decides, and
	// naming it is what tells the caller the seat frees in three minutes.
	recent := []poolSession{sess("local-coder-polecat", time.Minute)}
	if a, r, refused := choosePoolAgent(pool, poolBead{Type: "bug"}, "local-coder-polecat", recent, now); !refused || !strings.Contains(r, "stagger 1m0s") {
		t.Errorf("a requested local seat inside the stagger must refuse, got %q refused=%v (%s)", a, refused, r)
	}
	// The rule reads the same way round: a request for the overflow seat with a
	// local seat free stays on the overflow seat.
	if a, _, refused := choosePoolAgent(pool, poolBead{Type: "bug"}, "deepseek-flash", nil, now); a != "deepseek-flash" || refused {
		t.Errorf("a requested overflow seat must not be moved to a free local one, got %q refused=%v", a, refused)
	}
}

// An explicit request for an agent the pool does not own outranks the bead's
// spent local attempt (gt-gcrk). `gt sling <bead> gastown --agent claude-sonnet`
// on a local-attempt:1 bead was refused as "overflow full": the spent-attempt
// rule ran first and answered a request the pool never had. local-attempt:1
// records an attempt on the pool's own seat, so it says nothing about a seat the
// pool does not own, and the request has to reach the caller untouched.
func TestChoosePoolAgentNonPoolRequestOutranksSpentLocalAttempt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	capped := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 2, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	sess := func(agent string, age time.Duration) poolSession {
		return poolSession{name: agent + "-" + age.String(), agent: agent, created: now.Add(-age)}
	}
	// The state the sonnet sling hit: every seat the bead's labels could route it
	// to is taken, so the spent-attempt rule refused the sling outright.
	allFull := []poolSession{
		sess("local-coder-polecat", 30*time.Minute), sess("local-coder-polecat", 20*time.Minute),
		sess("deepseek-flash", 30*time.Minute), sess("deepseek-flash", 20*time.Minute),
	}
	spent := poolBead{Type: "bug", Labels: []string{localAttemptLabel}}

	// The request stands untouched whether or not the pool is full: it is not the
	// pool's to admit, and a non-empty answer here is the pool spending on the
	// provider the caller did not ask for.
	if a, r, refused := choosePoolAgent(capped, spent, "claude-sonnet", allFull, now); a != "" || r != "" || refused {
		t.Errorf("a non-pool request on a spent-attempt bead must stand untouched, got %q %q refused=%v", a, r, refused)
	}
	if a, r, refused := choosePoolAgent(capped, spent, "claude-sonnet", nil, now); a != "" || r != "" || refused {
		t.Errorf("a non-pool request with every seat free must stand untouched too, got %q %q refused=%v", a, r, refused)
	}
	// An explicit route:* label is the operator's own decision, and rule 1 keeps
	// it ahead of the request whatever the bead carries (gt-4lbz).
	routed := poolBead{Type: "bug", Labels: []string{localAttemptLabel, routeFlashLabel}}
	if a, _, refused := choosePoolAgent(capped, routed, "claude-sonnet", nil, now); a != "deepseek-flash" || refused {
		t.Errorf("route:flash must still outrank a non-pool request, got %q refused=%v", a, refused)
	}
	// A request naming one of the pool's own seats keeps the rules it had: the
	// spent attempt still spends it on the overflow seat, silently for the local
	// seat and as a refusal at that seat's cap.
	if a, r, refused := choosePoolAgent(capped, spent, "local-coder-polecat", nil, now); a != "deepseek-flash" || refused {
		t.Errorf("a requested local seat on a spent-attempt bead still overflows, got %q refused=%v (%s)", a, refused, r)
	}
	if a, r, refused := choosePoolAgent(capped, spent, "deepseek-flash", allFull, now); !refused || a != "deepseek-flash" {
		t.Errorf("a requested overflow seat at its cap must still refuse, got %q refused=%v (%s)", a, refused, r)
	}
	// The seat is the same one the request named, and the line still credits the
	// spent attempt rather than the request — the request changed no routing.
	if a, r, refused := choosePoolAgent(capped, spent, "deepseek-flash", nil, now); a != "deepseek-flash" || refused {
		t.Errorf("a requested overflow seat with room must be taken, got %q refused=%v", a, refused)
	} else if want := "pool: overflow -> deepseek-flash (local-attempt:1 failed)"; r != want {
		t.Errorf("reason = %q, want %q", r, want)
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
	t.Parallel()
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
	got, err := listPolecatSessions(f, "")
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
	if a, r, _ := choosePoolAgent(pool, poolBead{}, "", got, now); a != "local-coder-polecat" {
		t.Errorf("one local (marble, 1m ago) with a 30s gap and room for one more: want local, got %q (%s)", a, r)
	}
	pool.MinSpawnGap = "5m"
	if a, r, _ := choosePoolAgent(pool, poolBead{}, "", got, now); a != "deepseek-flash" {
		t.Errorf("marble 1m ago with a 5m gap: want overflow, got %q (%s)", a, r)
	}
}

// TestListPolecatSessionsExcludesDoneWithOpenMR pins gt-2nft: a polecat whose
// own agent bead reads done with an MR still in the queue does not occupy the
// seat its session name would otherwise claim. The pool refused a 4th flash
// spawn at 3/3 with only two live flash sessions because two "done, MR ready"
// polecats were counted as occupants; this is the fix.
func TestListPolecatSessionsExcludesDoneWithOpenMR(t *testing.T) {
	t.Parallel()
	disposition := func(rigName, polecatName string) (polecat.WorkstateDisposition, error) {
		if polecatName == "diamond" {
			return polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictPendingMR, ReuseStatus: "idle-pr-open"}, nil
		}
		return polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictWorking}, nil
	}

	f := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-granite": {"GT_ROLE": "gastown/polecats/granite", "GT_AGENT": "deepseek-flash"},
			"gt-diamond": {"GT_ROLE": "gastown/polecats/diamond", "GT_AGENT": "deepseek-flash"},
		},
	}
	got, err := listPolecatSessionsWith(f, disposition, poolTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].name != "gt-granite" {
		t.Fatalf("want only granite counted, got %+v", got)
	}

	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	_, over, _ := poolSeatCounts(pool, got)
	if over != 1 {
		t.Errorf("overflow occupancy should count only the live working session, got %d", over)
	}
}

// TestListPolecatSessionsFailsOpenOnDispositionError keeps a session counted
// when the polecat's own state cannot be read: an overrun the pool exists to
// prevent (gt-md4z) is worse than one extra refused sling.
func TestListPolecatSessionsFailsOpenOnDispositionError(t *testing.T) {
	t.Parallel()
	disposition := func(rigName, polecatName string) (polecat.WorkstateDisposition, error) {
		return polecat.WorkstateDisposition{}, errors.New("dolt unreachable")
	}

	f := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-diamond": {"GT_ROLE": "gastown/polecats/diamond", "GT_AGENT": "deepseek-flash"},
		},
	}
	got, err := listPolecatSessionsWith(f, disposition, poolTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want the session still counted on lookup failure, got %+v", got)
	}
}

// ── The pool router on fakes ────────────────────────────────────────────────

// poolTestNow is the fixed clock every pool router test decides at.
var poolTestNow = time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)

// testPoolTown is one town's pool with every collaborator of the decision
// faked: the settings, the tmux server, the bead lookup and label write, and
// the seat claim filesystem. Each process() is a fresh `gt sling` against it.
type testPoolTown struct {
	pool   *config.PolecatPool
	lister sessionLister
	fs     poolSeatFS
	root   string
	// lookup answers the bead lookup; by default every bead is a task.
	lookup func(beadID string) (poolBead, error)
	addErr error
	added  []string
	// dead names the PIDs whose slings are gone; every other PID is alive.
	dead    map[int]bool
	lockErr error
	nextPID int
}

func newTestPoolTown(pool *config.PolecatPool) *testPoolTown {
	return &testPoolTown{
		pool:   pool,
		lister: &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}},
		fs:     newFakeSeatFS(),
		root:   "/town",
		lookup: func(beadID string) (poolBead, error) { return poolBead{ID: beadID, Type: "task"}, nil },
		dead:   map[int]bool{},
	}
}

// liveSessions puts polecat sessions on the fake tmux server, one per agent
// given, each created an hour before the test clock.
func (w *testPoolTown) liveSessions(agents ...string) {
	f := &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}}
	for i, agent := range agents {
		name := fmt.Sprintf("gt-live-%d", i)
		f.sessions[name] = map[string]string{"GT_ROLE": "gastown/polecats/live" + fmt.Sprint(i), "GT_AGENT": agent}
		f.created[name] = poolTestNow.Add(-time.Hour)
	}
	w.lister = f
}

// process is a router standing in for one `gt sling` process: its own claim
// store and PID, the town's shared state.
func (w *testPoolTown) process() *poolRouter {
	w.nextPID++
	clock := func() time.Time { return poolTestNow }
	return &poolRouter{
		pool:        func() *config.PolecatPool { return w.pool },
		sessions:    func() sessionLister { return w.lister },
		disposition: nil,
		lookupBead:  func(beadID string) (poolBead, error) { return w.lookup(beadID) },
		addLabel: func(beadID, label string) error {
			w.added = append(w.added, beadID+":"+label)
			return w.addErr
		},
		seats: &poolSeatLedger{
			townRoot: w.root,
			fs:       w.fs,
			store:    &poolSeatClaimStore{},
			lock: func() (func(), error) {
				if w.lockErr != nil {
					return nil, w.lockErr
				}
				return func() {}, nil
			},
			alive: func(pid int) bool { return !w.dead[pid] },
			pid:   w.nextPID,
			now:   clock,
		},
		now: clock,
	}
}

// claims reads the town's claims. Every caller reads a claim set it wrote
// itself, so a read failure is the test being wrong.
func (w *testPoolTown) claims(t *testing.T) []poolSeatClaim {
	t.Helper()
	claims, err := w.process().seats.read()
	if err != nil {
		t.Fatalf("reading seat claims: %v", err)
	}
	return claims
}

// sling decides as a fresh live `gt sling` would, failing the test on an error.
func (w *testPoolTown) sling(t *testing.T, beadID string) (string, string) {
	t.Helper()
	agent, reason, err := w.process().route(beadID, "", true, false)
	if err != nil {
		t.Fatalf("slinging %s: %v", beadID, err)
	}
	return agent, reason
}

// resolvePolecatPoolAgent reads the town's pool; no pool -> "", "".
func TestResolvePolecatPoolAgent(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(nil)
	r := w.process()
	if a, rs, err := r.route("", "", true, false); a != "" || rs != "" || err != nil {
		t.Errorf("no pool: got %q %q %v", a, rs, err)
	}
	w.pool = &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 1, OverflowAgent: "deepseek-flash"}
	if a, _, _ := r.route("", "", true, false); a != "local-coder-polecat" {
		t.Errorf("empty town: got %q", a)
	}
	w.liveSessions("local-coder-polecat")
	if a, _, _ := r.route("", "", true, false); a != "deepseek-flash" {
		t.Errorf("full pool: got %q", a)
	}
	w.lister = &fakeLister{err: errors.New("no server")}
	if a, rs, err := r.route("", "", true, false); a != "deepseek-flash" || !strings.Contains(rs, "cannot list sessions") || err != nil {
		t.Errorf("a lister failure is not a full pool, it is an unknown one: got %q %q %v", a, rs, err)
	}

	// A capped overflow seat refuses the sling instead of growing (gt-jzr1):
	// the local seat is taken and the one overflow seat with it.
	w.pool = &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 1, OverflowAgent: "deepseek-flash", MaxOverflow: 1}
	w.liveSessions("local-coder-polecat", "deepseek-flash")
	agent, reason, err := r.route("", "", true, false)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("capped pool: want a refusal, got %q %q %v", agent, reason, err)
	}
	if agent != "deepseek-flash" || !strings.Contains(reason, "pool: overflow full (1/1) -> no seat") {
		t.Errorf("the refusal names the seat it could not take: %q %q", agent, reason)
	}
	// The convoy feeder defers on this prefix instead of failing the bead,
	// and the way through it names is the pool's own config — no flag spawns
	// past the cap (gt-4lbz). internal/daemon/convoy_sling_backpressure_test.go
	// pins the parse of this exact wording.
	if !strings.HasPrefix(err.Error(), "sling refused:") || !strings.HasSuffix(err.Error(), "; raise polecat_pool.max_local/max_overflow to spawn") {
		t.Errorf("refusal message must carry the deferral marker and the way through: %q", err)
	}
	// No flag spawns past the cap: every automated redispatch path carries
	// --force for its own safety guards, so a cap --force opens is one those
	// paths are not held by (gt-4lbz). Capacity is raised in the pool's own
	// settings, which the caller reads on the next sling.
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a refused sling must not claim a seat: %v", claims)
	}

	// A pool with no local seats has none to give: max_local 0 routes the bead
	// to the overflow seat rather than to the role default, which is the local
	// seat the operator just closed (gt-4lbz).
	w.pool = &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 0}
	w.liveSessions()
	if a, rs, _ := r.route("", "", true, false); a != "" || !strings.Contains(rs, "max_local") {
		t.Errorf("misconfigured pool: got %q %q", a, rs)
	}
}

// The sling path, not just the policy: a sling that named the local seat and
// found it taken comes back as a refusal — the marker the convoy feeder defers
// on, so the bead is queued rather than paid for on the overflow seat — and
// leaves no seat claimed behind it (gt-x40u).
func TestResolvePoolAgentRefusesARequestItCannotServe(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(&config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 1, OverflowAgent: "deepseek-flash", MaxOverflow: 3})
	w.liveSessions("local-coder-polecat")
	w.lookup = func(beadID string) (poolBead, error) { return poolBead{ID: beadID, Type: "bug"}, nil }

	agent, reason, err := w.process().route("gt-b", "local-coder-polecat", true, false)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("a requested local seat with the seat taken must refuse: got %q %q %v", agent, reason, err)
	}
	if want := "pool: local full (1/1) -> no seat (requested local-coder-polecat)"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	if !strings.HasPrefix(err.Error(), "sling refused: ") {
		t.Errorf("the refusal must carry the deferral marker: %q", err)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a refused request must not claim a seat: %v", claims)
	}
}

// The sling path the bug was reported on: `gt sling <bead> --agent claude-sonnet`
// on a local-attempt:1 bead with every seat taken came back as a backpressure
// refusal, so the convoy feeder deferred a bead that was never the pool's to
// place (gt-gcrk). The pool must answer nothing here — not a seat, not a
// refusal, not a claim.
func TestResolvePoolAgentLeavesANonPoolRequestUntouched(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(&config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 1, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", MaxOverflow: 1})
	w.lookup = func(string) (poolBead, error) {
		return poolBead{ID: "gt-b", Type: "bug", Labels: []string{localAttemptLabel}}, nil
	}
	w.liveSessions("local-coder-polecat", "deepseek-flash")

	agent, reason, err := w.process().route("gt-b", "claude-sonnet", true, false)
	if err != nil || agent != "" || reason != "" {
		t.Fatalf("a non-pool request must pass through untouched: got %q %q %v", agent, reason, err)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a request the pool does not answer must claim no seat: %v", claims)
	}
	if len(w.added) != 0 {
		t.Errorf("a request the pool does not answer must write no label: %v", w.added)
	}
}

// A tmux-listing failure must not override an explicit non-pool --agent
// (gt-67fj): the fallback that answers an unknown session count with the
// overflow agent ran ahead of the "not the pool's seat" check, so a request
// for claude-sonnet came back deepseek-flash whenever tmux hiccupped. The
// request has to stand untouched here exactly as it does with a clean count
// in TestResolvePoolAgentLeavesANonPoolRequestUntouched above.
func TestResolvePoolAgentLeavesANonPoolRequestUntouchedOnListSessionsFailure(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(&config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 1, OverflowAgent: "deepseek-flash"})
	w.lookup = func(string) (poolBead, error) { return poolBead{ID: "gt-b", Type: "bug"}, nil }
	w.lister = &fakeLister{err: errors.New("no server")}

	agent, reason, err := w.process().route("gt-b", "claude-sonnet", true, false)
	if err != nil || agent != "" || reason != "" {
		t.Fatalf("a non-pool request must survive a tmux-listing failure untouched: got %q %q %v", agent, reason, err)
	}
	if len(w.added) != 0 {
		t.Errorf("a request the pool does not answer must write no label: %v", w.added)
	}
}

// The same requirement against the seat-claim read path (gt-67fj): a claims
// directory that cannot be read must not override a non-pool --agent either.
func TestResolvePoolAgentLeavesANonPoolRequestUntouchedOnSeatClaimReadFailure(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(racingPool(3, ""))
	seatFS := newFakeSeatFS()
	seatFS.readDirErr = errSeatFSUnreadable
	w.fs = seatFS

	agent, reason, err := w.process().route("gt-a", "claude-sonnet", true, false)
	if err != nil || agent != "" || reason != "" {
		t.Fatalf("a non-pool request must survive a seat-claim read failure untouched: got %q %q %v", agent, reason, err)
	}
	if files := seatFS.claimFiles(); len(files) != 0 {
		t.Errorf("a request the pool does not answer must claim no seat: %v", files)
	}
}

// fakePoolTown is a town whose pool has three local seats, one taken 10m ago
// (so a seat is free and the stagger gap has passed), whose bead lookup answers
// bead (or lookupErr) and whose label write fails with addErr.
func fakePoolTown(bead poolBead, lookupErr, addErr error) *testPoolTown {
	return fakePoolTownWithPool(&config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash"}, bead, lookupErr, addErr)
}

// fakePoolTownWithPool is fakePoolTown with the pool under test supplied, for
// the cases where the knob rather than the routing table is what is on trial.
func fakePoolTownWithPool(pool *config.PolecatPool, bead poolBead, lookupErr, addErr error) *testPoolTown {
	w := newTestPoolTown(pool)
	w.lister = &fakeLister{
		sessions: map[string]map[string]string{"gt-a": {"GT_ROLE": "gastown/polecats/a", "GT_AGENT": "local-coder-polecat"}},
		created:  map[string]time.Time{"gt-a": poolTestNow.Add(-10 * time.Minute)},
	}
	w.lookup = func(string) (poolBead, error) {
		if lookupErr != nil {
			return poolBead{}, lookupErr
		}
		return bead, nil
	}
	w.addErr = addErr
	return w
}

// The idle-seat fill is the one routing branch with a side effect: it attaches
// local-attempt:1 so the bead cannot take a second local seat. A dry run must
// report the same route without writing.
func TestResolvePoolAgentIdleFillLabelsTheBead(t *testing.T) {
	t.Parallel()
	w := fakePoolTown(poolBead{ID: "gt-b", Type: "bug"}, nil, nil)
	r := w.process()

	agent, reason, err := r.route("gt-b", "", true, false)
	if err != nil {
		t.Fatalf("idle-seat fill must not refuse: %v", err)
	}
	want := "pool: local seat 2/3 -> local-coder-polecat (idle-seat fill, local-attempt:1)"
	if agent != "local-coder-polecat" || reason != want {
		t.Fatalf("got %q %q, want %q %q", agent, reason, "local-coder-polecat", want)
	}
	if len(w.added) != 1 || w.added[0] != "gt-b:"+localAttemptLabel {
		t.Errorf("label writes = %v, want [gt-b:%s]", w.added, localAttemptLabel)
	}

	w.added = nil
	peekAgent, peekReason, err := r.route("gt-b", "", false, false)
	if err != nil {
		t.Fatalf("peek must report the same route, not a refusal: %v", err)
	}
	if peekAgent != agent || peekReason != reason {
		t.Errorf("peek = %q %q, want the same route as resolve", peekAgent, peekReason)
	}
	if len(w.added) != 0 {
		t.Errorf("a dry run must not label the bead, got %v", w.added)
	}
}

// With idle_fill off, a bug bead with a free seat leaves the seat empty. It
// must not take local-attempt:1: that label bounds a bead to one local attempt,
// and this bead never spent one, so labeling it would route the bead to flash
// forever and retire a label that no longer means what it says (gt-nn7n).
func TestResolvePoolAgentFillOffLabelsNothing(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3, MinSpawnGap: "4m", OverflowAgent: "deepseek-flash", IdleFill: boolp(false)}
	w := fakePoolTownWithPool(pool, poolBead{ID: "gt-b", Type: "bug"}, nil, nil)

	agent, reason, err := w.process().route("gt-b", "", true, false)
	if err != nil {
		t.Fatalf("the fill being off must not refuse the sling: %v", err)
	}
	if agent != "deepseek-flash" || !strings.Contains(reason, "idle_fill off") {
		t.Fatalf("got %q %q, want the overflow agent and the knob named", agent, reason)
	}
	if len(w.added) != 0 {
		t.Errorf("a bead that never took a local seat must not be labeled: %v", w.added)
	}
}

// A bead that did not go to the idle-seat fill branch is never labeled.
func TestResolvePoolAgentNoLabelForShapedRoute(t *testing.T) {
	t.Parallel()
	w := fakePoolTown(poolBead{ID: "gt-b", Type: "task"}, nil, nil)
	agent, reason, err := w.process().route("gt-b", "", true, false)
	if err != nil {
		t.Fatalf("a task bead with a free seat must not refuse: %v", err)
	}
	if agent != "local-coder-polecat" || !strings.HasSuffix(reason, "(type=task)") {
		t.Fatalf("got %q %q", agent, reason)
	}
	if len(w.added) != 0 {
		t.Errorf("a task bead must not get local-attempt:1: %v", w.added)
	}
}

// A bead whose shape could not be read falls back to the seat count, and the
// reason says why rather than passing the seat-only decision off as a route.
func TestResolvePoolAgentBeadLookupFailure(t *testing.T) {
	t.Parallel()
	w := fakePoolTown(poolBead{}, errors.New("bd: database not found"), nil)
	agent, reason, err := w.process().route("gt-b", "", true, false)
	if err != nil {
		t.Fatalf("an unreadable bead must still route: %v", err)
	}
	if agent != "local-coder-polecat" {
		t.Errorf("seat-only decision should still take the free seat, got %q (%s)", agent, reason)
	}
	if !strings.Contains(reason, "gt-b unreadable") || !strings.Contains(reason, "database not found") {
		t.Errorf("reason must say the bead was unreadable: %q", reason)
	}
	if len(w.added) != 0 {
		t.Errorf("no shape, no fill rule, no label: %v", w.added)
	}
}

// The label write failing must not swallow the route: the sling still names
// the agent, and says the label did not land.
func TestResolvePoolAgentLabelWriteFailure(t *testing.T) {
	t.Parallel()
	w := fakePoolTown(poolBead{ID: "gt-b", Type: "bug"}, nil, errors.New("dolt is down"))
	agent, reason, err := w.process().route("gt-b", "", true, false)
	if err != nil {
		t.Fatalf("a failed label write must not refuse the sling: %v", err)
	}
	if agent != "local-coder-polecat" {
		t.Errorf("a failed label write must not change the route, got %q", agent)
	}
	if !strings.Contains(reason, idleFillReason) || !strings.Contains(reason, "local-attempt:1 label failed") {
		t.Errorf("reason should carry both the route and the failure: %q", reason)
	}
}

// A decision lock that cannot be taken still routes, from the live sessions
// alone, and the reason says the seat was not reserved rather than passing a
// racy decision off as a reserved one.
func TestResolvePoolAgentUnreservedWhenTheDecisionLockFails(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(racingPool(2, ""))
	w.lockErr = errors.New("polecat pool lock busy for 5s")

	agent, reason := w.sling(t, "gt-a")
	if agent != claimLocal || !strings.Contains(reason, "[seat not reserved: polecat pool lock busy for 5s]") {
		t.Errorf("got %q (%s), want the local seat with the missing reservation named", agent, reason)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a decision taken without the lock claims no seat: %v", claims)
	}
}

// ── Seat claims (gt-eoi9) ───────────────────────────────────────────────────

const (
	claimLocal    = "local-coder-polecat"
	claimOverflow = "deepseek-flash"
)

// racingPool is a pool with local seats and, unless a gap is given, no stagger
// at all — so the seat count alone decides. The fake tmux server has not heard
// of any of the slings racing each other, which is the moment the cap used to
// break.
func racingPool(maxLocal int, minSpawnGap string) *config.PolecatPool {
	return &config.PolecatPool{LocalAgent: claimLocal, MaxLocal: maxLocal, MinSpawnGap: minSpawnGap, OverflowAgent: claimOverflow}
}

// The gt-eoi9 bug: three slings started in parallel each counted the same live
// sessions, so each read "local pool 1/2" and spawned locally, putting three
// polecats on a two-seat pool (opal, shale and agate all local). The seat claim
// the first sling leaves behind is what the second counts instead.
func TestPoolSeatClaimsHoldTheCapForConcurrentSlings(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(racingPool(2, ""))

	a1, r1 := w.sling(t, "gt-a")
	a2, r2 := w.sling(t, "gt-b")
	a3, r3 := w.sling(t, "gt-c")

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
	t.Parallel()
	w := newTestPoolTown(racingPool(3, "4m"))
	w.liveSessions(claimLocal)

	if a, r := w.sling(t, "gt-a"); a != claimLocal {
		t.Fatalf("one live local, one seat free, gap long past: want local, got %q (%s)", a, r)
	}
	a, r := w.sling(t, "gt-b")
	if a != claimOverflow || !strings.Contains(r, "stagger") {
		t.Errorf("a sling racing the spawn it cannot see yet is a stagger, got %q (%s)", a, r)
	}
}

// The claim lives exactly as long as the seat is invisible to tmux: StartSession
// drops it, and the live session counts instead — one seat either way, never
// both and never neither.
func TestPoolSeatClaimIsHandedOverToTheSession(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(racingPool(2, ""))
	first := w.process()
	if a, r, err := first.route("gt-a", "", true, false); err != nil || a != claimLocal {
		t.Fatalf("empty pool: want the local seat, got %q (%s) %v", a, r, err)
	}
	if claims := w.claims(t); len(claims) != 1 {
		t.Fatalf("a local route must claim a seat, got %d claims", len(claims))
	}

	// StartSession: the tmux session is now the record of that seat.
	first.seats.store.release()
	if claims := w.claims(t); len(claims) != 0 {
		t.Fatalf("StartSession must drop the claim, %d left", len(claims))
	}

	// A second sling in a fresh process counts the live session, not a phantom
	// claim: seat 2 of 2, which is what a double-count would call "full".
	w.liveSessions(claimLocal)
	a, r := w.sling(t, "gt-b")
	if a != claimLocal || !strings.Contains(r, "local seat 2/2") {
		t.Errorf("the live session must count as one seat, got %q (%s)", a, r)
	}
}

// A dry run reads the claims — so it prints the route a real sling would take —
// but claims nothing and drops nothing.
func TestPeekPolecatPoolAgentNeitherClaimsNorDropsSeats(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(racingPool(2, ""))

	if a, _, _ := w.process().route("gt-a", "", false, false); a != claimLocal {
		t.Fatalf("empty pool: the preview route should be the local seat, got %q", a)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Fatalf("a dry run must not claim a seat, got %d claims", len(claims))
	}

	if a, _ := w.sling(t, "gt-a"); a != claimLocal {
		t.Fatalf("live sling: want the local seat, got %q", a)
	}
	// Seen from another process, that claim is the second seat taken.
	if _, r, _ := w.process().route("gt-b", "", false, false); !strings.Contains(r, "local seat 2/2") {
		t.Errorf("the preview must count the seat another sling claimed: %q", r)
	}
	if claims := w.claims(t); len(claims) != 1 {
		t.Errorf("a dry run must not drop another sling's claim, %d left", len(claims))
	}
}

// A claim whose sling is gone cannot become a session, so the seat is free: a
// crashed sling must not shadow a seat for the TTL, and a claim held too long
// by a process that lingers must not shadow one forever.
func TestPoolSeatClaimCleanupDropsCrashedAndStaleSlingers(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(racingPool(2, ""))
	const crashedPID, livePID = 9001, 9002
	w.dead[crashedPID] = true
	seats := w.process().seats
	crashed, err := seats.publish(poolSeatClaim{
		ID: "crashed-1", PID: crashedPID, Agent: claimLocal, CreatedAt: poolTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := seats.publish(poolSeatClaim{
		ID: "stale-1", PID: livePID, Agent: claimLocal, CreatedAt: poolTestNow.Add(-poolSeatClaimTTL - time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	held, err := seats.publish(poolSeatClaim{
		ID: "held-1", PID: livePID, Agent: claimLocal, CreatedAt: poolTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}

	seats.cleanupStale()

	got := w.claims(t)
	if len(got) != 1 || got[0].ID != held.ID {
		t.Fatalf("only the live, fresh claim survives, got %v (crashed=%s stale=%s held=%s)",
			got, crashed.ID, stale.ID, held.ID)
	}
}

// ── A claim set that cannot be read (gt-t8q5) ───────────────────────────────

// errSeatFSUnreadable stands in for the failure a real directory read produces
// (permissions, I/O) — the one that is not "no directory yet".
var errSeatFSUnreadable = errors.New("input/output error")

// fakeSeatFS is the seat claim filesystem in memory. It records every operation,
// so a test can tell a claim that was made and dropped from one that was never
// made, and it can be told to fail its directory reads, which is how the
// fail-closed path is reached without a town on disk.
type fakeSeatFS struct {
	mu         sync.Mutex
	dirs       map[string]bool
	files      map[string][]byte
	ops        []string
	readDirErr error
}

func newFakeSeatFS() *fakeSeatFS {
	return &fakeSeatFS{dirs: map[string]bool{}, files: map[string][]byte{}}
}

func (f *fakeSeatFS) log(format string, args ...any) {
	f.ops = append(f.ops, fmt.Sprintf(format, args...))
}

// claimFiles names the claim files still present.
func (f *fakeSeatFS) claimFiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.files))
	for path := range f.files {
		out = append(out, filepath.Base(path))
	}
	return out
}

// wroteClaim reports whether this filesystem saw a claim published: the setup
// assertion that separates "the claim was dropped on the way out" from "the
// claim was never made".
func (f *fakeSeatFS) wroteClaim() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.ops {
		if strings.HasPrefix(op, "rename ") {
			return true
		}
	}
	return false
}

func (f *fakeSeatFS) MkdirAll(path string, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[path] = true
	f.log("mkdirall %s", path)
	return nil
}

func (f *fakeSeatFS) WriteFile(name string, data []byte, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = append([]byte(nil), data...)
	f.log("write %s", filepath.Base(name))
	return nil
}

func (f *fakeSeatFS) Rename(oldpath, newpath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[oldpath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldpath, Err: os.ErrNotExist}
	}
	delete(f.files, oldpath)
	f.files[newpath] = data
	f.log("rename %s", filepath.Base(newpath))
	return nil
}

func (f *fakeSeatFS) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("remove %s", filepath.Base(name))
	delete(f.files, name)
	return nil
}

func (f *fakeSeatFS) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeSeatFS) ReadDir(name string) ([]os.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readDirErr != nil {
		f.log("readdir %s -> %v", name, f.readDirErr)
		return nil, f.readDirErr
	}
	if !f.dirs[name] {
		// A directory nothing has written to yet, as os.ReadDir reports it.
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	f.log("readdir %s", name)
	var entries []os.DirEntry
	for path := range f.files {
		if filepath.Dir(path) == name {
			entries = append(entries, fakeSeatDirEntry{name: filepath.Base(path)})
		}
	}
	return entries, nil
}

type fakeSeatDirEntry struct{ name string }

func (e fakeSeatDirEntry) Name() string { return e.name }
func (e fakeSeatDirEntry) IsDir() bool  { return false }
func (e fakeSeatDirEntry) Type() os.FileMode {
	return 0
}
func (e fakeSeatDirEntry) Info() (os.FileInfo, error) {
	return nil, fmt.Errorf("fakeSeatFS entry %s carries no file info", e.name)
}

// The same requirement against a claim path that really cannot be read — a
// regular file where the directory belongs, the shape an interrupted write or a
// bad copy leaves behind — so the fail-closed behavior does not rest on the
// in-memory filesystem alone: the real one has to report ENOTDIR as a failure,
// not as "no claims yet". On the fail-open code this test routes the sling to
// the local seat.
func TestPoolSeatClaimReadFailureFailsClosedWhenThePathIsNotADirectory(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(racingPool(3, ""))
	w.root = t.TempDir()
	w.fs = osPoolSeatFS{}
	if err := os.MkdirAll(filepath.Join(w.root, ".runtime"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poolSeatClaimDir(w.root), []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	agent, reason := w.sling(t, "gt-a")
	if agent != claimOverflow {
		t.Errorf("the local seat must not be taken while its claims are unreadable: got %q (%s)", agent, reason)
	}
	if !strings.Contains(reason, "cannot read seat claims") {
		t.Errorf("the reason names the failure rather than a clean count: %q", reason)
	}
}

// A claim set that could not be read is not a claim set that is empty: the
// seats it holds are unknown, and the local seat is the seat the claims exist
// to hold the cap on, so it is not taken on a count that cannot see them.
// Letting the read failure read as "no claims" is the gt-eoi9 race back, with
// the guard removed by a transient error.
func TestPoolSeatClaimReadFailureFailsClosedOnTheLocalSeat(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(racingPool(3, ""))
	seatFS := newFakeSeatFS()
	seatFS.readDirErr = errSeatFSUnreadable
	w.fs = seatFS

	agent, reason := w.sling(t, "gt-a")
	if agent != claimOverflow {
		t.Errorf("the local seat must not be taken while its claims are unreadable: got %q (%s)", agent, reason)
	}
	if !strings.Contains(reason, "cannot read seat claims") || !strings.Contains(reason, errSeatFSUnreadable.Error()) {
		t.Errorf("the reason names the failure rather than a clean count: %q", reason)
	}
	if files := seatFS.claimFiles(); len(files) != 0 {
		t.Errorf("a route taken without a reservation claims no seat: %v", files)
	}

	// A preview has to print the route a live sling would take, and that sling
	// would take the fallback.
	peekAgent, peekReason, err := w.process().route("gt-b", "", false, false)
	if err != nil || peekAgent != agent || peekReason != reason {
		t.Errorf("peek = %q %q %v, want the live sling's route %q %q", peekAgent, peekReason, err, agent, reason)
	}
}

// A claim is released on the spawn paths that fail too. The seat a failed spawn
// reserved stands for a polecat that never existed, and cleanup leaves it alone
// for the whole TTL because the process holding it is alive, so every sling in
// the meantime routes away from a seat nobody is using. A spawn that succeeds
// hands the claim to its session instead, and keeps it.
func TestSlingSeatSpawnReleasesTheClaimOfASpawnThatFailed(t *testing.T) {
	t.Parallel()
	spawnWith := func(prepareErr error) (*fakeSeatFS, *poolRouter, error) {
		w := newTestPoolTown(racingPool(3, ""))
		seatFS := newFakeSeatFS()
		w.fs = seatFS
		router := w.process()
		s := slingSeatSpawn{
			backpressure: func(string, string, SlingSpawnOptions) error { return nil },
			resolvePool: func(_, beadID, requested string, explicit bool) (string, string, error) {
				return router.route(beadID, requested, true, explicit)
			},
			releaseSeat: router.seats.store.release,
			prepare: func(_, rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
				if prepareErr != nil {
					return nil, prepareErr
				}
				return &SpawnedPolecatInfo{RigName: rigName, agent: opts.Agent}, nil
			},
		}
		_, err := s.spawn("/town", "gastown", SlingSpawnOptions{HookBead: "gt-a"})
		return seatFS, router, err
	}

	// A rig that does not exist is one of the failures that come after the pool
	// has already claimed the seat for the spawn.
	seatFS, router, err := spawnWith(errors.New("rig 'no-such-rig' not found"))
	if err == nil || !strings.Contains(err.Error(), "no-such-rig") {
		t.Fatalf("the failure names the rig: %v", err)
	}
	if !seatFS.wroteClaim() {
		t.Fatal("setup: the failed spawn must have reached the pool decision and claimed a seat")
	}
	if files := seatFS.claimFiles(); len(files) != 0 {
		t.Errorf("a spawn that failed must leave no seat claimed behind it: %v", files)
	}
	if own := router.seats.store.ownID(); own != "" {
		t.Errorf("this process must not still think it holds a claim: %q", own)
	}

	seatFS, router, err = spawnWith(nil)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if files := seatFS.claimFiles(); len(files) != 1 || router.seats.store.ownID() == "" {
		t.Errorf("a spawn that succeeded hands its claim to the session start, got files %v own %q", files, router.seats.store.ownID())
	}
}

// ── The overflow cap (gt-jzr1) ──────────────────────────────────────────────

// racingOverflowTown is a town whose local seat is taken and whose one
// overflow seat is capped, so every fresh sling is an overflow route racing the
// others for the last flash seat.
func racingOverflowTown() *testPoolTown {
	w := newTestPoolTown(&config.PolecatPool{LocalAgent: claimLocal, MaxLocal: 1, OverflowAgent: claimOverflow, MaxOverflow: 1})
	w.liveSessions(claimLocal)
	return w
}

// The overflow cap holds for the same reason the local cap does (gt-eoi9): the
// flash seat the first sling takes is claimed on disk, so a sling that has
// never heard of its session counts it. Without the claim every sling in a
// batch reads "overflow 1/1", overflows anyway, and the cap that was given to
// bound the spend bounds nothing.
func TestPoolOverflowCapRefusesTheSlingThatOverfillsIt(t *testing.T) {
	t.Parallel()
	w := racingOverflowTown()

	a1, r1 := w.sling(t, "gt-a")
	if a1 != claimOverflow || !strings.Contains(r1, "local full (1/1) -> "+claimOverflow) {
		t.Fatalf("the last flash seat goes to the first sling, got %q (%s)", a1, r1)
	}
	if claims := w.claims(t); len(claims) != 1 || claims[0].Agent != claimOverflow {
		t.Fatalf("an overflow route must claim the overflow seat, got %v", claims)
	}

	// The second sling sees the capped seat taken and refuses.
	agent, reason, err := w.process().route("gt-b", "", true, false)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("the second sling must refuse, not overflow again: got %q (%s) %v", agent, reason, err)
	}
	if !strings.Contains(reason, "pool: overflow full (1/1)") {
		t.Errorf("the refusal must count the capped seat: %q", reason)
	}
	if claims := w.claims(t); len(claims) != 1 {
		t.Errorf("a refused sling claims no seat, got %v", claims)
	}
}

// A dry run prints the refusal a live sling would raise — that is the route it
// would take — and still claims nothing.
func TestPeekPolecatPoolAgentReportsTheOverflowRefusal(t *testing.T) {
	t.Parallel()
	w := racingOverflowTown()
	if a, _ := w.sling(t, "gt-a"); a != claimOverflow {
		t.Fatalf("setup: the flash seat should go to the first sling, got %q", a)
	}
	_, reason, err := w.process().route("gt-b", "", false, false)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("a preview must report the refusal it would hit: %v", err)
	}
	if !strings.Contains(err.Error(), "sling refused: "+reason) {
		t.Errorf("the refusal carries the pool's own reason line: %q vs %q", err, reason)
	}
	if claims := w.claims(t); len(claims) != 1 {
		t.Errorf("a dry run must not claim or drop a seat, got %v", claims)
	}
}
