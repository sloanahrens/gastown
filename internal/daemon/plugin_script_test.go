package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/tmux"
)

func scriptPlugin(t *testing.T, name, script string) *plugin.Plugin {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o644); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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

// scriptBash is a fakeCLI for `bash run.sh` that answers every run with reply.
func scriptBash(reply cliReply) *fakeCLI {
	return newFakeCLI(func([]string) cliReply { return reply })
}

func TestRunPluginScript_SuccessCapturesOutputAndEnv(t *testing.T) {
	t.Parallel()
	p := scriptPlugin(t, "ok", "exit 0\n")
	bash := scriptBash(cliReply{stdout: "hello\n"})
	res := runPluginScript(context.Background(), bash.run, scriptEnv{environ: []string{"PATH=/usr/bin"}}, p, "/town", 5*time.Second)
	if !res.ok() {
		t.Fatalf("expected success, got %+v", res)
	}
	if !strings.Contains(res.output, "hello") {
		t.Errorf("output lacks the script's stdout:\n%s", res.output)
	}
	if !strings.HasPrefix(res.status(), "exit 0") {
		t.Errorf("status = %q", res.status())
	}
	calls := bash.recorded()
	if len(calls) != 1 || calls[0].name != "bash" || !slices.Equal(calls[0].args, []string{"run.sh"}) || calls[0].dir != p.Path {
		t.Fatalf("ran %+v, want `bash run.sh` in the plugin directory", calls)
	}
	for key, want := range map[string]string{
		"PATH": "/usr/bin", "GT_ROOT": "/town", "GT_TOWN_ROOT": "/town", "GT_PLUGIN_NAME": "ok",
		"GT_PLUGIN_RUNNER": "daemon", "GT_ROLE": "daemon/plugin", "BD_ACTOR": "daemon",
	} {
		if got := calls[0].getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// A script runs outside any tmux session, so a bare `tmux` in it asks the
// default server. The daemon hands over the town socket it resolved at start;
// without it stuck-agent-dog reads a live Deacon as crashed (claude-l5w).
func TestRunPluginScript_ExportsTownTmuxSocket(t *testing.T) {
	t.Parallel()
	ambient := []string{"GT_TMUX_SOCKET=stale-ambient"}
	p := scriptPlugin(t, "sock", "exit 0\n")

	bash := scriptBash(cliReply{})
	runPluginScript(context.Background(), bash.run, scriptEnv{environ: ambient, tmuxSocket: "gt-town"}, p, "/town", 5*time.Second)
	if got := bash.recorded()[0].getenv("GT_TMUX_SOCKET"); got != "gt-town" {
		t.Errorf("GT_TMUX_SOCKET = %q, want the town socket", got)
	}

	bash = scriptBash(cliReply{})
	runPluginScript(context.Background(), bash.run, scriptEnv{environ: ambient}, p, "/town", 5*time.Second)
	if got := bash.recorded()[0].getenv("GT_TMUX_SOCKET"); got != "stale-ambient" {
		t.Errorf("no resolved socket should leave the ambient value alone, got %q", got)
	}
}

// daemonScriptEnv is the wiring for the pair above: the socket handed to a
// script is the one the tmux package resolved for this process.
func TestDaemonScriptEnvCarriesTheResolvedSocket(t *testing.T) {
	t.Parallel()
	if got, want := daemonScriptEnv().tmuxSocket, tmux.GetDefaultSocket(); got != want {
		t.Errorf("daemonScriptEnv().tmuxSocket = %q, want tmux.GetDefaultSocket() %q", got, want)
	}
}

// A script that ran out its timeout reports as timed out, not by its exit.
func TestRunPluginScript_TimeoutIsReportedAsTimeout(t *testing.T) {
	t.Parallel()
	p := scriptPlugin(t, "slow", "exit 0\n")
	// A timeout already spent: the run's context is past its deadline when
	// the script returns, as it is when a kill ends a script at its timeout.
	killed := func(*exec.Cmd) ([]byte, []byte, error) { return nil, nil, errors.New("signal: killed") }
	res := runPluginScript(context.Background(), killed, scriptEnv{}, p, "/town", -time.Second)
	if !res.timedOut || res.ok() || res.exitCode != -1 {
		t.Fatalf("expected a timeout, got %+v", res)
	}
	if !strings.HasPrefix(res.status(), "timed out") {
		t.Errorf("status = %q", res.status())
	}
}

func TestRunPluginScript_FailureExitCode(t *testing.T) {
	t.Parallel()
	p := scriptPlugin(t, "bad", "exit 3\n")
	bash := scriptBash(cliReply{stderr: "boom\n", code: 3})
	res := runPluginScript(context.Background(), bash.run, scriptEnv{}, p, "/town", 5*time.Second)
	if res.ok() || res.exitCode != 3 || res.timedOut {
		t.Fatalf("expected exit 3, got %+v", res)
	}
	if !strings.Contains(res.output, "boom") || !strings.HasPrefix(res.status(), "exit 3") {
		t.Errorf("output/status: %q / %q", res.output, res.status())
	}
}

// gatedDeadline is a context whose deadline passes when the test says so:
// Done closes and Err reports context.DeadlineExceeded, which is what a
// context derived from it (runPluginScript's own timeout) reports too.
type gatedDeadline struct {
	context.Context
	once     sync.Once
	done     chan struct{}
	deadline time.Time // what Deadline reports; zero reports none
}

func (g *gatedDeadline) Deadline() (time.Time, bool) { return g.deadline, !g.deadline.IsZero() }

func newGatedDeadline() *gatedDeadline {
	return &gatedDeadline{Context: context.Background(), done: make(chan struct{})}
}

func (g *gatedDeadline) expire()               { g.once.Do(func() { close(g.done) }) }
func (g *gatedDeadline) Done() <-chan struct{} { return g.done }
func (g *gatedDeadline) Err() error {
	select {
	case <-g.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestTail(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// A run that exits 0 but prints the skip marker records a skipped receipt:
// the plugin ran, found nothing to do, and the history should say so instead
// of a green check (gt-chqi is the bug class where these no-ops serialized
// as success). Skipped is not a failure: no dog is dispatched, but a record
// IS written — that is what satisfies the cooldown gate.
func TestCompleteScriptRun_SkippedRecordsSkippedNoDog(t *testing.T) {
	t.Parallel()
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p"}
	var recs []plugin.PluginRunRecord
	dispatched := 0
	hooks := scriptRunHooks{
		record:    func(r plugin.PluginRunRecord) error { recs = append(recs, r); return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { dispatched++ },
		logf:      func(string, ...any) {},
	}
	completeScriptRun(p, scriptResult{exitCode: 0, output: "nothing to do\n[plugin-result skipped]\n"}, hooks)
	if len(recs) != 1 || recs[0].Result != plugin.ResultSkipped || dispatched != 0 {
		t.Fatalf("skipped: recs=%+v dispatched=%d", recs, dispatched)
	}
	// A plain exit 0 with no marker is a success: the marker is the only
	// thing that makes the receipt say "did nothing".
	completeScriptRun(p, scriptResult{exitCode: 0, output: "did the work"}, hooks)
	if len(recs) != 2 || recs[1].Result != plugin.ResultSuccess || dispatched != 0 {
		t.Fatalf("unmarked exit 0 must be success: recs=%+v dispatched=%d", recs, dispatched)
	}
	// A marker on a FAILED run says nothing — exit code 2 is a failure
	// either way, and the marker must not demote it.
	completeScriptRun(p, scriptResult{exitCode: 2, output: "boom [plugin-result skipped]"}, hooks)
	if len(recs) != 3 || recs[2].Result != plugin.ResultFailure || dispatched != 1 {
		t.Fatalf("marker on a failed run must still be a failure: recs=%+v dispatched=%d", recs, dispatched)
	}
}

// A deferral (exit 3) writes no record and touches no dog. The record is what
// satisfies a cooldown gate, so writing one for a run that accomplished
// nothing is what starves a plugin whose window opens and closes on its own
// (gt-oqbw) — and a deferral is not a failure, so there is nothing for a dog
// to do either.
func TestCompleteScriptRun_DeferralWritesNothing(t *testing.T) {
	t.Parallel()
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p", Execution: &plugin.Execution{AllowDeferredExit: true}}
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

// Exit 3 only means deferral for a plugin whose plugin.md opts in with
// [execution] allow_deferred_exit = true. Without the opt-in — the default,
// and every script plugin except rebuild-gt at the time of writing — exit 3
// is an ordinary failure: recorded and dispatched to a dog like any other
// nonzero exit. This is what keeps one plugin's private exit-code contract
// from silently swallowing a real failure in an unrelated plugin that
// happens to exit 3 (gt-oqbw).
func TestCompleteScriptRun_DeferralRequiresOptIn(t *testing.T) {
	t.Parallel()
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p"}
	var recs []plugin.PluginRunRecord
	dispatched := 0
	hooks := scriptRunHooks{
		record:    func(r plugin.PluginRunRecord) error { recs = append(recs, r); return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { dispatched++ },
		logf:      func(string, ...any) {},
	}
	completeScriptRun(p, scriptResult{exitCode: scriptExitDeferred, output: "boom"}, hooks)
	if len(recs) != 1 || recs[0].Result != plugin.ResultFailure || dispatched != 1 {
		t.Fatalf("exit 3 without opt-in must be an ordinary failure: recs=%+v dispatched=%d", recs, dispatched)
	}

	p.Execution = &plugin.Execution{AllowDeferredExit: false}
	completeScriptRun(p, scriptResult{exitCode: scriptExitDeferred, output: "boom"}, hooks)
	if len(recs) != 2 || recs[1].Result != plugin.ResultFailure || dispatched != 2 {
		t.Fatalf("exit 3 with allow_deferred_exit=false must still be an ordinary failure: recs=%+v dispatched=%d", recs, dispatched)
	}
}

// Fakes for the dispatch seam. Each guards its record with a mutex: the
// daemon calls them from the script's goroutine while the test reads them.
type fakeMgr struct {
	mu                sync.Mutex
	assigned, cleared []string
}

func (m *fakeMgr) AssignWork(name, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assigned = append(m.assigned, name)
	return nil
}

func (m *fakeMgr) ClearWork(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleared = append(m.cleared, name)
	return nil
}

func (m *fakeMgr) assignedNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.assigned...)
}

type fakeSM struct {
	mu      sync.Mutex
	started []string
}

func (s *fakeSM) Start(name string, _ dog.SessionStartOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, name)
	return nil
}

func (s *fakeSM) startedNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.started...)
}

type fakeRouter struct {
	mu   sync.Mutex
	sent []*mail.Message
}

func (r *fakeRouter) Send(m *mail.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, m)
	return nil
}

func (r *fakeRouter) sentMessages() []*mail.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*mail.Message(nil), r.sent...)
}

type fakeRecorder struct {
	mu   sync.Mutex
	recs []plugin.PluginRunRecord
}

func (r *fakeRecorder) RecordRun(rec plugin.PluginRunRecord) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return "hq-run", nil
}

func (r *fakeRecorder) records() []plugin.PluginRunRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]plugin.PluginRunRecord(nil), r.recs...)
}

// End to end through the daemon: a failing script plugin is recorded as a
// failure and handed to a dog whose mail carries the exit status and output
// under the "Direct run failed" heading; a succeeding one touches no dog.
func TestStartScriptPlugin_FailureHandsOffToDog(t *testing.T) {
	t.Parallel()
	mgr, sm, router, rec := &fakeMgr{}, &fakeSM{}, &fakeRouter{}, &fakeRecorder{}
	logs := &lockedBuffer{}
	release := make(chan struct{})
	bash := newFakeCLIFor(func(c cliCall) cliReply {
		switch filepath.Base(c.dir) {
		case "failing":
			return cliReply{stderr: "the disk is full\n", code: 7}
		case "slow":
			<-release
		}
		return cliReply{}
	})
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(logs, "", 0), ctx: context.Background(), execCmd: bash.run}
	d.findDogFn = func() *dog.Dog { return &dog.Dog{Name: "alpha"} }

	bad := scriptPlugin(t, "failing", "exit 7\n")
	d.startScriptPlugin(bad, mgr, sm, router, rec)
	waitFor(t, func() bool { return len(rec.records()) == 1 && len(sm.startedNames()) == 1 })
	if recs := rec.records(); recs[0].Result != plugin.ResultFailure {
		t.Errorf("record = %+v", recs[0])
	}
	sent := router.sentMessages()
	if len(sent) != 1 || !strings.Contains(sent[0].Body, "Direct run failed") ||
		!strings.Contains(sent[0].Body, "exit 7") || !strings.Contains(sent[0].Body, "the disk is full") {
		t.Errorf("failure mail: %+v", sent)
	}
	if assigned, started := mgr.assignedNames(), sm.startedNames(); assigned[0] != "alpha" || started[0] != "alpha" {
		t.Errorf("dog wiring: assigned=%v started=%v", assigned, started)
	}
	if _, err := os.Stat(scriptLogPath(d.config.TownRoot, "failing")); err != nil {
		t.Errorf("run log not written: %v", err)
	}

	good := scriptPlugin(t, "passing", "exit 0\n")
	d.startScriptPlugin(good, mgr, sm, router, rec)
	waitFor(t, func() bool { return len(rec.records()) == 2 })
	if recs := rec.records(); recs[1].Result != plugin.ResultSuccess || len(sm.startedNames()) != 1 || len(router.sentMessages()) != 1 {
		t.Errorf("success must not touch a dog: recs=%+v started=%v sent=%d", recs, sm.startedNames(), len(router.sentMessages()))
	}

	// A second start while the first still runs is a no-op. The script holds
	// until the test releases it, so the second start certainly overlaps.
	slow := scriptPlugin(t, "slow", "exit 0\n")
	d.startScriptPlugin(slow, mgr, sm, router, rec)
	d.startScriptPlugin(slow, mgr, sm, router, rec)
	if !strings.Contains(logs.String(), "script plugin slow still running, skipping") {
		t.Errorf("the overlapping start was not refused:\n%s", logs.String())
	}
	close(release)
	waitFor(t, func() bool { return d.scripts.runningCount() == 0 })
	if n := len(rec.records()); n != 3 {
		t.Errorf("in-flight guard failed: %d records for two overlapping starts, want 3", n)
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
