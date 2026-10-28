package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

// TestSlingDeliberate: which processes count as a hand sling, and so clear a
// bead's respawn count instead of spending it. The identity is the one the
// process carries in its environment; an identity the rule does not recognise
// is automated, so a dispatcher added later is held to the limit until it is
// classified here on purpose, and no identity makes a process the daemon
// started a hand dispatch (gt-jogkx).
func TestSlingDeliberate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		identity    string
		underDaemon bool
		stdoutIsTTY bool
		want        bool
	}{
		{name: "a crew member's session", identity: "gastown/crew/seagull", want: true},
		{name: "the overseer", identity: "town/crew/overseer", want: true},
		{name: "a bare crew identity", identity: "crew", want: true},
		{name: "the operator at a terminal", stdoutIsTTY: true, want: true},
		{name: "the operator with stdout redirected", stdoutIsTTY: false, want: false},
		{name: "a polecat's session", identity: "gastown/polecats/toast", want: false},
		{name: "the daemon's spec dispatcher", identity: "daemon/spec-dispatch", want: false},
		{name: "a scheduled dispatch", identity: "daemon/scheduled:nightly", want: false},
		{name: "a bare automation role", identity: "deacon", want: false},
		{
			// role.Unknown is an identity this rule does not recognise, and an
			// unrecognised dispatcher spends the budget (the previous revision
			// read it as the operator's terminal; gt-jogkx rework).
			name:     "an identity that parses as no known role",
			identity: "unknown",
			want:     false,
		},
		{
			// The daemon strips the identity it inherited and publishes
			// BD_ACTOR=daemon, so a child it starts is automated even when it
			// carries a crew identity (GH#3006, gt-jogkx).
			name:        "the daemon's process, whatever identity it carries",
			identity:    "town/crew/overseer",
			underDaemon: true,
			want:        false,
		},
		{
			name:        "the daemon's process at a terminal",
			underDaemon: true,
			stdoutIsTTY: true,
			want:        false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := slingDeliberate(tt.identity, tt.underDaemon, tt.stdoutIsTTY); got != tt.want {
				t.Errorf("slingDeliberate(%q, underDaemon=%v, stdoutIsTTY=%v) = %v, want %v",
					tt.identity, tt.underDaemon, tt.stdoutIsTTY, got, tt.want)
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
	recordSlingRespawn(io.Discard, townRoot, opts)

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

// TestSlingDeliberateReportsAClearItCannotWrite: the clear runs after the
// polecat exists, so it cannot refuse the spawn; a count it could not clear is
// reported instead of passing as cleared. The live state file is the one
// location the reset refuses to repair, so a corrupt file there drives the
// failure (gt-jogkx rework).
func TestSlingDeliberateReportsAClearItCannotWrite(t *testing.T) {
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

	var warn bytes.Buffer
	mark := recordSlingRespawn(&warn, townRoot, SlingSpawnOptions{HookBead: deliberateBead, Deliberate: true})
	if mark.Count != 0 {
		t.Errorf("a clear with no increment to give back returned mark %+v", mark)
	}
	if !strings.Contains(warn.String(), "clearing the respawn count") {
		t.Errorf("the failed clear was not reported; warning = %q", warn.String())
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
	recordSlingRespawn(io.Discard, townRoot, opts)
	if got := respawnCount(t, townRoot, deliberateBead); got != 1 {
		t.Fatalf("respawn count after one automated spawn = %d, want 1", got)
	}

	for i := 1; i < config.DefaultRecoveryMaxBeadRespawns; i++ {
		recordSlingRespawn(io.Discard, townRoot, opts)
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

// TestPrepareSlingRefusesAtTheRespawnBudget: the spawn path itself refuses a
// bead at its automated budget, before a rig is opened or a polecat allocated,
// so the limit holds however the dispatch reached the spawn (gt-jogkx).
func TestPrepareSlingRefusesAtTheRespawnBudget(t *testing.T) {
	t.Parallel()
	townRoot, _ := rigTown(t)
	spendRespawnBudget(t, townRoot, deliberateBead)

	_, err := prepareSlingPolecat(townRoot, "gastown", SlingSpawnOptions{HookBead: deliberateBead},
		fakeSlingStores(beadsfake.New()))
	if err == nil {
		t.Fatal("the spawn path took a bead at its respawn budget")
	}
	if !strings.Contains(err.Error(), "respawn limit reached") {
		t.Errorf("refusal %q does not name the limit", err)
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
	recordSlingRespawn(io.Discard, townRoot, opts)

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

// slingRunForClassification is a run whose recorded actor and classification
// facts are fixed, for the wiring tests below: every site that classifies a
// sling reads the facts from the deps, never the actor it records on the bead.
func slingRunForClassification(actor string, identity func() string, daemon daemonMarker, stdoutIsTTY func() bool) *slingRun {
	return &slingRun{
		slingDeps: &slingDeps{
			actor:       func() string { return actor },
			identity:    identity,
			daemon:      daemon,
			stdoutIsTTY: stdoutIsTTY,
			requester:   func() string { return "test" },
		},
	}
}

// TestSlingRigTargetParamsCarryDeliberate: `gt sling <bead> <rig>` builds the
// engine request with the classification of the process running it, which is
// what the respawn gate reads inside the spawn (gt-jogkx).
func TestSlingRigTargetParamsCarryDeliberate(t *testing.T) {
	t.Parallel()
	crew := func() string { return "gastown/crew/seagull" }
	polecat := func() string { return "gastown/polecats/toast" }
	daemon := func() bool { return true }
	terminal := func() bool { return true }
	for _, tt := range []struct {
		name        string
		identity    func() string
		daemon      daemonMarker
		stdoutIsTTY func() bool
		want        bool
	}{
		{name: "a crew sling", identity: crew, want: true},
		{name: "an operator at a terminal", stdoutIsTTY: terminal, want: true},
		{name: "a polecat's sling", identity: polecat, want: false},
		{name: "a daemon sling", identity: func() string { return "daemon/spec-dispatch" }, want: false},
		{name: "a crew identity in the daemon's process", identity: crew, daemon: daemon, want: false},
		{name: "an operator whose output is not a terminal", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			params := slingRunForClassification("gastown/polecats/toast", tt.identity, tt.daemon, tt.stdoutIsTTY).
				rigTargetParams("gastown", deliberateBead, "", "/town", "")
			if params.Deliberate != tt.want {
				t.Errorf("rig target Deliberate = %v, want %v", params.Deliberate, tt.want)
			}
		})
	}
}

// TestSlingDeliberateIgnoresTheRecordedActor: the actor a sling records on the
// bead is a string the caller writes, so it must not decide whether the budget
// is cleared. The classification reads the identity the process carries in its
// environment; here the recorded actor names a crew member while the process
// carries none, and the sling is still automated (gt-jogkx rework).
func TestSlingDeliberateIgnoresTheRecordedActor(t *testing.T) {
	t.Parallel()
	params := slingRunForClassification("town/crew/overseer", nil, nil, nil).
		rigTargetParams("gastown", deliberateBead, "", "/town", "")
	if params.Deliberate {
		t.Error("a self-declared crew actor cleared the respawn budget")
	}
}

// TestSlingResolveTargetOptionsCarryDeliberate: a named-polecat target spawns
// from the resolution options, so the classification has to reach them (and
// through them the spawn) or a named hand sling is held to the budget
// (gt-jogkx).
func TestSlingResolveTargetOptionsCarryDeliberate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		identity func() string
		want     bool
	}{
		{name: "a crew sling", identity: func() string { return "town/crew/overseer" }, want: true},
		{name: "a polecat's sling", identity: func() string { return "gastown/polecats/toast" }, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := slingRunForClassification("", tt.identity, nil, nil).resolveTargetOptions(deliberateBead, "/town", false)
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
// the batch's own classification of its process, and a batch wired with none of
// the facts is automated — the safe direction for the budget (gt-jogkx).
func TestSlingBatchParamsCarryDeliberate(t *testing.T) {
	t.Parallel()
	crew := func() string { return "gastown/crew/seagull" }
	daemon := func() bool { return true }
	terminal := func() bool { return true }
	for _, tt := range []struct {
		name        string
		identity    func() string
		daemon      daemonMarker
		stdoutIsTTY func() bool
		want        bool
	}{
		{name: "a crew batch", identity: crew, want: true},
		{name: "an operator's batch at a terminal", stdoutIsTTY: terminal, want: true},
		{name: "a scheduled batch", identity: func() string { return "daemon/scheduled:nightly" }, want: false},
		{name: "a crew identity in the daemon's process", identity: crew, daemon: daemon, want: false},
		{name: "a batch with no facts wired", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := batchSling{identity: tt.identity, daemon: tt.daemon, stdoutIsTTY: tt.stdoutIsTTY}
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

	recordSlingRespawn(io.Discard, townRoot, opts)
	if got := respawnCount(t, townRoot, deliberateBead); got != 0 {
		t.Errorf("respawn count after the spawn = %d, want 0", got)
	}
}
