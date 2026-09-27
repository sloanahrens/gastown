//go:build browser

package web

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/steveyegge/gastown/internal/activity"
)

// =============================================================================
// Browser-based E2E Tests using Rod
//
// These tests launch a real browser (Chromium) to verify the convoy dashboard
// works correctly in an actual browser environment.
//
// Run with: go test -tags=browser -v ./internal/web -run TestBrowser
//
// By default, tests run headless. Set BROWSER_VISIBLE=1 to watch:
//   BROWSER_VISIBLE=1 go test -tags=browser -v ./internal/web -run TestBrowser
//
// =============================================================================

// browserTestConfig holds configuration for browser tests
type browserTestConfig struct {
	headless bool
	slowMo   time.Duration
}

// getBrowserConfig returns test configuration based on environment
func getBrowserConfig() browserTestConfig {
	cfg := browserTestConfig{
		headless: true,
		slowMo:   0,
	}

	if os.Getenv("BROWSER_VISIBLE") == "1" {
		cfg.headless = false
		cfg.slowMo = 300 * time.Millisecond
	}

	return cfg
}

// launchBrowser creates a browser instance with the given configuration.
func launchBrowser(cfg browserTestConfig) (*rod.Browser, func()) {
	l := launcher.New().
		NoSandbox(true).
		Headless(cfg.headless)

	if !cfg.headless {
		l = l.Devtools(false)
	}

	u := l.MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()

	if !cfg.headless {
		browser = browser.SlowMotion(cfg.slowMo)
	}

	cleanup := func() {
		browser.MustClose()
		l.Cleanup()
	}

	return browser, cleanup
}

// newBrowserTestServer serves fetcher through the production dashboard mux, so
// the page loads htmx and idiomorph from /static/vendor as scripts. A bare
// ConvoyHandler answers those paths with the dashboard page itself (200
// text/html), which the browser will not execute, leaving window.htmx undefined
// for a reason the rendered markup cannot show (gt-18ox).
func newBrowserTestServer(t *testing.T, fetcher ConvoyFetcher) *httptest.Server {
	t.Helper()

	handler, err := NewDashboardMux(fetcher, nil)
	if err != nil {
		t.Fatalf("NewDashboardMux() error = %v", err)
	}

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// TestBrowser_ConvoyListLoads tests that the convoy list page loads correctly
func TestBrowser_ConvoyListLoads(t *testing.T) {
	// Setup test server with mock data
	fetcher := &MockConvoyFetcher{
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
				Status:       "closed",
				Progress:     "3/3",
				Completed:    3,
				Total:        3,
				LastActivity: activity.Calculate(time.Now().Add(-10 * time.Minute)),
			},
		},
	}

	ts := newBrowserTestServer(t, fetcher)

	cfg := getBrowserConfig()
	browser, cleanup := launchBrowser(cfg)
	defer cleanup()

	page := browser.MustPage(ts.URL).Timeout(30 * time.Second)
	defer page.MustClose()

	page.MustWaitLoad()

	// Verify page title
	title := page.MustElement("title").MustText()
	if !strings.Contains(title, "Gas Town") {
		t.Fatalf("Expected title to contain 'Gas Town', got: %s", title)
	}

	// Verify convoy IDs are displayed
	bodyText := page.MustElement("body").MustText()
	if !strings.Contains(bodyText, "hq-cv-abc") {
		t.Error("Expected convoy ID hq-cv-abc in page")
	}
	if !strings.Contains(bodyText, "hq-cv-def") {
		t.Error("Expected convoy ID hq-cv-def in page")
	}

	// Verify titles are displayed
	if !strings.Contains(bodyText, "Feature X") {
		t.Error("Expected title 'Feature X' in page")
	}
	if !strings.Contains(bodyText, "Bugfix Y") {
		t.Error("Expected title 'Bugfix Y' in page")
	}

	t.Log("PASSED: Convoy list loads correctly")
}

// TestBrowser_LastActivityColors tests that activity colors are displayed correctly
func TestBrowser_LastActivityColors(t *testing.T) {
	// Setup test server with convoys at different activity ages
	fetcher := &MockConvoyFetcher{
		Convoys: []ConvoyRow{
			{
				ID:           "hq-cv-green",
				Title:        "Active Work",
				Status:       "open",
				LastActivity: activity.Calculate(time.Now().Add(-1 * time.Minute)), // Green: <5min
			},
			{
				ID:           "hq-cv-yellow",
				Title:        "Stale Work",
				Status:       "open",
				LastActivity: activity.Calculate(time.Now().Add(-6 * time.Minute)), // Yellow: 5-10min
			},
			{
				ID:           "hq-cv-red",
				Title:        "Stuck Work",
				Status:       "open",
				LastActivity: activity.Calculate(time.Now().Add(-11 * time.Minute)), // Red: >=10min
			},
		},
	}

	ts := newBrowserTestServer(t, fetcher)

	cfg := getBrowserConfig()
	browser, cleanup := launchBrowser(cfg)
	defer cleanup()

	page := browser.MustPage(ts.URL).Timeout(30 * time.Second)
	defer page.MustClose()

	page.MustWaitLoad()

	// Check for activity color classes in the HTML
	html := page.MustHTML()

	if !strings.Contains(html, "activity-green") {
		t.Error("Expected activity-green class for recent activity")
	}
	if !strings.Contains(html, "activity-yellow") {
		t.Error("Expected activity-yellow class for stale activity")
	}
	if !strings.Contains(html, "activity-red") {
		t.Error("Expected activity-red class for stuck activity")
	}

	t.Log("PASSED: Activity colors display correctly")
}

// TestBrowser_HtmxAutoRefresh tests that htmx auto-refresh attributes are present
func TestBrowser_HtmxAutoRefresh(t *testing.T) {
	fetcher := &MockConvoyFetcher{
		Convoys: []ConvoyRow{
			{
				ID:     "hq-cv-test",
				Title:  "Test Convoy",
				Status: "open",
			},
		},
	}

	ts := newBrowserTestServer(t, fetcher)

	cfg := getBrowserConfig()
	browser, cleanup := launchBrowser(cfg)
	defer cleanup()

	page := browser.MustPage(ts.URL).Timeout(30 * time.Second)
	defer page.MustClose()

	page.MustWaitLoad()

	// Check for htmx attributes
	html := page.MustHTML()

	if !strings.Contains(html, "hx-get") {
		t.Error("Expected hx-get attribute for auto-refresh")
	}
	if !strings.Contains(html, "hx-trigger") {
		t.Error("Expected hx-trigger attribute for auto-refresh")
	}
	if !strings.Contains(html, "every 30s") {
		t.Error("Expected 'every 30s' trigger for auto-refresh")
	}
	if !strings.Contains(html, "gt:dashboard-update") {
		t.Error("Expected 'gt:dashboard-update' SSE trigger for auto-refresh")
	}
	if strings.Contains(html, "sse:dashboard-update") {
		t.Error("hx-trigger must not use 'sse:' prefix — reserved by htmx's SSE extension, " +
			"binding silently fails without an sse-connect source and dashboard never live-updates")
	}

	// Verify htmx binds: the vendored htmx.min.js sets window.htmx (gt-iav5
	// moved the library from unpkg to /static/vendor so the page runs offline)
	bound, evalErr := page.Eval(`() => typeof window.htmx`)
	if evalErr != nil {
		t.Fatalf("Evaluating window.htmx failed: %v", evalErr)
	}
	if bound.Value.Str() == "undefined" {
		t.Error("Expected htmx to be bound — the vendored /static/vendor/htmx.min.js never executed")
	}

	t.Log("PASSED: htmx auto-refresh attributes present")
}

// TestBrowser_EmptyState tests the empty state when no convoys exist
func TestBrowser_EmptyState(t *testing.T) {
	fetcher := &MockConvoyFetcher{
		Convoys: []ConvoyRow{}, // Empty convoy list
	}

	ts := newBrowserTestServer(t, fetcher)

	cfg := getBrowserConfig()
	browser, cleanup := launchBrowser(cfg)
	defer cleanup()

	page := browser.MustPage(ts.URL).Timeout(30 * time.Second)
	defer page.MustClose()

	page.MustWaitLoad()

	// Check for empty state message
	bodyText := page.MustElement("body").MustText()

	if !strings.Contains(bodyText, "No active convoys") {
		t.Errorf("Expected 'No active convoys' empty state message, got: %s", bodyText[:min(len(bodyText), 500)])
	}

	if rows := page.MustElements("tr.convoy-row"); len(rows) != 0 {
		t.Errorf("Expected no convoy rows in the empty state, got %d", len(rows))
	}

	t.Log("PASSED: Empty state displays correctly")
}

// TestBrowser_StatusIndicators tests the work-status badge a convoy row renders
func TestBrowser_StatusIndicators(t *testing.T) {
	fetcher := &MockConvoyFetcher{
		Convoys: []ConvoyRow{
			{
				ID:         "hq-cv-active",
				Title:      "Active Convoy",
				WorkStatus: "active",
				Progress:   "1/3",
				Completed:  1,
				Total:      3,
			},
			{
				ID:         "hq-cv-stuck",
				Title:      "Stuck Convoy",
				WorkStatus: "stuck",
				Progress:   "0/2",
				Total:      2,
			},
		},
	}

	ts := newBrowserTestServer(t, fetcher)

	cfg := getBrowserConfig()
	browser, cleanup := launchBrowser(cfg)
	defer cleanup()

	page := browser.MustPage(ts.URL).Timeout(30 * time.Second)
	defer page.MustClose()

	page.MustWaitLoad()

	active := page.MustElement(`tr[data-convoy-id="hq-cv-active"]`).MustHTML()
	if !strings.Contains(active, "badge-green") {
		t.Error("Expected badge-green on a convoy whose work is progressing")
	}

	stuck := page.MustElement(`tr[data-convoy-id="hq-cv-stuck"]`).MustHTML()
	if !strings.Contains(stuck, "badge-red") {
		t.Error("Expected badge-red on a convoy whose work has stalled")
	}

	t.Log("PASSED: Status indicators display correctly")
}

// TestBrowser_ProgressDisplay tests progress bar rendering
func TestBrowser_ProgressDisplay(t *testing.T) {
	fetcher := &MockConvoyFetcher{
		Convoys: []ConvoyRow{
			{
				ID:        "hq-cv-progress",
				Title:     "Progress Convoy",
				Status:    "open",
				Progress:  "3/7",
				Completed: 3,
				Total:     7,
			},
		},
	}

	ts := newBrowserTestServer(t, fetcher)

	cfg := getBrowserConfig()
	browser, cleanup := launchBrowser(cfg)
	defer cleanup()

	page := browser.MustPage(ts.URL).Timeout(30 * time.Second)
	defer page.MustClose()

	page.MustWaitLoad()

	bodyText := page.MustElement("body").MustText()

	// Verify progress text
	if !strings.Contains(bodyText, "3/7") {
		t.Errorf("Expected progress '3/7' in page, got: %s", bodyText[:min(len(bodyText), 500)])
	}

	// Verify progress bar elements exist
	html := page.MustHTML()
	if !strings.Contains(html, "progress-bar") {
		t.Error("Expected progress-bar class in page")
	}
	if !strings.Contains(html, "progress-fill") {
		t.Error("Expected progress-fill class in page")
	}

	t.Log("PASSED: Progress display works correctly")
}
