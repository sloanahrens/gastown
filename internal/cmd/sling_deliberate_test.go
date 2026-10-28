package cmd

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/sling"
)

// deliberateBead is the bead the respawn-budget cases below spend and clear. It
// is its own id so the state file these cases write names one bead.
const deliberateBead = "gt-deliberate"

// respawnCount is the count the state file holds for beadID, 0 when it holds
// no record: a cleared count is deleted, not decremented, so "zero" is the
// record's absence and a test can tell it from "one below the limit".
func respawnCount(t *testing.T, townRoot, beadID string) int {
	t.Helper()
	path := filepath.Join(townRoot, constants.DirRuntime, "bead-respawn-counts.json")
	data, err := os.ReadFile(path) //nolint:gosec // G304: a path this test's town root builds
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var state struct {
		Beads map[string]struct {
			Count int `json:"count"`
		} `json:"beads"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return state.Beads[beadID].Count
}

// spendRespawnBudget records a full automated budget's worth of respawns for
// beadID, the state a bead is in when the limit refuses it.
func spendRespawnBudget(t *testing.T, townRoot, beadID string) {
	t.Helper()
	for i := 0; i < config.DefaultRecoveryMaxBeadRespawns; i++ {
		if _, err := polecat.RecordBeadRespawn(townRoot, beadID); err != nil {
			t.Fatalf("RecordBeadRespawn: %v", err)
		}
	}
	if !polecat.ShouldBlockRespawn(townRoot, beadID) {
		t.Fatal("the fixture did not reach the respawn limit")
	}
}

// TestSlingDeliberate: which dispatchers count as a hand sling, and so clear a
// bead's respawn count instead of spending it. Every actor the classifier does
// not recognise is automated, so a dispatcher added later is held to the limit
// until it is classified here on purpose, and no actor makes a process the
// daemon started a hand dispatch (gt-jogkx).
func TestSlingDeliberate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		actor       string
		underDaemon bool
		want        bool
	}{
		{name: "the operator at a terminal", actor: "unknown", want: true},
		{name: "a crew member", actor: "gastown/crew/seagull", want: true},
		{name: "the overseer", actor: "town/crew/overseer", want: true},
		{name: "a polecat", actor: "gastown/polecats/toast", want: false},
		{name: "the daemon's spec dispatcher", actor: "daemon/spec-dispatch", want: false},
		{name: "a scheduled dispatch", actor: "daemon/scheduled:nightly", want: false},
		{name: "a bare automation role", actor: "deacon", want: false},
		{name: "no actor at all", actor: "", want: false},
		{
			// The daemon strips the identity it inherited, so a child it starts
			// has no live role: a crew actor on the command line must not turn an
			// automated redispatch into a hand sling (GH#3006, gt-jogkx).
			name:        "the daemon's process, whatever actor it names",
			actor:       "gastown/crew/seagull",
			underDaemon: true,
			want:        false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := slingDeliberate(tt.actor, tt.underDaemon); got != tt.want {
				t.Errorf("slingDeliberate(%q, underDaemon=%v) = %v, want %v",
					tt.actor, tt.underDaemon, got, tt.want)
			}
		})
	}
}

// TestSlingDeliberatePassesTheBudgetAndLeavesTheCountAtZero is the case the
// bead is about: a hand sling of a bead at its respawn budget launches a
// polecat, and the count is zero afterwards, so the next round starts with a
// full budget (gt-jogkx).
func TestSlingDeliberatePassesTheBudgetAndLeavesTheCountAtZero(t *testing.T) {
	t.Parallel()
	townRoot, _ := rigTown(t)
	spendRespawnBudget(t, townRoot, deliberateBead)

	opts := SlingSpawnOptions{HookBead: deliberateBead, Deliberate: true}
	if err := admitSlingRespawn(fakeSlingStores(beadsfake.New()), townRoot, "gastown", opts); err != nil {
		t.Fatalf("a deliberate sling was refused at the budget: %v", err)
	}
	recordSlingRespawn(townRoot, opts)

	if got := respawnCount(t, townRoot, deliberateBead); got != 0 {
		t.Errorf("respawn count after the deliberate sling = %d, want 0", got)
	}
}

// TestSlingDeliberateAdmitsAnUnreadableCount: the count file is the breaker's
// memory, and a hand sling is the person's decision, so a state file this
// process cannot parse does not stand between them and the work. The clear is
// attempted at the spawn and reported there; the bead keeps the count the next
// respawn-reset can move aside (gt-jogkx).
func TestSlingDeliberateAdmitsAnUnreadableCount(t *testing.T) {
	t.Parallel()
	townRoot, _ := rigTown(t)
	dir := filepath.Join(townRoot, constants.DirRuntime)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	path := filepath.Join(dir, "bead-respawn-counts.json")
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	opts := SlingSpawnOptions{HookBead: deliberateBead, Deliberate: true}
	if err := admitSlingRespawn(fakeSlingStores(beadsfake.New()), townRoot, "gastown", opts); err != nil {
		t.Errorf("a deliberate sling was refused on an unreadable count: %v", err)
	}
}

// TestSlingAutomatedSpendsTheBudgetAndRefusesAtIt: the automated redispatch
// path the limit exists for still refuses at the budget, and each spawn below
// it costs one increment (gt-jogkx).
func TestSlingAutomatedSpendsTheBudgetAndRefusesAtIt(t *testing.T) {
	t.Parallel()
	townRoot, _ := rigTown(t)
	opts := SlingSpawnOptions{HookBead: deliberateBead}

	if err := admitSlingRespawn(fakeSlingStores(beadsfake.New()), townRoot, "gastown", opts); err != nil {
		t.Fatalf("an automated sling below the budget was refused: %v", err)
	}
	recordSlingRespawn(townRoot, opts)
	if got := respawnCount(t, townRoot, deliberateBead); got != 1 {
		t.Fatalf("respawn count after one automated spawn = %d, want 1", got)
	}

	for i := 1; i < config.DefaultRecoveryMaxBeadRespawns; i++ {
		recordSlingRespawn(townRoot, opts)
	}
	if got := respawnCount(t, townRoot, deliberateBead); got != config.DefaultRecoveryMaxBeadRespawns {
		t.Fatalf("respawn count at the budget = %d, want %d", got, config.DefaultRecoveryMaxBeadRespawns)
	}

	err := admitSlingRespawn(fakeSlingStores(beadsfake.New()), townRoot, "gastown", opts)
	if err == nil {
		t.Fatal("an automated sling at the budget was admitted")
	}
	if !strings.Contains(err.Error(), "respawn limit reached") {
		t.Errorf("refusal %q does not name the limit", err)
	}
	if got := respawnCount(t, townRoot, deliberateBead); got != config.DefaultRecoveryMaxBeadRespawns {
		t.Errorf("the refused sling changed the count to %d", got)
	}
}

// TestSlingForcePassesWithoutTouchingTheCount: --force is the documented
// override, and it neither spends an increment nor clears the count — a forced
// sling leaves the bead exactly as it found it (gt-jogkx).
func TestSlingForcePassesWithoutTouchingTheCount(t *testing.T) {
	t.Parallel()
	townRoot, _ := rigTown(t)
	spendRespawnBudget(t, townRoot, deliberateBead)

	opts := SlingSpawnOptions{HookBead: deliberateBead, Force: true}
	if err := admitSlingRespawn(fakeSlingStores(beadsfake.New()), townRoot, "gastown", opts); err != nil {
		t.Fatalf("a forced sling was refused at the budget: %v", err)
	}
	recordSlingRespawn(townRoot, opts)

	if got := respawnCount(t, townRoot, deliberateBead); got != config.DefaultRecoveryMaxBeadRespawns {
		t.Errorf("respawn count after a forced sling = %d, want %d", got, config.DefaultRecoveryMaxBeadRespawns)
	}
}

// TestSlingRespawnResetClearsACount: the escape hatch the refusal names still
// clears a spent budget (gt-jogkx).
func TestSlingRespawnResetClearsACount(t *testing.T) {
	t.Parallel()
	townRoot, _ := rigTown(t)
	spendRespawnBudget(t, townRoot, deliberateBead)

	if err := polecat.ResetBeadRespawnCount(townRoot, deliberateBead); err != nil {
		t.Fatalf("ResetBeadRespawnCount: %v", err)
	}
	if got := respawnCount(t, townRoot, deliberateBead); got != 0 {
		t.Errorf("respawn count after the reset = %d, want 0", got)
	}
}

// slingRunForClassification is a run whose actor and daemon marker are fixed,
// for the wiring tests below: every site that classifies a sling reads both
// from the deps.
func slingRunForClassification(actor string, daemon daemonMarker) *slingRun {
	return &slingRun{
		slingDeps: &slingDeps{
			actor:     func() string { return actor },
			daemon:    daemon,
			requester: func() string { return "test" },
		},
	}
}

// TestSlingRigTargetParamsCarryDeliberate: `gt sling <bead> <rig>` builds the
// engine request with the classification of the actor running it, which is what
// the respawn gate reads inside the spawn (gt-jogkx).
func TestSlingRigTargetParamsCarryDeliberate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		actor  string
		daemon bool
		want   bool
	}{
		{name: "a crew sling", actor: "gastown/crew/seagull", want: true},
		{name: "a daemon sling", actor: "daemon/spec-dispatch", want: false},
		{name: "a crew actor in the daemon's process", actor: "gastown/crew/seagull", daemon: true, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var marker daemonMarker
			if tt.daemon {
				marker = func() bool { return true }
			}
			params := slingRunForClassification(tt.actor, marker).rigTargetParams("gastown", deliberateBead, "", "/town", "")
			if params.Deliberate != tt.want {
				t.Errorf("rig target Deliberate = %v, want %v", params.Deliberate, tt.want)
			}
		})
	}
}

// TestSlingResolveTargetOptionsCarryDeliberate: a named-polecat target spawns
// from the resolution options, so the classification has to reach them (and
// through them the spawn) or a named hand sling is held to the budget
// (gt-jogkx).
func TestSlingResolveTargetOptionsCarryDeliberate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		actor string
		want  bool
	}{
		{name: "a crew sling", actor: "town/crew/overseer", want: true},
		{name: "a polecat's sling", actor: "gastown/polecats/toast", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := slingRunForClassification(tt.actor, nil).resolveTargetOptions(deliberateBead, "/town", false)
			if opts.Deliberate != tt.want {
				t.Errorf("resolve target Deliberate = %v, want %v", opts.Deliberate, tt.want)
			}
			if got := polecatSpawnOptions("garnet", opts).Deliberate; got != tt.want {
				t.Errorf("spawn Deliberate = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSlingBatchParamsCarryDeliberate: every bead of a batch is dispatched with
// the batch's own classification, and a batch wired without an actor is
// automated — the safe direction for the budget (gt-jogkx).
func TestSlingBatchParamsCarryDeliberate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		actor  func() string
		daemon daemonMarker
		want   bool
	}{
		{name: "a crew batch", actor: func() string { return "gastown/crew/seagull" }, want: true},
		{name: "a scheduled batch", actor: func() string { return "daemon/scheduled:nightly" }, want: false},
		{name: "a crew batch in the daemon's process", actor: func() string { return "gastown/crew/seagull" }, daemon: func() bool { return true }, want: false},
		{name: "a batch with no actor wired", actor: nil, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := batchSling{actor: tt.actor, daemon: tt.daemon}
			params := b.params(deliberateBead, "gastown", "/town", "/town/.beads", "", "", false)
			if params.Deliberate != tt.want {
				t.Errorf("batch Deliberate = %v, want %v", params.Deliberate, tt.want)
			}
		})
	}
}

// TestSlingEngineSpawnCarriesDeliberate: the engine's spawn options are mapped
// onto this package's spawn options field for field, and the classification is
// one of them — without it the spawn can no longer tell a hand sling from an
// automated one (gt-jogkx).
func TestSlingEngineSpawnCarriesDeliberate(t *testing.T) {
	t.Parallel()
	var got SlingSpawnOptions
	d := &slingDeps{spawnPolecat: func(_ string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		got = opts
		return &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "onyx"}, nil
	}}

	if _, err := d.engineSpawnPolecat("gastown", sling.SpawnOptions{HookBead: deliberateBead, Deliberate: true}); err != nil {
		t.Fatalf("engineSpawnPolecat: %v", err)
	}
	if !got.Deliberate {
		t.Error("the engine's deliberate dispatch reached the spawn classified as automated")
	}
}

// TestSlingDeliberateClearWaitsForTheSeat: the count is cleared by the spawn
// that happens, not by the gate that admits it, so a deliberate sling that
// never reaches a polecat leaves the bead's count where it found it (gt-jogkx).
func TestSlingDeliberateClearWaitsForTheSeat(t *testing.T) {
	t.Parallel()
	townRoot, _ := rigTown(t)
	spendRespawnBudget(t, townRoot, deliberateBead)

	opts := SlingSpawnOptions{HookBead: deliberateBead, Deliberate: true}
	if err := admitSlingRespawn(fakeSlingStores(beadsfake.New()), townRoot, "gastown", opts); err != nil {
		t.Fatalf("a deliberate sling was refused at the budget: %v", err)
	}
	if got := respawnCount(t, townRoot, deliberateBead); got != config.DefaultRecoveryMaxBeadRespawns {
		t.Errorf("the gate alone changed the count to %d; the clear belongs to the spawn", got)
	}

	recordSlingRespawn(townRoot, opts)
	if got := respawnCount(t, townRoot, deliberateBead); got != 0 {
		t.Errorf("respawn count after the spawn = %d, want 0", got)
	}
}
