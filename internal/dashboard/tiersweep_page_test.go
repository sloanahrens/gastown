package dashboard

import (
	"strings"
	"testing"
)

// The Tier sweeps pane is gone from the page: the System updates pane replaced
// it (gt-gfglv0). The feed still carries the tiersweep section for other
// readers (TestTierSweepAndUpdateSectionsReachTheFeed; its trend JSON is
// TestTierSweepTrendReachesThePageJSON), but the page draws no pane for it: no
// fold under the key, no element the pane drew into, and none of its script.
// The first check finds a pane the page does have, so a lookup that found
// nothing cannot pass the checks that follow.
func TestPageHoldsNoTierSweepsPane(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if got := pageExpr(t, dom, `document.getElementById("systemupdates") !== null`); got != "true" {
		t.Fatal("the stub's element lookup finds no #systemupdates, so the absence checks below prove nothing")
	}
	for _, id := range []string{"tiersweep", "tiersweepnote", "tiersweepico"} {
		if got := pageExpr(t, dom, `document.getElementById(`+quoteJS(id)+`) === null`); got != "true" {
			t.Errorf("the page still has #%s", id)
		}
	}
	for _, key := range dom.foldKeys(t) {
		if key == "tiersweep" {
			t.Error(`the page still folds a section under the key "tiersweep"`)
		}
	}
	page := string(indexHTML)
	for _, gone := range []string{"TIERSWEEP_TIERS", "renderTierSweep", "tierSweepIcon", "paintTierIcon", "sweepico"} {
		if strings.Contains(page, gone) {
			t.Errorf("index.html still carries %q", gone)
		}
	}
}
