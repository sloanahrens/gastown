package dashboard

import (
	"strings"
	"testing"
)

// The om panel opens with what is running now and the last five om ran, then
// folds its Stats and Scores sections under details/summary lines built like the
// polecats panel's idle line (gt-hmthk).
func TestOMPanelOpensWithRunningAndLastFiveAboveTwoFolds(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderOM")

	running := strings.Index(body, `"running now"`)
	last := strings.Index(body, `om.last`)
	stats := strings.Index(body, `fold("Stats`)
	scores := strings.Index(body, `fold("Scores`)
	if running < 0 || last < 0 || stats < 0 || scores < 0 {
		t.Fatalf("renderOM is missing a block (running=%d last=%d stats=%d scores=%d)", running, last, stats, scores)
	}
	if !(running < last && last < stats && stats < scores) {
		t.Errorf("renderOM draws its blocks out of order: want running, last five, Stats, Scores (running=%d last=%d stats=%d scores=%d)", running, last, stats, scores)
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
