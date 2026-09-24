package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/wisp"
)

// patrolSeams stubs the seed step's two data calls and restores them when the
// test ends. Callers must not run in parallel: the seams are package state.
func patrolSeams(t *testing.T, find func(PatrolConfig) (string, string, bool, error), spawn func(PatrolConfig) (string, error)) {
	t.Helper()
	origFind, origSpawn := findPatrolFn, spawnPatrolFn
	findPatrolFn, spawnPatrolFn = find, spawn
	origEscalate := firePrimePatrolMissingEscalation
	firePrimePatrolMissingEscalation = func(actor, detail string) {}
	t.Cleanup(func() {
		findPatrolFn, spawnPatrolFn = origFind, origSpawn
		firePrimePatrolMissingEscalation = origEscalate
	})
}

func noPatrolFound(PatrolConfig) (string, string, bool, error) { return "", "", false, nil }

func witnessCtx(t *testing.T) RoleContext {
	t.Helper()
	return RoleContext{Role: RoleWitness, Rig: "testrig", TownRoot: t.TempDir(), WorkDir: t.TempDir()}
}

// A patrol role that already has a patrol must be verified, not replaced: a
// second wisp per prime would leave the role with a patrol it never runs.
func TestEnsurePrimePatrol_VerifiesLivePatrolWithoutReseeding(t *testing.T) {
	spawned := 0
	patrolSeams(t,
		func(PatrolConfig) (string, string, bool, error) {
			return "gt-wisp-live", "gt-wisp-live mole [hooked]", true, nil
		},
		func(PatrolConfig) (string, error) { spawned++; return "gt-wisp-new", nil })

	status, err := ensurePrimePatrol(witnessCtx(t))
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v", err)
	}
	if status.PatrolID != "gt-wisp-live" || status.Seeded {
		t.Fatalf("status = %+v, want the live patrol unseeded", status)
	}
	if spawned != 0 {
		t.Fatalf("seeded %d new patrol(s) despite a live one", spawned)
	}
	if status.Formula != constants.MolWitnessPatrol {
		t.Fatalf("formula = %q, want %q", status.Formula, constants.MolWitnessPatrol)
	}
}

// The missing-wisp path: prime must come back with a patrol it created, and the
// wisp must be addressed the way the emitters and `gt hook` look it up.
func TestEnsurePrimePatrol_SeedsMissingPatrol(t *testing.T) {
	var seeded PatrolConfig
	patrolSeams(t, noPatrolFound, func(cfg PatrolConfig) (string, error) {
		seeded = cfg
		return "gt-wisp-new", nil
	})

	status, err := ensurePrimePatrol(witnessCtx(t))
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v", err)
	}
	if !status.Seeded || status.PatrolID != "gt-wisp-new" {
		t.Fatalf("status = %+v, want a seeded gt-wisp-new", status)
	}
	if seeded.Assignee != "testrig/witness" || seeded.PatrolMolName != constants.MolWitnessPatrol {
		t.Fatalf("seeded %+v, want testrig/witness on %s", seeded, constants.MolWitnessPatrol)
	}
}

// A failed seed is the case the bug hid: prime reported success and the role
// sat with an empty hook (gt-e1ie).
func TestEnsurePrimePatrol_MissingPatrolSeedFailureIsError(t *testing.T) {
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) {
		return "", errors.New("proto mol-witness-patrol not found in catalog")
	})

	status, err := ensurePrimePatrol(witnessCtx(t))
	if err == nil {
		t.Fatalf("ensurePrimePatrol() = nil error with no patrol; status %+v", status)
	}
	if !strings.Contains(err.Error(), constants.MolWitnessPatrol) {
		t.Fatalf("error %q must name the patrol it could not start", err)
	}
	if status.PatrolID != "" || status.Seeded {
		t.Fatalf("status = %+v, want no patrol claimed", status)
	}
}

// A wisp that was created but not confirmed hooked is not the same as a
// confirmed-missing patrol: prime carries it as uncertain (with its best-known
// id) rather than aborting the whole payload over it. The next prime's
// findActivePatrol call re-checks it via the hook.
func TestEnsurePrimePatrol_UnhookedWispIsUncertainNotFatal(t *testing.T) {
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) {
		return "gt-wisp-half", errors.New("created wisp gt-wisp-half but failed to hook")
	})

	status, err := ensurePrimePatrol(witnessCtx(t))
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v, want nil (uncertain, not fatal)", err)
	}
	if status.PatrolID != "gt-wisp-half" {
		t.Fatalf("status = %+v, want the best-known patrol id kept", status)
	}
	if !strings.Contains(status.Uncertain, "not confirmed hooked") {
		t.Fatalf("status = %+v, want the uncertainty to say the wisp is not confirmed hooked", status)
	}
}

// One bad bd read must not be reported as "no patrol": the retry is what makes
// the difference between a transient blip and a rig that stops being watched.
// A discovery blip is also not fatal to the whole prime call — it must not
// blank the rest of the payload (mail, directives, hooked work) over one bad
// bd call, and a Dolt outage must not fire an escalation on every prime.
func TestEnsurePrimePatrol_DiscoveryFailureRetriesThenIsUncertain(t *testing.T) {
	reads, spawned := 0, 0
	patrolSeams(t,
		func(PatrolConfig) (string, string, bool, error) {
			reads++
			return "", "", false, errors.New("bd list: connection refused")
		},
		func(PatrolConfig) (string, error) { spawned++; return "gt-wisp-new", nil })

	status, err := ensurePrimePatrol(witnessCtx(t))
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v, want nil (uncertain, not fatal)", err)
	}
	if reads != 2 {
		t.Fatalf("discovery ran %d time(s), want 2", reads)
	}
	if spawned != 0 {
		t.Fatalf("seeded a patrol on a failed discovery (%d spawn(s)) — duplicates on a blip", spawned)
	}
	if !strings.Contains(status.Uncertain, "connection refused") || status.PatrolID != "" {
		t.Fatalf("status = %+v, want the discovery error carried as uncertain with no patrol claimed", status)
	}
}

// A parked rig is an operator stop: no wisp is expected, so prime neither seeds
// one nor calls the role patrol-less.
func TestEnsurePrimePatrol_ParkedRigSeedsNothing(t *testing.T) {
	ctx := witnessCtx(t)
	spawned := 0
	patrolSeams(t,
		func(PatrolConfig) (string, string, bool, error) {
			t.Error("findActivePatrol ran for a parked rig")
			return "", "", false, nil
		},
		func(PatrolConfig) (string, error) { spawned++; return "gt-wisp-new", nil })
	if err := wisp.NewConfig(ctx.TownRoot, ctx.Rig).Set(RigStatusKey, RigStatusParked); err != nil {
		t.Fatalf("park rig: %v", err)
	}

	status, err := ensurePrimePatrol(ctx)
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v", err)
	}
	if !strings.Contains(status.Suspended, "parked") {
		t.Fatalf("status = %+v, want a parked suspension", status)
	}
	if spawned != 0 || status.PatrolID != "" {
		t.Fatalf("status = %+v spawned=%d, want nothing seeded", status, spawned)
	}
}

// A rig-scoped wisp seeded with no rig is addressed "/witness", which nothing
// queries: the patrol would exist and the role still never find it.
func TestEnsurePrimePatrol_RigScopedRoleWithoutARigSeedsNothing(t *testing.T) {
	spawned := 0
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) { spawned++; return "gt-wisp-new", nil })

	for _, role := range []Role{RoleWitness, RoleRefinery} {
		status, err := ensurePrimePatrol(RoleContext{Role: role, TownRoot: t.TempDir()})
		if err != nil {
			t.Fatalf("%s: error = %v", role, err)
		}
		if status.Suspended == "" || status.PatrolID != "" {
			t.Fatalf("%s: status = %+v, want a suspension and no patrol", role, status)
		}
	}
	if spawned != 0 {
		t.Fatalf("seeded %d patrol wisp(s) without a rig", spawned)
	}
}

func TestEnsurePrimePatrol_PausedDeaconSeedsNothing(t *testing.T) {
	town := t.TempDir()
	spawned := 0
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) { spawned++; return "gt-wisp-new", nil })
	if err := deacon.Pause(town, "operator asked", "test"); err != nil {
		t.Fatalf("pause deacon: %v", err)
	}

	status, err := ensurePrimePatrol(RoleContext{Role: RoleDeacon, TownRoot: town, WorkDir: town})
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v", err)
	}
	if !strings.Contains(status.Suspended, "paused") {
		t.Fatalf("status = %+v, want the deacon pause reported", status)
	}
	if spawned != 0 {
		t.Fatal("seeded a patrol for a paused deacon")
	}
}

// refinerySafetyStopSeam stubs the precheck refinery.ActiveSafetyStop makes
// through patrolSuspended, so tests can drive it deterministically instead of
// depending on a live bd binary (om review, gt-e1ie).
func refinerySafetyStopSeam(t *testing.T, fn func(townRoot, rig string) (*refinery.SafetyStop, error)) {
	t.Helper()
	orig := refineryActiveSafetyStopFn
	refineryActiveSafetyStopFn = fn
	t.Cleanup(func() { refineryActiveSafetyStopFn = orig })
}

// A refinery safety stop is an operator stop wherever it is observed — before
// the seed or arriving from the seed itself — so neither route may be reported
// as a prime failure. The precheck is stubbed so this drives the
// arrives-from-the-seed route specifically, without the precheck already
// catching it first.
func TestEnsurePrimePatrol_RefinerySafetyStopIsSuspendedNotFailed(t *testing.T) {
	refinerySafetyStopSeam(t, func(string, string) (*refinery.SafetyStop, error) { return nil, nil })
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) {
		return "", refinery.NewSafetyStoppedError(&refinery.SafetyStop{StopID: "gt-x", Label: "safety_stop:gt-x"})
	})

	status, err := ensurePrimePatrol(RoleContext{Role: RoleRefinery, Rig: "testrig", TownRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v, want a suspension", err)
	}
	if status.Suspended == "" || status.PatrolID != "" {
		t.Fatalf("status = %+v, want a suspended refinery and no patrol", status)
	}
}

// The precheck route: a safety stop found before the seed ever runs must also
// suspend without spawning, not just the stop arriving from the seed itself.
func TestEnsurePrimePatrol_RefinerySafetyStopPrecheckSuspendsWithoutSpawning(t *testing.T) {
	refinerySafetyStopSeam(t, func(string, string) (*refinery.SafetyStop, error) {
		return &refinery.SafetyStop{StopID: "gt-y", Label: "safety_stop:gt-y"}, nil
	})
	spawned := 0
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) { spawned++; return "gt-wisp-new", nil })

	status, err := ensurePrimePatrol(RoleContext{Role: RoleRefinery, Rig: "testrig", TownRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v, want a suspension", err)
	}
	if !strings.Contains(status.Suspended, "gt-y") {
		t.Fatalf("status = %+v, want the precheck stop's id reported", status)
	}
	if spawned != 0 {
		t.Fatalf("seeded %d patrol(s) despite an active precheck safety stop", spawned)
	}
}

// An unreadable safety-stop check is not a confirmed stop: it must fail
// closed (no seed) but be distinguishable from a real operator pause, not
// silently reported the same way.
func TestEnsurePrimePatrol_RefinerySafetyStopUnreadableIsUncertainNotSuspended(t *testing.T) {
	refinerySafetyStopSeam(t, func(string, string) (*refinery.SafetyStop, error) {
		return nil, errors.New("bd show: connection refused")
	})
	spawned := 0
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) { spawned++; return "gt-wisp-new", nil })

	status, err := ensurePrimePatrol(RoleContext{Role: RoleRefinery, Rig: "testrig", TownRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("ensurePrimePatrol() error = %v, want nil (uncertain, not fatal)", err)
	}
	if status.Suspended != "" {
		t.Fatalf("status = %+v, want no Suspended — an unreadable check is not a confirmed stop", status)
	}
	if !strings.Contains(status.Uncertain, "unreadable") {
		t.Fatalf("status = %+v, want Uncertain to say the safety-stop check was unreadable", status)
	}
	if !status.PrecheckUncertain {
		t.Fatalf("status = %+v, want PrecheckUncertain so the display layer withholds the checklist too", status)
	}
	if spawned != 0 {
		t.Fatalf("seeded %d patrol(s) despite an unreadable safety-stop check", spawned)
	}
}

func TestEnsurePrimePatrol_NonPatrolRolesAreUntouched(t *testing.T) {
	patrolSeams(t,
		func(PatrolConfig) (string, string, bool, error) {
			t.Error("a non-patrol role read patrol state")
			return "", "", false, nil
		},
		func(PatrolConfig) (string, error) { t.Error("a non-patrol role seeded a patrol"); return "", nil })

	for _, role := range []Role{RolePolecat, RoleCrew, RoleDog, RoleMayor, RoleBoot, RoleUnknown} {
		status, err := ensurePrimePatrol(RoleContext{Role: role, Rig: "testrig"})
		if err != nil {
			t.Fatalf("%s: error = %v", role, err)
		}
		if status.Role != "" || status.PatrolID != "" {
			t.Fatalf("%s: status = %+v, want a no-op", role, status)
		}
	}
}

func TestPrimePatrolSection_NamesThePatrolAndItsChecklist(t *testing.T) {
	live := primePatrolSection(primePatrolStatus{
		Role: RoleWitness, Formula: constants.MolWitnessPatrol, PatrolID: "gt-wisp-abc",
	})
	for _, want := range []string{"gt-wisp-abc", "hooked", "prime --step 1", constants.MolWitnessPatrol} {
		if !strings.Contains(live, want) {
			t.Fatalf("live section %q must contain %q", live, want)
		}
	}

	seeded := primePatrolSection(primePatrolStatus{
		Role: RoleRefinery, Formula: constants.MolRefineryPatrol, PatrolID: "gt-wisp-new", Seeded: true,
	})
	if !strings.Contains(seeded, "created and hooked") {
		t.Fatalf("seeded section %q must say the wisp was created", seeded)
	}

	suspended := primePatrolSection(primePatrolStatus{Role: RoleWitness, Suspended: "Rig testrig is parked"})
	if !strings.Contains(suspended, "parked") {
		t.Fatalf("suspended section %q must carry the reason", suspended)
	}

	if got := primePatrolSection(primePatrolStatus{}); got != "" {
		t.Fatalf("a non-patrol role rendered %q, want nothing", got)
	}
}

// Path 1 of the bug: the molecule section is the one the hook budget drops, so
// the patrol line has to survive on its own (gt-e1ie).
func TestAssemblePrimePayload_PatrolLineSurvivesDroppedMoleculeSection(t *testing.T) {
	status := primePatrolStatus{Role: RoleWitness, Formula: constants.MolWitnessPatrol, PatrolID: "gt-wisp-abc"}
	parts := primeParts{
		session:    func() string { return "GAS TOWN role:witness pid:1 session:s\n" },
		patrol:     func() string { return primePatrolSection(status) },
		hookedWork: func() string { return strings.Repeat("hooked bead description line\n", 200) },
		molecule:   func() string { return strings.Repeat("patrol checklist line\n", 300) },
		directives: func() string { return strings.Repeat("operator directive line\n", 100) },
		memories:   func() string { return strings.Repeat("- memory-key: first sentence\n", 200) },
	}

	out := assemblePrimePayload(parts, "", false, true).render(primeHookBudget)

	if !strings.Contains(out, "**Patrol:** gt-wisp-abc") {
		t.Fatalf("the patrol line must survive the budget:\n%s", out)
	}
	if !strings.Contains(out, "prime --step 1 --formula "+constants.MolWitnessPatrol) {
		t.Fatalf("the patrol line must say how to read the checklist:\n%s", out)
	}
	if !strings.Contains(out, "omitted to fit the hook budget") || !strings.Contains(out, "molecule") {
		t.Fatalf("test setup no longer drops the molecule section:\n%s", out)
	}
	if len(out) > primeHookBudget {
		t.Fatalf("payload %d chars exceeds the hook budget %d", len(out), primeHookBudget)
	}
}

// Path 2 of the bug: the compact/resume path renders no sections at all, so
// nothing but this step would tell a resumed patrol role its wisp is gone.
func TestRunPrimeCompactResume_PatrolRoleSeedsAndReports(t *testing.T) {
	ctx := witnessCtx(t)
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) { return "gt-wisp-new", nil })
	primeHookSource = "resume"
	t.Cleanup(func() { primeHookSource = "" })

	var err error
	out := captureStdout(t, func() { err = runPrimeCompactResume(ctx) })
	if err != nil {
		t.Fatalf("runPrimeCompactResume() error = %v", err)
	}
	if !strings.Contains(out, "gt-wisp-new") {
		t.Fatalf("resume output must name the seeded patrol:\n%s", out)
	}
}

func TestRunPrimeCompactResume_PatrolRoleFailsLoudWhenSeedFails(t *testing.T) {
	ctx := witnessCtx(t)
	patrolSeams(t, noPatrolFound, func(PatrolConfig) (string, error) {
		return "", errors.New("proto mol-witness-patrol not found in catalog")
	})
	primeHookSource = "resume"
	t.Cleanup(func() { primeHookSource = "" })

	var err error
	stderr := captureStderr(t, func() {
		captureStdout(t, func() { err = runPrimeCompactResume(ctx) })
	})
	if err == nil {
		t.Fatal("runPrimeCompactResume() = nil error with no patrol and no seed")
	}
	if !strings.Contains(stderr, "NO PATROL WISP") {
		t.Fatalf("stderr must announce the failure:\n%s", stderr)
	}
}

// The resume path exists to stay off Dolt for non-patrol roles; a polecat
// resume that starts querying patrol state would slow every session start.
func TestRunPrimeCompactResume_NonPatrolRoleTouchesNoPatrolState(t *testing.T) {
	patrolSeams(t,
		func(PatrolConfig) (string, string, bool, error) {
			t.Error("a polecat resume read patrol state")
			return "", "", false, nil
		},
		func(PatrolConfig) (string, error) { t.Error("a polecat resume seeded a patrol"); return "", nil })
	primeHookSource = "resume"
	t.Cleanup(func() { primeHookSource = "" })

	var err error
	captureStdout(t, func() { err = runPrimeCompactResume(RoleContext{Role: RolePolecat, Rig: "testrig"}) })
	if err != nil {
		t.Fatalf("runPrimeCompactResume() error = %v", err)
	}
}

// Every patrol role reads its wisp through one builder, or prime seeds a wisp
// the emitters and `gt hook` cannot see (gt-e1ie).
func TestPatrolConfigForRole_AddressesEachRoleTheWayHookQueriesIt(t *testing.T) {
	want := map[Role]struct {
		mol      string
		assignee string
	}{
		RoleWitness:  {constants.MolWitnessPatrol, "testrig/witness"},
		RoleRefinery: {constants.MolRefineryPatrol, "testrig/refinery"},
		RoleDeacon:   {constants.MolDeaconPatrol, "deacon/"},
	}
	for role, expect := range want {
		cfg, ok := patrolConfigForRole(RoleContext{Role: role, Rig: "testrig", TownRoot: "/town"})
		if !ok {
			t.Fatalf("%s: patrolConfigForRole() = not a patrol role", role)
		}
		if cfg.PatrolMolName != expect.mol || cfg.Assignee != expect.assignee {
			t.Fatalf("%s: %s on %s, want %s on %s", role, cfg.PatrolMolName, cfg.Assignee, expect.mol, expect.assignee)
		}
		if cfg.BeadsDir != "/town" {
			t.Fatalf("%s: beads dir %q, want the town root", role, cfg.BeadsDir)
		}
	}

	for _, role := range []Role{RolePolecat, RoleCrew, RoleDog, RoleMayor, RoleBoot, RoleUnknown} {
		if _, ok := patrolConfigForRole(RoleContext{Role: role, Rig: "testrig"}); ok {
			t.Fatalf("%s must not be a patrol role", role)
		}
	}
}

// Three emitters and the patrol commands share these strings; a role named
// wrongly in the handoff command sends observations to the wrong session.
func TestPatrolWorkLoopSteps_CarryTheRoleHandoffSubject(t *testing.T) {
	for role, subject := range map[string]string{"witness": "Witness patrol", "refinery": "Refinery patrol", "deacon": "Deacon patrol"} {
		steps := patrolWorkLoopSteps(role)
		if len(steps) != 2 {
			t.Fatalf("%s: %d steps, want 2", role, len(steps))
		}
		joined := strings.Join(steps, "\n")
		if !strings.Contains(joined, fmt.Sprintf("-s %q", subject)) {
			t.Fatalf("%s: work loop must hand off with %q:\n%s", role, subject, joined)
		}
		if !strings.Contains(joined, "patrol report --summary") {
			t.Fatalf("%s: work loop must keep the report-and-loop command:\n%s", role, joined)
		}
	}
}

// A Dolt outage must not turn into an escalation storm: every patrol role in
// every rig priming during an outage hits this exact path (a discovery blip),
// and reportPrimeMissingPatrol — the only caller of the escalation — must
// never run for it (om review, gt-e1ie).
func TestRunPrimeCompactResume_DiscoveryBlipDoesNotEscalateOrFail(t *testing.T) {
	ctx := witnessCtx(t)
	patrolSeams(t,
		func(PatrolConfig) (string, string, bool, error) {
			return "", "", false, errors.New("bd list: connection refused")
		},
		func(PatrolConfig) (string, error) {
			t.Error("spawned a patrol after a discovery blip")
			return "", nil
		})
	escalated := 0
	origEscalate := firePrimePatrolMissingEscalation
	firePrimePatrolMissingEscalation = func(actor, detail string) { escalated++ }
	t.Cleanup(func() { firePrimePatrolMissingEscalation = origEscalate })
	primeHookSource = "resume"
	t.Cleanup(func() { primeHookSource = "" })

	var err error
	out := captureStdout(t, func() { err = runPrimeCompactResume(ctx) })
	if err != nil {
		t.Fatalf("runPrimeCompactResume() error = %v, want nil for an uncertain (non-fatal) patrol state", err)
	}
	if !strings.Contains(out, "state unknown") {
		t.Fatalf("resume output must say the patrol state is unknown:\n%s", out)
	}
	if escalated != 0 {
		t.Fatalf("a discovery blip escalated %d time(s) — during a Dolt outage this floods the Mayor", escalated)
	}
}

// The molecule section must render from prime's own ensurePrimePatrol result,
// not re-run discovery or seeding: before this fix, outputPatrolContext (via
// outputWitnessPatrolContext) called findActivePatrol and autoSpawnPatrol a
// second time, doubling every patrol role's bd round trips — and for
// refinery, its safety-stop check — on every full prime (gt-e1ie).
func TestOutputMoleculeContext_RendersFromStatusWithoutReDiscovering(t *testing.T) {
	patrolSeams(t,
		func(PatrolConfig) (string, string, bool, error) {
			t.Error("the molecule section re-ran patrol discovery")
			return "", "", false, nil
		},
		func(PatrolConfig) (string, error) {
			t.Error("the molecule section re-ran patrol seeding")
			return "", nil
		})

	status := primePatrolStatus{
		Role: RoleWitness, Formula: constants.MolWitnessPatrol,
		PatrolID: "gt-wisp-abc", PatrolLine: "gt-wisp-abc mol-witness-patrol [hooked]",
	}
	out := captureStdout(t, func() {
		outputMoleculeContext(RoleContext{Role: RoleWitness, Rig: "testrig", TownRoot: t.TempDir()}, status)
	})
	if !strings.Contains(out, "gt-wisp-abc") {
		t.Fatalf("molecule output must show the already-resolved patrol id:\n%s", out)
	}
}

// An unreadable operator-stop check (PrecheckUncertain) must still withhold
// the checklist, matching patrolSuspended's original all-or-nothing gate:
// whether a patrol should even run is unknown, not just whether one already
// exists. A generic discovery/seed Uncertain (PrecheckUncertain false) is not
// this gate — it still shows the checklist, as the pre-gt-e1ie discovery-failed
// path always did.
func TestOutputMoleculeContext_PrecheckUncertainWithholdsChecklist(t *testing.T) {
	status := primePatrolStatus{
		Role: RoleRefinery, Formula: constants.MolRefineryPatrol,
		Uncertain: "Refinery testrig safety stop unreadable (bd show: connection refused)", PrecheckUncertain: true,
	}
	out := captureStdout(t, func() {
		outputMoleculeContext(RoleContext{Role: RoleRefinery, Rig: "testrig", TownRoot: t.TempDir()}, status)
	})
	if !strings.Contains(out, "state unknown") {
		t.Fatalf("output must say the patrol state is unknown:\n%s", out)
	}
	for _, forbidden := range []string{"Refinery Patrol Work Loop", "Patrol Work Loop", "Formula Checklist"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("an unreadable safety-stop check must withhold the checklist, got %q in:\n%s", forbidden, out)
		}
	}
}

// A dry-run resume skips the seed step entirely, so the molecule section has
// no status to render from and must stay silent rather than print a
// zero-value "Patrol Active" line for nothing.
func TestOutputMoleculeContext_UnresolvedStatusRendersNothing(t *testing.T) {
	patrolSeams(t,
		func(PatrolConfig) (string, string, bool, error) {
			t.Error("an unresolved status still triggered patrol discovery")
			return "", "", false, nil
		},
		func(PatrolConfig) (string, error) {
			t.Error("an unresolved status still triggered patrol seeding")
			return "", nil
		})

	out := captureStdout(t, func() {
		outputMoleculeContext(RoleContext{Role: RoleWitness, Rig: "testrig", TownRoot: t.TempDir()}, primePatrolStatus{})
	})
	if out != "" {
		t.Fatalf("expected no output for an unresolved patrol status, got:\n%s", out)
	}
}

// patrolConfigForRole is the single builder gt patrol new, gt patrol report
// and prime's seed step all read the wisp through. Before gt-e1ie, gt patrol
// new/report built their own PatrolConfig switches that never set ExtraVars,
// so wisps spawned by those commands got no rig/prefix vars even though the
// witness/refinery patrol formulas expect them.
func TestPatrolConfigForRole_ExtraVarsFlowToEveryCaller(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "testrig"), 0o755); err != nil {
		t.Fatalf("setup rig dir: %v", err)
	}

	witnessCfg, ok := patrolConfigForRole(RoleContext{Role: RoleWitness, Rig: "testrig", TownRoot: town})
	if !ok {
		t.Fatal("witness: not a patrol role")
	}
	if !hasVarKey(witnessCfg.ExtraVars, "rig") {
		t.Fatalf("witness ExtraVars = %v, want a rig= var", witnessCfg.ExtraVars)
	}

	refineryCfg, ok := patrolConfigForRole(RoleContext{Role: RoleRefinery, Rig: "testrig", TownRoot: town})
	if !ok {
		t.Fatal("refinery: not a patrol role")
	}
	if !hasVarKey(refineryCfg.ExtraVars, "rig") {
		t.Fatalf("refinery ExtraVars = %v, want a rig= var", refineryCfg.ExtraVars)
	}
}

func hasVarKey(vars []string, key string) bool {
	for _, v := range vars {
		if strings.HasPrefix(v, key+"=") {
			return true
		}
	}
	return false
}
