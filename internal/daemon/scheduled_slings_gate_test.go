package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// exitErr is a run error that carries an exit status, as *exec.ExitError does.
type exitErr int

func (e exitErr) Error() string { return "exit status" }
func (e exitErr) ExitCode() int { return int(e) }

var gatedEntry = func() ScheduledSlingEntry {
	e := docAuditEntry
	e.GateCommand = "test -n \"$(git log --since=1.week -1)\""
	return e
}()

var scheduledGateNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func TestScheduledGate_ExitZeroRuns(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: scheduledGateNow, gateAnswers: []gateAnswer{{}}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{gatedEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), gatedEntry, scheduledGateNow); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || len(f.slungIDs) != 1 {
		t.Errorf("exit 0 must dispatch as today: created %v slung %v", f.created, f.slungIDs)
	}
	if len(f.gateDeadlines) != 1 || f.gateDeadlines[0] > defaultScheduledGateTimeout || f.gateDeadlines[0] < defaultScheduledGateTimeout-5*time.Second {
		t.Errorf("gate context deadlines = %v, want about the %s default", f.gateDeadlines, defaultScheduledGateTimeout)
	}
}

func TestScheduledGate_ExitOneIsGated(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: scheduledGateNow, gateAnswers: []gateAnswer{{err: exitErr(1)}}}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{gatedEntry}, f)
	for i := 0; i < 5; i++ {
		if err := d.runScheduledSlingEntry(context.Background(), gatedEntry, scheduledGateNow); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.gateCalls) != 5 {
		t.Errorf("each tick must ask the gate again: %d calls, want 5", len(f.gateCalls))
	}
	if len(f.created) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("a gated run creates no bead and no sling: created %v slung %v", f.created, f.slungIDs)
	}
	if len(*esc) != 0 || len(d.scheduledGateFailures.failing()) != 0 {
		t.Errorf("exit 1 is an answer, not a failure: escalations %v failing %v", *esc, d.scheduledGateFailures.failing())
	}
}

func TestScheduledGate_FailureAndTimeoutSkipTheTick(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		answer gateAnswer
		want   string
	}{
		"exit 2":   {gateAnswer{out: "git: not a repository", err: exitErr(2)}, "exit status 2: git: not a repository"},
		"timeout":  {gateAnswer{out: "still scanning", timedOut: true}, "timed out after 1m0s: still scanning"},
		"no start": {gateAnswer{err: errors.New("fork/exec sh: no such file")}, "could not run: fork/exec sh: no such file"},
	}
	for name, c := range cases {
		f := &fakeScheduledRunner{createID: "gt-run1", now: scheduledGateNow, gateAnswers: []gateAnswer{c.answer}}
		d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{gatedEntry}, f)
		if err := d.runScheduledSlingEntry(context.Background(), gatedEntry, scheduledGateNow); err != nil {
			t.Fatalf("%s: a gate failure skips the tick, it is not a run failure: %v", name, err)
		}
		if len(f.created) != 0 || len(f.slungIDs) != 0 {
			t.Errorf("%s: no bead on a failed gate: created %v slung %v", name, f.created, f.slungIDs)
		}
		if got := d.scheduledGateFailures.m[gatedEntry.Name]; got.count != 1 || got.detail != c.want {
			t.Errorf("%s: recorded %+v, want count 1 and detail %q", name, got, c.want)
		}
		if len(*esc) != 0 {
			t.Errorf("%s: a gate failure files an attention item, not an escalation: %v", name, *esc)
		}
	}
}

func TestScheduledGate_OutputTailIsBounded(t *testing.T) {
	t.Parallel()
	out := strings.Repeat("x", 500) + "THE-END"
	got := classifyScheduledGate([]byte(out+"\n"), exitErr(3), nil, time.Minute)
	if got.verdict != scheduledGateFailed || !strings.HasSuffix(got.detail, "THE-END") || len(got.detail) > len("exit status 3: ")+scheduledGateOutputTail {
		t.Errorf("detail %q: want the last %d bytes of output", got.detail, scheduledGateOutputTail)
	}
}

func TestScheduledGate_ThirdFailureRaisesAttentionItemAndSuccessClearsIt(t *testing.T) {
	t.Parallel()
	fail := gateAnswer{err: exitErr(2), out: "boom"}
	f := &fakeScheduledRunner{createID: "gt-run1", now: scheduledGateNow, gateAnswers: []gateAnswer{fail, fail, fail, {}}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{gatedEntry}, f)
	s := &attentionSources{d: d, now: scheduledGateNow}
	items := func() []attention.Item {
		t.Helper()
		got, err := s.collectScheduledGate(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	for i := 1; i <= 2; i++ {
		d.runScheduledSlings()
		if got := items(); len(got) != 0 {
			t.Fatalf("after %d failures: items %+v, want none", i, got)
		}
	}
	d.runScheduledSlings()
	got := items()
	if len(got) != 1 {
		t.Fatalf("after 3 failures: items %+v, want one", got)
	}
	it := got[0]
	if it.Key != "scheduled-gate:doc-audit" || it.Kind != attention.KindScheduledGate || it.Severity != attention.SeverityLow || it.Rig != "gastown" {
		t.Errorf("item = %+v, want a low scheduled-gate:doc-audit item on gastown", it)
	}
	if !strings.Contains(it.Summary, "boom") {
		t.Errorf("summary %q lacks the gate's output", it.Summary)
	}
	if len(f.created) != 0 {
		t.Errorf("three failed gates created beads: %v", f.created)
	}

	d.runScheduledSlings() // the gate answers 0: the run happens and the item clears
	if got := items(); len(got) != 0 {
		t.Errorf("a success must clear the item, got %+v", got)
	}
	if len(f.created) != 1 {
		t.Errorf("the passing gate must dispatch: created %v", f.created)
	}
}

func TestScheduledGate_NoGateCommandNeverAsks(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: scheduledGateNow, gateAnswers: []gateAnswer{{err: exitErr(1)}}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, scheduledGateNow); err != nil {
		t.Fatal(err)
	}
	if len(f.gateCalls) != 0 || len(f.slungIDs) != 1 {
		t.Errorf("an entry without gate_command behaves as before: gate calls %v slung %v", f.gateCalls, f.slungIDs)
	}
}

func TestScheduledGate_SkippedRunsNeverAskTheGate(t *testing.T) {
	t.Parallel()
	open := []scheduledBead{{ID: "gt-old", Status: "open", CreatedAt: scheduledGateNow.Add(-time.Minute)}}
	f := &fakeScheduledRunner{beads: open, now: scheduledGateNow}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{gatedEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), gatedEntry, scheduledGateNow); err != nil {
		t.Fatal(err)
	}
	if len(f.gateCalls) != 0 {
		t.Errorf("the gate runs only when a run is due, asked %v", f.gateCalls)
	}
}

func TestScheduledGate_EntryRemovedFromConfigClearsItsItem(t *testing.T) {
	t.Parallel()
	fail := gateAnswer{err: exitErr(2)}
	f := &fakeScheduledRunner{now: scheduledGateNow, gateAnswers: []gateAnswer{fail}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{gatedEntry}, f)
	for i := 0; i < 3; i++ {
		d.runScheduledSlings()
	}
	if len(d.scheduledGateFailures.failing()) != 1 {
		t.Fatal("setup: the entry should be failing")
	}
	d.patrolConfig.Patrols.ScheduledSlings.Entries = nil
	d.runScheduledSlings()
	if got := d.scheduledGateFailures.failing(); len(got) != 0 {
		t.Errorf("a removed schedule keeps no item: %v", got)
	}
}

func TestScheduledSlingEntry_ValidateGateTimeout(t *testing.T) {
	t.Parallel()
	e := gatedEntry
	for _, ok := range []string{"", "30s", "2m"} {
		e.GateTimeoutStr = ok
		if err := scheduledSlingValidate(e); err != nil {
			t.Errorf("gate_timeout %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"soon", "0s", "-5s"} {
		e.GateTimeoutStr = bad
		if err := scheduledSlingValidate(e); err == nil {
			t.Errorf("gate_timeout %q accepted", bad)
		}
	}
	if got := scheduledSlingGateTimeout(gatedEntry); got != 60*time.Second {
		t.Errorf("default gate timeout = %v, want 60s", got)
	}
	e.GateTimeoutStr = "90s"
	if got := scheduledSlingGateTimeout(e); got != 90*time.Second {
		t.Errorf("gate timeout = %v, want 90s", got)
	}
}

// An Ensure failure is a gate failure: the verdict is failed with the error in
// its detail, and no command starts. A git exit status inside the error must not
// read as the gate's own exit 1 ("nothing to do").
func TestScheduledGate_EnsureFailureRecordsAGateFailureAndStartsNoCommand(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		ensureErr error
		want      string
	}{
		"no bare repo": {errors.New("rig gastown has no bare repo"), "could not run: build checkout of rig gastown: rig gastown has no bare repo"},
		"git exit 1":   {fmt.Errorf("git clone: %w", exitErr(1)), "could not run: build checkout of rig gastown: git clone: exit status"},
	}
	for name, c := range cases {
		commands := 0
		r := &execScheduledSlingRunner{
			townRoot: t.TempDir(),
			ensureBuildSrc: func(context.Context, string, string, string) (string, error) {
				return "", c.ensureErr
			},
			execCmd: func(*exec.Cmd) ([]byte, []byte, error) {
				commands++
				return nil, nil, nil
			},
		}
		d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{gatedEntry}, r)
		if d.scheduledGateAllows(context.Background(), r, gatedEntry) {
			t.Errorf("%s: a failed build checkout let the run go ahead", name)
		}
		if commands != 0 {
			t.Errorf("%s: %d commands started after an Ensure failure, want none", name, commands)
		}
		if got := d.scheduledGateFailures.m[gatedEntry.Name]; got.count != 1 || got.detail != c.want {
			t.Errorf("%s: recorded %+v, want count 1 and detail %q", name, got, c.want)
		}
	}
}

// A build checkout whose lock wait runs until the gate's deadline is a gate
// timeout: Ensure sees the deadline on its ctx, the gate fails with the timeout
// as its detail, and no command starts. The failsafe fires only if the gate's
// deadline never reaches Ensure, and fails the test there.
func TestScheduledGate_BuildCheckoutWaitPastTheGateDeadlineTimesOut(t *testing.T) {
	t.Parallel()
	e := gatedEntry
	e.GateTimeoutStr = "1ms"
	commands := 0
	r := &execScheduledSlingRunner{
		townRoot: t.TempDir(),
		ensureBuildSrc: func(ctx context.Context, _, _, _ string) (string, error) {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("waiting for the build checkout lock: %w", ctx.Err())
			case <-time.After(10 * time.Second): //testpolicy:allow no-sleep — a hang guard only: Ensure returns once the gate deadline ends, so this fires only when the deadline never reaches it
				t.Error("ensure was not given the gate deadline")
				return "", errors.New("ensure was not given the gate deadline")
			}
		},
		execCmd: func(*exec.Cmd) ([]byte, []byte, error) {
			commands++
			return nil, nil, nil
		},
	}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, r)
	if d.scheduledGateAllows(context.Background(), r, e) {
		t.Fatal("a build checkout that timed out let the run go ahead")
	}
	if commands != 0 {
		t.Errorf("%d commands started after the build checkout timed out, want none", commands)
	}
	const want = "timed out after 1ms"
	if got := d.scheduledGateFailures.m[e.Name]; got.count != 1 || got.detail != want {
		t.Errorf("recorded %+v, want count 1 and detail %q", got, want)
	}
}

// The real runner: sh -c in the build checkout buildsrc.Ensure returns, at
// main, with GT_TOWN_ROOT and GT_SCHEDULE_NAME set, through the execCmd seam.
func TestExecScheduledSlingRunner_RunGateDirAndEnv(t *testing.T) {
	t.Parallel()
	const build = "/town/.runtime/build-src/gastown"
	var got *exec.Cmd
	var ensuredRig, ensuredRev string
	r := &execScheduledSlingRunner{
		townRoot: "/town",
		ensureBuildSrc: func(_ context.Context, _, rig, rev string) (string, error) {
			ensuredRig, ensuredRev = rig, rev
			return build, nil
		},
		execCmd: func(cmd *exec.Cmd) ([]byte, []byte, error) {
			got = cmd
			return nil, nil, nil
		},
	}
	if _, err := r.runGate(context.Background(), gatedEntry); err != nil {
		t.Fatal(err)
	}
	if ensuredRig != "gastown" || ensuredRev != "" {
		t.Errorf("Ensure called for rig %q rev %q, want gastown at main", ensuredRig, ensuredRev)
	}
	if got.Dir != build {
		t.Errorf("dir = %q, want the build checkout %q", got.Dir, build)
	}
	if len(got.Args) != 3 || got.Args[1] != "-c" || got.Args[2] != gatedEntry.GateCommand {
		t.Errorf("argv = %v, want sh -c <gate_command>", got.Args)
	}
	env := strings.Join(got.Env, "\n")
	for _, want := range []string{"GT_TOWN_ROOT=/town", "GT_SCHEDULE_NAME=doc-audit"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}
