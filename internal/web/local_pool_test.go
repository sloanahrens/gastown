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
	return poolTownAt(t, pool, "")
}

// poolTownAt also gives the pool's local agent a preset whose ANTHROPIC_BASE_URL
// is baseURL — the only place FetchLocalPool looks for the endpoint to probe.
func poolTownAt(t *testing.T, pool *config.PolecatPool, baseURL string) string {
	t.Helper()
	townRoot := t.TempDir()
	ts := config.NewTownSettings()
	ts.PolecatPool = pool
	if baseURL != "" && pool != nil && pool.LocalAgent != "" {
		if ts.Agents == nil {
			ts.Agents = make(map[string]*config.RuntimeConfig)
		}
		ts.Agents[pool.LocalAgent] = &config.RuntimeConfig{
			Provider: "claude",
			Env:      map[string]string{"ANTHROPIC_BASE_URL": baseURL},
		}
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatalf("saving town settings: %v", err)
	}
	return townRoot
}

// modelServerOpts is what the fake local model server answers. A nil pointer
// omits that field, which is how a server declines to report a figure.
type modelServerOpts struct {
	model     string
	ownedBy   string
	active    *int
	maxActive *int
	noModels  bool // /v1/models 404s: the server is not one we can read
	noHealth  bool // /health 404s, as an OpenAI-compatible-only server does
}

// modelServer serves a local model server's listing and health endpoints.
func modelServer(t *testing.T, o modelServerOpts) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models" && !o.noModels:
			list := map[string]any{"object": "list"}
			if o.model != "" {
				list["data"] = []map[string]any{{"id": o.model, "owned_by": o.ownedBy}}
			} else {
				list["data"] = []map[string]any{}
			}
			if err := json.NewEncoder(w).Encode(list); err != nil {
				t.Errorf("writing models: %v", err)
			}
		case r.URL.Path == "/health" && !o.noHealth:
			health := map[string]any{"ok": true, "model": o.model}
			if o.active != nil {
				health["active_requests"] = *o.active
			}
			if o.maxActive != nil {
				health["max_active_requests"] = *o.maxActive
			}
			if err := json.NewEncoder(w).Encode(health); err != nil {
				t.Errorf("writing health: %v", err)
			}
		default:
			// A server with no /slots is the case that read "down" while up
			// (gt-w0x3): the probe must not depend on one server's endpoints.
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func intPtr(n int) *int { return &n }

// staticAgents is a fake seat read: the GT_AGENT of every polecat session the
// town would report.
func staticAgents(agents ...string) poolAgentLister {
	return func() ([]string, error) { return agents, nil }
}

// The panel reads the endpoint the pool's local agent is configured against:
// a server up on any address, serving any compatible API, is up. The live bug
// was the opposite — a panel that probed a fixed address and one server's
// unique endpoint reported a serving box as down (gt-w0x3).
func TestFetchLocalPool_ReadsPoolConfigSeatsAndServer(t *testing.T) {
	srv := modelServer(t, modelServerOpts{
		model:     "mtplx-qwen38-27b-optimized-speed",
		ownedBy:   "mtplx",
		active:    intPtr(1),
		maxActive: intPtr(3),
	})
	town := poolTownAt(t, &config.PolecatPool{
		LocalAgent:    "local-coder-polecat",
		MaxLocal:      3,
		MinSpawnGap:   "30s",
		OverflowAgent: "deepseek-flash",
	}, srv.URL)

	f := &LiveConvoyFetcher{
		townRoot:       town,
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
	if got.ServerErr != "" {
		t.Fatalf("ServerErr = %q, want none: the configured server is serving", got.ServerErr)
	}
	if got.ServerEndpoint != srv.URL {
		t.Errorf("ServerEndpoint = %q, want %q — the preset's address", got.ServerEndpoint, srv.URL)
	}
	if got.ServerModel != "mtplx-qwen38-27b-optimized-speed" {
		t.Errorf("ServerModel = %q, want the model the server lists", got.ServerModel)
	}
	if got.ServerKind != "mtplx" {
		t.Errorf("ServerKind = %q, want the owner the server reports", got.ServerKind)
	}
	if !got.ServerInFlightKnown || got.ServerInFlight != 1 || got.ServerMaxFlight != 3 {
		t.Errorf("in flight = %d/%d known=%v, want 1/3 known",
			got.ServerInFlight, got.ServerMaxFlight, got.ServerInFlightKnown)
	}
	if got.LocalAgent != "local-coder-polecat" || got.OverflowAgent != "deepseek-flash" || got.MinSpawnGap != "30s" {
		t.Errorf("config = %q / %q / %q, want the pool's local agent, overflow and gap",
			got.LocalAgent, got.OverflowAgent, got.MinSpawnGap)
	}
	if got.SeatsErr != "" {
		t.Errorf("SeatsErr = %q, want none", got.SeatsErr)
	}
}

// A server with no /health still reads up with its model: the health read is
// enrichment, so its absence must not blank the panel.
func TestFetchLocalPool_ServerWithoutHealth(t *testing.T) {
	srv := modelServer(t, modelServerOpts{model: "some-model", noHealth: true})
	town := poolTownAt(t, &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3}, srv.URL)

	f := &LiveConvoyFetcher{townRoot: town, listPoolAgents: staticAgents("local-coder-polecat")}

	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got.ServerErr != "" {
		t.Errorf("ServerErr = %q, want none: /v1/models answered", got.ServerErr)
	}
	if got.ServerModel != "some-model" {
		t.Errorf("ServerModel = %q, want the model", got.ServerModel)
	}
	if got.ServerInFlightKnown {
		t.Error("ServerInFlightKnown = true, want false: nothing reported a count")
	}
}

func TestFetchLocalPool_ModelServerDown(t *testing.T) {
	srv := modelServer(t, modelServerOpts{model: "m"})
	url := srv.URL
	srv.Close() // the port is refused from here on

	town := poolTownAt(t, &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3}, url)
	f := &LiveConvoyFetcher{
		townRoot:       town,
		listPoolAgents: staticAgents("local-coder-polecat"),
	}

	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got == nil {
		t.Fatal("FetchLocalPool() = nil; a down server must still render the panel")
	}
	if got.ServerErr != "down" {
		t.Errorf("ServerErr = %q, want %q", got.ServerErr, "down")
	}
	if got.LocalSeats != 1 || got.MaxLocal != 3 {
		t.Errorf("seats = %d/%d, want 1/3: the seat count does not depend on the model server",
			got.LocalSeats, got.MaxLocal)
	}
}

// A pool whose local agent names no endpoint probes nothing and says so,
// rather than guessing an address and calling whatever answers it local.
func TestFetchLocalPool_NoEndpointConfigured(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "m"}}})
	}))
	defer srv.Close()

	town := poolTown(t, &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3})
	f := &LiveConvoyFetcher{townRoot: town, listPoolAgents: staticAgents("local-coder-polecat")}

	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got.ServerEndpoint != "" {
		t.Errorf("ServerEndpoint = %q, want empty with no preset base URL", got.ServerEndpoint)
	}
	if got.ServerErr != "no endpoint" {
		t.Errorf("ServerErr = %q, want %q: an unconfigured endpoint is not a down server",
			got.ServerErr, "no endpoint")
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Errorf("requests = %d, want 0: no endpoint means no probe", n)
	}
}

// The breaker owns the poll cadence: a server answering 500 costs one request
// per backoff window, not one per render.
func TestFetchLocalPool_ServerPollBacksOffAfterFailure(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	town := poolTownAt(t, &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3}, srv.URL)
	f := &LiveConvoyFetcher{
		townRoot:       town,
		listPoolAgents: staticAgents(),
	}

	for i := range 2 {
		got, err := f.FetchLocalPool()
		if err != nil {
			t.Fatalf("FetchLocalPool() #%d error = %v", i+1, err)
		}
		if got.ServerErr == "" {
			t.Fatalf("FetchLocalPool() #%d ServerErr = empty, want the failure reported", i+1)
		}
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("server requests = %d, want 1: the second render must back off, not re-poll", n)
	}
}

func TestFetchLocalPool_SeatsUnreadable(t *testing.T) {
	srv := modelServer(t, modelServerOpts{model: "m", active: intPtr(2)})
	town := poolTownAt(t, &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 3}, srv.URL)

	f := &LiveConvoyFetcher{
		townRoot:       town,
		listPoolAgents: func() ([]string, error) { return nil, errors.New("no tmux server") },
	}

	got, err := f.FetchLocalPool()
	if err != nil {
		t.Fatalf("FetchLocalPool() error = %v", err)
	}
	if got.SeatsErr == "" {
		t.Error("SeatsErr = empty; an uncountable seat must not read as zero seats")
	}
	if got.ServerErr != "" {
		t.Errorf("ServerErr = %q, want none: a failed seat read does not hide the server",
			got.ServerErr)
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
	srv := modelServer(t, modelServerOpts{model: "m"})
	town := poolTownAt(t, &config.PolecatPool{MaxLocal: 3}, srv.URL)

	f := &LiveConvoyFetcher{
		townRoot:       town,
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

func TestLocalServerEndpoint_ReadsLocalAgentPreset(t *testing.T) {
	townRoot := t.TempDir()
	ts := config.NewTownSettings()
	ts.Agents["coder"] = &config.RuntimeConfig{
		Env: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8099/"},
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatalf("saving town settings: %v", err)
	}

	tests := []struct {
		name  string
		alias string
		want  string
	}{
		{"the alias's base URL, with any trailing slash trimmed", "coder", "http://127.0.0.1:8099"},
		{"an alias with no env", "claude", ""},
		{"an alias that is not configured", "nobody", ""},
		{"no alias at all", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := localServerEndpoint(ts, tt.alias)
			if got != tt.want {
				t.Errorf("localServerEndpoint(%q) = %q, want %q", tt.alias, got, tt.want)
			}
		})
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
			MaxLocal:            3,
			LocalSeats:          2,
			LocalAgent:          "local-coder-polecat",
			OverflowAgent:       "deepseek-flash",
			MinSpawnGap:         "30s",
			ServerEndpoint:      "http://127.0.0.1:8099",
			ServerModel:         "mtplx-qwen38-27b-optimized-speed",
			ServerKind:          "mtplx",
			ServerInFlight:      1,
			ServerMaxFlight:     3,
			ServerInFlightKnown: true,
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
		"model server",
		"up",
		"mtplx-qwen38-27b-optimized-speed",
		"http://127.0.0.1:8099",
		"mtplx",
		"1/3",
		"local-coder-polecat",
		"deepseek-flash",
		"30s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
	if strings.Contains(body, "llama-server") {
		t.Error("rendered page names llama-server; the panel reports the configured endpoint's server")
	}
}

func TestConvoyHandler_RendersLocalPoolWithModelServerDown(t *testing.T) {
	mock := &MockConvoyFetcher{
		LocalPool: &LocalPoolData{
			MaxLocal:       3,
			LocalSeats:     2,
			LocalAgent:     "local-coder-polecat",
			ServerEndpoint: "http://127.0.0.1:8099",
			ServerErr:      "down",
		},
	}
	handler, err := NewConvoyHandler(mock, 8*time.Second, "test-token")
	if err != nil {
		t.Fatalf("NewConvoyHandler() error = %v", err)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	body := w.Body.String()
	for _, want := range []string{"local seats", "2/3", "model server", "down"} {
		if !strings.Contains(body, want) {
			t.Errorf("down server: rendered page missing %q", want)
		}
	}
	if strings.Contains(body, "llama-server") {
		t.Error("rendered page names llama-server")
	}
}
