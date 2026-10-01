package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/wisp"
)

func setupPolecatCapacityTestTown(t *testing.T, maxPolecats int) string {
	t.Helper()
	townRoot := t.TempDir()
	configureScheduler(t, townRoot, maxPolecats, 1)
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), &config.RigsConfig{Version: config.CurrentRigsVersion}); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}
	return townRoot
}

func TestCapacitySnapshotCleansStaleReservations(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)
	dir := polecatAdmissionDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir reservations: %v", err)
	}
	stale := polecatAdmissionReservation{
		ID:        "stale",
		PID:       99999999,
		Rig:       "gastown",
		Bead:      "gt-stale",
		Operation: "test",
		CreatedAt: time.Now().Add(-2 * polecatAdmissionReservationTTL),
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale reservation: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.json"), data, 0644); err != nil {
		t.Fatalf("write stale reservation: %v", err)
	}

	snapshot, err := polecatCapacitySnapshotForTown(townRoot)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Reservations != 0 || snapshot.Free != 1 {
		t.Fatalf("snapshot after stale cleanup = %+v, want reservations=0 free=1", snapshot)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.json")); !os.IsNotExist(err) {
		t.Fatalf("stale reservation still exists: %v", err)
	}
}

func TestCapacitySnapshotRemovesStructurallyInvalidReservations(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)
	dir := polecatAdmissionDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir reservations: %v", err)
	}
	path := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write invalid reservation: %v", err)
	}

	snapshot, err := polecatCapacitySnapshotForTown(townRoot)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Reservations != 0 || snapshot.Free != 1 {
		t.Fatalf("snapshot after invalid cleanup = %+v, want reservations=0 free=1", snapshot)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid reservation still exists: %v", err)
	}
}

func TestCapacitySnapshotRemovesMismatchedReservationFile(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)
	dir := polecatAdmissionDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir reservations: %v", err)
	}
	reservation := polecatAdmissionReservation{
		ID:        "other",
		PID:       os.Getpid(),
		Rig:       "gastown",
		Bead:      "gt-mismatch",
		Operation: "test",
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(reservation)
	if err != nil {
		t.Fatalf("marshal reservation: %v", err)
	}
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write mismatched reservation: %v", err)
	}

	snapshot, err := polecatCapacitySnapshotForTown(townRoot)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Reservations != 0 || snapshot.Free != 1 {
		t.Fatalf("snapshot after mismatch cleanup = %+v, want reservations=0 free=1", snapshot)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mismatched reservation still exists: %v", err)
	}
}

func TestCapacitySnapshotKeepsOldLiveReservation(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)
	dir := polecatAdmissionDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir reservations: %v", err)
	}
	reservation := polecatAdmissionReservation{
		ID:        "live",
		PID:       os.Getpid(),
		Rig:       "gastown",
		Bead:      "gt-live",
		Operation: "test",
		CreatedAt: time.Now().Add(-2 * polecatAdmissionReservationTTL),
	}
	data, err := json.Marshal(reservation)
	if err != nil {
		t.Fatalf("marshal reservation: %v", err)
	}
	path := filepath.Join(dir, "live.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write live reservation: %v", err)
	}

	snapshot, err := polecatCapacitySnapshotForTown(townRoot)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Reservations != 1 || snapshot.Free != 0 {
		t.Fatalf("snapshot with old live reservation = %+v, want reservations=1 free=0", snapshot)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live reservation should remain: %v", err)
	}
}

func TestAcquirePolecatAdmissionUsesConfiguredCap(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)

	first, snapshot, err := acquirePolecatAdmission(townRoot, "gastown", "gt-one", "test")
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	defer first.Release()
	if snapshot.Max != 1 || snapshot.Reservations != 1 || snapshot.Free != 0 {
		t.Fatalf("snapshot after first admission = %+v, want max=1 reservations=1 free=0", snapshot)
	}

	second, deniedSnapshot, err := acquirePolecatAdmission(townRoot, "gastown", "gt-two", "test")
	if second != nil {
		defer second.Release()
	}
	var admissionErr *polecatCapacityAdmissionError
	if !errors.As(err, &admissionErr) {
		t.Fatalf("second admission error = %v, want polecatCapacityAdmissionError", err)
	}
	if deniedSnapshot.Max != 1 || deniedSnapshot.Reservations != 1 || deniedSnapshot.Free != 0 {
		t.Fatalf("denied snapshot = %+v, want max=1 reservations=1 free=0", deniedSnapshot)
	}
	if !strings.Contains(err.Error(), "scheduler.max_polecats") {
		t.Fatalf("denial error %q should mention scheduler.max_polecats", err.Error())
	}

	first.Release()
	third, snapshot, err := acquirePolecatAdmission(townRoot, "gastown", "gt-three", "test")
	if err != nil {
		t.Fatalf("third admission after release: %v", err)
	}
	defer third.Release()
	if snapshot.Max != 1 || snapshot.Reservations != 1 || snapshot.Free != 0 {
		t.Fatalf("snapshot after third admission = %+v, want max=1 reservations=1 free=0", snapshot)
	}
}

func TestAcquirePolecatAdmissionDisabledWhenSchedulerCapNonPositive(t *testing.T) {
	t.Parallel()
	for _, maxPolecats := range []int{-1, 0} {
		t.Run("max", func(t *testing.T) {
			townRoot := t.TempDir()
			configureScheduler(t, townRoot, maxPolecats, 1)

			handle, snapshot, err := acquirePolecatAdmission(townRoot, "gastown", "gt-one", "test")
			if err != nil {
				t.Fatalf("admission with max=%d: %v", maxPolecats, err)
			}
			defer handle.Release()
			if !handle.disabled {
				t.Fatalf("admission handle should be disabled for max=%d", maxPolecats)
			}
			if snapshot.Max != maxPolecats {
				t.Fatalf("snapshot max = %d, want %d", snapshot.Max, maxPolecats)
			}
			if _, err := os.Stat(polecatAdmissionDir(townRoot)); !os.IsNotExist(err) {
				t.Fatalf("reservation dir exists for disabled admission: %v", err)
			}
		})
	}
}

func TestConcurrentPolecatAdmissionReservationsDoNotExceedCap(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var handles []*polecatAdmissionHandle
	successes := 0
	denials := 0

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			handle, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-race", "test")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
				handles = append(handles, handle)
				return
			}
			var admissionErr *polecatCapacityAdmissionError
			if errors.As(err, &admissionErr) || strings.Contains(err.Error(), "admission is busy") {
				denials++
				return
			}
			t.Errorf("unexpected admission error: %v", err)
		}()
	}
	close(start)
	wg.Wait()
	for _, handle := range handles {
		handle.Release()
	}

	if successes != 1 {
		t.Fatalf("successful admissions = %d, want 1", successes)
	}
	if denials != 5 {
		t.Fatalf("denied admissions = %d, want 5", denials)
	}
}

func TestApplyAgentFieldsToCapacitySnapshotSeparatesPendingMR(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		fields     *beads.AgentFields
		activeWork *beads.Issue
		want       polecatCapacitySnapshot
	}{
		{
			name:   "active mr is pending capacity",
			fields: &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: "clean", ActiveMR: "gt-mr-open"},
			want:   polecatCapacitySnapshot{PendingMR: 1},
		},
		{
			name:   "push failed remains recovery blocked",
			fields: &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: "clean", ActiveMR: "gt-mr-open", PushFailed: true},
			want:   polecatCapacitySnapshot{RecoveryBlocked: 1, capacityUsed: 1},
		},
		{
			name:   "clean idle is reusable",
			fields: &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: "clean"},
			want:   polecatCapacitySnapshot{ReusableIdle: 1},
		},
		{
			name:       "active work consumes capacity",
			fields:     &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: "clean"},
			activeWork: &beads.Issue{ID: "gt-work", Status: string(beads.StatusOpen), Assignee: "gastown/polecats/synth"},
			want:       polecatCapacitySnapshot{RecoveryBlocked: 1, capacityUsed: 1},
		},
		{
			name:       "deferred work blocks recovery without capacity",
			fields:     &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: "clean"},
			activeWork: &beads.Issue{ID: "gt-paused", Status: string(beads.StatusDeferred), Assignee: "gastown/polecats/synth"},
			want:       polecatCapacitySnapshot{RecoveryBlocked: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := polecatCapacitySnapshot{}
			applyAgentFieldsToCapacitySnapshot(&snapshot, t.TempDir(), "gastown", "synth", tt.fields, tt.activeWork, nil)
			if snapshot.Working != tt.want.Working || snapshot.RecoveryBlocked != tt.want.RecoveryBlocked || snapshot.ReusableIdle != tt.want.ReusableIdle || snapshot.PendingMR != tt.want.PendingMR || snapshot.capacityUsed != tt.want.capacityUsed {
				t.Fatalf("snapshot = %+v, want %+v", snapshot, tt.want)
			}
		})
	}
}

func TestCapacitySnapshotRecoveryBlockedDoesNotAlwaysConsumeFreeCapacity(t *testing.T) {
	t.Parallel()
	snapshot := polecatCapacitySnapshot{Max: 3}
	applyWorkstateDispositionToCapacitySnapshot(&snapshot, polecat.StateIdle, polecat.WorkstateDisposition{
		Verdict:              polecat.WorkstateVerdictNeedsRecovery,
		NeedsRecovery:        true,
		CountsTowardCapacity: false,
	})
	applyWorkstateDispositionToCapacitySnapshot(&snapshot, polecat.StateStalled, polecat.WorkstateDisposition{
		Verdict:              polecat.WorkstateVerdictNeedsRecovery,
		NeedsRecovery:        true,
		CountsTowardCapacity: true,
	})
	snapshot.Free = snapshot.Max - snapshot.occupied()

	if snapshot.RecoveryBlocked != 2 || snapshot.capacityUsed != 1 || snapshot.Free != 2 {
		t.Fatalf("snapshot = %+v, want recovery=2 capacityUsed=1 free=2", snapshot)
	}
}

func TestPrintDryRunPlanUsesCapacitySnapshot(t *testing.T) {
	t.Parallel()
	out := captureTo(func(w io.Writer) {
		printDryRunPlanTo(w, capacity.DispatchPlan{
			ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "gt-one", TargetRig: "gastown"}},
			Skipped:    2,
			Reason:     "capacity",
		}, polecatCapacitySnapshot{
			Max:             2,
			Working:         1,
			RecoveryBlocked: 1,
			Reservations:    0,
			ReusableIdle:    3,
			Parked:          4,
			PendingMR:       2,
			Free:            0,
		}, 5)
	})
	for _, want := range []string{"0 free of 2", "working: 1", "recovery_blocked: 1", "reusable_idle: 3", "parked: 4", "pending_mr: 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output %q missing %q", out, want)
		}
	}
}

func TestPrintDryRunPlanValidationReasonNotCapacity(t *testing.T) {
	t.Parallel()
	out := captureTo(func(w io.Writer) {
		printDryRunPlanTo(w, capacity.DispatchPlan{
			Skipped: 2,
			Reason:  "validation",
		}, polecatCapacitySnapshot{Max: 2, Free: 2}, 5)
	})
	if !strings.Contains(out, "validation failed for 2 candidate") {
		t.Fatalf("dry-run output %q missing validation reason", out)
	}
	if strings.Contains(out, "No capacity") {
		t.Fatalf("dry-run output %q should not report capacity for validation failures", out)
	}
}

func TestPrintDispatchNoOpReportsExplicitReason(t *testing.T) {
	t.Parallel()
	out := captureTo(func(w io.Writer) {
		printDispatchNoOpTo(w, capacity.DispatchReport{Reason: "none"}, polecatCapacitySnapshot{})
	})
	if !strings.Contains(out, "No ready beads scheduled for dispatch") {
		t.Fatalf("none output = %q", out)
	}

	out = captureTo(func(w io.Writer) {
		printDispatchNoOpTo(w, capacity.DispatchReport{Reason: "validation", Skipped: 1}, polecatCapacitySnapshot{})
	})
	if !strings.Contains(out, "No dispatchable beads") || !strings.Contains(out, "validation") {
		t.Fatalf("validation output = %q", out)
	}
}

func TestResolveTargetRigPassesHeldAdmissionToSpawn(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	called := false
	h.run.spawnPolecat = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		called = true
		if rigName != "gastown" {
			t.Fatalf("rigName = %q, want gastown", rigName)
		}
		if !opts.SkipAdmission {
			t.Fatal("spawn should skip admission when caller already holds reservation")
		}
		if opts.TownRoot != slingTestTown {
			t.Fatalf("TownRoot = %q, want %q", opts.TownRoot, slingTestTown)
		}
		return h.newSpawn(rigName), nil
	}

	resolved, err := h.run.resolveSlingTarget("gastown", ResolveTargetOptions{
		TownRoot:             slingTestTown,
		SkipPolecatAdmission: true,
		NoBoot:               true,
	})
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if !called {
		t.Fatal("spawnPolecatForSling was not called")
	}
	if resolved.Agent != "gastown/polecats/Toast" {
		t.Fatalf("resolved agent = %q, want gastown/polecats/Toast", resolved.Agent)
	}
}

func TestStandaloneFormulaRigTargetAcquiresSingleAdmission(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.resolveTarget = h.run.resolveSlingTarget
	// A formula wisp hooked to a just-spawned polecat is stale and is burned
	// before dispatch (gt-7evi4). Fail that burn so the sling stops before any
	// bd call, and record the rollback instead of running the real one.
	rollbacks := 0
	h.run.rollbackArtifacts = func(*SpawnedPolecatInfo, string, string, string) { rollbacks++ }
	h.run.burnWisp = func(string, string) error { return errors.New("stop before bd") }
	admissions := 0
	h.run.admitPolecat = func(townRootArg, rigName, beadID, operation string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error) {
		admissions++
		if townRootArg != slingTestTown || rigName != "gastown" || beadID != "test-formula" || operation != "formula" {
			t.Fatalf("admission args = (%q,%q,%q,%q)", townRootArg, rigName, beadID, operation)
		}
		return &polecatAdmissionHandle{disabled: true}, polecatCapacitySnapshot{Max: 1, Free: 0}, nil
	}
	h.run.spawnPolecat = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		if !opts.SkipAdmission {
			t.Fatal("formula rig spawn should use caller-held admission")
		}
		return h.newSpawn(rigName), nil
	}
	h.run.findHookedFormula = func(workDir, targetAgent, formulaName string) (*beads.Issue, error) {
		return &beads.Issue{ID: "gt-wisp-existing"}, nil
	}

	if err := h.run.runFormula(context.Background(), []string{"test-formula", "gastown"}); err == nil || !strings.Contains(err.Error(), "stop before bd") {
		t.Fatalf("runSlingFormula: want the injected burn failure, got %v", err)
	}
	if admissions != 1 {
		t.Fatalf("admissions = %d, want 1", admissions)
	}
	if rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1 (a failed sling must not strand the spawned polecat)", rollbacks)
	}
}

func TestStandaloneFormulaExistingPolecatNoopDoesNotRequireCapacity(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.resolveTarget = h.run.resolveSlingTarget
	h.run.admitPolecat = func(townRootArg, rigName, beadID, operation string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error) {
		t.Fatalf("no-op existing formula should not acquire capacity, got (%q,%q,%q,%q)", townRootArg, rigName, beadID, operation)
		return nil, polecatCapacitySnapshot{}, nil
	}
	h.run.resolveAgent = func(target string) (string, string, string, error) {
		if target != "gastown/polecats/toast" {
			t.Fatalf("target = %q, want gastown/polecats/toast", target)
		}
		return "gastown/polecats/toast", "%1", slingTestTown + "/gastown/polecats/toast/gastown", nil
	}
	h.run.findHookedFormula = func(workDir, targetAgent, formulaName string) (*beads.Issue, error) {
		return &beads.Issue{ID: "gt-wisp-existing"}, nil
	}

	if err := h.run.runFormula(context.Background(), []string{"test-formula", "gastown/polecats/toast"}); err != nil {
		t.Fatalf("runSlingFormula: %v", err)
	}
}

// setupPolecatCapacityTown creates a direct-dispatch town (scheduler.max_polecats
// = -1, no admission from the town cap) with one polecats directory per named
// rig, for tests that hand the town to acquirePolecatAdmission themselves.
func setupPolecatCapacityTown(t *testing.T, rigNames ...string) string {
	t.Helper()
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	configureScheduler(t, townRoot, -1, 1)
	rigs := make(map[string]config.RigEntry, len(rigNames))
	for _, name := range rigNames {
		if err := os.MkdirAll(filepath.Join(townRoot, name, "polecats"), 0755); err != nil {
			t.Fatalf("mkdir rig %s: %v", name, err)
		}
		rigs[name] = config.RigEntry{GitURL: "https://example.invalid/" + name + ".git"}
	}
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs:    rigs,
	}); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}
	return townRoot
}

func setRigMaxPolecats(t *testing.T, townRoot, rigName string, cap int) {
	t.Helper()
	if err := wisp.NewConfig(townRoot, rigName).Set("max_polecats", cap); err != nil {
		t.Fatalf("seed %s max_polecats=%d: %v", rigName, cap, err)
	}
}

// TestAcquirePolecatAdmissionEnforcesRigCapInDirectMode is gt-1kbi: with the town
// in direct dispatch (scheduler.max_polecats = -1) admission used to be a no-op,
// so a rig's max_polecats could not hold it to N concurrent polecats. The town
// cap stays off; the rig cap must still refuse the second slot.
func TestAcquirePolecatAdmissionEnforcesRigCapInDirectMode(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTown(t, "gastown")
	setRigMaxPolecats(t, townRoot, "gastown", 1)

	first, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-one", "test")
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	defer first.Release()
	if first.disabled {
		t.Fatal("admission is disabled for a capped rig, so the rig cap cannot bind")
	}

	second, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-two", "test")
	if second != nil {
		defer second.Release()
	}
	var admissionErr *polecatCapacityAdmissionError
	if !errors.As(err, &admissionErr) {
		t.Fatalf("second admission error = %v, want polecatCapacityAdmissionError", err)
	}
	if admissionErr.RigMax != 1 || admissionErr.RigUsed != 1 {
		t.Fatalf("denial = %+v, want rig max=1 used=1", admissionErr)
	}
	for _, want := range []string{"max_polecats", "gt rig config set gastown max_polecats"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("denial error %q should mention %q", err.Error(), want)
		}
	}

	first.Release()
	third, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-three", "test")
	if err != nil {
		t.Fatalf("admission after the first slot was released: %v", err)
	}
	defer third.Release()
}

// TestAcquirePolecatAdmissionLeavesUncappedRigAlone pins the default: a rig with
// no max_polecats of its own is uncapped, so nothing about this change throttles
// the rigs that never asked for a cap (gt-1kbi).
func TestAcquirePolecatAdmissionLeavesUncappedRigAlone(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTown(t, "gastown")

	handle, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-one", "test")
	if err != nil {
		t.Fatalf("admission for an uncapped rig in direct mode: %v", err)
	}
	defer handle.Release()
	if !handle.disabled {
		t.Fatal("an uncapped rig in direct dispatch should bypass admission")
	}
	if _, err := os.Stat(polecatAdmissionDir(townRoot)); !os.IsNotExist(err) {
		t.Fatalf("reservation dir exists for an uncapped rig: %v", err)
	}
}

// TestAcquirePolecatAdmissionRigCapIsPerRig: one rig's cap must not spend another
// rig's slots.
func TestAcquirePolecatAdmissionRigCapIsPerRig(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTown(t, "gastown", "hm")
	setRigMaxPolecats(t, townRoot, "gastown", 1)

	first, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-one", "test")
	if err != nil {
		t.Fatalf("first gastown admission: %v", err)
	}
	defer first.Release()

	other, _, err := acquirePolecatAdmission(townRoot, "hm", "hm-one", "test")
	if err != nil {
		t.Fatalf("hm admission while gastown is full: %v", err)
	}
	defer other.Release()

	if _, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-two", "test"); err == nil {
		t.Fatal("gastown admitted a second polecat at its cap")
	}
}

// TestAcquirePolecatAdmissionRigCapBindsUnderTownCap covers the branch where both
// caps are on: the rig cap is read from the town snapshot's per-rig accounting,
// and it refuses even though the town still has free slots.
func TestAcquirePolecatAdmissionRigCapBindsUnderTownCap(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTown(t, "gastown", "hm")
	configureScheduler(t, townRoot, 5, 1)
	setRigMaxPolecats(t, townRoot, "gastown", 1)

	first, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-one", "test")
	if err != nil {
		t.Fatalf("first gastown admission: %v", err)
	}
	defer first.Release()

	second, _, err := acquirePolecatAdmission(townRoot, "gastown", "gt-two", "test")
	if second != nil {
		defer second.Release()
	}
	var admissionErr *polecatCapacityAdmissionError
	if !errors.As(err, &admissionErr) {
		t.Fatalf("second admission error = %v, want polecatCapacityAdmissionError", err)
	}
	if admissionErr.RigMax != 1 {
		t.Fatalf("denial = %+v, want the rig cap to be the refusing one", admissionErr)
	}

	// The town still has room: another rig is not collateral damage.
	other, _, err := acquirePolecatAdmission(townRoot, "hm", "hm-one", "test")
	if err != nil {
		t.Fatalf("hm admission while gastown is at its rig cap: %v", err)
	}
	defer other.Release()
}

// captureTo returns what write wrote.
func captureTo(write func(w io.Writer)) string {
	var b strings.Builder
	write(&b)
	return b.String()
}
