package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// It also remembers the keys each plugin has an escalation open under, so a
// good run closes them and a plugin that never failed costs no
// `gt escalate clear`. The keys are a set because a plugin's alert key moves
// with its finding set: the run that raises a new key closes the ones it
// replaces, and a recovery closes every key this daemon raised.
// A key leaves the set only once its clear has landed, so a clear that fails
// leaves the escalation tracked and the next run tries again.
// The memory is the daemon's: after a restart an open escalation stays open
// until the operator closes it or the plugin fails and recovers again, so a
// set that changed across a restart leaves the old escalation for the
// operator rather than closing it unasked.
type scriptRunner struct {
	mu        sync.Mutex
	running   map[string]time.Time
	escalated map[string]map[string]bool
	active    sync.WaitGroup // one per run in flight, done when it finishes
}

func newScriptRunner() *scriptRunner {
	return &scriptRunner{running: map[string]time.Time{}, escalated: map[string]map[string]bool{}}
}

// markEscalated records that an escalation under key is open for name.
func (r *scriptRunner) markEscalated(name, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.escalated[name] == nil {
		r.escalated[name] = map[string]bool{}
	}
	r.escalated[name][key] = true
}

// escalatedKeys returns, sorted, the keys name has escalations open under.
// It does not forget them: only the clear that lands may do that.
func (r *scriptRunner) escalatedKeys(name string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sortedKeys(r.escalated[name])
}

// supersededEscalations is escalatedKeys for the keys other than keep: a run
// that raised keep supersedes every other escalation this daemon has open for
// name, and takes back those keys to close.
func (r *scriptRunner) supersededEscalations(name, keep string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make(map[string]bool, len(r.escalated[name]))
	for key := range r.escalated[name] {
		if key != keep {
			keys[key] = true
		}
	}
	return sortedKeys(keys)
}

// forgetEscalated drops key from name's open set, once its clear has landed.
func (r *scriptRunner) forgetEscalated(name, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := r.escalated[name]
	delete(keys, key)
	if len(keys) == 0 {
		delete(r.escalated, name)
	}
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

// inFlight lists, sorted, the plugins whose run.sh is executing. Callers read
// it to tell whether any is running and to name it; a list rather than a count
// because an operator staring at a stalled install wants to know which plugin
// holds it (gt-rtbbr). Safe on a nil runner: the daemon creates it lazily
// (scriptsOnce).
func (r *scriptRunner) inFlight() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.running) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.running))
	for name := range r.running {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// scriptResult is what one run.sh execution produced.
type scriptResult struct {
	exitCode int
	timedOut bool
	aborted  bool  // the daemon's own shutdown canceled the run
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
	case r.aborted:
		return "aborted by daemon shutdown"
	case r.timedOut:
		return fmt.Sprintf("timed out after %s", r.duration.Round(time.Second))
	case r.err != nil && r.exitCode == 0:
		return "could not run: " + r.err.Error()
	default:
		return fmt.Sprintf("exit %d after %s", r.exitCode, r.duration.Round(time.Second))
	}
}

// runsAsScript reports whether the daemon should execute p itself: the
// plugin declares [execution] type = "script" and ships a run.sh or names a
// binary. A script-type plugin with neither has nothing to run; the handler
// logs it as skipped every heartbeat so the mismatch stays visible.
func runsAsScript(p *plugin.Plugin) bool {
	return p != nil && p.Execution != nil && p.Execution.Type == plugin.ExecTypeScript &&
		(p.HasRunScript || p.Execution.Binary != "")
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

// pluginProcess builds what a script-type plugin runs: the binary its
// [execution] block names, with an empty argv and no shell in between, or
// `bash run.sh`. A plugin that names a binary and still ships run.sh runs the
// binary; the shim is a stepping stone, not a second entry point.
func pluginProcess(ctx context.Context, p *plugin.Plugin) (*exec.Cmd, error) {
	if p.Execution != nil && p.Execution.Binary != "" {
		bin, err := p.BinaryPath()
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, bin), nil //nolint:gosec // G204: the argv is empty; BinaryPath refused a path that leaves the plugin directory
	}
	return exec.CommandContext(ctx, "bash", "run.sh"), nil //nolint:gosec // G204: fixed argv, plugin dir from the scanner
}

// runPluginScript executes the plugin's entry point — the binary its
// [execution] block names, else <p.Path>/run.sh — with cwd = the plugin
// directory and a process group of its own, so a timeout kills the whole tree
// rather than the `bash` wrapper alone (gt-6t43 is the orphan this avoids).
// The environment is env's plus the town identity a dog would have carried:
// scripts read GT_TOWN_ROOT/GT_ROOT, and bd inside them reads the
// BEADS_DOLT_* endpoint the daemon exports. The process runs through run (nil
// runs it for real).
func runPluginScript(ctx context.Context, run cmdRunFunc, env scriptEnv, p *plugin.Plugin, townRoot string, timeout time.Duration) scriptResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := pluginProcess(ctx, p)
	if err != nil {
		return scriptResult{duration: time.Since(start), err: err}
	}
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
	// The daemon's own shutdown cancels this context, and the kill that ends
	// the run is then ours rather than a verdict on the script: the run is
	// aborted, recorded nowhere and escalated nowhere (gt-7uyfc). Only a
	// cancel reads that way — a timeout returned above as timedOut, and a run
	// that finished on its own exit code keeps its own result.
	if errors.Is(ctx.Err(), context.Canceled) && !res.ok() {
		res.aborted = true
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
//
// An aborted run (the daemon stopping, see scriptResult.aborted) writes
// nothing either: its kill was the daemon's own, so it is no verdict on the
// plugin — a failure record would spend the plugin's cooldown and a failure
// escalation would report the shutdown as the plugin's fault (gt-7uyfc).
func completeScriptRun(p *plugin.Plugin, res scriptResult, h scriptRunHooks) {
	if res.aborted {
		h.logf("Handler: script plugin %s aborted (daemon shutting down); no run recorded", p.Name)
		return
	}
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

// pluginTarget names the file the runner executed, for escalations: the
// binary the plugin names, else its run.sh.
func pluginTarget(p *plugin.Plugin) string {
	if p.Execution != nil && p.Execution.Binary != "" {
		return filepath.Join(p.Path, p.Execution.Binary)
	}
	return filepath.Join(p.Path, "run.sh")
}

// escalatePluginFailure raises (or adds to) p's failure escalation with the
// output tail, and remembers it so the next good run closes it.
//
// The cloud-check plugin's key carries the urgent finding set. Before changing
// that, read the cloud-check paragraph of docs/design/plugin-system.md: it
// states the refresh, supersede and reopen rule that this function and
// plugin_findings.go implement.
//
// A run that raised or refreshed one key supersedes every other escalation the
// plugin has open, because the key moves with the finding set and leaving the
// previous one open would put two escalations in the attention queue for one
// plugin (gt-ggd75). The old ones close only once the new raise has landed; a
// raise that failed leaves them open rather than trading one alarm for none.
func (d *Daemon) escalatePluginFailure(p *plugin.Plugin, res scriptResult, townRoot string) {
	key := pluginFailureAlertKey(p.Name)
	summary := ""
	if alertKey, alertSummary := d.cloudCheckAlert(p); alertKey != "" {
		key, summary = alertKey, alertSummary
	}
	if p.Name == cloudCheckPlugin && d.pluginEscalationClosedRecently(key) {
		// The current set's escalation was closed by hand, so nothing is
		// raised for it; an older set's open escalation is still stale, and
		// closes here so the queue is left with what the operator decided.
		d.supersedePluginEscalations(p.Name, key)
		d.logger.Printf("Handler: script plugin %s failed (%s); its escalation was closed within %s, not raising another",
			p.Name, res.status(), pluginEscalationReopenWindow)
		return
	}
	msg := fmt.Sprintf("script plugin %s failed: %s", p.Name, res.status())
	if summary != "" {
		msg += "\n\n" + summary
	}
	msg += fmt.Sprintf("\n\nThe daemon ran %s; full output of the last run: %s\n\n%s",
		pluginTarget(p), scriptLogPath(townRoot, p.Name), strings.TrimRight(res.output, "\n"))
	if err := d.escalateAlertErr(key, "plugin:"+p.Name, msg); err != nil {
		return
	}
	d.supersedePluginEscalations(p.Name, key)
	d.scripts.markEscalated(p.Name, key)
}

// supersedePluginEscalations closes the escalations this daemon has open for
// name under any key but key, the one the run just raised or refreshed. Their
// finding sets are gone: the report now describes key's set.
//
// A key is forgotten only after its clear lands. A clear that failed leaves
// the escalation open in the store, so forgetting it here would leak an alarm
// nothing would close; keeping it lets the next failing run, or the next
// recovery, retry the clear.
func (d *Daemon) supersedePluginEscalations(name, key string) {
	superseded := d.scripts.supersededEscalations(name, key)
	if len(superseded) == 0 {
		return
	}
	if err := d.clearAlertsErr(pluginCloseSupersededPrefix+key, superseded...); err != nil {
		return
	}
	for _, old := range superseded {
		d.scripts.forgetEscalated(name, old)
	}
}

// clearPluginFailure closes the failure escalations this daemon raised under
// p's keys. A key whose escalation was already closed is not an error to clear
// again (see clearAlerts), so recovery after a finding set came and went
// closes whatever is still open. As with a superseded key, a key stays tracked
// until its clear lands, so a failed clear is retried on the next pass.
func (d *Daemon) clearPluginFailure(p *plugin.Plugin) {
	keys := d.scripts.escalatedKeys(p.Name)
	if len(keys) == 0 {
		return
	}
	if err := d.clearAlertsErr(pluginCloseCleared, keys...); err != nil {
		return
	}
	for _, key := range keys {
		d.scripts.forgetEscalated(p.Name, key)
	}
}

// runRecorder writes a plugin run record; an interface so the handler can be
// exercised without a beads store.
type runRecorder interface {
	RecordRun(rec plugin.PluginRunRecord) (string, error)
}
