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
// repo, ref, commit, state and age with the stages under it, the reader's own
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

	// The row is the run: every cell of it, the stages under them and the
	// reader's warning under that.
	row := pageFunc(t, "deployRow")
	for _, want := range []string{
		`row.append(repo, el("span", "dref", r.ref || "–"), el("span", "dhash", r.hash || "–"), el("span", "dstate", r.status || "–"), el("span", "dage", r.at ? age(r.at) : "–"), deployStages(r));`,
		`if (r.warn) row.append(el("span", "dwarn warnc", r.warn));`,
		`const row = r.url ? el("a", "drow") : el("div", "drow");`,
		`if (r.url) { row.href = r.url; row.target = "_blank"; row.rel = "noopener"; row.title = r.url; }`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("deployRow has no %q", want)
		}
	}

	// The stage cell is the page's own chip vocabulary, and a run whose jobs
	// were not read says so rather than drawing nothing.
	stages := pageFunc(t, "deployStages")
	for _, want := range []string{
		`cell.textContent = r.stages_unread ? "stages not read" : "–";`,
		`el("span", "tag " + (DEPLOY_STAGE[st.status] || "sev-low"), (st.name || "–") + " " + (st.status || "?"))`,
	} {
		if !strings.Contains(stages, want) {
			t.Errorf("deployStages has no %q", want)
		}
	}

	// The block is drawn with every other pane, and only exists while the
	// reader has reported: the state's field is absent until then.
	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderDeploys(state)") {
		t.Error("renderAll does not draw the Deploys block")
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
