package dashboard

import (
	"strings"
	"testing"
)

// The Cloud panel is read where the operator already looks for the machine it
// runs on: under Machine, and above Forgejo, which is what the layout test
// pins. What it draws is the state the reader decided — the note names it, and
// the body is the run, the projects and the findings.
func TestCloudPanelDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderCloud")
	for _, want := range []string{
		`note.textContent = c.note || c.state || "";`,
		`c.state === "overdue" || c.state === "error"`,
		`c.state === "missing" || c.state === "error"`,
		`el("div", "k title", "last run " + age(c.last_run) + " ago, " + projects.length + " projects: " + (projects.length ? projects.join(", ") : "none"))`,
		`if (!c.total) { box.append(el("div", "empty", "no findings")); return; }`,
		`box.append(el("div", "empty", "and " + c.more + " more"));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderCloud has no %q", want)
		}
	}

	// The severity column is the dashboard's own severity colouring, with the
	// worst one in the red the alerts pane paints an alert with, and the
	// findings are listed in the reader's order.
	for _, want := range []string{
		`const CLOUD_SEV = {urgent: "sev-critical", normal: "sev-medium", low: "sev-low"};`,
		`el("td", "esc " + (CLOUD_SEV[f.severity] || "sev-low"), f.severity || "–")`,
		`for (const f of c.findings || [])`,
	} {
		if !strings.Contains(string(indexHTML), want) {
			t.Errorf("index.html has no %q", want)
		}
	}

	// The panel is drawn with every other pane, and only exists while the
	// reader has reported: the state's field is absent until then.
	all := pageFunc(t, "renderAll")
	if !strings.Contains(all, "renderCloud(state)") {
		t.Error("renderAll does not draw the Cloud panel")
	}
	if !strings.Contains(string(indexHTML), `<section><h2>Cloud <span class="r" id="cloudnote"></span></h2><div class="body" id="cloud"></div></section>`) {
		t.Error("the Cloud section is not the markup its neighbours are")
	}
}

// The report is written by another account, so a Detail that holds an HTML tag
// must reach the page as that text. The panel escapes it by building every cell
// with the page's el() helper, which sets textContent; the page parses no
// markup anywhere, so no string from the report can become one.
func TestCloudPanelWritesTheReportsTextAndNeverMarkup(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderCloud")
	if strings.Contains(draw, "innerHTML") {
		t.Error("renderCloud sets markup from the report's text")
	}
	// Each cell of a finding row is an element with text, never a string
	// concatenated into markup.
	for _, want := range []string{
		`tr.append(sev, el("td", "title", f.kind || "–"), el("td", "title", f.project || "–"), el("td", "title", f.resource || "–"), el("td", "", f.detail || ""));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderCloud no longer writes a finding's fields with el(): %q", want)
		}
	}

	page := string(indexHTML)
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(page, sink) {
			t.Errorf("index.html has a %s path, so report text could be parsed as markup", sink)
		}
	}
}
