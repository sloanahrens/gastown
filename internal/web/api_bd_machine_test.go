package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// bdAnswers returns a runProc that answers the bd child with what machine mode
// prints: stdout is written as given, stderr as given, and failWith is the
// process error.
func bdAnswers(argv *[]string, stdout, stderr string, failWith error) procRunner {
	return func(_ context.Context, cmd *exec.Cmd) error {
		*argv = cmd.Args[1:]
		if cmd.Stdout != nil {
			_, _ = cmd.Stdout.Write([]byte(stdout))
		}
		if cmd.Stderr != nil {
			_, _ = cmd.Stderr.Write([]byte(stderr))
		}
		return failWith
	}
}

func serveIssue(t *testing.T, h *APIHandler, req *http.Request) map[string]any {
	t.Helper()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dashboard-Token", "test-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	return resp
}

func TestAPIHandler_IssueCreate_ReadsSilentID(t *testing.T) {
	h := newFastAPIHandler(t)
	var argv []string
	// Machine mode wraps create's output in an envelope; --silent still yields
	// the bare id once the wrapper is removed.
	h.runProc = bdAnswers(&argv, `{"schema_version":1,"contract_version":1,"data":{"id":"gt-new1","title":"t"},"pagination":null,"error":null}`, "warning: slow dolt\n", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/issues/create", bytes.NewBufferString(`{"title":"t"}`))
	resp := serveIssue(t, h, req)

	if resp["success"] != true || resp["id"] != "gt-new1" {
		t.Errorf("response = %v, want success with id gt-new1", resp)
	}
	if !slices.Contains(argv, "--silent") {
		t.Errorf("argv %v lacks --silent", argv)
	}
	if i := slices.Index(argv, "--"); i < slices.Index(argv, "--silent") {
		t.Errorf("argv %v puts --silent after the -- that ends flags", argv)
	}
}

func TestAPIHandler_IssueCreate_FailureShowsStderrNotEnvelope(t *testing.T) {
	h := newFastAPIHandler(t)
	var argv []string
	h.runProc = bdAnswers(&argv, `{"schema_version":1,"contract_version":1,"data":null,"error":{"kind":"store_unavailable","message":"no db"}}`, "Error: no db\n", errors.New("exit status 25"))

	req := httptest.NewRequest(http.MethodPost, "/api/issues/create", bytes.NewBufferString(`{"title":"t"}`))
	resp := serveIssue(t, h, req)

	if resp["success"] != false {
		t.Fatalf("response = %v, want failure", resp)
	}
	if msg, _ := resp["message"].(string); msg != "Error: no db" {
		t.Errorf("message = %q, want bd's stderr prose", msg)
	}
}

func TestAPIHandler_IssueShow_ReadsEnvelope(t *testing.T) {
	h := newFastAPIHandler(t)
	var argv []string
	h.runProc = bdAnswers(&argv, `{"schema_version":1,"contract_version":1,"data":[{"id":"gt-abc","title":"Deploy widget","status":"open","priority":1,"issue_type":"task"}],"pagination":null,"error":null}`, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/issues/show?id=gt-abc", nil)
	resp := serveIssue(t, h, req)

	if resp["title"] != "Deploy widget" || resp["status"] != "open" {
		t.Errorf("response = %v, want the issue from the envelope's data", resp)
	}
}

func TestAPIHandler_IssueShow_StderrWarningDoesNotBreakParse(t *testing.T) {
	h := newFastAPIHandler(t)
	var argv []string
	h.runProc = bdAnswers(&argv, `[{"id":"gt-abc","title":"Deploy widget","status":"open","priority":2}]`, "warning: slow dolt\n", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/issues/show?id=gt-abc", nil)
	resp := serveIssue(t, h, req)

	if resp["title"] != "Deploy widget" {
		t.Errorf("response = %v, want the issue despite bd's stderr warning", resp)
	}
}

func TestAPIHandler_IssueShow_UnexpectedOutputIsAnError(t *testing.T) {
	h := newFastAPIHandler(t)
	var argv []string
	h.runProc = bdAnswers(&argv, `[]`, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/issues/show?id=gt-abc", nil)
	resp := serveIssue(t, h, req)

	if msg, _ := resp["error"].(string); !strings.Contains(msg, "unexpected bd show output") {
		t.Errorf("response = %v, want an unexpected-output error", resp)
	}
}
