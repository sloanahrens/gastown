package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// The strip is the whole of the page's top: the day's landings, the seats, the
// polecats on the job, what is waiting to land, what needs the operator, and
// the machine and the install — each drawn once and each off the reader that
// owns it, so the page cannot hold two pictures of one number (gt-ywlei).
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
		// What needs the operator, off the two panes that own those rows.
		`const esc = paneCount(s.escalations), ask = paneCount(s.questions);`,
		`fact(box, String(esc + ask), "needs you",`,
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
	// The ages in the strip drift between pushes, so the 15s tick redraws it
	// with the panes whose ages it carries.
	if !strings.Contains(string(indexHTML), "renderStrip(state); renderSeats(state); renderTierSweep(state);") {
		t.Error("the 15s tick does not redraw the strip, so its ages freeze")
	}
}

// The polecats the strip counts are the reader's own states, held in the page's
// tables rather than spelled out in the drawing: a state the reader adds
// reaches POLE_ALARM, POLE_WORKING and POLE_WORD or the strip names it as one
// the page does not know. A state missing from them would vanish from the
// strip's number and from its title — the sign of a polecat nobody can see
// (gt-ywlei).
func TestTheStripAccountsForEveryStateTheReaderSends(t *testing.T) {
	t.Parallel()

	// Every state this package can put on a row (types.go).
	states := []string{
		StateWorking, StateQuiet, StateGating, StateQueued, StateSpawning, StateStalled,
		StateReviewNeeded, StateRecovery, StateNeedsHuman, StateParked, StateIdle, StateUnknown,
	}
	alarm, working, words := pageTable(t, "POLE_ALARM"), pageTable(t, "POLE_WORKING"), pageTable(t, "POLE_WORD")
	// A state whose own name is a JS identifier (needs-human) is a quoted key
	// in the page's object literal.
	for _, table := range []map[string]string{alarm, working, words} {
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
	}{{"POLE_ALARM", alarm}, {"POLE_WORKING", working}, {"POLE_WORD", words}} {
		for state := range table.pairs {
			if !contains(states, state) {
				t.Errorf("%s names %q, which this package cannot put on a polecat", table.name, state)
			}
		}
	}
	for _, state := range states {
		if _, ok := alarm[state]; !ok {
			t.Errorf("POLE_ALARM has no alarm for %q, so the strip cannot colour it", state)
		}
		if _, ok := working[state]; !ok {
			t.Errorf("POLE_WORKING does not say whether %q is work in progress", state)
		}
	}

	// Which states are work in progress — a session is on somebody's bead — and
	// which alarm each earns. Quiet is work in progress that has gone silent
	// past QuietAfter; the two the town is waiting on an operator for are the
	// only ones that read red.
	wantWorking := []string{StateWorking, StateQuiet, StateGating}
	wantAlarm := map[string]string{StateQuiet: "warnc", StateStalled: "badc", StateNeedsHuman: "badc"}
	for _, state := range states {
		want := "0"
		if contains(wantWorking, state) {
			want = "1"
		}
		if working[state] != want {
			t.Errorf("POLE_WORKING says %q is %q, want %q", state, working[state], want)
		}
		if got := alarm[state]; got != wantAlarm[state] {
			t.Errorf("POLE_ALARM colours %q %q, want %q", state, got, wantAlarm[state])
		}
	}
	// The states the title names beside the count: the ones that are not work
	// in progress and the quiet ones, but never the working number's own states,
	// which the number already counts.
	for _, state := range []string{StateQueued, StateSpawning, StateStalled, StateReviewNeeded,
		StateRecovery, StateNeedsHuman, StateParked, StateUnknown, StateQuiet} {
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
		`Object.keys(POLE_WORKING).reduce(`,
		`live.map(p => POLE_ALARM[p.state] || "")`,
		`Object.keys(POLE_WORD).filter(k => n(k)).map(k => n(k) + " " + POLE_WORD[k])`,
		// A state in no table at all is named rather than dropped.
		`const strangers = live.filter(p => !(p.state in POLE_ALARM)).length;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}
	if m := regexp.MustCompile(`n\("[a-z-]+"\)`).FindString(body); m != "" {
		t.Errorf("renderStrip looks a state up by %s, which is the vocabulary held twice", m)
	}
}

// The alarming branches take their thresholds from the page's own table, so
// what counts as an alarm is one rule a test can hold rather than a number
// written into a comparison (gt-ywlei). The waits are the tile's own: a landing
// waiting a quarter of an hour is worth a word and one waiting an hour is not
// moving.
func TestTheStripTakesItsAlarmThresholdsFromThePageTable(t *testing.T) {
	t.Parallel()

	alarms := pageTable(t, "STRIP_ALARM")
	for _, tc := range []struct{ key, want string }{
		{"wait_warn_secs", "900"}, {"wait_bad_secs", "3600"},
		{"load_warn_cores", "1"}, {"load_bad_cores", "3"}, {"behind_warn", "20"},
	} {
		if got := alarms[tc.key]; got != tc.want {
			t.Errorf("STRIP_ALARM's %s is %q, want %q", tc.key, got, tc.want)
		}
	}

	body := pageFunc(t, "renderStrip")
	for _, want := range []string{
		`oldest > STRIP_ALARM.wait_bad_secs ? "badc" : oldest > STRIP_ALARM.wait_warn_secs ? "warnc" : ""`,
		`m.load1 > cores * STRIP_ALARM.load_bad_cores ? "badc" : m.load1 > cores * STRIP_ALARM.load_warn_cores ? "warnc" : ""`,
		`sm.behind <= STRIP_ALARM.behind_warn ? "" : "warnc"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderStrip does not take its alarm from the table: no %q", want)
		}
	}
}

// The strip draws what the tiles drew, in every state the tiles drew it: the
// load, the install's distance from main and the ship time were readings the
// tile showed whether or not they were alarming, and the strip is now the
// page's only readout of them. Only a reading with nothing to say at zero —
// a deploy backlog nobody is waiting in — is left out (gt-ywlei).
func TestTheStripDrawsTheTilesReadingsWhateverTheySay(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderStrip")
	// Each of these is drawn at the top level of the drawing, so no condition
	// can hold it back: the two spaces are the function body's own indent.
	for _, want := range []string{
		`  fact(box, m.load1 == null ? "–" : m.load1.toFixed(1), "load",`,
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

// The strip's alarm classes are the page's own tokens, defined for the strip:
// a class the strip names but the stylesheet never defines draws an alarm in
// the body colour, which is an alarm nobody sees (gt-ywlei).
func TestTheStripColoursItsAlarmsFromDefinedClasses(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	css := page[strings.Index(page, "<style>"):strings.Index(page, "</style>")]
	for _, want := range []string{
		"#strip .v{color:var(--text)}",
		"#strip .warnc{color:var(--amber)}",
		"#strip .badc{color:var(--red)}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the stylesheet has no %q, so the strip cannot colour it", want)
		}
	}

	// Every class the drawing hangs on a value is one of those: a class of its
	// own is a colour the stylesheet has never heard of.
	defined := map[string]bool{"": true, "v": true, "warnc": true, "badc": true}
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

// A reading nobody has made is never drawn as a zero: the landings, the queue
// and the two panes behind "needs you" each say they could not be read, and a
// failed queue read is never a drained one. The count a failed read left behind
// is still drawn — the tile showed it — with the alarm naming the read
// (gt-ywlei, gt-q6h8e).
func TestTheStripNeverDrawsAReadingNobodyMade(t *testing.T) {
	t.Parallel()

	count := pageFunc(t, "paneCount")
	for _, want := range []string{"if (!pane) return null;", `if (pane.unavailable) return "unavailable";`, "(pane.rows || []).length + (pane.more || 0)"} {
		if !strings.Contains(count, want) {
			t.Errorf("paneCount has no %q", want)
		}
	}
	strip := pageFunc(t, "renderStrip")
	for _, want := range []string{
		`const landed = ((t && t.hours) || []).reduce((n, h) => n + h.landed, 0);`,
		`if (q == null && why) fact(box, "unavailable", "waiting to land"`,
		`if (failed.length) fact(box, "unavailable", "needs you"`,
		`failed.join(" and ") + " could not be read"`,
		// A reading that has not come back is not a failed one: it reads as no
		// value yet, in the strip's own dim.
		`else if (pending.length) fact(box, "–", "needs you", {cls: "",`,
		`"the read that lists them has not come back"`,
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("renderStrip has no %q", want)
		}
	}
}

// The strip's seats are the open ones: a seat nothing may fill is left to the
// models line, a seat nobody could read says unknown rather than passing as a
// seat nobody runs, and the pro seat is named the reserved one — which is what
// the town keeps it for (gt pool mix). A seat whose polecats have not been read
// shows no count (gt-ywlei).
func TestTheStripNamesTheOpenSeatsAndTheReservedOne(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "seatFacts")
	for _, want := range []string{
		// No rows is a settings read that has not come back, never a town with
		// no seats: the reader always sends the three.
		`if (!m || !(m.seat_rows || []).length) return null;`,
		`if (sr.unknown) {`,
		`" seat unknown"`,
		`if (sr.closed) continue;`,
		`(sr.live == null ? "–" : sr.live)`,
		`(sr.seat === "pro" ? " (reserved)" : "")`,
		`sr.key || "no config key recorded"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("seatFacts has no %q", want)
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

// contains reports whether a state is one of the ones the table means.
func contains(all []string, one string) bool {
	for _, s := range all {
		if s == one {
			return true
		}
	}
	return false
}
