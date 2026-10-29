package dashboard

import (
	"strings"
	"testing"
)

// The Tier sweeps pane is kept collapsed, so the one thing it must say without
// being opened is the verdict of the last run. The icon therefore lives in the
// section's own title row — a collapsed section hides everything but its h2 —
// right after the caret the fold puts first and ahead of the "Tier sweeps"
// label, so the caret, the icon and the label read in that order (gt-5qk83).
// The order is read off the page's heading, not off its source text.
func TestTierSweepIconSitsInTheTitleRow(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	row := pageExpr(t, dom, `(function () {
		var h = __find(__fold("tiersweep"), ":scope > h2");
		var kids = h ? h.children : [];
		return [
			kids[0] && __cls(kids[0], "caret") ? "caret" : "-",
			kids[1] && kids[1] === __byId["tiersweepico"] ? "tiersweepico" : "-",
			h && h.textContent.indexOf("Tier sweeps") >= 0 ? "label" : "-"
		].join(",");
	})()`)
	if want := "caret,tiersweepico,label"; row != want {
		t.Errorf("the Tier sweeps title row reads %q, want %q: the caret, then the icon, then the label", row, want)
	}
}

// A tier a sweep did not run keeps the result of the sweep that last did, the
// rule the daemon's attention list holds to: a newer shell-only GREEN must not
// hide an integration RED still standing. The reader hands the page its sweeps
// newest first, so the fold must keep the first verdict it sees for a tier
// (gt-5qk83).
func TestTierSweepIconKeepsEachTiersLastResult(t *testing.T) {
	t.Parallel()

	latest := pageFunc(t, "tierSweepLatest")
	for _, want := range []string{"for (const row of sweeps", "(row.stages || [])", "if (!latest[x.tier])", "latest[x.tier] = {verdict: x.verdict, at: row.at}"} {
		if !strings.Contains(latest, want) {
			t.Errorf("tierSweepLatest does not hold each tier's last result: no %q", want)
		}
	}

	icon := pageFunc(t, "tierSweepIcon")
	if !strings.Contains(icon, "tierSweepLatest(ts.sweeps)") {
		t.Error("the icon does not fold the sweeps per tier")
	}
}

// The icon is one glyph for the pane: green check when every tier is GREEN, red
// cross when any is RED, neutral dash when there is nothing to read or a tier
// has never reported. The tooltip names each tier's verdict and the sweep it
// came from. A sweep in flight is not a verdict (gt-5qk83).
func TestTierSweepIconFoldsToCheckCrossOrDash(t *testing.T) {
	t.Parallel()

	icon := pageFunc(t, "tierSweepIcon")
	for _, want := range []string{
		`glyph: "✓", cls: "good"`,
		`glyph: "✗", cls: "badc"`,
		`glyph: "–", cls: ""`,
		"TIERSWEEP_TIERS.every(",
		"TIERSWEEP_TIERS.some(",
		"sweepWhen(v.at)",
		"if (!ts) return none;",
		"if (ts.unavailable)",
		"if (!(ts.sweeps || []).length) return none;",
	} {
		if !strings.Contains(icon, want) {
			t.Errorf("the icon's logic does not mention %q", want)
		}
	}
	red := strings.Index(icon, `=== "RED"`)
	green := strings.Index(icon, `=== "GREEN"`)
	if red < 0 || green < 0 || red > green {
		t.Error("RED is not settled before GREEN, so a green tier could outrank a red one")
	}
	if strings.Contains(icon, "running") {
		t.Error("a sweep in flight changes the icon; the body already says running")
	}

	// Green needs every tier; a tier with no result denies it and the fold
	// falls to the neutral dash, whose class carries the h2's own muted colour.
	if !strings.Contains(icon, `every(tier => latest[tier] && latest[tier].verdict === "GREEN")`) {
		t.Error("a green check does not require a GREEN result for every tier")
	}
}

// The verdict reaches the title row on every path renderTierSweep takes, the
// empty ones included, so an unreadable log cannot leave a stale green behind.
// Green and red reuse the page's good and badc tokens; the neutral glyph keeps
// the title's muted colour and so carries no colour class of its own (gt-5qk83).
func TestTierSweepIconIsPaintedOnTheTitleRow(t *testing.T) {
	t.Parallel()

	paint := pageFunc(t, "paintTierIcon")
	for _, want := range []string{`$("tiersweepico")`, "ico.textContent = v.glyph", "ico.className = \"sweepico\"", "ico.title = v.title"} {
		if !strings.Contains(paint, want) {
			t.Errorf("paintTierIcon does not set the icon: no %q", want)
		}
	}

	render := pageFunc(t, "renderTierSweep")
	at := strings.Index(render, "paintTierIcon(ts)")
	if at < 0 {
		t.Fatal("renderTierSweep never paints the icon")
	}
	empty := strings.Index(render, "if (!ts)")
	if empty >= 0 && at > empty {
		t.Error("the icon is painted after the first empty path returns, so that path keeps a stale glyph")
	}

	page := string(indexHTML)
	if !strings.Contains(page, "paintTierIcon(null);") {
		t.Error("the icon is blank, not neutral, until the first push arrives")
	}
	if !strings.Contains(page, ".sweepico{") {
		t.Error("index.html has no rule for the title icon")
	}
	for _, want := range []string{".good{color:var(--green)}", ".badc{color:var(--red)}"} {
		if !strings.Contains(page, want) {
			t.Errorf("the icon does not reuse the page's verdict colour %q", want)
		}
	}
}
