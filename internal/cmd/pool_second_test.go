package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// poolSecondSettings is a town whose pool seat runs claude-sonnet and whose
// settings define deepseek-flash as a custom agent, the way the real town does:
// the agent the verb is for is not a built-in preset.
const poolSecondSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {"claude-sonnet": {"command": "claude"}, "deepseek-flash": {"command": "claude"}},
  "polecat_pool": {"agent": "claude-sonnet", "max_seats": 2, "max_total": 4}
}
`

func poolTestSetSecond(root, agent string, n int) (string, error) {
	var out bytes.Buffer
	err := runPoolSetSecond(&out, root, agent, n)
	return out.String(), err
}

func poolTestShow(t *testing.T, root string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runPoolShow(&out, root); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestPoolSetSecondWritesAgentAndCapAndSaysWhenItLands(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, poolTestDaemon)
	out, err := poolTestSetSecond(root, "deepseek-flash", 2)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	if sd == nil || sd.HookedAgent != "deepseek-flash" || sd.MaxHooked != 2 {
		t.Errorf("spec_dispatch = %+v, want hooked_agent deepseek-flash and max_hooked 2", sd)
	}
	if raw, _ := os.ReadFile(config.DaemonPatrolConfigPath(root)); (sd.Enabled == nil || !*sd.Enabled) || !strings.Contains(string(raw), `"hookless_agent": "old"`) {
		t.Errorf("spec_dispatch = %+v, want the keys set-second does not own left as they were", sd)
	}
	if got := poolTestBudget(t, root); got["deepseek-flash"] != 2 || got["claude-sonnet"] != 2 {
		t.Errorf("dispatcher seats = %v, want claude-sonnet 2 and deepseek-flash 2", got)
	}
	// The write lands at the next tick and asks for no restart (gt-jdnab).
	if !strings.Contains(out, "second seat: deepseek-flash cap 2") || !strings.Contains(out, "next tick") {
		t.Errorf("output lacks the change or when it lands:\n%s", out)
	}
	if strings.Contains(out, "kickstart") || strings.Contains(out, "daemon start") {
		t.Errorf("the write still asks for a daemon kickstart:\n%s", out)
	}
}

func TestPoolSetSecondRefusesAnUnknownAgentAndNamesThePresets(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, poolTestDaemon)
	_, err := poolTestSetSecond(root, "deepseek-flsh", 2)
	if err == nil {
		t.Fatal("an unknown agent was accepted")
	}
	for _, want := range []string{`"deepseek-flsh"`, "known presets", "claude", "deepseek-flash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if got, _ := os.ReadFile(config.DaemonPatrolConfigPath(root)); string(got) != poolTestDaemon {
		t.Errorf("a refused agent changed the patrol config:\n%s", got)
	}
}

func TestPoolSetSecondRefusesACapAboveMaxTotal(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, poolTestDaemon)
	_, err := poolTestSetSecond(root, "deepseek-flash", 5)
	if err == nil || !strings.Contains(err.Error(), "max_total 4") {
		t.Fatalf("cap 5 over max_total 4: err = %v, want a refusal naming max_total 4", err)
	}
	if got, _ := os.ReadFile(config.DaemonPatrolConfigPath(root)); string(got) != poolTestDaemon {
		t.Errorf("a refused cap changed the patrol config:\n%s", got)
	}
	if _, err := poolTestSetSecond(root, "deepseek-flash", 4); err != nil {
		t.Errorf("a cap equal to max_total was refused: %v", err)
	}
}

func TestPoolSetSecondLeavesAnyCapWhenThereIsNoTotal(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus","max_seats":3}}`, poolTestDaemon)
	if _, err := poolTestSetSecond(root, "claude", 9); err != nil {
		t.Errorf("a town with no max_total refused cap 9: %v", err)
	}
}

func TestPoolSetSecondBacksUpTheFileItReplaces(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, poolTestDaemon)
	if _, err := poolTestSetSecond(root, "deepseek-flash", 2); err != nil {
		t.Fatal(err)
	}
	baks, _ := filepath.Glob(config.DaemonPatrolConfigPath(root) + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("patrol config backups = %v, want one", baks)
	}
	if got, _ := os.ReadFile(baks[0]); string(got) != poolTestDaemon {
		t.Errorf("backup = %q, want the file as it was before the change", got)
	}
}

func TestPoolSetSecondBacksUpTheHostWhenDaemonConfigLivesInSettings(t *testing.T) {
	t.Parallel()
	settings := `{"type":"town-settings","version":1,"agents":{"deepseek-flash":{"command":"claude"}},` +
		`"polecat_pool":{"agent":"claude-sonnet","max_seats":2},` +
		`"daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true,"max_hooked":1}}}}` + "\n"
	root := poolTestTown(t, settings, "")
	if _, err := poolTestSetSecond(root, "deepseek-flash", 2); err != nil {
		t.Fatal(err)
	}
	if got := poolTestBudget(t, root)["deepseek-flash"]; got != 2 {
		t.Errorf("second seat cap = %d, want 2", got)
	}
	baks, _ := filepath.Glob(config.TownSettingsPath(root) + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("settings backups = %v, want one", baks)
	}
	if got, _ := os.ReadFile(baks[0]); string(got) != settings {
		t.Errorf("backup = %q, want the settings file as it was", got)
	}
}

func TestPoolSetSecondOpensTheSeatWithoutSwitchingTheDispatcherOff(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, "")
	if _, err := poolTestSetSecond(root, "deepseek-flash", 2); err != nil {
		t.Fatal(err)
	}
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	if sd == nil || (sd.Enabled == nil || !*sd.Enabled) || sd.HookedAgent != "deepseek-flash" || sd.MaxHooked != 2 {
		t.Errorf("spec_dispatch = %+v, want enabled, deepseek-flash, max_hooked 2", sd)
	}
}

func TestPoolSetSecondZeroClosesTheSeatAndKeepsTheAgent(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, poolTestDaemon)
	out, err := poolTestSetSecond(root, "deepseek-flash", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "closed the second seat (deepseek-flash)") {
		t.Errorf("output does not say the seat closed:\n%s", out)
	}
	sd, _ := loadPoolSpecDispatch(root)
	if sd == nil || sd.HookedAgent != "deepseek-flash" || sd.MaxHooked != 0 {
		t.Errorf("spec_dispatch = %+v, want the agent recorded and the seat closed", sd)
	}
	if _, open := poolTestBudget(t, root)["deepseek-flash"]; open {
		t.Error("a closed second seat is still in the dispatcher's seats")
	}
}

func TestPoolSetSecondRefusesAFileItCannotParse(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, `{"patrols": `)
	if _, err := poolTestSetSecond(root, "deepseek-flash", 2); err == nil {
		t.Fatal("a patrol config that does not parse was overwritten")
	}
}

// Two seats on one agent: the dispatcher keeps one seat per agent, so show names
// the second seat and what holds its agent instead of dropping the line or
// printing it as the pool seat; set-second warns at the write.
func TestPoolShowLabelsTheSecondSeatWhenBothRunOneAgent(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, poolTestDaemon) // pool and second both claude-sonnet
	show := poolTestShow(t, root)

	var pool, second string
	for _, line := range strings.Split(show, "\n") {
		switch {
		case strings.HasPrefix(line, "pool seat"):
			pool = line
		case strings.HasPrefix(line, "second seat"):
			second = line
		}
	}
	if !strings.Contains(pool, "claude-sonnet") || strings.Contains(pool, "not run") {
		t.Errorf("pool seat line = %q, want the running seat", pool)
	}
	if !strings.Contains(second, "claude-sonnet") || !strings.Contains(second, "not run: the pool seat already holds claude-sonnet") {
		t.Errorf("second seat line = %q, want it labeled and named as not run behind the pool seat\n%s", second, show)
	}

	out, err := poolTestSetSecond(root, "claude-sonnet", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: the pool seat already runs claude-sonnet") {
		t.Errorf("set-second onto the pool's agent did not warn:\n%s", out)
	}
}

func TestPoolShowLabelsPoolAndSecondSeatsOnDifferentAgents(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, poolTestDaemon)
	if _, err := poolTestSetSecond(root, "deepseek-flash", 2); err != nil {
		t.Fatal(err)
	}
	show := poolTestShow(t, root)
	for _, want := range []string{"pool seat    claude-sonnet  cap 2", "second seat  deepseek-flash  cap 2"} {
		if !strings.Contains(show, want) {
			t.Errorf("show lacks %q:\n%s", want, show)
		}
	}
	if strings.Contains(show, "not run") {
		t.Errorf("show calls a running seat not run:\n%s", show)
	}
}
