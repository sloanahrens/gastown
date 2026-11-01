package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// archiveDigest is a digest in the shape the daemon's report patrol writes it:
// a header line, then the named sections, each row indented under its heading.
const archiveDigest = `overseer report 2026-10-15T20:00:00Z (covers 2026-10-15T19:45:00Z to 2026-10-15T20:00:00Z)

DONE
  gt-a1  landed a thing  gastown
  dv-b2  shipped another  dv

IN PROGRESS
  gastown/nux  gt-c3  working on it  23m

LEFT
  next  gt-d4  gastown  two seats free
  held  gt-e5  gastown  waiting on gt-a1

OPEN QUESTIONS
  gt-q1  2h  held: gt-a1

health: GREEN
`

// The state the page reads carries the panel, and the page is still read-only:
// a GET of /api/state serves the reports the hub polled, and the method that is
// not a read is refused as it always was.
func TestReportsReachTheAPIAndTheGuardStillRefusesWrites(t *testing.T) {
	t.Parallel()

	written := reportsNow.Add(-time.Minute)
	h := NewHub(Config{
		Now: func() time.Time { return reportsNow },
		Reports: func() *Reports {
			return &Reports{At: reportsNow, State: ReportsFresh, Text: "the report", Written: written}
		},
	})
	h.pollReports()

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"reports"`) || !strings.Contains(body, `"the report"`) {
		t.Errorf("/api/state does not carry the report the hub holds: %s", body)
	}

	for _, method := range []string{"POST", "PUT", "DELETE"} {
		req := httptest.NewRequest(method, "/api/state", nil)
		req.Host = "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/state = %d, want the page to refuse it", method, rec.Code)
		}
	}
}
