package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
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

// integrationWorkDir is a worktree whose Makefile's test-integration recipe is
// recipe.
func integrationWorkDir(t *testing.T, recipe string) string {
	t.Helper()
	dir := t.TempDir()
	mk := "test-integration:\n\t" + recipe + "\n"
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(mk), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireMake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Fatalf("make is required to run the integration command: %v", err)
	}
}

func TestRunRigIntegration_FailureEscalatesUnderItsOwnKey(t *testing.T) {
	t.Parallel()
	requireMake(t)
	townRoot := t.TempDir()
	workDir := integrationWorkDir(t, "@echo '--- FAIL: TestIntegrationWidget (0.01s)'; exit 1")
	rec := notifyfake.New()
	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})
	d.notifier = rec

	d.runRigIntegration(context.Background(), "gastown", "deadbeef", workDir)

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
	requireMake(t)
	townRoot := t.TempDir()
	workDir := integrationWorkDir(t, "@true")
	rec := notifyfake.New()
	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})
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
	requireMake(t)
	townRoot := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	workDir := integrationWorkDir(t, "@touch "+marker)
	if err := savePatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("gastown"), time.Now()); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})

	d.runRigIntegration(context.Background(), "gastown", "deadbeef", workDir)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the integration tier ran again inside its interval")
	}
	if !strings.Contains(logged.String(), "integration not due") {
		t.Errorf("a skipped run must say why:\n%s", logged.String())
	}
}

func TestRunRigIntegration_PerRigCadence(t *testing.T) {
	t.Parallel()
	requireMake(t)
	townRoot := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	workDir := integrationWorkDir(t, "@touch "+marker)
	// Another rig ran just now; this one has never run.
	if err := savePatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("otherrig"), time.Now()); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})

	d.runRigIntegration(context.Background(), "gastown", "deadbeef", workDir)

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("another rig's run must not make this rig's integration tier not due: %v\n%s", err, logged.String())
	}
}

func TestRunRigIntegration_OffAndAbsentAreNoOps(t *testing.T) {
	t.Parallel()
	requireMake(t)
	marker := filepath.Join(t.TempDir(), "ran")

	// Turned off by config: the tree has a tier, but nothing runs.
	townRoot := t.TempDir()
	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true, IntegrationIntervalStr: "0s"})
	d.runRigIntegration(context.Background(), "gastown", "deadbeef", integrationWorkDir(t, "@touch "+marker))
	if _, err := os.Stat(marker); err == nil {
		t.Error("integration_interval=0s must turn the run off")
	}

	// A tree with no tier: nothing runs and nothing is recorded.
	townRoot = t.TempDir()
	d = newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})
	d.runRigIntegration(context.Background(), "gastown", "deadbeef", t.TempDir())
	if _, ok, _ := loadPatrolLastRun(townRoot, mainBranchIntegrationPatrolKey("gastown")); ok {
		t.Error("a rig with no integration tier must not record a run")
	}
}

func TestRunRigIntegration_InterruptedIsNotAVerdict(t *testing.T) {
	t.Parallel()
	requireMake(t)
	townRoot := t.TempDir()
	workDir := integrationWorkDir(t, "@exit 1")
	rec := notifyfake.New()
	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})
	d.notifier = rec
	parent, cancel := context.WithCancel(context.Background())
	cancel() // the daemon is shutting down

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
	stubNoContainers(t)
	stubGatePool(t, poolHeldBy())
	requireMake(t)
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	rigPath := filepath.Join(townRoot, "gastown")
	marker := filepath.Join(t.TempDir(), "integration-ran")
	gateMarker := filepath.Join(t.TempDir(), "gate-ran")

	// The rig's test command, and a main whose Makefile declares the tier.
	cfg, err := json.Marshal(map[string]interface{}{
		"type": "rig", "version": 1, "name": "gastown",
		"merge_queue": map[string]interface{}{"test_command": "touch " + shellQuote(gateMarker)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(src, "init", "-q")
	mk := "test-integration:\n\t@touch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(src, "Makefile"), []byte(mk), 0o644); err != nil {
		t.Fatal(err)
	}
	git(src, "add", "Makefile")
	git(src, "commit", "-q", "-m", "tier")
	git(townRoot, "clone", "-q", "--bare", src, filepath.Join(rigPath, ".repo.git"))
	git(filepath.Join(rigPath, ".repo.git"), "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")

	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})
	d.ctx = context.Background()

	if err := d.testRigMainBranch("gastown", rigPath, time.Minute); err != nil {
		t.Fatalf("testRigMainBranch: %v\n%s", err, logged.String())
	}
	if _, err := os.Stat(gateMarker); err != nil {
		t.Fatalf("the rig's test command did not run: %v\n%s", err, logged.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the integration tier of main did not run: %v\n%s", err, logged.String())
	}
}
