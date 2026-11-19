package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"strings"
	"testing"
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
	if got, want := c.ByAgent["a"], (poolOccupancyTally{Total: 5, Claim: 2, DeadHooked: 2, Landing: 1, Sessions: 1}); got != want {
		t.Errorf("agent a = %+v, want %+v", got, want)
	}
	if got, want := c.ByAgent["b"], (poolOccupancyTally{Total: 1, Sessions: 1}); got != want {
		t.Errorf("agent b = %+v, want %+v", got, want)
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
