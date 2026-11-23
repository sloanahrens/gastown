package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The feed keeps the tiersweep section after the page's pane is gone
// (gt-gfglv0): one trend entry per rig, oldest sweep first, each point naming
// the sha, the tier walls and the cycle's total. This is the JSON contract
// between the reader and every feed consumer, so it is checked where a reader
// receives it (hub.go pollTierSweep).
func TestTierSweepTrendReachesThePageJSON(t *testing.T) {
	t.Parallel()

	total := 576.0
	ts := &TierSweep{Trends: []TierSweepTrendRig{{
		Rig: "gastown",
		Sweeps: []TierSweepTrendPoint{
			{At: time.Date(2026, 10, 3, 21, 9, 36, 0, time.UTC), SHA: "b7c1d2e3", Walls: map[string]float64{"shell": 70}, TotalSecs: &total},
			{At: time.Date(2026, 10, 3, 22, 9, 36, 0, time.UTC), SHA: "a9be03e4", Walls: map[string]float64{"shell": 65, "integration": 180}, TotalSecs: &total},
		},
	}}}
	h := NewHub(Config{TierSweep: func() *TierSweep { return ts }})
	h.pollTierSweep()

	b, err := json.Marshal(h.State())
	if err != nil {
		t.Fatalf("marshaling the state: %v", err)
	}
	var got struct {
		TierSweep struct {
			Trends []struct {
				Rig    string `json:"rig"`
				Sweeps []struct {
					At        time.Time          `json:"at"`
					SHA       string             `json:"sha"`
					Walls     map[string]float64 `json:"walls"`
					TotalSecs *float64           `json:"total_secs"`
				} `json:"sweeps"`
			} `json:"trends"`
		} `json:"tiersweep"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("reading the state JSON: %v", err)
	}
	trends := got.TierSweep.Trends
	if len(trends) != 1 || trends[0].Rig != "gastown" || len(trends[0].Sweeps) != 2 {
		t.Fatalf("trends = %+v, want gastown's two sweeps", trends)
	}
	p := trends[0].Sweeps[1]
	if p.SHA != "a9be03e4" || p.Walls["integration"] != 180 || p.TotalSecs == nil || *p.TotalSecs != 576 {
		t.Errorf("point = %+v, want the sha, tier walls and total", p)
	}
	if !p.At.After(trends[0].Sweeps[0].At) {
		t.Error("the page reads the points oldest first")
	}
}

// The page draws no Tier sweeps pane, but the feed keeps both of its sections
// for other readers (gt-gfglv0): /api/state still serves the tiersweep
// snapshot and the update run beside it. This is a feed contract, not a page
// check, so it sits here rather than with the update chip's page tests.
func TestTierSweepAndUpdateSectionsReachTheFeed(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		TierSweep: func() *TierSweep {
			return &TierSweep{Sweeps: []TierSweepRow{{
				At: at, Rig: "gastown", SHA: "a9be03e4",
				Stages: []TierSweepStage{{Tier: "shell", Verdict: "GREEN"}},
			}}}
		},
		Update: func() *UpdateStatus {
			return &UpdateStatus{RunID: "run-1", Outcome: "ok", StartedAt: at}
		},
	})
	h.pollTierSweep()
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

	var sweep TierSweep
	if raw, ok := body["tiersweep"]; !ok {
		t.Error("/api/state dropped the tiersweep section")
	} else if err := json.Unmarshal(raw, &sweep); err != nil {
		t.Fatalf("/api/state's tiersweep section is not a TierSweep: %v", err)
	} else if len(sweep.Sweeps) != 1 || sweep.Sweeps[0].SHA != "a9be03e4" {
		t.Errorf("/api/state's tiersweep sweeps = %+v, want the sweep the reader returned", sweep.Sweeps)
	}

	var update UpdateStatus
	if raw, ok := body["update"]; !ok {
		t.Error("/api/state dropped the update section")
	} else if err := json.Unmarshal(raw, &update); err != nil {
		t.Fatalf("/api/state's update section is not an UpdateStatus: %v", err)
	} else if update.RunID != "run-1" {
		t.Errorf("/api/state's update = %+v, want run-1", update)
	}
}
