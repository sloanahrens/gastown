package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The Overseer panel is one section drawing the overseer's whole side of the
// town as a stack of named blocks — the heartbeat, the digest, the review's
// schedules, the dispatcher's tick and what waits on the overseer — so the
// blocks have no ids of their own and a test finds one by the name the panel
// gives it.

// The state the page reads carries the panel: /api/state serves what the hub
// holds, and a panel nobody configured is absent rather than empty.
func TestOverseerReachesTheAPI(t *testing.T) {
	t.Parallel()

	open := 2
	age := int64(120)
	h := NewHub(Config{
		Now: func() time.Time { return questionPageNow },
		Overseer: func() *Overseer {
			return &Overseer{
				Status: OverseerAlive, State: "working", LastActiveSec: &age, SilentAfterSec: 5400,
				Journal:       []OverseerLine{{AgeSec: 30, Kind: "hold", Text: "parked gt-abc", Bead: "gt-abc"}},
				Interventions: []OverseerCount{{Kind: "hold", Count: 3}},
				OpenQuestions: &open,
			}
		},
	})
	h.pollOverseer()

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"overseer"`, `"status":"alive"`, `"parked gt-abc"`, `"open_questions":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("/api/state has no %s: %s", want, body)
		}
	}

	bare := NewHub(Config{Now: func() time.Time { return questionPageNow }})
	if bare.State().Overseer != nil {
		t.Error("a hub with no Overseer reader holds a panel")
	}
}

// The Overseer panel is gone (gt-ztrv1): no section, no fold, no draw code for
// it, and nothing in the registry, the default-open list or the redraw loops
// still names it. The state fields it read stay on the server's side.
func TestNoOverseerPanel(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	for _, gone := range []string{
		`data-fold="overseer"`,
		`id="overseernote"`,
		"function renderOverseer",
		"renderOverseer(",
		"function drawHeartbeat",
		"function drawDigest",
		"function drawDispatch",
		"function drawDeepReview",
		"function drawOverseerQueue",
		"function paintOverseerNote",
		"function oblock",
		"  overseer: {sec:",
	} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still holds %q", gone)
		}
	}
	for _, sp := range pageSections(t) {
		if sp.Key == "overseer" {
			t.Errorf("the page still folds a section under the key %q", sp.Key)
		}
	}
	if m := regexp.MustCompile(`const FOLD_DEFAULT_OPEN = \[([^\]]*)\];`).FindStringSubmatch(page); m == nil || strings.Contains(m[1], "overseer") {
		t.Errorf("FOLD_DEFAULT_OPEN still names the overseer: %v", m)
	}
	for _, key := range newPageDOM(t).foldKeys(t) {
		if key == "overseer" {
			t.Error("the folded page still has an overseer section")
		}
	}
}

// stripFacts reads the header strip's facts, in order, as text, class and hover.
func stripFacts(t *testing.T, dom *pageDOM) []struct{ Text, Class, Title string } {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const out = [];
		for (const f of __byId["strip"].children) {
			if (!f.children) continue;
			out.push({text: f.textContent, class: (f.children.__arr.filter(c => c.tagName === "B")[0] || {className: ""}).className, title: f.title || ""});
		}
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the strip: %v", err)
	}
	var raw []struct {
		Text, Class, Title string
	}
	if err := json.Unmarshal([]byte(v.String()), &raw); err != nil {
		t.Fatalf("reading the strip: %v", err)
	}
	return raw
}

// stripFact finds the one fact whose text names label, failing for none or two:
// each reading lives in one place.
func stripFact(t *testing.T, dom *pageDOM, label string) struct{ Text, Class, Title string } {
	t.Helper()

	var found []struct{ Text, Class, Title string }
	for _, f := range stripFacts(t, dom) {
		if strings.Contains(f.Text, label) {
			found = append(found, f)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the strip holds %d facts naming %q, want one: %+v", len(found), label, stripFacts(t, dom))
	}
	return found[0]
}

// The Overseer panel's two readings that go stale move to the header strip, one
// place each, with the panel note's rule: the overseer's standing is the
// reader's verdict (amber when not alive), and the dispatcher's tick is aged
// live, amber past 150 s and alert red past 300 s (gt-ztrv1).
func TestHeaderKeepsOverseerAndDispatchReadings(t *testing.T) {
	t.Parallel()

	state := func(overseer, dispatch map[string]any) map[string]any {
		s := map[string]any{}
		if overseer != nil {
			s["overseer"] = overseer
		}
		if dispatch != nil {
			s["dispatch"] = dispatch
		}
		return s
	}
	tickAgo := func(d time.Duration) map[string]any {
		return map[string]any{"at": time.Now().UTC().Add(-d).Format(time.RFC3339)}
	}
	alive := map[string]any{"status": "alive", "silent_after_sec": 5400, "last_active_sec": 120}

	cases := []struct {
		name         string
		overseer     map[string]any
		dispatch     map[string]any
		wantOverseer string
		wantOvClass  string
		wantTick     string
		wantTickCls  string
	}{
		{"alive and fresh", alive, tickAgo(time.Second), "overseer alive", "v", "dispatcher last tick ", "v"},
		{"silent overseer", map[string]any{"status": "silent", "silent_after_sec": 5400, "last_active_sec": 9000}, tickAgo(time.Second), "overseer silent window 1h30m", "warn", "dispatcher last tick ", "v"},
		{"no heartbeat", map[string]any{"status": "none", "silent_after_sec": 5400}, nil, "overseer no heartbeat", "warn", "dispatcher last tick none logged", ""},
		{"unreadable", map[string]any{"unavailable": true}, nil, "overseer unavailable", "warn", "dispatcher last tick none logged", ""},
		{"tick amber", alive, tickAgo(200 * time.Second), "overseer alive", "v", "dispatcher last tick 3m", "warn"},
		{"tick stopped", alive, tickAgo(20 * time.Minute), "overseer alive", "v", "dispatcher last tick 20m", "bad"},
		{"not read yet", nil, nil, "overseer –", "", "dispatcher last tick none logged", ""},
	}
	for _, c := range cases {
		dom := newPageDOM(t)
		dom.call(t, "renderStrip", state(c.overseer, c.dispatch))
		ov := stripFact(t, dom, "overseer")
		tick := stripFact(t, dom, "dispatcher last tick")
		if !strings.HasPrefix(ov.Text, c.wantOverseer) || ov.Class != c.wantOvClass {
			t.Errorf("%s: the overseer fact reads %q wearing %q, want %q wearing %q", c.name, ov.Text, ov.Class, c.wantOverseer, c.wantOvClass)
		}
		if !strings.HasPrefix(tick.Text, c.wantTick) || tick.Class != c.wantTickCls || (c.dispatch != nil && !strings.HasSuffix(tick.Text, " ago")) {
			t.Errorf("%s: the dispatcher fact reads %q wearing %q, want %q wearing %q", c.name, tick.Text, tick.Class, c.wantTick, c.wantTickCls)
		}
	}

	// A stopped dispatcher's hover says so in the words the panel's block used.
	dom := newPageDOM(t)
	dom.call(t, "renderStrip", state(alive, tickAgo(20*time.Minute)))
	if tip := stripFact(t, dom, "dispatcher last tick").Title; !strings.Contains(tip, "has not ticked for") || !strings.Contains(tip, "ticks every minute") {
		t.Errorf("a stopped dispatcher's hover reads %q, want the warning the dispatcher block carried", tip)
	}
}
