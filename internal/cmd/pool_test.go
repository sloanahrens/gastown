package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
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
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	caps := map[string]int{}
	for _, s := range specBudgetFromConfig(ts, sd).Seats {
		caps[s.Agent] = s.Cap
	}
	return caps
}

func poolTestSet(t *testing.T, root, agent string, n int) string {
	t.Helper()
	var out bytes.Buffer
	if err := runPoolSet(&out, root, agent, n); err != nil {
		t.Fatalf("set %s %d: %v", agent, n, err)
	}
	return out.String()
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
		"daemon.patrols.spec_dispatch.hooked_agent", "max_hooked",
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

func TestPoolSetChangesThePoolSeatCap(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	out := poolTestSet(t, root, "claude-opus", 5)
	if got := poolTestBudget(t, root)["claude-opus"]; got != 5 {
		t.Errorf("pool seat cap = %d, want 5", got)
	}
	if strings.Contains(out, "daemon") || strings.Contains(out, "kickstart") {
		t.Errorf("a pool seat change mentioned a restart:\n%s", out)
	}
}

func TestPoolSetChangesTheSecondSeatCapAndSaysToKickstart(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	out := poolTestSet(t, root, "claude-sonnet", 4)
	if got := poolTestBudget(t, root)["claude-sonnet"]; got != 4 {
		t.Errorf("second seat cap = %d, want 4", got)
	}
	if !strings.Contains(out, "read at daemon start") || !strings.Contains(out, "kickstart") {
		t.Errorf("a second seat change did not say a kickstart is needed:\n%s", out)
	}
}

func TestPoolSetOpensThePoolSeatWhenThePoolHasNone(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1}`, poolTestDaemon)
	poolTestSet(t, root, "claude-haiku", 2)
	caps := poolTestBudget(t, root)
	if caps["claude-haiku"] != 2 {
		t.Errorf("budget = %v, want claude-haiku at 2", caps)
	}
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if ts.PolecatPool.Agent != "claude-haiku" || ts.PolecatPool.MaxSeats != 2 {
		t.Errorf("polecat_pool = %+v, want agent claude-haiku max_seats 2", ts.PolecatPool)
	}
}

func TestPoolSetRefusesAnAgentHoldingNoSeat(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	before, _ := os.ReadFile(config.TownSettingsPath(root))
	for _, n := range []int{3, 0} {
		err := runPoolSet(&bytes.Buffer{}, root, "deepseek", n)
		if err == nil {
			t.Fatalf("set deepseek %d: want a refusal", n)
		}
		for _, want := range []string{"claude-opus", "claude-sonnet"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("set deepseek %d: refusal %q does not name %s", n, err, want)
			}
		}
	}
	after, _ := os.ReadFile(config.TownSettingsPath(root))
	if string(before) != string(after) {
		t.Error("a refused set changed the settings file")
	}
}

func TestPoolSetZeroClosesThePoolSeat(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	poolTestSet(t, root, "claude-opus", 0)
	if cap, ok := poolTestBudget(t, root)["claude-opus"]; ok {
		t.Errorf("budget still has a claude-opus seat at %d", cap)
	}
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
	for _, key := range []string{"agent", "max_seats"} {
		if v, ok := doc.PolecatPool[key]; ok {
			t.Errorf("polecat_pool.%s = %v was written, want the key removed", key, v)
		}
	}
	if doc.PolecatPool["min_spawn_gap"] != "4m" {
		t.Errorf("closing the seat dropped min_spawn_gap: %v", doc.PolecatPool)
	}
}

// A seat a file spells with the retired keys must close for real: the loader
// folds overflow_agent into agent, so the thread it just cut has to leave the
// file, or the next load hangs it again (gt-aq7zo).
func TestPoolSetZeroClosesASeatSpelledWithTheRetiredKeys(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestRetiredSeat, poolTestDaemon)
	poolTestSet(t, root, "codex", 0)

	pool := poolTestPoolSection(t, root)
	for _, key := range []string{"agent", "max_seats", "overflow_agent", "max_overflow"} {
		if v, ok := pool[key]; ok {
			t.Errorf("polecat_pool.%s = %v was left behind, want the key gone", key, v)
		}
	}
	if pool["min_spawn_gap"] != "4m" {
		t.Errorf("closing the seat dropped min_spawn_gap: %v", pool)
	}
	v, err := loadPoolSeats(root)
	if err != nil {
		t.Fatal(err)
	}
	if v.poolAgent != "" {
		t.Errorf("the pool seat still runs %s", v.poolAgent)
	}
	if cap, ok := poolTestBudget(t, root)["codex"]; ok {
		t.Errorf("the dispatcher still has a codex seat at %d", cap)
	}
}

// A cap set on a seat the file spells with the retired keys lands on the
// current keys, so what the file holds is what the loader reads (gt-aq7zo).
func TestPoolSetOpensASeatSpelledWithTheRetiredKeys(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestRetiredSeat, poolTestDaemon)
	out := poolTestSet(t, root, "codex", 5)
	if strings.Contains(out, "kickstart") {
		t.Errorf("a pool seat change mentioned a restart:\n%s", out)
	}

	pool := poolTestPoolSection(t, root)
	if pool["agent"] != "codex" || pool["max_seats"] != float64(5) {
		t.Errorf("polecat_pool = %v, want agent codex and max_seats 5", pool)
	}
	for _, key := range []string{"overflow_agent", "max_overflow"} {
		if v, ok := pool[key]; ok {
			t.Errorf("polecat_pool.%s = %v was left behind, want the key gone", key, v)
		}
	}
	v, err := loadPoolSeats(root)
	if err != nil {
		t.Fatal(err)
	}
	if v.poolAgent != "codex" || v.poolCap != 5 {
		t.Errorf("loadPoolSeats = %s cap %d, want codex cap 5", v.poolAgent, v.poolCap)
	}
}

// A cap that does not move is still written under the current name: the file
// held the value as max_overflow, so a write that only dropped the retired
// spelling would leave the cap on the page with no current key holding it
// (gt-aq7zo).
func TestPoolSetWritesTheCurrentKeysWhenTheCapDoesNotMove(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestRetiredSeat, poolTestDaemon)
	poolTestSet(t, root, "codex", 3)

	pool := poolTestPoolSection(t, root)
	if pool["agent"] != "codex" || pool["max_seats"] != float64(3) {
		t.Errorf("polecat_pool = %v, want agent codex and max_seats 3", pool)
	}
	for _, key := range []string{"overflow_agent", "max_overflow"} {
		if v, ok := pool[key]; ok {
			t.Errorf("polecat_pool.%s = %v was left behind, want the key gone", key, v)
		}
	}
}

func TestPoolSetZeroClosesTheSecondSeat(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	poolTestSet(t, root, "claude-sonnet", 0)
	if cap, ok := poolTestBudget(t, root)["claude-sonnet"]; ok {
		t.Errorf("budget still has a claude-sonnet seat at %d", cap)
	}
	if got := poolTestBudget(t, root)["claude-opus"]; got != 3 {
		t.Errorf("closing the second seat moved the pool seat to %d", got)
	}
	raw, _ := os.ReadFile(config.DaemonPatrolConfigPath(root))
	if strings.Contains(string(raw), "max_hooked") {
		t.Errorf("max_hooked was written back:\n%s", raw)
	}
}

func TestPoolSetSwapsSeatsTheWayTheIncidentDid(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t,
		`{"type":"town-settings","version":1,"polecat_pool":{"agent":"deepseek","max_seats":4}}`,
		`{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true,"hooked_agent":"claude-sonnet"}}}`)
	poolTestSet(t, root, "deepseek", 0)
	poolTestSet(t, root, "claude-sonnet", 3)
	caps := poolTestBudget(t, root)
	if _, ok := caps["deepseek"]; ok || caps["claude-sonnet"] != 3 {
		t.Errorf("budget = %v, want claude-sonnet at 3 and no deepseek seat", caps)
	}
}

func TestPoolSetKeepsEveryKeyItDoesNotOwn(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	poolTestSet(t, root, "claude-opus", 5)
	poolTestSet(t, root, "claude-sonnet", 4)

	settings, _ := os.ReadFile(config.TownSettingsPath(root))
	for _, want := range []string{`"feed_curator"`, `"min_spawn_gap": "4m"`, `"local_agent": "retired-local"`, `"max_seats": 5`} {
		if !strings.Contains(string(settings), want) {
			t.Errorf("settings lost %s:\n%s", want, settings)
		}
	}
	if err := json.Unmarshal(settings, new(map[string]json.RawMessage)); err != nil {
		t.Fatalf("settings no longer parse: %v", err)
	}

	daemon, _ := os.ReadFile(config.DaemonPatrolConfigPath(root))
	for _, want := range []string{`"steward"`, `"mode": "shadow"`, `"hookless_agent": "old"`, `"interval": "30s"`, `"max_hooked": 4`} {
		if !strings.Contains(string(daemon), want) {
			t.Errorf("daemon config lost %s:\n%s", want, daemon)
		}
	}
}

func TestPoolSetLeavesATimestampedBackupOfTheFileItReplaces(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, poolTestDaemon)
	poolTestSet(t, root, "claude-opus", 5)
	poolTestSet(t, root, "claude-sonnet", 4)
	for path, original := range map[string]string{
		config.TownSettingsPath(root):       poolTestSettings,
		config.DaemonPatrolConfigPath(root): poolTestDaemon,
	} {
		baks, _ := filepath.Glob(path + ".bak-*")
		if len(baks) != 1 {
			t.Fatalf("%s backups = %v, want one", filepath.Base(path), baks)
		}
		got, _ := os.ReadFile(baks[0])
		if string(got) != original {
			t.Errorf("%s holds %q, want the file as it was before the change", baks[0], got)
		}
	}
}

func TestPoolSetBacksUpTheHostWhenDaemonConfigLivesInSettings(t *testing.T) {
	t.Parallel()
	settings := `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus","max_seats":3},` +
		`"daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true,"max_hooked":2}}}}` + "\n"
	root := poolTestTown(t, settings, "")
	poolTestSet(t, root, "claude-sonnet", 6)
	if got := poolTestBudget(t, root)["claude-sonnet"]; got != 6 {
		t.Errorf("second seat cap = %d, want 6", got)
	}
	baks, _ := filepath.Glob(config.TownSettingsPath(root) + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("settings backups = %v, want one", baks)
	}
	if got, _ := os.ReadFile(baks[0]); string(got) != settings {
		t.Errorf("backup = %q, want the settings file as it was", got)
	}
}

func TestPoolSetOpensTheSecondSeatWithoutSwitchingTheDispatcherOff(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTestSettings, "")
	poolTestSet(t, root, "claude-sonnet", 2)
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	if sd == nil || !sd.Enabled || sd.MaxHooked != 2 {
		t.Errorf("spec_dispatch = %+v, want enabled with max_hooked 2", sd)
	}
}

func TestPoolSetRefusesAFileItCannotParse(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings",`, poolTestDaemon)
	if err := runPoolSet(&bytes.Buffer{}, root, "claude-opus", 2); err == nil {
		t.Fatal("want an error for settings that do not parse")
	}
	got, _ := os.ReadFile(config.TownSettingsPath(root))
	if string(got) != `{"type":"town-settings",` {
		t.Errorf("a file that did not parse was rewritten: %q", got)
	}
}
