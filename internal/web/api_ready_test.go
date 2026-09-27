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

// newReadyStubHandler builds an APIHandler whose `gt` is a shell stub that
// prints payload for `ready --json` and fails loudly on any other argv, so a
// handler change that shells out differently is caught rather than served.
func newReadyStubHandler(t *testing.T, payload string) *APIHandler {
	t.Helper()

	binDir := t.TempDir()
	gtPath := filepath.Join(binDir, "gt")
	gtScript := `#!/usr/bin/env sh
set -eu
case "$*" in
  "ready --json") cat <<'PAYLOAD'
` + payload + `
PAYLOAD
;;
  *) printf 'unexpected gt args: %s\n' "$*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(gtPath, []byte(gtScript), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	return &APIHandler{
		gtPath:            gtPath,
		workDir:           t.TempDir(),
		defaultRunTimeout: 5 * time.Second,
		maxRunTimeout:     10 * time.Second,
		cmdSem:            make(chan struct{}, maxConcurrentCommands),
		csrfToken:         "test-token",
	}
}

// readyAllFailedPayload is `gt ready --json` with every source reporting an
// error. Each source is queried independently, so the subprocess still exits 0
// and prints this — the failure rides in per-source "error", not in the exit
// code (internal/cmd/ready.go).
const readyAllFailedPayload = `{
  "sources": [
    {"name": "town", "issues": [], "error": "dolt: connection refused"},
    {"name": "gastown", "issues": [], "error": "dolt: connection refused"}
  ],
  "summary": {"total": 0, "p0_count": 0, "p1_count": 0, "p2_count": 0, "p3_count": 0}
}`

// readyPartialFailurePayload is the mixed case: town answered with real work,
// one rig's query failed.
const readyPartialFailurePayload = `{
  "sources": [
    {"name": "town", "issues": [
      {"id": "hq-real", "title": "Fix the flaky slot test", "priority": 2, "issue_type": "task"}
    ]},
    {"name": "gastown", "issues": [], "error": "dolt: connection refused"}
  ],
  "summary": {"total": 1, "p0_count": 0, "p1_count": 0, "p2_count": 1, "p3_count": 0}
}`

// TestAPIHandler_Ready_AllSourcesFailedIsNotAnEmptyQueue is the regression test
// behind gt-b3zk, the per-source half of gt-w7eg. `gt ready --json` fans out
// across town and every rig's bd independently and still exits 0 when some or
// all of them fail, reporting each failure in that source's "error" field. The
// handler did not read that field, so a town whose every source was
// unreachable produced 200 with zero items — byte-identical to a genuinely
// idle town, and rendered by the panel as "0 / No ready work".
//
// The whole-fetch failure above already answers 503; a total per-source failure
// must answer the same way, because neither one is a board an operator can read.
func TestAPIHandler_Ready_AllSourcesFailedIsNotAnEmptyQueue(t *testing.T) {
	h := newReadyStubHandler(t, readyAllFailedPayload)

	req := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	w := httptest.NewRecorder()
	h.handleReady(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("all-sources-failed /api/ready returned 200; the panel cannot "+
			"tell this apart from an empty queue (body: %s)", w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("all-sources-failed /api/ready status = %d, want %d",
			w.Code, http.StatusServiceUnavailable)
	}

	var resp CommandResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if resp.Success {
		t.Error("Expected Success=false when every ready source failed")
	}
	if !strings.Contains(resp.Error, "ready work unavailable") {
		t.Errorf("Error = %q, want it to name the ready fetch as the failure", resp.Error)
	}
	if !strings.Contains(resp.Error, "gastown") {
		t.Errorf("Error = %q, want it to name the sources that failed (gt-b3zk)", resp.Error)
	}
}

// TestAPIHandler_Ready_PartialFailureNamesTheMissingSource covers the case the
// 503 cannot: one source answered and another did not. Those rows are real work
// and stay on the wire, but the response must also carry which source is
// missing — otherwise the panel shows a complete-looking board that silently
// omits a whole rig, which is the same under-report in a friendlier costume.
func TestAPIHandler_Ready_PartialFailureNamesTheMissingSource(t *testing.T) {
	h := newReadyStubHandler(t, readyPartialFailurePayload)

	req := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	w := httptest.NewRecorder()
	h.handleReady(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("partial-failure /api/ready status = %d, want %d (body: %s)",
			w.Code, http.StatusOK, w.Body.String())
	}

	var resp ReadyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp.Items) != 1 || resp.Items[0].ID != "hq-real" {
		t.Errorf("Items = %+v, want the one row the answering source reported",
			resp.Items)
	}
	if got, want := len(resp.FailedSources), 1; got != want {
		t.Fatalf("FailedSources = %v, want exactly the failed source", resp.FailedSources)
	}
	if resp.FailedSources[0] != "gastown" {
		t.Errorf("FailedSources = %v, want [gastown]", resp.FailedSources)
	}
}

// TestAPIHandler_Ready_HealthyResponseOmitsFailedSources pins the wire shape of
// the common case. FailedSources is omitempty so a fully successful fetch is
// byte-for-byte what it was before gt-b3zk added the field; a caller that has
// never heard of it sees no change.
func TestAPIHandler_Ready_HealthyResponseOmitsFailedSources(t *testing.T) {
	h := newReadyStubHandler(t, readyStubPayload)

	req := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	w := httptest.NewRecorder()
	h.handleReady(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("healthy /api/ready status = %d, want %d", w.Code, http.StatusOK)
	}
	if strings.Contains(w.Body.String(), "failed_sources") {
		t.Errorf("healthy response carries failed_sources; the field must be "+
			"absent when every source answered: %s", w.Body.String())
	}
}

// TestAPIHandler_Ready_CarriesIssueType is a regression test for the field name
// mismatch behind gt-b9wq. `gt ready --json` marshals beads.Issue, whose type
// field is tagged `issue_type`; the handler read `type`, so every Ready Across
// Rigs row typed as "" and no downstream filter could tell an issue from a bug.
func TestAPIHandler_Ready_CarriesIssueType(t *testing.T) {
	h := newReadyStubHandler(t, readyStubPayload)

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
