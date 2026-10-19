> Status: historical (2026-10). Merged in: gt-mukgk. Not maintained.

# Seat map implementation plan

> **For agentic workers:** each task below is one bead, one polecat, one landing. Read the design first. Steps use checkbox (`- [ ]`) syntax.

**Goal:** One `polecat_pool.seats` map says which models run, how many of each and when, for any number of agents, read through one loader.

**Architecture:** A new `internal/config/seats.go` owns the seat map, its validation and the closed-window arithmetic. Every reader of the old seat keys moves onto `config.Seats`. The dispatcher core (`specdispatch.Budget.Seats`) already handles any number of seats; it gains a closed state. The old keys are removed last, with a load-time message and a `gt pool migrate` command.

**Tech stack:** Go, the town's strict JSON decoding (`config.DecodeJSONFile`), cobra commands in `internal/cmd`.

**Spec:** `docs/plans/2026-10-19-seat-map-design.md` (epic gt-mukgk).

## Global constraints

- Seat map key: `polecat_pool.seats`, an object keyed by agent name; each value `{"cap": <int>, "closed": "HH:MM-HH:MM", "names": [...]}`, `closed` and `names` optional.
- `polecat_pool.timezone`: an IANA name, required when any seat has `closed`.
- `polecat_pool.max_total` keeps its meaning: a cap across every seat, absent means none.
- Seats are returned sorted by agent name.
- Window: start inclusive, end exclusive, end before start wraps past midnight, daily, in `timezone`.
- An unlabelled bead draws uniformly among agents with a free, open seat. An `agent:<x>` bead takes only `x`, and waits when `x` is closed or full.
- Unit tests use no `time.Sleep`, `time.After` or `t.Setenv`; clocks and random sources are injected.
- Every task's gate is `make gate` plus the package target it names.

## File map

| File | Change | Task |
|---|---|---|
| `internal/config/seats.go` | new: `SeatConfig`, `Seat`, `Window`, `Seats()` | 1 |
| `internal/config/seats_test.go` | new | 1 |
| `internal/config/types.go` | add `Seats`, `Timezone` to `PolecatPool` (task 1); remove retired fields (task 7) | 1, 7 |
| `internal/specdispatch/dispatch.go` | `Seat.Closed`, `Seat.OpensAt`, closed handling in the choice and the reason text | 2 |
| `internal/cmd/spec.go` | `specBudgetFromConfig` reads `config.Seats` | 2 |
| `internal/daemon/spec_seats.go`, `patrol_scan_seatcap.go` | live counts per seat agent | 3 |
| `internal/cmd/pool.go`, `pool_seats.go`, `pool_mix.go` | `gt pool show`, `gt pool set`; `gt pool mix` removed | 4 |
| `internal/daemon/scheduled_slings_spec.go` | spec entries take no agent pin | 5 |
| `internal/land/land.go` | `LandingRecord.Agent` | 6 |
| `internal/cmd/dashboard_models.go` | landings and rejections per agent in the models panel | 6 |
| `internal/config/parse_error.go`, `internal/cmd/pool_migrate.go` | retired-key messages, `gt pool migrate` | 7 |
| `internal/polecat/namepool.go` | per-seat names and the agent binding | 8 |
| `docs/guides/spec-dispatcher.md` | rewritten seat section | 9 |

Dependencies: 1 → 2 → 3; 1 → 4; 2 → 5; 1 → 6; 1 → 8; 2, 3, 4, 5 → 7; 7 → 9. Tasks 1 and 7 both edit `types.go` and are chained through 7's dependencies.

---

### Task 1: The seat map type, its loader and closed windows

**Files:**
- Create: `internal/config/seats.go`, `internal/config/seats_test.go`
- Modify: `internal/config/types.go` (`PolecatPool`, add two fields only)

**Interfaces:**
- Produces:

```go
// internal/config/seats.go
package config

// SeatConfig is one entry of polecat_pool.seats.
type SeatConfig struct {
	Cap    int      `json:"cap"`
	Closed string   `json:"closed,omitempty"` // "HH:MM-HH:MM" in polecat_pool.timezone
	Names  []string `json:"names,omitempty"`
}

// Seat is a validated seat, as every reader sees it.
type Seat struct {
	Agent  string
	Cap    int
	Window *Window // nil: always open
	Names  []string
}

// Window is a daily closed period.
type Window struct {
	StartMin, EndMin int // minutes after local midnight
	Loc              *time.Location
}

// Seats returns the town's seats sorted by agent name. known reports whether
// an agent name resolves (the agents map or a built-in preset).
func Seats(ts *TownSettings, known func(agent string) bool) ([]Seat, error)

// ParseWindow parses "HH:MM-HH:MM" in loc.
func ParseWindow(s string, loc *time.Location) (*Window, error)

// Closed reports whether t falls inside the window.
func (w *Window) Closed(t time.Time) bool

// OpensAt returns the first instant at or after t when the window is open.
func (w *Window) OpensAt(t time.Time) time.Time
```

- In `PolecatPool` add:

```go
	// Seats is the seat map: agent name -> cap, window, names (gt-mukgk).
	Seats map[string]SeatConfig `json:"seats,omitempty"`
	// Timezone is the IANA zone the seats' closed windows are read in.
	Timezone string `json:"timezone,omitempty"`
```

- [ ] **Step 1: Write the failing tests**

```go
func TestSeatsSortedAndValidated(t *testing.T) {
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
}

func TestSeatsRejects(t *testing.T) {
	cases := map[string]PolecatPool{
		"unknown agent":      {Seats: map[string]SeatConfig{"nope": {Cap: 1}}},
		"unrecorded name":     {Seats: map[string]SeatConfig{"(unrecorded)": {Cap: 1}}},
		"negative cap":       {Seats: map[string]SeatConfig{"claude-sonnet": {Cap: -1}}},
		"bad window":         {Timezone: "America/Chicago", Seats: map[string]SeatConfig{"claude-sonnet": {Cap: 1, Closed: "8pm-8am"}}},
		"window, no zone":    {Seats: map[string]SeatConfig{"claude-sonnet": {Cap: 1, Closed: "20:00-08:00"}}},
		"bad zone":           {Timezone: "Mars/Olympus", Seats: map[string]SeatConfig{"claude-sonnet": {Cap: 1, Closed: "20:00-08:00"}}},
		"name in two seats":  {Seats: map[string]SeatConfig{"claude-sonnet": {Cap: 1, Names: []string{"nux"}}, "claude-haiku": {Cap: 1, Names: []string{"nux"}}}},
	}
	for name, pool := range cases {
		pool := pool
		t.Run(name, func(t *testing.T) {
			ts := NewTownSettings()
			ts.PolecatPool = &pool
			known := func(a string) bool { return a != "nope" && a != "(unrecorded)" }
			if _, err := Seats(ts, known); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestWindowBoundaries(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
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
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/config/ -run 'TestSeats|TestWindow' -count=1`
Expected: FAIL, `undefined: Seats`.

- [ ] **Step 3: Implement `seats.go`**

```go
func ParseWindow(s string, loc *time.Location) (*Window, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("window %q: want HH:MM-HH:MM", s)
	}
	start, err := parseClock(parts[0])
	if err != nil {
		return nil, fmt.Errorf("window %q: %w", s, err)
	}
	end, err := parseClock(parts[1])
	if err != nil {
		return nil, fmt.Errorf("window %q: %w", s, err)
	}
	if start == end {
		return nil, fmt.Errorf("window %q: start and end are the same", s)
	}
	return &Window{StartMin: start, EndMin: end, Loc: loc}, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (w *Window) minute(t time.Time) int {
	l := t.In(w.Loc)
	return l.Hour()*60 + l.Minute()
}

func (w *Window) Closed(t time.Time) bool {
	m := w.minute(t)
	if w.StartMin < w.EndMin {
		return m >= w.StartMin && m < w.EndMin
	}
	return m >= w.StartMin || m < w.EndMin
}

func (w *Window) OpensAt(t time.Time) time.Time {
	if !w.Closed(t) {
		return t
	}
	l := t.In(w.Loc)
	open := time.Date(l.Year(), l.Month(), l.Day(), w.EndMin/60, w.EndMin%60, 0, 0, w.Loc)
	if !open.After(l) {
		open = open.AddDate(0, 0, 1)
	}
	return open
}

func Seats(ts *TownSettings, known func(string) bool) ([]Seat, error) {
	if ts == nil || ts.PolecatPool == nil {
		return nil, nil
	}
	pool := ts.PolecatPool
	var loc *time.Location
	if pool.Timezone != "" {
		l, err := time.LoadLocation(pool.Timezone)
		if err != nil {
			return nil, fmt.Errorf("polecat_pool.timezone %q: %w", pool.Timezone, err)
		}
		loc = l
	}
	agents := make([]string, 0, len(pool.Seats))
	for a := range pool.Seats {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	owner := map[string]string{}
	out := make([]Seat, 0, len(agents))
	for _, a := range agents {
		c := pool.Seats[a]
		if !known(a) {
			return nil, fmt.Errorf("polecat_pool.seats.%s: no agent of that name in agents or the built-in presets", a)
		}
		if c.Cap < 0 {
			return nil, fmt.Errorf("polecat_pool.seats.%s.cap %d: must be 0 or more", a, c.Cap)
		}
		s := Seat{Agent: a, Cap: c.Cap, Names: c.Names}
		if c.Closed != "" {
			if loc == nil {
				return nil, fmt.Errorf("polecat_pool.seats.%s.closed needs polecat_pool.timezone", a)
			}
			w, err := ParseWindow(c.Closed, loc)
			if err != nil {
				return nil, fmt.Errorf("polecat_pool.seats.%s.closed: %w", a, err)
			}
			s.Window = w
		}
		for _, n := range c.Names {
			if prev, ok := owner[n]; ok {
				return nil, fmt.Errorf("polecat_pool.seats: name %q is in both %s and %s", n, prev, a)
			}
			owner[n] = a
		}
		out = append(out, s)
	}
	return out, nil
}
```

- [ ] **Step 4: Add the two `PolecatPool` fields** shown under Interfaces, beside `MaxTotal`.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/config/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit** — `feat(config): polecat_pool.seats map, loader and closed windows (gt-mukgk)`

**Acceptance:**
- [ ] `config.Seats` returns seats sorted by agent name
- [ ] each rejection in `TestSeatsRejects` returns an error naming the key
- [ ] `TestWindowBoundaries` passes at 07:59, 08:00, 19:59, 20:00, 00:00 and 23:59
- [ ] the old `PolecatPool` fields are unchanged, so today's settings file still loads

---

### Task 2: The dispatcher reads the seat map and honors closed seats

**Files:**
- Modify: `internal/specdispatch/dispatch.go` (`Seat`, `Free`, `reason`, the pinned-bead wait reason)
- Modify: `internal/cmd/spec.go` (`specBudgetFor`, `specBudgetFromSeats`, and the tick that builds the budget)
- Test: `internal/specdispatch/dispatch_test.go`, `internal/cmd/spec_test.go`

**Interfaces:**
- Consumes: `config.Seats`, `config.Seat`, `(*config.Window).Closed`, `(*config.Window).OpensAt` (Task 1).
- Produces: `specdispatch.Seat` gains

```go
	// Closed is true while the seat's window is shut: it counts its live
	// polecats but takes no new spawn. OpensAt is when it next opens.
	Closed  bool
	OpensAt time.Time
```

- [ ] **Step 1: Write the failing tests**

```go
// dispatch_test.go
func TestClosedSeatTakesNoSpawn(t *testing.T) {
	s := Seat{Agent: "deepseek-flash", Cap: 3, Closed: true}
	if s.Free() {
		t.Fatal("a closed seat must not be free")
	}
}

func TestPinnedBeadWaitsOnClosedSeat(t *testing.T) {
	opens := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	b := Budget{Seats: []Seat{
		{Agent: "claude-sonnet", Cap: 3},
		{Agent: "deepseek-flash", Cap: 3, Closed: true, OpensAt: opens},
	}, Rand: rand.New(rand.NewSource(1))}
	spec := Spec{Labels: []string{AgentLabelPrefix + "deepseek-flash"}}
	seat, why := b.Choose(spec)
	if seat != nil {
		t.Fatalf("chose %s; a pinned bead must wait", seat.Agent)
	}
	if !strings.Contains(why, "closed until 08:00") {
		t.Fatalf("reason = %q", why)
	}
}
```

Use the existing choice function's real name and signature where this test says `Choose`; read it in `dispatch.go` first, and keep its current return shape.

```go
// spec_test.go
func TestSpecBudgetFromSeatMap(t *testing.T) {
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{
		Timezone: "UTC",
		Seats: map[string]config.SeatConfig{
			"claude-sonnet":  {Cap: 3},
			"claude-haiku":   {Cap: 2},
			"deepseek-flash": {Cap: 3, Closed: "20:00-08:00"},
		},
	}
	now := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	b, err := specBudgetFromSeats(ts, now, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Seats) != 3 {
		t.Fatalf("seats = %+v", b.Seats)
	}
	var flash specdispatch.Seat
	for _, s := range b.Seats {
		if s.Agent == "deepseek-flash" {
			flash = s
		}
	}
	if !flash.Closed || flash.OpensAt.Hour() != 8 {
		t.Fatalf("flash = %+v", flash)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/specdispatch/ ./internal/cmd/ -run 'Closed|Pinned|SeatMap' -count=1`
Expected: FAIL on the missing fields and function.

- [ ] **Step 3: Implement**

In `dispatch.go`, make `Free` return false when `s.Closed`, and give the reason for a closed seat as `fmt.Sprintf("seat %s closed until %s", s.Agent, s.OpensAt.Format("15:04"))`. The pinned-bead path must return that reason, not fall through to another agent.

In `spec.go`, add `specBudgetFromSeats(ts, now, known) (specdispatch.Budget, error)` and have `specBudgetFor` choose it when `ts.PolecatPool.Seats` is non-empty, keeping the old path for a file without `seats` until Task 7. `specBudgetFor` returns the error too, and the tick stops on it: `gt spec dispatch` exits non-zero with `reading the seat map: <loader message>` and the daemon logs `spec_dispatch: tick failed: reading the seat map: <loader message>`. An empty budget returned in silence would read as a town with no work and hide the broken config that caused it.

```go
func specBudgetFromSeats(ts *config.TownSettings, now time.Time, known func(string) bool) (specdispatch.Budget, error) {
	var b specdispatch.Budget
	pool := ts.PolecatPool
	b.MinSpawnGap = pool.MinSpawnGapD()
	b.MaxTotal = pool.GetMaxTotal()
	b.Now = now
	seats, err := config.Seats(ts, known)
	if err != nil {
		return b, fmt.Errorf("reading the seat map: %w", err)
	}
	for _, s := range seats {
		seat := specdispatch.Seat{Agent: s.Agent, Cap: s.Cap}
		if s.Window != nil && s.Window.Closed(now) {
			seat.Closed = true
			seat.OpensAt = s.Window.OpensAt(now)
		}
		b.Seats = append(b.Seats, seat)
	}
	return b, nil
}
```

`known` is the existing agent resolver: an agent in `ts.Agents` or a name `config` resolves through its presets. Use the resolver the loader already has (`loader.go`, the function that ends in `costTierPresetByName`); do not write a second one.

At daemon start (`internal/daemon/daemon.go`, where the spec dispatch ticker starts), call `config.Seats` once. On an error, log it and exit non-zero, so the daemon never runs on a partial seat list. Add a test that a settings file with `seats: {"nope": {"cap": 1}}` makes start fail with the loader's message.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/specdispatch/ ./internal/cmd/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit** — `feat(dispatch): seats from the seat map; closed seats take no spawn (gt-mukgk)`

**Acceptance:**
- [ ] a closed seat is listed, counts live polecats, and is never chosen
- [ ] a bead pinned to a closed seat waits, and the tick says `closed until HH:MM`
- [ ] three agents in the map give three seats in the budget
- [ ] a settings file with no `seats` key dispatches exactly as before
- [ ] the daemon refuses to start on a seat map the loader rejects
- [ ] a tick that meets a rejected seat map fails with `reading the seat map: <loader message>` and dispatches nothing, rather than reporting an empty budget

---

### Task 3: Live counts per seat agent

**Files:**
- Modify: `internal/daemon/spec_seats.go`, `internal/daemon/patrol_scan_seatcap.go`
- Test: their `_test.go` files

**Interfaces:**
- Consumes: `config.Seats` (Task 1), `specdispatch.Budget.SetLive(map[string]int)`.
- Produces: the roster's per-agent map keys are the agent each seat was spawned on (the agent bead's `Agent` field), so `SetLive` fills every seat in the map.
- Produces: `specRoster.Unrecorded []string` holds the names of polecats that record no agent on a map with several seats. It is not a map key. The tick sets `Budget.LiveTotal` to the sum of `Live` plus `len(Unrecorded)`, because `SetLive` sums only the per-agent map.

- [ ] **Step 1: Write the failing test.** Build a roster with three live polecats whose agent beads record `claude-sonnet`, `claude-haiku` and `deepseek-flash`, and a sessionless one whose bead records `claude-haiku`. Assert the per-agent map is `{"claude-sonnet": 1, "claude-haiku": 2, "deepseek-flash": 1}`, not a count under the role default. A fourth polecat recording no agent, on a map with several seats, fills no seat's count and is returned in `Unrecorded`. Assert `LiveTotal` is 5: the four in `Live` plus that one.

- [ ] **Step 2: Run it and confirm it fails**, with `go test ./internal/daemon/ -run SeatLive -count=1`.

- [ ] **Step 3: Implement.** Key the count on the agent bead's recorded agent. Where no agent is recorded, use the seat map's single agent if the map has exactly one seat; with several seats, add the polecat's name to `Unrecorded` and leave `Live` alone. The tick computes `LiveTotal` as the sum of `Live` plus `len(Unrecorded)`, so `max_total` sees the polecat. The config package defines no bucket key (Task 1 has none), so neither the loader nor the seat map ever sees the name `(unrecorded)`. The tick reports the bucket once per tick, naming the polecats: `roster: N polecat(s) record no agent and count toward max_total on no seat: <names>`. `gt pool show` lists seats only, so that notice is where the bucket appears.

- [ ] **Step 4: Run the tests**, `go test ./internal/daemon/ -count=1`, expect PASS.

- [ ] **Step 5: Commit** — `fix(daemon): count live polecats by the agent their seat was spawned on (gt-mukgk)`

**Acceptance:**
- [ ] three agents' live polecats are each counted on their own seat
- [ ] a sessionless seat counts under its recorded agent (the gt-2q283 case)
- [ ] a polecat with no recorded agent fills no seat, counts toward `max_total`, and is named in the tick's notice

---

### Task 4: `gt pool show` and `gt pool set` for any number of agents

**Files:**
- Modify: `internal/cmd/pool.go`, `internal/cmd/pool_seats.go`
- Modify: `internal/cmd/pool_mix.go` (the command prints the replacement and exits 2)
- Test: `internal/cmd/pool_test.go`

**Interfaces:**
- Consumes: `config.Seats` (Task 1).
- Produces: `gt pool set <agent>=<cap>... [--total N] [--remove <agent>]` writing only `polecat_pool.seats` and `max_total`.

- [ ] **Step 1: Write the failing tests**, covering:
  - `gt pool set claude-haiku=2` on a map with sonnet and flash leaves those two unchanged and adds haiku;
  - `--remove deepseek-flash` drops that entry;
  - an unknown agent writes nothing and exits non-zero;
  - `gt pool show` at 21:00 Chicago prints `deepseek-flash  0/3  closed until 08:00`, one line per agent and a total line.
- [ ] **Step 2: Run them and confirm they fail**, `go test ./internal/cmd/ -run 'PoolSet|PoolShow' -count=1`.
- [ ] **Step 3: Implement.** `set` validates the whole resulting map with `config.Seats` before writing, and writes through the existing settings writer. `show` reads the map and the roster. `gt pool mix` prints the equivalent `gt pool set` line and exits 2.
- [ ] **Step 4: Run the tests**, `go test ./internal/cmd/ -count=1`, expect PASS.
- [ ] **Step 5: Commit** — `feat(pool): gt pool set and show over the seat map; retire gt pool mix (gt-mukgk)`

**Acceptance:**
- [ ] `gt pool set` changes only the named agents
- [ ] an invalid result writes nothing
- [ ] `gt pool show` prints cap, live, open or closed and next opening per agent, plus the total
- [ ] `gt pool mix` names the `gt pool set` replacement and exits 2

---

### Task 5: Deep reviews take a seat like any bead

**Files:**
- Modify: `internal/daemon/scheduled_slings_spec.go`
- Test: `internal/daemon/scheduled_slings_spec_test.go`

**Interfaces:**
- Consumes: Task 2's dispatcher.
- Produces: a `dispatch: "spec"` entry writes no `agent:` label. An entry that still sets `agent` is refused at validation with `scheduled_slings[<name>]: a spec entry takes no agent; it gets a seat from polecat_pool.seats`.

- [ ] **Step 1: Write the failing tests:** a spec entry with no agent creates a bead with no `agent:` label; a spec entry with an agent fails validation with the message above.
- [ ] **Step 2: Run them and confirm they fail**, `go test ./internal/daemon/ -run ScheduledSpec -count=1`.
- [ ] **Step 3: Implement.** Remove the label append at the current line 42, and replace the agent check near line 61 with the refusal.
- [ ] **Step 4: Run the tests**, `go test ./internal/daemon/ -count=1`, expect PASS.
- [ ] **Step 5: Commit** — `feat(daemon): spec-dispatched scheduled entries take a seat, not an agent pin (gt-mukgk)`

**Acceptance:**
- [ ] a deep-review bead carries no `agent:` label
- [ ] a spec entry that names an agent is refused with the message above

---

### Task 6: Each landing records its agent

**Files:**
- Modify: `internal/land/land.go` (`LandingRecord`, `record`, and where `Work` is built)
- Modify: `internal/cmd/dashboard_models.go` (`modelsReader.agentActivity` and the panel that renders its rows)
- Test: `internal/land/land_test.go`, `internal/cmd/dashboard_models_test.go`

**Interfaces:**
- Produces: `LandingRecord.Agent string` (`json:"agent,omitempty"`), and the bead's LANDING RECORD block gains a line `agent: <x>`.
- Produces: `dashboard.AgentActivity{Agent, Landed, Rejected}` rows, one per agent with activity in the last 24 hours, rendered in the models panel.

- [ ] **Step 1: Write the failing test:** a landing whose polecat's agent bead records `claude-haiku` writes `"agent":"claude-haiku"` to the landings file and `agent: claude-haiku` to the bead note.
- [ ] **Step 2: Run it and confirm it fails**, `go test ./internal/land/ -run LandingRecordAgent -count=1`.
- [ ] **Step 3: Implement.** Carry the agent in `Work` from the polecat's agent bead, and copy it into the record. A crew landing has no seat, so it writes no agent line.
- [ ] **Step 4: Run the tests**, `go test ./internal/land/ -count=1`, expect PASS.
- [ ] **Step 5: Write the failing dashboard test:** with two rigs' landings files and the town review log inside 24 hours, `agentActivity` returns one sorted row per agent with its landing and rejection counts; a record naming no agent credits nobody; a log with an entry outside the window contributes nothing.
- [ ] **Step 6: Run it and confirm it fails**, `go test ./internal/cmd/ -run ModelsAgentActivity -count=1`.
- [ ] **Step 7: Implement.** `agentActivity` reads `land.RigLandingsFile` for `hq` and each known rig and `land.TownReviewsFile`, keeps records at or after the reader's injected `now` minus `modelsActivityWindow` (24h), counts `Landed` once per landing record under its `Agent`, and `Rejected` once per review record under its `Agent` where the record's `Rejected` field is true or its `Verdict` is `request_changes` (the two conditions make one rejection, not two; a record with no `Agent` credits no row), sorts the agents, and returns the union of both maps. A missing file is an empty window; one that cannot be read is returned by name so the panel can say which. The panel renders the rows beneath the seat rows.
- [ ] **Step 8: Run the tests**, `go test ./internal/land/ ./internal/cmd/ -count=1`, expect PASS.
- [ ] **Step 9: Commit** — `feat(land): record the agent each landing ran on (gt-mukgk)`

**Acceptance:**
- [ ] a polecat landing records its agent in the landings file and the bead note
- [ ] a crew landing records no agent
- [ ] the dashboard models panel (`internal/cmd/dashboard_models.go`) shows landings per agent from the landings files and rejections per agent from the town review log, over the last 24 hours
- [ ] a record that names no agent adds nothing to any agent's row

---

### Task 7: Retire the old keys, and `gt pool migrate`

**Files:**
- Modify: `internal/config/types.go` (remove `Agent`, `MaxSeats`, `ProAgent`, `ProMax`, `ProLabel` from `PolecatPool`), `internal/config/daemon_config.go` (remove `HookedAgent`, `MaxHooked`), the `role_agents` handling for `polecat`
- Modify: `internal/config/parse_error.go` (replacement messages)
- Create: `internal/cmd/pool_migrate.go`, `internal/cmd/pool_migrate_test.go`, golden files under `internal/cmd/testdata/pool_migrate/`
- Modify: every reader still using a removed field (`git grep` them; the file map lists them)

**Interfaces:**
- Produces: `gt pool migrate [--dry-run]`.

- [ ] **Step 1: Write the failing tests.**
  - For each retired key, a settings file carrying it fails to load, with a message containing `was replaced by` and `gt pool migrate`.
  - Golden files: `two-seat.json` (pool seat + second seat) migrates to a `seats` map with both caps; `pro-seat.json` (pool and pro seats on different agents) becomes one entry per agent; `same-agent.json` (pool and pro seats on one agent) becomes one entry whose cap is the sum of the two caps; `deep-review-pins.json` strips `agent` from spec entries; `seat-conflict.json` (a legacy pool agent that `seats` already names) fails with nothing written and that agent named. Running migrate on its own output changes nothing.
- [ ] **Step 2: Run them and confirm they fail**, `go test ./internal/config/ ./internal/cmd/ -run 'Retired|PoolMigrate' -count=1`.
- [ ] **Step 3: Implement the messages.** In `parse_error.go`, when a `ParseError`'s `Keys` holds one of these paths, append the replacement:

```go
var retiredSeatKeys = map[string]string{
	"polecat_pool.agent":                   "polecat_pool.seats",
	"polecat_pool.max_seats":               "polecat_pool.seats.<agent>.cap",
	"polecat_pool.pro_agent":               "polecat_pool.seats",
	"polecat_pool.pro_max":                 "polecat_pool.seats.<agent>.cap",
	"polecat_pool.pro_label":               "an agent:<name> label on the bead",
	"daemon.patrols.spec_dispatch.hooked_agent": "polecat_pool.seats",
	"daemon.patrols.spec_dispatch.max_hooked":   "polecat_pool.seats.<agent>.cap",
	"role_agents.polecat":                  "polecat_pool.seats",
}
// message: "<key> was replaced by <replacement>: run gt pool migrate"
```

- [ ] **Step 4: Implement `gt pool migrate`.** It reads the file as raw JSON (a `map[string]any`), so the strict decoder's refusal does not stop it. It:
  1. adds a cap to `seats[<agent>].cap` for each legacy seat that was in use, mirroring the order `specBudgetFromConfig` read them:
     - the pool seat: `polecat_pool.agent`, capped at `max_seats`, or `defaultSpecSeatCap` when `max_seats` is unset or not above zero;
     - the pro seat: `pro_agent`, capped at `pro_max`, only when `pro_agent` is set and `pro_max` is above zero;
     - the second seat: `spec_dispatch.hooked_agent` (or `defaultSpecHookedAgent` when empty), capped at `max_hooked`, only when `max_hooked` is above zero.

     Two legacy keys that name one agent add their caps, since they were independent pools and the merged seat must hold what both held. `pro_label` has no seat equivalent and is dropped; the bead's `agent:<name>` label replaces it. The collision check reads `seats` as the file has it before any legacy key is applied. A legacy key naming an agent already in that map stops the migration with nothing written and a message naming that agent, because a sum there would silently change a cap the operator set on the new key. Legacy keys never collide with each other (they only sum), so the result does not depend on the order they are read;
  2. removes the retired keys, and `agent` from `scheduled_slings` entries whose `dispatch` is `spec`;
  3. backs up the file as `config.json.bak-<UTC timestamp>-pool-migrate`, then writes the result atomically;
  4. prints each change, or "nothing to migrate".

  `--dry-run` prints and writes nothing.
- [ ] **Step 5: Remove the old fields and move the remaining readers** onto `config.Seats`. `go build ./...` must pass with no reference to a removed field.
- [ ] **Step 6: Run the tests**, `go test ./internal/config/ ./internal/cmd/ ./internal/daemon/ -count=1`, expect PASS.
- [ ] **Step 7: Commit** — `feat(config): retire the two-seat keys; gt pool migrate (gt-mukgk)`

**Acceptance:**
- [ ] every retired key fails to load with `was replaced by` and `gt pool migrate`
- [ ] the four golden migrations match, and a second run changes nothing
- [ ] the pro seat migrates to a seat of its own; two legacy keys on one agent merge into one seat whose cap is their sum
- [ ] a legacy key naming an agent already in `seats` stops the migration with nothing written, and the message names that agent
- [ ] `--dry-run` writes nothing
- [ ] no reader references a removed field (`go build ./...`)

---

### Task 8: Seat names and the agent binding

**Files:**
- Modify: `internal/polecat/namepool.go`, `internal/polecat/namepool_test.go`
- Reference: `polecat/rictus/gt-m9dty+mvfi1zkp` at d7b85ed2 (the binding, built under gt-m9dty); rebase that work onto main and adapt it, do not write a second binding.

**Interfaces:**
- Consumes: `config.Seat.Names` (Task 1).
- Produces: a name allocated for a seat is bound to that seat's agent in the pool state and reused only for it.

- [ ] **Step 1: Write the failing tests:** a seat with `names: ["kestrel"]` allocates `kestrel` for its agent; `kestrel` is never allocated for another agent; a seat with no list draws from the themed default; a name bound before this change keeps its binding.
- [ ] **Step 2: Run them and confirm they fail**, `go test ./internal/polecat/ -run NamePool -count=1`.
- [ ] **Step 3: Implement**, starting from d7b85ed2's binding.
- [ ] **Step 4: Run the tests**, `go test ./internal/polecat/ -count=1`, expect PASS.
- [ ] **Step 5: Commit** — `feat(polecat): seat names bound to their agent (gt-mukgk, from gt-m9dty)`

**Acceptance:**
- [ ] a seat's own names are allocated only to its agent
- [ ] a bound name is never reused for another agent
- [ ] seats with no list use the themed default
- [ ] existing bindings survive

---

### Task 9: The spec-dispatcher guide

**Files:**
- Modify: `docs/guides/spec-dispatcher.md`

- [ ] **Step 1:** Replace the seat section with the seat map: the config shape, uniform choice, pins, closed windows, `gt pool show`/`set`/`migrate`. Remove every mention of the pool seat, second seat, pro seat, `hooked_agent` and `gt pool mix`.
- [ ] **Step 2:** `bash scripts/docs-lint.sh` passes.
- [ ] **Step 3: Commit** — `docs: spec dispatcher seats are the seat map (gt-mukgk)`

**Acceptance:**
- [ ] the guide describes only the seat map
- [ ] `git grep -n "hooked_agent\|pool mix\|pro seat" docs/guides/spec-dispatcher.md` finds nothing
- [ ] docs lint passes

---

## Operator steps (not polecat tasks)

1. After Task 7 lands, on a quiet town: `gt pool migrate --dry-run`, then `gt pool migrate`, then `launchctl kickstart -k gui/501/com.gastown.daemon`, then `gt pool show` and the first `spec_dispatch: tick` line.
2. Set the experiment's seats: `gt pool set claude-sonnet=3 claude-haiku=3 deepseek-flash=3`, and `closed: "20:00-08:00"` on deepseek-flash with `timezone: America/Chicago`.
3. Update the `/gastown` handbook and `~/.claude/docs/gastown-operations.md`: drop `gt-models.sh` and `gt pool mix`, keep the approval rule as policy. Drop the polecat and deep-review roles from `settings/models.json`. om's reviewer model stays in `~/.config/om/config.json`.
4. Teach `~/.claude/tools/gt-kpi.py` to group landings by the record's `agent` field.
5. Close claude-xev; close gt-xarrl and gt-m9dty against the tasks that took them in.
