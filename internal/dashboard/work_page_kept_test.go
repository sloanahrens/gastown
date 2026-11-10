package dashboard

import (
	"strings"
	"testing"
	"time"
)

// A seat the daemon keeps for the jev_rules battery draws in the live seats,
// not in the needs-a-look fold, and its age is the wait since its submit: it
// holds no bead, so the age is drawn from its state's own clock (gt-ddg96x).
func TestTheWorkPanelKeepsAKeptSeatOutOfTheLookFold(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	kept := seat("gastown", "beaver", "kept (jev)", "", "", "", 0)
	kept["state_since"] = time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	callWork(t, dom, workState([]any{kept}, nil))

	rows := workSeatRowsDrawn(t, dom)
	if got := strings.Join(statesOf(rows[1:], false), " "); got != "kept (jev)" {
		t.Errorf("the live seats read %q, want the kept seat drawn among them", got)
	}
	if got := strings.Join(statesOf(rows[1:], true), " "); got != "" {
		t.Errorf("the folded seats read %q, want none: a kept seat is not a look", got)
	}
	if text := workText(t, dom); strings.Contains(text, "needs a look") {
		t.Errorf("the panel folded a kept seat under needs a look: %q", text)
	}

	for _, row := range workRows(t, dom)[1:] {
		if row[0] != "beaver" {
			continue
		}
		if row[4] != "20m" {
			t.Errorf("the kept seat's age reads %q, want the 20m since its submit", row[4])
		}
		return
	}
	t.Error("the panel drew no row for the kept seat")
}
