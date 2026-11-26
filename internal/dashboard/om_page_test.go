package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// A window's reused count rides beside its neighbouring counts under `reused`
// (gt-lz4gu3): the landings that reused a stored review instead of running om
// are counted apart from the ones it ran.
func TestReviewPoolOMWindowReusedJSON(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(OMWindow{Reused: 2})
	if err != nil {
		t.Fatalf("marshal OMWindow: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal OMWindow: %v", err)
	}
	if got["reused"] != float64(2) {
		t.Errorf("OMWindow{Reused: 2} marshals reused as %v, want 2 (json: %s)", got["reused"], b)
	}
}

// omWindowsTable reads every row of the om panel's windows table as its cells'
// text — the table whose header carries a Landed column — so a test reads the
// rendered table rather than the source that draws it (gt-q83zwr).
func omWindowsTable(t *testing.T, dom *pageDOM) [][]string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((function () {
		var out = null;
		(function walk(n) {
			if (out) return;
			if (n.tagName === "TABLE") {
				var head = n.children[0], heads = [];
				if (head && head.tagName === "TR") for (var i = 0; i < head.children.length; i++) heads.push(String(head.children[i].textContent));
				if (heads.indexOf("Landed") >= 0) {
					out = [];
					for (var r = 0; r < n.children.length; r++) {
						var cells = [];
						for (var c = 0; c < n.children[r].children.length; c++) cells.push(String(n.children[r].children[c].textContent));
						out.push(cells);
					}
					return;
				}
			}
			for (var i = 0; i < n.children.length; i++) walk(n.children[i]);
		})(__byId["om"]);
		return out || [];
	})())`)
	if err != nil {
		t.Fatalf("reading the om windows table: %v", err)
	}
	var rows [][]string
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the om windows table: %v", err)
	}
	return rows
}

// A window that reused a stored review draws its count beside the other counts
// (gt-q83zwr): the column appears once any window has one, so each window reads
// its own number and a window with none reads 0. A table with no reused landing
// is the table it drew before the count existed.
func TestReviewPoolOMPanelRendersReused(t *testing.T) {
	t.Parallel()

	omState := func(windows ...map[string]any) map[string]any {
		ws := make([]any, len(windows))
		for i, w := range windows {
			ws[i] = w
		}
		return map[string]any{"om": map[string]any{
			"model": "haiku", "depth": "standard", "timeout_s": 900,
			"windows": ws, "days": []any{}, "scores": make([]any, 10),
			"rejects": map[string]any{}, "routes": map[string]any{},
		}}
	}

	dom := newPageDOM(t)
	dom.call(t, "renderOM", omState(
		map[string]any{"label": "1h", "landed": 2, "approved": 2, "skipped": 0, "errors": 0, "rejected": 0, "reused": 0},
		map[string]any{"label": "24h", "landed": 11, "approved": 6, "skipped": 1, "errors": 0, "rejected": 3, "reused": 4},
	))
	rows := omWindowsTable(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the windows table drew %d rows, want a header and one row per window: %v", len(rows), rows)
	}
	if got, want := strings.Join(rows[0], ","), ",Landed,Approved,No review,Errors,Rejected,Reused,Avg score,om median,om p95"; got != want {
		t.Errorf("the windows table's header reads %q, want %q", got, want)
	}
	for i, want := range []string{"1h,2,2,0,0,0,0,–,–,–", "24h,11,6,1,0,3,4,–,–,–"} {
		if got := strings.Join(rows[i+1], ","); got != want {
			t.Errorf("window row %d reads %q, want %q", i, got, want)
		}
	}

	// A zero count and a server too old to send the field both leave the table
	// as it was.
	plain := newPageDOM(t)
	plain.call(t, "renderOM", omState(
		map[string]any{"label": "1h", "landed": 2, "approved": 2, "skipped": 0, "errors": 0, "rejected": 0, "reused": 0},
		map[string]any{"label": "24h", "landed": 11, "approved": 6, "skipped": 1, "errors": 0, "rejected": 3},
	))
	rows = omWindowsTable(t, plain)
	if len(rows) != 3 {
		t.Fatalf("the windows table drew %d rows, want a header and one row per window: %v", len(rows), rows)
	}
	if got, want := strings.Join(rows[0], ","), ",Landed,Approved,No review,Errors,Rejected,Avg score,om median,om p95"; got != want {
		t.Errorf("a table with no reused landing changed its header: %q, want %q", got, want)
	}
	for i, want := range []string{"1h,2,2,0,0,0,–,–,–", "24h,11,6,1,0,3,–,–,–"} {
		if got := strings.Join(rows[i+1], ","); got != want {
			t.Errorf("window row %d with no reused landing reads %q, want %q", i, got, want)
		}
	}
}

// The om panel opens with what is running now and the last five om ran, then
// folds its Stats, Rework and Scores sections under details/summary lines built
// like the polecats panel's idle line (gt-hmthk).
func TestOMPanelOpensWithRunningAndLastFiveAboveItsFolds(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderOM")

	running := strings.Index(body, `"running now"`)
	last := strings.Index(body, `om.last`)
	stats := strings.Index(body, `fold("Stats`)
	// The last call is the one on the om-present path: renderOM also draws the
	// fold before returning from an absent om read (gt-ddxrq).
	rework := strings.LastIndex(body, "omRework(box, s.rework)")
	scores := strings.Index(body, `fold("Scores`)
	if running < 0 || last < 0 || stats < 0 || rework < 0 || scores < 0 {
		t.Fatalf("renderOM is missing a block (running=%d last=%d stats=%d rework=%d scores=%d)", running, last, stats, rework, scores)
	}
	if !(running < last && last < stats && stats < rework && rework < scores) {
		t.Errorf("renderOM draws its blocks out of order: want running, last five, Stats, Rework, Scores (running=%d last=%d stats=%d rework=%d scores=%d)", running, last, stats, rework, scores)
	}

	// Each fold is a details element whose open state lives in a module-level
	// variable and is written back on toggle, so the redraw keeps it.
	for _, want := range []string{
		`document.createElement("details")`,
		`d.open = isOpen`,
		`d.ontoggle = () => { setOpen(d.open); saveOMOpen(); }`,
		`el("summary", "title", title)`,
		`openOMStats, v => { openOMStats = v; }`,
		`openOMScores, v => { openOMScores = v; }`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderOM does not wire its folds the way the idle line does: no %q", want)
		}
	}

	// A one-line digest rides each summary so a closed section still says something.
	for _, want := range []string{`"Stats" + (day ?`, `landed, `, `" rejected"`, `"Scores" + (scored ?`, `" · avg "`} {
		if !strings.Contains(body, want) {
			t.Errorf("renderOM's summaries carry no digest: no %q", want)
		}
	}

	// Both sections start closed.
	page := string(indexHTML)
	if !strings.Contains(page, "let openOMStats = false, openOMScores = false;") {
		t.Error("the om folds do not start closed")
	}
}

// The folds are remembered per viewer under one key, by section name, and
// storage that throws leaves them closed instead of taking the page down.
func TestOMFoldsAreRememberedByNameAndSurviveBadStorage(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	load := strings.Index(page, `localStorage.getItem("gt-om-open")`)
	if load < 0 {
		t.Fatal("the om folds are not read back from localStorage (gt-om-open)")
	}
	if try := strings.LastIndex(page[:load], "try {"); try < 0 || load-try > 80 {
		t.Error("the read of gt-om-open is not inside a try")
	}
	for _, want := range []string{`saved.stats`, `saved.scores`, `{stats: openOMStats, scores: openOMScores}`} {
		if !strings.Contains(page, want) {
			t.Errorf("the saved om folds are not keyed by section name: no %q", want)
		}
	}
	save := pageFunc(t, "saveOMOpen")
	if !strings.Contains(save, `try { localStorage.setItem("gt-om-open"`) || !strings.Contains(save, "catch (e) {}") {
		t.Error("saving the om folds can throw out of the toggle handler")
	}

	// The other fold memory is not touched.
	if strings.Contains(save, "gt-collapsed") {
		t.Error("the om folds write the whole-panel fold's key")
	}
}

// A running review's elapsed counts up between pushes, and a row the page
// cannot read says so instead of throwing and blanking the panel.
func TestOMRunningTicksAndUnreadableRowsAreMarked(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderOM")
	for _, want := range []string{"took.dataset.since = x.started", `"unknown bead"`, `"unparsed"`, "Array.isArray(om.running)", "Array.isArray(om.last)"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderOM does not handle a running or last-five row the way the spec asks: no %q", want)
		}
	}
	if !strings.Contains(string(indexHTML), `document.querySelectorAll("#om [data-since]")`) {
		t.Error("no ticker moves the running review's elapsed between pushes")
	}

	// Only text goes in; never markup.
	if strings.Contains(string(indexHTML), "innerHTML") {
		t.Error("the page sets markup from a reading's text")
	}
}
