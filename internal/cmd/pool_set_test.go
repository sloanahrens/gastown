package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// poolRoleSettings is a seat-map town that also names role_agents.polecat, the
// key the retired gt pool mix used to write beside the seats.
const poolRoleSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "claude-sonnet": {"command": "claude"},
    "claude-haiku": {"command": "claude"}
  },
  "role_agents": {"polecat": "claude-sonnet", "witness": "claude-haiku"},
  "polecat_pool": {"seats": {"claude-sonnet": {"cap": 2}}}
}
`

const poolModelTable = `{"roles": {"polecat": "sonnet-profile"}}
`

// gt pool set writes polecat_pool alone: role_agents.polecat and the models
// table the retired mix used to write stay as they were, because the seat map
// is the source of truth for the polecat's model (gt-mukgk).
func TestPoolSetLeavesRoleAgentsAndTheModelTableAlone(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolRoleSettings, "")
	modelsPath := filepath.Join(root, "settings", "models.json")
	if err := os.WriteFile(modelsPath, []byte(poolModelTable), 0o600); err != nil {
		t.Fatal(err)
	}

	poolTestSetSeats(t, root, []poolSeatCap{{Agent: "claude-haiku", Cap: 1}}, nil, 3, true)

	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if ts.RoleAgents["polecat"] != "claude-sonnet" || ts.RoleAgents["witness"] != "claude-haiku" {
		t.Errorf("role_agents = %v, want both entries as they were", ts.RoleAgents)
	}
	if got, err := os.ReadFile(modelsPath); err != nil || string(got) != poolModelTable {
		t.Errorf("settings/models.json = %q (%v), want it untouched", got, err)
	}
}

// Naming a seat in both the set pairs and --remove, or giving --remove an empty
// name, is refused before the file is read for a write (gt-mukgk).
func TestPoolSetRefusesContradictoryOrEmptyRemoval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		changes []poolSeatCap
		remove  []string
		want    string
	}{
		{"a seat both set and removed", []poolSeatCap{{Agent: "claude-sonnet", Cap: 1}}, []string{"claude-sonnet"}, "both set and removed"},
		{"an empty removal", nil, []string{""}, "agent name is empty"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolTestTown(t, poolSeatMapSettings, "")
			before := poolTestRawSettings(t, root)
			err := runPoolSetSeats(&bytes.Buffer{}, root, tc.changes, tc.remove, 0, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("set = %v, want a refusal containing %q", err, tc.want)
			}
			if after := poolTestRawSettings(t, root); after != before {
				t.Error("a refused set rewrote the file")
			}
		})
	}
}

// With no two-seat keys left to fall back on, removing every seat is allowed:
// the guard is for a file whose old keys would take dispatch back (gt-mukgk).
func TestPoolSetRemovesEverySeatWhenNoTwoSeatKeysRun(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, poolTwoSeatDaemonWithoutSeats)

	poolTestSetSeats(t, root, nil, []string{"claude-sonnet", "deepseek-flash"}, 0, false)
	if caps := poolTestSeatCaps(t, root); len(caps) != 0 {
		t.Errorf("seats after removing both = %v, want none", caps)
	}
}

// The guard also fires on each way the daemon file can still run a second seat,
// and on the pool seat key alone (gt-mukgk).
func TestPoolSetRemoveGuardNamesEachLegacyKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		settings string
		daemon   string
	}{
		{"polecat_pool.agent", poolLegacyPoolAgentOverMap, ""},
		{"spec_dispatch.hooked_agent", poolSeatMapSettings,
			`{"type": "daemon-patrol-config", "version": 1, "patrols": {"spec_dispatch": {"enabled": true, "hooked_agent": "claude-sonnet"}}}`},
		{"spec_dispatch.max_hooked", poolSeatMapSettings,
			`{"type": "daemon-patrol-config", "version": 1, "patrols": {"spec_dispatch": {"enabled": true, "max_hooked": 1}}}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolTestTown(t, tc.settings, tc.daemon)
			before := poolTestRawSettings(t, root)
			err := runPoolSetSeats(&bytes.Buffer{}, root, nil, []string{"claude-sonnet", "deepseek-flash"}, 0, false)
			if err == nil || !strings.Contains(err.Error(), "cap to 0") {
				t.Fatalf("removing every seat = %v, want the two-seat-key refusal naming the cap-0 replacement", err)
			}
			if after := poolTestRawSettings(t, root); after != before {
				t.Error("a refused remove rewrote the file")
			}
		})
	}
}

const poolTwoSeatDaemonWithoutSeats = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {"spec_dispatch": {"enabled": true, "interval": "30s"}}
}
`

// poolLegacyPoolAgentOverMap is a map town that kept polecat_pool.agent, which
// an emptied map would hand dispatch back to.
const poolLegacyPoolAgentOverMap = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "claude-sonnet": {"command": "claude"},
    "claude-opus": {"command": "claude"},
    "deepseek-flash": {"command": "claude"}
  },
  "polecat_pool": {
    "agent": "claude-opus",
    "timezone": "America/Chicago",
    "seats": {
      "claude-sonnet": {"cap": 2},
      "deepseek-flash": {"cap": 3}
    }
  }
}
`

func TestPoolLegacySeatsSet(t *testing.T) {
	t.Parallel()
	pool := func(agent string) *config.TownSettings {
		return &config.TownSettings{PolecatPool: &config.PolecatPool{Agent: agent}}
	}
	for _, tc := range []struct {
		name string
		ts   *config.TownSettings
		sd   *config.SpecDispatchConfig
		want bool
	}{
		{"no settings, no patrol block", nil, nil, false},
		{"a pool with neither key", pool(""), &config.SpecDispatchConfig{}, false},
		{"polecat_pool.agent", pool("claude-opus"), nil, true},
		{"spec_dispatch.hooked_agent", pool(""), &config.SpecDispatchConfig{HookedAgent: "claude-sonnet"}, true},
		{"spec_dispatch.max_hooked", pool(""), &config.SpecDispatchConfig{MaxHooked: 1}, true},
	} {
		if got := poolLegacySeatsSet(tc.ts, tc.sd); got != tc.want {
			t.Errorf("%s: poolLegacySeatsSet = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The first set on a two-seat town says it seeded the map, so the operator
// learns the old keys no longer decide dispatch; a set on a town that already
// has a map says nothing of the kind (gt-mukgk).
func TestPoolSetSaysWhenItSeededTheMap(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolTwoSeatSettings, poolTwoSeatDaemon)
	out := poolTestSetSeats(t, root, []poolSeatCap{{Agent: "claude-haiku", Cap: 2}}, nil, 0, false)
	if !strings.Contains(out, "seeded polecat_pool.seats from the two-seat keys (claude-opus, claude-sonnet)") {
		t.Errorf("first set output lacks the seeding notice:\n%s", out)
	}
	out = poolTestSetSeats(t, root, []poolSeatCap{{Agent: "claude-haiku", Cap: 1}}, nil, 0, false)
	if strings.Contains(out, "seeded") {
		t.Errorf("a set on an existing map reports a seeding:\n%s", out)
	}
}

// Once the map exists the dispatcher ignores the second seat, so set-second
// refuses rather than write a key that changes nothing, and names the
// replacement (gt-mukgk).
func TestPoolSetSecondRefusesOnASeatMap(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, poolTwoSeatDaemon)
	before := poolTestRawSettings(t, root)
	daemonBefore, err := os.ReadFile(config.DaemonPatrolConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}

	err = runPoolSetSecond(&bytes.Buffer{}, root, "claude-haiku", 2)
	if err == nil || !strings.Contains(err.Error(), "gt pool set claude-haiku=2") {
		t.Fatalf("set-second on a seat map = %v, want a refusal naming gt pool set claude-haiku=2", err)
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a refused set-second rewrote the settings")
	}
	if daemonAfter, _ := os.ReadFile(config.DaemonPatrolConfigPath(root)); string(daemonAfter) != string(daemonBefore) {
		t.Error("a refused set-second rewrote the patrol config")
	}
}
