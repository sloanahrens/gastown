package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

// maintTestNow is the injected clock every runMaint test plans with, so a plan
// carries a fixed time instead of the day the suite happens to run.
var maintTestNow = time.Date(2026, 10, 28, 4, 30, 0, 0, time.UTC)

// fakeMaintTarget is one diskmaint.Target with no I/O: it records the policies
// and plans it was handed and answers with what the test set.
type fakeMaintTarget struct {
	name     string
	plan     diskmaint.Plan
	planErr  error
	result   diskmaint.Result
	applyErr error

	policies []diskmaint.Policy
	applied  []diskmaint.Plan
	contexts []context.Context
}

func (f *fakeMaintTarget) Name() string { return f.name }

func (f *fakeMaintTarget) Measure(context.Context) (diskmaint.Measurement, error) {
	return diskmaint.Measurement{}, nil
}

func (f *fakeMaintTarget) Plan(ctx context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	f.contexts = append(f.contexts, ctx)
	f.policies = append(f.policies, p)
	return f.plan, f.planErr
}

func (f *fakeMaintTarget) Apply(ctx context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	f.contexts = append(f.contexts, ctx)
	f.applied = append(f.applied, plan)
	return f.result, f.applyErr
}

// fakeMaintRunner answers diskmaint.Runner without running a program; every
// call is recorded so a test can assert the program and its argv.
type fakeMaintRunner struct {
	calls [][]string
	out   string
	err   error
}

func (f *fakeMaintRunner) run(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.out), nil
}

// maintTestPlan is one plan with a delete and a kept ref, so the printed plan
// has both sections.
func maintTestPlan() diskmaint.Plan {
	return diskmaint.Plan{
		Target: "registry",
		Actions: []diskmaint.Action{{
			Kind:  "delete-manifest",
			Ref:   "gastown-polecat@sha256:" + strings.Repeat("a", 64),
			Bytes: 4096,
			Why:   "older than the newest 5 commits",
		}},
		Kept: []diskmaint.Kept{{Ref: "gastown-polecat:dev", Rule: "the dev tag"}},
	}
}

// maintDepsFor builds the seams a test run uses: the given targets, a fixed
// clock and a keep window of 5.
func maintDepsFor(targets ...*fakeMaintTarget) maintDeps {
	m := make(map[string]diskmaint.Target, len(targets))
	for _, target := range targets {
		m[target.name] = target
	}
	return maintDeps{
		Targets: func() (map[string]diskmaint.Target, error) { return m, nil },
		Now:     func() time.Time { return maintTestNow },
		Keep:    5,
	}
}

func TestMaintPlanPrintsActionsAndKept(t *testing.T) {
	t.Parallel()
	fake := &fakeMaintTarget{name: "registry", plan: maintTestPlan()}
	var out strings.Builder

	if err := runMaint(&out, maintDepsFor(fake), maintModePlan, ""); err != nil {
		t.Fatalf("runMaint(plan) error = %v, want none", err)
	}
	for _, want := range []string{
		"registry: 1 action(s), 1 kept",
		"would delete-manifest " + maintTestPlan().Actions[0].Ref + " (4096 bytes): older than the newest 5 commits",
		"kept gastown-polecat:dev: the dev tag",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output is missing %q:\n%s", want, out.String())
		}
	}
	if len(fake.policies) != 1 {
		t.Fatalf("Plan called %d times, want once", len(fake.policies))
	}
	p := fake.policies[0]
	if p.Apply {
		t.Error("plan mode asked for a policy with Apply true")
	}
	if p.Keep != 5 || !p.Now.Equal(maintTestNow) {
		t.Errorf("policy = {Keep:%d Now:%v}, want {Keep:5 Now:%v}", p.Keep, p.Now, maintTestNow)
	}
}

func TestMaintPlanNeverApplies(t *testing.T) {
	t.Parallel()
	fake := &fakeMaintTarget{name: "registry", plan: maintTestPlan()}

	if err := runMaint(io.Discard, maintDepsFor(fake), maintModePlan, ""); err != nil {
		t.Fatalf("runMaint(plan) error = %v, want none", err)
	}
	if len(fake.applied) != 0 {
		t.Fatalf("Apply called %d times in plan mode, want none", len(fake.applied))
	}
}

func TestMaintApplyRunsApplyOnThePlan(t *testing.T) {
	t.Parallel()
	plan := maintTestPlan()
	fake := &fakeMaintTarget{
		name:   "registry",
		plan:   plan,
		result: diskmaint.Result{Target: "registry", Done: plan.Actions, BytesFreed: 4096, Collected: true},
	}
	var out strings.Builder

	if err := runMaint(&out, maintDepsFor(fake), maintModeApply, ""); err != nil {
		t.Fatalf("runMaint(apply) error = %v, want none", err)
	}
	if len(fake.applied) != 1 {
		t.Fatalf("Apply called %d times, want once", len(fake.applied))
	}
	if fake.applied[0].Refused != "" || len(fake.applied[0].Actions) != len(plan.Actions) {
		t.Errorf("Apply was handed %+v, want the plan Plan returned", fake.applied[0])
	}
	if !fake.policies[0].Apply {
		t.Error("apply mode asked for a policy with Apply false")
	}
	for _, want := range []string{"did delete-manifest", "freed 4096 bytes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("apply output is missing %q:\n%s", want, out.String())
		}
	}
}

func TestMaintApplyRefusesARefusedPlan(t *testing.T) {
	t.Parallel()
	const reason = "read failed: catalog: connection refused"
	for _, mode := range []string{maintModePlan, maintModeApply} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fake := &fakeMaintTarget{name: "registry", plan: diskmaint.Plan{Target: "registry", Refused: reason}}
			var out strings.Builder

			err := runMaint(&out, maintDepsFor(fake), mode, "")
			if err == nil || !strings.Contains(err.Error(), reason) {
				t.Fatalf("runMaint(%s) error = %v, want the refusal reason", mode, err)
			}
			if !strings.Contains(out.String(), "refused: "+reason) {
				t.Errorf("output is missing the reason:\n%s", out.String())
			}
			if len(fake.applied) != 0 {
				t.Errorf("Apply called %d times for a refused plan, want none", len(fake.applied))
			}
		})
	}
}

func TestMaintApplyReportsDeferralWhenLockHeld(t *testing.T) {
	t.Parallel()
	const deferred = "registry apply deferred: polecat-image lock is held"
	fake := &fakeMaintTarget{
		name:   "registry",
		plan:   maintTestPlan(),
		result: diskmaint.Result{Target: "registry", Deferred: deferred},
	}
	var out strings.Builder

	err := runMaint(&out, maintDepsFor(fake), maintModeApply, "")
	if err == nil || !strings.Contains(err.Error(), "retry") || !strings.Contains(err.Error(), deferred) {
		t.Fatalf("runMaint(apply) error = %v, want the deferral and that it can be retried", err)
	}
	if !strings.Contains(out.String(), "deferred: "+deferred) {
		t.Errorf("output is missing the deferral reason:\n%s", out.String())
	}
}

func TestMaintUnknownTargetListsKnownOnes(t *testing.T) {
	t.Parallel()
	alpha := &fakeMaintTarget{name: "alpha", plan: maintTestPlan()}
	beta := &fakeMaintTarget{name: "beta", plan: maintTestPlan()}

	err := runMaint(io.Discard, maintDepsFor(alpha, beta), maintModePlan, "gamma")
	if err == nil {
		t.Fatal("runMaint() error = nil, want the unknown target")
	}
	for _, want := range []string{`unknown target "gamma"`, "alpha, beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if len(alpha.policies)+len(beta.policies) != 0 {
		t.Error("an unknown target still planned another target")
	}
}

func TestMaintNoTargetRunsEveryTarget(t *testing.T) {
	t.Parallel()
	zeta := &fakeMaintTarget{name: "zeta", plan: maintTestPlan()}
	alpha := &fakeMaintTarget{name: "alpha", plan: maintTestPlan()}
	var out strings.Builder

	if err := runMaint(&out, maintDepsFor(zeta, alpha), maintModePlan, ""); err != nil {
		t.Fatalf("runMaint() error = %v, want none", err)
	}
	if len(zeta.policies) != 1 || len(alpha.policies) != 1 {
		t.Fatalf("planned zeta %d and alpha %d times, want once each", len(zeta.policies), len(alpha.policies))
	}
	if first, second := strings.Index(out.String(), "alpha:"), strings.Index(out.String(), "zeta:"); first < 0 || second < 0 || first > second {
		t.Errorf("targets are not printed in name order:\n%s", out.String())
	}
}

func TestMaintUnknownModeRefuses(t *testing.T) {
	t.Parallel()
	fake := &fakeMaintTarget{name: "registry", plan: maintTestPlan()}

	err := runMaint(io.Discard, maintDepsFor(fake), "destroy", "")
	if err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("runMaint(destroy) error = %v, want an unknown mode", err)
	}
	if len(fake.policies) != 0 {
		t.Error("an unknown mode still planned a target")
	}
}

func TestMaintRefusesANonPositiveKeep(t *testing.T) {
	t.Parallel()
	for _, keep := range []int{0, -1} {
		deps := maintDeps{
			Targets: func() (map[string]diskmaint.Target, error) {
				t.Fatalf("Targets called with keep %d, want the guard to refuse first", keep)
				return nil, nil
			},
			Keep: keep,
		}
		if err := runMaint(io.Discard, deps, maintModePlan, ""); err == nil || !strings.Contains(err.Error(), "keep must be at least 1") {
			t.Errorf("runMaint(keep %d) error = %v, want the keep guard", keep, err)
		}
	}
}

func TestMaintReportsASkippedGarbageCollect(t *testing.T) {
	t.Parallel()
	plan := maintTestPlan()
	fake := &fakeMaintTarget{
		name:   "registry",
		plan:   plan,
		result: diskmaint.Result{Target: "registry", Done: plan.Actions, BytesFreed: 4096},
	}
	var out strings.Builder

	if err := runMaint(&out, maintDepsFor(fake), maintModeApply, ""); err != nil {
		t.Fatalf("runMaint(apply) error = %v, want none", err)
	}
	if !strings.Contains(out.String(), "garbage collect did not run") {
		t.Errorf("output does not report the skipped collect:\n%s", out.String())
	}
}

func TestMaintContinuesAfterOneTargetFails(t *testing.T) {
	t.Parallel()
	broken := errors.New("the registry listing failed")
	alpha := &fakeMaintTarget{name: "alpha", planErr: broken}
	beta := &fakeMaintTarget{name: "beta", plan: maintTestPlan()}

	err := runMaint(io.Discard, maintDepsFor(alpha, beta), maintModePlan, "")
	if err == nil || !strings.Contains(err.Error(), "the registry listing failed") {
		t.Fatalf("runMaint() error = %v, want the failed target's error", err)
	}
	if len(beta.policies) != 1 {
		t.Errorf("beta planned %d times, want once: one target's failure must not stop the rest", len(beta.policies))
	}
}

func TestMaintTargetsErrorIsReturned(t *testing.T) {
	t.Parallel()
	want := errors.New("the town settings are unreadable")
	deps := maintDeps{
		Targets: func() (map[string]diskmaint.Target, error) { return nil, want },
		Keep:    5,
	}

	err := runMaint(io.Discard, deps, maintModePlan, "")
	if !errors.Is(err, want) {
		t.Fatalf("runMaint() error = %v, want the targets' %v", err, want)
	}
}

func TestMaintApplyDefersWhenThePluginLockIsHeld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := polecatimage.Config{LockDir: filepath.Join(dir, "polecat-image.lock"), StaleAfter: time.Hour}
	now := time.Now()
	gate := &maintPushGate{cfg: cfg, out: io.Discard, now: func() time.Time { return now }}
	opts := polecatimage.WithLockOptions{Out: io.Discard, Now: func() time.Time { return now }}

	// Outside the lock the gate must answer "a push may be in flight": the
	// collect is skipped on that answer, and reading it as "no push" would let
	// collect walk storage a push is writing.
	if !gate.pushInFlight() {
		t.Fatal("pushInFlight() = false outside the lock, want true")
	}

	// The first phase holds the lock the way a build does, so the gate sees a
	// held lock without the test hand-building the lock directory.
	ran := false
	held := polecatimage.WithLock(context.Background(), cfg, opts, func(context.Context) error {
		inner := gate.withLock(context.Background(), func(context.Context) error {
			ran = true
			return nil
		})
		if !errors.Is(inner, registry.ErrDeferred) {
			t.Errorf("withLock() while held = %v, want registry.ErrDeferred", inner)
		}
		if !errors.Is(inner, polecatimage.ErrLockHeld) {
			t.Errorf("withLock() while held = %v, want polecatimage.ErrLockHeld too", inner)
		}
		return nil
	})
	if held != nil {
		t.Fatalf("holding the plugin lock: %v", held)
	}
	if ran {
		t.Fatal("the apply body ran while another run holds the lock")
	}

	// The second phase takes the free lock: the gate may then answer that no
	// push is in flight, which is what lets the collect run.
	if err := gate.withLock(context.Background(), func(context.Context) error {
		if gate.pushInFlight() {
			t.Error("pushInFlight() = true while this run holds the lock, want false")
		}
		return nil
	}); err != nil {
		t.Fatalf("withLock() on a free lock = %v, want none", err)
	}

	// Once the lock is released the gate must say a push may be writing again:
	// a flag left set would answer "no push" for every later call, including
	// ones where a push has started since.
	if !gate.pushInFlight() {
		t.Error("pushInFlight() = false after the lock was released, want true")
	}
}

func TestMaintInstalledCommitComesFromGTVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		out     string
		runErr  error
		want    string
		wantErr string
	}{
		{"build and branch", "gt version 1.2.1 (dev: main@0123456789ab)\n", nil, "0123456789ab", ""},
		{"no branch", "gt version 1.2.1 (release: abcdef1)\n", nil, "abcdef1", ""},
		{"names no commit", "gt version 1.2.1 (dev only)\n", nil, "", "names no commit"},
		{"commit is not hex", "gt version 1.2.1 (dev: main@NOTACOMMIT)\n", nil, "", "names no commit"},
		{"run fails", "", errors.New("exit status 1"), "", "gt version"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeMaintRunner{out: tc.out, err: tc.runErr}
			got, err := maintInstalled(context.Background(), runner.run, "gt")()

			if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != "gt version" {
				t.Errorf("ran %v, want gt version", runner.calls)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Installed() error = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Installed() = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

func TestMaintKeepReadsThePluginKnob(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value   string
		want    int
		wantErr bool
	}{
		{"", maintKeepDefault, false},
		{"3", 3, false},
		{"0", 0, true},
		{"three", 0, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			got, err := maintKeep(func(key string) string {
				if key == "POLECAT_IMAGE_REGISTRY_KEEP" {
					return tc.value
				}
				return ""
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("maintKeep(%q) = %d, nil; want an error", tc.value, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("maintKeep(%q) = %d, %v; want %d, nil", tc.value, got, err, tc.want)
			}
		})
	}
}

func TestMaintTargetsBuildTheRegistryFromTheTownConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMaintRigs(t, town, `{"version":1,"rigs":{"gastown":{"git_url":"u"}}}`)
	settings := filepath.Join(town, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":"localhost:5001"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := polecatimage.ConfigFromEnv(func(key string) string {
		if key == "POLECAT_IMAGE_TOWN_ROOT" {
			return town
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	runner := &fakeMaintRunner{}

	targets, err := maintTargets(context.Background(), town, cfg, 5, maintModePlan, runner.run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v", err)
	}
	want := []string{maintTargetBaks, maintTargetColima, maintTargetNodes, maintTargetRegistry, maintTargetRegistryCollect, maintTargetSizes, maintTargetWorktree}
	if got := maintTargetNames(targets); !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v: the registry and its collect join the local and docker targets", got, want)
	}
	if got := targets[maintTargetRegistry].Name(); got != maintTargetRegistry {
		t.Errorf("Name() = %q, want %q", got, maintTargetRegistry)
	}
	if _, refused := targets[maintTargetWorktree].(maintRefusedTarget); refused {
		t.Errorf("worktree target = %v, want the real target: a readable rig registry builds one", targets[maintTargetWorktree])
	}
	if runner.calls != nil {
		t.Errorf("building a target ran %v, want no program", runner.calls)
	}
}

// TestMaintRegistryCollectIsATarget pins that registry-collect is a target the
// command names, and that its plan is the one collect action: it prunes nothing,
// so the plan carries no delete and runs no program.
func TestMaintRegistryCollectIsATarget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cfg := polecatimage.Config{Registry: "localhost:5001"}
	runner := &fakeMaintRunner{}

	targets, err := maintTargets(context.Background(), town, cfg, 5, maintModePlan, runner.run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v", err)
	}
	target, ok := targets[maintTargetRegistryCollect]
	if !ok {
		t.Fatalf("targets = %v, want %s beside the other targets", maintTargetNames(targets), maintTargetRegistryCollect)
	}
	if got := target.Name(); got != maintTargetRegistryCollect {
		t.Errorf("Name() = %q, want %q", got, maintTargetRegistryCollect)
	}
	plan, err := target.Plan(context.Background(), diskmaint.Policy{Keep: 5})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Refused != "" {
		t.Fatalf("Plan() refused: %s", plan.Refused)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != "collect" {
		t.Fatalf("actions = %+v, want the one collect action", plan.Actions)
	}
	if runner.calls != nil {
		t.Errorf("planning ran %v, want no program", runner.calls)
	}
}

func TestMaintTargetsWithoutARegistryKeepTheLocalTargets(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMaintRigs(t, town, `{"version":1,"rigs":{"gastown":{"git_url":"u"}}}`)
	targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, maintModePlan, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v, want the local targets", err)
	}
	want := []string{maintTargetBaks, maintTargetColima, maintTargetNodes, maintTargetSizes, maintTargetWorktree}
	if got := maintTargetNames(targets); !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v: no registry means no registry target, and the local and docker targets still run", got, want)
	}
}

func TestMaintColimaTargetPrunesThroughTheSandboxSocket(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	var env []string
	run := func(_ context.Context, e []string, _ string, _ ...string) ([]byte, error) {
		env = append(env, e...)
		return nil, nil
	}
	targets, err := maintTargets(context.Background(), t.TempDir(), polecatimage.Config{}, 5, maintModePlan, run, home, io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v", err)
	}
	if _, err := targets[maintTargetColima].Measure(context.Background()); err != nil {
		t.Fatalf("Measure() error = %v", err)
	}
	want := "DOCKER_HOST=unix://" + filepath.Join(home, ".colima", "gt-sbx", "docker.sock")
	if !slices.Contains(env, want) {
		t.Fatalf("docker env = %d entries, want %q: the colima target reads the sandbox's socket", len(env), want)
	}
}

func TestMaintNodesTargetNamesTheSandboxCluster(t *testing.T) {
	t.Parallel()
	runner := &fakeMaintRunner{}
	targets, err := maintTargets(context.Background(), t.TempDir(), polecatimage.Config{}, 5, maintModePlan, runner.run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v", err)
	}
	// The runner lists no nodes, so the plan refuses. The refusal names the
	// node prefix it looked for, and the docker call names the context.
	plan, err := targets[maintTargetNodes].Plan(context.Background(), diskmaint.Policy{Keep: 5})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if !strings.Contains(plan.Refused, "gt-sbx-") {
		t.Errorf("Refused = %q, want the sandbox's node prefix gt-sbx-", plan.Refused)
	}
	if len(runner.calls) == 0 || !strings.Contains(strings.Join(runner.calls[0], " "), "colima-gt-sbx") {
		t.Errorf("docker calls = %v, want one under the colima-gt-sbx context", runner.calls)
	}
}

// maintTargetNames returns a target map's names sorted, so a test compares the
// set of targets rather than the map's iteration order.
func maintTargetNames(targets map[string]diskmaint.Target) []string {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestMaintLocalTargetsRemoveOnlyInApply(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		apply   bool
		refused bool
	}{
		{name: "plan refuses to remove", apply: false, refused: true},
		{name: "apply may remove", apply: true, refused: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeMaintRigs(t, town, `{"version":1,"rigs":{"gastown":{"git_url":"u"}}}`)
			ledger := diskmaint.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), maintLedgerMaxBytes)
			targets := maintLocalTargets(town, t.TempDir(), (&fakeMaintRunner{}).run, ledger, tc.apply)
			// An empty plan removes nothing, so the only answer that differs
			// between the two modes is the dry-run gate itself. The worktree
			// target gates on its own config; the baks target gates on the
			// policy its plan carries, which the run sets to the mode's policy.
			worktreePlan := diskmaint.Plan{Target: maintTargetWorktree}
			baksPlan := diskmaint.Plan{Target: maintTargetBaks, Policy: diskmaint.Policy{Apply: tc.apply}}
			for name, plan := range map[string]diskmaint.Plan{
				maintTargetWorktree: worktreePlan,
				maintTargetBaks:     baksPlan,
			} {
				_, err := targets[name].Apply(context.Background(), plan)
				if got := err != nil; got != tc.refused {
					t.Errorf("%s Apply() refused = %v, want %v (error %v)", name, got, tc.refused, err)
				}
			}
		})
	}
}

func TestMaintRigNamesAreSortedAndARegistryThatCannotBeReadRefuses(t *testing.T) {
	t.Parallel()
	t.Run("sorted", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeMaintRigs(t, town, `{"version":1,"rigs":{"zulu":{"git_url":"u"},"alpha":{"git_url":"u"}}}`)
		got, err := maintRigNames(town)
		if err != nil {
			t.Fatalf("maintRigNames() error = %v", err)
		}
		if want := []string{"alpha", "zulu"}; !slices.Equal(got, want) {
			t.Errorf("maintRigNames() = %v, want %v", got, want)
		}
	})
	t.Run("missing registry is not an empty town", func(t *testing.T) {
		t.Parallel()
		got, err := maintRigNames(t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "rig registry") {
			t.Fatalf("maintRigNames() = %v, %v; want the missing rig registry refusal", got, err)
		}
	})
	t.Run("damaged registry refuses", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeMaintRigs(t, town, `{"version":1,"rigs":`)
		if _, err := maintRigNames(town); err == nil || !strings.Contains(err.Error(), "rig registry") {
			t.Fatalf("maintRigNames() error = %v, want the unreadable rig registry refusal", err)
		}
	})
}

func TestMaintRigNamesRefuseANameThatIsNotADirectoryName(t *testing.T) {
	t.Parallel()
	// Each name would join outside the rig it names, so the worktree target
	// must be refused rather than asked to look there.
	for _, name := range []string{"", ".", "..", "a/b", "../escape", "/abs"} {
		t.Run(fmt.Sprintf("name %q", name), func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeMaintRigs(t, town, fmt.Sprintf(`{"version":1,"rigs":{%q:{"git_url":"u"}}}`, name))
			targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, maintModePlan, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
			if err != nil {
				t.Fatalf("maintTargets() error = %v, want every target", err)
			}
			plan, err := targets[maintTargetWorktree].Plan(context.Background(), diskmaint.Policy{})
			if err != nil {
				t.Fatalf("worktree Plan() error = %v, want the refusal in the plan", err)
			}
			if !strings.Contains(plan.Refused, "not a directory in the town") {
				t.Fatalf("worktree plan refusal = %q, want the rig name refusal", plan.Refused)
			}
		})
	}
}

func TestMaintUnreadableRigRegistryRefusesOnlyTheWorktreeTarget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMaintRigs(t, town, `{"version":1,"rigs":`)
	cfg := polecatimage.Config{Registry: "localhost:5001"}
	targets, err := maintTargets(context.Background(), town, cfg, 5, maintModePlan, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v, want every target: a damaged rig registry refuses the worktree target alone", err)
	}
	if _, ok := targets[maintTargetRegistry]; !ok {
		t.Fatalf("targets = %v, want the registry target: a damaged rig registry must not take the registry run with it", maintTargetNames(targets))
	}
	plan, err := targets[maintTargetWorktree].Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("worktree Plan() error = %v, want the refusal in the plan", err)
	}
	if !strings.Contains(plan.Refused, "rig registry") {
		t.Fatalf("worktree plan refusal = %q, want it to name the rig registry", plan.Refused)
	}
}

// lsofExit1 is the error lsof's runner reports for "no process holds the file":
// the baks target reads the exit code through ExitCode().
type lsofExit1 struct{}

func (lsofExit1) Error() string { return "exit status 1" }
func (lsofExit1) ExitCode() int { return 1 }

// TestMaintRunSetsTheBaksGateFromTheMode drives the real run loop against a
// real baks target and a real backup file: a plan-mode run builds a dry-run
// plan and removes nothing, an apply-mode run builds an apply plan and removes
// the oldest copy. The mode reaches the gate through the policy runMaint hands
// Plan, which the baks target stamps on its plan, so this is the wiring a
// hand-built Plan cannot show (om d3a492b3cdfb).
func TestMaintRunSetsTheBaksGateFromTheMode(t *testing.T) {
	t.Parallel()
	closed := func(context.Context, []string, string, ...string) ([]byte, error) {
		// lsof exits 1 and prints nothing when no process holds the file.
		return nil, lsofExit1{}
	}
	cases := []struct {
		name     string
		mode     string
		wantGone bool
	}{
		{name: "plan mode keeps the file", mode: maintModePlan, wantGone: false},
		{name: "apply mode removes the oldest copy", mode: maintModeApply, wantGone: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, home := t.TempDir(), t.TempDir()
			binDir := filepath.Join(home, ".local", "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			var oldest string
			for i, name := range []string{"gt.bak-01", "gt.bak-02", "gt.bak-03"} {
				path := filepath.Join(binDir, name)
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				when := maintTestNow.Add(-time.Duration(i+1) * time.Hour)
				if err := os.Chtimes(path, when, when); err != nil {
					t.Fatal(err)
				}
				oldest = path
			}
			ledger := diskmaint.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), maintLedgerMaxBytes)
			deps := maintDeps{
				Targets: func() (map[string]diskmaint.Target, error) {
					return maintLocalTargets(town, home, closed, ledger, tc.mode == maintModeApply), nil
				},
				Now:  func() time.Time { return maintTestNow },
				Keep: 5,
			}
			if err := runMaint(io.Discard, deps, tc.mode, maintTargetBaks); err != nil {
				t.Fatalf("runMaint(%s) error = %v, want none", tc.mode, err)
			}
			_, statErr := os.Stat(oldest)
			if gone := os.IsNotExist(statErr); gone != tc.wantGone {
				t.Errorf("%s: oldest backup gone = %v, want %v (stat error %v)", tc.mode, gone, tc.wantGone, statErr)
			}
		})
	}
}

func TestMaintTargetsWireTheModeIntoTheDryRunGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mode    string
		refused bool
	}{
		{name: "a plan target refuses to remove", mode: maintModePlan, refused: true},
		{name: "an apply target may remove", mode: maintModeApply, refused: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeMaintRigs(t, town, `{"version":1,"rigs":{"gastown":{"git_url":"u"}}}`)
			targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, tc.mode, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
			if err != nil {
				t.Fatalf("maintTargets(%s) error = %v, want none", tc.mode, err)
			}
			// An empty plan removes nothing, so the only answer that differs
			// between the two modes is the dry-run gate the mode sets.
			_, applyErr := targets[maintTargetWorktree].Apply(context.Background(), diskmaint.Plan{Target: maintTargetWorktree})
			if got := applyErr != nil; got != tc.refused {
				t.Fatalf("%s worktree Apply() refused = %v, want %v (error %v)", tc.mode, got, tc.refused, applyErr)
			}
		})
	}
}

func TestMaintLedgerDirectoryIsMadeForAnApplyOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{name: "plan leaves none", mode: maintModePlan, want: false},
		{name: "apply makes one", mode: maintModeApply, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			// No registry configured: the ledger is the local targets' record,
			// not the registry's, so it is made for them too.
			if _, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, tc.mode, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard); err != nil {
				t.Fatalf("maintTargets(%s) error = %v, want none", tc.mode, err)
			}
			info, err := os.Stat(filepath.Join(town, ".runtime", "diskmaint"))
			if tc.want {
				if err != nil || !info.IsDir() {
					t.Fatalf("apply left no ledger directory (stat error %v): a delete it could not record must not run", err)
				}
				return
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a plan left the ledger directory behind (stat error %v): plan is a dry run and must write nothing", err)
			}
		})
	}
}

// writeMaintRigs writes the town's rig registry, the file a rig list is read
// from.
func writeMaintRigs(t *testing.T, town, doc string) {
	t.Helper()
	dir := filepath.Join(town, "mayor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rigs.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMaintTargetsRefuseARegistryThatIsNotHostPort(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"/town/.runtime/sandboxes", "http://localhost:5001"} {
		cfg := polecatimage.Config{Registry: value}
		_, err := maintTargets(context.Background(), t.TempDir(), cfg, 5, maintModePlan, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "host:port") {
			t.Errorf("maintTargets(registry %q) error = %v, want the host:port refusal", value, err)
		}
	}
}

func TestMaintApplyRefusesAnUnwritableLedgerDirectory(t *testing.T) {
	t.Parallel()
	// A regular file where the town should be is a runtime directory that
	// cannot exist, the way a town on a read-only or full disk fails. An apply
	// must refuse before it deletes anything it cannot record.
	town := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(town, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := polecatimage.Config{Registry: "localhost:5001"}
	_, err := maintTargets(context.Background(), town, cfg, 5, maintModeApply, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ledger directory") {
		t.Fatalf("maintTargets() error = %v, want the ledger directory refusal", err)
	}
}

func TestMaintRefusesWhenNoTargetsAreConfigured(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		targets func() (map[string]diskmaint.Target, error)
		target  string
	}{
		{"no reader", nil, ""},
		{"no target named", func() (map[string]diskmaint.Target, error) { return map[string]diskmaint.Target{}, nil }, ""},
		{"unknown target", func() (map[string]diskmaint.Target, error) { return map[string]diskmaint.Target{}, nil }, "gamma"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := maintDeps{Targets: tc.targets, Keep: 5}
			err := runMaint(io.Discard, deps, maintModePlan, tc.target)
			if err == nil || !strings.Contains(err.Error(), "no maintenance targets are configured") {
				t.Fatalf("runMaint() error = %v, want the empty target set", err)
			}
		})
	}
}

func TestMaintRunReportsAFailedTownLookup(t *testing.T) {
	t.Parallel()
	townErr := errors.New("no town here")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)

	err := maintRunIn(cmd, func() (string, error) { return "", townErr }, maintProductionDeps, func(string) string { return "" }, maintModePlan, nil)
	if !errors.Is(err, townErr) {
		t.Fatalf("maintRunIn() error = %v, want the lookup failure", err)
	}
}

func TestMaintProductionDepsRefuseUnreadableInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"stale minutes", map[string]string{"POLECAT_IMAGE_STALE_MINUTES": "twenty"}, "POLECAT_IMAGE_STALE_MINUTES"},
		{"keep is zero", map[string]string{"POLECAT_IMAGE_REGISTRY_KEEP": "0"}, "POLECAT_IMAGE_REGISTRY_KEEP"},
		{"no home", map[string]string{"POLECAT_IMAGE_REGISTRY": "localhost:5001"}, "HOME is unset"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			get := func(key string) string { return tc.env[key] }
			_, err := maintProductionDeps(context.Background(), io.Discard, t.TempDir(), get, maintModePlan)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("maintProductionDeps() error = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

func TestMaintRunReportsAFailedTargetBuild(t *testing.T) {
	t.Parallel()
	// A town whose settings file cannot be read (here because the town path is
	// a regular file) must fail the run rather than read as a town with no
	// settings, which would build no target and delete nothing.
	town := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(town, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	get := func(key string) string {
		switch key {
		case "POLECAT_IMAGE_REGISTRY":
			return "localhost:5001"
		case "HOME":
			return home
		}
		return ""
	}

	err := maintRunIn(cmd, func() (string, error) { return town, nil }, maintProductionDeps, get, maintModePlan, nil)
	if err == nil || !strings.Contains(err.Error(), "config.json could not be read") {
		t.Fatalf("maintRunIn() error = %v, want the unreadable town settings", err)
	}
}

// maintCtxKey marks the value the context tests put in a command's context, so
// a target can report which context it was handed.
type maintCtxKey struct{}

func TestMaintRunInPassesTheCommandContext(t *testing.T) {
	t.Parallel()
	const marker = "the command's own context"
	fake := &fakeMaintTarget{
		name:   "registry",
		plan:   maintTestPlan(),
		result: diskmaint.Result{Target: "registry"},
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.WithValue(context.Background(), maintCtxKey{}, marker))
	cmd.SetOut(io.Discard)
	buildDeps := func(context.Context, io.Writer, string, func(string) string, string) (maintDeps, error) {
		return maintDepsFor(fake), nil
	}

	if err := maintRunIn(cmd, func() (string, error) { return t.TempDir(), nil }, buildDeps, func(string) string { return "" }, maintModeApply, nil); err != nil {
		t.Fatalf("maintRunIn(apply) error = %v, want none", err)
	}
	if len(fake.contexts) != 2 {
		t.Fatalf("the target was called %d times, want plan and apply", len(fake.contexts))
	}
	for i, ctx := range fake.contexts {
		if got, _ := ctx.Value(maintCtxKey{}).(string); got != marker {
			t.Errorf("call %d got a context without the command's value (got %q): an interrupt would not reach the target", i, got)
		}
	}
}

func TestMaintPlanCreatesNoLedgerDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	get := func(key string) string {
		switch key {
		case "POLECAT_IMAGE_REGISTRY":
			return "localhost:5001"
		case "HOME":
			return t.TempDir()
		}
		return ""
	}

	if _, err := maintProductionDeps(context.Background(), io.Discard, town, get, maintModePlan); err != nil {
		t.Fatalf("maintProductionDeps(plan) error = %v, want none", err)
	}
	runtime := filepath.Join(town, ".runtime")
	if _, err := os.Stat(runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a plan left %s behind (stat error %v): plan is a dry run and must write nothing", runtime, err)
	}
}

func TestMaintApplyCreatesTheLedgerDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	get := func(key string) string {
		switch key {
		case "POLECAT_IMAGE_REGISTRY":
			return "localhost:5001"
		case "HOME":
			return t.TempDir()
		}
		return ""
	}

	if _, err := maintProductionDeps(context.Background(), io.Discard, town, get, maintModeApply); err != nil {
		t.Fatalf("maintProductionDeps(apply) error = %v, want none", err)
	}
	info, err := os.Stat(filepath.Join(town, ".runtime", "diskmaint"))
	if err != nil || !info.IsDir() {
		t.Fatalf("apply left no ledger directory (stat error %v): a delete it could not record must not run", err)
	}
}

func TestMaintExecFailureCarriesTheLastStderrLine(t *testing.T) {
	t.Parallel()
	cause := errors.New("exit status 3")
	cases := []struct {
		name   string
		stderr string
		want   string
		absent string
	}{
		{"last line only", "warning: one\nthe registry is down\n", "the registry is down", "warning: one"},
		{"no stderr", "", "kubectl", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := maintExecFailure("kubectl", cause, tc.stderr)
			if !errors.Is(err, cause) {
				t.Fatalf("maintExecFailure() = %v, want it to wrap the cause", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			if tc.absent != "" && strings.Contains(err.Error(), tc.absent) {
				t.Errorf("error = %q, want it to drop %q", err, tc.absent)
			}
		})
	}
}

// The maint help text wraps within 80 columns: no line of it runs past 80.
func TestMaintHelpIsWrapped(t *testing.T) {
	t.Parallel()
	for _, l := range strings.Split(maintCmd.Long, "\n") {
		if len(l) > 80 {
			t.Errorf("help line is %d columns, want at most 80: %q", len(l), l)
		}
	}
}

func TestMaintTargetsRefuseWithoutAHome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  polecatimage.Config
		want string
	}{
		// The local targets name their paths under HOME, so a town with no
		// registry still refuses rather than join them against the working
		// directory.
		{name: "no registry still needs HOME", cfg: polecatimage.Config{}, want: "HOME is unset"},
		// A town that configures a registry keeps the registry's own refusal.
		{name: "the registry keeps its refusal", cfg: polecatimage.Config{Registry: "localhost:5001"}, want: "colima docker socket"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			targets, err := maintTargets(context.Background(), town, tc.cfg, 5, maintModeApply, (&fakeMaintRunner{}).run, "", io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("maintTargets() error = %v, want one naming %q", err, tc.want)
			}
			if targets != nil {
				t.Errorf("targets = %v, want none: a blank HOME builds no docker target", maintTargetNames(targets))
			}
			// The ledger directory is made after the HOME check, so a refused
			// apply leaves nothing behind.
			if _, statErr := os.Stat(filepath.Join(town, ".runtime", "diskmaint")); !os.IsNotExist(statErr) {
				t.Errorf("ledger directory stat = %v, want it absent after a refused apply", statErr)
			}
		})
	}
}

// maintCloudRunner answers the calls the cloud target makes: cat of the two
// files the plan reads, and the docker reads and removal. It records every call
// and the environment it was handed, so a test drives the real target with no
// daemon and no host file.
type maintCloudRunner struct {
	files  map[string]string
	images string
	catErr error
	calls  [][]string
	envs   [][]string
}

func (f *maintCloudRunner) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	f.envs = append(f.envs, env)
	switch {
	case name == "cat" && len(args) == 2:
		if f.catErr != nil {
			return nil, f.catErr
		}
		content, ok := f.files[args[1]]
		if !ok {
			return nil, fmt.Errorf("cat %s: no such file", args[1])
		}
		return []byte(content), nil
	case name == "docker" && len(args) >= 2 && args[0] == "image" && args[1] == "ls":
		return []byte(f.images), nil
	case name == "docker" && len(args) >= 1 && args[0] == "ps":
		return nil, nil
	case name == "docker" && len(args) >= 2 && args[0] == "image" && args[1] == "rm":
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected call: %s %v", name, args)
}

// maintCloudImageID is the image id of the i-th row maintCloudImageRows makes.
func maintCloudImageID(i int) string { return fmt.Sprintf("sha256:%064x", i) }

// maintCloudImageRows is n fractals-nextjs tags as docker image ls prints them:
// t1 the oldest and t<n> the newest, each a gigabyte on an image of its own, so
// a plan keeping the newest five removes exactly t1's image.
func maintCloudImageRows(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "{\"Repository\":\"fractals-nextjs\",\"Tag\":\"t%d\",\"ID\":%q,\"Size\":\"1GB\","+
			"\"CreatedAt\":\"2026-11-%02d 00:00:00 +0000 UTC\"}\n", i, maintCloudImageID(i), i)
	}
	return b.String()
}

// writeMaintCloudTown writes the town config that names a cloud VM, and answers
// the socket, the runner label list and the deploy workflow the plan reads.
func writeMaintCloudTown(t *testing.T, dockerHost string) (town, labelsPath, deployPath string) {
	t.Helper()
	town = t.TempDir()
	labelsPath = filepath.Join(town, "runner-labels")
	deployPath = filepath.Join(town, "deploy.yaml")
	settings := filepath.Join(town, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"daemon":{"env":{%q:%q,%q:%q,%q:%q}}}`,
		maintCloudDockerHostKey, dockerHost,
		maintCloudLabelsKey, labelsPath,
		maintCloudDeployKey, deployPath)
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return town, labelsPath, deployPath
}

// maintCloudDeps is the deps the command builds from the town's real targets,
// with the fake runner behind them, so a test drives runMaintIn's whole path
// with no daemon.
func maintCloudDeps(run diskmaint.Runner, home string) maintDepsBuilder {
	return func(ctx context.Context, out io.Writer, town string, _ func(string) string, mode string) (maintDeps, error) {
		targets, err := maintTargets(ctx, town, polecatimage.Config{}, 5, mode, run, home, out)
		if err != nil {
			return maintDeps{}, err
		}
		return maintDeps{
			Targets: func() (map[string]diskmaint.Target, error) { return targets, nil },
			Keep:    5,
		}, nil
	}
}

// maintSubcommand returns parent's subcommand named name, failing the test when
// the command does not offer it.
func maintSubcommand(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, cmd := range parent.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}
	t.Fatalf("%s offers no %q subcommand", parent.Name(), name)
	return nil
}

// maintReadCloudLedger reads the run's ledger back, newest ten entries.
func maintReadCloudLedger(t *testing.T, town string) []diskmaint.LedgerEntry {
	t.Helper()
	path := filepath.Join(town, ".runtime", "diskmaint", "ledger.jsonl")
	entries, err := diskmaint.NewLedger(path, 0).Tail(10)
	if err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	return entries
}

// TestMaintPlanCloudPrintsTheDaemonsDryRunPlan drives `gt maint plan cloud`
// through the real command path: the town config names the VM's socket and the
// two files the plan reads, and the fake runner answers the fake daemon. The
// run prints the plan it would apply and records it as the dry run it was.
func TestMaintPlanCloudPrintsTheDaemonsDryRunPlan(t *testing.T) {
	t.Parallel()
	town, labelsPath, deployPath := writeMaintCloudTown(t, "unix:///cloud/docker.sock")
	runner := &maintCloudRunner{
		files: map[string]string{
			labelsPath: "node:docker://code.forgejo.org/oci/node:lts\n",
			deployPath: "# the workflow names no image\n",
		},
		images: maintCloudImageRows(6),
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)

	err := maintRunIn(cmd, func() (string, error) { return town, nil }, maintCloudDeps(runner.run, t.TempDir()),
		func(string) string { return "" }, maintModePlan, []string{maintTargetCloud})
	if err != nil {
		t.Fatalf("maintRunIn(plan cloud) error = %v, want none", err)
	}
	if !strings.Contains(out.String(), "cloud: 1 action(s), 5 kept") {
		t.Errorf("plan output = %q, want the cloud plan's counts", out.String())
	}
	removed := maintCloudImageID(1)
	if !strings.Contains(out.String(), "would remove-image "+removed) {
		t.Errorf("plan output = %q, want the removal of %s", out.String(), removed)
	}
	entries := maintReadCloudLedger(t, town)
	if len(entries) != 1 {
		t.Fatalf("ledger = %+v, want one entry for the plan run", entries)
	}
	if e := entries[0]; e.Target != maintTargetCloud || e.Mode != maintLedgerModeDryRun || e.Planned != 1 || e.Done != 0 || e.Refused != "" {
		t.Fatalf("ledger entry = %+v, want target cloud, mode dry-run, one planned, none done, no refusal", entries[0])
	}
}

// TestMaintApplyCloudRemovesThroughTheDaemon drives `gt maint apply cloud`: the
// subcommand is registered under maint, the apply removes the planned image by
// id through the VM's socket, and the run lands as one apply line.
func TestMaintApplyCloudRemovesThroughTheDaemon(t *testing.T) {
	t.Parallel()
	// The apply leaf is registered under maint; maintRunIn below drives the run
	// path that leaf and the plan leaf share.
	maintSubcommand(t, maintCmd, maintModeApply)
	town, labelsPath, deployPath := writeMaintCloudTown(t, "unix:///cloud/docker.sock")
	runner := &maintCloudRunner{
		files: map[string]string{
			labelsPath: "node:docker://code.forgejo.org/oci/node:lts\n",
			deployPath: "# the workflow names no image\n",
		},
		images: maintCloudImageRows(6),
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)

	err := maintRunIn(cmd, func() (string, error) { return town, nil }, maintCloudDeps(runner.run, t.TempDir()),
		func(string) string { return "" }, maintModeApply, []string{maintTargetCloud})
	if err != nil {
		t.Fatalf("maintRunIn(apply cloud) error = %v, want none", err)
	}
	removed := maintCloudImageID(1)
	want := []string{"docker", "image", "rm", "--no-prune", "--force", removed}
	ran := false
	for _, call := range runner.calls {
		ran = ran || slices.Equal(call, want)
	}
	if !ran {
		t.Fatalf("docker calls = %v, want %v: an apply removes the planned image by id", runner.calls, want)
	}
	entries := maintReadCloudLedger(t, town)
	if len(entries) != 1 {
		t.Fatalf("ledger = %+v, want one entry for the apply run", entries)
	}
	if e := entries[0]; e.Target != maintTargetCloud || e.Mode != maintModeApply || e.Planned != 1 || e.Done != 1 || e.BytesFreed != 1_000_000_000 {
		t.Fatalf("ledger entry = %+v, want target cloud, mode apply, one planned and done, a gigabyte freed", entries[0])
	}
}

// TestMaintCloudSocketComesFromTheTownConfig pins that the socket is the town
// config's, not a constant: two towns with two settings read two daemons. A
// town that names no socket has no cloud VM, so it gets no cloud target.
func TestMaintCloudSocketComesFromTheTownConfig(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"unix:///cloud/one.sock", "unix:///cloud/two.sock"} {
		town, _, _ := writeMaintCloudTown(t, host)
		runner := &maintCloudRunner{}
		targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, maintModePlan, runner.run, t.TempDir(), io.Discard)
		if err != nil {
			t.Fatalf("maintTargets() error = %v", err)
		}
		target, ok := targets[maintTargetCloud]
		if !ok {
			t.Fatalf("targets = %v, want %s beside the other targets", maintTargetNames(targets), maintTargetCloud)
		}
		if _, err := target.Measure(context.Background()); err != nil {
			t.Fatalf("Measure() error = %v", err)
		}
		if want := "DOCKER_HOST=" + host; !slices.Contains(runner.envs[0], want) {
			t.Fatalf("docker env = %v, want %q: the socket is the town config's", runner.envs[0], want)
		}
	}

	town := t.TempDir()
	writeMaintRigs(t, town, `{"version":1,"rigs":{"gastown":{"git_url":"u"}}}`)
	targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, maintModePlan, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v", err)
	}
	if _, ok := targets[maintTargetCloud]; ok {
		t.Fatalf("targets = %v, want none: a town that names no cloud socket has no cloud VM", maintTargetNames(targets))
	}
}

// TestMaintCloudRefusesSettingsItCannotUse pins the startup refusals: a socket
// docker would be pointed at another host with, and a file path the plan would
// read from whatever directory the command ran in.
func TestMaintCloudRefusesSettingsItCannotUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings string
		want     string
	}{
		{"a socket on another host", `{"daemon":{"env":{"GT_CLOUD_DOCKER_HOST":"tcp://10.0.0.2:2375"}}}`, maintCloudDockerHostKey},
		{"a socket with a relative path", `{"daemon":{"env":{"GT_CLOUD_DOCKER_HOST":"unix://cloud/docker.sock"}}}`, maintCloudDockerHostKey},
		{"a relative runner label list", `{"daemon":{"env":{"GT_CLOUD_DOCKER_HOST":"unix:///cloud/docker.sock","GT_CLOUD_RUNNER_LABELS":"runner-labels"}}}`, maintCloudLabelsKey},
		{"a relative deploy workflow", `{"daemon":{"env":{"GT_CLOUD_DOCKER_HOST":"unix:///cloud/docker.sock","GT_CLOUD_DEPLOY_WORKFLOW":"deploy.yaml"}}}`, maintCloudDeployKey},
		{"a town config that does not parse", `{"daemon":`, "cloud settings"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			settings := filepath.Join(town, "settings")
			if err := os.MkdirAll(settings, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(tc.settings), 0o644); err != nil {
				t.Fatal(err)
			}
			targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, maintModePlan, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
			if err != nil {
				t.Fatalf("maintTargets() error = %v, want every target: a cloud setting the target cannot use must not fail the build", err)
			}
			for _, name := range []string{maintTargetBaks, maintTargetSizes, maintTargetWorktree} {
				if _, ok := targets[name]; !ok {
					t.Fatalf("targets = %v, want %s beside the refused cloud target", maintTargetNames(targets), name)
				}
			}
			plan, err := targets[maintTargetCloud].Plan(context.Background(), diskmaint.Policy{})
			if err != nil {
				t.Fatalf("cloud Plan() error = %v, want the refusal in the plan", err)
			}
			if !strings.Contains(plan.Refused, tc.want) {
				t.Fatalf("cloud plan refusal = %q, want it to name %q", plan.Refused, tc.want)
			}
		})
	}
}

// TestMaintUnreadableCloudSettingsRefuseOnlyTheCloudTarget pins the scope of a
// cloud setting the target cannot use: a run of another target passes, and a
// run that includes cloud fails, naming the setting.
func TestMaintUnreadableCloudSettingsRefuseOnlyTheCloudTarget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	settings := filepath.Join(town, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"daemon":{"env":{"GT_CLOUD_DOCKER_HOST":"tcp://10.0.0.2:2375"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, maintModePlan, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v, want the other targets", err)
	}
	deps := maintDeps{
		Targets: func() (map[string]diskmaint.Target, error) { return targets, nil },
		Now:     func() time.Time { return maintTestNow },
		Keep:    5,
	}
	if err := runMaint(io.Discard, deps, maintModePlan, maintTargetSizes); err != nil {
		t.Fatalf("runMaint(plan sizes) error = %v, want none: the unreadable cloud setting is the cloud target's", err)
	}
	if err := runMaint(io.Discard, deps, maintModePlan, maintTargetCloud); err == nil || !strings.Contains(err.Error(), maintCloudDockerHostKey) {
		t.Fatalf("runMaint(plan cloud) error = %v, want the refusal to name %s", err, maintCloudDockerHostKey)
	}
}

// TestMaintCloudRecordsARefusedPlanOnce pins that a refused plan is one ledger
// line in either mode and removes nothing: the plan never reaches Apply, so the
// run's line is the only record of it.
func TestMaintCloudRecordsARefusedPlanOnce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, mode, wantMode string
	}{
		{"a dry run", maintModePlan, maintLedgerModeDryRun},
		{"an apply", maintModeApply, maintModeApply},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, labelsPath, deployPath := writeMaintCloudTown(t, "unix:///cloud/docker.sock")
			runner := &maintCloudRunner{
				files: map[string]string{
					labelsPath: "node:docker://code.forgejo.org/oci/node:lts\n",
					deployPath: "# the workflow names no image\n",
				},
				catErr: errors.New("exit status 1"),
			}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(io.Discard)

			err := maintRunIn(cmd, func() (string, error) { return town, nil }, maintCloudDeps(runner.run, t.TempDir()),
				func(string) string { return "" }, tc.mode, []string{maintTargetCloud})
			if err == nil || !strings.Contains(err.Error(), "plan refused") {
				t.Fatalf("maintRunIn(%s cloud) error = %v, want the refusal", tc.mode, err)
			}
			for _, call := range runner.calls {
				if len(call) > 2 && call[0] == "docker" && call[1] == "image" && call[2] == "rm" {
					t.Fatalf("a refused plan ran %v, want no removal", call)
				}
			}
			entries := maintReadCloudLedger(t, town)
			if len(entries) != 1 {
				t.Fatalf("ledger = %+v, want one entry for the refused run", entries)
			}
			if e := entries[0]; e.Target != maintTargetCloud || e.Mode != tc.wantMode || e.Refused == "" {
				t.Fatalf("ledger entry = %+v, want target cloud, mode %s and the refusal", entries[0], tc.wantMode)
			}
		})
	}
}

// TestMaintCloudPlanRefusesALedgerDirectoryItCannotMake pins that a plan of a
// town with a cloud target refuses before it plans when the line it must record
// could not be written: the run would otherwise leave no record of what it
// decided.
func TestMaintCloudPlanRefusesALedgerDirectoryItCannotMake(t *testing.T) {
	t.Parallel()
	town, _, _ := writeMaintCloudTown(t, "unix:///cloud/docker.sock")
	// A regular file where the runtime directory belongs is a ledger directory
	// that cannot exist, the way a town on a full disk fails.
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, maintModePlan, (&maintCloudRunner{}).run, t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ledger directory") {
		t.Fatalf("maintTargets(plan) = %v, %v; want the ledger directory refusal", maintTargetNames(targets), err)
	}
}

// TestMaintTargetsRefuseACloudTargetWithoutARunner pins that a cloud VM whose
// target cannot read its daemon is refused at build, never carried as a target
// that measures nothing.
func TestMaintTargetsRefuseACloudTargetWithoutARunner(t *testing.T) {
	t.Parallel()
	town, _, _ := writeMaintCloudTown(t, "unix:///cloud/docker.sock")
	targets, err := maintTargets(context.Background(), town, polecatimage.Config{}, 5, maintModePlan, nil, t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "building the cloud target") {
		t.Fatalf("maintTargets() = %v, %v; want the cloud target build refusal", maintTargetNames(targets), err)
	}
}

// TestMaintCloudLedgerLeavesTheTargetsErrorAlone pins the wrapper's contract
// with the target it wraps: a plan that failed is reported and recorded as
// nothing, because there is no plan to record.
func TestMaintCloudLedgerLeavesTheTargetsErrorAlone(t *testing.T) {
	t.Parallel()
	planErr := errors.New("the daemon did not answer")
	ledger := diskmaint.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), maintLedgerMaxBytes)
	wrapped := maintCloudLedger{inner: &fakeMaintTarget{name: maintTargetCloud, planErr: planErr}, ledger: ledger}

	_, err := wrapped.Plan(context.Background(), diskmaint.Policy{})
	if !errors.Is(err, planErr) {
		t.Fatalf("Plan() error = %v, want the target's own error", err)
	}
	if entries, err := ledger.Tail(10); err != nil || len(entries) != 0 {
		t.Fatalf("ledger = %+v, %v; want no line for a plan that was never built", entries, err)
	}
}

// TestMaintCloudLedgerReportsALedgerItCannotWrite pins that a run which cannot
// record its line fails rather than passing as clean.
func TestMaintCloudLedgerReportsALedgerItCannotWrite(t *testing.T) {
	t.Parallel()
	// A ledger under a directory that does not exist is a write that cannot
	// land, here without depending on the user the suite runs as.
	ledger := diskmaint.NewLedger(filepath.Join(t.TempDir(), "gone", "ledger.jsonl"), maintLedgerMaxBytes)
	wrapped := maintCloudLedger{inner: &fakeMaintTarget{name: maintTargetCloud}, ledger: ledger}

	_, err := wrapped.Plan(context.Background(), diskmaint.Policy{})
	if err == nil || !strings.Contains(err.Error(), "record the cloud run") {
		t.Fatalf("Plan() error = %v, want the ledger write refusal", err)
	}
}
