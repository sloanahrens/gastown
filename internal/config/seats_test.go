package config

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the window tests read America/Chicago, so they must not depend on the host's zoneinfo
)

// The seat map is the one place a town says which agents run, how many of each
// and when (gt-mukgk): the loader returns them sorted and refuses a map no
// reader could act on.
func TestSeatsSortedAndValidated(t *testing.T) {
	t.Parallel()

	ts := NewTownSettings()
	ts.PolecatPool = &PolecatPool{
		Timezone: "America/Chicago",
		Seats: map[string]SeatConfig{
			"deepseek-flash": {Cap: 3, Closed: "20:00-08:00"},
			"claude-sonnet":  {Cap: 2},
		},
	}
	got, err := Seats(ts, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Agent != "claude-sonnet" || got[1].Agent != "deepseek-flash" {
		t.Fatalf("order = %+v", got)
	}
	if got[0].Window != nil || got[1].Window == nil {
		t.Fatalf("windows = %+v", got)
	}
	if got[1].Cap != 3 || got[1].Window.StartMin != 20*60 || got[1].Window.EndMin != 8*60 {
		t.Fatalf("deepseek-flash seat = %+v", got[1])
	}
}

func TestSeatsRejects(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		pool PolecatPool
		key  string
	}{
		"unknown agent":      {PolecatPool{Seats: map[string]SeatConfig{"nope": {Cap: 1}}}, "polecat_pool.seats.nope"},
		"the unrecorded key": {PolecatPool{Seats: map[string]SeatConfig{UnknownSeatAgent: {Cap: 1}}}, "polecat_pool.seats." + UnknownSeatAgent},
		"negative cap":       {PolecatPool{Seats: map[string]SeatConfig{"claude-sonnet": {Cap: -1}}}, "polecat_pool.seats.claude-sonnet.cap"},
		"bad window":         {PolecatPool{Timezone: "America/Chicago", Seats: map[string]SeatConfig{"claude-sonnet": {Cap: 1, Closed: "8pm-8am"}}}, "polecat_pool.seats.claude-sonnet.closed"},
		"window, no zone":    {PolecatPool{Seats: map[string]SeatConfig{"claude-sonnet": {Cap: 1, Closed: "20:00-08:00"}}}, "polecat_pool.seats.claude-sonnet.closed"},
		"bad zone":           {PolecatPool{Timezone: "Mars/Olympus", Seats: map[string]SeatConfig{"claude-sonnet": {Cap: 1, Closed: "20:00-08:00"}}}, "polecat_pool.timezone"},
		"same start and end": {PolecatPool{Timezone: "UTC", Seats: map[string]SeatConfig{"claude-sonnet": {Cap: 1, Closed: "08:00-08:00"}}}, "polecat_pool.seats.claude-sonnet.closed"},
		"name in two seats": {PolecatPool{Seats: map[string]SeatConfig{
			"claude-sonnet": {Cap: 1, Names: []string{"nux"}},
			"claude-haiku":  {Cap: 1, Names: []string{"nux"}},
		}}, `name "nux"`},
		"name twice in one seat": {PolecatPool{Seats: map[string]SeatConfig{
			"claude-sonnet": {Cap: 2, Names: []string{"nux", "furiosa", "nux"}},
		}}, `polecat_pool.seats.claude-sonnet.names: name "nux" is listed twice`},
		"empty name": {PolecatPool{Seats: map[string]SeatConfig{
			"claude-sonnet": {Cap: 1, Names: []string{"nux", ""}},
		}}, "polecat_pool.seats.claude-sonnet.names"},
		"blank name": {PolecatPool{Seats: map[string]SeatConfig{
			"claude-sonnet": {Cap: 1, Names: []string{"  "}},
		}}, "polecat_pool.seats.claude-sonnet.names"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ts := NewTownSettings()
			pool := tc.pool
			ts.PolecatPool = &pool
			known := func(a string) bool { return a != "nope" }
			_, err := Seats(ts, known)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error %q does not name %q", err, tc.key)
			}
		})
	}
}

func TestWindowBoundaries(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	w, err := ParseWindow("20:00-08:00", loc)
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 19, h, m, 0, 0, loc) }
	for _, tc := range []struct {
		h, m   int
		closed bool
	}{{7, 59, true}, {8, 0, false}, {19, 59, false}, {20, 0, true}, {0, 0, true}, {23, 59, true}} {
		if got := w.Closed(at(tc.h, tc.m)); got != tc.closed {
			t.Errorf("%02d:%02d closed = %v, want %v", tc.h, tc.m, got, tc.closed)
		}
	}
	if got := w.OpensAt(at(21, 0)); !got.Equal(time.Date(2026, 10, 20, 8, 0, 0, 0, loc)) {
		t.Errorf("OpensAt(21:00) = %v", got)
	}
	if got := w.OpensAt(at(9, 0)); !got.Equal(at(9, 0)) {
		t.Errorf("OpensAt(09:00) = %v, want now", got)
	}
}

// SeatWindow is the one place a seat's window is resolved: nil for a seat with
// none, and an error the router reads as shut for a window it cannot load.
func TestSeatWindow(t *testing.T) {
	t.Parallel()

	chicago := &PolecatPool{Timezone: "America/Chicago"}
	if w, err := SeatWindow(chicago, SeatConfig{Cap: 1}); w != nil || err != nil {
		t.Errorf("a seat with no window = %v, %v; want nil, nil", w, err)
	}
	if _, err := SeatWindow(&PolecatPool{}, SeatConfig{Closed: "20:00-08:00"}); err == nil || !strings.Contains(err.Error(), "polecat_pool.timezone") {
		t.Errorf("a window with no timezone must name polecat_pool.timezone: %v", err)
	}
	if _, err := SeatWindow(&PolecatPool{Timezone: "Mars/Olympus"}, SeatConfig{Closed: "20:00-08:00"}); err == nil {
		t.Error("a window in an unknown zone must fail")
	}
	if _, err := SeatWindow(chicago, SeatConfig{Closed: "bogus"}); err == nil {
		t.Error("a window that is not HH:MM-HH:MM must fail")
	}
	w, err := SeatWindow(chicago, SeatConfig{Closed: "20:00-08:00"})
	if err != nil {
		t.Fatalf("SeatWindow: %v", err)
	}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Closed(time.Date(2026, 10, 19, 21, 0, 0, 0, loc)) || w.Closed(time.Date(2026, 10, 19, 12, 0, 0, 0, loc)) {
		t.Errorf("the 20:00-08:00 window in Chicago reads wrong at 21:00 or 12:00")
	}
}

// A same-day window is closed between its bounds, and OpensAt returns the same
// day's end.
func TestWindowSameDay(t *testing.T) {
	t.Parallel()

	w, err := ParseWindow("09:00-17:00", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 19, h, m, 0, 0, time.UTC) }
	if w.Closed(at(8, 59)) || !w.Closed(at(9, 0)) || !w.Closed(at(16, 59)) || w.Closed(at(17, 0)) {
		t.Fatalf("09:00-17:00 bounds are wrong")
	}
	if got := w.OpensAt(at(10, 0)); !got.Equal(at(17, 0)) {
		t.Errorf("OpensAt(10:00) = %v, want 17:00", got)
	}
}

// A settings file with no polecat_pool, or a pool with no seats, has no seats
// and is not an error.
func TestSeatsAbsent(t *testing.T) {
	t.Parallel()

	for name, ts := range map[string]*TownSettings{
		"no pool":  NewTownSettings(),
		"no seats": {PolecatPool: &PolecatPool{MinSpawnGap: "4m"}},
		"nil town": nil,
	} {
		got, err := Seats(ts, func(string) bool { return true })
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: seats = %+v", name, got)
		}
	}
}

// The resolver reads a PATH and environment like the launcher's; a scripted
// host keeps this test from depending on where it runs (gt-nkedc).
func TestAgentResolverKnowsWhatTheLauncherKnows(t *testing.T) {
	t.Parallel()
	ts := NewTownSettings()
	ts.Agents = map[string]*RuntimeConfig{"my-agent": {Command: "my-agent"}}
	known := agentResolverIn(agentHost(nil), "", ts)
	for name, want := range map[string]bool{
		"my-agent":      true, // the town's own agents
		"claude-sonnet": true, // a cost-tier preset
		"claude-haiku":  true,
		"nope":          false,
	} {
		if got := known(name); got != want {
			t.Errorf("known(%q) = %v, want %v", name, got, want)
		}
	}
}

// A nil known callback accepts every agent name instead of panicking.
func TestSeatsNilKnown(t *testing.T) {
	t.Parallel()

	ts := NewTownSettings()
	ts.PolecatPool = &PolecatPool{Seats: map[string]SeatConfig{"anything": {Cap: 1}}}
	got, err := Seats(ts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Agent != "anything" {
		t.Fatalf("seats = %+v", got)
	}
}

// SeatsKnownTo resolves an agent the settings define and a built-in preset, and
// refuses any other name.
func TestSeatsKnownTo(t *testing.T) {
	t.Parallel()

	ts := NewTownSettings()
	ts.Agents = map[string]*RuntimeConfig{"my-agent": {Command: "claude"}}
	known := SeatsKnownTo(ts)
	if !known("my-agent") {
		t.Error("an agent defined in settings must resolve")
	}
	if !known("claude") {
		t.Error("a built-in preset must resolve")
	}
	if known("no-such-agent") {
		t.Error("an undefined name must not resolve")
	}
	if SeatsKnownTo(nil)("my-agent") {
		t.Error("nil settings define no agents")
	}
}

// A settings file decodes and yields its seats with the new keys, and decodes
// to no seats without them.
func TestSeatsFromSettingsFile(t *testing.T) {
	t.Parallel()

	decode := func(t *testing.T, body string) *TownSettings {
		t.Helper()
		var ts TownSettings
		if err := DecodeJSONFile("settings/config.json", []byte(body), &ts); err != nil {
			t.Fatal(err)
		}
		return &ts
	}

	t.Run("with the keys", func(t *testing.T) {
		t.Parallel()
		ts := decode(t, `{"type":"town-settings","version":1,"polecat_pool":{
			"timezone":"America/Chicago",
			"seats":{
				"deepseek-flash":{"cap":3,"closed":"20:00-08:00","names":["nux"]},
				"claude-sonnet":{"cap":2}
			}}}`)
		got, err := Seats(ts, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Agent != "claude-sonnet" || got[1].Agent != "deepseek-flash" {
			t.Fatalf("seats = %+v", got)
		}
		if got[1].Window == nil || got[1].Window.StartMin != 20*60 || len(got[1].Names) != 1 || got[1].Names[0] != "nux" {
			t.Fatalf("deepseek-flash seat = %+v", got[1])
		}
	})

	t.Run("without the keys", func(t *testing.T) {
		t.Parallel()
		for name, body := range map[string]string{
			"no pool":  `{"type":"town-settings","version":1}`,
			"no seats": `{"type":"town-settings","version":1,"polecat_pool":{"min_spawn_gap":"4m"}}`,
		} {
			got, err := Seats(decode(t, body), nil)
			if err != nil || len(got) != 0 {
				t.Errorf("%s: seats = %+v, err = %v", name, got, err)
			}
		}
	})
}

// Chicago leaves daylight time on 2026-11-01 and enters it on 2026-03-08, so a
// window's wall-clock bounds sit at different UTC offsets on either side. The
// window reads the wall clock, so it opens at 08:00 local whichever offset
// that is.
func TestWindowAcrossDST(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	w, err := ParseWindow("20:00-08:00", loc)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		from  time.Time // closed
		opens time.Time
	}{
		{
			name:  "fall back, evening before",
			from:  time.Date(2026, 10, 31, 21, 0, 0, 0, loc),
			opens: time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC), // 08:00 CST, UTC-6
		},
		{
			name:  "spring forward, evening before",
			from:  time.Date(2026, 3, 7, 21, 0, 0, 0, loc),
			opens: time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC), // 08:00 CDT, UTC-5
		},
	} {
		if !w.Closed(tc.from) {
			t.Errorf("%s: %v must be closed", tc.name, tc.from)
		}
		got := w.OpensAt(tc.from)
		if !got.Equal(tc.opens) {
			t.Errorf("%s: OpensAt = %v, want %v", tc.name, got.UTC(), tc.opens)
		}
		if w.Closed(got) {
			t.Errorf("%s: the window is still closed at the instant OpensAt returned", tc.name)
		}
	}

	// The repeated hour: 01:30 happens twice on 2026-11-01, once in each offset,
	// and both fall inside the closed period.
	first := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)  // 01:30 CDT
	second := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC) // 01:30 CST
	if !w.Closed(first) || !w.Closed(second) {
		t.Errorf("both 01:30s must be closed: %v %v", w.Closed(first), w.Closed(second))
	}
}

func TestUnrecordedAgent(t *testing.T) {
	t.Parallel()
	seat := SeatConfig{Cap: 1}
	cases := []struct {
		name string
		pool *PolecatPool
		want string
	}{
		{"no pool", nil, ""},
		{"no seats place it on none", &PolecatPool{MinSpawnGap: "4m"}, ""},
		{"one seat is where it sits", &PolecatPool{Seats: map[string]SeatConfig{"claude-haiku": seat}}, "claude-haiku"},
		{"several seats place it on none", &PolecatPool{Seats: map[string]SeatConfig{"claude-haiku": seat, "claude-sonnet": seat}}, UnknownSeatAgent},
	}
	for _, c := range cases {
		if got := c.pool.UnrecordedAgent(); got != c.want {
			t.Errorf("%s: UnrecordedAgent() = %q, want %q", c.name, got, c.want)
		}
	}
}

// A seat names its backend and, for openshell, the sandbox profile it runs in.
// Each rejection names the settings key the operator must fix.
func TestSeatsBackendAndSandbox(t *testing.T) {
	t.Parallel()

	prof := map[string]SandboxProfile{"polecat-default": {Image: "gastown-polecat:dev", Policy: "deploy/polecat/policy.yaml"}}
	cases := []struct {
		name        string
		seat        SeatConfig
		profiles    map[string]SandboxProfile
		wantErr     string
		wantBackend string
	}{
		{"default is tmux", SeatConfig{Cap: 1}, nil, "", BackendTmux},
		{"explicit tmux", SeatConfig{Cap: 1, Backend: "tmux"}, nil, "", BackendTmux},
		{"openshell with profile", SeatConfig{Cap: 1, Backend: "openshell", Sandbox: "polecat-default"}, prof, "", BackendOpenShell},
		{"openshell without profile", SeatConfig{Cap: 1, Backend: "openshell"}, prof, "polecat_pool.seats.deepseek-flash.sandbox: required when backend is openshell", ""},
		{"openshell unknown profile", SeatConfig{Cap: 1, Backend: "openshell", Sandbox: "nope"}, prof, `polecat_pool.seats.deepseek-flash.sandbox "nope": no such entry in sandboxes`, ""},
		{"unknown backend", SeatConfig{Cap: 1, Backend: "firecracker"}, nil, `polecat_pool.seats.deepseek-flash.backend "firecracker": want tmux or openshell`, ""},
		{"tmux seat naming a sandbox", SeatConfig{Cap: 1, Sandbox: "polecat-default"}, prof, "polecat_pool.seats.deepseek-flash.sandbox: only an openshell seat names a sandbox", ""},
		{"profile missing image", SeatConfig{Cap: 1, Backend: "openshell", Sandbox: "polecat-default"}, map[string]SandboxProfile{"polecat-default": {Policy: "p.yaml"}}, "sandboxes.polecat-default.image: required", ""},
		{"profile missing policy", SeatConfig{Cap: 1, Backend: "openshell", Sandbox: "polecat-default"}, map[string]SandboxProfile{"polecat-default": {Image: "i"}}, "sandboxes.polecat-default.policy: required", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ts := &TownSettings{PolecatPool: &PolecatPool{Seats: map[string]SeatConfig{"deepseek-flash": c.seat}}, Sandboxes: c.profiles}
			seats, err := Seats(ts, nil)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if seats[0].Backend != c.wantBackend || seats[0].Sandbox != c.seat.Sandbox {
				t.Fatalf("seat = %+v", seats[0])
			}
		})
	}
}

// The sandbox table and the seat's backend and sandbox keys decode from a
// settings file under strict decoding, and an unknown profile key is refused.
func TestSandboxesFromSettingsFile(t *testing.T) {
	t.Parallel()

	t.Run("with the keys", func(t *testing.T) {
		t.Parallel()
		var ts TownSettings
		err := DecodeJSONFile("settings/config.json", []byte(`{"type":"town-settings","version":1,
			"sandboxes":{"polecat-default":{"image":"gastown-polecat:dev","policy":"deploy/polecat/policy.yaml",
				"providers":["deepseek"],"cpu":"4","memory":"8Gi","cache_volume":"{rig}-cache"}},
			"polecat_pool":{"seats":{"deepseek-flash":{"cap":1,"backend":"openshell","sandbox":"polecat-default"}}}}`), &ts)
		if err != nil {
			t.Fatal(err)
		}
		p := ts.Sandboxes["polecat-default"]
		if p.Image != "gastown-polecat:dev" || p.Policy != "deploy/polecat/policy.yaml" || len(p.Providers) != 1 ||
			p.CPU != "4" || p.Memory != "8Gi" || p.CacheVolume != "{rig}-cache" {
			t.Fatalf("profile = %+v", p)
		}
		seats, err := Seats(&ts, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(seats) != 1 || seats[0].Backend != BackendOpenShell || seats[0].Sandbox != "polecat-default" {
			t.Fatalf("seats = %+v", seats)
		}
	})

	t.Run("unknown profile key", func(t *testing.T) {
		t.Parallel()
		var ts TownSettings
		err := DecodeJSONFile("settings/config.json", []byte(`{"type":"town-settings","version":1,
			"sandboxes":{"polecat-default":{"image":"i","policy":"p","nonsense":1}}}`), &ts)
		if err == nil || !strings.Contains(err.Error(), "nonsense") {
			t.Fatalf("err = %v, want an unknown-key error naming nonsense", err)
		}
	})
}

func TestSandboxProfileWithDefaults(t *testing.T) {
	t.Parallel()

	got := SandboxProfile{Image: "i", Policy: "p"}.WithDefaults()
	if got.Gateway != "openshell" || got.ControlPlaneHost != "host.docker.internal" {
		t.Fatalf("defaults = %q, %q", got.Gateway, got.ControlPlaneHost)
	}
	set := SandboxProfile{Gateway: "g", ControlPlaneHost: "h"}.WithDefaults()
	if set.Gateway != "g" || set.ControlPlaneHost != "h" {
		t.Fatalf("set fields overwritten: %q, %q", set.Gateway, set.ControlPlaneHost)
	}
}

func TestSandboxProfileValidate(t *testing.T) {
	t.Parallel()

	ok := SandboxProfile{Image: "i", Policy: "deploy/p.yaml"}
	cases := []struct {
		name    string
		mutate  func(*SandboxProfile)
		wantErr string
	}{
		{"minimal profile", func(*SandboxProfile) {}, ""},
		{"all fields set", func(p *SandboxProfile) { p.Providers = []string{"deepseek"}; p.CPU = "500m"; p.Memory = "8Gi" }, ""},
		{"fractional cpu", func(p *SandboxProfile) { p.CPU = "1.5" }, ""},
		{"missing image", func(p *SandboxProfile) { p.Image = "" }, "sandboxes.p.image: required"},
		{"missing policy", func(p *SandboxProfile) { p.Policy = "" }, "sandboxes.p.policy: required"},
		{"absolute policy", func(p *SandboxProfile) { p.Policy = "/etc/policy.yaml" }, `sandboxes.p.policy "/etc/policy.yaml": want a path relative to the rig repo root`},
		{"empty provider", func(p *SandboxProfile) { p.Providers = []string{"deepseek", " "} }, "sandboxes.p.providers[1]: empty provider name"},
		{"bad cpu", func(p *SandboxProfile) { p.CPU = "four" }, `sandboxes.p.cpu "four": want a k8s CPU quantity such as "4" or "500m"`},
		{"bad memory", func(p *SandboxProfile) { p.Memory = "8 GB" }, `sandboxes.p.memory "8 GB": want a k8s memory quantity such as "8Gi"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := ok
			c.mutate(&p)
			err := p.Validate("p")
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}
