package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// The panel lists both polecat seats the way gt pool show does: an open seat
// with its agent and cap, a seat nothing may fill as closed (gt-lqa6p).
func TestModelsReaderListsEachSeat(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeModelsSettings(t, town, `{"polecat_pool":{"agent":"deepseek-flash","max_seats":3}}`)
	writePanelFile(t, town, "mayor/daemon.json", panelDaemon)
	om := writeOMConfig(t, `{"backend":["claude","-p","--model","sonnet"]}`)

	rows := (&modelsReader{townRoot: town, omPath: om}).read().SeatRows
	if len(rows) != 2 {
		t.Fatalf("seat rows = %+v, want the pool seat and the second seat", rows)
	}
	pool, second := rows[0], rows[1]
	if pool.Seat != "pool" || pool.Agent != "deepseek-flash" || pool.Cap != 3 || pool.Closed || pool.CapDefault {
		t.Errorf("pool seat = %+v", pool)
	}
	if second.Seat != "second" || second.Agent != "claude-sonnet" || second.Cap != 2 || second.Closed {
		t.Errorf("second seat = %+v", second)
	}
	if pool.Key == "" || second.Key == "" {
		t.Error("a seat does not name the config it was read from")
	}

	// A pool with no max_seats is the dispatcher's default cap, marked as such.
	writeModelsSettings(t, town, `{"polecat_pool":{"agent":"deepseek-flash"}}`)
	if pool := (&modelsReader{townRoot: town, omPath: om}).read().SeatRows[0]; pool.Cap != defaultSpecSeatCap || !pool.CapDefault {
		t.Errorf("an unset max_seats = %+v, want the default cap marked", pool)
	}

	// No pool agent and no max_hooked: both seats are closed.
	closed := t.TempDir()
	writeModelsSettings(t, closed, `{"polecat_pool":{}}`)
	writePanelFile(t, closed, "mayor/daemon.json", `{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true}}}`)
	for _, s := range (&modelsReader{townRoot: closed, omPath: om}).read().SeatRows {
		if !s.Closed || s.Agent != "" || s.Cap != 0 {
			t.Errorf("seat %q = %+v, want closed", s.Seat, s)
		}
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
