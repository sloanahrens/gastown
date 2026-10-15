package dashboard

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// The Deploys block is read where the operator reads a release: inside the
// Cloud section, directly under the patrol's findings, so the panel that knows
// what the machine is doing sits over the panel that knows where the deploy is.
func TestDeploysBlockSitsInsideTheCloudSectionUnderTheFindings(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	cloud := strings.Index(page, `id="cloud"`)
	head := strings.Index(page, `id="deploysnote"`)
	body := strings.Index(page, `id="deploys"`)
	// The Forgejo section is the next one over; nothing of the block may spill
	// past it.
	after := strings.Index(page, `<section><h2>Forgejo`)
	if cloud < 0 || head < 0 || body < 0 || after < 0 {
		t.Fatalf("the Cloud section is missing one of #cloud, #deploysnote, #deploys or Forgejo (cloud=%d head=%d body=%d forgejo=%d)",
			cloud, head, body, after)
	}
	if !(cloud < head && head < body) {
		t.Errorf("the block is not under the findings in the order #cloud, #deploysnote, #deploys (%d, %d, %d)", cloud, head, body)
	}
	if body > after {
		t.Error("the block is not inside the Cloud section")
	}

	// The section is one element of the same shape as its neighbours, with the
	// block's own heading and body inside it.
	if !strings.Contains(page, `<section><h2>Cloud <span class="r" id="cloudnote"></span></h2><div class="body" id="cloud"></div><h3 class="sub">Deploys <span class="r" id="deploysnote"></span></h3><div class="body" id="deploys"></div></section>`) {
		t.Error("the Cloud section is not the markup its neighbours are")
	}
}

// The block draws what the reader decided: the runs it kept, each as a row of
// repo, ref, commit, state and age with the stages after them, the reader's own
// warning on the runs it inferred one for, and its own words for the states
// where there is nothing to draw.
func TestDeploysBlockDrawsWhatTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderDeploys")
	for _, want := range []string{
		`if (!d) { note.className = "r"; note.textContent = ""; box.append(el("div", "empty", "no viewer token")); return; }`,
		// The note names every read that came back empty-handed, the scheduled
		// checks included: a repo whose checks could not be listed is one the
		// block could not ask, not one that answered with no checks (gt-t9w60).
		`note.textContent = err ? "could not read the runs: " + err : [`,
		`unreadable ? unreadable + (unreadable === 1 ? " repo unreadable" : " repos unreadable") : "",`,
		`unchecked ? unchecked + (unchecked === 1 ? " repo's checks unreadable" : " repos' checks unreadable") : "",`,
		`].filter(Boolean).join(" · ");`,
		`box.append(el("div", err ? "badc" : "empty", err ? "no deploy runs: " + err : "no deploy runs yet"));`,
		`for (const r of runs) box.append(deployRow(r));`,
		`if (!runs.length && !health.length)`,
		// The runs' header is drawn only where there are runs: a block holding
		// scheduled rows alone has no run column to name (gt-t9w60).
		`if (runs.length) {`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderDeploys has no %q", want)
		}
	}

	// The repo column is measured from the runs rather than fixed (gt-1hob8),
	// so the longest name the block lists is the width of the column, with a
	// character of slack for the rounding of a name that fits, and the header
	// cells carry the same classes as the row cells they name. The ref column
	// keeps its width and takes the staging tag's on top of it, so a list
	// holding one still lines its refs up with the header (gt-2h2lx, gt-egffr).
	for _, want := range []string{
		`for (const r of runs) {`,
		`widest = Math.max(widest, repoShort(r.repo).length);`,
		`tags = tags || r.workflow === DEPLOY_STAGING;`,
		`box.style.setProperty("--drepo", (widest + 1) + "ch");`,
		`box.style.setProperty("--dref", (DEPLOY_REF_CH + (tags ? DEPLOY_STAGING_TAG_CH : 0)) + "ch");`,
		`[["Repo", "drepo"], ["Ref", "dref"], ["Commit", "dhash"], ["State", "dstate"], ["Age", "dage"]]`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderDeploys has no %q", want)
		}
	}

	// The header takes the rows' font. The widths are in ch, and a ch is the
	// drawing cell's own font's, so a header rule that set a font size would
	// give its columns another width and land every header left of its column
	// (gt-egffr).
	head := cssRule(t, ".dhead")
	if strings.Contains(head, "font-size") {
		t.Errorf(".dhead rule is %q: a font size of its own makes a ch-based column another width under the header than under the rows", head)
	}
	if !strings.Contains(head, "color:var(--dim)") {
		t.Errorf(".dhead rule is %q: the header is no longer dim, so it reads as a row", head)
	}

	// The row is the run: every cell of it, the stages after them and the
	// reader's warning under that. The State cell is the page's own reading of
	// the run's status (gt-egffr).
	row := pageFunc(t, "deployRow")
	for _, want := range []string{
		`if (repo.textContent !== r.repo) repo.title = r.repo;`,
		`const state = el("span", "dstate", deployState(r));`,
		`if (state.textContent !== (r.status || "–")) state.title = r.status;`,
		`row.append(repo, ref, el("span", "dhash", r.hash || "–"), state, el("span", "dage", r.at ? age(r.at) : "–"), deployStages(r));`,
		`if (r.warn) row.append(el("span", "dwarn warnc", r.warn));`,
		`const row = r.url ? el("a", "drow") : el("div", "drow");`,
		`if (r.url) { row.href = r.url; row.target = "_blank"; row.rel = "noopener"; row.title = r.url; }`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("deployRow has no %q", want)
		}
	}

	// The stage cell is the page's own chip vocabulary — the name, the glyph of
	// the state, and the state's word on the chip's title — and the two ways a
	// run's stages can be absent are drawn differently: a jobs call that failed
	// says so, where a run past the job-fetch cap draws nothing at all
	// (gt-egffr).
	stages := pageFunc(t, "deployStages")
	for _, want := range []string{
		`if (r.stages_unread) { cell.className += " title"; cell.textContent = "stages not read"; }`,
		`else if (!r.stages_skipped) cell.textContent = "–";`,
		`const chip = el("span", "tag " + (DEPLOY_STAGE[st.status] || "sev-low"), (st.name || "–") + " " + (DEPLOY_GLYPH[st.status] || "–"));`,
		`chip.title = (st.name || "–") + " " + (st.status || "?");`,
	} {
		if !strings.Contains(stages, want) {
			t.Errorf("deployStages has no %q", want)
		}
	}
	if !strings.Contains(string(indexHTML), `const DEPLOY_GLYPH = {success: "✓", failure: "✗", running: "◌"};`) {
		t.Error("index.html has no stage glyphs, so a chip names no state")
	}

	// The repo and ref columns take their measured widths — the ref column one
	// that holds a staging run's tag beside its ref (gt-2h2lx) — and the stage
	// chips sit on the row's line while they fit, wrapping under it on a panel
	// too narrow for both (gt-1hob8).
	for _, want := range []string{
		`.drepo{flex:0 0 var(--drepo,15ch)}`,
		`.dref{flex:0 0 var(--dref,13ch);display:flex;gap:4px;align-items:baseline}`,
		`.dref .tag{flex:0 0 auto;margin-right:0}`,
		`.dstage{flex:0 1 auto;min-width:0;display:flex;gap:4px;flex-wrap:wrap}`,
	} {
		if !strings.Contains(string(indexHTML), want) {
			t.Errorf("index.html has no %q", want)
		}
	}

	// The block is drawn with every other pane, and only exists while the
	// reader has reported: the state's field is absent until then.
	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderDeploys(state)") {
		t.Error("renderAll does not draw the Deploys block")
	}
}

// The State cell is the page's own reading of a run's status. Forgejo calls a
// run "blocked" between one job finishing and the next being picked up, so a
// run with a stage already succeeded or running is running, not blocked; a run
// no stage has started in is still waiting, and says what the API said. The
// API's own word stays on the cell's title (gt-egffr).
func TestDeploysBlockReadsABlockedRunWithAStageDoneAsRunning(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	for _, want := range []string{
		`const DEPLOY_MOVING = {blocked: true, waiting: true};`,
		`const DEPLOY_STARTED = {success: true, running: true};`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no %q, so the state cell reads no status", want)
		}
	}

	state := pageFunc(t, "deployState")
	for _, want := range []string{
		`const raw = r.status || "–";`,
		`if (!DEPLOY_MOVING[raw]) return raw;`,
		`return (r.stages || []).some(st => DEPLOY_STARTED[st.status]) ? "running" : raw;`,
	} {
		if !strings.Contains(state, want) {
			t.Errorf("deployState has no %q", want)
		}
	}
}

// cssRule returns one rule's declarations from the page's style block, so a
// test can hold a rule to its shape rather than to a pixel (gt-fn9e6.48).
func cssRule(t *testing.T, sel string) string {
	t.Helper()

	page := string(indexHTML)
	at := strings.Index(page, sel+"{")
	if at < 0 {
		t.Fatalf("index.html has no %s rule", sel)
	}
	body := page[at+len(sel)+1:]
	if end := strings.Index(body, "}"); end >= 0 {
		body = body[:end]
	}
	return body
}

// A staging run is told from a release at a glance: the reader names the
// workflow each row came from, and the row whose workflow is staging.yml —
// whose ref is the branch it deployed, where a release's is a tag — carries a
// dim tag beside that ref. A release row carries none (gt-2h2lx).
func TestDeploysBlockTagsAStagingRunAndNotARelease(t *testing.T) {
	t.Parallel()

	row := pageFunc(t, "deployRow")
	for _, want := range []string{
		`const ref = el("span", "dref", r.ref || "–");`,
		`if (r.workflow === DEPLOY_STAGING) {`,
		`const g = el("span", "tag", "staging");`,
		`ref.append(g);`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("deployRow has no %q", want)
		}
	}
	if !strings.Contains(string(indexHTML), `const DEPLOY_STAGING = "staging.yml";`) {
		t.Error("index.html does not name the staging workflow, so the tag is read off no value")
	}

	// The tag is dim by inheritance: it rides the ref cell, which the row's
	// rule colours the way it colours a ref, and carries no colour of its own.
	if !strings.Contains(string(indexHTML), `.drow .dref,.drow .dhash,.drow .dage{color:var(--dim)}`) {
		t.Error("index.html does not colour the ref cell dim, so the tag beside a ref is not")
	}
	if !strings.Contains(string(indexHTML), `.dref .tag{flex:0 0 auto;margin-right:0}`) {
		t.Error("index.html does not size the ref cell's tag, so a long ref could shrink it")
	}
}

// A stage name and a ref are a repository's own text, so they reach the page as
// text: every cell is built with the page's el() helper, which sets
// textContent, and the page parses no markup anywhere.
func TestDeploysBlockWritesTheReposTextAndNeverMarkup(t *testing.T) {
	t.Parallel()

	for _, fn := range []string{"renderDeploys", "deployRow", "deployStages", "healthHead", "healthRow"} {
		if strings.Contains(pageFunc(t, fn), "innerHTML") {
			t.Errorf("%s sets markup from a repository's text", fn)
		}
	}
	page := string(indexHTML)
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(page, sink) {
			t.Errorf("index.html has a %s path, so a repository's text could be parsed as markup", sink)
		}
	}
}

// The scheduled checks are the block's other kind of row: one row per app and
// environment, under the runs, with a header of its own because their columns
// are not a run's — no stages, commit or ref to line up with (gt-t9w60).
func TestDeploysBlockDrawsTheScheduledHealthRows(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderDeploys")
	for _, want := range []string{
		`const runs = d.runs || [], health = d.health || [];`,
		`for (const h of health) happ = Math.max(happ, (h.app || "–").length);`,
		`if (happ) box.style.setProperty("--happ", (happ + 1) + "ch");`,
		`if (health.length) box.append(healthHead());`,
		`for (const h of health) box.append(healthRow(h));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderDeploys has no %q", want)
		}
	}

	// The header names the columns and takes the rows' font, for the reason the
	// runs' header does: the widths are in ch, and a ch is the drawing font's
	// own (gt-egffr).
	head := pageFunc(t, "healthHead")
	if !strings.Contains(head, `[["App", "happ"], ["Environment", "henv"], ["Result", "hstate"], ["Age", "hage"], ["Last green", "hgreen"], ["Passed", "hpass"]]`) {
		t.Error("the scheduled rows' header does not name their columns")
	}
	rule := cssRule(t, ".hhead")
	if strings.Contains(rule, "font-size") {
		t.Errorf(".hhead rule is %q: a font size of its own makes a ch-based column another width under the header than under the rows", rule)
	}
	if !strings.Contains(rule, "color:var(--dim)") {
		t.Errorf(".hhead rule is %q: the header is no longer dim, so it reads as a row", rule)
	}

	// The row is the app and the environment it is about, the newest result and
	// how long ago it ran, the last green and the pass count. It links the
	// newest run the way a deploy row links its run, and shows the app's whole
	// repo name on the shortened cell it draws (gt-t9w60).
	row := pageFunc(t, "healthRow")
	for _, want := range []string{
		`const row = h.url ? el("a", "hrow") : el("div", "hrow");`,
		`if (h.url) { row.href = h.url; row.target = "_blank"; row.rel = "noopener"; row.title = h.url; }`,
		`if (app.textContent !== h.repo) app.title = h.repo;`,
		`el("span", "hstate " + (HEALTH_RESULT_CLASS[h.status] || "title"), h.unread ? "not read" : healthResult(h))`,
		`el("span", "hage", h.at ? age(h.at) : "–")`,
		`el("span", "hgreen", h.last_green ? age(h.last_green) + " ago" : "–")`,
		`el("span", "hpass", h.runs ? h.passed + "/" + h.runs : "–")`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("healthRow has no %q", want)
		}
	}

	// The app column keeps the width it is measured to, the environment names
	// the width of the longest one health.yml carries, and the counts sit on
	// the row's line beside them.
	for _, want := range []string{
		`.happ{flex:0 0 var(--happ,16ch)}`,
		`.henv{flex:0 0 20ch;color:var(--dim)}`,
		`.hstate{flex:0 0 16ch}`,
		`.hage,.hgreen{flex:0 0 8ch;color:var(--dim)}`,
		`.hpass{flex:0 0 8ch;color:var(--dim)}`,
	} {
		if !strings.Contains(string(indexHTML), want) {
			t.Errorf("index.html has no %q", want)
		}
	}
}

// A check the app was off for is not a pass, and the page is where that is
// said: a skipped run reads "off on purpose" and carries no colour, and only a
// pass and a failure have one, so no cell drawn from a skipped run can read as
// green (gt-t9w60).
func TestDeploysBlockNeverDrawsASkippedCheckAsGreen(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if !strings.Contains(page, `const HEALTH_RESULT = {success: "green", failure: "failed", skipped: "off on purpose"};`) {
		t.Error("the page does not word a skipped check as off on purpose")
	}
	if !strings.Contains(page, `const HEALTH_RESULT_CLASS = {success: "good", failure: "badc"};`) {
		t.Error("the coloured results are not the pass and the failure alone, so another check could be drawn as one")
	}
	if strings.Contains(pageFunc(t, "healthRow"), `"good"`) {
		t.Error("healthRow colours a result of its own, so a check nobody ran could read as green")
	}
	// The page's own word for a skipped run is on the cell, and the API's word
	// stays on its title: an operator reading "off on purpose" can see the run
	// itself was skipped.
	if !strings.Contains(pageFunc(t, "healthRow"), `if (h.status && result.textContent !== h.status) result.title = h.status;`) {
		t.Error("healthRow does not keep the API's own word for the result it rewords")
	}
}

// A failed scheduled check is marked in the header, where a glance at the page
// finds it beside the town's own verdict: one red pill naming the app, hidden
// until a reader names one, so an empty marker is never a strip that claims
// nothing is wrong (gt-t9w60).
func TestDeploysBlockMarksAFailedCheckInTheHeaderStrip(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	header := page[strings.Index(page, "<header>"):strings.Index(page, "</header>")]
	if !strings.Contains(header, `<span id="healthmark" class="pill red" hidden></span>`) {
		t.Fatal("the header has no red health marker (#healthmark)")
	}
	if strings.Index(header, `id="healthline"`) > strings.Index(header, `id="healthmark"`) {
		t.Error("the marker is drawn above the health line it belongs beside")
	}

	mark := pageFunc(t, "renderHealthMark")
	for _, want := range []string{
		`const box = $("healthmark"), d = (s && s.deploys) || null;`,
		`const failed = (d && d.health_failed) || [], unread = (d && d.health_unread) || [];`,
		`const parts = [];`,
		`box.hidden = !parts.length;`,
		`box.className = "pill " + (failed.length ? "red" : "degraded");`,
		`box.textContent = parts.join(" · ");`,
	} {
		if !strings.Contains(mark, want) {
			t.Errorf("renderHealthMark has no %q", want)
		}
	}

	// The marker carries the reader's own finding: the apps the deploys reader
	// marked, not a second reading of the rows the block draws.
	if !strings.Contains(pageFunc(t, "renderAll"), "renderDeploys(state); renderHealthMark(state);") {
		t.Error("the page's redraw does not include the health marker with the block it comes from")
	}
}

// The marker is only as good as what the reader hands it, so this executes the
// reader over a town whose apps are the four the strip has to tell apart: one
// that passes, one that is off, one whose newest check failed and one whose
// newest check could not be read. A failed app is named red, an app nobody
// could read is named too — a silent one is the strip lying by omission — and
// the app that is off is in neither, so an untested deployment never reads as a
// green one (gt-t9w60).
func TestDeploysReaderFeedsTheStripWhatItMarks(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/arkansas-beaver-committee", "sloan/beads", "sloan/fractals-nextjs", "sloan/gt-cloud"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/arkansas-beaver-committee": {testHealthRun(1, "success", 5)},
			"sloan/beads":                     {testHealthRun(2, "success", 5)},
			"sloan/fractals-nextjs":           {testHealthRun(3, "success", 5)},
			"sloan/gt-cloud":                  {testHealthRun(4, "failure", 5)},
		},
		jobs: map[int64][]forgejo.ActionRunJob{
			1: healthJobs("success", "success", "success"),
			2: healthJobs("skipped", "skipped", "skipped"),
			3: healthJobs("success", "success", "failure"),
			4: healthJobs("success", "success", "success"),
		},
		jobsErr: map[int64]error{4: io.EOF},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Equal(t, []string{"fractals-nextjs"}, d.HealthFailed)
	assert.Equal(t, []string{"gt-cloud"}, d.HealthUnread)
	assert.Empty(t, d.HealthMissing)

	// The two words and the two colours are the page's, not the reader's, so
	// they are held here to the statuses the reader sends: a row the page has no
	// word for draws the API's own word dim, and no status but a pass may reach
	// the pass colour.
	words := pageTable(t, "HEALTH_RESULT")
	classes := pageTable(t, "HEALTH_RESULT_CLASS")
	for _, status := range []string{healthSuccess, healthFailure, "skipped"} {
		if _, ok := words[status]; !ok {
			t.Errorf("the page has no word for %q, a result the reader puts on a row", status)
		}
	}
	assert.Equal(t, "green", words[healthSuccess], "a pass is not the page's word for a pass")
	assert.Equal(t, "off on purpose", words["skipped"], "a skipped run is a deployment nobody tested, and the page must say so")
	assert.Equal(t, map[string]string{healthSuccess: "good", healthFailure: "badc"}, classes,
		"only a pass and a failure may be coloured, so no other result can read as one")
	if _, coloured := classes[""]; coloured {
		t.Error("a result the reader did not read has a colour, so an unread row could read as one")
	}
}

// pageTable reads one of the page's const value tables, so a test can hold the
// words and colours the page draws with to the values the reader sends
// (gt-t9w60). The page itself is not executed anywhere in this repo — every page
// test reads index.html — and the table is the part of it a reader can check.
func pageTable(t *testing.T, name string) map[string]string {
	t.Helper()

	page := string(indexHTML)
	at := strings.Index(page, "const "+name+" = {")
	if at < 0 {
		t.Fatalf("index.html has no %s table", name)
	}
	body := page[at:]
	end := strings.Index(body, "};")
	if end < 0 {
		t.Fatalf("index.html's %s table is not closed", name)
	}
	out := map[string]string{}
	for _, pair := range strings.Split(body[strings.Index(body, "{")+1:end], ",") {
		key, value, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return out
}
