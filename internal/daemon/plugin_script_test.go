package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/plugin"
)

func scriptPlugin(t *testing.T, name, script string) *plugin.Plugin {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &plugin.Plugin{
		Name:         name,
		Path:         dir,
		HasRunScript: true,
		Execution:    &plugin.Execution{Type: plugin.ExecTypeScript, Timeout: "5s"},
	}
}

func TestRunsAsScript(t *testing.T) {
	p := scriptPlugin(t, "s", "exit 0\n")
	if !runsAsScript(p) {
		t.Error("script plugin with run.sh must run as script")
	}
	p.HasRunScript = false
	if runsAsScript(p) {
		t.Error("script type without run.sh must stay on the dog path")
	}
	p.HasRunScript = true
	p.Execution.Type = plugin.ExecTypeAgent
	if runsAsScript(p) {
		t.Error("agent type must stay on the dog path")
	}
	p.Execution = nil
	if runsAsScript(p) {
		t.Error("no [execution] block defaults to the dog path")
	}
	if runsAsScript(nil) {
		t.Error("nil plugin")
	}
}

func TestScriptTimeout(t *testing.T) {
	p := scriptPlugin(t, "s", "exit 0\n")
	if got := scriptTimeout(p); got != 5*time.Second {
		t.Errorf("timeout = %s, want 5s", got)
	}
	p.Execution.Timeout = "not-a-duration"
	if got := scriptTimeout(p); got != defaultScriptTimeout {
		t.Errorf("unparsable timeout -> %s, want default %s", got, defaultScriptTimeout)
	}
	p.Execution.Timeout = ""
	if got := scriptTimeout(p); got != defaultScriptTimeout {
		t.Errorf("empty timeout -> %s, want default", got)
	}
}

func TestRunPluginScript_SuccessCapturesOutputAndEnv(t *testing.T) {
	p := scriptPlugin(t, "ok", "echo hello; echo root=$GT_TOWN_ROOT name=$GT_PLUGIN_NAME runner=$GT_PLUGIN_RUNNER role=$GT_ROLE >&2; pwd\n")
	res := runPluginScript(context.Background(), p, "/town", 5*time.Second)
	if !res.ok() {
		t.Fatalf("expected success, got %+v", res)
	}
	for _, want := range []string{"hello", "root=/town", "name=ok", "runner=daemon", "role=daemon/plugin", p.Path} {
		if !strings.Contains(res.output, want) {
			t.Errorf("output lacks %q:\n%s", want, res.output)
		}
	}
	if !strings.HasPrefix(res.status(), "exit 0") {
		t.Errorf("status = %q", res.status())
	}
}

func TestRunPluginScript_FailureExitCode(t *testing.T) {
	p := scriptPlugin(t, "bad", "echo boom >&2; exit 3\n")
	res := runPluginScript(context.Background(), p, "/town", 5*time.Second)
	if res.ok() || res.exitCode != 3 || res.timedOut {
		t.Fatalf("expected exit 3, got %+v", res)
	}
	if !strings.Contains(res.output, "boom") || !strings.HasPrefix(res.status(), "exit 3") {
		t.Errorf("output/status: %q / %q", res.output, res.status())
	}
}

// A timeout must kill the whole process tree, not just bash: the child the
// script backgrounds has to be gone too (gt-6t43 is the orphan shape).
func TestRunPluginScript_TimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	p := scriptPlugin(t, "slow", "sleep 60 &\necho $! > "+pidFile+"\nwait\n")
	start := time.Now()
	res := runPluginScript(context.Background(), p, "/town", 500*time.Millisecond)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout did not bound the run: %s", time.Since(start))
	}
	if !res.timedOut || res.ok() {
		t.Fatalf("expected timeout, got %+v", res)
	}
	if !strings.HasPrefix(res.status(), "timed out") {
		t.Errorf("status = %q", res.status())
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid not recorded: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("backgrounded child %d survived the timeout: process group was not killed", pid)
}

func TestTail(t *testing.T) {
	s := strings.Repeat("line\n", 100)
	got := tail(s, 30)
	if !strings.HasPrefix(got, "[... ") || !strings.HasSuffix(got, "line\n") || len(got) > 80 {
		t.Errorf("tail = %q", got)
	}
	if tail("short", 30) != "short" {
		t.Error("short input must be returned whole")
	}
}

// completeScriptRun: success records success and does not call the dog;
// failure records failure AND dispatches; a record error never suppresses
// the dispatch.
func TestCompleteScriptRun(t *testing.T) {
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p"}
	var recs []plugin.PluginRunRecord
	dispatched := 0
	hooks := scriptRunHooks{
		record:    func(r plugin.PluginRunRecord) error { recs = append(recs, r); return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { dispatched++ },
		logf:      func(string, ...any) {},
	}
	completeScriptRun(p, scriptResult{exitCode: 0, output: "fine"}, hooks)
	if len(recs) != 1 || recs[0].Result != plugin.ResultSuccess || dispatched != 0 {
		t.Fatalf("success: recs=%+v dispatched=%d", recs, dispatched)
	}
	if recs[0].RigName != "gastown" || !strings.Contains(recs[0].Title, "(script)") || !strings.Contains(recs[0].Body, "fine") {
		t.Errorf("record fields: %+v", recs[0])
	}
	completeScriptRun(p, scriptResult{exitCode: 2, output: "boom"}, hooks)
	if len(recs) != 2 || recs[1].Result != plugin.ResultFailure || dispatched != 1 {
		t.Fatalf("failure: recs=%d last=%+v dispatched=%d", len(recs), recs[len(recs)-1], dispatched)
	}
	hooks.record = func(plugin.PluginRunRecord) error { return errors.New("bd down") }
	completeScriptRun(p, scriptResult{timedOut: true, exitCode: -1}, hooks)
	if dispatched != 2 {
		t.Error("a failed record must not suppress the failure dispatch")
	}
}

// A deferral (exit 3) writes no record and touches no dog. The record is what
// satisfies a cooldown gate, so writing one for a run that accomplished
// nothing is what starves a plugin whose window opens and closes on its own
// (gt-oqbw) — and a deferral is not a failure, so there is nothing for a dog
// to do either.
func TestCompleteScriptRun_DeferralWritesNothing(t *testing.T) {
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p"}
	recs := 0
	dispatched := 0
	var logs []string
	hooks := scriptRunHooks{
		record:    func(plugin.PluginRunRecord) error { recs++; return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { dispatched++ },
		logf:      func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	completeScriptRun(p, scriptResult{exitCode: scriptExitDeferred, output: "gate busy"}, hooks)
	if recs != 0 {
		t.Errorf("a deferral must write no run record (wrote %d)", recs)
	}
	if dispatched != 0 {
		t.Error("a deferral must not hand off to a dog")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "deferred") || !strings.Contains(logs[0], "next heartbeat") {
		t.Errorf("deferral must be logged as a retry: %v", logs)
	}

	// The exit code only means deferral on its own: the same code with a
	// timeout, or with the process never starting, is an ordinary failure.
	for _, res := range []scriptResult{
		{exitCode: scriptExitDeferred, timedOut: true},
		{exitCode: scriptExitDeferred, err: errors.New("no such file")},
	} {
		completeScriptRun(p, res, hooks)
	}
	if recs != 2 || dispatched != 2 {
		t.Errorf("timed-out/never-started runs must be recorded as failures: recs=%d dispatched=%d", recs, dispatched)
	}
}

// Fakes for the dispatch seam.
type fakeMgr struct{ assigned, cleared []string }

func (m *fakeMgr) AssignWork(name, _ string) error { m.assigned = append(m.assigned, name); return nil }
func (m *fakeMgr) ClearWork(name string) error     { m.cleared = append(m.cleared, name); return nil }

type fakeSM struct{ started []string }

func (s *fakeSM) Start(name string, _ dog.SessionStartOptions) error {
	s.started = append(s.started, name)
	return nil
}

type fakeRouter struct{ sent []*mail.Message }

func (r *fakeRouter) Send(m *mail.Message) error { r.sent = append(r.sent, m); return nil }

type fakeRecorder struct{ recs []plugin.PluginRunRecord }

func (r *fakeRecorder) RecordRun(rec plugin.PluginRunRecord) (string, error) {
	r.recs = append(r.recs, rec)
	return "hq-run", nil
}

// End to end through the daemon: a failing script plugin is recorded as a
// failure and handed to a dog whose mail carries the exit status and output
// under the "Direct run failed" heading; a succeeding one touches no dog.
func TestStartScriptPlugin_FailureHandsOffToDog(t *testing.T) {
	mgr, sm, router, rec := &fakeMgr{}, &fakeSM{}, &fakeRouter{}, &fakeRecorder{}
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0), ctx: context.Background()}
	d.findDogFn = func() *dog.Dog { return &dog.Dog{Name: "alpha"} }

	bad := scriptPlugin(t, "failing", "echo the disk is full >&2; exit 7\n")
	d.startScriptPlugin(bad, mgr, sm, router, rec)
	waitFor(t, func() bool { return len(rec.recs) == 1 && len(sm.started) == 1 })
	if rec.recs[0].Result != plugin.ResultFailure {
		t.Errorf("record = %+v", rec.recs[0])
	}
	if len(router.sent) != 1 || !strings.Contains(router.sent[0].Body, "Direct run failed") ||
		!strings.Contains(router.sent[0].Body, "exit 7") || !strings.Contains(router.sent[0].Body, "the disk is full") {
		t.Errorf("failure mail: %+v", router.sent)
	}
	if mgr.assigned[0] != "alpha" || sm.started[0] != "alpha" {
		t.Errorf("dog wiring: assigned=%v started=%v", mgr.assigned, sm.started)
	}
	if _, err := os.Stat(scriptLogPath(d.config.TownRoot, "failing")); err != nil {
		t.Errorf("run log not written: %v", err)
	}

	good := scriptPlugin(t, "passing", "exit 0\n")
	d.startScriptPlugin(good, mgr, sm, router, rec)
	waitFor(t, func() bool { return len(rec.recs) == 2 })
	if rec.recs[1].Result != plugin.ResultSuccess || len(sm.started) != 1 || len(router.sent) != 1 {
		t.Errorf("success must not touch a dog: recs=%+v started=%v sent=%d", rec.recs, sm.started, len(router.sent))
	}

	// A second start while the first still runs is a no-op.
	slow := scriptPlugin(t, "slow", "sleep 2\n")
	d.startScriptPlugin(slow, mgr, sm, router, rec)
	d.startScriptPlugin(slow, mgr, sm, router, rec)
	waitFor(t, func() bool { return len(rec.recs) == 3 })
	time.Sleep(200 * time.Millisecond)
	if len(rec.recs) != 3 {
		t.Errorf("in-flight guard failed: %d records for two overlapping starts", len(rec.recs))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
