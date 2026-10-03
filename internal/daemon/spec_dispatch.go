package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/townhealth"
)

// Spec dispatcher ticker (gt-4k3fj.5).
//
// Every interval (default 60s) the daemon runs `gt spec dispatch --json`: one
// tick of the spec dispatcher, which lints ready work beads and slings clean
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

	// dispatchTickHistory bounds the tick records the daemon keeps for
	// townhealth's dispatch field: two hours at the default 60s interval,
	// far past the health window (gt-xiw7o).
	dispatchTickHistory = 120
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
	// LabeledFailed is the tick's count of ready beads held out of the queue
	// by the spec-dispatch-failed label, which the health field surfaces
	// (gt-q6zoo).
	LabeledFailed int `json:"labeled_failed"`
	Dispatched    []struct {
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
	Notices []string `json:"notices"`
	Errors  []string `json:"errors"`
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
	// A held tick decided nothing, so it carries no roster to record.
	if r, perr := parseSpecDispatchReport(out); perr == nil && r.Hold == "" {
		d.recordDispatchTick(r, d.clk().Now())
	}
	for _, line := range formatSpecDispatchReport(out) {
		d.logger.Printf("spec_dispatch: %s", line)
	}
}

// recordDispatchTick appends one tick's decision to the history townhealth's
// dispatch field judges, keeping the newest dispatchTickHistory records
// (gt-xiw7o). The history is in memory: a restart drops it, and the field
// reads the fresh daemon as too young to have ticked until the ticker has
// run again.
func (d *Daemon) recordDispatchTick(r specDispatchTickReport, at time.Time) {
	seats, unreadable := dispatchRosterSeats(r.Roster)
	t := townhealth.DispatchTick{
		At:               at,
		Candidates:       r.Candidates,
		Seats:            seats,
		RosterUnreadable: unreadable,
		Dispatched:       len(r.Dispatched),
		Refused:          len(r.Refused),
		Planning:         len(r.Planning),
		Skipped:          len(r.Skipped),
		Failed:           len(r.Failed),
		LabeledFailed:    r.LabeledFailed,
	}
	d.dispatchTicksMu.Lock()
	defer d.dispatchTicksMu.Unlock()
	d.dispatchTicks = append(d.dispatchTicks, t)
	if n := len(d.dispatchTicks); n > dispatchTickHistory {
		d.dispatchTicks = append([]townhealth.DispatchTick(nil), d.dispatchTicks[n-dispatchTickHistory:]...)
	}
}

// dispatchTickRecords copies the recorded ticks, oldest first.
func (d *Daemon) dispatchTickRecords() []townhealth.DispatchTick {
	d.dispatchTicksMu.Lock()
	defer d.dispatchTicksMu.Unlock()
	return append([]townhealth.DispatchTick(nil), d.dispatchTicks...)
}

// dispatchRosterSeats reads the seats out of a tick's roster, which
// Budget.Picture renders as "agent live/cap, agent live/cap" and "no seats"
// for none. It reports unreadable for a roster it cannot read in full: an
// empty roster (the tick could not count one) or a seat it cannot parse
// could be hiding a free seat, so the tick must not be judged as a full town
// (gt-xiw7o).
func dispatchRosterSeats(roster string) ([]townhealth.DispatchSeat, bool) {
	roster = strings.TrimSpace(roster)
	if roster == "" {
		return nil, true
	}
	if roster == "no seats" {
		return nil, false
	}
	var out []townhealth.DispatchSeat
	for _, part := range strings.Split(roster, ",") {
		part = strings.TrimSpace(part)
		i := strings.LastIndexByte(part, ' ')
		if i < 0 {
			return nil, true
		}
		live, cap, ok := strings.Cut(part[i+1:], "/")
		if !ok {
			return nil, true
		}
		l, lerr := strconv.Atoi(live)
		c, cerr := strconv.Atoi(cap)
		if lerr != nil || cerr != nil {
			return nil, true
		}
		out = append(out, townhealth.DispatchSeat{Live: l, Cap: c})
	}
	return out, false
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

// parseSpecDispatchReport reads a tick's report out of its output. The sling
// path prints progress to stdout before the JSON, so the report is the last
// JSON object in the output.
func parseSpecDispatchReport(out []byte) (specDispatchTickReport, error) {
	var r specDispatchTickReport
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndex(text, "\n{"); i >= 0 {
		text = text[i+1:]
	}
	err := json.Unmarshal([]byte(text), &r)
	return r, err
}

// formatSpecDispatchReport renders a tick's JSON as log lines.
func formatSpecDispatchReport(out []byte) []string {
	r, err := parseSpecDispatchReport(out)
	if err != nil {
		return []string{fmt.Sprintf("unparseable tick output (%v)", err)}
	}
	if r.Hold != "" {
		return []string{"held: " + r.Hold}
	}
	lines := []string{fmt.Sprintf("tick: %d candidate(s), roster %s, %d dispatched, %d refused, %d planning, %d skipped, %d failed, %d held by the failed label",
		r.Candidates, r.Roster, len(r.Dispatched), len(r.Refused), len(r.Planning), len(r.Skipped), len(r.Failed), r.LabeledFailed)}
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
	for _, n := range r.Notices {
		lines = append(lines, "note: "+n)
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
