package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readyStubPayload is the shape `gt ready --json` emits, trimmed to the fields
// /api/ready reads. The type field is `issue_type`, because that is the tag on
// beads.Issue.Type — the whole point of the test below.
const readyStubPayload = `{
  "sources": [
    {
      "name": "town",
      "issues": [
        {"id": "hq-real", "title": "Fix the flaky slot test", "priority": 2, "issue_type": "task"},
        {"id": "gt-epic", "title": "de-flake the suite", "priority": 1, "issue_type": "epic"}
      ]
    }
  ],
  "summary": {"total": 2, "p1_count": 0, "p2_count": 1, "p3_count": 0}
}`

// TestAPIHandler_Ready_CarriesIssueType is a regression test for the field name
// mismatch behind gt-b9wq. `gt ready --json` marshals beads.Issue, whose type
// field is tagged `issue_type`; the handler read `type`, so every Ready Across
// Rigs row typed as "" and no downstream filter could tell an issue from a bug.
func TestAPIHandler_Ready_CarriesIssueType(t *testing.T) {
	binDir := t.TempDir()
	gtPath := filepath.Join(binDir, "gt")
	gtScript := `#!/usr/bin/env sh
set -eu
case "$*" in
  "ready --json") cat <<'PAYLOAD'
` + readyStubPayload + `
PAYLOAD
;;
  *) printf 'unexpected gt args: %s\n' "$*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(gtPath, []byte(gtScript), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	h := &APIHandler{
		gtPath:            gtPath,
		workDir:           t.TempDir(),
		defaultRunTimeout: 5 * time.Second,
		maxRunTimeout:     10 * time.Second,
		cmdSem:            make(chan struct{}, maxConcurrentCommands),
		csrfToken:         "test-token",
	}

	req := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	w := httptest.NewRecorder()
	h.handleReady(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/ready status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp ReadyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	types := map[string]string{}
	for _, item := range resp.Items {
		types[item.ID] = item.Type
	}
	if got, want := types["hq-real"], "task"; got != want {
		t.Errorf("ReadyItem.Type for hq-real = %q, want %q (the handler must read issue_type)", got, want)
	}
	if got, want := types["gt-epic"], "epic"; got != want {
		t.Errorf("ReadyItem.Type for gt-epic = %q, want %q", got, want)
	}
	if resp.Summary.Total != 2 {
		t.Errorf("Summary.Total = %d, want 2", resp.Summary.Total)
	}
}

// TestAPIHandler_Ready_ForcedTimeoutIsNotAnEmptyQueue is the regression test
// behind gt-w7eg. The ready query is the slow one on the dashboard, and the
// handler used to answer a timed-out `gt ready --json` with 200 and zero
// items — the same bytes a genuinely idle town produces. The panel renders
// from that response, so a backend that could not be reached was displayed
// as "0 / No ready work": the failure-equals-success anti-pattern on the one
// panel an operator uses to decide whether the town is starved.
//
// This forces the timeout branch by shrinking the handler's budget rather
// than sleeping out the production 12s, and the fake gt replaces itself with
// `sleep` via exec so the context kill lands on the sleeper itself and no
// orphan outlives the test.
func TestAPIHandler_Ready_ForcedTimeoutIsNotAnEmptyQueue(t *testing.T) {
	binDir := t.TempDir()
	gtPath := filepath.Join(binDir, "gt")
	gtScript := `#!/usr/bin/env sh
set -eu
case "$*" in
  "ready --json") exec sleep 30 ;;
  *) printf 'unexpected gt args: %s\n' "$*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(gtPath, []byte(gtScript), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	h := &APIHandler{
		gtPath:            gtPath,
		workDir:           t.TempDir(),
		defaultRunTimeout: 5 * time.Second,
		maxRunTimeout:     10 * time.Second,
		cmdSem:            make(chan struct{}, maxConcurrentCommands),
		csrfToken:         "test-token",
		readyFetchTimeout: 100 * time.Millisecond,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	w := httptest.NewRecorder()
	h.handleReady(w, req)

	// The load-bearing assertion: a forced timeout must not be a 2xx. If this
	// ever goes back to 200, dashboard.js's failure branch is unreachable and
	// the panel is silently back to reporting an empty queue.
	if w.Code == http.StatusOK {
		t.Fatalf("timed-out /api/ready returned 200; the panel cannot tell this "+
			"apart from an empty queue (body: %s)", w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("timed-out /api/ready status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	// The body must be an error payload rather than a ready one, so a caller
	// (or a log) that reads only the body still sees a failure.
	var resp CommandResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if resp.Success {
		t.Error("Expected Success=false for a timed-out ready fetch")
	}
	if !strings.Contains(resp.Error, "ready work unavailable") {
		t.Errorf("Error = %q, want it to name the ready fetch as the failure", resp.Error)
	}
}
