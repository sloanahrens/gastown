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

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
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
		t.Error("script type without run.sh must not run as a script")
	}
	p.HasRunScript = true
	p.Execution.Type = plugin.ExecTypeAgent
	if runsAsScript(p) {
		t.Error("agent type must not run as a script")
	}
	p.Execution = nil
	if runsAsScript(p) {
		t.Error("no [execution] block must not run as a script")
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
// without it a plugin that asks tmux about an agent reads the wrong server
// (claude-l5w).
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

// A run the daemon's own shutdown canceled is aborted, not failed: the
// process-group kill at shutdown ends it with no exit code of its own, and
// reading that as the script's verdict recorded a failure and escalated it
// every time the daemon stopped mid-run (gt-7uyfc).
func TestRunPluginScript_DaemonShutdownAbortsTheRun(t *testing.T) {
	t.Parallel()
	p := scriptPlugin(t, "x", "exit 0\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the daemon stopped
	killed := func(*exec.Cmd) ([]byte, []byte, error) { return nil, nil, errors.New("signal: killed") }
	res := runPluginScript(ctx, killed, scriptEnv{}, p, "/town", 5*time.Second)
	if !res.aborted || res.ok() {
		t.Fatalf("expected an aborted run, got %+v", res)
	}
	if !strings.HasPrefix(res.status(), "aborted") {
		t.Errorf("status = %q, want an aborted run", res.status())
	}
}

// The abort verdict is the daemon's cancel alone. The runPluginScript timeout
// above still reports a timeout, and a script that finished on its own exit
// code keeps it however the run ended — the cancel landing in the same
// instant does not rewrite a real result (gt-7uyfc).
func TestRunPluginScript_AbortOnlyForTheDaemonsOwnCancel(t *testing.T) {
	t.Parallel()
	p := scriptPlugin(t, "x", "exit 0\n")

	// Past its own deadline: a timeout, as before.
	killed := func(*exec.Cmd) ([]byte, []byte, error) { return nil, nil, errors.New("signal: killed") }
	if res := runPluginScript(context.Background(), killed, scriptEnv{}, p, "/town", -time.Second); res.aborted || !res.timedOut {
		t.Fatalf("a spent deadline must stay a timeout, got %+v", res)
	}

	// Canceled while the script was finishing on its own: the result stands.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bash := scriptBash(cliReply{stdout: "did the work\n"})
	if res := runPluginScript(ctx, bash.run, scriptEnv{}, p, "/town", 5*time.Second); res.aborted || !res.ok() {
		t.Fatalf("a clean exit keeps its result, got %+v", res)
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

// completeScriptRun: success records success and reports the good run;
// failure records failure AND escalates; a record error never suppresses
// the escalation.
func TestCompleteScriptRun(t *testing.T) {
	t.Parallel()
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p"}
	var recs []plugin.PluginRunRecord
	escalated, good := 0, 0
	hooks := scriptRunHooks{
		record:    func(r plugin.PluginRunRecord) error { recs = append(recs, r); return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { escalated++ },
		onSuccess: func(*plugin.Plugin) { good++ },
		logf:      func(string, ...any) {},
	}
	completeScriptRun(p, scriptResult{exitCode: 0, output: "fine"}, hooks)
	if len(recs) != 1 || recs[0].Result != plugin.ResultSuccess || escalated != 0 || good != 1 {
		t.Fatalf("success: recs=%+v escalated=%d good=%d", recs, escalated, good)
	}
	if recs[0].RigName != "gastown" || !strings.Contains(recs[0].Title, "(script)") || !strings.Contains(recs[0].Body, "fine") {
		t.Errorf("record fields: %+v", recs[0])
	}
	completeScriptRun(p, scriptResult{exitCode: 2, output: "boom"}, hooks)
	if len(recs) != 2 || recs[1].Result != plugin.ResultFailure || escalated != 1 {
		t.Fatalf("failure: recs=%d last=%+v escalated=%d", len(recs), recs[len(recs)-1], escalated)
	}
	hooks.record = func(plugin.PluginRunRecord) error { return errors.New("bd down") }
	completeScriptRun(p, scriptResult{timedOut: true, exitCode: -1}, hooks)
	if escalated != 2 {
		t.Error("a failed record must not suppress the failure escalation")
	}
	if good != 1 {
		t.Errorf("failures must not report a good run: good=%d", good)
	}
}

// A run that exits 0 but prints the skip marker records a skipped receipt:
// the plugin ran, found nothing to do, and the history should say so instead
// of a green check (gt-chqi is the bug class where these no-ops serialized
// as success). Skipped is not a failure: nothing is escalated, but a record
// IS written — that is what satisfies the cooldown gate.
func TestCompleteScriptRun_SkippedRecordsSkippedNoEscalation(t *testing.T) {
	t.Parallel()
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p"}
	var recs []plugin.PluginRunRecord
	escalated := 0
	hooks := scriptRunHooks{
		record:    func(r plugin.PluginRunRecord) error { recs = append(recs, r); return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { escalated++ },
		logf:      func(string, ...any) {},
	}
	completeScriptRun(p, scriptResult{exitCode: 0, output: "nothing to do\n[plugin-result skipped]\n"}, hooks)
	if len(recs) != 1 || recs[0].Result != plugin.ResultSkipped || escalated != 0 {
		t.Fatalf("skipped: recs=%+v escalated=%d", recs, escalated)
	}
	// A plain exit 0 with no marker is a success: the marker is the only
	// thing that makes the receipt say "did nothing".
	completeScriptRun(p, scriptResult{exitCode: 0, output: "did the work"}, hooks)
	if len(recs) != 2 || recs[1].Result != plugin.ResultSuccess || escalated != 0 {
		t.Fatalf("unmarked exit 0 must be success: recs=%+v escalated=%d", recs, escalated)
	}
	// A marker on a FAILED run says nothing — exit code 2 is a failure
	// either way, and the marker must not demote it.
	completeScriptRun(p, scriptResult{exitCode: 2, output: "boom [plugin-result skipped]"}, hooks)
	if len(recs) != 3 || recs[2].Result != plugin.ResultFailure || escalated != 1 {
		t.Fatalf("marker on a failed run must still be a failure: recs=%+v escalated=%d", recs, escalated)
	}
}

// A deferral (exit 3) writes no record and escalates nothing. The record is
// what satisfies a cooldown gate, so writing one for a run that accomplished
// nothing is what starves a plugin whose window opens and closes on its own
// (gt-oqbw) — and a deferral is not a failure, so there is nothing to
// escalate either.
func TestCompleteScriptRun_DeferralWritesNothing(t *testing.T) {
	t.Parallel()
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p", Execution: &plugin.Execution{AllowDeferredExit: true}}
	recs := 0
	escalated := 0
	var logs []string
	hooks := scriptRunHooks{
		record:    func(plugin.PluginRunRecord) error { recs++; return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { escalated++ },
		logf:      func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	completeScriptRun(p, scriptResult{exitCode: scriptExitDeferred, output: "gate busy"}, hooks)
	if recs != 0 {
		t.Errorf("a deferral must write no run record (wrote %d)", recs)
	}
	if escalated != 0 {
		t.Error("a deferral must not escalate")
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
	if recs != 2 || escalated != 2 {
		t.Errorf("timed-out/never-started runs must be recorded as failures: recs=%d escalated=%d", recs, escalated)
	}
}

// A run aborted by the daemon's shutdown writes no record and escalates
// nothing: its kill was the daemon's own, so a failure receipt would spend
// the plugin's cooldown and a failure escalation would report the shutdown as
// the plugin's fault (gt-7uyfc). It is not a deferral either — nothing was
// accomplished and nothing is retried.
func TestCompleteScriptRun_AbortedWritesNothing(t *testing.T) {
	t.Parallel()
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p", Execution: &plugin.Execution{AllowDeferredExit: true}}
	recs := 0
	escalated, good := 0, 0
	var logs []string
	hooks := scriptRunHooks{
		record:    func(plugin.PluginRunRecord) error { recs++; return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { escalated++ },
		onSuccess: func(*plugin.Plugin) { good++ },
		logf:      func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	for _, res := range []scriptResult{
		{aborted: true, exitCode: -1, err: errors.New("signal: killed")},
		// An abort whose kill was read as the deferred exit code: still no
		// retry, because the daemon ended the run rather than the script.
		{aborted: true, exitCode: scriptExitDeferred},
	} {
		completeScriptRun(p, res, hooks)
	}
	if recs != 0 {
		t.Errorf("an aborted run must write no run record (wrote %d)", recs)
	}
	if escalated != 0 || good != 0 {
		t.Errorf("an aborted run must neither escalate nor report a good run: escalated=%d good=%d", escalated, good)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "aborted") || !strings.Contains(logs[0], "shutting down") {
		t.Errorf("aborts must be logged as the daemon's own: %v", logs)
	}
}

// Exit 3 only means deferral for a plugin whose plugin.md opts in with
// [execution] allow_deferred_exit = true. Without the opt-in — the default,
// and every script plugin except rebuild-gt at the time of writing — exit 3
// is an ordinary failure: recorded and escalated like any other
// nonzero exit. This is what keeps one plugin's private exit-code contract
// from silently swallowing a real failure in an unrelated plugin that
// happens to exit 3 (gt-oqbw).
func TestCompleteScriptRun_DeferralRequiresOptIn(t *testing.T) {
	t.Parallel()
	p := &plugin.Plugin{Name: "x", RigName: "gastown", Path: "/p"}
	var recs []plugin.PluginRunRecord
	escalated := 0
	hooks := scriptRunHooks{
		record:    func(r plugin.PluginRunRecord) error { recs = append(recs, r); return nil },
		onFailure: func(*plugin.Plugin, scriptResult) { escalated++ },
		logf:      func(string, ...any) {},
	}
	completeScriptRun(p, scriptResult{exitCode: scriptExitDeferred, output: "boom"}, hooks)
	if len(recs) != 1 || recs[0].Result != plugin.ResultFailure || escalated != 1 {
		t.Fatalf("exit 3 without opt-in must be an ordinary failure: recs=%+v escalated=%d", recs, escalated)
	}

	p.Execution = &plugin.Execution{AllowDeferredExit: false}
	completeScriptRun(p, scriptResult{exitCode: scriptExitDeferred, output: "boom"}, hooks)
	if len(recs) != 2 || recs[1].Result != plugin.ResultFailure || escalated != 2 {
		t.Fatalf("exit 3 with allow_deferred_exit=false must still be an ordinary failure: recs=%+v escalated=%d", recs, escalated)
	}
}

// fakeRecorder is a runRecorder. It guards its records with a mutex: the
// daemon calls it from the script's goroutine while the test reads it.
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
// failure and escalated under its own fingerprint with the exit status and
// output; its next good run closes that escalation; a plugin that never
// failed escalates and clears nothing (gt-ckunw).
func TestStartScriptPlugin_FailureEscalatesAndRecoveryClears(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	logs := &lockedBuffer{}
	release := make(chan struct{})
	failing := true
	var mu sync.Mutex
	bash := newFakeCLIFor(func(c cliCall) cliReply {
		switch filepath.Base(c.dir) {
		case "flaky":
			mu.Lock()
			defer mu.Unlock()
			if failing {
				return cliReply{stderr: "the disk is full\n", code: 7}
			}
		case "slow":
			<-release
		}
		return cliReply{}
	})
	notes := notifyfake.New()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(logs, "", 0), ctx: context.Background(), execCmd: bash.run, notifier: notes}

	flaky := scriptPlugin(t, "flaky", "exit 7\n")
	d.startScriptPlugin(flaky, rec)
	d.scripts.wait()
	if recs := rec.records(); recs[0].Result != plugin.ResultFailure {
		t.Errorf("record = %+v", recs[0])
	}
	esc := notes.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want one", esc)
	}
	if e := esc[0].Escalation; e.Fingerprint != pluginFailureAlertKey("flaky") ||
		!strings.Contains(e.Reason, "exit 7") || !strings.Contains(e.Reason, "the disk is full") {
		t.Errorf("failure escalation: %+v", e)
	}
	if !strings.Contains(logs.String(), "script plugin flaky FAILED (exit 7") {
		t.Errorf("failure not logged:\n%s", logs.String())
	}
	if _, err := os.Stat(scriptLogPath(d.config.TownRoot, "flaky")); err != nil {
		t.Errorf("run log not written: %v", err)
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	d.startScriptPlugin(flaky, rec)
	d.scripts.wait()
	clears := notes.Clears()
	if len(clears) != 1 || !slices.Equal(clears[0].Fingerprints, []string{pluginFailureAlertKey("flaky")}) {
		t.Errorf("recovery must clear the plugin's escalation: %+v", clears)
	}
	if open := notes.OpenEscalations(pluginFailureAlertKey("flaky")); len(open) != 0 {
		t.Errorf("escalation still open after recovery: %+v", open)
	}

	good := scriptPlugin(t, "passing", "exit 0\n")
	d.startScriptPlugin(good, rec)
	d.scripts.wait()
	if recs := rec.records(); recs[2].Result != plugin.ResultSuccess || len(notes.Escalations()) != 1 || len(notes.Clears()) != 1 {
		t.Errorf("a plugin that never failed must not escalate or clear: recs=%+v calls=%+v", recs, notes.Calls())
	}

	// A second start while the first still runs is a no-op. The script holds
	// until the test releases it, so the second start certainly overlaps.
	slow := scriptPlugin(t, "slow", "exit 0\n")
	d.startScriptPlugin(slow, rec)
	d.startScriptPlugin(slow, rec)
	if !strings.Contains(logs.String(), "script plugin slow still running, skipping") {
		t.Errorf("the overlapping start was not refused:\n%s", logs.String())
	}
	close(release)
	d.scripts.wait()
	if names := d.scripts.inFlight(); len(names) != 0 {
		t.Errorf("script plugins still marked running after they finished: %v", names)
	}
	if n := len(rec.records()); n != 4 {
		t.Errorf("in-flight guard failed: %d records for two overlapping starts, want 4", n)
	}
}
