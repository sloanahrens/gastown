package dashboard

import (
	"strings"
	"testing"
)

// The Rigs panel names each rig's theme, with a few of its names in the cell's
// tooltip, so a short polecat name elsewhere on the page can be read back to
// the rig it belongs to (gt-yieek).
func TestRigsPaneNamesEachRigsTheme(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderRigs")
	for _, want := range []string{
		`["Names", "the name theme this rig draws its polecats from; hover for a few of its names", 0]`,
		`tr.append(el("td", "", r.name), themeCell(r));`,
		`const themeCell = r => {`,
		`td.title = (r.names || []).length ? r.theme + ": " + r.names.join(", ") : r.theme;`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderRigs has no %q", want)
		}
	}

	// A rig with no theme of its own is a dash, the way the panel shows every
	// reading it does not have, and says which rig that is.
	if !strings.Contains(draw, `el("td", "title", r.theme || "–")`) {
		t.Error("a rig with no theme does not fall back to a dash")
	}
}

// Polecats are shown by their short name, because each rig names its polecats
// from its own theme — except a short name that more than one rig in view
// uses, which without its rig names none of them (gt-yieek).
func TestPolecatNamesAreShortUnlessTwoRigsShareOne(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	helper := pageFunc(t, "polecatNames")
	for _, want := range []string{
		"const rigs = new Map();", // short name -> the rigs using it
		`return name && s && s.size > 1 && rig ? rig + "/" + name : (name || "");`,
		`rigOf: name => {`,
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("polecatNames has no %q", want)
		}
	}

	// The whole page names polecats through it: the seats pane, and the Landings
	// pane, which hands the table its names.
	for _, pane := range []string{"renderSeats", "renderTrend"} {
		if body := pageFunc(t, pane); !strings.Contains(body, "polecatNames(") {
			t.Errorf("%s does not name its polecats through the shared helper", pane)
		}
	}

	// The rig is no longer decided by name one rig at a time: gastown used to
	// go unqualified while every other rig was prefixed.
	if strings.Contains(page, `p.rig === "gastown" ? p.name : p.rig + "/" + p.name`) {
		t.Error("a pane still special-cases one rig when naming a polecat")
	}

	// The short name is what shows; the rig the polecat is in stays on the cell.
	seats := pageFunc(t, "renderSeats")
	if want := `const nameCell = p => { const td = el("td", "", names.label(p.rig, p.name)); td.title = p.rig + "/" + p.name; return td; };`; !strings.Contains(seats, want) {
		t.Errorf("a polecat's cell does not carry the full rig/name: %s", want)
	}

	landing := pageFunc(t, "landingTable")
	for _, want := range []string{
		"const rig = r.rig || names.rigOf(who);",
		"by.textContent = names.label(rig, who);",
		"by.title = rig ? rig + \"/\" + who : who;",
	} {
		if !strings.Contains(landing, want) {
			t.Errorf("the landings table has no %q", want)
		}
	}
}
