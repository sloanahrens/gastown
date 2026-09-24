package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
)

// Script-type plugins run without a dog.
//
// Measured 2026-09-18 (gt-fo2k): a dog session for a run.sh plugin was six
// tool calls of ceremony around `bash run.sh` and a ~24k-token first turn,
// 27 times in 90 minutes, with no judgment involved for 11 of 13 plugins.
// The daemon can run the script itself, record the result from the exit
// code, and hand the output to a dog only when the script fails — that is
// the one moment an agent has something to decide.

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
// A deferral is NOT a failure: it hands nothing to a dog, and unlike a
// failure it is expected to be the common outcome of a run whose window is
// shut.
//
// The opt-in is per plugin, not global: exit 3 has no inherent meaning to a
// shell script, and a plugin author who doesn't know this contract could
// exit 3 for an ordinary failure (a tool it shells out to uses that code, a
// case statement's default arm). Without the opt-in, that failure would be
// silently read as "nothing to see here, retry later" instead of recorded
// and dispatched to a dog like every other nonzero exit (gt-oqbw).
const scriptExitDeferred = 3

// scriptSkippedMarker is the line a run.sh prints on an exit-0 path that
// accomplished nothing, so the daemon records the run as skipped instead of
// success. A silent no-op serializing as a success receipt is the bug class
// the plugins had on 2026-09-18 (gt-chqi): the wrapper records whatever the
// script's exit code says, and exit 0 meant "all fine" even when the script
// had nothing to do. A skipped run is not a failure: it records a receipt,
// satisfies the cooldown gate, and dispatches no dog.
const scriptSkippedMarker = "[plugin-result skipped]"

// scriptRunner tracks plugins whose run.sh is executing in-process so a
// heartbeat that fires mid-run neither starts a second copy nor waits on the
// first. It is the in-flight half of the cooldown gate: the run record that
// satisfies the gate is written when the script finishes, with its real
// result, instead of at dispatch as the dog path does.
type scriptRunner struct {
	mu      sync.Mutex
	running map[string]time.Time
}

func newScriptRunner() *scriptRunner {
	return &scriptRunner{running: map[string]time.Time{}}
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
	return true
}

func (r *scriptRunner) finish(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, name)
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
// dispatched to a dog like any other nonzero exit. A run that never started,
// timed out or failed some other way is not a deferral either way: those are
// outcomes to record.
func (r scriptResult) deferred(p *plugin.Plugin) bool {
	return p.Execution != nil && p.Execution.AllowDeferredExit &&
		r.err == nil && !r.timedOut && r.exitCode == scriptExitDeferred
}

// status is the one-line summary used in run records and mail.
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
// that declares script without a run.sh has nothing to run; it stays on the
// dog path so the mismatch is visible in a session rather than silent.
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

// runPluginScript executes <p.Path>/run.sh with cwd = the plugin directory
// and a process group of its own, so a timeout kills the whole tree rather
// than the `bash` wrapper alone (gt-6t43 is the orphan this avoids). The
// environment is the daemon's plus the town identity a dog would have
// carried: scripts read GT_TOWN_ROOT/GT_ROOT and default their Dolt
// coordinates from the ambient GT_DOLT_* variables.
func runPluginScript(ctx context.Context, p *plugin.Plugin, townRoot string, timeout time.Duration) scriptResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "run.sh") //nolint:gosec // G204: fixed argv, plugin dir from the scanner
	cmd.Dir = p.Path
	cmd.Env = append(os.Environ(),
		"GT_ROOT="+townRoot,
		"GT_TOWN_ROOT="+townRoot,
		"GT_PLUGIN_NAME="+p.Name,
		"GT_PLUGIN_RUNNER=daemon",
		"GT_ROLE=daemon/plugin",
		"BD_ACTOR=daemon",
	)
	// A dog runs inside the town's tmux server and reaches it through $TMUX;
	// the daemon is outside any session, so a bare `tmux` in the script would
	// ask the default server. Hand over the socket the daemon resolved.
	if sock := tmux.GetDefaultSocket(); sock != "" {
		cmd.Env = append(cmd.Env, "GT_TMUX_SOCKET="+sock)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// SetProcessGroup puts the script in its own group AND installs a Cancel
	// hook that SIGKILLs the negative pid, so a timeout takes bash and every
	// child it backgrounded; CommandContext alone would signal only bash.
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	res := scriptResult{duration: time.Since(start), output: tail(out.String(), scriptOutputTail)}
	if ctx.Err() == context.DeadlineExceeded {
		res.timedOut = true
		res.exitCode = -1
		return res
	}
	if err != nil {
		var exitErr *exec.ExitError
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
// so tests can observe them without a beads store or a dog pack.
type scriptRunHooks struct {
	record    func(rec plugin.PluginRunRecord) error
	onFailure func(p *plugin.Plugin, res scriptResult)
	logf      func(format string, args ...any)
}

// completeScriptRun records the result and, on failure, hands the plugin to
// a dog with the output attached. A failed record must not hide the failure:
// the dog dispatch runs regardless, and the record error is logged.
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
		return
	}
	h.logf("Handler: script plugin %s FAILED (%s); dispatching a dog with the output", p.Name, res.status())
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
// wait on a script) and, when it finishes, records the run and dispatches a
// dog on failure. A second heartbeat while the script runs is a no-op.
func (d *Daemon) startScriptPlugin(p *plugin.Plugin, mgr dogManager, sm dogSessionStarter, router mailSender, recorder runRecorder) {
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
		res := runPluginScript(d.ctx, p, townRoot, timeout)
		writeScriptLog(townRoot, p.Name, res)
		completeScriptRun(p, res, scriptRunHooks{
			record: func(rec plugin.PluginRunRecord) error {
				_, err := recorder.RecordRun(rec)
				return err
			},
			onFailure: func(p *plugin.Plugin, res scriptResult) {
				d.dispatchPluginToDog(p, mgr, sm, router, p.FormatFailureMailBody(res.status(), res.output))
			},
			logf: d.logger.Printf,
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

// The daemon's plugin dispatch talks to these three collaborators; they are
// interfaces so the failure path can be exercised without a dog pack, a
// tmux server or a beads store.
type dogManager interface {
	AssignWork(name, desc string) error
	ClearWork(name string) error
}

type dogSessionStarter interface {
	Start(name string, opts dog.SessionStartOptions) error
}

type mailSender interface {
	Send(msg *mail.Message) error
}

type runRecorder interface {
	RecordRun(rec plugin.PluginRunRecord) (string, error)
}
