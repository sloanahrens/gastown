package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
