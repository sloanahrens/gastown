package dashboard

import (
	"strings"
	"testing"
)

// There is no JS engine in the unit tier, so like the other page tests these
// pin the lines costSpend draws from, not a rendered DOM.

// The balance is split into cash and credit under the tile, the credit runway
// rides beside it, balance rises are listed by the kind the report gave them,
// and the spent tile reads the sampled ledger from its start date (gt-jrrrv).
func TestSpendPanelShowsCashVersusCreditAndTheCreditEvents(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "costSpend")
	for _, want := range []string{
		// credit > 0: cash and credit under the balance, and the runway.
		`money(d.cash) + " cash + " + money(d.credit) + " credit"`,
		`d.credit > 0 && cr != null`,
		`d.credit_runway_days_at_24h_pace`,
		`"credit lasts ~" + cr.toFixed(1) + "d, then cash"`,
		// credits non-empty: one dim line, each rise with its kind.
		`"credit": " credit"`,
		`"top-up": " top-up"`,
		`"credit+top-up": " credit + top-up"`,
		`(kind || " added")`,
		`" +" + money(c.amount)`,
		// the spent tile reads the ledger and names where it starts.
		`d.spent_ledger != null`,
		`"DeepSeek spent (sampled" + (since ? " since " + since[0] : "") + ")"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("costSpend does not draw the credit view: no %q", want)
		}
	}

	// A negative day is a balance rise, never called a top-up by the per-day
	// line: the credits line carries the kind.
	if strings.Contains(body, `v.spent < 0 ? "top-up"`) || strings.Contains(body, `v.spent < 0 && !Array.isArray`) {
		t.Error("the per-day line still calls a negative day a top-up")
	}
	if !strings.Contains(body, `v.spent < 0 ? "+" + money(-v.spent) + " added" : money(v.spent)`) {
		t.Error("the per-day line does not show a negative day as a rise")
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

	body := pageFunc(t, "costSpend")
	for _, want := range []string{
		// cash and credit lines need both fields.
		`d.cash != null && d.credit != null`,
		// credits must be an array before it is read.
		`Array.isArray(d.credits) ?`,
		// the old tile stays only when there is no ledger total.
		`} else add(money(d.spent_total), "DeepSeek spent since top-up")`,
		// a ledger_since that is not a date is left out of the label.
		`/^\d{4}-\d{2}-\d{2}/.exec(String(d.ledger_since || ""))`,
		// no runway number, no runway line.
		`cr != null`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("costSpend does not tolerate an older report: no %q", want)
		}
	}
}
