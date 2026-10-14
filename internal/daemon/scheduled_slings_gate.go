package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

const (
	// defaultScheduledGateTimeout bounds a gate_command when gate_timeout is unset.
	defaultScheduledGateTimeout = 60 * time.Second
	// scheduledGateOutputTail is how much of a failed gate's output the warning keeps.
	scheduledGateOutputTail = 200
	// scheduledGateAttentionAfter is the consecutive gate failures that raise the item.
	scheduledGateAttentionAfter = 3
)

func scheduledSlingGateCommand(e ScheduledSlingEntry) string {
	return strings.TrimSpace(e.GateCommand)
}

// scheduledSlingGateTimeout is the entry's gate bound. Validation has already
// rejected an unparseable or non-positive gate_timeout.
func scheduledSlingGateTimeout(e ScheduledSlingEntry) time.Duration {
	if e.GateTimeoutStr == "" {
		return defaultScheduledGateTimeout
	}
	d, _ := time.ParseDuration(e.GateTimeoutStr)
	return d
}

type scheduledGateVerdict int

const (
	scheduledGateRun     scheduledGateVerdict = iota // exit 0: the run happens
	scheduledGateNothing                             // exit 1: nothing to do
	scheduledGateFailed                              // any other exit, or a timeout
)

// scheduledGateOutcome is one gate evaluation: the verdict, and for a failure
// the exit status (or the timeout) and the tail of its output.
type scheduledGateOutcome struct {
	verdict scheduledGateVerdict
	detail  string
}

// classifyScheduledGate reads a gate run: runErr is the runner's error, ctxErr
// the gate context's error after the run, out the command's combined output.
// A deadline wins over the exit status, since a killed command exits with a
// signal status that says nothing about the gate's own answer.
func classifyScheduledGate(out []byte, runErr, ctxErr error, timeout time.Duration) scheduledGateOutcome {
	if errors.Is(ctxErr, context.DeadlineExceeded) || errors.Is(runErr, context.DeadlineExceeded) {
		return scheduledGateOutcome{scheduledGateFailed, scheduledGateDetail(fmt.Sprintf("timed out after %s", timeout), out)}
	}
	if runErr == nil {
		return scheduledGateOutcome{verdict: scheduledGateRun}
	}
	var ee interface{ ExitCode() int }
	if errors.As(runErr, &ee) {
		if ee.ExitCode() == 1 {
			return scheduledGateOutcome{verdict: scheduledGateNothing}
		}
		return scheduledGateOutcome{scheduledGateFailed, scheduledGateDetail(fmt.Sprintf("exit status %d", ee.ExitCode()), out)}
	}
	return scheduledGateOutcome{scheduledGateFailed, scheduledGateDetail(fmt.Sprintf("could not run: %v", runErr), out)}
}

func scheduledGateDetail(status string, out []byte) string {
	tail := strings.TrimSpace(string(out))
	if len(tail) > scheduledGateOutputTail {
		tail = tail[len(tail)-scheduledGateOutputTail:]
	}
	if tail == "" {
		return status
	}
	return fmt.Sprintf("%s: %s", status, tail)
}

// runGate runs the entry's gate_command with sh -c in the rig's canonical
// clone, bounded by ctx. It returns the combined output and the run's error.
func (r *execScheduledSlingRunner) runGate(ctx context.Context, e ScheduledSlingEntry) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", scheduledSlingGateCommand(e)) //nolint:gosec // G204: the operator's own configured command
	cmd.Dir = filepath.Join(r.townRoot, e.Rig, "mayor", "rig")
	cmd.Env = append(daemonGTEnv(bdMutationRoutingEnv(r.townRoot)), "GT_TOWN_ROOT="+r.townRoot, "GT_SCHEDULE_NAME="+e.Name)
	return combinedOutputWith(r.execCmd, boundDaemonCmd(cmd))
}

// scheduledGateState is one entry's consecutive gate failures.
type scheduledGateState struct {
	rig    string
	count  int
	detail string
}

// scheduledGateCounts holds every entry's gate failures. The patrol goroutine
// writes it; the attention tick reads it from the heartbeat goroutine.
type scheduledGateCounts struct {
	mu sync.Mutex
	m  map[string]scheduledGateState
}

// fail records a failure and returns the new consecutive count.
func (c *scheduledGateCounts) fail(name, rig, detail string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]scheduledGateState{}
	}
	st := c.m[name]
	st.rig, st.detail = rig, detail
	st.count++
	c.m[name] = st
	return st.count
}

// clear forgets name's failures.
func (c *scheduledGateCounts) clear(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, name)
}

// retain forgets every entry not in names, so a schedule removed from the
// config does not keep its item up.
func (c *scheduledGateCounts) retain(names map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name := range c.m {
		if !names[name] {
			delete(c.m, name)
		}
	}
}

// failing returns the entries at or past the attention threshold.
func (c *scheduledGateCounts) failing() map[string]scheduledGateState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]scheduledGateState{}
	for name, st := range c.m {
		if st.count >= scheduledGateAttentionAfter {
			out[name] = st
		}
	}
	return out
}

// scheduledGateAllows reports whether the entry's run goes ahead. An entry
// without gate_command always does. A gated or failed gate creates no bead, so
// the next tick asks again. Exit 0 and exit 1 are both answers and clear the
// failure count; a failure or timeout counts toward the attention item.
func (d *Daemon) scheduledGateAllows(ctx context.Context, r scheduledSlingRunner, e ScheduledSlingEntry) bool {
	if scheduledSlingGateCommand(e) == "" {
		return true
	}
	timeout := scheduledSlingGateTimeout(e)
	// The gate's deadline is its own: under the caller's 5-minute context a
	// longer gate_timeout would be cut short and still reported as timing out
	// after the configured value (gt-0frud).
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	out, runErr := r.runGate(gctx, e)
	ctxErr := gctx.Err()
	cancel()
	res := classifyScheduledGate(out, runErr, ctxErr, timeout)
	switch res.verdict {
	case scheduledGateRun:
		d.scheduledGateFailures.clear(e.Name)
		return true
	case scheduledGateNothing:
		d.scheduledGateFailures.clear(e.Name)
		d.logger.Printf("scheduled_slings: %s: gated, nothing to do", e.Name)
		return false
	}
	n := d.scheduledGateFailures.fail(e.Name, e.Rig, res.detail)
	d.logger.Printf("scheduled_slings: %s: warning: gate_command failed (%d in a row), skipping this tick: %s", e.Name, n, res.detail)
	return false
}

// collectScheduledGate raises one low item per scheduled sling whose gate has
// failed three ticks in a row. The next gate answer clears it.
func (s *attentionSources) collectScheduledGate(_ context.Context) ([]attention.Item, error) {
	var out []attention.Item
	for name, st := range s.d.scheduledGateFailures.failing() {
		out = append(out, attention.Item{
			Key:      "scheduled-gate:" + name,
			Kind:     attention.KindScheduledGate,
			Severity: attention.SeverityLow,
			Rig:      st.rig,
			Summary:  fmt.Sprintf("scheduled sling %s: gate_command failed %d ticks in a row (%s)", name, st.count, st.detail),
		})
	}
	return out, nil
}
