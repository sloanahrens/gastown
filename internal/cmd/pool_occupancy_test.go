package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// poolOccupancyFixture holds one seat of each state the pool counts, on agent
// "a", plus a live session on agent "b". The claimed and dead-hooked seat
// carries both flags, so each count must take it once and no more.
func poolOccupancyFixture() []poolSession {
	return []poolSession{
		{name: "gt-live", agent: "a"},
		{name: "gt-landing", agent: "a", landing: true},
		{name: "dead/gastown/ember", agent: "a", deadHooked: true},
		{name: "pool-claim/x", agent: "a", claim: true},
		{name: "pool-claim/y", agent: "a", claim: true, deadHooked: true},
		{name: "gt-parked", agent: "a", parkedStale: true},
		{name: "gt-other", agent: "b"},
	}
}

// TestPoolOccupancyCountsEachStateOnce counts the fixture by hand: five seats
// hold a pool seat on agent "a" and the parked-stale one is in none of them.
func TestPoolOccupancyCountsEachStateOnce(t *testing.T) {
	t.Parallel()
	c := poolOccupancy(poolOccupancyFixture())
	if c.Total != 6 || c.Claim != 2 || c.DeadHooked != 2 || c.Landing != 1 || c.Sessions != 2 {
		t.Errorf("total %d, claim %d, dead-hooked %d, landing %d, sessions %d; want 6, 2, 2, 1, 2",
			c.Total, c.Claim, c.DeadHooked, c.Landing, c.Sessions)
	}
	// The fixture's dead-hooked seats carry no class, so both count under "".
	if got, want := c.ByAgent["a"], (poolOccupancyTally{Total: 5, Claim: 2, DeadHooked: 2, DeadHookedByClass: map[string]int{"": 2}, Landing: 1, Sessions: 1}); !reflect.DeepEqual(got, want) {
		t.Errorf("agent a = %+v, want %+v", got, want)
	}
	if got, want := c.ByAgent["b"], (poolOccupancyTally{Total: 1, Sessions: 1}); !reflect.DeepEqual(got, want) {
		t.Errorf("agent b = %+v, want %+v", got, want)
	}
}

// TestPoolOccupancyCountsDeadHookedByClass pins that the dead-hooked seats are
// counted once each under the class they carry (gt-fp32o8): two idle-submitted
// and one idle-recovery-needed are three dead-hooked seats, split two and one.
func TestPoolOccupancyCountsDeadHookedByClass(t *testing.T) {
	t.Parallel()
	c := poolOccupancy([]poolSession{
		{name: "dead/gastown/a", agent: "a", deadHooked: true, deadClass: "idle-submitted"},
		{name: "dead/gastown/b", agent: "a", deadHooked: true, deadClass: "idle-submitted"},
		{name: "dead/gastown/c", agent: "a", deadHooked: true, deadClass: "idle-recovery-needed"},
	})
	if c.DeadHooked != 3 {
		t.Fatalf("dead-hooked = %d, want 3", c.DeadHooked)
	}
	if want := map[string]int{"idle-submitted": 2, "idle-recovery-needed": 1}; !reflect.DeepEqual(c.DeadHookedByClass, want) {
		t.Errorf("dead-hooked by class = %v, want %v", c.DeadHookedByClass, want)
	}
}

// TestPoolOccupancyRosterAgreesOnOneFixture pins the tick roster's Live and
// DeadHooked on the fixture to the numbers the roster read produced before the
// occupancy count existed: the claimed and dead-hooked seat is in Live once and
// in DeadHooked once, and the parked-stale seat is in ParkedStale alone.
func TestPoolOccupancyRosterAgreesOnOneFixture(t *testing.T) {
	t.Parallel()
	r := specRosterFrom(poolOccupancyFixture(), nil)
	if want := map[string]int{"a": 5, "b": 1}; !reflect.DeepEqual(r.Live, want) {
		t.Errorf("Live = %v, want %v", r.Live, want)
	}
	if want := map[string]int{"a": 2}; !reflect.DeepEqual(r.DeadHooked, want) {
		t.Errorf("DeadHooked = %v, want %v", r.DeadHooked, want)
	}
	if want := map[string]int{"a": 1}; !reflect.DeepEqual(r.ParkedStale, want) {
		t.Errorf("ParkedStale = %v, want %v", r.ParkedStale, want)
	}
}

// agreementFixture holds the four states on agent "a" (a live session, a seat
// mid-landing, a dead-hooked seat and a claim) and one live session on agent
// "b", so the total is one more than agent a's own count of four.
func agreementFixture() []poolSession {
	return []poolSession{
		{name: "gt-live", agent: "a"},
		{name: "gt-landing", agent: "a", landing: true},
		{name: "dead/gastown/ember", agent: "a", deadHooked: true},
		{name: "pool-claim/x", agent: "a", claim: true},
		{name: "gt-other", agent: "b"},
	}
}

// TestPoolOccupancyAgreesAcrossTheThreeReaders reads one fixture through the
// tick roster, the sling's recount and the pool check. The three give the same
// per-agent and total count, and the recount and the pool check refuse on the
// same total: a full seat (a at its cap of 4) while the town has room, and a
// full town (5 held of max_total 5).
func TestPoolOccupancyAgreesAcrossTheThreeReaders(t *testing.T) {
	t.Parallel()
	sessions := agreementFixture()
	now := time.Now()

	roster := specRosterFrom(sessions, nil)
	if want := map[string]int{"a": 4, "b": 1}; !reflect.DeepEqual(roster.Live, want) {
		t.Fatalf("roster Live = %v, want %v", roster.Live, want)
	}
	if want := map[string]int{"a": 1}; !reflect.DeepEqual(roster.DeadHooked, want) {
		t.Fatalf("roster DeadHooked = %v, want %v", roster.DeadHooked, want)
	}
	held := 0
	for _, n := range roster.Live {
		held += n
	}

	// b's seat is open and the town has room, so the pool check admits it at
	// its own count: a sling for b takes the second of its two seats.
	openPool := &config.PolecatPool{Seats: map[string]config.SeatConfig{"a": {Cap: 4}, "b": {Cap: 2}}, MaxTotal: intPtr(6)}
	if got := specRecountOf(openPool, "b", sessions).Seat; got != roster.Live["b"] {
		t.Errorf("recount seat for b = %d, roster Live = %d", got, roster.Live["b"])
	}
	if _, reason, refused, _ := choosePoolAgent(openPool, "b", sessions, now); refused || !strings.HasPrefix(reason, "pool: seat 2/2 -> b") {
		t.Errorf("pool check for b = refused %v, %q; want admitted at seat 2/2", refused, reason)
	}

	cases := []struct {
		name             string
		maxTotal         int
		wantTotalRefusal bool
		wantReason       string
	}{
		{name: "a full seat refuses on its own count", maxTotal: 6, wantReason: "pool: full (4/4"},
		{name: "a full town refuses on the total", maxTotal: 5, wantTotalRefusal: true, wantReason: "pool: total full (5/5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{"a": {Cap: 4}, "b": {Cap: 2}}, MaxTotal: intPtr(tc.maxTotal)}

			r := specRecountOf(pool, "a", sessions)
			if want := (specSeatRecount{Seat: 4, Cap: 4, Total: held, MaxTotal: tc.maxTotal}); r != want {
				t.Fatalf("recount for a = %+v, want %+v", r, want)
			}
			if r.Total != held {
				t.Fatalf("recount total %d, roster total %d", r.Total, held)
			}

			_, reason, refused, _ := choosePoolAgent(pool, "a", sessions, now)
			if !refused || !strings.HasPrefix(reason, tc.wantReason) {
				t.Fatalf("pool check for a = refused %v, %q; want a refusal beginning %q", refused, reason, tc.wantReason)
			}
			recountRefusesOnTotal := r.MaxTotal > 0 && r.Total >= r.MaxTotal
			if recountRefusesOnTotal != tc.wantTotalRefusal {
				t.Errorf("recount refuses on total = %v, want %v", recountRefusesOnTotal, tc.wantTotalRefusal)
			}
			if poolCheckRefusesOnTotal := strings.Contains(reason, "total full"); poolCheckRefusesOnTotal != recountRefusesOnTotal {
				t.Errorf("pool check refuses on total = %v, recount = %v: they refuse at different totals", poolCheckRefusesOnTotal, recountRefusesOnTotal)
			}
		})
	}
}

// TestPoolOccupancyDocListsTheCountedFlags reads poolOccupancy's doc comment:
// its bulleted flags must be the flags the count carries (every field but
// Total), and it must name the patrol census as its one exception.
func TestPoolOccupancyDocListsTheCountedFlags(t *testing.T) {
	t.Parallel()
	doc := poolOccupancyDoc(t)
	listed := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*-\s+(\w+):`).FindAllStringSubmatch(doc, -1) {
		listed[m[1]] = true
	}
	counted := map[string]bool{}
	tally := reflect.TypeOf(poolOccupancyTally{})
	for i := 0; i < tally.NumField(); i++ {
		if name := tally.Field(i).Name; name != "Total" {
			counted[name] = true
		}
	}
	if !reflect.DeepEqual(listed, counted) {
		t.Errorf("the doc lists flags %v, poolOccupancy counts %v", listed, counted)
	}
	if !strings.Contains(doc, "livePolecatCensus") {
		t.Errorf("the doc does not name livePolecatCensus as the census exception:\n%s", doc)
	}
}

// poolOccupancyDoc returns the doc comment of poolOccupancy in this package's
// source file, which go test runs in.
func poolOccupancyDoc(t *testing.T) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "pool_occupancy.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing pool_occupancy.go: %v", err)
	}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "poolOccupancy" {
			return fn.Doc.Text()
		}
	}
	t.Fatal("poolOccupancy has no declaration in pool_occupancy.go")
	return ""
}
