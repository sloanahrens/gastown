package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Spec dispatcher ticker (gt-4k3fj.5).
//
// Every interval (default 60s) the daemon runs `gt spec dispatch --json`: one
// tick of the spec dispatcher, which lints ready spec beads and slings clean
// ones onto a free seat of their class (internal/cmd/spec.go). The decision
// lives in internal/cmd next to the pool accounting and the sling path, which
// the daemon cannot import; this side is the cadence, the single-flight guard
// and the log. Off unless patrols.spec_dispatch.enabled is true.
//
// The ticker never restarts or kills anything, and it honors the operator
// hold file and ESTOP through the command itself.

const (
	defaultSpecDispatchInterval = 60 * time.Second

	// specDispatchTimeout bounds one tick. A sling spawns a worktree and a
	// session, and a tick slings at most max_per_tick beads (default 1), so
	// five minutes covers a slow Dolt without stranding the ticker.
	specDispatchTimeout = 5 * time.Minute
)

// specDispatchInterval returns the configured interval, or 60s.
func specDispatchInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.SpecDispatch != nil {
		if s := config.Patrols.SpecDispatch.IntervalStr; s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultSpecDispatchInterval
}

// specDispatchTickReport is the slice of the tick's JSON the daemon logs.
type specDispatchTickReport struct {
	Hold       string `json:"hold"`
	Roster     string `json:"roster"`
	Candidates int    `json:"candidates"`
	Dispatched []struct {
		Line string `json:"line"`
	} `json:"dispatched"`
	Refused []struct {
		Line string `json:"line"`
	} `json:"refused"`
	Planning []struct {
		Line string `json:"line"`
	} `json:"planning"`
	Skipped []struct {
		Line string `json:"line"`
	} `json:"skipped"`
	Failed []struct {
		Line string `json:"line"`
	} `json:"failed"`
	Errors []string `json:"errors"`
}

// triggerSpecDispatch starts one tick on its own goroutine unless one is
// already running. It reports whether a tick started.
func (d *Daemon) triggerSpecDispatch() bool {
	if !d.specDispatchRunning.CompareAndSwap(false, true) {
		d.logger.Printf("spec_dispatch: previous tick still running, skipping")
		return false
	}
	d.specDispatchCycles.Add(1)
	go func() {
		defer d.specDispatchCycles.Done()
		defer d.specDispatchRunning.Store(false)
		d.runSpecDispatch()
	}()
	return true
}

// runSpecDispatch runs one tick and logs its outcome, one line per decision.
func (d *Daemon) runSpecDispatch() {
	if !d.isPatrolActive("spec_dispatch") || d.config == nil {
		return
	}
	out, err := d.runSpecDispatchCommand()
	if err != nil {
		d.logger.Printf("spec_dispatch: tick failed: %v", err)
		return
	}
	for _, line := range formatSpecDispatchReport(out) {
		d.logger.Printf("spec_dispatch: %s", line)
	}
}

func (d *Daemon) runSpecDispatchCommand() ([]byte, error) {
	ctx, cancel := context.WithTimeout(d.ctx, specDispatchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.gtPath, "spec", "dispatch", "--json") //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot
	cmd.Env = daemonGTEnv(os.Environ())
	stdout, stderr, err := d.runCmd(cmd)
	if err != nil {
		if msg := strings.TrimSpace(string(stderr)); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, lastLine(msg))
		}
		return nil, err
	}
	return stdout, nil
}

// formatSpecDispatchReport renders a tick's JSON as log lines. The sling path
// prints progress to stdout before the JSON, so the report is read from the
// last JSON object in the output.
func formatSpecDispatchReport(out []byte) []string {
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndex(text, "\n{"); i >= 0 {
		text = text[i+1:]
	}
	var r specDispatchTickReport
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return []string{fmt.Sprintf("unparseable tick output (%v)", err)}
	}
	if r.Hold != "" {
		return []string{"held: " + r.Hold}
	}
	lines := []string{fmt.Sprintf("tick: %d candidate(s), roster %s, %d dispatched, %d refused, %d planning, %d skipped, %d failed",
		r.Candidates, r.Roster, len(r.Dispatched), len(r.Refused), len(r.Planning), len(r.Skipped), len(r.Failed))}
	for _, group := range []struct {
		name    string
		entries []struct {
			Line string `json:"line"`
		}
	}{{"dispatched", r.Dispatched}, {"refused", r.Refused}, {"planning", r.Planning}, {"failed", r.Failed}} {
		for _, e := range group.entries {
			lines = append(lines, group.name+": "+e.Line)
		}
	}
	for _, e := range r.Errors {
		lines = append(lines, "error: "+e)
	}
	return lines
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
