package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// The page folds every section of its grid (gt-5de3i): one foldable() owns the
// caret, aria-expanded, the keyboard, the saved choice and the one line a
// closed section shows. These tests drive that through the page itself — the
// stub browser builds the markup's sections, so each one is a real element with
// a real heading — rather than reading index.html's source, because a fold that
// is remembered and a fold that only mentions being remembered look the same as
// text.

// foldDefaultOpen is the sections the page starts open for a reader with no
// saved choice. The list is a constant in the page's script and the operator's
// to edit; this test pins what the bead asked for, and the rest of this file
// reads the page's own list rather than assuming it.
var foldDefaultOpen = []string{"attention", "overseer", "cost", "work", "landings"}

// foldIsDefaultOpen reports whether key is one of the sections the page opens.
func foldIsDefaultOpen(key string) bool {
	for _, k := range foldDefaultOpen {
		if k == key {
			return true
		}
	}
	return false
}

// foldView is one section's fold as a test reads it back: the class the section
// wears, the header's aria-expanded, role and caret, and the summary line a
// closed section shows in the note's place. NoteHidden is nil for a section
// whose heading carries no note span at all.
type foldView struct {
	Collapsed     bool   `json:"collapsed"`
	Aria          string `json:"aria"`
	Role          string `json:"role"`
	Caret         string `json:"caret"`
	Summary       string `json:"summary"`
	SummaryHidden bool   `json:"summaryHidden"`
	NoteHidden    *bool  `json:"noteHidden"`
	Title         string `json:"title"`
}

// fold reads one section's fold back by its key. A key the page never folded —
// a section whose markup lost its data-fold — fails here rather than reading as
// a zero value.
func (p *pageDOM) fold(t *testing.T, key string) foldView {
	t.Helper()

	v, err := p.vm.RunString("JSON.stringify(__foldRead(" + strconv.Quote(key) + "))")
	if err != nil {
		t.Fatalf("reading the %s section's fold: %v", key, err)
	}
	if v.String() == "null" {
		t.Fatalf("the page folds no section under the key %q", key)
	}
	var f foldView
	if err := json.Unmarshal([]byte(v.String()), &f); err != nil {
		t.Fatalf("reading the %s section's fold: %v", key, err)
	}
	return f
}

// foldKeys is every key the page folded its sections under, in the order the
// markup declares them.
func (p *pageDOM) foldKeys(t *testing.T) []string {
	t.Helper()

	v, err := p.vm.RunString("JSON.stringify(__foldKeys())")
	if err != nil {
		t.Fatalf("reading the page's fold keys: %v", err)
	}
	var keys []string
	if err := json.Unmarshal([]byte(v.String()), &keys); err != nil {
		t.Fatalf("reading the page's fold keys: %v", err)
	}
	return keys
}

// press is one reader input on a section's header: how is "click", "enter" or
// "space", and the page hears the click or the key the browser would deliver.
func (p *pageDOM) press(t *testing.T, key, how string) {
	t.Helper()

	if _, err := p.vm.RunString("__pressFold(" + strconv.Quote(key) + ", " + strconv.Quote(how) + ")"); err != nil {
		t.Fatalf("pressing %s on the %s section: %v", how, key, err)
	}
}

// wantFold checks one section's fold against the state asked for: the class,
// the caret and the aria-expanded the header carries for a closed or open
// section. The three are what a reader sees and what a screen reader hears, and
// they must never disagree.
func wantFold(t *testing.T, got foldView, key string, closed bool) {
	t.Helper()

	if got.Collapsed != closed {
		t.Errorf("the %s section is collapsed=%v, want %v", key, got.Collapsed, closed)
	}
	wantAria, wantCaret := "true", "▾"
	if closed {
		wantAria, wantCaret = "false", "▸"
	}
	if got.Aria != wantAria {
		t.Errorf("the %s section's aria-expanded is %q, want %q", key, got.Aria, wantAria)
	}
	if got.Caret != wantCaret {
		t.Errorf("the %s section's caret reads %q, want %q", key, got.Caret, wantCaret)
	}
	if got.Role != "button" {
		t.Errorf("the %s section's heading has role %q, want button", key, got.Role)
	}
}

// Every section of the grid carries a key of its own, and the page folds each
// one: two sections sharing a key would save one choice over the other, and a
// section with none could not remember anything.
func TestFoldEverySectionHasItsOwnKey(t *testing.T) {
	t.Parallel()

	specs := pageSections(t)
	if len(specs) == 0 {
		t.Fatal("index.html's grid holds no sections")
	}
	seen := map[string]bool{}
	for _, sp := range specs {
		if sp.Key == "" {
			t.Errorf("the section %q carries no data-fold key", sp.Head)
			continue
		}
		if seen[sp.Key] {
			t.Errorf("two sections share the key %q", sp.Key)
		}
		seen[sp.Key] = true
	}
	dom := newPageDOM(t)
	keys := dom.foldKeys(t)
	if len(keys) != len(specs) {
		t.Fatalf("the page folded %d sections, the markup carries %d: %v", len(keys), len(specs), keys)
	}
	for i, sp := range specs {
		if keys[i] != sp.Key {
			t.Errorf("section %d folds under %q, its markup says %q", i, keys[i], sp.Key)
		}
	}
}

// A reader with no saved choice sees the sections the page opens open, and
// every other section closed.
func TestFoldDefaultsFollowThePagesOwnList(t *testing.T) {
	t.Parallel()

	specs := pageSections(t)
	dom := newPageDOM(t)
	for _, sp := range specs {
		wantClosed := !foldIsDefaultOpen(sp.Key) || sp.DefaultCollapsed
		wantFold(t, dom.fold(t, sp.Key), sp.Key, wantClosed)
	}
}

// The header is a button: a click, Enter or Space folds the section, and
// aria-expanded and the caret move with it in both directions.
func TestFoldHeaderTogglesWithClickEnterAndSpace(t *testing.T) {
	t.Parallel()

	for _, how := range []string{"click", "enter", "space"} {
		how := how
		t.Run(how, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			before := dom.fold(t, "cost")
			dom.press(t, "cost", how)
			after := dom.fold(t, "cost")
			wantFold(t, after, "cost", !before.Collapsed)
			dom.press(t, "cost", how)
			wantFold(t, dom.fold(t, "cost"), "cost", before.Collapsed)
		})
	}
}

// clickWithin is a click on one of the heading's own controls — the Attention
// chips — which the browser would deliver with the control as the event's
// target, its click bubbling to the header.
func (p *pageDOM) clickWithin(t *testing.T, key, id string) {
	t.Helper()

	js := `(function () {
		var h = __find(__fold(` + strconv.Quote(key) + `), ":scope > h2");
		__fireOn(h, "click", { type: "click", target: __byId[` + strconv.Quote(id) + `] });
	})()`
	if _, err := p.vm.RunString(js); err != nil {
		t.Fatalf("clicking #%s in the %s heading: %v", id, key, err)
	}
}

// A click that lands on one of the heading's own controls belongs to the
// control: the Attention chips filter the panel, and folding it would hide the
// list they just chose.
func TestFoldLeavesAClickOnItsControlsAlone(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	before := dom.fold(t, "attention")
	dom.clickWithin(t, "attention", "attentionchips")
	wantFold(t, dom.fold(t, "attention"), "attention", before.Collapsed)
}

// A pane that opts out with data-default-collapsed starts closed even where the
// default list would open it: the attribute is the pane's own word against the
// list, and it holds until the reader opens the pane.
func TestFoldPaneThatOptsOutStaysClosed(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if _, err := dom.vm.RunString(`(function () {
		var sec = __el("section");
		sec.id = "optsout";
		__byId["optsout"] = sec;
		sec.setAttribute("data-default-collapsed", "");
		var h = __el("h2");
		sec.append(h);
		foldable(sec, {key: "optsout", head: h, defaultOpen: true});
	})()`); err != nil {
		t.Fatalf("folding a pane that opts out: %v", err)
	}
	if got := dom.element(t, "optsout"); got.Class != "collapsed" {
		t.Errorf("a pane that opts out of an open default wears %q, want collapsed", got.Class)
	}
}

// A saved choice wins over the default in both directions, and a toggle writes
// the choice where the next load will read it.
func TestFoldSavedChoiceWinsOverTheDefault(t *testing.T) {
	t.Parallel()

	dom := newPageDOMStoring(t, map[string]string{"gt-fold": `{"work":true,"rigs":false}`})
	wantFold(t, dom.fold(t, "work"), "work", true)  // the default is open
	wantFold(t, dom.fold(t, "rigs"), "rigs", false) // the default is closed

	dom.press(t, "cost", "click")
	stored := dom.storage(t)
	var fold map[string]bool
	if err := json.Unmarshal([]byte(stored["gt-fold"]), &fold); err != nil {
		t.Fatalf("the page's saved folds: %v (storage: %v)", err, stored)
	}
	if !fold["cost"] {
		t.Errorf("closing the cost section saved %v, want cost:true", fold)
	}
	again := newPageDOMStoring(t, stored)
	wantFold(t, again.fold(t, "cost"), "cost", true)
}

// A choice saved before the sections had keys — in gt-collapsed, under the
// heading's own text — still applies where the new key says nothing, so a
// reader who folded a pane yesterday finds it folded today.
func TestFoldChoiceFromBeforeTheKeysStillApplies(t *testing.T) {
	t.Parallel()

	legacy := newPageDOMStoring(t, map[string]string{"gt-collapsed": `{"Work":true,"Rigs":false}`})
	wantFold(t, legacy.fold(t, "work"), "work", true)  // the default is open
	wantFold(t, legacy.fold(t, "rigs"), "rigs", false) // the legacy entry read it open

	// A new key with a choice of its own is the one that counts.
	both := newPageDOMStoring(t, map[string]string{
		"gt-collapsed": `{"Work":true}`,
		"gt-fold":      `{"work":false}`,
	})
	wantFold(t, both.fold(t, "work"), "work", false)
}

// Storage that throws — private mode, a locked-down browser — leaves the page
// on its defaults and folding working for the session, and saves nothing it
// cannot save.
func TestFoldSurvivesStorageThatThrows(t *testing.T) {
	t.Parallel()

	dom := newPageDOMNoStorage(t)
	wantFold(t, dom.fold(t, "rigs"), "rigs", true)            // the default is closed
	wantFold(t, dom.fold(t, "attention"), "attention", false) // the default is open

	dom.press(t, "attention", "click")
	wantFold(t, dom.fold(t, "attention"), "attention", true)
	dom.press(t, "rigs", "enter")
	wantFold(t, dom.fold(t, "rigs"), "rigs", false)
}

// The summary line is the closed header's own text: the note's place is taken
// by it while the section is closed and given back when it opens.
func TestFoldSummaryShowsOnlyWhileClosed(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderAttention", map[string]any{
		"attention": map[string]any{"rows": []any{
			map[string]any{"kind": "question", "owner": "operator", "class": "human", "id": "gt-aaaaa", "title": "which one?", "age_sec": 30},
			map[string]any{"kind": "item", "owner": "operator", "class": "pipeline", "rule": "bead-stale", "level": "info", "id": "gt-bbbbb", "age_sec": 30},
		}},
	})

	open := dom.fold(t, "attention")
	if !open.SummaryHidden || open.Summary != "" {
		t.Errorf("an open section shows a summary line %q (hidden=%v)", open.Summary, open.SummaryHidden)
	}
	if open.NoteHidden != nil && *open.NoteHidden {
		t.Error("an open section hides its own note")
	}

	dom.press(t, "attention", "click")
	closed := dom.fold(t, "attention")
	if closed.SummaryHidden || closed.Summary == "" {
		t.Fatalf("a closed section shows no summary line (hidden=%v)", closed.SummaryHidden)
	}
	if closed.NoteHidden != nil && !*closed.NoteHidden {
		t.Error("a closed section still shows the note the summary stands in for")
	}
	if want := "1 need you · 1 in the pipeline"; closed.Summary != want {
		t.Errorf("the closed attention section reads %q, want %q", closed.Summary, want)
	}
}

// A section whose note is only a read age says something a reader can use
// instead: the counts from the same reading the panel draws. The work section
// takes them from the top strip's fields, the attention section from the fields
// the panel counts, and Cost has none of its own yet.
func TestFoldSummaryComesFromTheReading(t *testing.T) {
	t.Parallel()

	// The reader folds the sections first: the summary is what a closed section
	// says when a reading arrives behind it, not something a toggle recomputes.
	dom := newPageDOM(t)
	dom.press(t, "work", "click")
	dom.press(t, "attention", "click")
	dom.press(t, "cost", "click")

	if _, err := dom.vm.RunString(`state = {
		summary_at: "2026-10-29T00:00:00Z",
		summary: {ready_to_land: 1},
		polecats: [
			{rig: "gastown", name: "angharad", state: "working"},
			{rig: "gastown", name: "capable", state: "working"},
			{rig: "gastown", name: "ivy", state: "parked", rig_parked: true},
			{rig: "gastown", name: "merlin", state: "queued"},
			{rig: "gastown", name: "wren", state: "working", rig_parked: true}
		],
		queue: {at: "2026-10-29T00:00:00Z"},
		attention: {rows: []}
	}; renderAll();`); err != nil {
		t.Fatalf("rendering the page with a reading: %v", err)
	}

	if got, want := dom.fold(t, "work").Summary, "2 working · 1 waiting to land"; got != want {
		t.Errorf("the closed work section reads %q, want %q", got, want)
	}
	// The summary's working count is the top strip's: both leave out the polecat
	// on a parked rig, so the strip and the section agree on the same number.
	if strip := dom.element(t, "strip").Text; !strings.Contains(strip, "2 working") {
		t.Errorf("the top strip reads %q, want its working count to be the summary's 2", strip)
	}
	// Attention's own note is the summary when it has rows; with none it says
	// so rather than leaving the header blank of a figure.
	if got, want := dom.fold(t, "attention").Summary, "nothing needs you"; got != want {
		t.Errorf("the closed attention section with no rows reads %q, want %q", got, want)
	}
	// A closed Cost section's summary is the Cost header's line. This reading has
	// no spend report, so the header names that rather than going quiet.
	if got := dom.fold(t, "cost"); got.Summary != "no spend read" || got.SummaryHidden {
		t.Errorf("the closed cost section's summary is %q, want the header naming the missing spend report", got.Summary)
	}
}

// A closed section is still drawn into: the render functions write their panes
// whatever the fold says, so opening it shows the last reading with no fetch.
func TestFoldClosedSectionKeepsUpWithTheReading(t *testing.T) {
	t.Parallel()

	read := func(name string) map[string]any {
		return map[string]any{
			"summary_at": "2026-10-29T00:00:00Z",
			"summary":    map[string]any{"ready_to_land": 1},
			"polecats": []any{
				map[string]any{"rig": "gastown", "name": name, "state": "working"},
			},
		}
	}
	dom := newPageDOM(t)
	requests := pageNumber(t, dom, "__fetches.length")
	dom.press(t, "work", "click") // closed by the reader
	dom.call(t, "renderWork", read("angharad"))
	first := dom.element(t, "work")
	if first.Kids == 0 {
		t.Fatal("a closed section's body was not drawn into")
	}
	dom.call(t, "renderWork", read("capable"))
	second := dom.element(t, "work")
	if second.Text == first.Text {
		t.Fatalf("a closed section's body stopped following the reading: %q", second.Text)
	}

	dom.press(t, "work", "click") // opened again
	if dom.fold(t, "work").Collapsed {
		t.Error("the work section did not open again")
	}
	if got := dom.fold(t, "work").Summary; got != "" {
		t.Errorf("the opened work section still shows the summary %q", got)
	}
	// Closing, drawing and opening the section asked the server for nothing: the
	// last reading is what it shows.
	if got := pageNumber(t, dom, "__fetches.length") - requests; got != 0 {
		t.Errorf("closing, drawing and opening the work section sent %d request(s), want none", got)
	}
}

// Setup that runs twice must not fold a section twice: a second caret or a
// second click handler on the same header would make one click fold and open
// it again, so the reader could never close it.
func TestFoldSetupRunTwiceFoldsOnce(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if _, err := dom.vm.RunString("setupCollapse(); setupCollapse();"); err != nil {
		t.Fatalf("running the page's setup a second time: %v", err)
	}
	carets := pageExpr(t, dom, `(function () {
		var h = __find(__fold("work"), ":scope > h2"), n = 0;
		__walk(h, function (c) { if (__cls(c, "caret")) n++; });
		return String(n);
	})()`)
	if carets != "1" {
		t.Fatalf("the work header holds %s carets after setup ran twice, want 1", carets)
	}

	dom.press(t, "work", "click")
	wantFold(t, dom.fold(t, "work"), "work", true)
}

// With every owner chip switched off the Attention panel says so, and so does
// the closed section's summary: a blank line would read as nothing waiting.
func TestFoldAttentionSummaryWhenEveryOwnerChipIsOff(t *testing.T) {
	t.Parallel()

	dom := newPageDOMStoring(t, map[string]string{"gt-attention-owners": "[]"})
	dom.press(t, "attention", "click")
	if _, err := dom.vm.RunString(`state = {
		summary_at: "2026-10-29T00:00:00Z",
		attention: {rows: [{owner: "operator", kind: "question", class: "needs"}]}
	}; renderAll();`); err != nil {
		t.Fatalf("rendering a reading with every owner chip off: %v", err)
	}

	if got, want := dom.fold(t, "attention").Summary, "every owner chip is off"; got != want {
		t.Errorf("the closed attention section reads %q, want %q", got, want)
	}
}
