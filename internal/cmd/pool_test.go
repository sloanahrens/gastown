package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// poolTestSettings is a one-seat town with keys gt pool does not own beside the
// pool: a feed curator block, a spawn gap and the retired local seat's key.
const poolTestSettings = `{
  "type": "town-settings",
  "version": 1,
  "feed_curator": {"enabled": false},
  "agents": {"claude-opus": {"command": "claude"}},
  "polecat_pool": {
    "seats": {"claude-opus": {"cap": 3}},
    "min_spawn_gap": "4m",
    "local_agent": "retired-local"
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

// poolTestTownBudget is the dispatcher's whole budget after the change, the
// town's total included, built the way a tick builds it.
func poolTestTownBudget(t *testing.T, root string) specdispatch.Budget {
	t.Helper()
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	b, err := specBudgetFor(root, ts, poolTestNow)
	if err != nil {
		t.Fatal(err)
	}
	return b
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
	return poolTestTownBudget(t, root).Seats
}

// poolTestShowAt is what gt pool show prints for root at the test clock. The
// town has no rig registry unless a test wrote one, so the page names caps and
// reads no tmux.
func poolTestShowAt(t *testing.T, root string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runPoolShowAt(&out, root, poolTestNow); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// poolTestShowLine is the page's line whose first word is first, with its runs
// of spaces folded to one so a test does not depend on the column width, or ""
// when no line starts so.
func poolTestShowLine(page, first string) string {
	for _, line := range strings.Split(page, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == first {
			return strings.Join(fields, " ")
		}
	}
	return ""
}

// poolTestShownSeats is the seats show printed, in the order it printed them:
// the first word of each line before the total line.
func poolTestShownSeats(t *testing.T, out string) []string {
	t.Helper()
	var agents []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "total" {
			break
		}
		if fields[0] == "no" && len(fields) > 1 && fields[1] == "seats:" {
			continue
		}
		agents = append(agents, fields[0])
	}
	return agents
}

// show reads the seat map from disk and prints one line per seat, then the
// total and the keys it read (gt-mukgk). The clock is 2026-09-18 16:00 UTC, so
// the flash seat's 20:00-08:00 Chicago window is open (11:00 there).
func TestPoolShowPrintsEachSeatAndTheKeysRead(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")
	got := poolTestShowAt(t, root)
	for first, want := range map[string]string{
		"claude-sonnet":  "claude-sonnet cap 2 open",
		"deepseek-flash": "deepseek-flash cap 3 open",
		"total":          "total cap 5",
	} {
		if line := poolTestShowLine(got, first); line != want {
			t.Errorf("show line for %s = %q, want %q:\n%s", first, line, want, got)
		}
	}
	if !strings.Contains(got, "polecat_pool.seats, max_total, timezone America/Chicago") {
		t.Errorf("show does not name the keys it read:\n%s", got)
	}
}

// A town with no seat says so instead of printing nothing, and a seat shut by a
// cap of 0 is listed as closed rather than left out (gt-mukgk).
func TestPoolShowSaysWhenThereIsNoSeatAndListsClosedSeats(t *testing.T) {
	t.Parallel()
	for _, settings := range []string{
		`{"type":"town-settings","version":1}`,
		`{"type":"town-settings","version":1,"polecat_pool":{"min_spawn_gap":"4m"}}`,
	} {
		root := poolTestTown(t, settings, "")
		if got := poolTestShowAt(t, root); !strings.Contains(got, "no seats:") {
			t.Errorf("a town with no seat did not say so:\n%s", got)
		}
	}

	root := poolTestTown(t, `{"type":"town-settings","version":1,"agents":{"claude-opus":{"command":"claude"}},`+
		`"polecat_pool":{"seats":{"claude-opus":{"cap":0}}}}`, "")
	got := poolTestShowAt(t, root)
	if line := poolTestShowLine(got, "claude-opus"); line != "claude-opus cap 0 closed (cap 0)" {
		t.Errorf("a seat with a cap of 0 = %q, want it listed as closed:\n%s", line, got)
	}
	if strings.Contains(got, "no seats:") {
		t.Errorf("a town with a closed seat was called seatless:\n%s", got)
	}
}

// show prints the town's total beside the seats, because it is the cap in force
// when the seats each have room (gt-bcpj5). A key the file does not carry, and a
// written 0, are both no cap.
func TestPoolShowPrintsTheTownTotal(t *testing.T) {
	t.Parallel()
	const agents = `"agents":{"claude-opus":{"command":"claude"}}`
	root := poolTestTown(t, `{"type":"town-settings","version":1,`+agents+`,"polecat_pool":{"seats":{"claude-opus":{"cap":3}},"max_total":4}}`, "")
	got := poolTestShowAt(t, root)
	if line := poolTestShowLine(got, "total"); line != "total cap 4" {
		t.Errorf("show total line = %q, want the cap the dispatcher and sling read:\n%s", line, got)
	}
	if !strings.Contains(got, "max_total") {
		t.Errorf("show did not name the key it read:\n%s", got)
	}

	for _, settings := range []string{
		`{"type":"town-settings","version":1,` + agents + `,"polecat_pool":{"seats":{"claude-opus":{"cap":3}}}}`,
		`{"type":"town-settings","version":1,` + agents + `,"polecat_pool":{"seats":{"claude-opus":{"cap":3}},"max_total":0}}`,
		`{"type":"town-settings","version":1}`,
	} {
		root := poolTestTown(t, settings, "")
		if line := poolTestShowLine(poolTestShowAt(t, root), "total"); line != "total no cap" {
			t.Errorf("show of %s: total line = %q, want no cap", settings, line)
		}
	}
}

// set-total writes polecat_pool.max_total, which the dispatcher's next tick
// reads, and 0 removes the key: an absent max_total is no cap and a written 0
// would say the same, so a zero is never left on the page (gt-bcpj5).
func TestPoolSetTotalSetsAndRemovesTheCap(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, "")

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
	for key, want := range map[string]any{"seats": map[string]any{"claude-opus": map[string]any{"cap": float64(3)}}, "min_spawn_gap": "4m"} {
		if !reflect.DeepEqual(pool[key], want) {
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
	root := poolTestTown(t, poolTestSettings, "")
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
	root := poolTestTown(t, `{"type":"town-settings",`, "")
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

// show marks a pinned-only seat, and set changes a cap without dropping the key
// (gt-zor86).
func TestPoolPinnedOnlySeatShowsAndSurvivesSet(t *testing.T) {
	t.Parallel()
	settings := `{
  "type": "town-settings",
  "version": 1,
  "agents": {"claude-sonnet": {"command": "claude"}, "deepseek-flash": {"command": "claude"}},
  "polecat_pool": {"seats": {
    "claude-sonnet": {"cap": 2},
    "deepseek-flash": {"cap": 1, "pinned_only": true}
  }}
}`
	root := poolTestTown(t, settings, "")
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPoolSeatMapShow(&out, root, ts, time.Now(), poolShowLive{ok: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deepseek-flash  0/1  open, pinned-only", "claude-sonnet   0/2  open\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show output lacks %q:\n%s", want, out.String())
		}
	}

	if err := runPoolSetSeats(&bytes.Buffer{}, root, []poolSeatCap{{Agent: "claude-sonnet", Cap: 3}}, nil, 0, false); err != nil {
		t.Fatal(err)
	}
	after, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if seat := after.PolecatPool.Seats["deepseek-flash"]; !seat.PinnedOnly || seat.Cap != 1 {
		t.Errorf("gt pool set changed the pinned-only seat: %+v", seat)
	}
	if seat := after.PolecatPool.Seats["claude-sonnet"]; seat.Cap != 3 || seat.PinnedOnly {
		t.Errorf("gt pool set did not change claude-sonnet alone: %+v", seat)
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

// A file that still holds the two-seat keys beside, or instead of, a seat map
// does not load, so show refuses it and names the replacement and the command
// that converts it, rather than printing a table the dispatcher would not run
// (gt-mukgk).
func TestPoolShowRefusesAFileCarryingRetiredKeys(t *testing.T) {
	t.Parallel()
	settings := `{"type":"town-settings","version":1,` +
		`"agents":{"claude-sonnet":{"command":"claude"}},` +
		`"polecat_pool":{"agent":"claude-opus","max_seats":9,"seats":{"claude-sonnet":{"cap":2}}}}`
	root := poolTestTown(t, settings, "")
	var out bytes.Buffer
	err := runPoolShow(&out, root)
	if err == nil {
		t.Fatalf("show printed a table for a file with retired keys:\n%s", out.String())
	}
	for _, want := range []string{"polecat_pool.agent was replaced by", "polecat_pool.max_seats was replaced by", "run gt pool migrate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to contain %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("a refused show printed:\n%s", out.String())
	}
}

// show prints the seats the dispatcher runs, in its order. The matrix is the
// states a map can be in — absent, one seat, several seats at their own caps,
// one shut by a cap of 0 and one by its window — and each one has to come out
// naming the agents the dispatcher spends caps on, or the picture is a second
// opinion (gt-riazi).
func TestPoolShowListsTheSeatsTheDispatcherRuns(t *testing.T) {
	t.Parallel()
	towns := []struct {
		name     string
		settings string
	}{
		{"no section", `{"type":"town-settings","version":1}`},
		{"an empty map", `{"type":"town-settings","version":1,"polecat_pool":{"seats":{}}}`},
		{"one seat", poolTestSettings},
		{"several seats, one shut by its window", poolSeatMapSettings},
		{"a seat shut by a cap of 0",
			`{"type":"town-settings","version":1,"agents":{"claude-haiku":{"command":"claude"},"claude-sonnet":{"command":"claude"}},` +
				`"polecat_pool":{"seats":{"claude-sonnet":{"cap":2},"claude-haiku":{"cap":0}}}}`},
	}
	for _, tc := range towns {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolTestTown(t, tc.settings, "")
			out := poolTestShowAt(t, root)
			seats := poolTestSeatList(t, root)
			if len(seats) == 0 {
				if !strings.Contains(out, "no seats:") {
					t.Errorf("no seat is configured and show did not say so:\n%s", out)
				}
				return
			}
			want := make([]string, 0, len(seats))
			for _, s := range seats {
				want = append(want, s.Agent)
			}
			if got := poolTestShownSeats(t, out); !slices.Equal(got, want) {
				t.Errorf("show lists %v, want the dispatcher's seats %v:\n%s", got, want, out)
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
	root := poolTestTown(t, `{"type":"town-settings","version":1,"agents":{"deepseek-flash":{"command":"claude"}},`+
		`"polecat_pool":{"seats":{"deepseek-flash":{"cap":3}},"max_total":4}}`, "")
	writeTestRigsConfig(t, root, "gastown")
	writeTownPolecatDir(t, root, "gastown", "ember")
	reads := poolSeatReadsAnswering(t, deferredExitFields(), assignedActiveWork("gt-held"))

	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	live := poolLiveForWith(root, func(pool *config.PolecatPool) ([]poolSession, error) {
		return poolSeatSessionsWith(&fakeLister{}, root, poolDispositionAnswering(root, reads), nil, pool)
	})

	var out bytes.Buffer
	if err := runPoolSeatMapShow(&out, root, ts, poolTestNow, live); err != nil {
		t.Fatal(err)
	}
	for first, want := range map[string]string{
		"deepseek-flash": "deepseek-flash 1/3 (1 dead-hooked) open",
		"total":          "total 1/4",
	} {
		if got := poolTestShowLine(out.String(), first); got != want {
			t.Errorf("show line for %s = %q, want %q:\n%s", first, got, want, out.String())
		}
	}
}

// TestPoolShowAndTheTickAgreeOnAMixedPoolsSessionlessSeat is gt-2q283 through
// both readers of one seat picture: `gt pool show`'s page and the spec
// dispatcher's tick line are built from the same count (poolSeatSessions →
// specRosterFrom), so the counts an operator reads on the page are the counts
// the dispatcher refuses on.
//
// The town is the mixed pool of 2026-10-14: claude-sonnet with two seats,
// deepseek-flash with two, and a flash seat's session is gone with its bead
// still hooked. On 11:34 the tick line read claude-sonnet 4/2 (1 dead-hooked),
// deepseek-flash 0/2 — the crashed seat counted on the wrong agent, so one seat
// read full with two Sonnet sessions in the town and the other read empty.
func TestPoolShowAndTheTickAgreeOnAMixedPoolsSessionlessSeat(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, mixedPoolTestSettings, "")
	writeTestRigsConfig(t, root, "gastown")
	writeTownPolecatDir(t, root, "gastown", "ember")
	// The seat was slung on the flash seat's agent, and its session is gone:
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
	live := poolLiveForWith(root, func(pool *config.PolecatPool) ([]poolSession, error) {
		return poolSeatSessionsWith(&fakeLister{}, root, poolDispositionAnswering(root, reads), nil, pool)
	})
	var out bytes.Buffer
	if err := runPoolSeatMapShow(&out, root, ts, poolTestNow, live); err != nil {
		t.Fatal(err)
	}
	page := out.String()

	// The crash is on the seat it was slung on, in both readings, and the other
	// seat is empty because nothing live sits on it.
	for _, want := range []string{"claude-sonnet 0/2", "deepseek-flash 1/2 (1 dead-hooked)"} {
		if !strings.Contains(budget.Picture(), want) {
			t.Errorf("the tick line reads %q, want it to name %q", budget.Picture(), want)
		}
	}
	for _, seat := range budget.Seats {
		line := poolTestShowLine(page, seat.Agent)
		if line == "" {
			t.Fatalf("the page prints no line for seat %s:\n%s", seat.Agent, page)
		}
		if got := fmt.Sprintf("%s %d/%d", seat.Agent, seat.Live, seat.Cap); !strings.HasPrefix(line, got) {
			t.Errorf("the page reads %q for seat %s, the tick line counts it %q:\n%s", line, seat.Agent, got, page)
		}
	}
	// The town's total is the one number both readings already agreed on, and
	// it is not what changed: one polecat is one polecat.
	if got := poolTestShowLine(page, "total"); got != "total 1/4" {
		t.Errorf("the page's total = %q, want the crashed seat counted once (total 1/4):\n%s", got, page)
	}
}

// mixedPoolTestSettings is the 2026-10-14 town: two seats of two, and the town
// caps every seat together at four.
const mixedPoolTestSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "claude-sonnet": {"command": "claude"},
    "deepseek-flash": {"command": "claude"}
  },
  "polecat_pool": {
    "seats": {"claude-sonnet": {"cap": 2}, "deepseek-flash": {"cap": 2}},
    "max_total": 4
  }
}
`

// TestPoolShowReadsNoSeatPictureWithoutARigRegistry pins the guard that keeps
// the page off the tmux server where there is nothing to count: a town whose rig
// registry is missing names the caps alone, with no live column, rather than a
// partial count (the unit tier starts no tmux).
func TestPoolShowReadsNoSeatPictureWithoutARigRegistry(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, "")
	out := poolTestShowAt(t, root)
	if got := poolTestShowLine(out, "claude-opus"); got != "claude-opus cap 3 open" {
		t.Errorf("a town with no rigs has no polecats to count: seat line = %q\n%s", got, out)
	}
}
