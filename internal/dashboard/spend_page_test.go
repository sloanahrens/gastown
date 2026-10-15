package dashboard

import (
	"strings"
	"testing"
)

// The balance is split into cash and credit under the tile, the credit runway
// rides beside it, balance rises are listed by the kind the report gave them,
// and the spent tile reads the sampled ledger from its start date (gt-jrrrv).
func TestSpendPanelShowsCashVersusCreditAndTheCreditEvents(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderSpend")
	for _, want := range []string{
		// credit > 0: cash and credit under the balance, and the runway.
		`money(d.cash) + " cash + " + money(d.credit) + " credit"`,
		`d.credit > 0 && cr != null`,
		`d.credit_runway_days_at_24h_pace`,
		`"credit lasts ~" + cr.toFixed(1) + "d, then cash"`,
		// credits non-empty: one dim line, each rise with its kind.
		`d.credits`,
		`"credit": " credit"`,
		`"top-up": " top-up"`,
		`"credit+top-up": " credit + top-up"`,
		`(kind || " added")`,
		`" +" + money(c.amount)`,
		// the spent tile starts where the ledger does.
		`d.spent_ledger != null`,
		`"spent (sampled"`,
		`d.ledger_since`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderSpend does not draw the credit view: no %q", want)
		}
	}

	// A rise is never called a top-up by the per-day line once the report
	// classifies its rises: the only "top-up" left there is for an older report.
	if !strings.Contains(body, `v.spent < 0 && !Array.isArray(d.credits) ? "top-up"`) {
		t.Error("the per-day line still calls every negative day a top-up")
	}

	// The newest rise leads, like the per-day line.
	if !strings.Contains(body, `d.credits.slice().reverse()`) {
		t.Error("the credits line does not lead with the newest rise")
	}
}

// Every new field is optional: a report from before them gets the lump balance,
// the old spent tile and no credit lines, without a throw (gt-jrrrv).
func TestSpendPanelToleratesAReportWithoutTheCreditFields(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderSpend")
	for _, want := range []string{
		// cash and credit lines need both fields.
		`d.cash != null && d.credit != null`,
		// credits must be an array before it is read.
		`Array.isArray(d.credits) ?`,
		// the old tile stays when there is no ledger total.
		`else add(money(d.spent_total), "spent since top-up")`,
		// no runway number, no runway line.
		`cr != null`,
		`d.ledger_since ?`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderSpend does not tolerate an older report: no %q", want)
		}
	}
}
