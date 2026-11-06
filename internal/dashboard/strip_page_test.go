package dashboard

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The strip is the whole of the page's top: the day's landings, the seats, the
// polecats on the job, what is waiting to land, what needs the operator, and
// the host and the install — each drawn once and each off the reader that owns
// it, so the page cannot hold two pictures of one number (gt-ywlei).
func TestTheStripDrawsEachFactOnce(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderStrip")
	for _, want := range []string{
		// The landings off the Landings pane's own window.
		`const t = s.trend || null;`,
		`fact(box, t ? String(landed) : "–", "landed/24h",`,
		// The seats off the polecats the Polecats pane counts, and the queue
		// off the read that lists it.
		`seatFacts(s.models)`,
		`fact(box, s.polecats ? String(active) : "–", "working",`,
		`const rr = readOf(s, "ready");`,
		`fact(box, q == null ? "–" : String(q), "waiting to land",`,
		`oldest == null ? null : "oldest " + fmtDur(oldest)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}

	// Every fact is one drawing of a number the panes below also count, so the
	// strip is the only thing here that draws it: the tiles and the header's
	// own models text are gone, and the page draws the strip once per push.
	all := pageFunc(t, "renderAll")
	if !strings.Contains(all, "renderHealth(state.health); renderStrip(state); renderModelsPanel(state);") {
		t.Error("the page's redraw does not draw the one strip")
	}
	if strings.Contains(all, "renderTiles(state)") {
		t.Error("the page's redraw still draws the tiles the strip replaced")
	}
	if strings.Contains(string(indexHTML), "renderModels(") {
		t.Error("the page still draws the header's own models text")
	}
	if n := strings.Count(all, "renderStrip(state)"); n != 1 {
		t.Errorf("renderAll draws the strip %d times, want 1", n)
	}
	// The age of the oldest landing drifts between pushes, so the 15s tick
	// redraws the strip with the panes whose ages it carries.
	if !strings.Contains(string(indexHTML), "renderStrip(state); renderPolecats(state); renderBeads(state); renderTierSweep(state);") {
		t.Error("the 15s tick does not redraw the strip, so its ages freeze")
	}
}

// The strip wears the verdict the reader decided and holds no threshold of its
// own, so what the page calls an alarm is the rule this package runs against
// the readings it holds rather than a second copy of it here (gt-ywlei).
func TestTheStripWearsTheVerdictsTheReaderDecided(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderStrip")
	// A verdict the reader left out is a fact with nothing to act on, drawn in
	// the strip's own dim: read as absent rather than as the loudest reading,
	// which is what the empty string means to the page's classes.
	for _, want := range []string{
		`const a = s.strip || {};`,
		`const worst = s.polecats ? (a.working || "") : "";`,
		`{cls: worst,`,
		`{cls: a.waiting || "", tip: why}`,
		`{cls: q == null ? "" : (a.waiting || ""),`,
		`{cls: a.needs_you || "",`,
		`{cls: a.behind || "",`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}

	// Nothing here carries a threshold or a state's alarm of its own: a second
	// copy of the rule is how the page and the reader drift.
	page := string(indexHTML)
	for _, gone := range []string{"STRIP_ALARM", "POLE_ALARM", "wait_bad_secs", "wait_warn_secs", "behind_warn", "load_bad_cores", "load_warn_cores"} {
		if strings.Contains(page, gone) {
			t.Errorf("index.html still holds %q, which the reader decides", gone)
		}
	}
}

// The polecats the strip counts are the reader's own states, held in the page's
// tables rather than spelled out in the drawing: a state the reader adds
// reaches POLE_WORKING and POLE_WORD or the strip names it as one the page
// does not know. A state missing from them would vanish from the strip's
// number and from its title — the sign of a polecat nobody can see (gt-ywlei).
func TestTheStripAccountsForEveryStateTheReaderSends(t *testing.T) {
	t.Parallel()

	// Every state this package can put on a row (types.go).
	states := []string{
		StateWorking, StateQuiet, StateGating, StateQueued, StateSpawning, StateStalled,
		StateReviewNeeded, StateRecovery, StateNeedsHuman, StateNeedsOverseer, StateNeedsOperator,
		StateParked, StateIdle, StateUnknown,
	}
	working, words := pageTable(t, "POLE_WORKING"), pageTable(t, "POLE_WORD")
	// A state whose own name is a JS identifier (needs-human) is a quoted key
	// in the page's object literal.
	for _, table := range []map[string]string{working, words} {
		for key, value := range table {
			if bare := strings.Trim(key, `"`); bare != key {
				delete(table, key)
				table[bare] = value
			}
		}
	}

	// The page carries no state the reader cannot send: a table key of its own
	// is a rule nothing will ever exercise, and a misspelt one is a state that
	// silently stops being counted.
	for _, table := range []struct {
		name  string
		pairs map[string]string
	}{{"POLE_WORKING", working}, {"POLE_WORD", words}} {
		for state := range table.pairs {
			if !contains(states, state) {
				t.Errorf("%s names %q, which this package cannot put on a polecat", table.name, state)
			}
		}
	}
	for _, state := range states {
		if _, ok := working[state]; !ok {
			t.Errorf("POLE_WORKING does not say whether %q is work in progress", state)
		}
	}

	// Which states are work in progress — a session is on somebody's bead.
	wantWorking := []string{StateWorking, StateQuiet, StateGating}
	for _, state := range states {
		want := "0"
		if contains(wantWorking, state) {
			want = "1"
		}
		if working[state] != want {
			t.Errorf("POLE_WORKING says %q is %q, want %q", state, working[state], want)
		}
	}
	// The states the title names beside the count: the ones that are not work
	// in progress and the quiet ones, but never the working number's own states,
	// which the number already counts.
	for _, state := range []string{StateQueued, StateSpawning, StateStalled, StateReviewNeeded,
		StateRecovery, StateNeedsHuman, StateNeedsOverseer, StateNeedsOperator, StateParked, StateUnknown, StateQuiet} {
		if words[state] == "" {
			t.Errorf("POLE_WORD names %q nothing, so its polecats would vanish from the title", state)
		}
	}
	for _, state := range []string{StateWorking, StateGating, StateIdle} {
		if _, ok := words[state]; ok {
			t.Errorf("POLE_WORD gives %q a word, but the working number already counts it", state)
		}
	}

	// The drawing reads those tables and spells out no state of its own: a
	// second copy of the vocabulary is how the page and the reader drift.
	body := pageFunc(t, "renderStrip")
	for _, want := range []string{
		`polecatsWorking(pcs)`,
		`Object.keys(POLE_WORD).filter(k => n(k)).map(k => n(k) + " " + POLE_WORD[k])`,
		// A state in no table at all is named rather than dropped.
		`const strangers = live.filter(p => !(p.state in POLE_WORKING)).length;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}
	if m := regexp.MustCompile(`n\("[a-z-]+"\)`).FindString(body); m != "" {
		t.Errorf("renderStrip looks a state up by %s, which is the vocabulary held twice", m)
	}
	// The working number comes from the one helper the Work section's summary
	// also reads, so the strip and the section cannot count different polecats.
	if !strings.Contains(pageFunc(t, "polecatsWorking"), `Object.keys(POLE_WORKING).reduce(`) {
		t.Error("polecatsWorking no longer counts the states POLE_WORKING marks as work in progress")
	}
}

// The readings the tiles drew are drawn in every state the tiles drew them:
// the host's and the install's distance from main say whether the town is
// healthy, so only a reading with nothing to say at zero — a deploy backlog
// nobody is waiting in — is left out (gt-ywlei).
func TestTheStripDrawsTheTilesReadingsWhateverTheySay(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderStrip")
	// Each of these is drawn at the top level of the drawing, so no condition
	// can hold it back: the two spaces are the function body's own indent.
	for _, want := range []string{
		`  fact(box, m.cpu_idle == null ? "–" : Math.round(m.cpu_idle) + "%", "idle",`,
		`  fact(box, sm.behind == null ? "–" : sm.behind === 0 ? "current" : sm.behind + " behind", "vs main",`,
		`  fact(box, sm.median_deploy_min == null ? "–" : Math.round(sm.median_deploy_min) + "m", "to ship",`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderStrip does not draw this reading in every state: no %q", want)
		}
	}
	if !strings.Contains(body, `  if (sm.deploy_waiting > 0) fact(box, String(sm.deploy_waiting), "awaiting deploy",`) {
		t.Error("renderStrip does not draw the deploy backlog the tiles carried")
	}
	// The reading is in the title of the fact it belongs to, where the tile had
	// it in the tile's own sub-line.
	if !strings.Contains(body, `landed and awaiting a deploy`) || !strings.Contains(body, `the Machine pane has the curve`) {
		t.Error("the strip does not carry the detail the tiles showed beside these three readings")
	}
}

// The strip's alarm classes are the reader's own verdict tokens, defined for
// the strip: a class the strip names but the stylesheet never defines draws an
// alarm in the body color, which is an alarm nobody sees (gt-ywlei).
func TestTheStripColoursItsAlarmsFromDefinedClasses(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	css := page[strings.Index(page, "<style>"):strings.Index(page, "</style>")]
	for _, want := range []string{
		"#strip .v{color:var(--text)}",
		"#strip .warn{color:var(--amber)}",
		"#strip .bad{color:var(--red)}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the stylesheet has no %q, so the strip cannot color it", want)
		}
	}

	// Every class the drawing hangs on a value is one the stylesheet defines,
	// and every verdict the reader can send is drawn in one of them.
	defined := map[string]bool{"": true, "v": true, "warn": true, "bad": true}
	cls := regexp.MustCompile(`cls: ([^,}]+)`)
	quoted := regexp.MustCompile(`"([^"]*)"`)
	for _, line := range strings.Split(pageFunc(t, "renderStrip"), "\n") {
		m := cls.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
			if !defined[q[1]] {
				t.Errorf("the strip hangs the class %q on a value, which the stylesheet does not define", q[1])
			}
		}
	}
}

// A reading nobody has made is never drawn as a zero: the landings and the
// queue each say they could not be read, and a failed queue read is never a
// drained one (gt-ywlei, gt-q6h8e). What the strip draws behind "needs you" is
// the merged list's own reads, which
// attention_page_test.go runs the page over (gt-bi6w0).
func TestTheStripNeverDrawsAReadingNobodyMade(t *testing.T) {
	t.Parallel()

	strip := pageFunc(t, "renderStrip")
	for _, want := range []string{
		`const landed = ((t && t.hours) || []).reduce((n, h) => n + h.landed, 0);`,
		`if (q == null && why) fact(box, "unavailable", "waiting to land"`,
		`"the read that lists them has not come back"`,
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}
}

// The strip's seats are the open ones: a seat nothing may fill is left to the
// models line, and a seat nobody could read says unknown rather than passing as
// a seat nobody runs. A seat whose roster has not been read shows no count
// (gt-ywlei, gt-ly90n).
func TestTheStripNamesTheOpenSeatsAndTheRetiredSeatIsGone(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "seatFacts")
	for _, want := range []string{
		// No rows is a settings read that has not come back, never a town with
		// no seats: the reader always sends the seats it read.
		`if (!m || !(m.seat_rows || []).length) return null;`,
		`if (sr.unknown) {`,
		`"seats unknown"`,
		`if (sr.closed) continue;`,
		`const c = seatCount(sr, m);`,
		`sr.key || "no config key recorded"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("seatFacts has no %q", want)
		}
	}
	// A count the strip does not have is the number, or the dash for a read
	// not yet made, or the question mark for one that failed.
	count := pageFunc(t, "seatCount")
	for _, want := range []string{
		`if (sr.live != null) return {text: String(sr.live), why: ""};`,
		`if (m && m.seats_unreadable) return {text: "?", why: "the dispatcher's roster could not be read"};`,
		`return {text: "–", why: "the dispatcher's roster has not been read yet"};`,
	} {
		if !strings.Contains(count, want) {
			t.Errorf("seatCount has no %q", want)
		}
	}
	// No seat read at all is not a town with no seats, and no seat open is a
	// town running nothing: both are named rather than drawn as a count.
	strip := pageFunc(t, "renderStrip")
	for _, want := range []string{
		`if (!seats) { sp.append(el("b", "", "–"), document.createTextNode(" seats"));`,
		`else if (!seats.length) { sp.append(el("b", "", "no open seats"));`,
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}
}

// A seat count the strip does not have says which way it is missing: "–" for a
// roster read not yet made, "?" for one that failed, and the number once the
// roster has been read, so the two missing counts never draw alike (gt-ly90n).
func TestTheStripTellsAFailedSeatReadFromOneNotMade(t *testing.T) {
	t.Parallel()

	const seat = "sbx-flash"
	unread := map[string]any{"agent": seat, "cap": 3, "state": "open", "key": "gastown." + seat}
	read := map[string]any{"agent": seat, "cap": 3, "state": "open", "key": "gastown." + seat, "live": 3}
	dom := newPageDOM(t)
	for _, c := range []struct {
		name   string
		models map[string]any
		cell   string
		why    string
	}{
		{"not yet read", map[string]any{"seat_rows": []any{unread}}, "flash –/3", "has not been read yet"},
		{"read failed", map[string]any{"seat_rows": []any{unread}, "seats_unreadable": true}, "flash ?/3", "could not be read"},
		{"read", map[string]any{"seat_rows": []any{read}}, "flash 3/3", ""},
	} {
		dom.call(t, "renderStrip", map[string]any{"models": c.models})
		cell := dom.cellWithText(t, "strip", c.cell)
		if c.why == "" {
			if want := seat + " · gastown." + seat; cell.Title != want {
				t.Errorf("%s: the seat carries title %q, want %q", c.name, cell.Title, want)
			}
			continue
		}
		if !strings.Contains(cell.Title, c.why) {
			t.Errorf("%s: the seat carries title %q, want it to name why the count is missing (%q)", c.name, cell.Title, c.why)
		}
	}
}

// The header strip prints the two sandbox seats as flash and haiku and keeps
// each full name on hover; a seat in no table prints as it always has, and a
// seat with no agent prints "no agent" (gt-2mnrl, gt-1f68sk).
func TestTheStripNamesTheSandboxSeatsByTheirShortNames(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderStrip", map[string]any{
		"models": map[string]any{"seat_rows": []any{
			map[string]any{"agent": "sbx-flash", "cap": 3, "state": "open", "live": 1, "key": "gastown.sbx-flash"},
			map[string]any{"agent": "sbx-haiku", "cap": 2, "state": "open", "live": 0, "key": "gastown.sbx-haiku"},
			map[string]any{"agent": "claude-haiku", "cap": 2, "state": "open", "live": 1, "key": "gastown.claude-haiku"},
			map[string]any{"agent": "gemini-pro", "cap": 1, "state": "open", "live": 0, "key": "gastown.gemini-pro"},
			map[string]any{"agent": "", "cap": 1, "state": "open", "live": 0, "key": "gastown.unnamed"},
		}},
	})
	// Each fact after the first is drawn with the strip's " · " separator in
	// its own cell, and cellWithText reads a cell by its whole text.
	for _, c := range []struct{ cell, full string }{
		{"flash 1/3", "sbx-flash"},
		{" · haiku 0/2", "sbx-haiku"},
		{" · claude-haiku 1/2", "claude-haiku"},
		{" · gemini-pro 0/1", "gemini-pro"},
	} {
		cell := dom.cellWithText(t, "strip", c.cell)
		if !strings.Contains(cell.Title, c.full) {
			t.Errorf("the strip's %q does not carry the full name %q on hover: %q", c.cell, c.full, cell.Title)
		}
	}
	noAgent := dom.cellWithText(t, "strip", " · no agent 0/1")
	if !strings.Contains(noAgent.Title, "gastown.unnamed") {
		t.Errorf("the strip's no-agent seat carries %q on hover, want its config key", noAgent.Title)
	}
}

// The sandbox seats print as flash and haiku on every surface that names a
// seat: the header strip, the models panel's seat table and the polecat panel's
// Agent cell. Each keeps the full name on hover, and no surface prints the sbx-
// prefix as text (gt-1f68sk).
func TestTheSandboxSeatsPrintAsFlashAndHaikuOnEverySeatSurface(t *testing.T) {
	t.Parallel()

	seats := []any{
		map[string]any{"agent": "sbx-flash", "cap": 3, "state": "open", "live": 1, "key": "gastown.sbx-flash"},
		map[string]any{"agent": "sbx-haiku", "cap": 2, "state": "open", "live": 0, "key": "gastown.sbx-haiku"},
	}
	dom := newPageDOM(t)
	dom.call(t, "renderStrip", map[string]any{"models": map[string]any{"seat_rows": seats}})
	if _, err := dom.vm.RunString("modelsOpen = true"); err != nil {
		t.Fatalf("opening the models panel: %v", err)
	}
	dom.call(t, "renderModelsPanel", map[string]any{"models": map[string]any{"seat_rows": seats}})
	callWork(t, dom, workState([]any{
		seat("gastown", "alpha", "working", "gt-a1", "flash seat", "sbx-flash", time.Minute),
		seat("gastown", "bravo", "working", "gt-a2", "haiku seat", "sbx-haiku", time.Minute),
	}, workQueueReading(0, 0, 0, 0)))

	for _, c := range []struct{ id, cell, full string }{
		{"strip", "flash 1/3", "sbx-flash"},
		{"strip", " · haiku 0/2", "sbx-haiku"},
		{"modelspanel", "flash", "sbx-flash"},
		{"modelspanel", "haiku", "sbx-haiku"},
	} {
		cell := dom.cellWithText(t, c.id, c.cell)
		if !strings.Contains(cell.Title, c.full) {
			t.Errorf("#%s's %q carries %q on hover, want the full name %q", c.id, c.cell, cell.Title, c.full)
		}
	}

	// The Agent cell's tooltip opens with the agent's full name, so the cell
	// is matched to its seat by that name.
	shown := map[string]string{}
	agents := workCells(t, dom, "wagent")
	for _, a := range agents {
		shown[strings.SplitN(a.Title, " ", 2)[0]] = a.Text
	}
	if len(agents) != 2 || shown["sbx-flash"] != "flash" || shown["sbx-haiku"] != "haiku" {
		t.Errorf("the polecat panel's Agent cells read %v, want sbx-flash as flash and sbx-haiku as haiku", shown)
	}

	for _, id := range []string{"strip", "modelspanel", "polecats"} {
		for _, text := range leafTexts(t, dom, id) {
			if strings.Contains(text, "sbx-") {
				t.Errorf("#%s prints %q as text, want the short names only", id, text)
			}
		}
	}
}

// leafTexts reads the text of every leaf under the element id, in the order the
// page drew them, so a test can check every cell a surface prints.
func leafTexts(t *testing.T, dom *pageDOM, id string) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const out = [];
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (c.children && c.children.length === 0) out.push(String(c.textContent));
				walk(c);
			}
		})(__byId[` + quoteJS(id) + `]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the text under #%s: %v", id, err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the text under #%s: %v", id, err)
	}
	if len(out) == 0 {
		t.Fatalf("#%s draws no text, so nothing under it can be checked", id)
	}
	return out
}

// contains reports whether a state is one of the ones the table means.
func contains(all []string, one string) bool {
	for _, s := range all {
		if s == one {
			return true
		}
	}
	return false
}
