package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Overseer pane is drawn from the state the reader decided: an unreadable
// read is named rather than drawn as a silent overseer, and the status word is
// the reader's own.
func TestOverseerPanelDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderOverseer")
	for _, want := range []string{
		`const o = s.overseer;`,
		`box.append(el("div", "badc", "unavailable: the overseer's heartbeat or journal could not be read"))`,
		`const alive = o.status === "alive";`,
		`el("b", alive ? "good" : "warnc", o.status === "none" ? "no heartbeat" : o.status)`,
		`"last active " + fmtDur(o.last_active_sec) + " ago"`,
		`"has never beaten"`,
		`"last 7 days: " + counts.map(c => c.kind + " " + c.count).join(" · ")`,
		`o.open_questions != null`,
		`const lines = o.journal || [];`,
		`"silent window " + fmtDur(o.silent_after_sec)`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderOverseer has no %q", want)
		}
	}
	// The journal text is the overseer's own, so every cell goes through el(),
	// which writes textContent: a tag in it shows as that text, never markup.
	if strings.Contains(draw, "innerHTML") {
		t.Error("renderOverseer sets markup from the journal's text")
	}
	if !strings.Contains(pageFunc(t, "renderAll"), "renderOverseer(state)") {
		t.Error("renderAll does not draw the Overseer panel")
	}
	if !strings.Contains(string(indexHTML), `<section><h2>Overseer <span class="r" id="overseernote"></span></h2><div class="body" id="overseer"></div></section>`) {
		t.Error("the Overseer section is not the markup its neighbours are")
	}
}

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
