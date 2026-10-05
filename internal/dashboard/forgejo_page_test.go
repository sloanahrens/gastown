package dashboard

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pageFunc returns the body of one top-level function of the embedded page.
// The page is data-driven, so what its wiring reads and calls is what these
// tests can hold it to (gt-fn9e6.48).
func pageFunc(t *testing.T, name string) string {
	t.Helper()

	page := string(indexHTML)
	start := strings.Index(page, "function "+name+"(")
	if start < 0 {
		t.Fatalf("index.html has no %s", name)
	}
	body := page[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	return body
}

// The Actions section is drawn above the feed and left out entirely when the
// server sends no actions — an older server, or a reader with nothing to
// report — so the pane is then the feed it was before (gt-fn9e6.48).
func TestForgejoActionsSectionSitsAboveTheFeedAndDegradesQuietly(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	acts := strings.Index(page, `id="forgejoacts"`)
	feed := strings.Index(page, `id="forgejo"`)
	if acts < 0 {
		t.Fatal("index.html has no Actions section (#forgejoacts)")
	}
	if feed < 0 {
		t.Fatal("index.html has no feed (#forgejo)")
	}
	if acts > feed {
		t.Error("the Actions section does not sit above the feed")
	}

	draw := pageFunc(t, "renderForgejoActs")
	if !strings.Contains(draw, "if (!a) return;") {
		t.Error("renderForgejoActs does not leave the section out when there are no actions")
	}
	if !strings.Contains(draw, "replaceChildren") {
		t.Error("renderForgejoActs does not clear the section it redraws")
	}
	if !strings.Contains(draw, `el("div", "actsidle", "idle")`) {
		t.Error("an idle section does not say so in one muted line")
	}

	render := pageFunc(t, "renderForgejo")
	if !strings.Contains(render, "renderForgejoActs(f.actions)") {
		t.Error("renderForgejo does not pass the state's actions to the section")
	}
	if !strings.Contains(render, "renderForgejoActs(null)") {
		t.Error("renderForgejo does not clear the section when the pane has no feed at all")
	}
}

// The stats line and the rows are drawn from the fields the backend sends, and
// the status colours reuse the page's own tokens: success green, failure red,
// running or waiting amber, cancelled or skipped muted (gt-fn9e6.48).
func TestForgejoActionRowsCarryTheStatsAndTheStatusColours(t *testing.T) {
	t.Parallel()

	sum := pageFunc(t, "actsSum")
	for _, want := range []string{"running", "queued", "ok", "failed", "median"} {
		if !strings.Contains(sum, want) {
			t.Errorf("the stats line does not mention %s", want)
		}
	}

	row := pageFunc(t, "actRow")
	if !strings.Contains(row, "ACT_CLASS[st]") {
		t.Error("the row does not colour its status dot")
	}
	for _, want := range []string{"a.repo", "a.bead", "a.duration_secs", ".title"} {
		if !strings.Contains(row, want) {
			t.Errorf("the row does not use %s", want)
		}
	}

	page := string(indexHTML)
	for _, want := range []string{
		".act-success{color:var(--green)}",
		".act-failure{color:var(--red)}",
		".act-running,.act-waiting,.act-blocked{color:var(--amber)}",
		".act-cancelled,.act-skipped{color:var(--dim)}",
		".actsrow.act-failure,.actsrow.act-failure .an{color:var(--red)}",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no colour rule %q", want)
		}
	}
}

// The repo cell shows the name after the last "/": the owner is the same for
// every run the pane reads, and the name is what tells rigs apart. The column
// is sized in characters — the longest repo name here plus the one a shortened
// cell needs before the ellipsis rule can bite — and the cell keeps the full
// owner/name on hover, which is the only place the owner survives (gt-fn9e6.54).
func TestForgejoActionRepoCellShowsTheNameWithoutItsOwner(t *testing.T) {
	t.Parallel()

	// The longest repo name the panel reads, so the column's width is checked
	// against what it must hold rather than against the number in the CSS.
	const longest = "organic-mechanic"

	short := pageFunc(t, "repoShort")
	for _, want := range []string{`lastIndexOf("/")`, "slice(cut + 1)"} {
		if !strings.Contains(short, want) {
			t.Errorf("repoShort does not split the name off at its owner: %s", want)
		}
	}

	row := pageFunc(t, "actRow")
	if !strings.Contains(row, `el("span", "arepo", repo)`) {
		t.Error("the repo cell does not hold the shortened name")
	}
	if !strings.Contains(row, "repoCell.title = a.repo") {
		t.Error("the shortened cell does not keep the full owner/name on hover")
	}

	page := string(indexHTML)
	cols := regexp.MustCompile(`\n\.actsrow\{[^}]*grid-template-columns:\s*10px\s+(\d+)ch\s`).FindStringSubmatch(page)
	if cols == nil {
		t.Fatal("the repo column is not sized in characters, so nothing holds it to the longest name")
	}
	if n, _ := strconv.Atoi(cols[1]); n < len(longest)+1 {
		t.Errorf("the repo column is %sch, too narrow for %q plus a character of slack", cols[1], longest)
	}
}

// A running row's elapsed ticks every second from the start time, and the cells
// carry the stamp they count from so the ticker moves them without a redraw
// (gt-fn9e6.48).
func TestForgejoRunningActionElapsedTicksEachSecond(t *testing.T) {
	t.Parallel()

	row := pageFunc(t, "actRow")
	if !strings.Contains(row, "dataset.since") {
		t.Error("the running row's elapsed cell carries no start to tick from")
	}

	page := string(indexHTML)
	if !strings.Contains(page, `document.querySelectorAll("#forgejoacts [data-since]")`) {
		t.Error("no ticker walks the actions section's aged cells")
	}
	tick := page[strings.Index(page, `document.querySelectorAll("#forgejoacts [data-since]")`):]
	if end := strings.Index(tick, "}, 1000);"); end < 0 {
		t.Error("the ticker does not run once a second")
	}
}

// The feed folds on its own header below the Actions section, and the choice is
// remembered per viewer. Storage that throws must not take the pane down: the
// feed starts expanded and still folds for the session. Without an Actions
// section the header is hidden and the feed is drawn as it was before, which is
// what a server that sends no actions gets (gt-fn9e6.48).
func TestForgejoFeedFoldsOnItsOwnHeader(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	head := strings.Index(page, `id="forgejofeedhead"`)
	feed := strings.Index(page, `id="forgejo"`)
	if head < 0 {
		t.Fatal("index.html has no feed header (#forgejofeedhead)")
	}
	if head > feed {
		t.Error("the feed header does not sit above the feed")
	}
	if !strings.Contains(page, "#forgejo.folded{display:none}") {
		t.Error("a folded feed is not hidden")
	}
	if !strings.Contains(page, `try { feedFolded = localStorage.getItem("gt-forgejo-feed") === "folded"; } catch (e) {}`) {
		t.Error("the remembered fold is not read, or not read defensively")
	}
	if !strings.Contains(page, "let feedFolded = false") {
		t.Error("the feed does not start expanded")
	}

	fold := pageFunc(t, "foldFeed")
	if !strings.Contains(fold, "$(\"forgejofeedhead\").hidden = !hasActions") {
		t.Error("the header is not hidden when the pane has no Actions section")
	}
	if !strings.Contains(fold, "!!hasActions && feedFolded") {
		t.Error("the feed is not left expanded when the pane has no Actions section")
	}
	toggle := pageFunc(t, "toggleFeed")
	if !strings.Contains(toggle, `localStorage.setItem("gt-forgejo-feed"`) {
		t.Error("folding does not remember the choice")
	}
	if !strings.Contains(page, "foldFeed(!!f.actions);") {
		t.Error("renderForgejo does not fold the feed with the section it sits under")
	}
	if !strings.Contains(page, `$("forgejofeedhead").addEventListener("click", toggleFeed);`) {
		t.Error("the header does not fold the feed when it is clicked")
	}

	paint := pageFunc(t, "paintFeedHead")
	for _, want := range []string{"caret", "events"} {
		if !strings.Contains(paint, want) {
			t.Errorf("the collapsed header does not show %s", want)
		}
	}
}
