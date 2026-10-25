package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
)

// The strip's seat count is the dispatcher's roster read over the same seat
// picture, so the page cannot disagree with the occupancy a sling is refused
// against: a 3-cap seat with three live polecats and two released parked seats
// reads 3/3, never 5/3 (gt-ly90n).
func TestTheHeaderSeatCountsAreTheDispatchersRoster(t *testing.T) {
	t.Parallel()

	const seat = "deepseek-flash-sandbox-k8s"
	root := poolTestTown(t, `{"type":"town-settings","version":1,`+
		`"agents":{"deepseek-flash-sandbox-k8s":{"command":"claude"}},`+
		`"polecat_pool":{"seats":{"deepseek-flash-sandbox-k8s":{"cap":3}}}}`, "")
	writeTestRigsConfig(t, root, "gastown")
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	seatSession := func(name string) poolSession {
		return poolSession{name: name, agent: seat, rig: "gastown", polecat: name}
	}
	sessions := []poolSession{
		seatSession("a"), seatSession("b"), seatSession("c"),
		// Two seats the town released: their polecats are parked, and a
		// released seat holds no cap.
		{name: "dead/gastown/d", agent: seat, rig: "gastown", polecat: "d", parkedStale: true},
		{name: "dead/gastown/e", agent: seat, rig: "gastown", polecat: "e", parkedStale: true},
	}
	got := dashboardSeatRoster(poolLiveForWith(root, func(*config.PolecatPool) ([]poolSession, error) {
		return sessions, nil
	}))
	if !got.Read {
		t.Fatal("the strip could not read the roster")
	}
	want := specRosterFrom(sessions, ts)
	if got.Live[seat] != want.Live[seat] || got.Live[seat] != 3 {
		t.Errorf("the strip's %s count = %d, the dispatcher's roster = %d, want 3",
			seat, got.Live[seat], want.Live[seat])
	}
	if got.Total != 3 {
		t.Errorf("the strip's seat total = %d, want the roster's 3, not the inventory's 5", got.Total)
	}
}

// The header strip names the model a reviewer runs, not the command that runs
// it: a backend is an argv, and its raw command line says nothing about which
// model an operator is looking at (gt-bj47s).
func TestOMModelName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		backend []string
		want    string
	}{
		{"the reviewer's own config", []string{"/Users/x/.local/bin/claude", "-p", "--model", "sonnet"}, "sonnet"},
		{"a model given with =", []string{"claude", "--model=sonnet", "-p"}, "sonnet"},
		{"a model given short", []string{"claude", "-m", "sonnet"}, "sonnet"},
		{"a vendor-prefixed model", []string{"claude", "--model", "deepseek-flash"}, "flash"},
		{"a wrapper that names no model", []string{"/x/claude-deepseek-flash", "-p"}, "flash"},
		{"a model with a release in it", []string{"claude", "--model", "claude-3-5-haiku-2026"}, "haiku"},
		// An unknown backend is not a family, and its own name is all the town
		// knows about it.
		{"an unknown backend", []string{"/usr/local/bin/aider", "--yes"}, "aider"},
		{"an unknown model", []string{"claude", "--model", "gpt-5"}, "gpt-5"},
		{"a flag with no value", []string{"/x/aider", "--model"}, "aider"},
		{"no backend at all", nil, ""},
	} {
		if got := omModelName(tc.backend); got != tc.want {
			t.Errorf("%s: omModelName(%q) = %q, want %q", tc.name, tc.backend, got, tc.want)
		}
	}
}

// modelWord is a word match, not a substring one: a backend called "flashback"
// is not a flash model.
func TestOMModelNameMatchesWholeWords(t *testing.T) {
	t.Parallel()

	if got := omModelName([]string{"/x/flashback"}); got != "flashback" {
		t.Errorf("a name that merely contains a family was read as one: %q", got)
	}
}

func writeModelsSettings(t *testing.T, townRoot, body string) {
	t.Helper()

	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeOMConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// modelsSeatAgents defines the agents the seat fixtures below name, so the
// seat loader's known-agent check passes.
const modelsSeatAgents = `"agents":{"deepseek-flash":{"command":"claude"},"claude-sonnet":{"command":"claude"},"claude-haiku":{"command":"claude"}}`

// The strip reads both configs whole on every poll, so what it shows is what
// the files say now (gt-bj47s).
func TestModelsReaderReadsTheTownAndTheReviewer(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeModelsSettings(t, town, `{`+modelsSeatAgents+`,"polecat_pool":{"seats":{"deepseek-flash":{"cap":4}}}}`)
	r := &modelsReader{townRoot: town, omPath: writeOMConfig(t, `{"backend":["/Users/x/.local/bin/claude","-p","--model","sonnet"],"depth":"standard"}`)}

	m := r.read()
	if m.Unrecorded != "deepseek-flash" {
		t.Errorf("unrecorded agent = %q, want the map's only seat", m.Unrecorded)
	}
	if m.SeatCap != 4 {
		t.Errorf("seat cap = %d, want the open seat's cap", m.SeatCap)
	}
	if m.OM != "sonnet" {
		t.Errorf("om model = %q, want sonnet", m.OM)
	}

	// A mode switch is a file the reader has not read yet: the next read is
	// the new model, with no cache in between. Two seats add up to the cap, and
	// leave a polecat that records no agent counted on neither.
	writeModelsSettings(t, town, `{`+modelsSeatAgents+`,"polecat_pool":{"seats":{"deepseek-flash":{"cap":2},"claude-sonnet":{"cap":1}}}}`)
	r.omPath = writeOMConfig(t, `{"backend":["/x/claude-deepseek-flash","-p"]}`)
	m = r.read()
	if m.Unrecorded != config.UnknownSeatAgent || m.SeatCap != 3 || m.OM != "flash" {
		t.Errorf("a switched mode did not show on the next read: %+v", m)
	}

	// max_total is the cap in force when the town names one.
	writeModelsSettings(t, town, `{`+modelsSeatAgents+`,"polecat_pool":{"max_total":5,"seats":{"deepseek-flash":{"cap":2},"claude-sonnet":{"cap":1}}}}`)
	if m = r.read(); m.SeatCap != 5 {
		t.Errorf("seat cap = %d, want max_total", m.SeatCap)
	}
}

// A config the town cannot read leaves its field empty — the page calls that
// unknown — and never stops the other config from reporting (gt-bj47s).
func TestModelsReaderLeavesAnUnreadableConfigUnknown(t *testing.T) {
	t.Parallel()

	om := writeOMConfig(t, `{"backend":["/x/claude-deepseek-flash","-p"]}`)

	// No settings file at all: a town that never set a model.
	m := (&modelsReader{townRoot: t.TempDir(), omPath: om}).read()
	if m.Unrecorded != "" || m.SeatCap != 0 || m.OM != "flash" {
		t.Errorf("a missing settings file took the reviewer with it: %+v", m)
	}

	// A settings file that does not parse: the town's model is not a guess.
	town := t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":`)
	m = (&modelsReader{townRoot: town, omPath: om}).read()
	if m.Unrecorded != "" || m.SeatCap != 0 {
		t.Errorf("an unparseable settings file was read as a model: %+v", m)
	}
	if m.OM != "flash" {
		t.Errorf("an unparseable settings file took the reviewer with it: %+v", m)
	}

	// No reviewer's config: the om model is unknown, the town's still reports.
	town = t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":{"crew":"deepseek-flash"}}`)
	m = (&modelsReader{townRoot: town, omPath: filepath.Join(t.TempDir(), "gone.json")}).read()
	if m.OM != "" {
		t.Errorf("a missing reviewer's config was read as a model: %+v", m)
	}
	if crew := panelRole(t, m.Roles, "crew"); crew.Model != "deepseek-flash" {
		t.Errorf("a missing reviewer's config took the crew model with it: %+v", crew)
	}
	// A town with no pool declares no cap, which is not a cap of zero.
	if m.SeatCap != 0 {
		t.Errorf("a town with no pool declared a cap: %+v", m)
	}
}

func writePanelFile(t *testing.T, townRoot, rel, body string) {
	t.Helper()

	path := filepath.Join(townRoot, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const panelTable = `{"version":1,"profiles":{
  "flash":{"preset":"deepseek-flash","om_backend":["~/.local/bin/claude-deepseek-flash","-p"]},
  "sonnet":{"preset":"claude-sonnet","om_backend":["~/.local/bin/claude","-p","--model","sonnet"]}},
  "default":"flash","roles":{"crew":"sonnet","om-review":"sonnet"}}`

func panelRole(t *testing.T, roles []dashboard.ModelRole, role string) dashboard.ModelRole {
	t.Helper()
	for _, r := range roles {
		if r.Role == role {
			return r
		}
	}
	t.Fatalf("no %s row in %+v", role, roles)
	return dashboard.ModelRole{}
}

// The panel lists every seat of polecat_pool.seats the way gt pool show does,
// in the loader's order: an open seat with its agent and cap, a seat nothing may
// fill as closed with the reason, by a cap of 0 or by its daily window
// (gt-lqa6p, gt-mukgk).
func TestModelsReaderListsEachSeat(t *testing.T) {
	t.Parallel()

	om := writeOMConfig(t, `{"backend":["claude","-p","--model","sonnet"]}`)
	town := t.TempDir()
	writeModelsSettings(t, town, `{`+modelsSeatAgents+`,"polecat_pool":{"timezone":"UTC","seats":{
	  "deepseek-flash":{"cap":3,"closed":"20:00-08:00"},
	  "claude-sonnet":{"cap":2},
	  "claude-haiku":{"cap":0}}}}`)

	night := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	m := (&modelsReader{townRoot: town, omPath: om, now: func() time.Time { return night }}).read()
	want := []dashboard.ModelSeat{
		{Agent: "claude-haiku", Cap: 0, State: "closed (cap 0)", Closed: true},
		{Agent: "claude-sonnet", Cap: 2, State: "open"},
		{Agent: "deepseek-flash", Cap: 3, State: "closed until 08:00", Closed: true},
	}
	if len(m.SeatRows) != len(want) {
		t.Fatalf("seat rows = %+v, want one per seat in name order", m.SeatRows)
	}
	for i, w := range want {
		got := m.SeatRows[i]
		if got.Agent != w.Agent || got.Cap != w.Cap || got.State != w.State || got.Closed != w.Closed || got.Unknown {
			t.Errorf("seat %d = %+v, want %+v", i, got, w)
		}
		if !strings.Contains(got.Key, "polecat_pool.seats."+w.Agent) || !strings.Contains(got.Key, "config.json") {
			t.Errorf("seat %s key = %q, want the key and the file it was read from", w.Agent, got.Key)
		}
	}
	// Only the seat open now adds to the cap the strip shows.
	if m.SeatCap != 2 {
		t.Errorf("seat cap at night = %d, want the open seat's 2", m.SeatCap)
	}

	// The window is read at the reader's clock: at noon the flash seat is open.
	noon := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	m = (&modelsReader{townRoot: town, omPath: om, now: func() time.Time { return noon }}).read()
	if flash := m.SeatRows[2]; flash.Closed || flash.State != "open" {
		t.Errorf("flash at noon = %+v, want open", flash)
	}
	if m.SeatCap != 5 {
		t.Errorf("seat cap at noon = %d, want 2 + 3", m.SeatCap)
	}

	// A town with no seat, or no polecat_pool section at all, is one closed row
	// that says so: no rows would read as a settings read that has not come back.
	for name, body := range map[string]string{
		"an empty pool":        `{"polecat_pool":{}}`,
		"no pool section":      `{"role_agents":{"crew":"deepseek-flash"}}`,
		"max_total and no map": `{"polecat_pool":{"max_total":3}}`,
	} {
		bare := t.TempDir()
		writeModelsSettings(t, bare, body)
		rows := (&modelsReader{townRoot: bare, omPath: om}).read().SeatRows
		if len(rows) != 1 || !rows[0].Closed || rows[0].Unknown || rows[0].State != "no seats" || rows[0].Agent != "" {
			t.Errorf("%s: seat rows = %+v, want one closed row reading no seats", name, rows)
		}
	}
}

// A seat map the loader rejects, or a settings file that does not parse, is one
// unknown row naming the settings file: neither a table of seats the dispatcher
// would refuse to run nor a closed one (gt-mukgk).
func TestModelsReaderCallsAnUnreadableSeatMapUnknown(t *testing.T) {
	t.Parallel()

	om := writeOMConfig(t, `{"backend":["claude","-p","--model","sonnet"]}`)
	for name, body := range map[string]string{
		"an agent nothing defines":   `{"polecat_pool":{"seats":{"no-such-agent":{"cap":2}}}}`,
		"a file that does not parse": `{"polecat_pool":{"seats":`,
	} {
		town := t.TempDir()
		writeModelsSettings(t, town, body)
		m := (&modelsReader{townRoot: town, omPath: om}).read()
		if len(m.SeatRows) != 1 || !m.SeatRows[0].Unknown || m.SeatRows[0].Closed || m.SeatRows[0].Agent != "" {
			t.Fatalf("%s: seat rows = %+v, want one unknown row", name, m.SeatRows)
		}
		if key := m.SeatRows[0].Key; !strings.Contains(key, "polecat_pool.seats") || !strings.Contains(key, "config.json") {
			t.Errorf("%s: key = %q, want the seat map and the file", name, key)
		}
		if m.SeatCap != 0 {
			t.Errorf("%s: seat cap = %d, want none from an unknown row", name, m.SeatCap)
		}
	}
}

// Each role names its model and the key it was read from, and a config the
// town cannot read leaves its role unknown rather than empty (gt-lqa6p). The
// polecat and the deep review have no row: they run on the seats the seat table
// lists (gt-mukgk).
func TestModelsReaderListsTheModelPerRole(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":{"crew":"claude-opus"}}`)
	om := writeOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)

	roles := (&modelsReader{townRoot: town, omPath: om}).read().Roles
	if len(roles) != 2 {
		t.Fatalf("roles = %+v, want the reviewer and the crew only", roles)
	}
	for role, want := range map[string]string{"om-review": "sonnet", "crew": "claude-opus"} {
		r := panelRole(t, roles, role)
		if r.Model != want {
			t.Errorf("%s model = %q, want %q", role, r.Model, want)
		}
		if r.Key == "" {
			t.Errorf("%s does not name the config key it was read from", role)
		}
	}

	// Nothing readable: every role is unknown, none is an empty value passed
	// off as a model, and no key goes unnamed.
	bad := t.TempDir()
	writeModelsSettings(t, bad, `{"role_agents":`)
	for _, r := range (&modelsReader{townRoot: bad, omPath: filepath.Join(bad, "nope.json")}).read().Roles {
		if r.Model != "" || r.Note == "" || r.Key == "" {
			t.Errorf("%s with nothing readable = %+v, want unknown with a note", r.Role, r)
		}
		if r.Drift != "" {
			t.Errorf("%s is called drifted by a table that was not read: %+v", r.Role, r)
		}
	}
}

// A role whose live setting differs from the town model table wears a drift
// mark naming the table's value, the check gt-models.sh show makes; a table
// nobody can read marks nothing (gt-lqa6p).
func TestModelsReaderMarksDriftFromTheModelTable(t *testing.T) {
	t.Parallel()

	om := writeOMConfig(t, `{"backend":["/x/claude-deepseek-flash","-p"]}`)
	town := t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":{"crew":"deepseek-flash"}}`)
	writePanelFile(t, town, "settings/models.json", panelTable)

	roles := (&modelsReader{townRoot: town, omPath: om}).read().Roles
	if r := panelRole(t, roles, "om-review"); r.Drift != "sonnet" {
		t.Errorf("om-review runs flash against a sonnet table and drift = %q", r.Drift)
	}
	if r := panelRole(t, roles, "crew"); r.Drift != "claude-sonnet" {
		t.Errorf("crew runs deepseek-flash against a claude-sonnet table and drift = %q", r.Drift)
	}

	// A crew that matches the table is not marked.
	writeModelsSettings(t, town, `{"role_agents":{"crew":"claude-sonnet"}}`)
	if r := panelRole(t, (&modelsReader{townRoot: town, omPath: om}).read().Roles, "crew"); r.Drift != "" {
		t.Errorf("crew matches the table and is marked: %+v", r)
	}

	// The table's "~/" path is the live absolute one: the same backend is not
	// drift.
	home, _ := os.UserHomeDir()
	same := writeOMConfig(t, `{"backend":["`+filepath.Join(home, ".local/bin/claude")+`","-p","--model","sonnet"]}`)
	if r := panelRole(t, (&modelsReader{townRoot: town, omPath: same}).read().Roles, "om-review"); r.Drift != "" {
		t.Errorf("a backend equal to the table's, once ~ is expanded, is marked: %+v", r)
	}

	// An unreadable table has no opinion.
	writePanelFile(t, town, "settings/models.json", `{"profiles":`)
	for _, r := range (&modelsReader{townRoot: town, omPath: om}).read().Roles {
		if r.Drift != "" {
			t.Errorf("%s drifted from a table that did not parse: %+v", r.Role, r)
		}
	}
}

// TestDashboardModelsCountsLandingsAndRejectionsPerAgent: the panel's last 24
// hours count each agent's landings off the landings file, where the landing
// worker records the agent the seat ran on, and its rejections off the review
// log, where it records the verdicts that refused a landing. A record outside
// the window, and one naming no agent (a crew landing), is counted for nobody
// (gt-mukgk.6).
func TestDashboardModelsCountsLandingsAndRejectionsPerAgent(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(town, "mayor", "rigs.json"),
		`{"version":1,"rigs":{"gastown":{"git_url":"u","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"gt"}}}}`)
	now := time.Date(2026, 10, 19, 18, 0, 0, 0, time.UTC)

	landings, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []land.LandingRecord{
		{BeadID: "gt-a", Rig: "gastown", Agent: "claude-sonnet", LandedAt: now.Add(-1 * time.Hour)},
		{BeadID: "gt-b", Rig: "gastown", Agent: "claude-sonnet", LandedAt: now.Add(-2 * time.Hour)},
		{BeadID: "gt-c", Rig: "gastown", Agent: "claude-haiku", LandedAt: now.Add(-3 * time.Hour)},
		// Outside the window: the panel counts a day, not the file.
		{BeadID: "gt-d", Rig: "gastown", Agent: "deepseek-flash", LandedAt: now.Add(-30 * time.Hour)},
		// A crew landing has no seat, so it names no agent.
		{BeadID: "gt-e", Rig: "gastown", LandedAt: now.Add(-1 * time.Hour)},
	} {
		if err := landings.Append(rec); err != nil {
			t.Fatal(err)
		}
	}

	reviews := land.TownReviewsFile(town)
	for _, rec := range []land.ReviewRecord{
		{BeadID: "gt-c", Rig: "gastown", Agent: "claude-haiku", Verdict: land.VerdictRequestChanges, At: now.Add(-1 * time.Hour)},
		{BeadID: "gt-f", Rig: "gastown", Agent: "claude-haiku", Verdict: land.VerdictApprove, Rejected: true, At: now.Add(-2 * time.Hour)},
		// An approval is not a rejection.
		{BeadID: "gt-a", Rig: "gastown", Agent: "claude-sonnet", Verdict: land.VerdictApprove, At: now.Add(-1 * time.Hour)},
		// Outside the window.
		{BeadID: "gt-g", Rig: "gastown", Agent: "claude-sonnet", Verdict: land.VerdictRequestChanges, At: now.Add(-30 * time.Hour)},
		// A crew review names no agent.
		{BeadID: "gt-h", Rig: "gastown", Verdict: land.VerdictRequestChanges, At: now.Add(-1 * time.Hour)},
	} {
		if err := reviews.Append(rec); err != nil {
			t.Fatal(err)
		}
	}

	r := &modelsReader{townRoot: town, omPath: filepath.Join(t.TempDir(), "missing.json"), now: func() time.Time { return now }}
	m := r.read()
	want := []dashboard.AgentActivity{{Agent: "claude-haiku", Landed: 1, Rejected: 2}, {Agent: "claude-sonnet", Landed: 2}}
	if len(m.Activity) != len(want) {
		t.Fatalf("activity = %+v, want %+v", m.Activity, want)
	}
	for i := range want {
		if m.Activity[i] != want[i] {
			t.Errorf("activity[%d] = %+v, want %+v", i, m.Activity[i], want[i])
		}
	}
}

// TestDashboardModelsNamesActivitySourcesItCouldNotRead: a landings file or
// review log that is there and cannot be read is a window the panel must not
// show as empty, so the count names each source it lost. A file that is not
// there, and a town that never wrote a rig registry, are an empty window and
// name nothing (gt-h3cwd).
func TestDashboardModelsNamesActivitySourcesItCouldNotRead(t *testing.T) {
	t.Parallel()

	// A directory where a log belongs opens and then fails to scan, standing
	// in for a file the panel cannot read; it is emphatically not missing.
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(town, "mayor", "rigs.json"),
		`{"version":1,"rigs":{"gastown":{"git_url":"u","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"gt"}}}}`)
	landings, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(landings.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(landings.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(land.TownReviewsFile(town).Path, 0o700); err != nil {
		t.Fatal(err)
	}

	m := (&modelsReader{townRoot: town, omPath: filepath.Join(t.TempDir(), "missing.json")}).read()
	want := []string{"gastown landings", "the review log"}
	if strings.Join(m.ActivityUnreadable, "|") != strings.Join(want, "|") {
		t.Errorf("activity unreadable = %v, want %v", m.ActivityUnreadable, want)
	}

	// A registry that is there and does not parse loses every rig's landings,
	// and says so.
	broken := t.TempDir()
	if err := os.MkdirAll(filepath.Join(broken, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(broken, "mayor", "rigs.json"), `{"rigs":`)
	if got := (&modelsReader{townRoot: broken, omPath: filepath.Join(t.TempDir(), "missing.json")}).read().ActivityUnreadable; len(got) != 1 || got[0] != "the rig registry" {
		t.Errorf("unreadable registry = %v, want the rig registry named", got)
	}

	// Nothing written yet is an empty window, not a read that failed.
	fresh := (&modelsReader{townRoot: t.TempDir(), omPath: filepath.Join(t.TempDir(), "missing.json")}).read()
	if len(fresh.ActivityUnreadable) != 0 {
		t.Errorf("fresh town unreadable = %v, want none", fresh.ActivityUnreadable)
	}
}

// TestDashboardModelsWithNoLandingsSaysNothing: a town that has never landed
// has no per-agent rows, and the panel says nothing rather than inventing a
// seat with zero landings (gt-mukgk.6).
func TestDashboardModelsWithNoLandingsSaysNothing(t *testing.T) {
	t.Parallel()

	r := &modelsReader{townRoot: t.TempDir(), omPath: filepath.Join(t.TempDir(), "missing.json")}
	if got := r.read().Activity; len(got) != 0 {
		t.Errorf("activity = %+v, want none for a town with no landings", got)
	}
}

// A roster read that failed is not a roster of zero: the strip must be told the
// read did not answer, so it says so instead of showing an empty seat. A read
// that succeeded passes its counts through (gt-ly90n).
func TestDashboardSeatRosterFailedReadIsNotARoster(t *testing.T) {
	t.Parallel()
	failed := dashboardSeatRoster(poolShowLive{})
	if failed.Read {
		t.Errorf("a failed roster read reports Read = true, want the read not answered")
	}
	if len(failed.Live) != 0 || failed.Total != 0 {
		t.Errorf("a failed roster read counted seats: %+v", failed)
	}
	answered := dashboardSeatRoster(poolShowLive{ok: true, live: map[string]int{"seat": 2}, total: 2})
	if !answered.Read || answered.Total != 2 || answered.Live["seat"] != 2 {
		t.Errorf("an answered roster read = %+v, want its counts passed through", answered)
	}
}
