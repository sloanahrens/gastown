package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// The town a mix runs against: two agents the presets can launch, a pool seat
// on one of them, and a second seat that is closed.
const poolMixTestSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "claude-sonnet": {"command": "claude", "args": ["--model", "sonnet"]},
    "claude-opus": {"command": "claude", "args": ["--model", "opus"]},
    "deepseek-flash": {"command": "claude", "args": ["--model", "flash"]},
    "deepseek-pro": {"command": "claude", "args": ["--model", "pro"]}
  },
  "role_agents": {"polecat": "claude-sonnet"},
  "polecat_pool": {"agent": "claude-sonnet", "max_seats": 2}
}
`

const poolMixTestPatrol = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {
    "spec_dispatch": {"enabled": true, "interval": "30s", "max_hooked": 0}
  }
}
`

// poolMixTestPinnedPatrol is the same patrol config with a scheduled sling that
// names an agent, the way the daily deep review is pinned to claude-sonnet.
const poolMixTestPinnedPatrol = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {
    "spec_dispatch": {"enabled": true, "interval": "30s", "max_hooked": 0},
    "scheduled_slings": {
      "enabled": true,
      "entries": [
        {"name": "deep-review", "rig": "alpha", "formula": "mol-deep-review", "agent": "claude-sonnet", "interval": "24h", "dispatch": "spec"}
      ]
    }
  }
}
`

// poolMixTestModels is a table shaped like the town's own, third profile and
// approval entry included: a mix writes one role of it and nothing else.
const poolMixTestModels = `{
  "version": 1,
  "profiles": {
    "flash": {"preset": "deepseek-flash", "om_backend": ["claude-deepseek-flash", "-p"]},
    "sonnet": {"preset": "claude-sonnet", "om_backend": ["claude", "-p", "--model", "sonnet"]},
    "pro": {"preset": "deepseek-pro", "om_backend": ["claude-deepseek-pro", "-p"]}
  },
  "default": "flash",
  "roles": {"polecat": "sonnet", "om-review": "sonnet", "deep-review": "sonnet"},
  "escalations": [
    {"profile": "pro", "role": "deep-review", "reason": "the deep review is infrequent", "approved_by": "sloan", "approved_at": "2026-10-12", "expires_at": null}
  ]
}
`

// poolMixTown writes the town a mix needs. models is skipped when empty, so a
// test can run a town with no model table.
func poolMixTown(t *testing.T, settings, patrol, models string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		config.TownSettingsPath(root):       settings,
		config.DaemonPatrolConfigPath(root): patrol,
	}
	if models != "" {
		files[poolMixModelTablePath(root)] = models
	}
	for path, body := range files {
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

// poolMixTestRig registers a rig with a beads database, so the pinned-work scan
// walks it. The entry carries no beads prefix, which is what keeps the dock
// check from asking bd about a rig that has none.
func poolMixTestRig(t *testing.T, root, name string) {
	t.Helper()
	rigs := `{"version": 1, "rigs": {"` + name + `": {"git_url": "file:///tmp/` + name + `", "added_at": "2026-01-01T00:00:00Z"}}}`
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(constants.MayorRigsPath(root), []byte(rigs), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, name, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// poolMixNoBeads is the town read of a town whose rigs carry no pinned beads
// and whose seats nothing occupies: neither read leaves the test's own files.
func poolMixNoBeads() poolMixTownReads {
	return poolMixTownReads{
		openBeads: func(string) ([]*beads.Issue, error) { return nil, nil },
		live:      func() poolShowLive { return poolShowLive{} },
	}
}

// poolMixRun parses args the way the command does and runs the mix against a
// town with no pinned beads.
func poolMixRun(t *testing.T, root string, args []string, opts poolMixOptions) (string, error) {
	t.Helper()
	return poolMixRunWith(t, root, args, opts, poolMixNoBeads())
}

func poolMixRunWith(t *testing.T, root string, args []string, opts poolMixOptions, reads poolMixTownReads) (string, error) {
	t.Helper()
	mix, err := parsePoolMix(args)
	if err != nil {
		t.Fatalf("parsePoolMix(%v): %v", args, err)
	}
	var out bytes.Buffer
	err = runPoolMixWith(&out, root, mix, opts, reads)
	return out.String(), err
}

// poolMixTownSettings reads the town settings back.
func poolMixTownSettings(t *testing.T, root string) *config.TownSettings {
	t.Helper()
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// poolMixSecondSeat reads spec_dispatch back.
func poolMixSecondSeat(t *testing.T, root string) *config.SpecDispatchConfig {
	t.Helper()
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	if sd == nil {
		return &config.SpecDispatchConfig{}
	}
	return sd
}

// poolMixModelRole reads settings/models.json's polecat role: the profile name,
// or null for a role that follows the table's default.
func poolMixModelRole(t *testing.T, root string) (name string, null bool) {
	t.Helper()
	table := poolMixModelTableRead(t, root)
	role, ok := table.Roles["polecat"]
	if !ok {
		t.Fatalf("settings/models.json has no roles.polecat")
	}
	if role == nil {
		return "", true
	}
	return *role, false
}

// poolMixModelPreset is the agent the model table's polecat role resolves to:
// the preset of the profile the role names, or of the table's default for a
// null role. It is the comparison gt-models.sh show makes, so a test can prove
// the run leaves it reading no drift.
func poolMixModelPreset(t *testing.T, root string) string {
	t.Helper()
	table := poolMixModelTableRead(t, root)
	role, ok := table.Roles["polecat"]
	if !ok {
		t.Fatalf("settings/models.json has no roles.polecat")
	}
	profile := table.Default
	if role != nil {
		profile = *role
	}
	preset, ok := table.Profiles[profile]
	if !ok {
		t.Fatalf("settings/models.json names profile %q, which it does not define", profile)
	}
	return preset.Preset
}

func poolMixModelTableRead(t *testing.T, root string) poolMixModelTable {
	t.Helper()
	data, err := os.ReadFile(poolMixModelTablePath(root))
	if err != nil {
		t.Fatal(err)
	}
	var table poolMixModelTable
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatalf("settings/models.json does not parse: %v\n%s", err, data)
	}
	return table
}

// poolMixFile is a file's bytes, for a test that has to prove a run changed
// nothing.
func poolMixFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestPoolMixIsAttachedUnderPool is the wiring the tree lint cannot see: the
// verb and its two flags.
func TestPoolMixIsAttachedUnderPool(t *testing.T) {
	t.Parallel()
	var mix *cobra.Command
	for _, c := range poolCmd.Commands() {
		if c.Name() == "mix" {
			mix = c
		}
	}
	if mix == nil {
		t.Fatal("gt pool mix is not a subcommand of gt pool")
	}
	for _, flag := range []string{"dry-run"} {
		if mix.Flags().Lookup(flag) == nil {
			t.Errorf("gt pool mix has no --%s", flag)
		}
	}
	if mix.Args == nil {
		t.Error("gt pool mix accepts any number of arguments, a mix of none included")
	}
	if err := mix.Args(mix, nil); err == nil {
		t.Error("gt pool mix with no argument: want a usage refusal")
	}
}

// The seats the town ran the day the mix went wrong (gt-lwlva): two Sonnet in
// the pool seat and two flash in the second one, with the daily deep reviews
// pinned to claude-sonnet.
const poolMixTestIncidentSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "claude-sonnet": {"command": "claude", "args": ["--model", "sonnet"]},
    "deepseek-flash": {"command": "claude", "args": ["--model", "flash"]}
  },
  "role_agents": {"polecat": "claude-sonnet"},
  "polecat_pool": {"agent": "claude-sonnet", "max_seats": 2, "max_total": 4}
}
`

const poolMixTestIncidentPatrol = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {
    "spec_dispatch": {"enabled": true, "interval": "30s", "hooked_agent": "deepseek-flash", "max_hooked": 2},
    "scheduled_slings": {
      "enabled": true,
      "entries": [
        {"name": "deep-review-slice", "rig": "alpha", "formula": "mol-deep-review", "agent": "claude-sonnet", "interval": "24h", "dispatch": "spec"}
      ]
    }
  }
}
`

// TestPoolMixIsTheIncidentFixInOneCommand is the day gt-lwlva describes: two
// Sonnet plus two flash to two flash plus two Sonnet, with the reviews pinned
// to Sonnet left a seat. It took six commands and three silent traps; this is
// the one. The pro seat is retired, so a pinned agent is seated by naming it in
// the mix.
func TestPoolMixIsTheIncidentFixInOneCommand(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestIncidentSettings, poolMixTestIncidentPatrol, poolMixTestModels)
	if _, err := poolMixRun(t, root, []string{"deepseek-flash=2", "claude-sonnet=2"}, poolMixOptions{}); err != nil {
		t.Fatalf("mix deepseek-flash=2 claude-sonnet=2: %v", err)
	}
	pool := poolMixTownSettings(t, root).PolecatPool
	if pool.Agent != "deepseek-flash" || pool.MaxSeats != 2 {
		t.Errorf("pool seat = %s cap %d, want deepseek-flash cap 2", pool.Agent, pool.MaxSeats)
	}
	if pool.GetMaxTotal() != 4 {
		t.Errorf("max_total = %d, want 4 (the mix's two seats together)", pool.GetMaxTotal())
	}
	if got := poolMixTownSettings(t, root).RoleAgents["polecat"]; got != "deepseek-flash" {
		t.Errorf("role_agents.polecat = %q, want deepseek-flash", got)
	}
	if got := poolMixSecondSeat(t, root).MaxHooked; got != 2 {
		t.Errorf("max_hooked = %d, want 2: the mix names two agents", got)
	}
	seats := poolTestTownBudget(t, root).Seats
	if len(seats) != 2 {
		t.Fatalf("the dispatcher runs %d seats, want 2: %v", len(seats), seats)
	}
	if seats[0].Agent != "deepseek-flash" || seats[0].Cap != 2 {
		t.Errorf("first seat = %s cap %d, want deepseek-flash cap 2", seats[0].Agent, seats[0].Cap)
	}
	if seats[1].Agent != "claude-sonnet" || seats[1].Cap != 2 {
		t.Errorf("second seat = %+v, want claude-sonnet cap 2", seats[1])
	}
}

func TestPoolMixSetsTheWholeMix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		args      []string
		wantSeats map[string]int
		wantTotal int
		wantAgent string // the pool seat's agent, which role_agents.polecat follows
		wantModel string // settings/models.json roles.polecat; "" is null
	}{
		{
			name:      "two agents two each",
			args:      []string{"deepseek-flash=2", "claude-sonnet=2"},
			wantSeats: map[string]int{"deepseek-flash": 2, "claude-sonnet": 2},
			wantTotal: 4,
			wantAgent: "deepseek-flash",
			wantModel: "",
		},
		{
			name:      "four flash",
			args:      []string{"deepseek-flash=4"},
			wantSeats: map[string]int{"deepseek-flash": 4},
			wantTotal: 4,
			wantAgent: "deepseek-flash",
			wantModel: "",
		},
		{
			name:      "four sonnet",
			args:      []string{"claude-sonnet=4"},
			wantSeats: map[string]int{"claude-sonnet": 4},
			wantTotal: 4,
			wantAgent: "claude-sonnet",
			wantModel: "sonnet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
			if _, err := poolMixRun(t, root, tc.args, poolMixOptions{}); err != nil {
				t.Fatalf("mix %v: %v", tc.args, err)
			}
			if got := poolTestBudget(t, root); !maps.Equal(got, tc.wantSeats) {
				t.Errorf("dispatcher seats = %v, want %v", got, tc.wantSeats)
			}
			if got := poolTestTownBudget(t, root).MaxTotal; got != tc.wantTotal {
				t.Errorf("max_total = %d, want %d", got, tc.wantTotal)
			}
			ts := poolMixTownSettings(t, root)
			if ts.PolecatPool.Agent != tc.wantAgent {
				t.Errorf("polecat_pool.agent = %q, want %q", ts.PolecatPool.Agent, tc.wantAgent)
			}
			if got := ts.RoleAgents["polecat"]; got != tc.wantAgent {
				t.Errorf("role_agents.polecat = %q, want %q (the pool seat's agent)", got, tc.wantAgent)
			}
			// The second seat is the mix's second agent, recorded with its cap.
			sd := poolMixSecondSeat(t, root)
			if len(tc.args) < 2 && sd.MaxHooked != 0 {
				t.Errorf("max_hooked = %d, want 0: a one-agent mix closes the second seat", sd.MaxHooked)
			}
			// The models table names the profile whose preset is the pool seat's
			// agent, so gt-models.sh show reads no drift.
			name, null := poolMixModelRole(t, root)
			if tc.wantModel == "" && !null {
				t.Errorf("models roles.polecat = %q, want null (%s is the table's default profile)", name, tc.wantAgent)
			}
			if tc.wantModel != "" && (null || name != tc.wantModel) {
				t.Errorf("models roles.polecat = %q (null=%v), want %q", name, null, tc.wantModel)
			}
			if got := poolMixModelPreset(t, root); got != tc.wantAgent {
				t.Errorf("the model table's polecat role resolves to %q, want %q: gt-models.sh show would read this mix as drift", got, tc.wantAgent)
			}
		})
	}
}

// TestPoolMixKeepsTheSecondSeatAndItsAgent checks the one key gt pool set
// cannot reach: which agent the second seat runs.
func TestPoolMixKeepsTheSecondSeatAndItsAgent(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	if _, err := poolMixRun(t, root, []string{"deepseek-flash=2", "claude-sonnet=3"}, poolMixOptions{}); err != nil {
		t.Fatalf("mix: %v", err)
	}
	sd := poolMixSecondSeat(t, root)
	if sd.HookedAgent != "claude-sonnet" || sd.MaxHooked != 3 {
		t.Errorf("second seat = %s cap %d, want claude-sonnet cap 3", sd.HookedAgent, sd.MaxHooked)
	}
	// A zero on the second agent closes the seat and records the agent, the way
	// gt pool set-second does, so reopening it is one cap away.
	if _, err := poolMixRun(t, root, []string{"deepseek-flash=2", "claude-sonnet=0"}, poolMixOptions{}); err != nil {
		t.Fatalf("mix with the second seat closed: %v", err)
	}
	sd = poolMixSecondSeat(t, root)
	if sd.HookedAgent != "claude-sonnet" || sd.MaxHooked != 0 {
		t.Errorf("second seat = %s cap %d, want claude-sonnet cap 0", sd.HookedAgent, sd.MaxHooked)
	}
	if got := poolTestBudget(t, root); !maps.Equal(got, map[string]int{"deepseek-flash": 2}) {
		t.Errorf("dispatcher seats = %v, want deepseek-flash alone", got)
	}
}

func TestPoolMixRefusesAPinnedAgentWithoutASeat(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPinnedPatrol, poolMixTestModels)
	before := poolMixFile(t, config.TownSettingsPath(root))

	_, err := poolMixRun(t, root, []string{"deepseek-flash=4"}, poolMixOptions{})
	if err == nil {
		t.Fatal("mix deepseek-flash=4 with a review pinned to claude-sonnet: want a refusal")
	}
	for _, want := range []string{"claude-sonnet", "gt pool mix deepseek-flash=4 claude-sonnet=4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if got := poolMixFile(t, config.TownSettingsPath(root)); got != before {
		t.Error("a refused mix wrote the town settings")
	}
}

// A pinned agent named in the mix gets a seat of its own: the second seat, with
// the cap the mix gives it.
func TestPoolMixSeatsAPinnedAgentNamedInTheMix(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPinnedPatrol, poolMixTestModels)
	if _, err := poolMixRun(t, root, []string{"deepseek-flash=4", "claude-sonnet=1"}, poolMixOptions{}); err != nil {
		t.Fatalf("mix seating the pinned agent: %v", err)
	}
	ts := poolMixTownSettings(t, root)
	if got := ts.PolecatPool.GetMaxTotal(); got != 5 {
		t.Errorf("max_total = %d, want 5: the mix's four plus the pinned seat", got)
	}
	seats := poolTestTownBudget(t, root).Seats
	if len(seats) != 2 || seats[0].Agent != "deepseek-flash" || seats[0].Cap != 4 {
		t.Fatalf("the dispatcher runs %+v, want flash cap 4 and the pinned agent's seat", seats)
	}
	if seats[1].Agent != "claude-sonnet" || seats[1].Cap != 1 {
		t.Errorf("pinned agent's seat = %+v, want claude-sonnet cap 1", seats[1])
	}
}

// TestPoolMixRefusesALabelPinnedBead is the other half of the pinned scan: an
// open bead carrying an agent:<name> label, read from a rig's own database.
func TestPoolMixRefusesALabelPinnedBead(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	poolMixTestRig(t, root, "alpha")
	reads := poolMixNoBeads()
	reads.openBeads = func(rigPath string) ([]*beads.Issue, error) {
		if filepath.Base(rigPath) != "alpha" {
			t.Errorf("the pinned scan read %s, want the registered rig alpha", rigPath)
		}
		return []*beads.Issue{{
			ID:     "alpha-1",
			Labels: []string{"scheduled:deep-review-slice", "agent:deepseek-pro"},
		}}, nil
	}
	_, err := poolMixRunWith(t, root, []string{"deepseek-flash=4"}, poolMixOptions{}, reads)
	if err == nil || !strings.Contains(err.Error(), "deepseek-pro") {
		t.Fatalf("mix with alpha-1 pinned to deepseek-pro: err = %v, want a refusal naming deepseek-pro", err)
	}
	// Naming the pinned agent in the mix gives it the second seat.
	if _, err := poolMixRunWith(t, root, []string{"deepseek-flash=4", "deepseek-pro=1"}, poolMixOptions{}, reads); err != nil {
		t.Fatalf("mix seating the pinned agent: %v", err)
	}
	if got := poolMixSecondSeat(t, root).HookedAgent; got != "deepseek-pro" {
		t.Errorf("hooked_agent = %q, want deepseek-pro", got)
	}
}

// TestPoolMixRefusesABoardItCannotRead: a rig whose beads cannot be read is not
// a rig with no pinned work.
func TestPoolMixRefusesABoardItCannotRead(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	poolMixTestRig(t, root, "alpha")
	reads := poolMixNoBeads()
	reads.openBeads = func(string) ([]*beads.Issue, error) { return nil, errors.New("dolt is away") }
	if _, err := poolMixRunWith(t, root, []string{"deepseek-flash=4"}, poolMixOptions{}, reads); err == nil {
		t.Fatal("a rig board that could not be read: want a refusal")
	}
}

func TestPoolMixDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	before := map[string]string{
		config.TownSettingsPath(root):       poolMixFile(t, config.TownSettingsPath(root)),
		config.DaemonPatrolConfigPath(root): poolMixFile(t, config.DaemonPatrolConfigPath(root)),
		poolMixModelTablePath(root):         poolMixFile(t, poolMixModelTablePath(root)),
	}
	out, err := poolMixRun(t, root, []string{"deepseek-flash=2", "claude-sonnet=2"}, poolMixOptions{dryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, want := range []string{"keys this would write:", "polecat_pool.agent = deepseek-flash", "dry run: nothing written"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output does not contain %q:\n%s", want, out)
		}
	}
	for path, want := range before {
		if got := poolMixFile(t, path); got != want {
			t.Errorf("the dry run changed %s", path)
		}
	}
}

// TestPoolMixSaysWhenTheSecondSeatNeedsAKickstart: the second seat is read at
// daemon start, so only a mix that moves it asks for one.
func TestPoolMixSaysWhenTheSecondSeatNeedsAKickstart(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	out, err := poolMixRun(t, root, []string{"deepseek-flash=2", "claude-sonnet=2"}, poolMixOptions{})
	if err != nil {
		t.Fatalf("mix: %v", err)
	}
	if !strings.Contains(out, "kickstart the daemon") {
		t.Errorf("a mix that opens the second seat does not ask for a kickstart:\n%s", out)
	}
	out, err = poolMixRun(t, root, []string{"deepseek-flash=2", "claude-sonnet=2"}, poolMixOptions{})
	if err != nil {
		t.Fatalf("second mix: %v", err)
	}
	if !strings.Contains(out, "no daemon kickstart is needed") {
		t.Errorf("a mix that leaves the second seat where it was still asks for a kickstart:\n%s", out)
	}
}

// TestPoolMixWritesThePolecatRoleAloneWithoutAModelTable: the seats are still
// worth writing, and the run says the table is what is missing.
func TestPoolMixWritesThePolecatRoleAloneWithoutAModelTable(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, "")
	out, err := poolMixRun(t, root, []string{"deepseek-flash=4"}, poolMixOptions{})
	if err != nil {
		t.Fatalf("mix without a model table: %v", err)
	}
	if !strings.Contains(out, "no settings/models.json") {
		t.Errorf("the run does not say the town has no model table:\n%s", out)
	}
	if _, err := os.Stat(poolMixModelTablePath(root)); !os.IsNotExist(err) {
		t.Error("the run created a model table")
	}
	if got := poolMixTownSettings(t, root).RoleAgents["polecat"]; got != "deepseek-flash" {
		t.Errorf("role_agents.polecat = %q, want deepseek-flash", got)
	}
}

// TestPoolMixNotesAProfileTheTableDoesNotHave: the seats are written and the
// run says which role will still read as drift.
func TestPoolMixNotesAProfileTheTableDoesNotHave(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	before := poolMixFile(t, poolMixModelTablePath(root))
	out, err := poolMixRun(t, root, []string{"claude-opus=2"}, poolMixOptions{})
	if err != nil {
		t.Fatalf("mix: %v", err)
	}
	if !strings.Contains(out, "DRIFT") {
		t.Errorf("the run does not warn about the missing profile:\n%s", out)
	}
	if got := poolMixFile(t, poolMixModelTablePath(root)); got != before {
		t.Error("the run wrote a table it has no profile for")
	}
}

// TestPoolMixLeavesTheRestOfTheModelTableAlone: the run writes one role of a
// file gt-models.sh owns. The profiles, the default, the other roles and the
// approvals are the operator's.
func TestPoolMixLeavesTheRestOfTheModelTableAlone(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	if _, err := poolMixRun(t, root, []string{"claude-sonnet=4"}, poolMixOptions{}); err != nil {
		t.Fatalf("mix: %v", err)
	}
	table := poolMixModelTableRead(t, root)
	if table.Default != "flash" {
		t.Errorf("default = %q, want flash", table.Default)
	}
	if len(table.Profiles) != 3 || table.Profiles["pro"].Preset != "deepseek-pro" {
		t.Errorf("profiles = %v, want the file's three", table.Profiles)
	}
	if len(table.Escalations) != 1 {
		t.Errorf("escalations = %d, want the file's one approval", len(table.Escalations))
	}
	for role, want := range map[string]string{"om-review": "sonnet", "deep-review": "sonnet"} {
		got, ok := table.Roles[role]
		if !ok || got == nil || *got != want {
			t.Errorf("roles.%s = %v, want %s", role, got, want)
		}
	}
}

// TestPoolMixLeavesRoleAgentAloneWithNoOpenSeat: every seat closed, so there is
// no agent to point the role at.
func TestPoolMixLeavesRoleAgentAloneWithNoOpenSeat(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	if _, err := poolMixRun(t, root, []string{"deepseek-flash=0"}, poolMixOptions{}); err != nil {
		t.Fatalf("mix: %v", err)
	}
	if got := poolMixTownSettings(t, root).RoleAgents["polecat"]; got != "claude-sonnet" {
		t.Errorf("role_agents.polecat = %q, want the file's own claude-sonnet: a mix with no open seat does not choose one", got)
	}
	if got := poolTestBudget(t, root); len(got) != 0 {
		t.Errorf("seats = %v, want none", got)
	}
}

// TestPoolMixBacksUpEveryFileItWrites keeps gt pool's own promise: the previous
// copy of each file is beside it.
func TestPoolMixBacksUpEveryFileItWrites(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	if _, err := poolMixRun(t, root, []string{"deepseek-flash=4"}, poolMixOptions{}); err != nil {
		t.Fatalf("mix: %v", err)
	}
	for _, path := range []string{
		config.TownSettingsPath(root),
		config.DaemonPatrolConfigPath(root),
		poolMixModelTablePath(root),
	} {
		matches, err := filepath.Glob(path + ".bak-*")
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Errorf("no backup beside %s", path)
		}
	}
}

func TestParsePoolMixRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no equals", args: []string{"deepseek-flash"}, want: "want <agent>=<n>"},
		{name: "negative", args: []string{"deepseek-flash=-1"}, want: "0 or more"},
		{name: "not a number", args: []string{"deepseek-flash=many"}, want: "0 or more"},
		{name: "empty agent", args: []string{"=2"}, want: "want <agent>=<n>"},
		{name: "same agent twice", args: []string{"deepseek-flash=2", "deepseek-flash=3"}, want: "twice"},
		{name: "three agents", args: []string{"a=1", "b=1", "c=1"}, want: "at most 2 agents"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parsePoolMix(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parsePoolMix(%v) = %v, want an error naming %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestPoolMixRefusesAnUnknownAgent(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPatrol, poolMixTestModels)
	before := poolMixFile(t, config.TownSettingsPath(root))
	out, err := poolMixRun(t, root, []string{"gpt-9=2"}, poolMixOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("mix with an agent the town cannot launch: err = %v, want an unknown-agent refusal", err)
	}
	if out != "" {
		t.Errorf("a refused mix printed a page:\n%s", out)
	}
	if got := poolMixFile(t, config.TownSettingsPath(root)); got != before {
		t.Error("a refused mix wrote the town settings")
	}
}

// TestPoolMixRefusesMorePinsThanThereAreSeats is the multi-pin refusal: the
// town runs two seats, so three pinned agents cannot all be seated, and the
// refusal has to say so.
func TestPoolMixRefusesMorePinsThanThereAreSeats(t *testing.T) {
	t.Parallel()
	root := poolMixTown(t, poolMixTestSettings, poolMixTestPinnedPatrol, poolMixTestModels)
	poolMixTestRig(t, root, "alpha")
	reads := poolMixNoBeads()
	reads.openBeads = func(string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "alpha-2", Labels: []string{"agent:deepseek-pro"}}}, nil
	}
	_, err := poolMixRunWith(t, root, []string{"deepseek-flash=4"}, poolMixOptions{}, reads)
	if err == nil {
		t.Fatal("two pinned agents and two seats: want a refusal")
	}
	for _, want := range []string{"claude-sonnet", "deepseek-pro", "gt pool mix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}
