package dashboard

import (
	"strings"
	"testing"
)

// The app power line is drawn inside the Cloud panel, above the patrol's
// findings and whatever the patrol's own state is: an app that is off is news
// even when the patrol has written no report. Off takes the amber and unknown the
// red, so neither reads as on, and the state's words come from the reader's.
func TestCloudPanelDrawsTheAppPowerLine(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderCloud")
	checks := strings.Index(draw, "drawCloudChecks(box, checks);")
	apps := strings.Index(draw, "drawAppPower(box, c.apps);")
	state := strings.Index(draw, `if (c.state === "missing" || c.state === "error")`)
	if checks < 0 || apps < checks || state < apps {
		t.Errorf("renderCloud draws the app power line out of place (checks %d, apps %d, state return %d)", checks, apps, state)
	}

	power := pageFunc(t, "drawAppPower")
	for _, want := range []string{
		`if (!p) return;`,
		`p.note ? "app power: unknown (" + p.note + ")" : "app power: no apps"`,
		`a.state === "on" ? "" : a.state === "off" ? "warnc" : "badc"`,
		`"since " + age(a.since) + " ago"`,
		`"by " + a.by`,
		`"last: " + [rel.at ? age(rel.at) + " ago" : "", rel.action || "", rel.reason || ""]`,
		`"ledger: " + a.release_note`,
		`"and " + p.more + " more apps"`,
	} {
		if !strings.Contains(power, want) {
			t.Errorf("drawAppPower has no %q", want)
		}
	}

	// The state's words: on, off in the marker's environment (or why it has none),
	// and unknown with its reason, never a bare on for an app the reader could not
	// place.
	state2 := pageFunc(t, "appPowerState")
	for _, want := range []string{
		`if (a.state === "on") return "on";`,
		`"off (" + (a.env || a.note || "unreadable marker") + ")"`,
		`"unknown (" + (a.note || "unreadable") + ")"`,
	} {
		if !strings.Contains(state2, want) {
			t.Errorf("appPowerState has no %q", want)
		}
	}
}

// The markers and ledgers are other accounts' files, so the line writes every
// cell with el() and parses no markup.
func TestAppPowerLineWritesTextNeverMarkup(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"drawAppPower", "appPowerState"} {
		if body := pageFunc(t, name); strings.Contains(body, "innerHTML") {
			t.Errorf("%s sets markup from a marker or ledger", name)
		}
	}
	power := pageFunc(t, "drawAppPower")
	for _, want := range []string{
		`el("span", "apname title", a.app)`,
		`el("span", "aptext", who.join(" · "))`,
		`el("span", "aprel", "last: "`,
	} {
		if !strings.Contains(power, want) {
			t.Errorf("drawAppPower no longer writes a field with el(): %q", want)
		}
	}
	for _, want := range []string{
		`.apow{display:flex;gap:8px;align-items:baseline;flex-wrap:wrap;margin-bottom:2px}`,
		`.aprel{margin-left:auto;color:var(--dim);overflow-wrap:anywhere}`,
	} {
		if !strings.Contains(string(indexHTML), want) {
			t.Errorf("index.html has no %q", want)
		}
	}
}
