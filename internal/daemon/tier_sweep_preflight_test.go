package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// preflightShellSummary is the shell stage's own verdict, which the cycle still
// records while the container stage is declined.
const preflightShellSummary = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n"

// preflightTightListing is a control node with 6 CPU and 24 GiB allocatable and
// no pod on it. The incident's runner block (cpus 4, memory 5, dind) asks
// 4200m/5376Mi with its sync and reader pods, so the free amount less the job
// is 1800m: 200m short of the 2000m headroom.
const preflightTightListing = `{
  "kind": "List",
  "items": [
    {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "6", "memory": "24Gi"}}}
  ]
}`

// preflightShorterListing is the same node with 5 CPU: the reason stands, but
// the figures have moved, as they do while the node's pods come and go.
const preflightShorterListing = `{
  "kind": "List",
  "items": [
    {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "5", "memory": "24Gi"}}}
  ]
}`

// preflightRoomyListing is the same node with 7 CPU: the run fits with 2800m to
// spare, which is over the headroom.
const preflightRoomyListing = `{
  "kind": "List",
  "items": [
    {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "7", "memory": "24Gi"}}}
  ]
}`

// preflightDaemon is a sweep daemon whose runner block launches the Kubernetes
// Job the capacity preflight guards, with every command answered by cmds
// instead of a cluster, and every stage of a cycle answered without a host run.
func preflightDaemon(t *testing.T, now time.Time, cmds *runnerFakeCmds) (*Daemon, sweeprunner.Spec, tierSweepStage, *tierSweepFakeBeads, *bytes.Buffer) {
	t.Helper()
	cfg := runnerTestConfig(t)
	cfg.Backend = config.TierSweepRunnerKubernetes
	cfg.Context = "gt-ctl"
	cfg.CPUs = 4
	cfg.Memory = 5
	d, _, bd, logs := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = cfg
	d.execCmd = cmds.run
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return runnerTestSHA, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.tierSweepSeams.run = func(ctx context.Context, dir string, st tierSweepStage) tierSweepStageResult {
		if st.slot {
			// Short, so a launch this test expects to be declined defers in
			// milliseconds instead of waiting out the real stage budget.
			st.budget = 50 * time.Millisecond
			return d.tierSweepRunStage(ctx, dir, st)
		}
		return tierSweepStageResult{output: preflightShellSummary, exitCode: 0, ran: true}
	}
	// The budget is short so a launch this test expects to be declined defers
	// in milliseconds rather than waiting out a real run.
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: 50 * time.Millisecond}
	spec, ok, err := d.tierSweepRunnerSpec(st)
	if err != nil || !ok {
		t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
	}
	return d, spec, st, bd, logs
}

// A node whose free amount does not leave the headroom is never launched on: no
// Job is created, the cycle defers without a bead and without advancing the
// patrol clock, and the decline line is one line an hour while the reason
// stands. Once the node has room, the same stage creates the Job and logs the
// three figures first (gt-gg9hv.38).
func TestTierSweepPreflightDeclinesWithoutHeadroom(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	cmds := &runnerFakeCmds{answer: func(argv []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "nodes,pods") {
			return []byte(preflightTightListing), nil, nil
		}
		return nil, nil, nil
	}}
	d, spec, st, bd, logs := preflightDaemon(t, now, cmds)

	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()
	if d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
		t.Fatal("the cycle reached a verdict; a declined launch learned nothing about the tree and defers")
	}
	if len(bd.created) != 0 {
		t.Errorf("beads = %+v, want none: a decline is not a tree failure", bd.created)
	}
	for _, ran := range cmds.ran {
		if argv := strings.Fields(ran); slices.Contains(argv, "apply") || slices.Contains(argv, "create") {
			t.Errorf("step %q ran; a declined launch creates nothing", ran)
		}
	}
	// The patrol clock is written only by a cycle that reached a verdict, so the
	// next heartbeat is due rather than this decline consuming the hour.
	if _, found, _, err := latestPatrolLastRun(d.config.TownRoot, "tier_sweep", d.tierSweepRun.last()); err != nil || found {
		t.Errorf("patrol clock found = %v, err = %v; want it not advanced", found, err)
	}
	wantDecline := "tier_sweep: not launching on gt-ctl: node gt-control: free 6000m/24576Mi, job needs 4200m/5376Mi, headroom 2000m/2048Mi"
	if got := strings.Count(logs.String(), "tier_sweep: not launching on gt-ctl:"); got != 1 {
		t.Fatalf("decline lines = %d after one cycle, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), wantDecline) {
		t.Errorf("the decline line is missing %q:\n%s", wantDecline, logs.String())
	}

	// The deferred cycle retries on the next heartbeat; inside the hour the
	// reason stands silently, even when the figures have moved.
	clk := d.clock.(*clockwork.FakeClock)
	clk.Advance(30 * time.Minute)
	cmds.answer = func(argv []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "nodes,pods") {
			return []byte(preflightShorterListing), nil, nil
		}
		return nil, nil, nil
	}
	if started := d.tierSweepLaunchRunner(context.Background(), st, spec); !started.declined {
		t.Fatalf("launch = %+v, want it declined again", started)
	}
	if got := strings.Count(logs.String(), "tier_sweep: not launching on gt-ctl:"); got != 1 {
		t.Errorf("decline lines = %d half an hour later, want still 1: the reason stands though the figures moved", got)
	}
	clk.Advance(31 * time.Minute)
	cmds.answer = func(argv []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "nodes,pods") {
			return []byte(preflightTightListing), nil, nil
		}
		return nil, nil, nil
	}
	d.tierSweepLaunchRunner(context.Background(), st, spec)
	if got := strings.Count(logs.String(), "tier_sweep: not launching on gt-ctl:"); got != 2 {
		t.Errorf("decline lines = %d past the hour, want 2", got)
	}

	// A node with room launches the Job, and the three figures are logged first.
	// The priority guard's listing answers a healthy cluster so this test is
	// about the capacity preflight alone.
	cmds.answer = func(argv []string) ([]byte, []byte, error) {
		switch {
		case slices.Contains(argv, "nodes,pods"):
			return []byte(preflightRoomyListing), nil, nil
		case slices.Contains(argv, "pods,priorityclasses"):
			return []byte(runnerPriorityListing), nil, nil
		}
		return nil, nil, nil
	}
	started := d.tierSweepLaunchRunner(context.Background(), st, spec)
	if started.declined || started.err != nil {
		t.Fatalf("launch = %+v, want it started once the node has room", started)
	}
	if !appliedJobDocument(cmds.stdin) {
		t.Fatalf("the launch applied no Job document; steps run: %q", cmds.ran)
	}
	wantLaunch := "tier_sweep: preflight on gt-ctl: node gt-control: allocatable 7000m/24576Mi, requested 0m/0Mi, job 4200m/5376Mi"
	if !strings.Contains(logs.String(), wantLaunch) {
		t.Errorf("the launch line is missing %q:\n%s", wantLaunch, logs.String())
	}
}

// A node that cannot be read is not a pass: a failing command and an answer
// that is not the listing each decline the launch, with the error in the line,
// and create nothing. A Kubernetes plan with no capacity check at all is
// refused the same way, one line an hour while it stays broken (gt-gg9hv.38).
func TestTierSweepPreflightRefusesWhenTheNodeCannotBeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(argv []string) ([]byte, []byte, error)
		want   string
	}{
		{
			name: "the command fails",
			answer: func(argv []string) ([]byte, []byte, error) {
				if slices.Contains(argv, "nodes,pods") {
					return nil, nil, errors.New("exit status 1")
				}
				return nil, nil, nil
			},
			want: "tier_sweep: not launching on gt-ctl: cannot read the node's capacity: exit status 1",
		},
		{
			name: "the answer cannot be read",
			answer: func(argv []string) ([]byte, []byte, error) {
				if slices.Contains(argv, "nodes,pods") {
					return []byte("Error from server (Forbidden)"), nil, nil
				}
				return nil, nil, nil
			},
			want: "tier_sweep: not launching on gt-ctl: cannot read the node's capacity: reading the node and pod listing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmds := &runnerFakeCmds{answer: tc.answer}
			d, spec, st, _, logs := preflightDaemon(t, atHour(time.Now(), 15), cmds)

			started := d.tierSweepLaunchRunner(context.Background(), st, spec)
			if !started.declined {
				t.Fatalf("launch = %+v, want it declined", started)
			}
			for _, ran := range cmds.ran {
				if argv := strings.Fields(ran); slices.Contains(argv, "apply") || slices.Contains(argv, "create") {
					t.Errorf("step %q ran; a node that cannot be read is never launched on", ran)
				}
			}
			if !strings.Contains(logs.String(), tc.want) {
				t.Errorf("the log does not hold %q:\n%s", tc.want, logs.String())
			}
		})
	}

	t.Run("a Kubernetes plan with no preflight is refused", func(t *testing.T) {
		t.Parallel()
		d, _, _, _, logs := preflightDaemon(t, atHour(time.Now(), 15), &runnerFakeCmds{})
		const line = "tier_sweep: not launching: no capacity preflight is wired for the kubernetes backend"

		if !d.tierSweepPreflightDeclines(context.Background(), config.TierSweepRunnerKubernetes, "gt-ctl", sweeprunner.Plan{}) {
			t.Fatal("a Kubernetes plan with no preflight was not refused")
		}
		if got := strings.Count(logs.String(), line); got != 1 {
			t.Fatalf("unwired-preflight lines = %d, want 1:\n%s", got, logs.String())
		}
		// Inside the hour the same broken plan is silent; past it the reason is
		// news again.
		d.tierSweepPreflightDeclines(context.Background(), config.TierSweepRunnerKubernetes, "gt-ctl", sweeprunner.Plan{})
		if got := strings.Count(logs.String(), line); got != 1 {
			t.Errorf("unwired-preflight lines = %d on the next retry, want still 1", got)
		}
		clk := d.clock.(*clockwork.FakeClock)
		clk.Advance(61 * time.Minute)
		d.tierSweepPreflightDeclines(context.Background(), config.TierSweepRunnerKubernetes, "gt-ctl", sweeprunner.Plan{})
		if got := strings.Count(logs.String(), line); got != 2 {
			t.Errorf("unwired-preflight lines = %d past the hour, want 2", got)
		}

		// A Docker plan's engine is the run's own, so it renders no preflight and
		// is not refused for it.
		if d.tierSweepPreflightDeclines(context.Background(), config.TierSweepRunnerDocker, "colima-gt-sbx", sweeprunner.Plan{}) {
			t.Error("a Docker plan with no preflight was refused")
		}
	})
}

// A declined launch is not a stage result: the stage reports that it did not
// run, so the cycle defers instead of recording a RED verdict for the tree.
func TestTierSweepPreflightDeclineDefersTheStage(t *testing.T) {
	t.Parallel()
	cmds := &runnerFakeCmds{answer: func(argv []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "nodes,pods") {
			return []byte(preflightTightListing), nil, nil
		}
		return nil, nil, nil
	}}
	d, _, st, _, _ := preflightDaemon(t, atHour(time.Now(), 15), cmds)

	res := d.tierSweepRunStage(context.Background(), t.TempDir(), st)
	if res.ran {
		t.Fatalf("stage result = %+v, want it not run: the cycle defers", res)
	}
	if res.err == nil {
		t.Error("the stage result names no reason for the deferral")
	}
}

// A preflight the renderer failed to wire is refused rather than skipped: a
// capacity check with no command or no reader has not passed.
func TestTierSweepPreflightRefusesAnUnwiredPlan(t *testing.T) {
	t.Parallel()
	d, _, _, _, _ := preflightDaemon(t, atHour(time.Now(), 15), &runnerFakeCmds{})
	for _, preflight := range []*sweeprunner.Preflight{
		{},
		{Step: sweeprunner.Step{Argv: []string{"kubectl"}}},
	} {
		report, err := d.tierSweepRunnerPreflight(context.Background(), preflight)
		if err == nil {
			t.Errorf("preflight %+v = %+v, want it refused", preflight, report)
		}
	}
}

// appliedJobDocument reports whether any recorded stdin holds a Job document.
func appliedJobDocument(stdins [][]byte) bool {
	for _, in := range stdins {
		var object struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(in, &object) == nil && object.Kind == "Job" {
			return true
		}
	}
	return false
}
