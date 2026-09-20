package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// poolTown writes a town whose settings carry pool, and returns the town root.
func poolTown(t *testing.T, pool *config.PolecatPool) string {
	t.Helper()
	townRoot := t.TempDir()
	ts := config.NewTownSettings()
	ts.PolecatPool = pool
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatalf("saving town settings: %v", err)
	}
	return townRoot
}

// slotsServer serves llama-server's /slots array: total entries, busy of them
// processing at once.
func slotsServer(t *testing.T, total, busy int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slots" {
			http.NotFound(w, r)
			return
		}
		slots := make([]map[string]any, total)
		for i := range slots {
			slots[i] = map[string]any{"id": i, "n_ctx": 4096, "is_processing": i < busy}
		}
		if err := json.NewEncoder(w).Encode(slots); err != nil {
			t.Errorf("writing slots: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// staticAgents is a fake seat read: the GT_AGENT of every polecat session the
// town would report.
func staticAgents(agents ...string) poolAgentLister {
	return func() ([]string, error) { return agents, nil }
}

func TestFetchLocalPool_ReadsPoolConfigSeatsAndSlots(t *testing.T) {
	town := poolTown(t, &config.PolecatPool{
		LocalAgent:    "local-coder-polecat",
		MaxLocal:      3,
		MinSpawnGap:   "30s",
		OverflowAgent: "deepseek-flash",
	})
	srv := slotsServer(t, 3, 1)

	f := &LiveConvoyFetcher{
		townRoot:       town,
		llamaSlotsURL:  srv.URL + "/slots",
		listPoolAgents: staticAgents("local-coder-polecat", "local-coder-polecat", "deepseek-flash", ""),
	}

	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got == nil {
		t.Fatal("FetchLocalPool() = nil, want the panel data")
	}
	if got.LocalSeats != 2 || got.MaxLocal != 3 {
		t.Errorf("seats = %d/%d, want 2/3", got.LocalSeats, got.MaxLocal)
	}
	if got.SlotsBusy != 1 || got.SlotsTotal != 3 {
		t.Errorf("slots = %d/%d, want 1/3", got.SlotsBusy, got.SlotsTotal)
	}
	if got.LocalAgent != "local-coder-polecat" || got.OverflowAgent != "deepseek-flash" || got.MinSpawnGap != "30s" {
		t.Errorf("config = %q / %q / %q, want the pool's local agent, overflow and gap",
			got.LocalAgent, got.OverflowAgent, got.MinSpawnGap)
	}
	if got.SeatsErr != "" || got.SlotsErr != "" {
		t.Errorf("errors = %q / %q, want none", got.SeatsErr, got.SlotsErr)
	}
}

func TestFetchLocalPool_LlamaServerDown(t *testing.T) {
	town := poolTown(t, &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3})
	srv := slotsServer(t, 1, 0)
	url := srv.URL + "/slots"
	srv.Close() // the port is refused from here on

	f := &LiveConvoyFetcher{
		townRoot:       town,
		llamaSlotsURL:  url,
		listPoolAgents: staticAgents("local-coder-polecat"),
	}

	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got == nil {
		t.Fatal("FetchLocalPool() = nil; a down server must still render the panel")
	}
	if got.SlotsErr != "llama-server: down" {
		t.Errorf("SlotsErr = %q, want %q", got.SlotsErr, "llama-server: down")
	}
	if got.LocalSeats != 1 || got.MaxLocal != 3 {
		t.Errorf("seats = %d/%d, want 1/3: the seat count does not depend on llama-server",
			got.LocalSeats, got.MaxLocal)
	}
}

// The breaker owns the poll cadence: a server answering 500 costs one request
// per backoff window, not one per render.
func TestFetchLocalPool_SlotsPollBacksOffAfterFailure(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	town := poolTown(t, &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3})
	f := &LiveConvoyFetcher{
		townRoot:       town,
		llamaSlotsURL:  srv.URL + "/slots",
		listPoolAgents: staticAgents(),
	}

	for i := range 2 {
		got, err := f.FetchLocalPool()
		if err != nil {
			t.Fatalf("FetchLocalPool() #%d error = %v", i+1, err)
		}
		if got.SlotsErr == "" {
			t.Fatalf("FetchLocalPool() #%d SlotsErr = empty, want the failure reported", i+1)
		}
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("slots requests = %d, want 1: the second render must back off, not re-poll", n)
	}
}

func TestFetchLocalPool_SeatsUnreadable(t *testing.T) {
	town := poolTown(t, &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3})
	srv := slotsServer(t, 2, 1)

	f := &LiveConvoyFetcher{
		townRoot:       town,
		llamaSlotsURL:  srv.URL + "/slots",
		listPoolAgents: func() ([]string, error) { return nil, errors.New("no tmux server") },
	}

	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got.SeatsErr == "" {
		t.Error("SeatsErr = empty; an uncountable seat must not read as zero seats")
	}
	if got.SlotsBusy != 1 || got.SlotsTotal != 2 {
		t.Errorf("slots = %d/%d, want 1/2: a failed seat read does not hide the slots",
			got.SlotsBusy, got.SlotsTotal)
	}
}

func TestFetchLocalPool_NoPoolConfigured(t *testing.T) {
	f := &LiveConvoyFetcher{townRoot: t.TempDir()}
	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got != nil {
		t.Errorf("FetchLocalPool() = %+v, want nil for a town with no polecat_pool", got)
	}
}

// An unset local_agent matches no session: every polecat reports an empty
// GT_AGENT, and counting those would show a full pool on an unconfigured one.
func TestFetchLocalPool_EmptyLocalAgentCountsNoSeats(t *testing.T) {
	town := poolTown(t, &config.PolecatPool{MaxLocal: 3})
	srv := slotsServer(t, 1, 0)

	f := &LiveConvoyFetcher{
		townRoot:       town,
		llamaSlotsURL:  srv.URL + "/slots",
		listPoolAgents: staticAgents("", "", ""),
	}

	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got.LocalSeats != 0 {
		t.Errorf("LocalSeats = %d, want 0 with no local_agent configured", got.LocalSeats)
	}
}

// The seat read parses tmux's own output: "KEY=value" lines, one variable per
// call, and only sessions whose GT_ROLE names a polecat. A polecat without
// GT_AGENT reports an empty agent so it cannot inflate the local count.
func TestPoolAgents_ReadsTmuxEnvironment(t *testing.T) {
	restore := fetcherRunCmd
	t.Cleanup(func() { fetcherRunCmd = restore })

	tmuxOutput := map[string]string{
		"list-sessions -F #{session_name}":        "gt-marble\ngt-slate\ngt-opal\ngt-witness\ngt-refinery\n",
		"show-environment -t gt-marble GT_ROLE":   "GT_ROLE=gastown/polecats/marble\n",
		"show-environment -t gt-marble GT_AGENT":  "GT_AGENT=local-coder-polecat\n",
		"show-environment -t gt-slate GT_ROLE":    "GT_ROLE=gastown/polecats/slate\n",
		"show-environment -t gt-slate GT_AGENT":   "GT_AGENT=deepseek-flash\n",
		"show-environment -t gt-opal GT_ROLE":     "GT_ROLE=gastown/polecats/opal\n",
		"show-environment -t gt-witness GT_ROLE":  "GT_ROLE=gastown/witness\n",
		"show-environment -t gt-refinery GT_ROLE": "GT_ROLE=gastown/refinery\n",
	}
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name != "tmux" {
			return nil, fmt.Errorf("ran %q, want tmux", name)
		}
		joined := strings.Join(args, " ")
		out, ok := tmuxOutput[joined]
		if !ok {
			// gt-opal carries no GT_AGENT: the real command fails.
			return nil, fmt.Errorf("unknown variable")
		}
		return bytes.NewBufferString(out), nil
	}

	f := &LiveConvoyFetcher{}
	got, err := f.poolAgents()
	if err != nil {
		t.Fatalf("poolAgents() error = %v", err)
	}
	// The witness and refinery never reach a GT_AGENT read; a polecat without
	// the variable reports "".
	if want := []string{"local-coder-polecat", "deepseek-flash", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("poolAgents() = %q, want %q", got, want)
	}
}

func TestConvoyHandler_RendersLocalPoolPanel(t *testing.T) {
	mock := &MockConvoyFetcher{
		LocalPool: &LocalPoolData{
			MaxLocal:      3,
			LocalSeats:    2,
			LocalAgent:    "local-coder-polecat",
			OverflowAgent: "deepseek-flash",
			MinSpawnGap:   "30s",
			SlotsBusy:     1,
			SlotsTotal:    3,
		},
	}
	handler, err := NewConvoyHandler(mock, 8*time.Second, "test-token")
	if err != nil {
		t.Fatalf("NewConvoyHandler() error = %v", err)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("Status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	for _, want := range []string{
		"local seats",
		"2/3",
		"slots busy",
		"1/3",
		"local-coder-polecat",
		"deepseek-flash",
		"30s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

func TestConvoyHandler_RendersLocalPoolWithLlamaServerDown(t *testing.T) {
	mock := &MockConvoyFetcher{
		LocalPool: &LocalPoolData{MaxLocal: 3, LocalSeats: 2, LocalAgent: "local-coder-polecat", SlotsErr: "llama-server: down"},
	}
	handler, err := NewConvoyHandler(mock, 8*time.Second, "test-token")
	if err != nil {
		t.Fatalf("NewConvoyHandler() error = %v", err)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	body := w.Body.String()
	for _, want := range []string{"local seats", "2/3", "llama-server: down"} {
		if !strings.Contains(body, want) {
			t.Errorf("down server: rendered page missing %q", want)
		}
	}
}
