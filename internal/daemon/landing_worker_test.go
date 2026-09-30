package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/landworker"
)

func TestLandingWorkerConfigDefaults(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "landing_worker") {
		t.Fatal("landing_worker enabled with no config; it must be opt-in")
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if IsPatrolEnabled(cfg, "landing_worker") {
		t.Fatal("landing_worker enabled with no entry")
	}
	if landingWorkerInterval(cfg) != defaultLandingWorkerInterval || landingWorkerLandTimeout(cfg) != landworker.DefaultLandTimeout {
		t.Fatal("defaults not applied")
	}
	cfg.Patrols.LandingWorker = &LandingWorkerConfig{Enabled: true, IntervalStr: "2m", LandTimeoutStr: "45m", Rigs: []string{"gastown"}}
	if !IsPatrolEnabled(cfg, "landing_worker") || landingWorkerInterval(cfg) != 2*time.Minute || landingWorkerLandTimeout(cfg) != 45*time.Minute {
		t.Fatal("configured values not applied")
	}
	if got := landingWorkerRigs(cfg, []string{"beads", "gastown", "hm"}); len(got) != 1 || got[0] != "gastown" {
		t.Fatalf("rigs = %v", got)
	}
}

func TestPruneLandingLogs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	old, fresh := filepath.Join(root, "land-old"), filepath.Join(root, "land-new")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-landingLogRetention - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	pruneLandingLogs(root, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old log dir kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh log dir removed")
	}
}

func TestLandingReviewOnByDefault(t *testing.T) {
	t.Parallel()
	if !landingReviewEnabled(nil) || !landingReviewEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Enabled: true}}}) {
		t.Fatal("om review must be on unless explicitly disabled")
	}
	off := false
	if landingReviewEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Review: &off}}}) {
		t.Fatal("review:false did not disable the review")
	}
}

func TestResolveOMPath(t *testing.T) {
	t.Parallel()
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	onPath := func(string) (string, error) { return "/usr/local/bin/om", nil }
	if got := resolveOMPath("/opt/om", onPath, "/Users/x"); got != "/opt/om" {
		t.Errorf("configured: %s", got)
	}
	if got := resolveOMPath("", onPath, "/Users/x"); got != "/usr/local/bin/om" {
		t.Errorf("on PATH: %s", got)
	}
	if got := resolveOMPath("", missing, "/Users/x"); got != "/Users/x/go/bin/om" {
		t.Errorf("fallback: %s", got)
	}
}

func TestRigPostLandCommandReadsRigSettingsOnly(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if got := rigPostLandCommand(rigPath); got != "" {
		t.Fatalf("no settings: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"rig-settings","version":1,"merge_queue":{"gate":"make gate","post_land_command":"make test-slow"}}`
	if err := os.WriteFile(filepath.Join(rigPath, "settings", "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rigPostLandCommand(rigPath); got != "make test-slow" {
		t.Fatalf("post_land_command = %q", got)
	}
}

// The landing work root must never be under the town root: internal/git
// refuses worktree targets there, which failed every landing on 2026-09-30.
func TestLandingWorkRootIsOutsideTheTownRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	root, err := landingWorkRoot("", town, "gastown")
	if err != nil {
		t.Fatalf("default work root: %v", err)
	}
	if landingPathWithin(root, town) {
		t.Fatalf("default work root %s is under the town root %s", root, town)
	}
	if filepath.Base(root) != "gastown" || !strings.Contains(root, fmt.Sprintf("gt-landing-%d", os.Getuid())) {
		t.Fatalf("default work root %s is not uid- and rig-scoped", root)
	}

	for _, bad := range []string{filepath.Join(town, ".runtime", "landing-work"), town} {
		if _, err := landingWorkRoot(bad, town, "gastown"); err == nil || !strings.Contains(err.Error(), "under the town root") {
			t.Errorf("work_root %s: err = %v; want a refusal naming the town root", bad, err)
		}
	}
	if _, err := landingWorkRoot("relative/dir", town, "gastown"); err == nil {
		t.Error("a relative work_root was accepted")
	}
	outside := t.TempDir()
	if got, err := landingWorkRoot(outside, town, "gastown"); err != nil || got != filepath.Join(outside, "gastown") {
		t.Errorf("work_root outside the town: %s, %v", got, err)
	}
}

// A work root spelled through a symlink into the town root is refused too.
func TestLandingWorkRootRefusesASymlinkIntoTheTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(town, link); err != nil {
		t.Fatal(err)
	}
	if _, err := landingWorkRoot(filepath.Join(link, "work"), town, "gastown"); err == nil {
		t.Fatal("a work_root reaching the town root through a symlink was accepted")
	}
}

// gitRemote reads the landing target on origin: the branch tip there, and
// whether a commit is on it after fetching the target.
func TestGitRemoteReadsTheTargetOnOrigin(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	root := t.TempDir()
	origin, bare := filepath.Join(root, "origin.git"), filepath.Join(root, ".repo.git")
	f.InitBare(t, origin)
	seed := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	if err := f.Open(root).CloneBareWithBranch(origin, bare, "main"); err != nil {
		t.Fatal(err)
	}
	landed := f.Commit(t, origin, "main", "landed", map[string]string{"b.txt": "b\n"})
	r := gitRemote{g: f.Open(bare), remote: "origin"}

	if tip, err := r.BranchTip("main"); err != nil || tip != landed {
		t.Fatalf("BranchTip(main) = %q, %v; want origin's tip %s", tip, err, landed)
	}
	for _, c := range []string{seed, landed} {
		if on, err := r.Contains("main", c); err != nil || !on {
			t.Errorf("Contains(main, %s) = %v, %v; want true once the target is fetched", c, on, err)
		}
	}
	if on, err := r.Contains("main", strings.Repeat("e", 40)); err != nil || on {
		t.Errorf("Contains(main, unknown) = %v, %v; want false and no error", on, err)
	}
	if on, err := r.Contains("no-such-target", seed); err == nil || on {
		t.Errorf("Contains(missing target) = %v, %v; want an error, never a no", on, err)
	}
}
