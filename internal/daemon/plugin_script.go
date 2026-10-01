package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
)

// Script-type plugins run in the daemon.
//
// Measured 2026-09-18 (gt-fo2k): a dog session for a run.sh plugin was six
// tool calls of ceremony around `bash run.sh` and a ~24k-token first turn,
// 27 times in 90 minutes, with no judgment involved for 11 of 13 plugins.
// The daemon runs the script itself and records the result from the exit
// code. A failure used to go to a dog with the output; those dispatches
// fixed nothing (gt-ckunw), so a failure now logs and escalates, one open
// escalation per plugin, closed by its next good run.

// defaultScriptTimeout bounds a run.sh whose plugin.md sets no
// [execution] timeout.
const defaultScriptTimeout = 10 * time.Minute

// scriptOutputTail is how much of the combined output is kept for the run
// record and the failure mail. Scripts can be chatty; the end is what
// explains an exit code.
const scriptOutputTail = 16 * 1024

// scriptExitDeferred is the exit code a script plugin whose plugin.md sets
// [execution] allow_deferred_exit = true uses to say "I did nothing this
// time; ask me again on the next heartbeat".
//
// It exists because the cooldown gate is satisfied by a run RECORD, and a
// run record is what the runner writes on any exit — so an exit-0 skip spent
// a full cooldown (1h for rebuild-gt) on a run that accomplished nothing.
// For a plugin whose work waits on a window that opens and closes on its own
// (rebuild-gt waits for the town to be quiet, gt-oqbw) that is starvation:
// every cooldown tick landed inside a busy window, and the work never ran at
// all. A deferral writes no record, so no gate is satisfied and the next
// heartbeat retries; the daemon log and the plugin's own last-run log carry
// the trail instead of a bead per attempt.
//
// A deferral is NOT a failure: it escalates nothing, and unlike a
// failure it is expected to be the common outcome of a run whose window is
// shut.
//
// The opt-in is per plugin, not global: exit 3 has no inherent meaning to a
// shell script, and a plugin author who doesn't know this contract could
// exit 3 for an ordinary failure (a tool it shells out to uses that code, a
// case statement's default arm). Without the opt-in, that failure would be
// silently read as "nothing to see here, retry later" instead of recorded
// and escalated like every other nonzero exit (gt-oqbw).
const scriptExitDeferred = 3

// scriptSkippedMarker is the line a run.sh prints on an exit-0 path that
// accomplished nothing, so the daemon records the run as skipped instead of
// success. A silent no-op serializing as a success receipt is the bug class
// the plugins had on 2026-09-18 (gt-chqi): the wrapper records whatever the
// script's exit code says, and exit 0 meant "all fine" even when the script
// had nothing to do. A skipped run is not a failure: it records a receipt,
// satisfies the cooldown gate, and escalates nothing.
const scriptSkippedMarker = "[plugin-result skipped]"

// scriptRunner tracks plugins whose run.sh is executing in-process so a
// heartbeat that fires mid-run neither starts a second copy nor waits on the
// first. It is the in-flight half of the cooldown gate: the run record that
// satisfies the gate is written when the script finishes, with its real
// result.
//
// It also remembers which plugins have an escalation open, so a good run
// closes it and a plugin that never failed costs no `gt escalate clear`.
// The memory is the daemon's: after a restart an open escalation stays open
// until the mayor closes it or the plugin fails and recovers again.
type scriptRunner struct {
	mu        sync.Mutex
	running   map[string]time.Time
	escalated map[string]bool
	active    sync.WaitGroup // one per run in flight, done when it finishes
}

func newScriptRunner() *scriptRunner {
	return &scriptRunner{running: map[string]time.Time{}, escalated: map[string]bool{}}
}

// markEscalated records that name has an escalation open.
func (r *scriptRunner) markEscalated(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.escalated[name] = true
}

// takeEscalated reports whether name had an escalation open and forgets it.
func (r *scriptRunner) takeEscalated(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.escalated[name]
	delete(r.escalated, name)
	return was
}

// tryStart marks name as running and reports whether the caller may start
// it (false when a run is already in flight).
func (r *scriptRunner) tryStart(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.running[name]; busy {
		return false
	}
	r.running[name] = time.Now()
	r.active.Add(1)
	return true
}

func (r *scriptRunner) finish(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, name)
	r.active.Done()
}

// wait blocks until every run started so far has finished.
func (r *scriptRunner) wait() {
	r.active.Wait()
}

// runningCount reports how many script plugins are in flight. Safe on a nil
// runner: the daemon creates it lazily (scriptsOnce).
func (r *scriptRunner) runningCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running)
}

// scriptResult is what one run.sh execution produced.
type scriptResult struct {
	exitCode int
	timedOut bool
	err      error // process could not be started, or a non-exit error
	output   string
	duration time.Duration
}

// ok reports whether the script succeeded (exit 0 within its budget).
func (r scriptResult) ok() bool { return r.err == nil && !r.timedOut && r.exitCode == 0 }

// skipped reports whether the script exited 0 and printed the skip marker
// (see scriptSkippedMarker): it ran, found nothing to do, and wants the
// receipt to say so.
func (r scriptResult) skipped() bool {
	return r.ok() && strings.Contains(r.output, scriptSkippedMarker)
}

// deferred reports whether the script asked to be retried on the next
// heartbeat (see scriptExitDeferred). Only a plugin that declares
// [execution] allow_deferred_exit = true gets that reading of exit 3; for
// every other script plugin it is an ordinary failure, recorded and
// escalated like any other nonzero exit. A run that never started,
// timed out or failed some other way is not a deferral either way: those are
// outcomes to record.
func (r scriptResult) deferred(p *plugin.Plugin) bool {
	return p.Execution != nil && p.Execution.AllowDeferredExit &&
		r.err == nil && !r.timedOut && r.exitCode == scriptExitDeferred
}

// status is the one-line summary used in run records and escalations.
func (r scriptResult) status() string {
	switch {
	case r.timedOut:
		return fmt.Sprintf("timed out after %s", r.duration.Round(time.Second))
	case r.err != nil && r.exitCode == 0:
		return "could not run: " + r.err.Error()
	default:
		return fmt.Sprintf("exit %d after %s", r.exitCode, r.duration.Round(time.Second))
	}
}

// runsAsScript reports whether the daemon should execute p itself: the
// plugin declares [execution] type = "script" and ships a run.sh. A plugin
// that declares script without a run.sh has nothing to run; the handler
// logs it as skipped every heartbeat so the mismatch stays visible.
func runsAsScript(p *plugin.Plugin) bool {
	return p != nil && p.HasRunScript && p.Execution != nil && p.Execution.Type == plugin.ExecTypeScript
}

// scriptTimeout returns the plugin's [execution] timeout, or the default
// when unset or unparsable.
func scriptTimeout(p *plugin.Plugin) time.Duration {
	if p.Execution != nil && p.Execution.Timeout != "" {
		if d, err := time.ParseDuration(p.Execution.Timeout); err == nil && d > 0 {
			return d
		}
	}
	return defaultScriptTimeout
}

// scriptEnv carries what a script run inherits: the daemon's environment and
// the tmux socket it resolved, read once by the caller so a test can hand in
// its own.
type scriptEnv struct {
	environ    []string
	tmuxSocket string
}

// daemonScriptEnv is the scriptEnv of this daemon process.
func daemonScriptEnv() scriptEnv {
	return scriptEnv{environ: os.Environ(), tmuxSocket: tmux.GetDefaultSocket()}
}

// runPluginScript executes <p.Path>/run.sh with cwd = the plugin directory
// and a process group of its own, so a timeout kills the whole tree rather
// than the `bash` wrapper alone (gt-6t43 is the orphan this avoids). The
// environment is env's plus the town identity a dog would have carried:
// scripts read GT_TOWN_ROOT/GT_ROOT, and bd inside them reads the
// BEADS_DOLT_* endpoint the daemon exports. The script runs through run (nil
// runs bash for real).
func runPluginScript(ctx context.Context, run cmdRunFunc, env scriptEnv, p *plugin.Plugin, townRoot string, timeout time.Duration) scriptResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "run.sh") //nolint:gosec // G204: fixed argv, plugin dir from the scanner
	cmd.Dir = p.Path
	cmd.Env = append(daemonGTEnv(env.environ),
		"GT_ROOT="+townRoot,
		"GT_TOWN_ROOT="+townRoot,
		"GT_PLUGIN_NAME="+p.Name,
		"GT_PLUGIN_RUNNER=daemon",
		"GT_ROLE=daemon/plugin",
	)
	// A dog runs inside the town's tmux server and reaches it through $TMUX;
	// the daemon is outside any session, so a bare `tmux` in the script would
	// ask the default server. Hand over the socket the daemon resolved.
	if env.tmuxSocket != "" {
		cmd.Env = append(cmd.Env, "GT_TMUX_SOCKET="+env.tmuxSocket)
	}
	// SetProcessGroup puts the script in its own group AND installs a Cancel
	// hook that SIGKILLs the negative pid, so a timeout takes bash and every
	// child it backgrounded; CommandContext alone would signal only bash.
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	out, err := combinedOutputWith(run, cmd)
	res := scriptResult{duration: time.Since(start), output: tail(string(out), scriptOutputTail)}
	if ctx.Err() == context.DeadlineExceeded {
		res.timedOut = true
		res.exitCode = -1
		return res
	}
	if err != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) {
			res.exitCode = exitErr.ExitCode()
		} else {
			res.err = err
		}
	}
	return res
}

// tail returns the last n bytes of s, cut at a line boundary when possible.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "[... " + fmt.Sprint(len(s)-len(cut)) + " bytes elided ...]\n" + cut
}

// scriptRunHooks are the side effects of a finished script run, split out
// so tests can observe them without a beads store or a gt binary.
type scriptRunHooks struct {
	record    func(rec plugin.PluginRunRecord) error
	onFailure func(p *plugin.Plugin, res scriptResult)
	onSuccess func(p *plugin.Plugin)
	logf      func(format string, args ...any)
}

// completeScriptRun records the result and, on failure, escalates it with
// the output attached. A failed record must not hide the failure: the
// escalation runs regardless, and the record error is logged. A good run
// (success or skipped) calls onSuccess, which closes an open escalation.
//
// A deferral is the one outcome with no record: the record is what satisfies
// the cooldown gate, and a run that accomplished nothing must not buy one
// (see scriptExitDeferred). The log line is the whole of its trail.
func completeScriptRun(p *plugin.Plugin, res scriptResult, h scriptRunHooks) {
	if res.deferred(p) {
		h.logf("Handler: script plugin %s deferred (%s); will retry on the next heartbeat", p.Name, res.status())
		return
	}
	result := plugin.ResultSuccess
	switch {
	case !res.ok():
		result = plugin.ResultFailure
	case res.skipped():
		result = plugin.ResultSkipped
	}
	body := fmt.Sprintf("Direct run by the daemon (execution type script): %s\n\n%s", res.status(), res.output)
	if err := h.record(plugin.PluginRunRecord{
		PluginName: p.Name,
		RigName:    p.RigName,
		Result:     result,
		Title:      fmt.Sprintf("Plugin run: %s (script)", p.Name),
		Body:       body,
	}); err != nil {
		h.logf("Handler: failed to record script run for plugin %s: %v", p.Name, err)
	}
	if res.ok() {
		if res.skipped() {
			h.logf("Handler: script plugin %s skipped (%s); nothing to do this run", p.Name, res.status())
		} else {
			h.logf("Handler: script plugin %s ok (%s)", p.Name, res.status())
		}
		if h.onSuccess != nil {
			h.onSuccess(p)
		}
		return
	}
	h.logf("Handler: script plugin %s FAILED (%s); escalating", p.Name, res.status())
	if h.onFailure != nil {
		h.onFailure(p, res)
	}
}

// scriptLogPath is where the full combined output of the last run lands,
// for a human who wants more than the tail in the run record.
func scriptLogPath(townRoot, name string) string {
	return filepath.Join(townRoot, "daemon", "plugin-runs", name+".log")
}

// startScriptPlugin runs p's run.sh in a goroutine (the heartbeat must not
// wait on a script) and, when it finishes, records the run and escalates a
// failure. A second heartbeat while the script runs is a no-op.
func (d *Daemon) startScriptPlugin(p *plugin.Plugin, recorder runRecorder) {
	d.scriptsOnce.Do(func() { d.scripts = newScriptRunner() })
	if !d.scripts.tryStart(p.Name) {
		d.logger.Printf("Handler: script plugin %s still running, skipping", p.Name)
		return
	}
	timeout := scriptTimeout(p)
	townRoot := d.config.TownRoot
	d.logger.Printf("Handler: running script plugin %s directly (timeout %s)", p.Name, timeout)
	go func() {
		defer d.scripts.finish(p.Name)
		res := runPluginScript(d.ctx, d.execCmd, daemonScriptEnv(), p, townRoot, timeout)
		writeScriptLog(townRoot, p.Name, res)
		completeScriptRun(p, res, scriptRunHooks{
			record: func(rec plugin.PluginRunRecord) error {
				_, err := recorder.RecordRun(rec)
				return err
			},
			onFailure: func(p *plugin.Plugin, res scriptResult) {
				d.escalatePluginFailure(p, res, townRoot)
			},
			onSuccess: d.clearPluginFailure,
			logf:      d.logger.Printf,
		})
	}()
}

// writeScriptLog keeps the last run's output tail on disk next to the
// daemon log, so a failure can be read without opening the run bead.
func writeScriptLog(townRoot, name string, res scriptResult) {
	path := scriptLogPath(townRoot, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	body := fmt.Sprintf("# %s — %s — %s\n%s", name, time.Now().Format(time.RFC3339), res.status(), res.output)
	_ = os.WriteFile(path, []byte(body), 0o644)
}

// pluginFailureAlertKey is the escalation fingerprint for one plugin's
// failures: a plugin that keeps failing adds to one open escalation instead
// of opening one per run.
func pluginFailureAlertKey(name string) string {
	return "plugin:" + name + ":failed"
}

// escalatePluginFailure raises (or adds to) p's failure escalation with the
// output tail, and remembers it so the next good run closes it.
func (d *Daemon) escalatePluginFailure(p *plugin.Plugin, res scriptResult, townRoot string) {
	msg := fmt.Sprintf("script plugin %s failed: %s\n\nThe daemon ran %s/run.sh; full output of the last run: %s\n\n%s",
		p.Name, res.status(), p.Path, scriptLogPath(townRoot, p.Name), strings.TrimRight(res.output, "\n"))
	if err := d.escalateAlertErr(pluginFailureAlertKey(p.Name), "plugin:"+p.Name, msg); err != nil {
		return
	}
	d.scripts.markEscalated(p.Name)
}

// clearPluginFailure closes p's failure escalation when this daemon raised
// one.
func (d *Daemon) clearPluginFailure(p *plugin.Plugin) {
	if d.scripts.takeEscalated(p.Name) {
		d.clearAlerts(fmt.Sprintf("plugin %s ran clean", p.Name), pluginFailureAlertKey(p.Name))
	}
}

// runRecorder writes a plugin run record; an interface so the handler can be
// exercised without a beads store.
type runRecorder interface {
	RecordRun(rec plugin.PluginRunRecord) (string, error)
}
