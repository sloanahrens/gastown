package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
}

func (f *fakeMaintTarget) Name() string { return f.name }

func (f *fakeMaintTarget) Measure(context.Context) (diskmaint.Measurement, error) {
	return diskmaint.Measurement{}, nil
}

func (f *fakeMaintTarget) Plan(_ context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	f.policies = append(f.policies, p)
	return f.plan, f.planErr
}

func (f *fakeMaintTarget) Apply(_ context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
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

	targets, err := maintTargets(context.Background(), town, cfg, 5, runner.run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v", err)
	}
	if len(targets) != 1 || targets[maintTargetRegistry] == nil {
		t.Fatalf("targets = %v, want the registry alone", targets)
	}
	if got := targets[maintTargetRegistry].Name(); got != maintTargetRegistry {
		t.Errorf("Name() = %q, want %q", got, maintTargetRegistry)
	}
	if runner.calls != nil {
		t.Errorf("building a target ran %v, want no program", runner.calls)
	}
}

func TestMaintTargetsWithoutARegistryAreNone(t *testing.T) {
	t.Parallel()
	targets, err := maintTargets(context.Background(), t.TempDir(), polecatimage.Config{}, 5, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maintTargets() error = %v, want none", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %v, want none: a town with no registry has nothing to prune", targets)
	}
}

func TestMaintTargetsRefuseARegistryThatIsNotHostPort(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"/town/.runtime/sandboxes", "http://localhost:5001"} {
		cfg := polecatimage.Config{Registry: value}
		_, err := maintTargets(context.Background(), t.TempDir(), cfg, 5, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "host:port") {
			t.Errorf("maintTargets(registry %q) error = %v, want the host:port refusal", value, err)
		}
	}
}

func TestMaintTargetsReportAnUnwritableLedgerDirectory(t *testing.T) {
	t.Parallel()
	// A regular file where the town should be is a runtime directory that
	// cannot exist, the way a town on a read-only or full disk fails.
	town := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(town, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := polecatimage.Config{Registry: "localhost:5001"}
	_, err := maintTargets(context.Background(), town, cfg, 5, (&fakeMaintRunner{}).run, t.TempDir(), io.Discard)
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

	err := maintRunIn(cmd, func() (string, error) { return "", townErr }, func(string) string { return "" }, maintModePlan, nil)
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
			_, err := maintProductionDeps(context.Background(), io.Discard, t.TempDir(), get)
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

	err := maintRunIn(cmd, func() (string, error) { return town, nil }, get, maintModePlan, nil)
	if err == nil || !strings.Contains(err.Error(), "config.json could not be read") {
		t.Fatalf("maintRunIn() error = %v, want the unreadable town settings", err)
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

func TestMaintTargetsRefuseWithoutAHome(t *testing.T) {
	t.Parallel()
	cfg := polecatimage.Config{Registry: "localhost:5001"}
	_, err := maintTargets(context.Background(), t.TempDir(), cfg, 5, (&fakeMaintRunner{}).run, "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "HOME is unset") {
		t.Fatalf("maintTargets() error = %v, want the missing HOME", err)
	}
}
