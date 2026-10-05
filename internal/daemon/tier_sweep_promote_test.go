package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/promote"
)

// promoteRigConfig is a rig config.json naming a Forgejo landing block and a
// GitHub promote_target with a deploy key: the operator tier d.newPromoter
// reads (gt-fn9e6.37).
func promoteRigConfig(t *testing.T, townRoot, rigName, keyFile string) string {
	t.Helper()
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
		t.Fatalf("mkdir .repo.git: %v", err)
	}
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"`+rigName+`","default_branch":"main",
		"merge_queue":{"forgejo":{"remote_url":"https://forgejo.example/gastown/gastown.git","gate_workflow":"gate","bots":{"landing":"gt-landing"},
		"promote_target":"git@github.com:example/gastown.git","promote_key_file":"`+keyFile+`"}}}`)
	return rigPath
}

// newForgejoPromoteDaemon is a daemon whose rig lands through Forgejo and
// whose tier sweep, when sweptRig is non-empty, covers sweptRig.
func newForgejoPromoteDaemon(t *testing.T, townRoot, sweptRig string) *Daemon {
	t.Helper()
	tokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, "forgejo-landing.env"), []byte("FORGEJO_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	patrols := &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
		Forgejo: &config.ForgejoWorkerConfig{TokenDir: tokenDir}}}
	if sweptRig != "" {
		patrols.TierSweep = &TierSweepConfig{Enabled: true, Rigs: []string{sweptRig}}
	}
	d := &Daemon{
		logger:       discardLogger,
		config:       &Config{TownRoot: townRoot},
		notifier:     notifyfake.New(),
		patrolConfig: &DaemonPatrolConfig{Patrols: patrols},
	}
	// The worker resolves the rig's landing remote through the daemon's git
	// seam, so the unit tier reads a fake world rather than starting git.
	f := useGitfake(t, d)
	forgejoLandingRemote(t, f, townRoot, "gastown")
	return d
}

// redMainOf is the RedMain (and so the promotion owner) behind a worker the
// daemon built.
func redMainOf(t *testing.T, w *landworker.Worker) *landworker.RedMain {
	t.Helper()
	rm, ok := w.Reverts.(*landworker.RedMain)
	if !ok {
		t.Fatalf("worker Reverts = %T, want *landworker.RedMain", w.Reverts)
	}
	return rm
}

// TestNewRigLandingWorker_SweptRigPromotesOnlyFromTheSweep: for a rig the
// tier sweep covers, a green post-land verdict is the tiers `make gate` ran,
// not the full sweep, so the worker carries no promoter at all and the verdict
// has no path that could push (gt-fn9e6.38).
func TestNewRigLandingWorker_SweptRigPromotesOnlyFromTheSweep(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "promote-gastown.key")
	promoteRigConfig(t, townRoot, "gastown", keyFile)
	writeRigsJSON(t, townRoot, []string{"gastown"})

	d := newForgejoPromoteDaemon(t, townRoot, "gastown")
	w, err := d.newRigLandingWorker("gastown")
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	if rm := redMainOf(t, w); rm.Promote != nil {
		t.Error("the post-land verdict carries a promoter for a swept rig; only a fully green sweep may promote")
	}
}

// TestNewRigLandingWorker_UnsweptRigPromotesOnAGreenVerdict: a rig the sweep
// does not cover keeps the main-custody bead's behavior — its green post-land
// verdict is the promotion candidate (gt-fn9e6.38).
func TestNewRigLandingWorker_UnsweptRigPromotesOnAGreenVerdict(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "promote-gastown.key")
	promoteRigConfig(t, townRoot, "gastown", keyFile)
	writeRigsJSON(t, townRoot, []string{"gastown", "otherrig"})

	// The sweep runs, but over another rig.
	d := newForgejoPromoteDaemon(t, townRoot, "otherrig")
	w, err := d.newRigLandingWorker("gastown")
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	rm := redMainOf(t, w)
	if rm.Promote == nil {
		t.Fatal("a rig the tier sweep does not cover must still promote on a green post-land verdict")
	}
	if rm.Promote.Target != "git@github.com:example/gastown.git" {
		t.Errorf("promote target = %q, want the rig's promote_target", rm.Promote.Target)
	}
	if rm.Promote.LockPath != promote.LockPath(townRoot, "gastown") {
		t.Errorf("promote lock = %q, want the rig's one promotion lock", rm.Promote.LockPath)
	}
}

// TestNewRigLandingWorker_NoTierSweepPromotesOnAGreenVerdict: with the sweep
// patrol off there is no sweep to wait for, so every rig keeps promoting from
// its green post-land verdict.
func TestNewRigLandingWorker_NoTierSweepPromotesOnAGreenVerdict(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "promote-gastown.key")
	promoteRigConfig(t, townRoot, "gastown", keyFile)
	writeRigsJSON(t, townRoot, []string{"gastown"})

	d := newForgejoPromoteDaemon(t, townRoot, "")
	w, err := d.newRigLandingWorker("gastown")
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	if rm := redMainOf(t, w); rm.Promote == nil {
		t.Error("with no tier sweep configured, a green post-land verdict must still promote")
	}
}

// TestTierSweepCoversRig_OnlyTheRigsACycleSweeps: the sweep owns a rig's
// promotion only when the patrol is on and the rig is one a cycle actually
// sweeps — a disabled sweep must not leave a rig promoting nowhere.
func TestTierSweepCoversRig_OnlyTheRigsACycleSweeps(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeRigsJSON(t, townRoot, []string{"gastown", "otherrig"})

	cases := []struct {
		name    string
		patrols *PatrolsConfig
		rig     string
		want    bool
	}{
		{"the default sweep covers gastown", &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true}}, "gastown", true},
		{"the default sweep leaves another rig alone", &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true}}, "otherrig", false},
		{"a configured sweep covers the rig it names", &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true, Rigs: []string{"otherrig"}}}, "otherrig", true},
		{"a configured sweep leaves the rest alone", &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true, Rigs: []string{"otherrig"}}}, "gastown", false},
		{"a disabled sweep covers nothing", &PatrolsConfig{TierSweep: &TierSweepConfig{Rigs: []string{"gastown"}}}, "gastown", false},
		{"no sweep covers nothing", &PatrolsConfig{}, "gastown", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot},
				patrolConfig: &DaemonPatrolConfig{Patrols: tc.patrols}}
			if got := d.tierSweepCoversRig(tc.rig); got != tc.want {
				t.Errorf("tierSweepCoversRig(%q) = %v, want %v", tc.rig, got, tc.want)
			}
		})
	}
}

// tierSweepPromoteRepo is the git surface a sweep test gives its Promoter: a
// target main tip, whether it is an ancestor of the green commit, and every
// push's refspec.
type tierSweepPromoteRepo struct {
	tip      string
	ancestor bool
	pushes   []string
}

func (r *tierSweepPromoteRepo) ListRemoteRefsWithHashes(_, _ string) ([]git.RemoteRef, error) {
	if r.tip == "" {
		return nil, nil
	}
	return []git.RemoteRef{{Hash: r.tip, Name: promote.MainRef}}, nil
}

func (r *tierSweepPromoteRepo) IsAncestor(_, _ string) (bool, error) { return r.ancestor, nil }

func (r *tierSweepPromoteRepo) PushWithEnv(_, refspec string, _ bool, _ []string) error {
	r.pushes = append(r.pushes, refspec)
	return nil
}

// tierSweepPromoteHarness is the sweep's promotion seam over a stub repo: it
// records every rig it was asked to build a promoter for.
type tierSweepPromoteHarness struct {
	repo promote.Repo
	rigs []string
	// target is the promote target the built promoter pushes to; empty means
	// a placeholder URL no test resolves.
	target string
}

func (h *tierSweepPromoteHarness) build(rig, _ string) *promote.Promoter {
	h.rigs = append(h.rigs, rig)
	target := h.target
	if target == "" {
		target = "git@github.com:example/gastown.git"
	}
	return &promote.Promoter{
		Rig:     rig,
		Target:  target,
		KeyFile: "/tmp/promote-gastown.key",
		Repo:    h.repo,
	}
}

// sweepFixture is a sweep over gastown on the given local hour with its
// outside effects faked, including the promote seam over repo.
func sweepFixture(t *testing.T, hour int, repo promote.Repo) (*Daemon, *tierSweepRunRecorder, *tierSweepPromoteHarness, string) {
	t.Helper()
	d, rec, _, _ := newTierSweepDaemon(t, atHour(time.Now(), hour), "gastown")
	h := &tierSweepPromoteHarness{repo: repo}
	d.tierSweepSeams.promote = h.build
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	return d, rec, h, sha
}

// TestRunTierSweep_AFullyGreenCyclePromotesItsCommit: a cycle that covered
// every tier green is the rig's promotion verdict, so the commit it swept is
// pushed to GitHub main through the one promotion owner, and recorded in the
// record the post-land path keeps (gt-fn9e6.38).
func TestRunTierSweep_AFullyGreenCyclePromotesItsCommit(t *testing.T) {
	t.Parallel()
	repo := &tierSweepPromoteRepo{tip: "bbbb1111", ancestor: true}
	d, rec, h, sha := sweepFixture(t, 14, repo)
	rec.green()
	// The record the post-land verdict writes is seeded first: the sweep must
	// update that same file rather than keeping a second one.
	if err := (fileMainState{path: RedMainStatePath(d.config.TownRoot, "gastown")}).Save(
		landworker.MainState{LastGreen: "cccc2222", LastRun: "cccc2222"}); err != nil {
		t.Fatal(err)
	}

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}

	if len(repo.pushes) != 1 || repo.pushes[0] != sha+":"+promote.MainRef {
		t.Fatalf("pushes = %v, want the swept commit pushed to main", repo.pushes)
	}
	st, err := (fileMainState{path: RedMainStatePath(d.config.TownRoot, "gastown")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastPromoted != sha {
		t.Errorf("LastPromoted = %q, want the swept commit %s", st.LastPromoted, sha)
	}
	if st.LastGreen != "cccc2222" || st.LastRun != "cccc2222" {
		t.Errorf("state = %+v, want the post-land verdict fields kept beside the promotion", st)
	}
	if len(h.rigs) != 1 || h.rigs[0] != "gastown" {
		t.Errorf("promoters built for %v, want gastown once", h.rigs)
	}
}

// TestRunTierSweep_AFullyGreenCycleFastForwardsGitHubMain is the same
// promotion over fake bare repositories: the target's main, behind the swept
// commit and an ancestor of it, fast-forwards to exactly that commit.
func TestRunTierSweep_AFullyGreenCycleFastForwardsGitHubMain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := gitfake.New()
	rig := filepath.Join(root, "rig.git")
	gh := filepath.Join(root, "github.git")
	f.InitBare(t, rig)
	f.InitBare(t, gh)
	base := f.Commit(t, rig, "main", "main: seed", map[string]string{"a.txt": "one\n"})
	// GitHub's main is where the last promotion left it: behind.
	f.SetRef(t, gh, "refs/heads/main", base)
	green := f.Commit(t, rig, "main", "green", map[string]string{"b.txt": "ok\n"})

	d, rec, h, _ := sweepFixture(t, 14, f.Open(rig))
	h.target = gh
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return green, nil }
	rec.green()

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}

	if got := f.Ref(gh, promote.MainRef); got != green {
		t.Errorf("target main = %q, want the swept commit %s", got, green)
	}
}

// TestRunTierSweep_ARedCyclePromotesNothing: a red sweep files its beads and
// reverts nothing, and promotes nothing — GitHub main keeps the last commit a
// fully green cycle named (gt-fn9e6.38).
func TestRunTierSweep_ARedCyclePromotesNothing(t *testing.T) {
	t.Parallel()
	repo := &tierSweepPromoteRepo{tip: "bbbb1111", ancestor: true}
	d, rec, h, _ := sweepFixture(t, 14, repo)
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		var b strings.Builder
		for _, tier := range st.tiers {
			b.WriteString("tier-sweep: " + tier + " RED passed=1 failed=1 skipped=0 failed: ./x (logs /tmp/x)\n")
		}
		return tierSweepStageResult{output: b.String(), exitCode: 1, ran: true}
	}

	d.runTierSweep()

	if len(repo.pushes) != 0 {
		t.Errorf("pushes = %v, want none on a red sweep", repo.pushes)
	}
	if len(h.rigs) != 0 {
		t.Errorf("promoters built for %v, want none: a red cycle is no promotion candidate", h.rigs)
	}
}

// TestRunTierSweep_APartialGreenCyclePromotesNothing: the odd-hour cycle runs
// the shell tier alone, so its green says nothing about the integration tiers
// and is not a promotion candidate (gt-fn9e6.38).
func TestRunTierSweep_APartialGreenCyclePromotesNothing(t *testing.T) {
	t.Parallel()
	repo := &tierSweepPromoteRepo{tip: "bbbb1111", ancestor: true}
	d, rec, h, _ := sweepFixture(t, 15, repo)
	rec.green()

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}

	if len(rec.stages) != 1 {
		t.Fatalf("stages = %v, want the shell tier alone on an odd hour", rec.stages)
	}
	if len(repo.pushes) != 0 {
		t.Errorf("pushes = %v, want none for a shell-only green", repo.pushes)
	}
	if len(h.rigs) != 0 {
		t.Errorf("promoters built for %v, want none for a shell-only green", h.rigs)
	}
}

// TestRunTierSweep_ADivergedTargetIsRecordedNotForced: a GitHub main that is
// not an ancestor of the swept commit is recorded as diverged in the rig's own
// state and never overwritten, exactly as the post-land path treats it.
func TestRunTierSweep_ADivergedTargetIsRecordedNotForced(t *testing.T) {
	t.Parallel()
	d, rec, _, sha := sweepFixture(t, 14, &tierSweepPromoteRepo{tip: "bbbb1111", ancestor: false})
	rec.green()

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}

	st, err := (fileMainState{path: RedMainStatePath(d.config.TownRoot, "gastown")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.GitHubDiverged == nil || st.GitHubDiverged.RemoteMain != "bbbb1111" || st.GitHubDiverged.Commit != sha {
		t.Errorf("GitHubDiverged = %+v, want the target's tip and the swept commit", st.GitHubDiverged)
	}
}

// TestTierSweepPromoter_OnlyForARigWithATarget: a rig whose forgejo block
// names no promote_target, or names one with no deploy key, has no promotion
// owner at all, so its sweep is unchanged.
func TestTierSweepPromoter_OnlyForARigWithATarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"no forgejo block", `{"type":"rig","version":1,"name":"testrig","default_branch":"main"}`, false},
		{"a forgejo block with no promote_target",
			`{"type":"rig","version":1,"name":"testrig","default_branch":"main","merge_queue":{"forgejo":{"remote_url":"https://forgejo.example/gastown/gastown.git"}}}`, false},
		{"a promote_target with no deploy key",
			`{"type":"rig","version":1,"name":"testrig","default_branch":"main","merge_queue":{"forgejo":{"remote_url":"https://forgejo.example/gastown/gastown.git","promote_target":"git@github.com:example/gastown.git"}}}`, false},
		{"a promote_target and a deploy key",
			`{"type":"rig","version":1,"name":"testrig","default_branch":"main","merge_queue":{"forgejo":{"remote_url":"https://forgejo.example/gastown/gastown.git","promote_target":"git@github.com:example/gastown.git","promote_key_file":"/tmp/k"}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			rigPath := filepath.Join(townRoot, "testrig")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}
			writeDaemonRigConfigFile(t, rigPath, tc.body)
			d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}}
			p := d.tierSweepPromoter("testrig", filepath.Join(rigPath, ".repo.git"))
			if (p != nil) != tc.want {
				t.Fatalf("tierSweepPromoter = %v, want non-nil = %v", p, tc.want)
			}
			if p != nil && p.Target != "git@github.com:example/gastown.git" {
				t.Errorf("target = %q, want the rig's promote_target", p.Target)
			}
		})
	}
}
