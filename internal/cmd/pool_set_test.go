package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// poolRoleSettings is a seat-map town that also names role_agents for other
// roles: the keys gt pool does not own and must leave as they are.
const poolRoleSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "claude-sonnet": {"command": "claude"},
    "claude-haiku": {"command": "claude"}
  },
  "role_agents": {"crew": "claude-sonnet", "witness": "claude-haiku"},
  "polecat_pool": {"seats": {"claude-sonnet": {"cap": 2}}}
}
`

const poolModelTable = `{"roles": {"crew": "sonnet-profile"}}
`

// gt pool set writes polecat_pool alone: role_agents and the models table stay
// as they were, because the seat map is the source of truth for the polecat's
// model (gt-mukgk).
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
	if ts.RoleAgents["crew"] != "claude-sonnet" || ts.RoleAgents["witness"] != "claude-haiku" || len(ts.RoleAgents) != 2 {
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

// Removing every seat is allowed: an empty map is a map with no seats, so the
// dispatcher spawns nothing rather than falling back to anything (gt-mukgk).
func TestPoolSetRemovesEverySeat(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")

	poolTestSetSeats(t, root, nil, []string{"claude-sonnet", "deepseek-flash"}, 0, false)
	if caps := poolTestSeatCaps(t, root); len(caps) != 0 {
		t.Errorf("seats after removing both = %v, want none", caps)
	}
	if got := poolTestTownBudget(t, root).Picture(); got != "no seats" {
		t.Errorf("the dispatcher's budget after removing every seat = %q, want no seats", got)
	}
}

// A cap of 0 closes a seat and keeps what else it carries, so the seat can be
// reopened with its window and names as they were (gt-mukgk).
func TestPoolSetZeroClosesTheSeatAndKeepsItsWindowAndNames(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSeatMapSettings, "")

	out := poolTestSetSeats(t, root, []poolSeatCap{{Agent: "claude-sonnet", Cap: 0}, {Agent: "deepseek-flash", Cap: 0}}, nil, 0, false)
	if !strings.Contains(out, "claude-sonnet: cap 0") {
		t.Errorf("set output = %q, want the cap it wrote", out)
	}
	sonnet, ok := poolTestSeatEntry(t, root, "claude-sonnet")
	if !ok || sonnet.Cap != 0 || !slices.Equal(sonnet.Names, []string{"kestrel"}) {
		t.Errorf("claude-sonnet = %+v, want it kept at cap 0 with its names", sonnet)
	}
	flash, ok := poolTestSeatEntry(t, root, "deepseek-flash")
	if !ok || flash.Cap != 0 || flash.Closed != "20:00-08:00" {
		t.Errorf("deepseek-flash = %+v, want it kept at cap 0 with its window", flash)
	}
	if !strings.Contains(poolTestShowAt(t, root), "closed (cap 0)") {
		t.Error("show does not say the closed seat is closed")
	}
}

// A file that still holds a retired two-seat key does not load, so set refuses
// it before writing, and the error names the replacement and the command that
// converts the file (gt-mukgk).
func TestPoolSetRefusesAFileCarryingRetiredKeys(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-opus","max_seats":3}}`, "")
	before := poolTestRawSettings(t, root)

	err := runPoolSetSeats(&bytes.Buffer{}, root, []poolSeatCap{{Agent: "claude-haiku", Cap: 2}}, nil, 0, false)
	if err == nil {
		t.Fatal("set on a file with retired keys: want a refusal")
	}
	for _, want := range []string{"polecat_pool.agent was replaced by polecat_pool.seats", "run gt pool migrate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to contain %q", err, want)
		}
	}
	if after := poolTestRawSettings(t, root); after != before {
		t.Error("a refused set rewrote the file")
	}
}
