package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The patrol restart census counts live tmux sessions only, on purpose: the
// pool's other counter, poolOccupancy (internal/cmd/pool_occupancy.go), also
// counts the seats the pool holds. Hold the difference here so it stays a
// decision and cannot drift into a fifth rule.
//
// One town holds a live session beside both seats the census leaves out: a
// dead-hooked seat (a crashed polecat whose bead is still hooked) and a landing
// seat (submitted work the landing worker has not closed). The census counts
// the session alone; a rule that counted either seat would read a total of 2
// or 3.
func TestLivePolecatCensusCountsOnlyTheLiveSession(t *testing.T) {
	t.Parallel()
	s := newSeatShadowTown(t, seatShadowSettings, []sessionEnv{
		{rig: "gastown", name: "ruby", agent: "deepseek-flash"},
	})
	s.agentBead("gastown", "ember", "deepseek-flash", "gt-ember") // dead-hooked: hook set, session gone
	s.slung("gastown", "pike", "gt-pike")                         // landing: submitted, not landed

	byAgent, total, err := s.host.livePolecatCensus("", seatLivePool(t, seatShadowSettings))
	if err != nil {
		t.Fatalf("livePolecatCensus: %v", err)
	}
	if total != 1 || len(byAgent) != 1 || byAgent["deepseek-flash"] != 1 {
		t.Fatalf("census = %v (total %d), want the live session alone {deepseek-flash:1} (total 1)", byAgent, total)
	}
}

// The doc comment on livePolecatCensus names the seats it leaves out with the
// same literals poolOccupancy documents them under, so a state renamed on one
// side is a visible mismatch in review.
func TestLivePolecatCensusDocNamesTheSeatsItLeavesOut(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "patrol_scan_seatcap.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing patrol_scan_seatcap.go: %v", err)
	}
	doc := ""
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "livePolecatCensus" && fn.Doc != nil {
			doc = fn.Doc.Text()
			break
		}
	}
	if doc == "" {
		t.Fatal("livePolecatCensus has no doc comment in patrol_scan_seatcap.go")
	}
	for _, seat := range []string{"DeadHooked", "Landing", "Claim"} {
		if !strings.Contains(doc, seat) {
			t.Errorf("the livePolecatCensus doc does not name %s among the seats it leaves out:\n%s", seat, doc)
		}
	}
}
