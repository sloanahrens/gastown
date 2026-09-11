package web

import (
	"bytes"
	"os"
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
