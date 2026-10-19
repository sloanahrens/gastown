package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
)

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

// The strip reads both configs whole on every poll, so what it shows is what
// the files say now (gt-bj47s).
func TestModelsReaderReadsTheTownAndTheReviewer(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":{"polecat":"deepseek-flash"},"polecat_pool":{"agent":"deepseek-flash","max_seats":4}}`)
	r := &modelsReader{townRoot: town, omPath: writeOMConfig(t, `{"backend":["/Users/x/.local/bin/claude","-p","--model","sonnet"],"depth":"standard"}`)}

	m := r.read()
	if m.Polecat != "deepseek-flash" {
		t.Errorf("polecat model = %q, want deepseek-flash", m.Polecat)
	}
	if m.SeatCap != 4 {
		t.Errorf("seat cap = %d, want the pool's max_seats", m.SeatCap)
	}
	if m.OM != "sonnet" {
		t.Errorf("om model = %q, want sonnet", m.OM)
	}

	// A mode switch is a file the reader has not read yet: the next read is
	// the new model, with no cache in between.
	writeModelsSettings(t, town, `{"role_agents":{"polecat":"claude-sonnet"},"polecat_pool":{"agent":"deepseek-flash","max_seats":2}}`)
	r.omPath = writeOMConfig(t, `{"backend":["/x/claude-deepseek-flash","-p"]}`)
	m = r.read()
	if m.Polecat != "claude-sonnet" || m.SeatCap != 2 || m.OM != "flash" {
		t.Errorf("a switched mode did not show on the next read: %+v", m)
	}
}

// A config the town cannot read leaves its field empty — the page calls that
// unknown — and never stops the other config from reporting (gt-bj47s).
func TestModelsReaderLeavesAnUnreadableConfigUnknown(t *testing.T) {
	t.Parallel()

	om := writeOMConfig(t, `{"backend":["/x/claude-deepseek-flash","-p"]}`)

	// No settings file at all: a town that never set a model.
	m := (&modelsReader{townRoot: t.TempDir(), omPath: om}).read()
	if m.Polecat != "" || m.SeatCap != 0 || m.OM != "flash" {
		t.Errorf("a missing settings file took the reviewer with it: %+v", m)
	}

	// A settings file that does not parse: the town's model is not a guess.
	town := t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":`)
	m = (&modelsReader{townRoot: town, omPath: om}).read()
	if m.Polecat != "" || m.SeatCap != 0 {
		t.Errorf("an unparseable settings file was read as a model: %+v", m)
	}
	if m.OM != "flash" {
		t.Errorf("an unparseable settings file took the reviewer with it: %+v", m)
	}

	// No reviewer's config: the om model is unknown, the town's still reports.
	town = t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":{"polecat":"deepseek-flash"}}`)
	m = (&modelsReader{townRoot: town, omPath: filepath.Join(t.TempDir(), "gone.json")}).read()
	if m.OM != "" {
		t.Errorf("a missing reviewer's config was read as a model: %+v", m)
	}
	if m.Polecat != "deepseek-flash" {
		t.Errorf("a missing reviewer's config took the polecat model with it: %+v", m)
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

const panelDaemon = `{"type":"daemon-patrol-config","version":1,"patrols":{
  "spec_dispatch":{"enabled":true,"hooked_agent":"claude-sonnet","max_hooked":2},
  "scheduled_slings":{"enabled":true,"entries":[{"name":"deep-review","rig":"gastown","formula":"mol-deep-review","agent":"claude-sonnet","interval":"24h"}]}}}`

const panelTable = `{"version":1,"profiles":{
  "flash":{"preset":"deepseek-flash","om_backend":["~/.local/bin/claude-deepseek-flash","-p"]},
  "sonnet":{"preset":"claude-sonnet","om_backend":["~/.local/bin/claude","-p","--model","sonnet"]}},
  "default":"flash","roles":{"polecat":"sonnet","om-review":"sonnet","deep-review":"sonnet"}}`

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

// The panel lists every polecat seat the way gt pool show does: an open seat
// with its agent and cap, a seat nothing may fill as closed (gt-lqa6p). The
// reserved pro seat is retired, so the table is the pool seat and the second
// seat, and a settings file still carrying pro_* keys opens no third row.
func TestModelsReaderListsEachSeat(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeModelsSettings(t, town, `{"polecat_pool":{"agent":"deepseek-flash","max_seats":3,"pro_agent":"deepseek-reasoner","pro_max":2}}`)
	writePanelFile(t, town, "mayor/daemon.json", panelDaemon)
	om := writeOMConfig(t, `{"backend":["claude","-p","--model","sonnet"]}`)

	rows := (&modelsReader{townRoot: town, omPath: om}).read().SeatRows
	if len(rows) != 2 {
		t.Fatalf("seat rows = %+v, want the pool seat and the second seat: the pro seat is retired", rows)
	}
	pool, second := rows[0], rows[1]
	if pool.Seat != "pool" || pool.Agent != "deepseek-flash" || pool.Cap != 3 || pool.Closed || pool.CapDefault {
		t.Errorf("pool seat = %+v", pool)
	}
	if second.Seat != "second" || second.Agent != "claude-sonnet" || second.Cap != 2 || second.Closed {
		t.Errorf("second seat = %+v", second)
	}
	for _, s := range rows {
		if s.Key == "" {
			t.Errorf("seat %q does not name the config it was read from", s.Seat)
		}
	}
	if !strings.Contains(pool.Key, "polecat_pool.agent") || !strings.Contains(pool.Key, "max_seats") {
		t.Errorf("the pool seat's key = %q, want the keys it was read from", pool.Key)
	}

	// A pool with no max_seats is the dispatcher's default cap, marked as such.
	writeModelsSettings(t, town, `{"polecat_pool":{"agent":"deepseek-flash"}}`)
	if pool := (&modelsReader{townRoot: town, omPath: om}).read().SeatRows[0]; pool.Cap != defaultSpecSeatCap || !pool.CapDefault {
		t.Errorf("an unset max_seats = %+v, want the default cap marked", pool)
	}

	// No pool agent and no max_hooked: the pool seat and the second seat are
	// closed, and no pro row is invented beside them.
	closed := t.TempDir()
	writeModelsSettings(t, closed, `{"polecat_pool":{}}`)
	writePanelFile(t, closed, "mayor/daemon.json", `{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true}}}`)
	rows = (&modelsReader{townRoot: closed, omPath: om}).read().SeatRows
	if len(rows) != 2 {
		t.Fatalf("seat rows = %+v, want the pool seat and the second seat", rows)
	}
	for _, s := range rows {
		if !s.Closed || s.Agent != "" || s.Cap != 0 {
			t.Errorf("seat %q = %+v, want closed", s.Seat, s)
		}
	}

	// A town with no polecat_pool section at all has a closed pool seat.
	noPool := t.TempDir()
	writeModelsSettings(t, noPool, `{"role_agents":{"polecat":"deepseek-flash"}}`)
	writePanelFile(t, noPool, "mayor/daemon.json", panelDaemon)
	if pool := (&modelsReader{townRoot: noPool, omPath: om}).read().SeatRows[0]; !pool.Closed || pool.Agent != "" {
		t.Errorf("no polecat_pool section = %+v, want a closed pool seat", pool)
	}

	// A patrol config that does not parse is not a closed seat.
	broken := t.TempDir()
	writeModelsSettings(t, broken, `{"polecat_pool":{"agent":"a","max_seats":1}}`)
	writePanelFile(t, broken, "mayor/daemon.json", `{"patrols":`)
	rows = (&modelsReader{townRoot: broken, omPath: om}).read().SeatRows
	if rows[0].Unknown || rows[0].Closed || !rows[1].Unknown || rows[1].Closed {
		t.Errorf("an unreadable daemon.json: %+v", rows)
	}
}

// panelPreferHooked is panelDaemon with prefer_hooked, which moves the second
// seat first in the dispatcher's own seat order (gt-6oxrj).
const panelPreferHooked = `{"type":"daemon-patrol-config","version":1,"patrols":{
  "spec_dispatch":{"enabled":true,"hooked_agent":"claude-sonnet","max_hooked":2,"prefer_hooked":true},
  "scheduled_slings":{"enabled":true,"entries":[{"name":"deep-review","rig":"gastown","formula":"mol-deep-review","agent":"claude-sonnet","interval":"24h"}]}}}`

// An agent the town gives two seats is one seat to the dispatcher, so the panel
// prints one row for it: the seat that comes first in the dispatcher's order,
// whose cap is the one it spends (gt-6oxrj).
func TestModelsReaderSkipsASeatRepeatingAnEarlierSeatsAgent(t *testing.T) {
	t.Parallel()

	om := writeOMConfig(t, `{"backend":["claude","-p","--model","sonnet"]}`)

	town := t.TempDir()
	writeModelsSettings(t, town, `{"polecat_pool":{"agent":"deepseek-pro","max_seats":3}}`)
	writePanelFile(t, town, "mayor/daemon.json", panelDaemon)
	rows := (&modelsReader{townRoot: town, omPath: om}).read().SeatRows
	if len(rows) != 2 {
		t.Fatalf("seat rows = %+v, want the pool seat and the second seat", rows)
	}
	if rows[0].Seat != "pool" || rows[0].Agent != "deepseek-pro" || rows[0].Cap != 3 {
		t.Errorf("pool seat = %+v, want the seat the dispatcher's order reaches first", rows[0])
	}
	if rows[1].Seat != "second" {
		t.Errorf("seat rows = %+v, want the second seat after the pool seat", rows)
	}

	// prefer_hooked moves the second seat first, so when both seats name the
	// same agent the second is the one row kept.
	preferring := t.TempDir()
	writeModelsSettings(t, preferring, `{"polecat_pool":{"agent":"claude-sonnet","max_seats":3}}`)
	writePanelFile(t, preferring, "mayor/daemon.json", panelPreferHooked)
	rows = (&modelsReader{townRoot: preferring, omPath: om}).read().SeatRows
	if len(rows) != 1 || rows[0].Seat != "second" {
		t.Fatalf("seat rows = %+v, want only the second seat: the pool seat names the same agent", rows)
	}
	if rows[0].Agent != "claude-sonnet" || rows[0].Cap != 2 {
		t.Errorf("second seat = %+v, want the seat prefer_hooked moves first", rows[0])
	}
}

// Each role names its model and the key it was read from, and a config the
// town cannot read leaves its role unknown rather than empty (gt-lqa6p).
func TestModelsReaderListsTheModelPerRole(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":{"polecat":"claude-sonnet","crew":"claude-opus"}}`)
	writePanelFile(t, town, "mayor/daemon.json", panelDaemon)
	om := writeOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)

	roles := (&modelsReader{townRoot: town, omPath: om}).read().Roles
	for role, want := range map[string]string{"polecat": "claude-sonnet", "om-review": "sonnet", "deep-review": "claude-sonnet", "crew": "claude-opus"} {
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
	writePanelFile(t, bad, "mayor/daemon.json", `{"patrols":`)
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
	writeModelsSettings(t, town, `{"role_agents":{"polecat":"claude-sonnet","crew":"deepseek-flash"}}`)
	writePanelFile(t, town, "mayor/daemon.json", strings.Replace(panelDaemon, `"agent":"claude-sonnet"`, `"agent":"deepseek-flash"`, 1))
	writePanelFile(t, town, "settings/models.json", panelTable)

	roles := (&modelsReader{townRoot: town, omPath: om}).read().Roles
	if r := panelRole(t, roles, "polecat"); r.Drift != "" {
		t.Errorf("polecat matches the table and is marked: %+v", r)
	}
	if r := panelRole(t, roles, "om-review"); r.Drift != "sonnet" {
		t.Errorf("om-review runs flash against a sonnet table and drift = %q", r.Drift)
	}
	if r := panelRole(t, roles, "deep-review"); r.Drift != "claude-sonnet" {
		t.Errorf("deep-review drift = %q, want claude-sonnet", r.Drift)
	}
	// The table says nothing about crew.
	if r := panelRole(t, roles, "crew"); r.Drift != "" {
		t.Errorf("crew is not in the table and is marked: %+v", r)
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

// The seat table and the deep-review row name the file the patrol settings
// really live in: the "daemon" section of settings/config.json on a two-file
// town, mayor/daemon.json on a five-file one (gt-nk9qk).
func TestDashboardModelsNamesTheRealPatrolConfigLocation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		migrate bool
		want    string
		notWant string
	}{
		{"five-file", false, "(mayor/daemon.json)", `section "daemon"`},
		{"two-file", true, `(settings/config.json (section "daemon"))`, "mayor/daemon.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeModelsSettings(t, town, `{"polecat_pool":{"agent":"deepseek-flash","max_seats":3}}`)
			writePanelFile(t, town, "mayor/daemon.json", panelDaemon)
			if tc.migrate {
				writePanelFile(t, town, "mayor/town.json", `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`)
				if _, err := config.MigrateLayout(town); err != nil {
					t.Fatal(err)
				}
			}
			m := (&modelsReader{townRoot: town, omPath: writeOMConfig(t, `{"backend":["claude","-p","--model","sonnet"]}`)}).read()
			second, deep := m.SeatRows[1], panelRole(t, m.Roles, "deep-review")
			for what, key := range map[string]string{"second seat": second.Key, "deep-review": deep.Key} {
				if !strings.Contains(key, tc.want) {
					t.Errorf("%s key = %q, want it to name %q", what, key, tc.want)
				}
				if strings.Contains(key, tc.notWant) {
					t.Errorf("%s key = %q names %q", what, key, tc.notWant)
				}
			}
		})
	}
}

// deepReviewRows is the deep-review rows of a role list.
func deepReviewRows(roles []dashboard.ModelRole) []dashboard.ModelRole {
	var rows []dashboard.ModelRole
	for _, r := range roles {
		if strings.HasPrefix(r.Role, "deep-review") {
			rows = append(rows, r)
		}
	}
	return rows
}

// The review runs as two schedules, so the panel names each one's agent, checks
// each against the table, and keeps the legacy schedule's row while its entry
// is still configured (gt-m9oj7).
func TestModelsReaderNamesTheAgentOfEachDeepReviewSchedule(t *testing.T) {
	t.Parallel()

	const split = `{"type":"daemon-patrol-config","version":1,"patrols":{
  "scheduled_slings":{"enabled":true,"entries":[
    {"name":"deep-review-slice","rig":"gastown","formula":"mol-deep-review","agent":"claude-sonnet","interval":"24h"},
    {"name":"deep-review-risk","rig":"gastown","formula":"mol-deep-review","agent":"deepseek-pro","interval":"24h"},
    {"name":"nightly-other","rig":"gastown","formula":"mol-other","agent":"claude-opus","interval":"24h"}]}}}`
	const both = `{"type":"daemon-patrol-config","version":1,"patrols":{
  "scheduled_slings":{"enabled":true,"entries":[
    {"name":"deep-review","rig":"gastown","formula":"mol-deep-review","agent":"claude-sonnet","interval":"24h"},
    {"name":"deep-review-slice","rig":"gastown","formula":"mol-deep-review","interval":"24h"}]}}}`

	om := writeOMConfig(t, `{"backend":["claude","-p","--model","sonnet"]}`)
	roles := func(daemon string) []dashboard.ModelRole {
		town := t.TempDir()
		writeModelsSettings(t, town, `{"role_agents":{"polecat":"claude-sonnet"}}`)
		writePanelFile(t, town, "mayor/daemon.json", daemon)
		writePanelFile(t, town, "settings/models.json", panelTable)
		return (&modelsReader{townRoot: town, omPath: om}).read().Roles
	}

	deep := deepReviewRows(roles(split))
	if len(deep) != 2 || deep[0].Role != "deep-review-slice" || deep[1].Role != "deep-review-risk" {
		t.Fatalf("deep-review rows = %+v, want the slice row then the risk row", deep)
	}
	if deep[0].Model != "claude-sonnet" || deep[0].Drift != "" {
		t.Errorf("slice row = %+v, want claude-sonnet with no drift", deep[0])
	}
	if deep[1].Model != "deepseek-pro" || deep[1].Drift != "claude-sonnet" {
		t.Errorf("risk row = %+v, want deepseek-pro drifted from claude-sonnet", deep[1])
	}
	for _, r := range deep {
		if !strings.Contains(r.Key, "["+r.Role+"]") {
			t.Errorf("%s key = %q, want it to name its own schedule", r.Role, r.Key)
		}
	}

	// The legacy schedule configured next to a new one is a row of its own,
	// and a schedule that names no agent says so.
	r := roles(both)
	if s := panelRole(t, r, "deep-review-slice"); s.Model != "" || s.Note != "the schedule names no agent" || s.Drift != "" {
		t.Errorf("slice row with no agent = %+v", s)
	}
	if l := panelRole(t, r, "deep-review"); l.Model != "claude-sonnet" {
		t.Errorf("legacy row = %+v, want claude-sonnet", l)
	}

	// None of the review's schedules, and a config that cannot be read, are
	// each one deep-review row that says so.
	for name, tc := range map[string]struct{ daemon, note string }{
		"none":       {`{"patrols":{"scheduled_slings":{"enabled":true,"entries":[]}}}`, "no scheduled sling named"},
		"unreadable": {`{"patrols":`, "could not be read"},
	} {
		rows := deepReviewRows(roles(tc.daemon))
		if len(rows) != 1 || rows[0].Role != "deep-review" || rows[0].Model != "" || !strings.Contains(rows[0].Note, tc.note) || rows[0].Drift != "" {
			t.Errorf("%s: deep-review rows = %+v, want one unknown row noting %q", name, rows, tc.note)
		}
	}
}
