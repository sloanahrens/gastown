package dashboard

import (
	"strings"
	"testing"
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
		`note.textContent = err ? "could not read the runs: " + err : (unreadable ? unreadable + (unreadable === 1 ? " repo unreadable" : " repos unreadable") : "");`,
		`box.append(el("div", err ? "badc" : "empty", err ? "no deploy runs: " + err : "no deploy runs yet"));`,
		`for (const r of runs) box.append(deployRow(r));`,
		`if (!runs.length)`,
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

	for _, fn := range []string{"renderDeploys", "deployRow", "deployStages"} {
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
