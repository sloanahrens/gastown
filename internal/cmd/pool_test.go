package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

const poolTestSettings = `{
  "type": "town-settings",
  "version": 1,
  "feed_curator": {"enabled": false},
  "polecat_pool": {
    "agent": "claude-opus",
    "max_seats": 3,
    "min_spawn_gap": "4m",
    "local_agent": "retired-local"
  }
}
`

const poolTestDaemon = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {
    "steward": {"enabled": false, "interval": "60s", "mode": "shadow"},
    "spec_dispatch": {
      "enabled": true,
      "interval": "30s",
      "hooked_agent": "claude-sonnet",
      "max_hooked": 2,
      "hookless_agent": "old"
    }
  }
}
`

// poolSeatMapSettings is a town on the seat map: two seats, one of them with a
// closed window, and names on one so a cap change can be shown to keep them.
const poolSeatMapSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "claude-sonnet": {"command": "claude"},
    "claude-haiku": {"command": "claude"},
    "deepseek-flash": {"command": "claude"}
  },
  "polecat_pool": {
    "timezone": "America/Chicago",
    "seats": {
      "claude-sonnet": {"cap": 2, "names": ["kestrel"]},
      "deepseek-flash": {"cap": 3, "closed": "20:00-08:00"}
    },
    "max_total": 5,
    "min_spawn_gap": "4m"
  }
}
`

// poolTwoSeatSettings is a town still on the two-seat keys: the map has not
// replaced them yet, and claude-opus is a town-defined agent as it is in town.
const poolTwoSeatSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "claude-opus": {"command": "claude"},
    "claude-sonnet": {"command": "claude"},
    "claude-haiku": {"command": "claude"}
  },
  "polecat_pool": {"agent": "claude-opus", "max_seats": 3, "min_spawn_gap": "4m"}
}
`

const poolTwoSeatDaemon = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {
    "spec_dispatch": {"enabled": true, "interval": "30s", "hooked_agent": "claude-sonnet", "max_hooked": 2}
  }
}
`

func poolTestTown(t *testing.T, settings, daemon string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range map[string]string{
		config.TownSettingsPath(root):       settings,
		config.DaemonPatrolConfigPath(root): daemon,
	} {
		if body == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// poolTestBudget is what the dispatcher would run with after the change.
func poolTestBudget(t *testing.T, root string) map[string]int {
	t.Helper()
	caps := map[string]int{}
	for _, s := range poolTestTownBudget(t, root).Seats {
		caps[s.Agent] = s.Cap
	}
	return caps
}

// poolTestTownBudget is the dispatcher's whole budget after the change, the
// town's total included.
func poolTestTownBudget(t *testing.T, root string) specdispatch.Budget {
	t.Helper()
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	return specBudgetFromConfig(ts, sd)
}

// poolTestSetTotal runs the total cap the way the command does.
func poolTestSetTotal(t *testing.T, root string, n int) string {
	t.Helper()
	var out bytes.Buffer
	if err := runPoolSetTotal(&out, root, n); err != nil {
		t.Fatalf("set-total %d: %v", n, err)
	}
	return out.String()
}

// poolTestSetSeats runs one gt pool set on parsed changes.
func poolTestSetSeats(t *testing.T, root string, changes []poolSeatCap, remove []string, total int, totalSet bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := runPoolSetSeats(&out, root, changes, remove, total, totalSet); err != nil {
		t.Fatalf("pool set: %v", err)
	}
	return out.String()
}

// poolTestSeatCaps is polecat_pool.seats as caps by agent.
func poolTestSeatCaps(t *testing.T, root string) map[string]int {
	t.Helper()
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if ts.PolecatPool == nil {
		return nil
	}
	out := make(map[string]int, len(ts.PolecatPool.Seats))
	for agent, seat := range ts.PolecatPool.Seats {
		out[agent] = seat.Cap
	}
	return out
}

// poolTestSeatEntry is one seat as the settings file holds it.
func poolTestSeatEntry(t *testing.T, root, agent string) (config.SeatConfig, bool) {
	t.Helper()
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if ts.PolecatPool == nil {
		return config.SeatConfig{}, false
	}
	seat, ok := ts.PolecatPool.Seats[agent]
	return seat, ok
}

// poolTestRawSettings is the settings file's bytes, for the no-write refusals.
func poolTestRawSettings(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// poolTestRetiredSeat is a pool seat as the rename gt-plk1z left it: spelled
// with the retired keys, which the loader folds into agent and max_seats.
const poolTestRetiredSeat = `{
  "type": "town-settings",
  "version": 1,
  "polecat_pool": {
    "overflow_agent": "codex",
    "max_overflow": 3,
    "min_spawn_gap": "4m"
  }
}
`

// poolTestPoolSection is polecat_pool as the settings file holds it.
func poolTestPoolSection(t *testing.T, root string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		PolecatPool map[string]any `json:"polecat_pool"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.PolecatPool
}

// poolTestSeatList is the seats the dispatcher would run, in its order.
func poolTestSeatList(t *testing.T, root string) []specdispatch.Seat {
	t.Helper()
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	return specBudgetFromConfig(ts, sd).Seats
}

// poolTestShownSeats is the seats show printed, in the order it printed them:
// the agent on each seat line, up to the line that names the keys read.
func poolTestShownSeats(t *testing.T, out string) []string {
	t.Helper()
	var agents []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "read:") {
			break
		}
		for _, name := range []string{"pool seat", "second seat"} {
			rest, ok := strings.CutPrefix(line, name)
			if !ok {
				continue
			}
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				t.Fatalf("seat line %q names no agent", line)
			}
			agents = append(agents, fields[0])
		}
	}
	return agents
}

func TestPoolShowPrintsEachOpenSeatAndTheKeysRead(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	var out bytes.Buffer
	if err := runPoolShow(&out, root); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"pool seat    claude-opus  cap 3",
		"second seat  claude-sonnet  cap 2",
		"polecat_pool.agent", "polecat_pool.max_seats",
		"patrols.spec_dispatch.hooked_agent", "max_hooked",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("show output lacks %q:\n%s", want, got)
		}
	}
}

func TestPoolShowOmitsClosedSeatsAndNamesTheDispatcherDefault(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus"}}`, "")
	var out bytes.Buffer
	if err := runPoolShow(&out, root); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "second seat") {
		t.Errorf("a closed second seat was shown:\n%s", got)
	}
	if !strings.Contains(got, "uncapped for sling, cap 2 for the dispatcher") {
		t.Errorf("an unset max_seats was not explained:\n%s", got)
	}

	root = poolTestTown(t, `{"type":"town-settings","version":1}`, "")
	out.Reset()
	if err := runPoolShow(&out, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no open seat") {
		t.Errorf("a town with no seat did not say so:\n%s", out.String())
	}
}

// show prints the town's total beside the seats, because it is the cap in
// force when the seats each have room (gt-bcpj5). A key the file does not carry
// is named as unset, and a written 0 as itself.
func TestPoolShowPrintsTheTownTotal(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus","max_seats":3,"max_total":4}}`, "")
	var out bytes.Buffer
	if err := runPoolShow(&out, root); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "total        cap 4 (max_total): live polecats across every seat") {
		t.Errorf("show did not print the total cap the dispatcher and sling read:\n%s", got)
	}
	if !strings.Contains(got, "polecat_pool.max_total") {
		t.Errorf("show did not name the key it read:\n%s", got)
	}

	for _, tc := range []struct {
		settings string
		want     string
	}{
		{`{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus"}}`, "total        no cap (max_total unset)"},
		{`{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus","max_total":0}}`, "total        no cap (max_total 0)"},
		{`{"type":"town-settings","version":1}`, "total        no cap (max_total unset)"},
	} {
		root := poolTestTown(t, tc.settings, "")
		out.Reset()
		if err := runPoolShow(&out, root); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("show of %s lacks %q:\n%s", tc.settings, tc.want, out.String())
		}
	}
}

// set-total writes polecat_pool.max_total, which the dispatcher's next tick
// reads, and 0 removes the key: an absent max_total is no cap and a written 0
// would say the same, so a zero is never left on the page (gt-bcpj5).
func TestPoolSetTotalSetsAndRemovesTheCap(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus","max_seats":3,"min_spawn_gap":"4m"}}`, "")

	out := poolTestSetTotal(t, root, 4)
	if got := poolTestTownBudget(t, root).MaxTotal; got != 4 {
		t.Errorf("budget total cap = %d, want 4", got)
	}
	if pool := poolTestPoolSection(t, root); pool["max_total"] != float64(4) {
		t.Errorf("polecat_pool = %v, want max_total 4", pool)
	}
	if !strings.Contains(out, "cap 4 live polecats across every seat") {
		t.Errorf("set-total output = %q, want the cap it wrote", out)
	}

	out = poolTestSetTotal(t, root, 0)
	if got := poolTestTownBudget(t, root).MaxTotal; got != 0 {
		t.Errorf("budget total cap = %d after removing it, want 0", got)
	}
	pool := poolTestPoolSection(t, root)
	if v, ok := pool["max_total"]; ok {
		t.Errorf("polecat_pool.max_total = %v was left behind, want the key gone", v)
	}
	for key, want := range map[string]any{"agent": "claude-opus", "max_seats": float64(3), "min_spawn_gap": "4m"} {
		if pool[key] != want {
			t.Errorf("removing the total cap moved polecat_pool.%s to %v, want %v", key, pool[key], want)
		}
	}
	if !strings.Contains(out, "total cap removed (max_total)") {
		t.Errorf("set-total 0 output = %q, want the removal named", out)
	}
}

// The total is a cap on a town rather than on a seat, so it can be set on a
// pool that has no agent: the dispatcher holds and sling refuses at it either
// way (gt-bcpj5). Setting it opens the section the key lives in, and removing
// it on a town that has no section writes nothing to invent one.
func TestPoolSetTotalWithoutAPoolSeat(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1}`, "")
	poolTestSetTotal(t, root, 2)
	if got := poolTestTownBudget(t, root).MaxTotal; got != 2 {
		t.Errorf("budget total cap = %d, want 2 on a town with no pool seat", got)
	}
	if got := poolTestPoolSection(t, root)["max_total"]; got != float64(2) {
		t.Errorf("polecat_pool.max_total = %v, want 2", got)
	}

	bare := poolTestTown(t, `{"type":"town-settings","version":1}`, "")
	poolTestSetTotal(t, bare, 0)
	if _, ok := poolTestPoolSection(t, bare)["max_total"]; ok {
		t.Error("set-total 0 invented a max_total key")
	}
}

// The write path is the seat sets' path: a backup beside the file, keys the
// command does not own left alone (gt-bcpj5).
func TestPoolSetTotalBacksUpTheFile(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	poolTestSetTotal(t, root, 2)
	baks, _ := filepath.Glob(config.TownSettingsPath(root) + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("settings backups = %v, want one", baks)
	}
	if got, _ := os.ReadFile(baks[0]); string(got) != poolTestSettings {
		t.Errorf("backup = %q, want the settings file as it was", got)
	}
	raw, _ := os.ReadFile(config.TownSettingsPath(root))
	for _, want := range []string{`"max_total": 2`, `"feed_curator"`, `"min_spawn_gap": "4m"`, `"local_agent": "retired-local"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("settings lost %s:\n%s", want, raw)
		}
	}
}

// A set changes the named agents and nothing else: a seat it did not name keeps
// its cap, and a named seat keeps the window and names a cap change does not
// touch (gt-mukgk).
func TestPoolSetChangesOnlyTheNamedAgents(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")

	out := poolTestSetSeats(t, root, []poolSeatCap{{Agent: "claude-haiku", Cap: 2}}, nil, 0, false)
	caps := poolTestSeatCaps(t, root)
	for agent, want := range map[string]int{"claude-haiku": 2, "claude-sonnet": 2, "deepseek-flash": 3} {
		if caps[agent] != want {
			t.Errorf("seats[%s] = %d, want %d (seats: %v)", agent, caps[agent], want, caps)
		}
	}
	if !strings.Contains(out, "claude-haiku: cap 2 (new seat)") {
		t.Errorf("set output = %q, want the new seat named", out)
	}

	out = poolTestSetSeats(t, root, []poolSeatCap{{Agent: "deepseek-flash", Cap: 4}}, nil, 0, false)
	seat, ok := poolTestSeatEntry(t, root, "deepseek-flash")
	if !ok || seat.Cap != 4 || seat.Closed != "20:00-08:00" {
		t.Errorf("deepseek-flash = %+v, want cap 4 with its closed window kept", seat)
	}
	if strings.Contains(out, "new seat") {
		t.Errorf("a cap change on an existing seat is reported as a new seat:\n%s", out)
	}
	if sonnet, _ := poolTestSeatEntry(t, root, "claude-sonnet"); !slices.Contains(sonnet.Names, "kestrel") {
		t.Errorf("claude-sonnet lost its names: %+v", sonnet)
	}
}

// --remove drops one seat and --total sets the town cap; neither implies the
// other, and a total of 0 removes the key (gt-mukgk).
func TestPoolSetRemovesASeatAndSetsTheTotal(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")

	out := poolTestSetSeats(t, root, nil, []string{"deepseek-flash"}, 8, true)
	if caps := poolTestSeatCaps(t, root); caps["deepseek-flash"] != 0 || caps["claude-sonnet"] != 2 {
		t.Errorf("seats after --remove = %v, want claude-sonnet at 2 and no deepseek-flash", caps)
	}
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if ts.PolecatPool.MaxTotal == nil || *ts.PolecatPool.MaxTotal != 8 {
		t.Errorf("max_total = %v, want 8", ts.PolecatPool.MaxTotal)
	}
	for _, want := range []string{"removed deepseek-flash", "total: cap 8 live polecats across every seat"} {
		if !strings.Contains(out, want) {
			t.Errorf("set output lacks %q:\n%s", want, out)
		}
	}

	poolTestSetSeats(t, root, nil, nil, 0, true)
	ts, err = config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if ts.PolecatPool.MaxTotal != nil {
		t.Errorf("max_total = %d after --total 0, want the key gone", *ts.PolecatPool.MaxTotal)
	}
}

// An invalid result writes nothing: an unknown agent is refused by the loader
// the town starts with, and the file keeps its bytes (gt-mukgk).
func TestPoolSetRefusesAnInvalidMapAndWritesNothing(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")
	before := poolTestRawSettings(t, root)

	err := runPoolSetSeats(&bytes.Buffer{}, root, []poolSeatCap{{Agent: "not-an-agent", Cap: 2}}, nil, 0, false)
	if err == nil {
		t.Fatal("set on an unknown agent: want a refusal")
	}
	if !strings.Contains(err.Error(), "no agent of that name") {
		t.Errorf("refusal = %q, want the loader's unknown-agent message", err)
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Errorf("a refused set rewrote the file:\n%s", after)
	}
}

func TestPoolSetRefusesToRemoveASeatThatIsNotThere(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")
	before := poolTestRawSettings(t, root)

	err := runPoolSetSeats(&bytes.Buffer{}, root, nil, []string{"claude-haiku"}, 0, false)
	if err == nil {
		t.Fatal("removing a seat the map does not have: want a refusal")
	}
	for _, want := range []string{"is not a seat", "claude-sonnet, deepseek-flash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a refused remove rewrote the file")
	}
}

// A town still on the two-seat keys keeps every seat the dispatcher runs: the
// first set seeds the map from those keys, so naming one agent does not drop
// the others, and the old keys stay for the migration (gt-mukgk).
func TestPoolSetSeedsTheMapFromTheTwoSeatKeys(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTwoSeatSettings, poolTwoSeatDaemon)

	poolTestSetSeats(t, root, []poolSeatCap{{Agent: "claude-haiku", Cap: 2}}, nil, 0, false)
	caps := poolTestSeatCaps(t, root)
	for agent, want := range map[string]int{"claude-opus": 3, "claude-sonnet": 2, "claude-haiku": 2} {
		if caps[agent] != want {
			t.Errorf("seats[%s] = %d, want %d (seats: %v)", agent, caps[agent], want, caps)
		}
	}
	pool := poolTestPoolSection(t, root)
	if pool["agent"] != "claude-opus" || pool["max_seats"] != float64(3) {
		t.Errorf("set rewrote the two-seat keys: %v", pool)
	}

	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := specBudgetFor(root, ts, sd, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(budget.Seats) != 3 {
		t.Errorf("the dispatcher runs %+v, want every seeded seat", budget.Seats)
	}
}

// Removing the last seat from a file still running the two-seat keys is
// refused: an empty map is no map, so the next tick would go back to those keys
// rather than run nothing. A cap of 0 closes the seat without that detour
// (gt-mukgk).
func TestPoolSetRefusesToRemoveEverySeatWhileTheTwoSeatKeysRun(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTwoSeatSettings, poolTwoSeatDaemon)
	before := poolTestRawSettings(t, root)

	err := runPoolSetSeats(&bytes.Buffer{}, root, nil, []string{"claude-opus", "claude-sonnet"}, 0, false)
	if err == nil || !strings.Contains(err.Error(), "cap to 0") {
		t.Fatalf("removing every seat = %v, want a refusal naming the cap-0 replacement", err)
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a refused remove rewrote the file")
	}
}

// The write keeps every key gt pool does not own and a backup of the file it
// replaces (gt-mukgk).
func TestPoolSetKeepsEveryKeyItDoesNotOwn(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")
	poolTestSetSeats(t, root, []poolSeatCap{{Agent: "claude-haiku", Cap: 2}}, nil, 0, false)

	raw := poolTestRawSettings(t, root)
	for _, want := range []string{`"min_spawn_gap": "4m"`, `"max_total": 5`, `"timezone": "America/Chicago"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("settings lost %s:\n%s", want, raw)
		}
	}
	baks, _ := filepath.Glob(config.TownSettingsPath(root) + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("settings backups = %v, want one", baks)
	}
	if got, _ := os.ReadFile(baks[0]); string(got) != poolSeatMapSettings {
		t.Errorf("backup = %q, want the settings file as it was", got)
	}
}

func TestPoolSetRefusesAFileItCannotParse(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings",`, poolTestDaemon)
	if err := runPoolSetSeats(&bytes.Buffer{}, root, []poolSeatCap{{Agent: "claude-haiku", Cap: 2}}, nil, 0, false); err == nil {
		t.Fatal("want an error for settings that do not parse")
	}
	if got := poolTestRawSettings(t, root); got != `{"type":"town-settings",` {
		t.Errorf("a file that did not parse was rewritten: %q", got)
	}
}

func TestParsePoolSeatCapsRefuses(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"claude-haiku", "=2", "claude-haiku=x", "claude-haiku=-1"} {
		if _, err := parsePoolSeatCaps([]string{arg}); err == nil {
			t.Errorf("parsePoolSeatCaps(%q): want a refusal", arg)
		}
	}
	if _, err := parsePoolSeatCaps([]string{"claude-haiku=1", "claude-haiku=2"}); err == nil {
		t.Error("parsePoolSeatCaps: want a refusal for one agent named twice")
	}
}

// gt pool set takes --total and --remove in addition to the pairs (gt-mukgk).
func TestPoolSetCommandTakesTheTotalAndRemoveFlags(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"total", "remove"} {
		if poolSetCmd.Flags().Lookup(flag) == nil {
			t.Errorf("gt pool set has no --%s", flag)
		}
	}
}

// show reads the seat map: live over cap, open or closed with the next
// opening, one line per agent and the town's total (gt-mukgk).
func TestPoolShowSeatMapPrintsEachAgentAndTheTotal(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 19, 21, 0, 0, 0, loc)
	live := poolShowLive{ok: true, live: map[string]int{"claude-sonnet": 1}, total: 1}

	var out bytes.Buffer
	if err := runPoolSeatMapShow(&out, root, ts, now, live); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"claude-sonnet   1/2  open",
		"deepseek-flash  0/3  closed until 08:00",
		"total           1/5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("show output lacks %q:\n%s", want, got)
		}
	}
}

// A picture that could not be read is no count: show names the caps and the
// total cap rather than inventing a zero (gt-bc9b6).
func TestPoolShowSeatMapNamesTheCapsWithoutARoster(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPoolSeatMapShow(&out, root, ts, time.Now(), poolShowLive{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deepseek-flash  cap 3", "total           cap 5"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show output lacks %q:\n%s", want, out.String())
		}
	}
}

// A file with a seat map runs the map: the two-seat keys beside it open no seat
// and show does not print them (gt-mukgk).
func TestPoolShowSeatMapIgnoresTheTwoSeatKeys(t *testing.T) {
	t.Parallel()
	settings := `{"type":"town-settings","version":1,` +
		`"agents":{"claude-sonnet":{"command":"claude"},"claude-haiku":{"command":"claude"}},` +
		`"polecat_pool":{"agent":"claude-opus","max_seats":9,` +
		`"seats":{"claude-sonnet":{"cap":2},"claude-haiku":{"cap":1}}}}`
	root := poolTestTown(t, settings, "")
	var out bytes.Buffer
	if err := runPoolShow(&out, root); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "pool seat") || strings.Contains(got, "claude-opus") {
		t.Errorf("show printed the two-seat keys beside the map:\n%s", got)
	}
	for _, agent := range []string{"claude-sonnet", "claude-haiku"} {
		if !strings.Contains(got, agent) {
			t.Errorf("show does not print the map's %s seat:\n%s", agent, got)
		}
	}
}

// show prints the seats the dispatcher runs, in its order. The matrix is the
// states a seat can be in — absent, at a default, capped, deduped into
// another, preferred first — and each one has to come out naming the agents
// the dispatcher spends caps on, or the picture is a second opinion (gt-riazi).
func TestPoolShowListsTheSeatsTheDispatcherRuns(t *testing.T) {
	t.Parallel()
	towns := []struct {
		name     string
		settings string
		daemon   string
	}{
		{"no section", `{"type":"town-settings","version":1}`, ""},
		{"pool seat alone", `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus"}}`, ""},
		{"pool seat and second seat", poolTestSettings, poolTestDaemon},
		{"stale pro keys open no seat",
			`{"type":"town-settings","version":1,"polecat_pool":{"agent":"deepseek-pro","max_seats":3,"pro_agent":"deepseek-reasoner","pro_max":2}}`, ""},
		{"second seat preferred", `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus","max_seats":3}}`,
			`{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true,"hooked_agent":"claude-sonnet","max_hooked":2,"prefer_hooked":true}}}`},
	}
	for _, tc := range towns {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolTestTown(t, tc.settings, tc.daemon)
			var out bytes.Buffer
			if err := runPoolShow(&out, root); err != nil {
				t.Fatal(err)
			}
			seats := poolTestSeatList(t, root)
			if len(seats) == 0 {
				if !strings.Contains(out.String(), "no open seat") {
					t.Errorf("no seat is open and show did not say so:\n%s", out.String())
				}
				return
			}
			want := make([]string, 0, len(seats))
			for _, s := range seats {
				want = append(want, s.Agent)
			}
			if got := poolTestShownSeats(t, out.String()); !slices.Equal(got, want) {
				t.Errorf("show lists %v, want the dispatcher's seats %v:\n%s", got, want, out.String())
			}
		})
	}
}

// TestPoolShowNamesTheDeadHookedSeatsInTheCount is gt-bc9b6's display half: the
// page an operator reads must show the crashed seat inside the live count, the
// way the tick line and the sling refusal do, so a pool that is full with fewer
// live sessions than its number says why. The seat is the 2026-10-12 shape — a
// DEFERRED exit that cleared the agent bead's hook_bead while the work bead
// stayed assigned — and the picture it is counted in is the real one: the real
// rig and seat enumeration, the real seat read, canned beads reads and a canned
// tmux server (the unit tier starts no tmux; docs/testing.md).
func TestPoolShowNamesTheDeadHookedSeatsInTheCount(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"polecat_pool":{"agent":"deepseek-flash","max_seats":3,"max_total":4}}`, "")
	writeTestRigsConfig(t, root, "gastown")
	writeTownPolecatDir(t, root, "gastown", "ember")
	reads := poolSeatReadsAnswering(t, deferredExitFields(), assignedActiveWork("gt-held"))

	view, err := loadPoolSeats(root)
	if err != nil {
		t.Fatal(err)
	}
	live := poolLiveForWith(root, func(pool *config.PolecatPool) ([]poolSession, error) {
		return poolSeatSessionsWith(&fakeLister{}, root, poolDispositionAnswering(root, reads), nil, pool)
	})

	var out bytes.Buffer
	if err := renderPoolShow(&out, root, view, live); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"pool seat    deepseek-flash  cap 3  live 1/3 (1 dead-hooked)",
		"total        cap 4 (max_total): live polecats across every seat  live 1/4",
	} {
		if got := out.String(); !strings.Contains(got, want) {
			t.Errorf("show output lacks %q:\n%s", want, got)
		}
	}
}

// TestPoolShowAndTheTickAgreeOnAMixedPoolsSessionlessSeat is gt-2q283 through
// both readers of one seat picture: `gt pool show`'s page and the spec
// dispatcher's tick line are built from the same count (poolSeatSessions →
// specRosterFrom), so the counts an operator reads on the page are the counts
// the dispatcher refuses on.
//
// The town is the mixed pool of 2026-10-14: the pool's own seat is
// claude-sonnet with two seats, the hooked seat is deepseek-flash with two, and
// a flash seat's session is gone with its bead still hooked. On 11:34 the tick
// line read claude-sonnet 4/2 (1 dead-hooked), deepseek-flash 0/2 — the crashed
// seat counted on the pool's own agent, so the pool read full with two Sonnet
// sessions in the town and the other seat read empty.
func TestPoolShowAndTheTickAgreeOnAMixedPoolsSessionlessSeat(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, mixedPoolTestSettings, mixedPoolTestDaemon)
	writeTestRigsConfig(t, root, "gastown")
	writeTownPolecatDir(t, root, "gastown", "ember")
	// The seat was slung on the hooked seat's agent, and its session is gone:
	// the record is the only place that still says so.
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ember"}, root,
		intent.Record{Agent: "deepseek-flash"})
	reads := poolSeatReadsAnswering(t, deferredExitFields(), assignedActiveWork("gt-held"))

	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := poolSeatSessionsWith(&fakeLister{}, root, poolDispositionAnswering(root, reads), nil, ts.PolecatPool)
	if err != nil {
		t.Fatalf("poolSeatSessionsWith: %v", err)
	}

	// The tick line: the seats the dispatcher runs, occupied from the roster.
	budget := poolTestTownBudget(t, root)
	roster := specRosterFrom(sessions, ts)
	budget.SetLive(roster.Live)
	budget.SetDeadHooked(roster.DeadHooked)

	// The page: the same seats, occupied from the same seats' picture.
	view, err := loadPoolSeats(root)
	if err != nil {
		t.Fatal(err)
	}
	live := poolLiveForWith(root, func(pool *config.PolecatPool) ([]poolSession, error) {
		return poolSeatSessionsWith(&fakeLister{}, root, poolDispositionAnswering(root, reads), nil, pool)
	})
	var out bytes.Buffer
	if err := renderPoolShow(&out, root, view, live); err != nil {
		t.Fatal(err)
	}
	page := out.String()

	// The crash is on the seat it was slung on, in both readings, and the
	// pool's own seat is empty because nothing live sits on it.
	for _, want := range []string{"claude-sonnet 0/2", "deepseek-flash 1/2 (1 dead-hooked)"} {
		if !strings.Contains(budget.Picture(), want) {
			t.Errorf("the tick line reads %q, want it to name %q", budget.Picture(), want)
		}
	}
	for _, seat := range budget.Seats {
		line := seatLineFor(page, seat.Agent)
		if line == "" {
			t.Fatalf("the page prints no line for seat %s:\n%s", seat.Agent, page)
		}
		if got := fmt.Sprintf("live %d/%d", seat.Live, seat.Cap); !strings.Contains(line, got) {
			t.Errorf("the page reads %q for seat %s, the tick line counts it %q:\n%s", line, seat.Agent, got, page)
		}
	}
	// The town's total is the one number both readings already agreed on, and
	// it is not what changed: one polecat is one polecat.
	if !strings.Contains(page, "live 1/4") {
		t.Errorf("the page's total should count the crashed seat once:\n%s", page)
	}
}

// seatLineFor returns the pool-show line for agent, or "".
func seatLineFor(page, agent string) string {
	for _, line := range strings.Split(page, "\n") {
		if strings.Contains(line, " "+agent+" ") || strings.HasSuffix(line, " "+agent) {
			return line
		}
	}
	return ""
}

// mixedPoolTestSettings is the 2026-10-14 town: the pool's seat is
// claude-sonnet (also the polecat role default) with two seats, and the town
// caps every seat together at four.
const mixedPoolTestSettings = `{
  "type": "town-settings",
  "version": 1,
  "polecat_pool": {"agent": "claude-sonnet", "max_seats": 2, "max_total": 4},
  "role_agents": {"polecat": "claude-sonnet"}
}
`

// mixedPoolTestDaemon puts the dispatcher's second seat on deepseek-flash with
// two seats.
const mixedPoolTestDaemon = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {
    "spec_dispatch": {"enabled": true, "interval": "60s", "hooked_agent": "deepseek-flash", "max_hooked": 2}
  }
}
`

// TestPoolShowReadsNoSeatPictureWithoutARigRegistry pins the guard that keeps
// the page off the tmux server where there is nothing to count: a town whose rig
// registry is missing names no live column at all, rather than a partial count
// (the unit tier starts no tmux).
func TestPoolShowReadsNoSeatPictureWithoutARigRegistry(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"polecat_pool":{"agent":"deepseek-flash","max_seats":3}}`, "")
	var out bytes.Buffer
	if err := runPoolShow(&out, root); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "live ") {
		t.Errorf("a town with no rigs has no polecats to count:\n%s", got)
	}
}

// A two-file town (gt config migrate) has no mayor/daemon.json: the patrol
// settings are the "daemon" section of settings/config.json. show must name
// that, not the retired file, and a five-file town still gets the old path
// (gt-nk9qk).
func TestPoolShowNamesTheRealPatrolConfigLocation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		migrate bool
		want    string
		notWant string
	}{
		{"five-file", false, "mayor/daemon.json (patrols.spec_dispatch.hooked_agent, max_hooked)", `section "daemon"`},
		{"two-file", true, `settings/config.json (section "daemon") (patrols.spec_dispatch.hooked_agent, max_hooked)`, "mayor/daemon.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolTestTown(t, poolTestSettings, poolTestDaemon)
			if tc.migrate {
				poolTestMigrate(t, root)
			}
			var out bytes.Buffer
			if err := runPoolShow(&out, root); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("show output lacks %q:\n%s", tc.want, got)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("show output names %q:\n%s", tc.notWant, got)
			}
		})
	}
}

// poolTestMigrate moves a pool test town to the two-file layout.
func poolTestMigrate(t *testing.T, root string) {
	t.Helper()
	townJSON := filepath.Join(root, "mayor", "town.json")
	if err := os.MkdirAll(filepath.Dir(townJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(townJSON, []byte(`{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.MigrateLayout(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.DaemonPatrolConfigPath(root)); !os.IsNotExist(err) {
		t.Fatalf("mayor/daemon.json survived the migration: %v", err)
	}
}

// A block specDispatchOf makes writes "enabled": true, and it reads back as
// enabled:true after the JSON round trip the daemon.json write and the ticker's
// read make.
func TestSpecDispatchOfKeepsEnabled(t *testing.T) {
	t.Parallel()
	pc := &config.DaemonPatrolConfig{}
	specDispatchOf(pc).MaxHooked = 1
	raw, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"enabled":true`) {
		t.Errorf("daemon.json = %s, want the block to say enabled:true", raw)
	}
	var back config.DaemonPatrolConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	sd := back.Patrols.SpecDispatch
	if sd == nil || (sd.Enabled == nil || !*sd.Enabled) {
		t.Errorf("spec_dispatch after the round trip = %+v, want enabled:true", sd)
	}
}

// The second seat's agent is named the second seat: there is no other seat
// left for it to be mistaken for.
func TestSeatHeldBySecondSeat(t *testing.T) {
	t.Parallel()
	ts := &config.TownSettings{PolecatPool: &config.PolecatPool{Agent: "claude-sonnet"}}
	sd := &config.SpecDispatchConfig{HookedAgent: "deepseek-pro", MaxHooked: 2}

	v := poolSeatsOf(ts, sd)

	if got := v.seatHeldBy("deepseek-pro"); got != secondSeat {
		t.Errorf("seatHeldBy(deepseek-pro) = %s, want the second seat", seatName(got))
	}
	var seats []string
	for _, s := range v.seats {
		seats = append(seats, seatName(s.seat)+"="+s.agent)
	}
	if want := []string{"pool seat=claude-sonnet", "second seat=deepseek-pro"}; !slices.Equal(seats, want) {
		t.Errorf("listed seats = %q, want %q", seats, want)
	}
	if holder, shadowed := v.shadowedSecondSeat(); shadowed {
		t.Errorf("the running second seat is reported as not run, shadowed by the %s", seatName(holder))
	}
}
