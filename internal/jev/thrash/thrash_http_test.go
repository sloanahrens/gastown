package thrash

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// httpJev is an in-process Jev. It answers both questions with fixed
// probabilities, except that a request whose state holds POISON gets a 400, the
// way Jev refuses a bead's own request. It is a RoundTripper, so no socket opens.
type httpJev struct {
	mu     sync.Mutex
	bodies []string
}

func (f *httpJev) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.serve(rec, r)
	return rec.Result(), nil
}

func (f *httpJev) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()
	if strings.Contains(string(body), "POISON") {
		http.Error(w, `{"error":"refused"}`, http.StatusBadRequest)
		return
	}
	var req struct {
		Questions map[string]json.RawMessage `json:"questions"`
	}
	_ = json.Unmarshal(body, &req)
	noul := map[string]float64{qWillRespawn: 0.7, qNeedsSplit: 0.2}
	answers := map[string]map[string]any{}
	for id := range req.Questions {
		answers[id] = map[string]any{"type": "noul", "noul": noul[id]}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": "jev-test", "answers": answers,
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 2},
	})
}

// TestThrashPassAgainstJevHTTP runs a pass through the real Jev client: each
// placement logs one line with both answers, and a bead Jev refuses is skipped
// without holding the pass.
func TestThrashPassAgainstJevHTTP(t *testing.T) {
	t.Parallel()
	fake := &httpJev{}
	client, err := jev.NewClient(
		jev.WithBaseURL("http://jev.test"), jev.WithKey("test-key"),
		jev.WithHTTPClient(&http.Client{Transport: fake}),
		jev.WithLedger(filepath.Join(t.TempDir(), "usage.jsonl")), jev.WithCaller(Caller),
	)
	if err != nil {
		t.Fatal(err)
	}
	p := newPass(t, client,
		placed("gt-a", 1, "First"), placed("gt-p", 2, "POISON"), placed("gt-c", 3, "Third"))

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 2 || sum.Failed != 1 {
		t.Fatalf("asked %d, failed %d; want 2 and 1", sum.Asked, sum.Failed)
	}
	lines := logLines(t, p.LogPath)
	if len(lines) != 2 || lines[0].Bead != "gt-a" || lines[1].Bead != "gt-c" {
		t.Fatalf("log = %+v, want gt-a and gt-c", lines)
	}
	if lines[0].Answers.WillRespawn != 0.7 || lines[0].Answers.NeedsSplit != 0.2 || lines[0].Model != "jev-test" {
		t.Errorf("line = %+v, want both answers from the server", lines[0])
	}
	cur, _, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != "gt-c" {
		t.Errorf("cursor at %s, want gt-c", cur.ID)
	}
}
