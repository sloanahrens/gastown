package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The birds' reader is built over the town's runtime directory.
func TestDashboardBirdsRuntimeDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if path, want := dashboardRuntimeDir(root), filepath.Join(root, ".runtime"); path != want {
		t.Errorf("the birds are read from %q, want %q", path, want)
	}
}

// gt dashboard's help says what its Birds panel reads, in the whole paragraph
// the command shows.
func TestDashboardBirdsHelpParagraph(t *testing.T) {
	t.Parallel()

	const want = `The Birds panel lists the daily reports the town's birds leave for the operator:
one row per bird with a report under <town>/.runtime/<bird>/reports, from its
daily.json, with the daily.html page beside it openable from the row. A summary
older than 26 hours is marked stale. The panel reads those files and writes
nothing.`
	var found []string
	for _, para := range strings.Split(dashboardCmd.Long, "\n\n") {
		if strings.Contains(para, "Birds panel") {
			found = append(found, para)
		}
	}
	if len(found) != 1 {
		t.Fatalf("gt dashboard's help has %d paragraphs about its Birds panel, want 1", len(found))
	}
	if found[0] != want {
		t.Errorf("gt dashboard's Birds paragraph reads:\n%s\nwant:\n%s", found[0], want)
	}
}

// The hub the command builds answers /api/birds with the row for a bird that
// wrote its daily report under the town's runtime directory, and serves the
// bird's page from that directory. A hub wired to the town root instead of its
// runtime directory answers no row and no page (gt-7f7nn).
func TestDashboardHubServesTheBirdsRuntimeDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runtimeDir := filepath.Join(root, ".runtime")
	reports := filepath.Join(runtimeDir, "dunlin", "reports")
	if err := os.MkdirAll(reports, 0o755); err != nil {
		t.Fatal(err)
	}
	// The sidecar is dated off the same clock the hub's reader calls, so the
	// row is fresh whatever date the suite runs on.
	written := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	sidecar := fmt.Sprintf(`{"bird":"dunlin","written_at":%q,"title":"t","summary":"s"}`, written)
	if err := os.WriteFile(filepath.Join(reports, "daily.json"), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reports, "daily.html"), []byte("<html>all quiet</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	hub, closeSources, err := newDashboardHub(root, birdsNow, time.UTC, "", nil, 0, 0, "", io.Discard)
	if err != nil {
		t.Fatalf("building the dashboard's hub: %v", err)
	}
	defer closeSources()
	handler := hub.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/birds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/birds: status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var rows []dashboard.BirdReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decoding /api/birds: %v (body %q)", err, rec.Body.String())
	}
	if len(rows) != 1 || rows[0].Bird != "dunlin" || rows[0].State != dashboard.BirdReportFresh {
		t.Fatalf("/api/birds returned %+v, want the fresh row for dunlin", rows)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/birds/dunlin/daily.html", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "all quiet") {
		t.Errorf("/birds/dunlin/daily.html: status %d body %q, want the bird's page", rec.Code, rec.Body.String())
	}
}

// The hub marks the overseer's own report as the town's headline and no other
// reporter's, so the page can draw the headline first without carrying a
// reporter's name of its own (gt-5c4df). The name is the directory
// internal/overseer keeps the overseer's files in, which is the one the
// overseer writes its daily report under.
func TestDashboardHubMarksTheOverseerHeadline(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	written := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, bird := range []string{"heron", "overseer"} {
		reports := filepath.Join(root, ".runtime", bird, "reports")
		if err := os.MkdirAll(reports, 0o755); err != nil {
			t.Fatal(err)
		}
		sidecar := fmt.Sprintf(`{"bird":%q,"written_at":%q,"title":"t","summary":"s"}`, bird, written)
		if err := os.WriteFile(filepath.Join(reports, "daily.json"), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	hub, closeSources, err := newDashboardHub(root, birdsNow, time.UTC, "", nil, 0, 0, "", io.Discard)
	if err != nil {
		t.Fatalf("building the dashboard's hub: %v", err)
	}
	defer closeSources()

	rec := httptest.NewRecorder()
	hub.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/birds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/birds: status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var rows []dashboard.BirdReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decoding /api/birds: %v (body %q)", err, rec.Body.String())
	}
	headline := map[string]bool{}
	for _, r := range rows {
		headline[r.Bird] = r.Headline
	}
	if len(headline) != 2 || !headline["overseer"] {
		t.Errorf("the rows mark the headline as %+v, want the overseer's row and only it", headline)
	}
	if headline["heron"] {
		t.Errorf("the rows mark the headline as %+v, want the overseer's row and only it", headline)
	}
}
