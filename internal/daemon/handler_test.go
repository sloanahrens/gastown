package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// testHandlerDaemon creates a minimal Daemon with a logger for handler tests.
func testHandlerDaemon(t *testing.T, townRoot string) *Daemon {
	t.Helper()
	return &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   discardLogger,
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
	}
}

// fakeDogSessions is dogSessions over a test daemon's fakeTmux. Sessions are
// named exactly as dog.SessionManager names them.
type fakeDogSessions struct {
	tm      *fakeTmux
	started []string
}

func handlerDogSessions(d *Daemon) *fakeDogSessions {
	return &fakeDogSessions{tm: d.tmux.(*fakeTmux)}
}

func (f *fakeDogSessions) SessionName(dogName string) string {
	return dog.NewSessionManager(nil, "", nil).SessionName(dogName)
}

func (f *fakeDogSessions) IsRunning(dogName string) (bool, error) {
	return f.tm.HasSession(f.SessionName(dogName))
}

func (f *fakeDogSessions) Start(dogName string, _ dog.SessionStartOptions) error {
	f.started = append(f.started, dogName)
	return f.tm.NewSessionWithCommandAndEnv(f.SessionName(dogName), "", "claude", nil)
}

func (f *fakeDogSessions) Stop(dogName string, _ bool) error {
	if running, _ := f.IsRunning(dogName); !running {
		return dog.ErrSessionNotFound
	}
	return f.tm.KillSession(f.SessionName(dogName))
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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

	testSetupWorkingDogState(t, townRoot, "stale", constants.MolConvoyFeed, time.Now().Add(-3*time.Hour))

	sessionName := sm.SessionName("stale")
	tm := sm.tm
	tm.addSession(sessionName, "claude", time.Now().Add(-3*time.Hour))

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

	// Should not panic or error with empty kennel.
	d.detectStaleWorkingDogs(mgr, sm, &config.DaemonThresholds{})
}

func TestDetectStaleWorkingDogs_Constants(t *testing.T) {
	t.Parallel()
	if staleWorkingTimeout != 2*time.Hour {
		t.Errorf("staleWorkingTimeout = %v, want 2h", staleWorkingTimeout)
	}
}

func TestReapIdleDogs_SkipsWorkingDogs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

	// Create a working dog with old LastActive — should NOT be reaped.
	testSetupDogState(t, townRoot, "worker", dog.StateWorking, time.Now().Add(-5*time.Hour))

	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})

	if !testDogExists(townRoot, "worker") {
		t.Error("working dog should not be removed by reapIdleDogs")
	}
}

func TestReapIdleDogs_SkipsRecentlyActiveDogs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

	// Should not panic or error with empty kennel.
	d.reapIdleDogs(mgr, sm, &config.DaemonThresholds{})
}

func TestReapIdleDogs_Constants(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())
	testSetupDogState(t, townRoot, "bravo", dog.StateIdle, time.Now())

	var wisps wispOps
	wisps.hooked = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		return hookedFormulaResult{}, nil
	}

	mgr := dog.NewManager(townRoot, nil)
	sm := handlerDogSessions(d)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger, wisps)
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
// wispOps seams (hooked and tree) so it never shells out to
// a real bd/Dolt backend.
func TestFindDispatchableDog_SkipsIdleDogWithHookedFormula(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())
	testSetupDogState(t, townRoot, "bravo", dog.StateIdle, time.Now())

	var wisps wispOps
	wisps.hooked = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		if dogName == "alpha" {
			return hookedFormulaResult{hasHooked: true, wispID: "wisp-alpha"}, nil
		}
		return hookedFormulaResult{}, nil
	}

	// No wispCreated is reported, so the wisp can never be judged abandoned —
	// which is the point of this test: a hooked dog is skipped, not reaped.
	// The tree read is stubbed to fail so the test also pins the fail-safe
	// (an unreadable tree must leave the wisp alone rather than close it).
	wisps.tree = func(townRoot, wispID string) ([]beads.WispStep, error) {
		return nil, errors.New("bd unavailable")
	}
	wisps.closeStale = func(townRoot, wispID string, tree []beads.WispStep) (int, error) {
		t.Errorf("closeStaleWisp called for %s; an unreadable tree must not close anything", wispID)
		return 0, nil
	}

	mgr := dog.NewManager(townRoot, nil)
	sm := handlerDogSessions(d)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger, wisps)
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
// handler must close the wisp (via the wispOps close seam) and skip the
// dog for THIS tick — with the wisp gone, the dog is dispatchable on the next
// tick. A healthy wisp (progress made, or created after the dog went idle)
// must be left alone and the dog skipped.
func TestFindDispatchableDog_ClosesStaleWisp(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	// alpha went idle 10 min ago; its wisp was created 30 min ago (before the
	// idle transition) with zero step progress — abandoned.
	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now().Add(-10*time.Minute))
	testSetupDogState(t, townRoot, "bravo", dog.StateIdle, time.Now())

	var wisps wispOps
	wisps.hooked = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
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
	wisps.tree = func(townRoot, wispID string) ([]beads.WispStep, error) {
		return []beads.WispStep{
			{ID: "wisp-alpha"},
			{ID: "wisp-alpha.1", Status: "open"},
			{ID: "wisp-alpha.2", Status: "open"},
		}, nil
	}

	var closedTrees [][]beads.WispStep
	wisps.closeStale = func(townRoot, wispID string, tree []beads.WispStep) (int, error) {
		if wispID != "wisp-alpha" {
			t.Errorf("closeStaleWisp wispID = %q, want wisp-alpha", wispID)
		}
		closedTrees = append(closedTrees, tree)
		return len(tree), nil
	}

	mgr := dog.NewManager(townRoot, nil)
	sm := handlerDogSessions(d)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger, wisps)
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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now().Add(-10*time.Minute))

	var wisps wispOps
	wisps.hooked = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		return hookedFormulaResult{
			hasHooked:   true,
			wispID:      "wisp-alpha",
			wispCreated: time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		}, nil
	}

	// Mid-formula: the dog started step 1 and bailed.
	wisps.tree = func(townRoot, wispID string) ([]beads.WispStep, error) {
		return []beads.WispStep{
			{ID: "wisp-alpha"},
			{ID: "wisp-alpha.1", Status: "in_progress"},
			{ID: "wisp-alpha.2", Status: "open"},
		}, nil
	}

	var closedIDs []string
	wisps.closeStale = func(townRoot, wispID string, tree []beads.WispStep) (int, error) {
		closedIDs = append(closedIDs, wispID)
		return len(tree), nil
	}

	mgr := dog.NewManager(townRoot, nil)
	sm := handlerDogSessions(d)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger, wisps)
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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())
	testSetupDogState(t, townRoot, "bravo", dog.StateIdle, time.Now())

	var wisps wispOps
	wisps.hooked = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		return hookedFormulaResult{hasHooked: true, wispID: "wisp-" + dogName}, nil
	}

	// An empty tree, and no wispCreated, so neither dog's wisp is abandoned.
	wisps.tree = func(townRoot, wispID string) ([]beads.WispStep, error) {
		return []beads.WispStep{{ID: wispID}}, nil
	}
	wisps.closeStale = func(townRoot, wispID string, tree []beads.WispStep) (int, error) {
		t.Errorf("closeStaleWisp called for %s; no wisp here is abandoned", wispID)
		return 0, nil
	}

	mgr := dog.NewManager(townRoot, nil)
	sm := handlerDogSessions(d)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger, wisps)
	if got != nil {
		t.Errorf("findDispatchableDog = %q, want nil (every dog holds a hooked formula)", got.Name)
	}
}

// TestFindDispatchableDog_ErrorFallsBackToDispatchable confirms that a
// hooked-formula check error is treated as "not hooked" rather than
// disqualifying the dog — a flaky hook check must not wedge dispatch.
func TestFindDispatchableDog_ErrorFallsBackToDispatchable(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	testSetupDogState(t, townRoot, "alpha", dog.StateIdle, time.Now())

	var wisps wispOps
	wisps.hooked = func(townRoot, beadsDir, dogName string) (hookedFormulaResult, error) {
		return hookedFormulaResult{}, fmt.Errorf("simulated bd failure")
	}

	mgr := dog.NewManager(townRoot, nil)
	sm := handlerDogSessions(d)

	got := findDispatchableDog(mgr, sm, townRoot, d.logger, wisps)
	if got == nil {
		t.Fatal("findDispatchableDog returned nil; expected alpha to be dispatchable despite the hook-check error")
	}
	if got.Name != "alpha" {
		t.Errorf("findDispatchableDog = %q, want alpha", got.Name)
	}
}

func TestCleanupStuckDogs_ClearsDeadSessionWorker(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

	testSetupWorkingDogState(t, townRoot, "alpha", constants.MolDogReaper, time.Now())

	// The session is up but its pane is back at a shell: the agent died.
	sessionName := sm.SessionName("alpha")
	tm := sm.tm
	tm.addSession(sessionName, "", time.Now())

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
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)

	rigsConfig := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	mgr := dog.NewManager(townRoot, rigsConfig)
	sm := handlerDogSessions(d)

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

// --- gt-da2x: coverage of the real read/close bodies -------------------------
//
// The tests above stub wispOps' tree and close functions, which is what makes the
// dispatch loop testable — but a seam that replaces the body leaves the body
// itself unverified. dogHasHookedFormulaWithID's body runs here on a fakeCLI
// bd; wispTree and closeStaleWisp run bd inside internal/beads, so their
// bodies are pinned in handler_bd_integration_test.go.

// TestIsWispStale covers the guard that decides whether to reap a hooked wisp.
// The timestamp cases are the regression that motivated the shared
// RFC3339Nano-first parser: bd/Dolt emit fractional seconds, and parsing with
// plain RFC3339 made every comparison fail, which silently disabled the whole
// recovery path (fail-safe, but inert).
func TestIsWispStale(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	bd := newFakeCLI(cliBySub(map[string]cliReply{
		"query": {stdout: `[{"id":"hq-task","status":"hooked","description":"unrelated"},{"id":"hq-wisp-admuv","status":"hooked","description":"attached_formula: mol-dog-reaper\n","created_at":"2026-09-18T11:31:07.289Z"}]`},
	}))

	result, err := dogHasHookedFormulaWithIDRun(bd.run, t.TempDir(), t.TempDir(), "alpha")
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

	calls := bd.recorded()
	if len(calls) != 1 || len(calls[0].args) == 0 || calls[0].args[0] != "query" {
		t.Fatalf("fake bd calls = %+v, want exactly one query", calls)
	}
	if got, ro := calls[0].getenv("BD_DOLT_AUTO_COMMIT"), calls[0].getenv("BD_READONLY"); got != "off" || ro != "true" {
		t.Errorf("query ran with BD_DOLT_AUTO_COMMIT=%q BD_READONLY=%q, want the daemon's read-only routing env (off, true)", got, ro)
	}
}

// TestDogHasHookedFormulaWithID_RealBodyNoFormulaWisp confirms a dog whose
// hook holds only non-formula work is reported as not hooked, so the caller
// does not try to reap someone's unrelated hooked bead.
func TestDogHasHookedFormulaWithID_RealBodyNoFormulaWisp(t *testing.T) {
	t.Parallel()
	bd := newFakeCLI(cliBySub(map[string]cliReply{
		"query": {stdout: `[{"id":"hq-task","status":"hooked","description":"unrelated"}]`},
	}))

	result, err := dogHasHookedFormulaWithIDRun(bd.run, t.TempDir(), t.TempDir(), "alpha")
	if err != nil {
		t.Fatalf("dogHasHookedFormulaWithID: %v", err)
	}
	if result.hasHooked {
		t.Errorf("hasHooked = true (wispID %q), want false: no bead carried attached_formula", result.wispID)
	}
}
