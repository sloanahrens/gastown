package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReworkProgramNumbersFeedKey: the rework program's document reaches
// /api/state under its own key, and a read that reports nothing leaves the
// document the feed last carried.
func TestReworkProgramNumbersFeedKey(t *testing.T) {
	t.Parallel()

	var next json.RawMessage
	h := NewHub(Config{
		Now:           func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) },
		ReworkNumbers: func() json.RawMessage { return next },
	})
	h.pollReworkNumbers()
	if h.State().ReworkNumbers != nil {
		t.Fatal("a nil read set the document")
	}

	next = json.RawMessage(`{"numbers":{"two_rounds_share":{"value":0.25}}}`)
	h.pollReworkNumbers()
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"rework_numbers"`, `"two_rounds_share":{"value":0.25}`} {
		if !strings.Contains(body, want) {
			t.Errorf("/api/state does not carry %s: %s", want, body)
		}
	}

	next = nil
	h.pollReworkNumbers()
	if got := h.State().ReworkNumbers; got == nil || !strings.Contains(string(got), "two_rounds_share") {
		t.Errorf("a nil read replaced the last document with %s", got)
	}
}

// TestReworkProgramNumbersEveryDefaultsToAMinute: the step writes weekly, so the
// poll reads the one small file on the slow readers' minute.
func TestReworkProgramNumbersEveryDefaultsToAMinute(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	if c.ReworkNumbersEvery != 60*time.Second {
		t.Errorf("ReworkNumbersEvery defaults to %v, want 1m", c.ReworkNumbersEvery)
	}
}
