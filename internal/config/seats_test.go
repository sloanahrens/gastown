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
		"no seats": {PolecatPool: &PolecatPool{MaxSeats: 2}},
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
			"no seats": `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-sonnet","max_seats":2}}`,
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
