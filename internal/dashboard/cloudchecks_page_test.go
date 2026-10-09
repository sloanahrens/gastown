package dashboard

import (
	"strings"
	"testing"
)

// The checks line is composed on the page from the state the reader decided:
// the counts of a fresh run, one dim line for a status it could not use, and
// the stale reading's own words. Drawing it is the page's only chance to get
// the three states apart, so each is pinned.
func TestCloudChecksLineDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "drawCloudChecks")
	for _, want := range []string{
		`if (!checks) return;`,
		`if (checks.state === "error") { box.append(el("div", "empty", "cloud checks: unreadable")); return; }`,
		`"cloud checks: last ran " + age(checks.at) + " ago (not running?)"`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("drawCloudChecks has no %q", want)
		}
	}

	// A fresh run's line is the counts over the whole run and the age, with the
	// line's colour the worst level it carries: failures the alert red, warnings
	// the amber, all ok the page's own style.
	for _, want := range []string{
		`const worst = checks.failed > 0 ? "badc" : (checks.warnings > 0 ? "warnc" : "");`,
		`const counts = [checks.ok + " ok", checks.warnings + " warning" + (checks.warnings === 1 ? "" : "s"), checks.failed + " failed"];`,
		`"checks: " + counts.join(" · ") + " · " + age(checks.at) + " ago"`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("drawCloudChecks has no %q", want)
		}
	}

	// Each failure and warning is a row under the line: its section dim, its
	// text in the alert style for a failure and the warning style for a warning.
	for _, want := range []string{
		`if (it.section) row.append(el("span", "ccsection", it.section));`,
		`row.append(el("span", "cctext " + (it.level === "fail" ? "badc" : "warnc"), it.text || ""));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("drawCloudChecks has no %q", want)
		}
	}

	// A run with failures also marks the pane's note, in the amber, so the
	// header carries the one thing on this line the operator must not miss.
	note := pageFunc(t, "cloudNote")
	if !strings.Contains(note, `checks.state === "fresh" && checks.failed > 0`) {
		t.Error("cloudNote does not carry the checks' failures")
	}
	if !strings.Contains(note, `checks.failed + (checks.failed === 1 ? " check failing" : " checks failing")`) {
		t.Error("cloudNote does not name the failing checks")
	}
	if !strings.Contains(pageFunc(t, "renderCloud"), `const failing = checks && checks.state === "fresh" && checks.failed > 0;`) {
		t.Error("renderCloud does not raise the note for failing checks")
	}
	if !strings.Contains(pageFunc(t, "renderCloud"), `note.className = (stale || failing) ? "r warnc" : "r";`) {
		t.Error("renderCloud does not put the note in the warning style for failing checks")
	}
}

// The line sits above the patrol's findings and is drawn whatever the patrol's
// own state is, so a missing patrol report cannot hide the checks.
func TestCloudChecksLineSitsAboveThePatrolFindings(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderCloud")
	checks := strings.Index(draw, "drawCloudChecks(box, checks);")
	missing := strings.Index(draw, `if (c.state === "missing" || c.state === "error")`)
	if checks < 0 {
		t.Fatal("renderCloud does not draw the checks line")
	}
	if missing < 0 {
		t.Fatal("renderCloud no longer names the missing and error states")
	}
	if checks > missing {
		t.Error("the checks line is drawn after the missing or error state, so it would be hidden")
	}
}

// The status is another process's file, so its strings must reach the page as
// text and never as markup: the line is built from el(), which writes
// textContent, and no part of it parses markup.
func TestCloudChecksLineWritesTextAndNeverMarkup(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"drawCloudChecks", "cloudNote"} {
		if body := pageFunc(t, name); strings.Contains(body, "innerHTML") {
			t.Errorf("%s sets markup from the status", name)
		}
	}

	page := string(indexHTML)
	for _, want := range []string{
		`.ccheck{margin-bottom:4px}`,
		`.ccrow{display:flex;gap:6px;align-items:baseline;padding-left:10px}`,
		`.ccsection{flex:0 0 auto;white-space:nowrap;color:var(--dim)}`,
		`.cctext{min-width:0;overflow-wrap:anywhere}`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no %q", want)
		}
	}
}
