package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/tmux"
)

// testStubWispTree replaces the wisp-tree read for the duration of the test.
func testStubWispTree(t *testing.T, fn func(townRoot, wispID string) ([]beads.WispStep, error)) {
	t.Helper()
	prev := wispTreeFn
	t.Cleanup(func() { wispTreeFn = prev })
	wispTreeFn = fn
}

// testStubCloseStaleWisp replaces the abandoned-wisp close for the duration of
// the test.
func testStubCloseStaleWisp(t *testing.T, fn func(townRoot, wispID string, tree []beads.WispStep) (int, error)) {
	t.Helper()
	prev := closeStaleWispFn
	t.Cleanup(func() { closeStaleWispFn = prev })
	closeStaleWispFn = fn
}

// testHandlerDaemon creates a minimal Daemon with a logger for handler tests.
func testHandlerDaemon(t *testing.T, townRoot string) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: discardLogger,
	}
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// testSetupDogState creates a dog directory with a .dog.json state file.
func testSetupDogState(t *testing.T, townRoot, name string, state dog.State, lastActive time.Time) {
	t.Helper()

	kennelDir := filepath.Join(townRoot, "deacon", "dogs", name)
	if err := os.MkdirAll(kennelDir, 0755); err != nil {
		t.Fatalf("Failed to create kennel dir for %s: %v", name, err)
	}

	ds := &dog.DogState{
		Name:       name,
		State:      state,
		LastActive: lastActive,
		Worktrees:  map[string]string{},
		CreatedAt:  lastActive,
		UpdatedAt:  lastActive,
	}

	data, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal dog state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kennelDir, ".dog.json"), data, 0644); err != nil {
		t.Fatalf("Failed to write dog state: %v", err)
	}
}

// testDogExists checks if a dog directory exists in the kennel.
func testDogExists(townRoot, name string) bool {
	_, err := os.Stat(filepath.Join(townRoot, "deacon", "dogs", name, ".dog.json"))
	return err == nil
}

// testSetupWorkingDogState creates a working dog with a work assignment.
func testSetupWorkingDogState(t *testing.T, townRoot, name, work string, lastActive time.Time) {
	t.Helper()

	kennelDir := filepath.Join(townRoot, "deacon", "dogs", name)
	if err := os.MkdirAll(kennelDir, 0755); err != nil {
		t.Fatalf("Failed to create kennel dir for %s: %v", name, err)
	}

	ds := &dog.DogState{
		Name:       name,
		State:      dog.StateWorking,
		Work:       work,
		LastActive: lastActive,
		Worktrees:  map[string]string{},
		CreatedAt:  lastActive,
		UpdatedAt:  lastActive,
	}

	data, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal dog state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kennelDir, ".dog.json"), data, 0644); err != nil {
		t.Fatalf("Failed to write dog state: %v", err)
	}
}

func TestDetectStaleWorkingDogs_ClearsStaleWorkers(t *testing.T) {
	requireTmux(t)

	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Dog working for 3 hours with no activity — should be cleared.
	testSetupWorkingDogState(t, townRoot, "stale", constants.MolConvoyFeed, time.Now().Add(-3*time.Hour))

	d.detectStaleWorkingDogs(mgr, sm, &config.DaemonThresholds{})

	dg, err := mgr.Get("stale")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if dg.State != dog.StateIdle {
		t.Errorf("stale dog state = %q, want idle", dg.State)
	}
	if dg.Work != "" {
		t.Errorf("stale dog work = %q, want empty", dg.Work)
	}
}

func TestDetectStaleWorkingDogs_KillsSessionBeforeClearing(t *testing.T) {
	requireTmux(t)

	oldSocket := tmux.GetDefaultSocket()
	socketName := constants.TestSocketName("gt-test-dog-stale")
	tmux.SetDefaultSocket(socketName)
	// This test runs on its own socket rather than the package one, so it owns
	// tearing that server down: killing the last session leaves the server (and
	// its socket file) behind, which tmux.KillServer clears (gt-20di).
	t.Cleanup(func() { _ = tmux.NewTmuxWithSocket(socketName).KillServer() })
	t.Cleanup(func() { tmux.SetDefaultSocket(oldSocket) })

	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	testSetupWorkingDogState(t, townRoot, "stale", constants.MolConvoyFeed, time.Now().Add(-3*time.Hour))

	sessionName := sm.SessionName("stale")
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession(%q): %v", sessionName, err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	d.detectStaleWorkingDogs(mgr, sm, &config.DaemonThresholds{})

	dg, err := mgr.Get("stale")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if dg.State != dog.StateIdle {
		t.Errorf("stale dog state = %q, want idle", dg.State)
	}
	if dg.Work != "" {
		t.Errorf("stale dog work = %q, want empty", dg.Work)
	}
	if has, err := tm.HasSession(sessionName); err != nil {
		t.Fatalf("HasSession(%q): %v", sessionName, err)
	} else if has {
		t.Errorf("stale dog session %q still exists after cleanup", sessionName)
	}
}

func TestDetectStaleWorkingDogs_SkipsRecentWorkers(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Dog working for 30 minutes — should NOT be cleared.
	testSetupWorkingDogState(t, townRoot, "active", constants.MolConvoyFeed, time.Now().Add(-30*time.Minute))

	d.detectStaleWorkingDogs(mgr, sm, &config.DaemonThresholds{})

	dg, err := mgr.Get("active")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if dg.State != dog.StateWorking {
		t.Errorf("active dog state = %q, want working", dg.State)
	}
	if dg.Work != constants.MolConvoyFeed {
		t.Errorf("active dog work = %q, want %s", dg.Work, constants.MolConvoyFeed)
	}
}

func TestDetectStaleWorkingDogs_SkipsIdleDogs(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Idle dog with old last_active — should NOT be touched by this function.
	testSetupDogState(t, townRoot, "idle-old", dog.StateIdle, time.Now().Add(-5*time.Hour))

	d.detectStaleWorkingDogs(mgr, sm, &config.DaemonThresholds{})

	dg, err := mgr.Get("idle-old")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if dg.State != dog.StateIdle {
		t.Errorf("idle dog state = %q, want idle", dg.State)
	}
}

func TestDetectStaleWorkingDogs_EmptyKennel(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Should not panic or error with empty kennel.
	d.detectStaleWorkingDogs(mgr, sm, &config.DaemonThresholds{})
}

func TestDetectStaleWorkingDogs_Constants(t *testing.T) {
	if staleWorkingTimeout != 2*time.Hour {
		t.Errorf("staleWorkingTimeout = %v, want 2h", staleWorkingTimeout)
	}
}

func TestReapIdleDogs_SkipsWorkingDogs(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Create a working dog with old LastActive — should NOT be reaped.
	testSetupDogState(t, townRoot, "worker", dog.StateWorking, time.Now().Add(-5*time.Hour))

	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})

	if !testDogExists(townRoot, "worker") {
		t.Error("working dog should not be removed by reapIdleDogs")
	}
}

func TestReapIdleDogs_SkipsRecentlyActiveDogs(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Create idle dogs that were active recently — should NOT be reaped.
	for i := 0; i < 6; i++ {
		name := "recent-" + string(rune('a'+i))
		testSetupDogState(t, townRoot, name, dog.StateIdle, time.Now().Add(-30*time.Minute))
	}

	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})

	// All dogs should still exist.
	dogs, err := mgr.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(dogs) != 6 {
		t.Errorf("expected 6 dogs after reap, got %d", len(dogs))
	}
}

func TestReapIdleDogs_RemovesLongIdleDogsWhenPoolOversized(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows: requires tmux")
	}
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Create 6 idle dogs: 4 recent, 2 long-idle.
	// Pool is 6 > maxDogPoolSize(4), so long-idle dogs should be removed.
	for i := 0; i < 4; i++ {
		name := "recent-" + string(rune('a'+i))
		testSetupDogState(t, townRoot, name, dog.StateIdle, time.Now().Add(-10*time.Minute))
	}
	testSetupDogState(t, townRoot, "old-1", dog.StateIdle, time.Now().Add(-5*time.Hour))
	testSetupDogState(t, townRoot, "old-2", dog.StateIdle, time.Now().Add(-6*time.Hour))

	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})

	// Long-idle dogs should be removed, recent ones kept.
	dogs, err := mgr.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	if len(dogs) > maxDogPoolSize {
		t.Errorf("expected pool trimmed to at most %d, got %d", maxDogPoolSize, len(dogs))
	}

	// Verify the old dogs were removed.
	if testDogExists(townRoot, "old-1") {
		t.Error("old-1 should have been removed")
	}
	if testDogExists(townRoot, "old-2") {
		t.Error("old-2 should have been removed")
	}
}

func TestReapIdleDogs_DoesNotRemoveWhenPoolAtMaxSize(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Create exactly maxDogPoolSize idle dogs, all long-idle.
	// Pool is NOT oversized, so none should be removed.
	for i := 0; i < maxDogPoolSize; i++ {
		name := "idle-" + string(rune('a'+i))
		testSetupDogState(t, townRoot, name, dog.StateIdle, time.Now().Add(-5*time.Hour))
	}

	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})

	dogs, err := mgr.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(dogs) != maxDogPoolSize {
		t.Errorf("expected %d dogs (pool not oversized), got %d", maxDogPoolSize, len(dogs))
	}
}

func TestReapIdleDogs_StopsRemovingAtMaxPoolSize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows: requires tmux")
	}
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Create 7 idle dogs, all long-idle.
	// Should remove 3 to get down to maxDogPoolSize(4).
	for i := 0; i < 7; i++ {
		name := "dog-" + string(rune('a'+i))
		testSetupDogState(t, townRoot, name, dog.StateIdle, time.Now().Add(-5*time.Hour))
	}

	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})

	dogs, err := mgr.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(dogs) > maxDogPoolSize {
		t.Errorf("expected pool trimmed to %d, got %d", maxDogPoolSize, len(dogs))
	}
}

func TestReapIdleDogs_MixedStates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows: requires tmux")
	}
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// 2 working + 3 recent idle + 2 long-idle = 7 total.
	// Pool is oversized (7 > 4). Only long-idle IDLE dogs should be removed.
	// Working dogs are never touched.
	testSetupDogState(t, townRoot, "worker-a", dog.StateWorking, time.Now().Add(-5*time.Hour))
	testSetupDogState(t, townRoot, "worker-b", dog.StateWorking, time.Now().Add(-5*time.Hour))
	testSetupDogState(t, townRoot, "recent-a", dog.StateIdle, time.Now().Add(-10*time.Minute))
	testSetupDogState(t, townRoot, "recent-b", dog.StateIdle, time.Now().Add(-10*time.Minute))
	testSetupDogState(t, townRoot, "recent-c", dog.StateIdle, time.Now().Add(-10*time.Minute))
	testSetupDogState(t, townRoot, "old-a", dog.StateIdle, time.Now().Add(-5*time.Hour))
	testSetupDogState(t, townRoot, "old-b", dog.StateIdle, time.Now().Add(-6*time.Hour))

	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})

	// Working dogs must survive.
	if !testDogExists(townRoot, "worker-a") {
		t.Error("worker-a should not be removed")
	}
	if !testDogExists(townRoot, "worker-b") {
		t.Error("worker-b should not be removed")
	}

	// Long-idle dogs should be removed (pool was 7 > 4).
	if testDogExists(townRoot, "old-a") {
		t.Error("old-a should have been removed")
	}
	if testDogExists(townRoot, "old-b") {
		t.Error("old-b should have been removed")
	}

	// Recent idle dogs should survive.
	if !testDogExists(townRoot, "recent-a") {
		t.Error("recent-a should not be removed")
	}
}

func TestReapIdleDogs_EmptyKennel(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	// Should not panic or error with empty kennel.
	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})
}

func TestReapIdleDogs_Constants(t *testing.T) {
	if dogIdleSessionTimeout != 1*time.Hour {
		t.Errorf("dogIdleSessionTimeout = %v, want 1h", dogIdleSessionTimeout)
	}
	if dogIdleRemoveTimeout != 4*time.Hour {
		t.Errorf("dogIdleRemoveTimeout = %v, want 4h", dogIdleRemoveTimeout)
	}
	if maxDogPoolSize != 4 {
		t.Errorf("maxDogPoolSize = %d, want 4", maxDogPoolSize)
	}
}

func TestDispatchPlugins_SkipsManualGatePlugin(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	pluginDir := filepath.Join(townRoot, "plugins", "test-manual")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	pluginMD := "+++\nname = \"test-manual\"\ndescription = \"manual gate plugin\"\n\n[gate]\ntype = \"manual\"\n+++\n\n# Instructions\n"
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.md"), []byte(pluginMD), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	testSetupDogState(t, townRoot, "idle-dog", dog.StateIdle, time.Now().Add(-10*time.Minute))

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	d.dispatchPlugins(mgr, sm, rigsConfig)

	dg, err := mgr.Get("idle-dog")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dg.State != dog.StateIdle {
		t.Errorf("dog state = %q, want idle (manual-gate plugin must not auto-dispatch)", dg.State)
	}
	if dg.Work != "" {
		t.Errorf("dog work = %q, want empty (manual-gate plugin must not auto-dispatch)", dg.Work)
	}
}
func TestFindDispatchableDog_PicksFirstIdleWhenNoSessionsLive(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())
	testSetupDogState(t, townRoot, "bravo", dog.StateIdle, time.Now())

	prevHooked := dogHasHookedFormulaWithIDFn
	t.Cleanup(func() { dogHasHookedFormulaWithIDFn = prevHooked })
	dogHasHookedFormulaWithIDFn = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		return hookedFormulaResult{}, nil
	}

	mgr := dog.NewManager(townRoot, nil)
	sm := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger)
	if got == nil {
		t.Fatal("findDispatchableDog returned nil; expected an idle dog")
	}
	if got.Name != "alpha" && got.Name != "bravo" {
		t.Errorf("findDispatchableDog = %q, want alpha or bravo", got.Name)
	}
}

// The attached-formula predicate itself lives in internal/beads
// (FormulaWispIDs / FirstFormulaWisp) and is covered there — it is shared with
// `gt dog done`'s cleanup path, so it is tested once, next to the code.

// TestFindDispatchableDog_SkipsIdleDogWithHookedFormula is the gt-bygj
// regression: a dog can show registry state=idle (its session exited, or `gt
// dog done` cleared the work record) while it still holds an open hooked
// formula molecule from `gt sling`. Dispatching a NEW plugin onto that dog
// abandons the plugin the instant the dog boots and finds the stale hook —
// propulsion always runs the hook first — so the plugin never actually runs
// and the hook still never advances. findDispatchableDog must skip it.
//
// The decision logic itself (does a set of hooked beads carry
// attached_formula metadata) is covered in internal/beads. This test
// exercises findDispatchableDog's dispatch loop via the
// dogHasHookedFormulaWithIDFn and wispTreeFn seams so it never shells out to
// a real bd/Dolt backend.
func TestFindDispatchableDog_SkipsIdleDogWithHookedFormula(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())
	testSetupDogState(t, townRoot, "bravo", dog.StateIdle, time.Now())

	prevHooked := dogHasHookedFormulaWithIDFn
	t.Cleanup(func() { dogHasHookedFormulaWithIDFn = prevHooked })
	dogHasHookedFormulaWithIDFn = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		if dogName == "alpha" {
			return hookedFormulaResult{hasHooked: true, wispID: "wisp-alpha"}, nil
		}
		return hookedFormulaResult{}, nil
	}

	// No wispCreated is reported, so the wisp can never be judged abandoned —
	// which is the point of this test: a hooked dog is skipped, not reaped.
	// The tree read is stubbed to fail so the test also pins the fail-safe
	// (an unreadable tree must leave the wisp alone rather than close it).
	testStubWispTree(t, func(townRoot, wispID string) ([]beads.WispStep, error) {
		return nil, errors.New("bd unavailable")
	})
	testStubCloseStaleWisp(t, func(townRoot, wispID string, tree []beads.WispStep) (int, error) {
		t.Errorf("closeStaleWisp called for %s; an unreadable tree must not close anything", wispID)
		return 0, nil
	})

	mgr := dog.NewManager(townRoot, nil)
	sm := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger)
	if got == nil {
		t.Fatal("findDispatchableDog returned nil; expected bravo to be dispatchable")
	}
	if got.Name != "bravo" {
		t.Errorf("findDispatchableDog = %q, want bravo (alpha holds a hooked formula)", got.Name)
	}
}

// TestFindDispatchableDog_ClosesStaleWisp covers the gt-da2x recovery path:
// a dog that went idle while its formula wisp was created BEFORE the idle
// transition, and whose wisp has zero step progress, is abandoned. The
// handler must close the wisp (via the closeStaleWispFn seam) and skip the
// dog for THIS tick — with the wisp gone, the dog is dispatchable on the next
// tick. A healthy wisp (progress made, or created after the dog went idle)
// must be left alone and the dog skipped.
func TestFindDispatchableDog_ClosesStaleWisp(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	// alpha went idle 10 min ago; its wisp was created 30 min ago (before the
	// idle transition) with zero step progress — abandoned.
	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now().Add(-10*time.Minute))
	testSetupDogState(t, townRoot, "bravo", dog.StateIdle, time.Now())

	prevHooked := dogHasHookedFormulaWithIDFn
	t.Cleanup(func() { dogHasHookedFormulaWithIDFn = prevHooked })
	dogHasHookedFormulaWithIDFn = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		if dogName == "alpha" {
			return hookedFormulaResult{
				hasHooked:   true,
				wispID:      "wisp-alpha",
				wispCreated: time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
			}, nil
		}
		return hookedFormulaResult{}, nil
	}

	// Zero progress: every step is still open.
	testStubWispTree(t, func(townRoot, wispID string) ([]beads.WispStep, error) {
		return []beads.WispStep{
			{ID: "wisp-alpha"},
			{ID: "wisp-alpha.1", Status: "open"},
			{ID: "wisp-alpha.2", Status: "open"},
		}, nil
	})

	var closedTrees [][]beads.WispStep
	testStubCloseStaleWisp(t, func(townRoot, wispID string, tree []beads.WispStep) (int, error) {
		if wispID != "wisp-alpha" {
			t.Errorf("closeStaleWisp wispID = %q, want wisp-alpha", wispID)
		}
		closedTrees = append(closedTrees, tree)
		return len(tree), nil
	})

	mgr := dog.NewManager(townRoot, nil)
	sm := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger)
	if got == nil {
		t.Fatal("findDispatchableDog returned nil; expected bravo to be dispatchable")
	}
	if got.Name != "bravo" {
		t.Errorf("findDispatchableDog = %q, want bravo (alpha's stale wisp should have been closed and skipped)", got.Name)
	}
	if len(closedTrees) != 1 {
		t.Fatalf("closeStaleWisp calls = %d, want exactly 1", len(closedTrees))
	}
	if len(closedTrees[0]) != 3 || closedTrees[0][0].ID != "wisp-alpha" {
		t.Errorf("closeStaleWisp tree = %+v, want the tree the handler read (root wisp-alpha plus its 2 steps)", closedTrees[0])
	}
}

// TestFindDispatchableDog_KeepsProgressedWisp confirms the converse of
// ClosesStaleWisp: when the wisp's steps have progress (the dog bailed
// mid-formula), the wisp is NOT closed — it is left for the reaper/deacon
// patrol, and the dog is skipped for new plugin dispatch.
func TestFindDispatchableDog_KeepsProgressedWisp(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now().Add(-10*time.Minute))

	prevHooked := dogHasHookedFormulaWithIDFn
	t.Cleanup(func() { dogHasHookedFormulaWithIDFn = prevHooked })
	dogHasHookedFormulaWithIDFn = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		return hookedFormulaResult{
			hasHooked:   true,
			wispID:      "wisp-alpha",
			wispCreated: time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		}, nil
	}

	// Mid-formula: the dog started step 1 and bailed.
	testStubWispTree(t, func(townRoot, wispID string) ([]beads.WispStep, error) {
		return []beads.WispStep{
			{ID: "wisp-alpha"},
			{ID: "wisp-alpha.1", Status: "in_progress"},
			{ID: "wisp-alpha.2", Status: "open"},
		}, nil
	})

	var closedIDs []string
	testStubCloseStaleWisp(t, func(townRoot, wispID string, tree []beads.WispStep) (int, error) {
		closedIDs = append(closedIDs, wispID)
		return len(tree), nil
	})

	mgr := dog.NewManager(townRoot, nil)
	sm := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger)
	if got != nil {
		t.Errorf("findDispatchableDog = %q, want nil (alpha's progressed wisp must not be closed or dispatched onto)", got.Name)
	}
	if len(closedIDs) != 0 {
		t.Errorf("closeStaleWisp calls = %v, want none (wisp has step progress)", closedIDs)
	}
}

// TestFindDispatchableDog_NilWhenAllDogsHooked confirms that when every idle
// dog in the kennel holds an open hooked formula molecule, findDispatchableDog
// returns nil rather than falling back to picking one anyway.
func TestFindDispatchableDog_NilWhenAllDogsHooked(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())
	testSetupDogState(t, townRoot, "bravo", dog.StateIdle, time.Now())

	prevHooked := dogHasHookedFormulaWithIDFn
	t.Cleanup(func() { dogHasHookedFormulaWithIDFn = prevHooked })
	dogHasHookedFormulaWithIDFn = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		return hookedFormulaResult{hasHooked: true, wispID: "wisp-" + dogName}, nil
	}

	// An empty tree, and no wispCreated, so neither dog's wisp is abandoned.
	testStubWispTree(t, func(townRoot, wispID string) ([]beads.WispStep, error) {
		return []beads.WispStep{{ID: wispID}}, nil
	})
	testStubCloseStaleWisp(t, func(townRoot, wispID string, tree []beads.WispStep) (int, error) {
		t.Errorf("closeStaleWisp called for %s; no wisp here is abandoned", wispID)
		return 0, nil
	})

	mgr := dog.NewManager(townRoot, nil)
	sm := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger)
	if got != nil {
		t.Errorf("findDispatchableDog = %q, want nil (every dog holds a hooked formula)", got.Name)
	}
}

// TestFindDispatchableDog_ErrorFallsBackToDispatchable confirms that a
// dogHasHookedFormulaWithIDFn error is treated as "not hooked" rather than
// disqualifying the dog — a flaky hook check must not wedge dispatch.
func TestFindDispatchableDog_ErrorFallsBackToDispatchable(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())

	prevHooked := dogHasHookedFormulaWithIDFn
	t.Cleanup(func() { dogHasHookedFormulaWithIDFn = prevHooked })
	dogHasHookedFormulaWithIDFn = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		return hookedFormulaResult{}, fmt.Errorf("simulated bd failure")
	}

	mgr := dog.NewManager(townRoot, nil)
	sm := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger)
	if got == nil {
		t.Fatal("findDispatchableDog returned nil; expected alpha to be dispatchable despite the hook-check error")
	}
	if got.Name != "alpha" {
		t.Errorf("findDispatchableDog = %q, want alpha", got.Name)
	}
}

func TestCleanupStuckDogs_ClearsDeadSessionWorker(t *testing.T) {
	requireTmux(t)

	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)

	testSetupWorkingDogState(t, townRoot, "alpha", constants.MolDogReaper, time.Now())

	d.cleanupStuckDogs(mgr, sm)

	dg, err := mgr.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if dg.State != dog.StateIdle {
		t.Errorf("stuck dog state = %q, want idle", dg.State)
	}
	if dg.Work != "" {
		t.Errorf("stuck dog work = %q, want empty", dg.Work)
	}
}

func TestCleanupStuckDogs_ClearsAgentDeadWorker(t *testing.T) {
	requireTmux(t)

	oldSocket := tmux.GetDefaultSocket()
	socketName := constants.TestSocketName("gt-test-dog-cleanup")
	tmux.SetDefaultSocket(socketName)
	// Same as TestDetectStaleWorkingDogs_KillsSessionBeforeClearing: this test
	// owns the server it binds, so it tears it down (gt-20di).
	t.Cleanup(func() { _ = tmux.NewTmuxWithSocket(socketName).KillServer() })
	t.Cleanup(func() { tmux.SetDefaultSocket(oldSocket) })

	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	tm := tmux.NewTmux()
	sm := dog.NewSessionManager(tm, townRoot, mgr)

	testSetupWorkingDogState(t, townRoot, "alpha", constants.MolDogReaper, time.Now())

	sessionName := sm.SessionName("alpha")
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession(%q): %v", sessionName, err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })
	time.Sleep(200 * time.Millisecond)

	d.cleanupStuckDogs(mgr, sm)

	dg, err := mgr.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if dg.State != dog.StateIdle {
		t.Errorf("agent-dead dog state = %q, want idle", dg.State)
	}
	if dg.Work != "" {
		t.Errorf("agent-dead dog work = %q, want empty", dg.Work)
	}
	if has, err := tm.HasSession(sessionName); err != nil {
		t.Fatalf("HasSession(%q): %v", sessionName, err)
	} else if has {
		t.Errorf("agent-dead session %q still exists after cleanup", sessionName)
	}
}

func TestCleanupStuckDogs_SkipsIdleDogs(t *testing.T) {
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())

	d.cleanupStuckDogs(mgr, sm)

	dg, err := mgr.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if dg.State != dog.StateIdle {
		t.Errorf("idle dog state = %q, want idle", dg.State)
	}
}

// --- gt-da2x: hermetic coverage of the real read/close bodies ---------------
//
// The tests above stub wispTreeFn/closeStaleWispFn, which is what makes the
// dispatch loop testable — but a seam that replaces the body leaves the body
// itself unverified. These drive the production functions against a fake bd
// so the code that actually runs in the daemon is what the assertions cover.

// writeFakeBdForHandler installs a mock `bd` in binDir that appends its argv
// and its bd read/write mode env to logPath before running body. Logging the
// mode is the point: gt-da2x's first finding was that the wisp reads and
// closes reintroduced gh#3596 connection churn by running without the daemon's
// routing env, so the tests assert on the mode rather than trusting it.
func writeFakeBdForHandler(t *testing.T, binDir, logPath, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script; skipping on Windows")
	}
	script := "#!/bin/sh\n" +
		"printf '%s|BD_DOLT_AUTO_COMMIT=%s|BD_READONLY=%s\\n' \"$*\" \"$BD_DOLT_AUTO_COMMIT\" \"$BD_READONLY\" >> \"" + logPath + "\"\n" +
		body
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeBdCallsForHandler returns the argv and bd mode of each recorded call.
func fakeBdCallsForHandler(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading fake bd log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

const fakeWispTreeScriptBody = `if [ "$1" = "show" ]; then
  case "$2" in
    wisp-alpha)
      echo '{"wisp-alpha":[{"id":"wisp-alpha.1","status":"open"},{"id":"wisp-alpha.2","status":"open"}],"schema_version":1}'
      exit 0
      ;;
  esac
  echo '{"schema_version":1}'
  exit 0
fi
exit 0
`

// TestWispTree_RealBodyReadsChildrenReadOnly covers wispTree's body: it must
// find ephemeral wisp steps (via `bd show --children`, which sees the
// wisp_dependencies table that `bd children` misses) and it must do so with
// BD_DOLT_AUTO_COMMIT=off. That read runs once per idle dog per dispatch tick;
// with auto-commit on it opens a connection attempting a no-op commit every
// time (gh#3596).
func TestWispTree_RealBodyReadsChildrenReadOnly(t *testing.T) {
	townRoot := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBdForHandler(t, t.TempDir(), logPath, fakeWispTreeScriptBody)

	tree, err := wispTree(townRoot, "wisp-alpha")
	if err != nil {
		t.Fatalf("wispTree: %v", err)
	}
	want := []beads.WispStep{
		{ID: "wisp-alpha"},
		{ID: "wisp-alpha.1", Status: "open"},
		{ID: "wisp-alpha.2", Status: "open"},
	}
	if len(tree) != len(want) {
		t.Fatalf("wispTree() = %+v, want %+v", tree, want)
	}
	for i := range want {
		if tree[i] != want[i] {
			t.Fatalf("wispTree()[%d] = %+v, want %+v", i, tree[i], want[i])
		}
	}

	calls := fakeBdCallsForHandler(t, logPath)
	if len(calls) == 0 {
		t.Fatal("fake bd was never invoked")
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "show wisp-alpha") || !strings.Contains(call, "--children --json") {
			t.Errorf("call = %q, want a `bd show <id> --children --json` read", call)
		}
		if !strings.Contains(call, "BD_DOLT_AUTO_COMMIT=off") || !strings.Contains(call, "BD_READONLY=true") {
			t.Errorf("call = %q, want the daemon's read-only routing env (BD_DOLT_AUTO_COMMIT=off, BD_READONLY=true)", call)
		}
	}
}

// TestCloseStaleWisp_RealBodyClosesDeepestFirstWithMutationEnv covers
// closeStaleWisp's body: children are closed before the root (a parent closed
// while its child survives strands the child — gt-7lx3), already-closed steps
// are skipped, and the write runs with auto-commit on so the closes are not
// stranded in a daemon subprocess context.
func TestCloseStaleWisp_RealBodyClosesDeepestFirstWithMutationEnv(t *testing.T) {
	townRoot := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBdForHandler(t, t.TempDir(), logPath, "if [ \"$1\" = \"close\" ]; then\n  exit 0\nfi\nexit 0\n")

	tree := []beads.WispStep{
		{ID: "wisp-alpha"},
		{ID: "wisp-alpha.1", Status: "open"},
		{ID: "wisp-alpha.2", Status: "closed"},
		{ID: "wisp-alpha.1.1", Status: "open"},
	}
	closed, err := closeStaleWisp(townRoot, "wisp-alpha", tree)
	if err != nil {
		t.Fatalf("closeStaleWisp: %v", err)
	}
	if closed != 3 {
		t.Errorf("closeStaleWisp() closed = %d, want 3 (the root and the two open steps)", closed)
	}

	calls := fakeBdCallsForHandler(t, logPath)
	want := "close wisp-alpha.1.1 wisp-alpha.1 wisp-alpha --force --reason " + wispAbandonedReason +
		"|BD_DOLT_AUTO_COMMIT=on|BD_READONLY="
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("fake bd calls = %q, want exactly [%q]", calls, want)
	}
}

// TestCloseStaleWisp_RealBodySurfacesCloseFailure pins the return path the
// handler logs on: a failed bd close must not be reported as a successful reap.
func TestCloseStaleWisp_RealBodySurfacesCloseFailure(t *testing.T) {
	townRoot := t.TempDir()
	writeFakeBdForHandler(t, t.TempDir(), filepath.Join(t.TempDir(), "bd.log"), "echo 'dolt unreachable' >&2\nexit 1\n")

	closed, err := closeStaleWisp(townRoot, "wisp-alpha", []beads.WispStep{{ID: "wisp-alpha"}})
	if err == nil {
		t.Fatal("closeStaleWisp() error = nil, want an error when bd close fails")
	}
	if closed != 0 {
		t.Errorf("closeStaleWisp() closed = %d, want 0 on failure", closed)
	}
	if !strings.Contains(err.Error(), "dolt unreachable") {
		t.Errorf("closeStaleWisp() error = %v, want it to carry bd's stderr", err)
	}
}

// TestIsWispStale covers the guard that decides whether to reap a hooked wisp.
// The timestamp cases are the regression that motivated the shared
// RFC3339Nano-first parser: bd/Dolt emit fractional seconds, and parsing with
// plain RFC3339 made every comparison fail, which silently disabled the whole
// recovery path (fail-safe, but inert).
func TestIsWispStale(t *testing.T) {
	idleSince := time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		dog         *dog.Dog
		wispCreated string
		tree        []beads.WispStep
		want        bool
	}{
		{
			name:        "no progress and created before the dog went idle",
			dog:         &dog.Dog{Name: "alpha", LastActive: idleSince},
			wispCreated: "2026-09-18T10:30:00Z",
			tree:        []beads.WispStep{{ID: "w"}, {ID: "w.1", Status: "open"}},
			want:        true,
		},
		{
			name:        "fractional-second created_at still parses",
			dog:         &dog.Dog{Name: "alpha", LastActive: idleSince},
			wispCreated: "2026-09-18T10:30:00.123456789Z",
			tree:        []beads.WispStep{{ID: "w"}, {ID: "w.1", Status: "open"}},
			want:        true,
		},
		{
			name:        "unparseable created_at is never stale",
			dog:         &dog.Dog{Name: "alpha", LastActive: idleSince},
			wispCreated: "not a timestamp",
			tree:        []beads.WispStep{{ID: "w"}, {ID: "w.1", Status: "open"}},
			want:        false,
		},
		{
			name:        "empty created_at is never stale",
			dog:         &dog.Dog{Name: "alpha", LastActive: idleSince},
			wispCreated: "",
			tree:        []beads.WispStep{{ID: "w"}, {ID: "w.1", Status: "open"}},
			want:        false,
		},
		{
			name:        "wisp created after the dog went idle is not abandoned",
			dog:         &dog.Dog{Name: "alpha", LastActive: idleSince},
			wispCreated: "2026-09-18T11:30:00Z",
			tree:        []beads.WispStep{{ID: "w"}, {ID: "w.1", Status: "open"}},
			want:        false,
		},
		{
			name:        "a step in progress means the dog did not bail",
			dog:         &dog.Dog{Name: "alpha", LastActive: idleSince},
			wispCreated: "2026-09-18T10:30:00Z",
			tree:        []beads.WispStep{{ID: "w"}, {ID: "w.1", Status: "in_progress"}, {ID: "w.2", Status: "open"}},
			want:        false,
		},
		{
			name:        "a closed step counts as progress",
			dog:         &dog.Dog{Name: "alpha", LastActive: idleSince},
			wispCreated: "2026-09-18T10:30:00Z",
			tree:        []beads.WispStep{{ID: "w"}, {ID: "w.1", Status: "closed"}},
			want:        false,
		},
		{
			name:        "root with no children at all",
			dog:         &dog.Dog{Name: "alpha", LastActive: idleSince},
			wispCreated: "2026-09-18T10:30:00Z",
			tree:        []beads.WispStep{{ID: "w"}},
			want:        true,
		},
		{
			name:        "a zero LastActive (never recorded) is not proof of idleness",
			dog:         &dog.Dog{Name: "alpha"},
			wispCreated: "2026-09-18T10:30:00Z",
			tree:        []beads.WispStep{{ID: "w"}, {ID: "w.1", Status: "open"}},
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hookedFormulaResult{hasHooked: true, wispID: "w", wispCreated: tt.wispCreated}
			if got := isWispStale(tt.dog, result, tt.tree); got != tt.want {
				t.Errorf("isWispStale() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDogHasHookedFormulaWithID_RealBody covers the discovery read the daemon
// runs per idle dog per tick: it must shell out with the read-only routing env
// and pick out the attached-formula wisp (not any hooked bead) so the caller
// gets an ID it can actually reap.
func TestDogHasHookedFormulaWithID_RealBody(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bd.log")
	// The response goes through a quoted heredoc, not echo: sh's echo
	// interprets \n, which would turn the JSON escape in the description into
	// a literal newline and make the payload unparseable.
	writeFakeBdForHandler(t, t.TempDir(), logPath, `
if [ "$1" = "query" ]; then
  cat <<'EOF'
[{"id":"hq-task","status":"hooked","description":"unrelated"},{"id":"hq-wisp-admuv","status":"hooked","description":"attached_formula: mol-dog-reaper\n","created_at":"2026-09-18T11:31:07.289Z"}]
EOF
  exit 0
fi
echo '[]'
exit 0
`)

	result, err := dogHasHookedFormulaWithID(t.TempDir(), t.TempDir(), "alpha")
	if err != nil {
		t.Fatalf("dogHasHookedFormulaWithID: %v", err)
	}
	if !result.hasHooked {
		t.Fatal("hasHooked = false, want true")
	}
	if result.wispID != "hq-wisp-admuv" {
		t.Errorf("wispID = %q, want hq-wisp-admuv (the formula wisp, not the plain hooked bead)", result.wispID)
	}
	if result.wispCreated != "2026-09-18T11:31:07.289Z" {
		t.Errorf("wispCreated = %q, want the bead's created_at", result.wispCreated)
	}

	calls := fakeBdCallsForHandler(t, logPath)
	if len(calls) != 1 {
		t.Fatalf("fake bd calls = %q, want exactly one query", calls)
	}
	if !strings.Contains(calls[0], "BD_DOLT_AUTO_COMMIT=off") || !strings.Contains(calls[0], "BD_READONLY=true") {
		t.Errorf("call = %q, want the daemon's read-only routing env", calls[0])
	}
}

// TestDogHasHookedFormulaWithID_RealBodyNoFormulaWisp confirms a dog whose
// hook holds only non-formula work is reported as not hooked, so the caller
// does not try to reap someone's unrelated hooked bead.
func TestDogHasHookedFormulaWithID_RealBodyNoFormulaWisp(t *testing.T) {
	writeFakeBdForHandler(t, t.TempDir(), filepath.Join(t.TempDir(), "bd.log"), `
if [ "$1" = "query" ]; then
  echo '[{"id":"hq-task","status":"hooked","description":"unrelated"}]'
  exit 0
fi
echo '[]'
exit 0
`)

	result, err := dogHasHookedFormulaWithID(t.TempDir(), t.TempDir(), "alpha")
	if err != nil {
		t.Fatalf("dogHasHookedFormulaWithID: %v", err)
	}
	if result.hasHooked {
		t.Errorf("hasHooked = true (wispID %q), want false: no bead carried attached_formula", result.wispID)
	}
}

// TestWispTreeTimeoutIsBounded guards the context the daemon hands down: a
// wedged bd (one that never exits) must be bounded by
// dogHookedFormulaCheckTimeout rather than stalling the dispatch cycle.
func TestWispTreeTimeoutIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script; skipping on Windows")
	}
	binDir := t.TempDir()
	// A bd that hangs forever: sleep is on PATH and blocks past the timeout.
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatalf("writing hanging fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	_, err := wispTree(t.TempDir(), "wisp-x")
	if err == nil {
		t.Fatal("wispTree() error = nil, want a timeout error from a hung bd")
	}
	if elapsed := time.Since(start); elapsed > 3*dogHookedFormulaCheckTimeout {
		t.Errorf("wispTree() took %v, want it bounded near dogHookedFormulaCheckTimeout (%v)", elapsed, dogHookedFormulaCheckTimeout)
	}
}
