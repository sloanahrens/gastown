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
		`note.textContent = cloudNote(c, checks);`,
		`c.state === "overdue" || c.state === "error"`,
		`c.state === "missing" || c.state === "error"`,
		`if (!c.total) { box.append(el("div", "empty", "no findings")); return; }`,
		`box.append(el("div", "empty", "and " + c.more + " more"));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderCloud has no %q", want)
		}
	}

	// The header is the run and its counts on one line (gt-1hob8): the projects
	// the run watched ride the line's title rather than its width, and the
	// counts sit beside it as chips with the zero ones left out and the same
	// severity names the findings use.
	for _, want := range []string{
		`head.title = projects.length ? projects.join(", ") : "no projects in the report";`,
		`"last run " + age(c.last_run) + " ago · " + projects.length + (projects.length === 1 ? " project" : " projects")`,
		`if (n) chips.append(el("span", "tag " + (CLOUD_SEV[k] || "sev-low"), n + " " + k));`,
		`if (counts.other) chips.append(el("span", "tag sev-low", counts.other + " other"));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderCloud has no %q", want)
		}
	}

	// The severity chip is the dashboard's own severity colouring, with the
	// worst one in the red the alerts pane paints an alert with, and the
	// findings are listed in the reader's order.
	for _, want := range []string{
		`const CLOUD_SEV = {urgent: "sev-critical", normal: "sev-medium", low: "sev-low"};`,
		`el("span", "tag esc " + (CLOUD_SEV[f.severity] || "sev-low"), f.severity || "–")`,
		`const findings = c.findings || [];`,
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
	if !strings.Contains(string(indexHTML), `<section><h2>Cloud <span class="r" id="cloudnote"></span></h2><div class="body" id="cloud"></div><h3 class="sub">Deploys <span class="r" id="deploysnote"></span></h3><div class="body" id="deploys"></div></section>`) {
		t.Error("the Cloud section is not the markup its neighbours are")
	}
}

// A finding is one line until the operator opens it (gt-1hob8): the row is a
// button carrying a severity chip, the kind, the resource and the Detail cut to
// the line, and the whole Detail twice: on the row's title, and in the block
// the row opens under itself. Opening is page state rather than the drawn list,
// so the refresh that rebuilds the list leaves the open rows open.
func TestCloudFindingsAreOneLineUntilOpened(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderCloud")
	if !strings.Contains(string(indexHTML), `const cloudOpen = new Set();`) {
		t.Error("index.html keeps no set of opened findings, so a refresh would close them")
	}
	for _, want := range []string{
		`const open = cloudOpen.has(id);`,
		`const row = el("button", "crow");`,
		`row.setAttribute("aria-expanded", open ? "true" : "false");`,
		`row.title = f.detail || "";`,
		`row.onclick = () => { open ? cloudOpen.delete(id) : cloudOpen.add(id); renderCloud(state); };`,
		`if (open) box.append(el("div", "cfull", f.detail || ""));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderCloud has no %q", want)
		}
	}
	// The row is one line because the Detail cell clips it; the whole text is
	// in the block the row opens.
	for _, want := range []string{
		`.crow{display:flex;gap:8px;align-items:baseline;`,
		`.cdetail{flex:1 1 24ch;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}`,
		`.cfull{padding:4px 0 8px 10px;margin-bottom:4px;border-left:2px solid var(--line);overflow-wrap:anywhere}`,
	} {
		if !strings.Contains(string(indexHTML), want) {
			t.Errorf("index.html has no %q", want)
		}
	}

	// The project is a cell of its own only while the findings name more than
	// one; a single project is already named on the header line, and more than
	// one rides its resource as a prefix that never wraps.
	for _, want := range []string{
		`const multi = new Set(findings.map(f => f.project || "")).size > 1;`,
		`if (multi && f.project) res.append(el("span", "cproj title", f.project));`,
		`res.append(el("span", "cname", f.resource || "–"));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderCloud has no %q", want)
		}
	}
	if !strings.Contains(string(indexHTML), `.cproj{flex:0 0 auto;white-space:nowrap}`) {
		t.Error("index.html has no .cproj, so a project name could wrap")
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
	// Each field of a finding row is an element with text, never a string
	// concatenated into markup.
	for _, want := range []string{
		`row.append(sev, el("span", "ckind title", f.kind || "–"));`,
		`res.append(el("span", "cname", f.resource || "–"));`,
		`row.append(res, el("span", "cdetail", f.detail || ""));`,
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
