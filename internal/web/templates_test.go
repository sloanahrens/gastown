package web

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/activity"
)

func TestConvoyTemplate_RendersConvoyList(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Convoys: []ConvoyRow{
			{
				ID:           "hq-cv-abc",
				Title:        "Feature X",
				Status:       "open",
				Progress:     "2/5",
				Completed:    2,
				Total:        5,
				LastActivity: activity.Calculate(time.Now().Add(-1 * time.Minute)),
			},
			{
				ID:           "hq-cv-def",
				Title:        "Bugfix Y",
				Status:       "open",
				Progress:     "1/3",
				Completed:    1,
				Total:        3,
				LastActivity: activity.Calculate(time.Now().Add(-3 * time.Minute)),
			},
		},
	}

	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "convoy.html", data)
	if err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}

	output := buf.String()

	// Check convoy IDs are rendered
	if !strings.Contains(output, "hq-cv-abc") {
		t.Error("Template should contain convoy ID hq-cv-abc")
	}
	if !strings.Contains(output, "hq-cv-def") {
		t.Error("Template should contain convoy ID hq-cv-def")
	}

	// The simplified dashboard no longer shows convoy titles in the table,
	// only the convoy IDs. Titles are shown in expanded view.
}

func TestDashboardScript_SlingUsesLongRunTimeout(t *testing.T) {
	js, err := os.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("ReadFile(static/dashboard.js) error = %v", err)
	}

	if !strings.Contains(string(js), `JSON.stringify({ command: cmd, confirmed: true, timeout: 120 })`) {
		t.Error("Sling action should request a long /api/run timeout")
	}
}

// dashJSFuncBody returns the source of the named function from dashboard.js,
// from its `function <name>(` declaration to the matching closing brace. It
// fails the test if the anchor is gone, so a rename surfaces as a test
// failure instead of a silently vacuous assertion.
func dashJSFuncBody(t *testing.T, content, name string) string {
	t.Helper()
	decl := "function " + name + "("
	start := strings.Index(content, decl)
	if start == -1 {
		t.Fatalf("dashboard.js has no %s() — update this test's anchors", decl)
	}
	depth := 0
	for i := start; i < len(content); i++ {
		switch content[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return content[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces in %s() — update this test's anchors", decl)
	return ""
}

// TestDashboardScript_ReadyFailureIsNotAnEmptyQueue guards the client half of
// gt-w7eg. The server-side companion tests (see api_ready_test.go) make a
// failed fetch answer 503, but the panel is what the operator actually reads:
// if the failed-fetch render leaves a number in the count badge or a "No ready
// work" body on screen, a broken panel is once again indistinguishable from a
// town with nothing to do — which is precisely what was observed live, count 0
// over a queue of 100 ready issues.
//
// dashboard.js has no JS runtime in this suite (there is no JS engine in
// go.mod and the rod tests are behind the opt-in `browser` build tag), so this
// asserts against the render contract of the code that runs, anchored to the
// extracted function bodies rather than the whole file.
func TestDashboardScript_ReadyFailureIsNotAnEmptyQueue(t *testing.T) {
	js, err := os.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("ReadFile(static/dashboard.js) error = %v", err)
	}
	content := string(js)

	render := dashJSFuncBody(t, content, "renderReadyError")
	load := dashJSFuncBody(t, content, "loadReady")

	// The badge must carry a non-numeric marker. '?' is the sentinel; a digit
	// here would be the bug.
	if !strings.Contains(render, "count.textContent = '?'") {
		t.Error("renderReadyError must set the count badge to a non-numeric marker ('?'); " +
			"any number, 0 included, reads as a real count (gt-w7eg)")
	}
	if strings.Contains(render, "count.textContent = '0'") {
		t.Error("renderReadyError must not fall back to 0 — that is what an empty queue shows")
	}
	if !strings.Contains(render, "count.classList.add('count-error')") {
		t.Error("renderReadyError must mark the badge with the count-error class so it is visually distinct from a count")
	}
	// A stale "No ready work" left visible beside the error is the same
	// confusion with extra steps.
	if !strings.Contains(render, "empty.style.display = 'none'") {
		t.Error("renderReadyError must hide the 'No ready work' empty state; leaving it up reports an empty queue on a failed fetch")
	}
	if !strings.Contains(render, "table.style.display = 'none'") {
		t.Error("renderReadyError must hide the (possibly stale) ready table")
	}
	// The error text must still reach the body.
	if !strings.Contains(render, "loading.textContent = text") {
		t.Error("renderReadyError must render the error text in the panel body")
	}

	// The failure path must actually route through it.
	if !strings.Contains(load, "renderReadyError('Failed to load ready work: '") {
		t.Error("loadReady's catch must render through renderReadyError so the badge cannot keep a numeric value")
	}

	// And a clean fetch must clear the error state again.
	if !strings.Contains(load, "clearReadyError()") {
		t.Error("loadReady's success path must clear the error badge state")
	}

	// The client must outwait the endpoint's own 12s budget, or it aborts the
	// request the handler is still working on and never sees the honest 503.
	if !strings.Contains(load, "fetchPanelJSON('/api/ready', READY_FETCH_TIMEOUT_MS)") {
		t.Error("loadReady must pass its tiered timeout: the default 8s panel budget is " +
			"shorter than the endpoint's 12s budget, so the client gives up before the server answers (gt-w7eg)")
	}
	if !strings.Contains(content, "var READY_FETCH_TIMEOUT_MS = 15000;") {
		t.Error("READY_FETCH_TIMEOUT_MS must exceed readyFetchTimeoutDefault (12s) so the server's error arrives first")
	}
}

// TestDashboardScript_ReadyPartialFailureIsNotAnEmptyQueue guards the client
// half of gt-b3zk, the per-source case. The server now answers a partial read
// with 200 plus a failed_sources list (see api_ready_test.go), but 200 is the
// same status a whole board arrives with — so if the client ignores that list,
// a rig that could not be reached renders exactly like a rig with nothing to
// do, and the panel is back to reporting an incomplete town as a complete one.
//
// Same approach as its gt-w7eg sibling: no JS engine here, so this asserts
// against the render contract of the extracted function bodies.
func TestDashboardScript_ReadyPartialFailureIsNotAnEmptyQueue(t *testing.T) {
	js, err := os.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("ReadFile(static/dashboard.js) error = %v", err)
	}
	content := string(js)

	partial := dashJSFuncBody(t, content, "renderReadyPartialError")
	load := dashJSFuncBody(t, content, "loadReady")

	// The success path must read the server's failed_sources list; without this
	// the 200 is taken at face value.
	if !strings.Contains(load, "data.failed_sources") {
		t.Error("loadReady must read failed_sources from the response — a partial read arrives " +
			"as a 200, so the status code alone cannot flag it (gt-b3zk)")
	}
	if !strings.Contains(load, "renderReadyPartialError(failed, total)") {
		t.Error("loadReady's success path must route a failed_sources list through renderReadyPartialError")
	}

	// Zero rows plus a failed source must not render as "No ready work": that
	// sentence is only true when every source answered.
	if !strings.Contains(load, "empty.style.display = failed.length > 0 ? 'none' : 'block'") {
		t.Error("loadReady must suppress the 'No ready work' empty state when a source failed; " +
			"zero visible rows then means 'nothing we could see', not 'nothing to do'")
	}

	// The badge is the load-bearing part, exactly as in renderReadyError: with
	// no rows, any number — 0 included — reads as a real count.
	if !strings.Contains(partial, "count.textContent = total > 0 ? total : '?'") {
		t.Error("renderReadyPartialError must drop the badge to '?' when no rows came back; " +
			"a 0 there is what a genuinely idle town shows")
	}
	if !strings.Contains(partial, "count.classList.add('count-error')") {
		t.Error("renderReadyPartialError must mark the badge so a partial count is visually distinct")
	}
	if !strings.Contains(partial, "var names = failedSources.join(', ')") {
		t.Error("renderReadyPartialError must render the failed source names, not a generic message")
	}
	if !strings.Contains(partial, "loading.textContent = 'Ready work may be incomplete") {
		t.Error("renderReadyPartialError must put the degradation in the panel body, " +
			"where an operator reading the list sees it")
	}
	// The class is what carries the warning treatment; a paint that never adds
	// it looks exactly like the loading placeholder.
	if !strings.Contains(partial, "loading.classList.add('ready-partial')") {
		t.Error("renderReadyPartialError must add the ready-partial class the warning style hangs off")
	}
}

func TestConvoyTemplate_LastActivityColors(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	tests := []struct {
		name      string
		age       time.Duration
		wantClass string
	}{
		{"green for 1 minute", 1 * time.Minute, "activity-green"},
		{"yellow for 6 minutes", 6 * time.Minute, "activity-yellow"},
		{"red for 11 minutes", 11 * time.Minute, "activity-red"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := ConvoyData{
				Convoys: []ConvoyRow{
					{
						ID:           "hq-cv-test",
						Title:        "Test",
						Status:       "open",
						LastActivity: activity.Calculate(time.Now().Add(-tt.age)),
					},
				},
			}

			var buf bytes.Buffer
			err = tmpl.ExecuteTemplate(&buf, "convoy.html", data)
			if err != nil {
				t.Fatalf("ExecuteTemplate() error = %v", err)
			}

			output := buf.String()
			if !strings.Contains(output, tt.wantClass) {
				t.Errorf("Template should contain class %q for %v age", tt.wantClass, tt.age)
			}
		})
	}
}

func TestConvoyTemplate_HtmxAutoRefresh(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Convoys: []ConvoyRow{
			{
				ID:     "hq-cv-test",
				Title:  "Test",
				Status: "open",
			},
		},
	}

	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "convoy.html", data)
	if err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}

	output := buf.String()

	// Check for htmx attributes
	if !strings.Contains(output, "hx-get") {
		t.Error("Template should contain hx-get for auto-refresh")
	}
	if !strings.Contains(output, "hx-trigger") {
		t.Error("Template should contain hx-trigger for auto-refresh")
	}
	if !strings.Contains(output, "gt:dashboard-update") {
		t.Error("Template should contain gt:dashboard-update trigger")
	}
	if strings.Contains(output, "sse:dashboard-update") {
		t.Error("Template must not use 'sse:' prefix — reserved by htmx's SSE extension, breaks binding")
	}
	if !strings.Contains(output, "every 30s") {
		t.Error("Template should contain polling fallback trigger")
	}
}

// extractDivByID returns the substring of html spanning the opening <div ...>
// tag whose attributes contain id="id" through its matching </div>, tracking
// nested div depth so it doesn't stop at the first unrelated closing tag.
func extractDivByID(t *testing.T, html, id string) string {
	t.Helper()

	idAttr := `id="` + id + `"`
	idIdx := strings.Index(html, idAttr)
	if idIdx == -1 {
		t.Fatalf("could not find %s in template output", idAttr)
	}
	tagStart := strings.LastIndex(html[:idIdx], "<div")
	if tagStart == -1 {
		t.Fatalf("could not find opening <div for %s", idAttr)
	}
	tagOpenEnd := strings.Index(html[tagStart:], ">")
	if tagOpenEnd == -1 {
		t.Fatalf("could not find end of opening <div> tag for %s", idAttr)
	}
	cursor := tagStart + tagOpenEnd + 1
	depth := 1
	for depth > 0 {
		nextOpen := strings.Index(html[cursor:], "<div")
		nextClose := strings.Index(html[cursor:], "</div>")
		if nextClose == -1 {
			t.Fatalf("unbalanced <div> for %s: ran out of input before depth reached 0", idAttr)
		}
		if nextOpen != -1 && nextOpen < nextClose {
			depth++
			cursor += nextOpen + len("<div")
			continue
		}
		depth--
		cursor += nextClose + len("</div>")
	}
	return html[tagStart:cursor]
}

// TestConvoyTemplate_DashboardMainScopesHtmxSwap guards gt-8tpe: without
// hx-select scoping the swap to #dashboard-main's own subtree, htmx's
// morph:outerHTML swap takes the ENTIRE response body as the new content —
// including the CDN htmx/idiomorph <script> tags in <head> and
// /static/dashboard.js at the end of <body> — and (allowScriptTags defaults
// true) re-executes dashboard.js on every swap. Each execution opens a fresh
// EventSource, panel-loader interval and htmx:afterSwap listener, multiplying
// until the browser's per-host connection budget is exhausted and unrelated
// panel fetches (crew/mail/ready) start timing out.
func TestConvoyTemplate_DashboardMainScopesHtmxSwap(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Convoys: []ConvoyRow{
			{ID: "hq-cv-test", Title: "Test", Status: "open"},
		},
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "convoy.html", data); err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}
	output := buf.String()

	dashboardMain := extractDivByID(t, output, "dashboard-main")

	openingTagEnd := strings.Index(dashboardMain, ">")
	if openingTagEnd == -1 {
		t.Fatal("could not isolate #dashboard-main's opening tag")
	}
	openingTag := dashboardMain[:openingTagEnd]
	if !strings.Contains(openingTag, `hx-select="#dashboard-main"`) {
		t.Errorf(`#dashboard-main must carry hx-select="#dashboard-main" so htmx's `+
			"morph:outerHTML swap excludes <script> tags outside the div (gt-8tpe); got opening tag: %s",
			openingTag)
	}

	if strings.Contains(dashboardMain, "<script") {
		t.Error("no <script> tag may live inside #dashboard-main (gt-8tpe): a script inside the " +
			"swapped region re-executes on every htmx swap even with hx-select scoping in place")
	}
}

// TestDashboardScript_HasIdempotencyGuard guards gt-8tpe defense-in-depth:
// if a future template change ever lets dashboard.js's own <script> tag back
// into the swapped #dashboard-main content, a second execution must bail out
// immediately instead of opening a second EventSource and duplicating every
// panel-loader and htmx:afterSwap listener.
func TestDashboardScript_HasIdempotencyGuard(t *testing.T) {
	js, err := os.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("ReadFile(static/dashboard.js) error = %v", err)
	}
	content := string(js)

	if !strings.Contains(content, "window.__gtDashboardBooted") {
		t.Fatal("dashboard.js should guard against re-execution via window.__gtDashboardBooted (gt-8tpe)")
	}

	iifeStart := strings.Index(content, "(function()")
	guardIdx := strings.Index(content, "window.__gtDashboardBooted")
	firstRealStatement := strings.Index(content, "var _origFetch = window.fetch;")
	if iifeStart == -1 || firstRealStatement == -1 {
		t.Fatal("dashboard.js structure changed; update this test's anchors")
	}
	if !(iifeStart < guardIdx && guardIdx < firstRealStatement) {
		t.Error("the __gtDashboardBooted guard must run before any other work in the IIFE " +
			"(e.g. before CSRF fetch patching), or a re-execution still does damage before bailing out")
	}
}

// TestConvoyTemplate_ServesNoExternalAssets guards gt-iav5: the dashboard is a
// local ops tool that must render with outbound network blocked, so htmx and
// the idiomorph morph extension are vendored under /static/vendor instead of
// loaded from a CDN. Any remote asset in the rendered page is a failure mode:
// if unpkg is unreachable the page loads but the htmx refresh never binds,
// which is worse than not loading at all.
func TestConvoyTemplate_ServesNoExternalAssets(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Convoys: []ConvoyRow{
			{ID: "hq-cv-test", Title: "Test", Status: "open"},
		},
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "convoy.html", data); err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}
	output := buf.String()

	const re = `href="http|src="http|href='http|src='http`
	if match := regexp.MustCompile(re).FindAllString(output, -1); len(match) > 0 {
		t.Errorf("rendered page references external asset(s) %v (gt-iav5): every script and stylesheet must be served from /static", match)
	}

	// The two refresh-critical libraries must come from the local vendor dir.
	for _, want := range []string{
		`<script src="/static/vendor/htmx.min.js"></script>`,
		`<script src="/static/vendor/idiomorph-ext.min.js"></script>`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("rendered page must load %q from the local vendor dir (gt-iav5)", want)
		}
	}

	// Vendored files must exist on disk where the dashboard serves them from.
	for _, name := range []string{"htmx.min.js", "idiomorph-ext.min.js"} {
		if _, err := os.ReadFile("static/vendor/" + name); err != nil {
			t.Errorf("static/vendor/%s is missing — /static/vendor would serve a 404 (gt-iav5)", name)
		}
	}
}

func TestConvoyTemplate_ProgressDisplay(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Convoys: []ConvoyRow{
			{
				ID:        "hq-cv-test",
				Title:     "Test",
				Status:    "open",
				Progress:  "3/7",
				Completed: 3,
				Total:     7,
			},
		},
	}

	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "convoy.html", data)
	if err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}

	output := buf.String()

	// Check progress is displayed
	if !strings.Contains(output, "3/7") {
		t.Error("Template should display progress '3/7'")
	}
}

func TestConvoyTemplate_StatusIndicators(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Convoys: []ConvoyRow{
			{
				ID:         "hq-cv-active",
				Title:      "Active Convoy",
				Status:     "open",
				WorkStatus: "active",
			},
			{
				ID:         "hq-cv-stuck",
				Title:      "Stuck Convoy",
				Status:     "open",
				WorkStatus: "stuck",
			},
		},
	}

	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "convoy.html", data)
	if err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}

	output := buf.String()

	// Check work status badges are rendered (replaced status-open/closed classes)
	if !strings.Contains(output, "badge-green") {
		t.Error("Template should contain badge-green class for active status")
	}
	if !strings.Contains(output, "badge-red") {
		t.Error("Template should contain badge-red class for stuck status")
	}
}

// TestConvoyTemplate_UnreadableConvoyRenders covers gt-huzu: a convoy whose
// detail read failed renders as a row marked Unknown, and the panel names how
// many rows are unreadable. The list must not read as whole when part of it
// failed to load.
func TestConvoyTemplate_UnreadableConvoyRenders(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Convoys: []ConvoyRow{
			{ID: "hq-cv-ok", Title: "Readable", Status: "open", Progress: "1/2", Total: 2},
			{
				ID:           "hq-cv-slow",
				Title:        "Unreadable",
				Status:       "open",
				Progress:     "—",
				DetailErr:    convoyDetailUnavailable,
				LastActivity: activity.Info{FormattedAge: convoyDetailUnavailable, ColorClass: activity.ColorUnknown},
			},
		},
		UnreadableConvoys: 1,
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "convoy.html", data); err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "hq-cv-slow") {
		t.Error("an unreadable convoy must still be listed, not omitted")
	}
	// Anchored to the badge's own title, not the word "Unknown": the mayor
	// banner renders "Unknown" too when no mayor is attached, so a bare word
	// check passes on a panel that never marked the row (gt-huzu).
	if !strings.Contains(output, "Progress could not be read") {
		t.Error("an unreadable convoy must carry the Unknown badge in its status cell")
	}
	if !strings.Contains(output, "convoy-unreadable-note") {
		t.Error("the panel must say how many of its rows are unreadable")
	}
	if !strings.Contains(output, "1 of 2 convoys unreadable") {
		t.Error("the panel warning must name the unreadable count against the total")
	}
	if !strings.Contains(output, "1/2") {
		t.Error("a readable convoy beside an unreadable one must still show its progress")
	}
}

func TestConvoyTemplate_EmptyState(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Convoys: []ConvoyRow{},
	}

	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "convoy.html", data)
	if err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}

	output := buf.String()

	// Check for empty state message
	if !strings.Contains(output, "No active convoys") {
		t.Error("Template should show empty state message when no convoys")
	}
}

// TestConvoyTemplate_PolecatsPanelShowsAgentAndMR covers gt-kqi2: the Polecats
// panel's AGENT and MR columns, and the "Idle (merged)" reading for an idle
// polecat whose merge request already landed.
func TestConvoyTemplate_PolecatsPanelShowsAgentAndMR(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Workers: []WorkerRow{
			{
				Name:       "malachite",
				Rig:        "gastown",
				AgentType:  "polecat",
				WorkStatus: "idle",
				Agent:      "claude-opus-5",
				MRID:       "gt-wisp-merged",
				MRStatus:   "merged",
			},
			{
				Name:       "opal",
				Rig:        "gastown",
				AgentType:  "polecat",
				WorkStatus: "idle",
				Agent:      "deepseek-flash",
				MRStatus:   "unknown",
			},
			{
				Name:       "refinery",
				Rig:        "gastown",
				AgentType:  "refinery",
				WorkStatus: "working",
			},
		},
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "convoy.html", data); err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}
	output := buf.String()

	panel := panelSection(t, output, "🦨 Polecats", "📟 Sessions")

	// The columns exist, not just the data behind them.
	for _, header := range []string{"<th>Agent</th>", "<th>MR</th>"} {
		if !strings.Contains(panel, header) {
			t.Errorf("Polecats panel is missing the %s column", header)
		}
	}

	// The agent column carries the session's coding agent.
	if !strings.Contains(panel, "claude-opus-5") {
		t.Error("Polecats panel should name the polecat's coding agent")
	}

	// The MR column carries the merge request and its state.
	if !strings.Contains(panel, "gt-wisp-merged") {
		t.Error("Polecats panel should show the polecat's merge request id")
	}
	if !strings.Contains(panel, `class="badge badge-blue">merged`) {
		t.Error("Polecats panel should badge a merged MR")
	}

	// "unknown" reaches the panel only when the inventory says the rig's queue
	// could not be read — not as a placeholder the fetcher wrote itself.
	if !strings.Contains(panel, `class="badge badge-muted">unknown`) {
		t.Error("a queue that could not be read should badge as unknown")
	}
	if strings.Contains(panel, `<span class="mr-id"></span>`) {
		t.Error("an MR with no bead behind it should not render an empty mr-id span")
	}

	// An idle polecat whose MR merged says so; an idle polecat without one does
	// not.
	if !strings.Contains(panel, "Idle (merged)") {
		t.Error(`idle row with a merged MR should read "Idle (merged)"`)
	}
	if got := strings.Count(panel, "Idle (merged)"); got != 1 {
		t.Errorf(`"Idle (merged)" rendered %d times, want 1`, got)
	}
	if !strings.Contains(panel, `>Idle</span>`) {
		t.Error("idle row without a merged MR should still read plain Idle")
	}
}

// TestConvoyTemplate_PolecatsPanelShowsPendingMR covers gt-ppja: a finished
// polecat whose MR has not landed reads "MR pending" rather than "Idle", and
// its MR id links to the Merge Queue row that will merge it.
func TestConvoyTemplate_PolecatsPanelShowsPendingMR(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Workers: []WorkerRow{
			{
				Name:       "jasper",
				Rig:        "gastown",
				AgentType:  "polecat",
				WorkStatus: "mr-pending",
				MRID:       "gt-wisp-8cs",
				MRStatus:   "ready",
			},
		},
		TownMergeQueue: TownMergeQueue{
			Loaded: true,
			Rows: []TownMergeQueueRow{
				{ID: "gt-wisp-8cs", Rig: "gastown", Branch: "polecat/jasper/om-d4p", Status: "ready"},
			},
		},
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "convoy.html", data); err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}
	output := buf.String()

	panel := panelSection(t, output, "🦨 Polecats", "📟 Sessions")

	if !strings.Contains(panel, `class="badge badge-yellow">MR pending`) {
		t.Error("a done polecat with an in-flight MR should read MR pending, not Idle")
	}
	if strings.Contains(panel, `>Idle</span>`) {
		t.Error("an MR-pending row is not idle and must not claim to be")
	}

	// The link has to reach the Merge Queue row, or the MR column is a label
	// with nowhere to go.
	const anchor = `href="#mr-gt-wisp-8cs"`
	if !strings.Contains(panel, anchor) {
		t.Errorf("Polecats panel should link the MR id to its queue row; want %s", anchor)
	}
	if !strings.Contains(output, `id="mr-gt-wisp-8cs"`) {
		t.Error(`the Merge Queue row should carry id="mr-gt-wisp-8cs" as the link target`)
	}
}

// panelSection returns the rendered slice between two panel headings, failing
// the test if either is absent — otherwise a renamed heading would silently
// widen the window to the whole page and let the assertions below match another
// panel's text.
func panelSection(t *testing.T, output, from, to string) string {
	t.Helper()
	start := strings.Index(output, from)
	if start < 0 {
		t.Fatalf("rendered page has no %q heading", from)
	}
	section := output[start:]
	end := strings.Index(section, to)
	if end < 0 {
		t.Fatalf("rendered page has no %q heading after %q", to, from)
	}
	return section[:end]
}

// TestConvoyTemplate_MarksParkedRigsAndTheirMRs is the panel half of gt-94xz:
// a parked rig has to be visible on the Rigs row and on every MR row it owns,
// so a stale READY is not read as a refinery stall.
func TestConvoyTemplate_MarksParkedRigsAndTheirMRs(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}

	data := ConvoyData{
		Rigs: []RigRow{
			{Name: "gastown", HasWitness: true, HasRefinery: true},
			{Name: "hm", OpState: "parked"},
		},
		TownMergeQueue: TownMergeQueue{
			Loaded:      true,
			ReadyCount:  2,
			ParkedCount: 1,
			Rows: []TownMergeQueueRow{
				{ID: "hm-wisp-t7f", Rig: "hm", Status: "ready", ColorClass: "mq-green", RigOpState: "parked", Age: "5d"},
				{ID: "gt-wisp-a", Rig: "gastown", Status: "ready", ColorClass: "mq-green", Age: "2m"},
			},
		},
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "convoy.html", data); err != nil {
		t.Fatalf("ExecuteTemplate() error = %v", err)
	}
	output := buf.String()

	// The Rigs panel row for the parked rig carries the marker...
	if !strings.Contains(output, `class="rig-inactive"`) {
		t.Error("parked rig row should carry the rig-inactive class")
	}
	// ...and the rig that accepts work does not.
	if strings.Count(output, "rig-state-badge") != 2 {
		t.Errorf("rig-state-badge appears %d times, want 2 (the parked rig row and its MR row)",
			strings.Count(output, "rig-state-badge"))
	}
	// The MR row is tagged and greyed, while its status keeps saying ready —
	// which is exactly the pairing that was missing.
	if !strings.Contains(output, "mr-row mq-green rig-inactive") {
		t.Error("MR row in a parked rig should carry the rig-inactive class alongside its status colour")
	}
	if !strings.Contains(output, "hm-wisp-t7f") || !strings.Contains(output, "5d") {
		t.Error("parked MR row should still render its id and age")
	}
	// The header has to name the parked work, or the ready count reads as
	// work the refinery could take right now.
	if !strings.Contains(output, "1 in parked rigs") {
		t.Error("merge queue header should report how many MRs sit in parked rigs")
	}
}
