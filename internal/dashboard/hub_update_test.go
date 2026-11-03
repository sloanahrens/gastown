package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stateWithUpdate serves /api/state for a hub whose update provider returns
// next, and returns the body's keys and the update section it carries.
func stateWithUpdate(t *testing.T, next func() *UpdateStatus) (map[string]json.RawMessage, *UpdateStatus) {
	t.Helper()
	h := NewHub(Config{Update: next})
	h.pollUpdate()
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/api/state is not the page's JSON: %v", err)
	}
	var got *UpdateStatus
	if raw, ok := body["update"]; ok {
		got = &UpdateStatus{}
		if err := json.Unmarshal(raw, got); err != nil {
			t.Fatalf("/api/state update section is not an UpdateStatus: %v", err)
		}
	}
	return body, got
}

// The page draws the update job from the feed: /api/state carries the run the
// provider read, its ended_at only once it ended, and a provider with nothing
// to report leaves the section out.
func TestDashboardUpdateStateFeedCarriesTheUpdateSection(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 11, 3, 11, 52, 0, 0, time.UTC)
	u := &UpdateStatus{
		Running:         true,
		RunID:           "run-1",
		Source:          "manual",
		Current:         "install",
		Outcome:         "running",
		StartedAt:       started,
		ElapsedSec:      480,
		HeartbeatAgeSec: 20,
		Steps: []UpdateStep{
			{Name: "plan", WillRun: true, Result: "ok", Detail: "six steps"},
			{Name: "drain", WillRun: true, Result: "running"},
		},
	}
	body, got := stateWithUpdate(t, func() *UpdateStatus { return u })
	if got == nil {
		t.Fatal("/api/state carries no update section")
	}
	if !got.Running || got.RunID != "run-1" || got.Source != "manual" || got.Current != "install" ||
		got.Outcome != "running" || got.ElapsedSec != 480 || got.HeartbeatAgeSec != 20 {
		t.Errorf("/api/state update = %+v", got)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, started)
	}
	if len(got.Steps) != 2 || got.Steps[0] != (UpdateStep{Name: "plan", WillRun: true, Result: "ok", Detail: "six steps"}) {
		t.Errorf("/api/state update steps = %+v", got.Steps)
	}
	var section map[string]json.RawMessage
	if err := json.Unmarshal(body["update"], &section); err != nil {
		t.Fatalf("/api/state update section is not an object: %v", err)
	}
	if _, ok := section["ended_at"]; ok {
		t.Error("/api/state carries ended_at for a running run")
	}

	ended := started.Add(5 * time.Minute)
	_, finished := stateWithUpdate(t, func() *UpdateStatus {
		return &UpdateStatus{RunID: "run-2", Outcome: "ok", StartedAt: started, EndedAt: &ended, ElapsedSec: 300}
	})
	if finished == nil || finished.EndedAt == nil || !finished.EndedAt.Equal(ended) {
		t.Errorf("/api/state finished update = %+v, want EndedAt %v", finished, ended)
	}

	body, none := stateWithUpdate(t, func() *UpdateStatus { return nil })
	if none != nil {
		t.Errorf("/api/state carries an update section for a nil read: %+v", none)
	}
	if _, ok := body["update"]; ok {
		t.Error("/api/state keeps the update key when the provider reports nothing")
	}
}
