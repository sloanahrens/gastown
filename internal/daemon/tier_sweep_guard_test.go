package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// guardForbidden is what the priority guard's read printed on the live cluster
// on 2026-11-05, when the service account was forbidden to list pods
// cluster-wide (gt-gg9hv.56).
const guardForbidden = "Error from server (Forbidden): pods is forbidden: User \"system:serviceaccount:gastown:sweep\" cannot list resource \"pods\" in API group \"\" at the cluster scope\n"

// guardUnreadLine is the log line a failed read writes for guardForbidden, up to
// the sentence that says the cycle is deferred.
const guardUnreadLine = "tier_sweep: not launching: the guard's read failed (exit 1): Error from server (Forbidden): pods is forbidden"

// guardFixture is a Kubernetes-backed rig whose launch runs through the
// production launcher against a fake cluster. fail names the reads that exit
// 1 with the stderr given, keyed by a word of their argv; every other read
// answers the healthy live-cluster fixtures.
type guardFixture struct {
	d       *Daemon
	st      tierSweepStage
	spec    sweeprunner.Spec
	cluster *runnerCluster
	cmds    *runnerFakeCmds
	fail    map[string][]byte
	logs    *bytes.Buffer
	beads   *tierSweepFakeBeads
}

func newGuardFixture(t *testing.T, now time.Time) *guardFixture {
	t.Helper()
	cfg := runnerTestConfig(t)
	cfg.Backend = config.TierSweepRunnerKubernetes
	d, _, bd, logs := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = cfg
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA, budget: time.Minute}
	spec, ok, err := d.tierSweepRunnerSpec(st)
	if err != nil || !ok {
		t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
	}
	result := runnerTestResult(spec, now)
	g := &guardFixture{
		d:       d,
		st:      st,
		spec:    spec,
		cluster: &runnerCluster{t: t, status: "Complete", result: &result},
		logs:    logs,
		beads:   bd,
	}
	g.cmds = &runnerFakeCmds{answer: g.answer}
	d.execCmd = g.cmds.run
	return g
}

// answer fails the reads named in fail with exit 1 and answers the rest from
// the healthy cluster.
func (g *guardFixture) answer(argv []string) ([]byte, []byte, error) {
	for word, stderr := range g.fail {
		if slices.Contains(argv, word) {
			return nil, stderr, &cliExitError{code: 1}
		}
	}
	return g.cluster.answer(argv)
}

// cycle runs one launch of the stage, as a heartbeat's retry does.
func (g *guardFixture) cycle() tierSweepStageResult {
	return g.d.tierSweepRunRunner(context.Background(), g.st, g.spec)
}

// applied reports whether the launch created or applied any object.
func (g *guardFixture) applied() bool {
	for _, c := range g.cmds.ran {
		if strings.Contains(c, " apply ") || strings.Contains(c, " create ") {
			return true
		}
	}
	return false
}

// clock is the fake clock the daemon runs on.
func (g *guardFixture) clock(t *testing.T) *clockwork.FakeClock {
	t.Helper()
	clk, ok := g.d.clock.(*clockwork.FakeClock)
	if !ok {
		t.Fatalf("test clock = %T, want a fake clock to advance", g.d.clock)
	}
	return clk
}

// TestTierSweepGuardReadFailureDefersTheCycle: a guard read that exits 1 defers
// the stage with a typed read failure and makes no launch call, where it used to
// record the tiers RED with "runner: no result". The stage creates no bead; the
// cycle files beads only from a stage that ran.
func TestTierSweepGuardReadFailureDefersTheCycle(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	g.fail = map[string][]byte{"pods,priorityclasses": []byte(guardForbidden)}

	stage := g.cycle()
	if stage.ran {
		t.Fatalf("stage = %+v, want ran=false so the cycle defers rather than going RED", stage)
	}
	var unreadable *sweeprunner.GuardUnreadable
	if !errors.As(stage.err, &unreadable) {
		t.Fatalf("stage err = %v, want the guard's *GuardUnreadable", stage.err)
	}
	if unreadable.ExitCode != 1 {
		t.Errorf("exit code = %d, want the read's exit 1", unreadable.ExitCode)
	}
	if unreadable.Argv0Verb != "kubectl get pods,priorityclasses" {
		t.Errorf("read named %q, want the verb and resource", unreadable.Argv0Verb)
	}
	if g.applied() {
		t.Errorf("the launch applied objects after a failed read: %v", g.cmds.ran)
	}
	if _, err := os.Stat(filepath.Join(g.spec.OutDir, tierSweepRunnerRecordName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a launch record was written after a failed read: %v", err)
	}
	if n := len(g.beads.created); n != 0 {
		t.Errorf("%d beads filed for a deferred cycle, want none", n)
	}
}

// TestTierSweepGuardReadFailureLogsOnceAnHour: the deferral line is logged once
// while the read keeps failing, again after an hour, and a launch whose guards
// answer clears it so the next failure logs at once.
func TestTierSweepGuardReadFailureLogsOnceAnHour(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	g.fail = map[string][]byte{"pods,priorityclasses": []byte(guardForbidden)}
	clk := g.clock(t)
	count := func() int { return strings.Count(g.logs.String(), guardUnreadLine) }
	deferred := func() {
		t.Helper()
		if stage := g.cycle(); stage.ran || stage.err == nil {
			t.Fatalf("stage = %+v, want a deferral with a reason", stage)
		}
	}

	deferred()
	if n := count(); n != 1 {
		t.Fatalf("the deferral line appears %d times after the first cycle, want once:\n%s", n, g.logs.String())
	}
	if !strings.Contains(g.logs.String(), "; the cycle is deferred\n") {
		t.Errorf("the line does not end with the deferral sentence:\n%s", g.logs.String())
	}
	deferred()
	clk.Advance(30 * time.Minute)
	deferred()
	if n := count(); n != 1 {
		t.Fatalf("the deferral line appears %d times inside the hour, want it once:\n%s", n, g.logs.String())
	}
	clk.Advance(31 * time.Minute)
	deferred()
	if n := count(); n != 2 {
		t.Fatalf("the deferral line appears %d times after the hour, want it logged again:\n%s", n, g.logs.String())
	}

	// The guards answer: the launch goes through and the read stands no more.
	g.fail = nil
	clk.Advance(time.Minute)
	if stage := g.cycle(); !stage.ran {
		t.Fatalf("stage = %+v, want the launch once the guards answer", stage)
	}
	if g.d.tierSweepGuardReads.get("gastown") != nil {
		t.Error("the failed read still stands after a launch whose guards answered")
	}
	if err := os.Remove(filepath.Join(g.spec.OutDir, sweeprunner.ResultFileName)); err != nil {
		t.Fatalf("removing the launched run's result for the next cycle: %v", err)
	}

	// The read fails again inside the hour: the reason re-arose, so it logs at once.
	g.fail = map[string][]byte{"pods,priorityclasses": []byte(guardForbidden)}
	clk.Advance(time.Minute)
	deferred()
	if n := count(); n != 3 {
		t.Errorf("the deferral line appears %d times after the reason cleared and re-arose, want it logged at once:\n%s", n, g.logs.String())
	}
}

// TestTierSweepGuardReadFailureClearsThePriorityDeclineLog: a priority decline
// that stood before a guard read failed is no decline the failed read observed,
// so it does not survive it: the same decline re-arising inside the hour logs at
// once instead of being suppressed as the standing one.
func TestTierSweepGuardReadFailureClearsThePriorityDeclineLog(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	g.cluster.priority = []byte(runnerPriorityDeclineListing)
	const line = "tier_sweep: not launching: pod forgejo/forgejo-abc"
	count := func() int { return strings.Count(g.logs.String(), line) }

	// The priority guard refuses: its line stands, logged once.
	if stage := g.cycle(); stage.ran {
		t.Fatalf("stage = %+v, want the priority decline to defer the cycle", stage)
	}
	if n := count(); n != 1 {
		t.Fatalf("the decline line appears %d times after the first cycle, want once:\n%s", n, g.logs.String())
	}

	// The guard's read fails, still inside the hour: what stands is now the
	// failed read, not the decline it never re-observed.
	g.fail = map[string][]byte{"pods,priorityclasses": []byte(guardForbidden)}
	stage := g.cycle()
	if stage.ran {
		t.Fatalf("stage = %+v, want the failed read to defer the cycle", stage)
	}
	var unreadable *sweeprunner.GuardUnreadable
	if !errors.As(stage.err, &unreadable) {
		t.Fatalf("stage err = %v, want the guard's *GuardUnreadable", stage.err)
	}
	g.fail = nil

	// The same decline comes back inside the hour: the failed read cleared it,
	// so it is news again and logs at once.
	if stage := g.cycle(); stage.ran {
		t.Fatalf("stage = %+v, want the returning priority decline to defer the cycle", stage)
	}
	if n := count(); n != 2 {
		t.Errorf("the decline line appears %d times after a failed guard read cleared it, want it logged at once:\n%s", n, g.logs.String())
	}
}

// TestTierSweepGuardUnreadableAttentionItem: the guard collector, stored through
// the attention reconcile, raises one item keyed by the rig, naming the read and
// its first line; a later cycle that is still failing adds none, and a cycle
// whose guards read clears it.
func TestTierSweepGuardUnreadableAttentionItem(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	g := newGuardFixture(t, now)
	g.fail = map[string][]byte{"pods,priorityclasses": []byte(guardForbidden)}
	src := &attentionSources{d: g.d, tierSweepRigs: func() []string { return []string{attentionRig} }}
	const key = "tier-sweep-guard-unreadable:gastown"
	acks := attention.Acks{}
	// tick is one attention tick over the guard collector, the same collect
	// and store a tick runs for it. The other collectors read the town's
	// rigs and beads, which this test does not stand up.
	var state attention.State
	var raised int
	tick := func(at time.Time) {
		t.Helper()
		src.now = at
		observed := collectAttention(context.Background(), state, []attentionCollector{{kind: attention.KindTierSweepGuardUnreadable, collect: src.collectTierSweepGuardUnreadable}}, t.Logf)
		res := attention.Reconcile(state, observed, acks, at)
		state = res.State
		for _, e := range res.Events {
			if e.Key == key && e.State == attention.EventNew {
				raised++
			}
		}
	}

	g.cycle()
	tick(now)
	items := itemsWithKey(state, key)
	if len(items) != 1 {
		t.Fatalf("items keyed %s = %d, want exactly one: %+v", key, len(items), state.Items)
	}
	item := items[0]
	if item.Kind != attention.KindTierSweepGuardUnreadable || item.Severity != attention.SeverityLow {
		t.Errorf("item kind %s severity %s, want the low-severity guard kind", item.Kind, item.Severity)
	}
	for _, want := range []string{"kubectl get pods,priorityclasses", "exit 1", "Error from server (Forbidden)", "the cycle is deferred", "deploy/k8s/sweep-rbac.yaml"} {
		if !strings.Contains(item.Summary, want) {
			t.Errorf("summary %q does not contain %q", item.Summary, want)
		}
	}

	// Still failing: a second cycle keeps one item and raises nothing new.
	g.cycle()
	tick(now.Add(time.Minute))
	if n := len(itemsWithKey(state, key)); n != 1 {
		t.Fatalf("items keyed %s = %d after a second failing cycle, want still one", key, n)
	}
	if raised != 1 {
		t.Errorf("raised events for %s = %d, want one", key, raised)
	}

	// A tick that was canceled reads nothing and reports the cancellation.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.collectTierSweepGuardUnreadable(canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled collector returned %v, want the cancellation", err)
	}

	// A cycle whose guards read clears the item.
	g.fail = nil
	g.clock(t).Advance(time.Minute)
	if stage := g.cycle(); !stage.ran {
		t.Fatalf("stage = %+v, want the launch once the guards answer", stage)
	}
	tick(now.Add(2 * time.Minute))
	if n := len(itemsWithKey(state, key)); n != 0 {
		t.Errorf("the item survived a cycle whose guards read: %+v", state.Items)
	}
}

// TestTierSweepGuardFirstLineIsSanitized: a failed read's stderr of thousands
// of characters, with newlines and ANSI escapes, reaches the log line and the
// item as one line of at most 200 runes with no control character.
func TestTierSweepGuardFirstLineIsSanitized(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	g := newGuardFixture(t, now)
	var noisy strings.Builder
	noisy.WriteString("\x1b[31m")
	noisy.WriteString(strings.Repeat("Forbidden ", 500))
	noisy.WriteString("\x1b[0m\n")
	for noisy.Len() < 5000 {
		noisy.WriteString("\x1b[0m\tline\r\n")
	}
	g.fail = map[string][]byte{"pods,priorityclasses": []byte(noisy.String())}

	g.cycle()
	line := ""
	for _, l := range strings.Split(g.logs.String(), "\n") {
		if strings.HasPrefix(l, "tier_sweep: not launching: the guard's read failed") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no deferral line in the log:\n%s", g.logs.String())
	}
	const lead = "tier_sweep: not launching: the guard's read failed (exit 1): "
	const tail = "; the cycle is deferred"
	if !strings.HasPrefix(line, lead) || !strings.HasSuffix(line, tail) {
		t.Fatalf("the line %q does not carry the read's exit and the deferral sentence", line)
	}
	first := strings.TrimSuffix(strings.TrimPrefix(line, lead), tail)
	assertOneSanitizedLine(t, "log line's first line", first)

	f := newAttentionFixture(t, now)
	f.src.d = g.d
	f.src.tierSweepRigs = func() []string { return []string{attentionRig} }
	items := f.collect(t, f.src.collectTierSweepGuardUnreadable)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want the one guard item", items)
	}
	if !strings.Contains(items[0].Summary, first) {
		t.Errorf("the item summary does not carry the same sanitized first line %q: %q", first, items[0].Summary)
	}
}

// assertOneSanitizedLine fails unless s is at most 200 runes with no control
// character, which is what a log line and an item both have to hold.
func assertOneSanitizedLine(t *testing.T, what, s string) {
	t.Helper()
	if n := len([]rune(s)); n == 0 || n > 200 {
		t.Errorf("%s is %d runes, want 1 to 200: %q", what, n, s)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			t.Errorf("%s holds control character %U: %q", what, r, s)
			return
		}
	}
}

// TestTierSweepGuardRefusalStillRefuses: a guard whose read succeeds and whose
// refusal fires keeps the decline path, and a failed capacity preflight read
// still defers with no bead and no launch.
func TestTierSweepGuardRefusalStillRefuses(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	cases := []struct {
		name string
		fail map[string][]byte
		// listing answers the priority guard's read when set.
		listing []byte
		check   func(t *testing.T, g *guardFixture, stage tierSweepStageResult)
	}{
		{
			name:    "a refusing guard stays a decline",
			listing: []byte(runnerPriorityDeclineListing),
			check: func(t *testing.T, g *guardFixture, stage tierSweepStageResult) {
				var decline *sweeprunner.PriorityDecline
				var unreadable *sweeprunner.GuardUnreadable
				if !errors.As(stage.err, &decline) {
					t.Fatalf("stage err = %v, want the priority decline", stage.err)
				}
				if errors.As(stage.err, &unreadable) {
					t.Errorf("a refusal was reported as an unreadable read: %v", stage.err)
				}
				if g.d.tierSweepGuardReads.get("gastown") != nil {
					t.Error("a refused guard raised a failed-read item")
				}
			},
		},
		{
			name: "a failed capacity preflight read defers with no bead",
			fail: map[string][]byte{"nodes,pods": []byte(guardForbidden)},
			check: func(t *testing.T, g *guardFixture, stage tierSweepStageResult) {
				// The deferral is the preflight's decline, not the typed guard
				// read failure: the two defer the cycle for different reasons,
				// and only the guard's raises an attention item (gt-adkq39).
				const want = "the capacity preflight declined the launch"
				if stage.err == nil || stage.err.Error() != want {
					t.Errorf("stage err = %v, want %q", stage.err, want)
				}
				var unreadable *sweeprunner.GuardUnreadable
				if errors.As(stage.err, &unreadable) {
					t.Errorf("a failed preflight read was reported as a guard read failure: %v", stage.err)
				}
				var decline *sweeprunner.PriorityDecline
				if errors.As(stage.err, &decline) {
					t.Errorf("a failed preflight read was reported as a priority decline: %v", stage.err)
				}
				if g.applied() {
					t.Errorf("the launch applied objects after a failed preflight read: %v", g.cmds.ran)
				}
				if n := len(g.beads.created); n != 0 {
					t.Errorf("%d beads filed for a declined launch, want none", n)
				}
				if g.d.tierSweepGuardReads.get("gastown") != nil {
					t.Error("a preflight read raised a guard item; only a guard read does")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGuardFixture(t, now)
			g.fail = tc.fail
			if tc.listing != nil {
				g.cluster.priority = tc.listing
			}
			stage := g.cycle()
			if stage.ran {
				t.Fatalf("stage = %+v, want ran=false so the cycle defers", stage)
			}
			tc.check(t, g, stage)
		})
	}
}
