package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/slot"
)

func TestMainBranchIntegrationCommandFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		makefile string // "" means no Makefile at all
		want     string
	}{
		{"no makefile", "", ""},
		{"target", "build:\n\tgo build\n\ntest-integration:\n\tgo test -tags integration ./...\n", "make test-integration"},
		{"target with prerequisites", "test-integration: build\n\ttrue\n", "make test-integration"},
		{"target before a space", "test-integration :\n\ttrue\n", "make test-integration"},
		{"no such target", "test:\n\tgo test ./...\n", ""},
		{"longer target name", "test-integration-slow:\n\ttrue\n", ""},
		{"variable, not a rule", "test-integration := x\n", ""},
		{"named in a recipe only", "ci:\n\t$(MAKE) test-integration:\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.makefile != "" {
				if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(tc.makefile), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := integrationCommandFor(dir); got != tc.want {
				t.Errorf("integrationCommandFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMainBranchIntegrationIntervalAndTimeout(t *testing.T) {
	t.Parallel()
	cfg := func(interval, timeout string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{MainBranchTest: &MainBranchTestConfig{
			Enabled: true, IntegrationIntervalStr: interval, IntegrationTimeoutStr: timeout,
		}}}
	}
	if got := mainBranchIntegrationInterval(nil); got != 24*time.Hour {
		t.Errorf("default interval = %v, want 24h (on by default: no live config change needed)", got)
	}
	if got := mainBranchIntegrationInterval(cfg("", "")); got != 24*time.Hour {
		t.Errorf("unset interval = %v, want 24h", got)
	}
	if got := mainBranchIntegrationInterval(cfg("6h", "")); got != 6*time.Hour {
		t.Errorf("interval = %v, want 6h", got)
	}
	for _, off := range []string{"0s", "-1h", "daily"} {
		if got := mainBranchIntegrationInterval(cfg(off, "")); got != 0 {
			t.Errorf("interval %q = %v, want 0 (off, never the default)", off, got)
		}
	}
	if got := mainBranchIntegrationTimeout(nil); got != 30*time.Minute {
		t.Errorf("default timeout = %v, want 30m", got)
	}
	if got := mainBranchIntegrationTimeout(cfg("", "45m")); got != 45*time.Minute {
		t.Errorf("timeout = %v, want 45m", got)
	}
	if got := mainBranchIntegrationTimeout(cfg("", "bad")); got != 30*time.Minute {
		t.Errorf("invalid timeout = %v, want the 30m default", got)
	}
}

// integrationWorkDir is a worktree whose Makefile declares a test-integration
// target.
func integrationWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mk := "test-integration:\n\t@true\n"
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(mk), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// integrationDaemon is newMainBranchTestDaemon with its gate commands
// answered by gate.
func integrationDaemon(townRoot string, logged *bytes.Buffer, cfg *MainBranchTestConfig, gate *gateShell) *Daemon {
	d := newMainBranchTestDaemon(townRoot, logged, cfg)
	d.execCmd = gate.run
	return d
}

func TestRunRigIntegration_FailureEscalatesUnderItsOwnKey(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	workDir := integrationWorkDir(t)
	rec := notifyfake.New()
	var logged bytes.Buffer
	gate := newGateShell(gateExit("--- FAIL: TestIntegrationWidget (0.01s)\n", 2))
	d := integrationDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true}, gate)
	d.notifier = rec

	d.runRigIntegration(context.Background(), "gastown", "deadbeef", workDir)

	if got := gate.ran(); !slices.Equal(got, []string{"make test-integration"}) || gate.dirs[0] != workDir {
		t.Fatalf("ran %q in %q, want make test-integration in the worktree", got, gate.dirs)
	}
	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("want one escalation, got %+v\nlog:\n%s", rec.Calls(), logged.String())
	}
	if got := esc[0].Escalation.Fingerprint; got != mainBranchIntegrationAlertKey("gastown") {
		t.Errorf("fingerprint = %q, want the rig's integration key (not %q)", got, alertKeyMainBranchTest)
	}
	if !strings.Contains(esc[0].Escalation.Reason, "TestIntegrationWidget") {
		t.Errorf("escalation must carry the failing test:\n%s", esc[0].Escalation.Reason)
	}
	if len(rec.Clears()) != 0 {
		t.Errorf("a red run cleared an alert: %+v", rec.Clears())
	}
	if _, ok, err := loadPatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("gastown")); err != nil || !ok {
		t.Errorf("a finished red run must record its last run (ok=%v err=%v), or it re-runs every cycle", ok, err)
	}
}

func TestRunRigIntegration_PassClearsItsAlertAndRecordsTheRun(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	workDir := integrationWorkDir(t)
	rec := notifyfake.New()
	var logged bytes.Buffer
	d := integrationDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true}, newGateShell(gateExit("", 0)))
	d.notifier = rec

	d.runRigIntegration(context.Background(), "gastown", "deadbeef", workDir)

	if len(rec.Escalations()) != 0 {
		t.Fatalf("a green run escalated: %+v", rec.Escalations())
	}
	clears := rec.Clears()
	if len(clears) != 1 || !slices.Equal(clears[0].Fingerprints, []string{mainBranchIntegrationAlertKey("gastown")}) {
		t.Errorf("a green run must clear exactly the rig's integration alert, got %+v", clears)
	}
	if _, ok, _ := loadPatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("gastown")); !ok {
		t.Error("a green run must record its last run")
	}
}

func TestRunRigIntegration_NotDueDoesNotRun(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	workDir := integrationWorkDir(t)
	if err := savePatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("gastown"), time.Now()); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	gate := newGateShell(gateExit("", 0))
	d := integrationDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true}, gate)

	d.runRigIntegration(context.Background(), "gastown", "deadbeef", workDir)

	if got := gate.ran(); len(got) != 0 {
		t.Fatalf("the integration tier ran again inside its interval: %q", got)
	}
	if !strings.Contains(logged.String(), "integration not due") {
		t.Errorf("a skipped run must say why:\n%s", logged.String())
	}
}

func TestRunRigIntegration_PerRigCadence(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	workDir := integrationWorkDir(t)
	// Another rig ran just now; this one has never run.
	if err := savePatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("otherrig"), time.Now()); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	gate := newGateShell(gateExit("", 0))
	d := integrationDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true}, gate)

	d.runRigIntegration(context.Background(), "gastown", "deadbeef", workDir)

	if got := gate.ran(); len(got) != 1 {
		t.Fatalf("another rig's run must not make this rig's integration tier not due: ran %q\n%s", got, logged.String())
	}
}

func TestRunRigIntegration_OffAndAbsentAreNoOps(t *testing.T) {
	t.Parallel()
	gate := newGateShell(gateExit("", 0))

	// Turned off by config: the tree has a tier, but nothing runs.
	townRoot := t.TempDir()
	var logged bytes.Buffer
	d := integrationDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true, IntegrationIntervalStr: "0s"}, gate)
	d.runRigIntegration(context.Background(), "gastown", "deadbeef", integrationWorkDir(t))
	if got := gate.ran(); len(got) != 0 {
		t.Errorf("integration_interval=0s must turn the run off, ran %q", got)
	}

	// A tree with no tier: nothing runs and nothing is recorded.
	townRoot = t.TempDir()
	d = integrationDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true}, gate)
	d.runRigIntegration(context.Background(), "gastown", "deadbeef", t.TempDir())
	if got := gate.ran(); len(got) != 0 {
		t.Errorf("a tree with no integration tier ran %q", got)
	}
	if _, ok, _ := loadPatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("gastown")); ok {
		t.Error("a rig with no integration tier must not record a run")
	}
}

func TestRunRigIntegration_InterruptedIsNotAVerdict(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	workDir := integrationWorkDir(t)
	rec := notifyfake.New()
	var logged bytes.Buffer
	parent, cancel := context.WithCancel(context.Background())
	cancel() // the daemon is shutting down
	// os/exec refuses to start a command whose context is done.
	d := integrationDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true}, newGateShell(func(string) (string, error) {
		return "", parent.Err()
	}))
	d.notifier = rec

	d.runRigIntegration(parent, "gastown", "deadbeef", workDir)

	if len(rec.Calls()) != 0 {
		t.Errorf("a stopped run must neither escalate nor clear, got %+v", rec.Calls())
	}
	if _, ok, _ := loadPatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("gastown")); ok {
		t.Error("a stopped run must not record a last run, or the tier waits a full interval unverified")
	}
}

// TestTestRigMainBranch_RunsTheIntegrationTier is the wiring guard: the real
// per-rig path — fetch, worktree of main, slot hold, gates — must reach the
// integration tier of the tree it checked out. Before gt-22hdp.39 it ran only
// the test command, and nothing ran the tier at all.
func TestTestRigMainBranch_RunsTheIntegrationTier(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	rigPath := filepath.Join(townRoot, "gastown")

	// The rig's test command, and a main whose Makefile declares the tier.
	cfg, err := json.Marshal(map[string]interface{}{
		"type": "rig", "version": 1, "name": "gastown",
		"merge_queue": map[string]interface{}{"test_command": "go test ./..."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	gate := newGateShell(gateExit("", 0))
	d := integrationDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true}, gate)
	stubGatePool(d, poolHeldBy())
	d.seams.slots = slot.NewGate(slot.WithRuntime(noContainers{}))
	d.ctx = context.Background()

	// The rig's bare repo, cloned from an origin whose main declares the tier.
	f := useGitfake(t, d)
	src := filepath.Join(t.TempDir(), "src.git")
	f.InitBare(t, src)
	f.Commit(t, src, "main", "tier", map[string]string{"Makefile": "test-integration:\n\t@true\n"})
	if err := f.Open(townRoot).CloneBareWithBranch(src, filepath.Join(rigPath, ".repo.git"), "main"); err != nil {
		t.Fatal(err)
	}

	if err := d.testRigMainBranch("gastown", rigPath, time.Minute); err != nil {
		t.Fatalf("testRigMainBranch: %v\n%s", err, logged.String())
	}
	if got := gate.ran(); !slices.Equal(got, []string{"go test ./...", "make test-integration"}) {
		t.Fatalf("ran %q, want the rig's test command and then the integration tier of main\n%s", got, logged.String())
	}
	// Both run in the worktree of main: the Makefile the integration tier is
	// read from is the one checked out there.
	if gate.dirs[0] != gate.dirs[1] || gate.dirs[0] == rigPath {
		t.Errorf("gate dirs = %q, want both in the one worktree of main", gate.dirs)
	}
}
