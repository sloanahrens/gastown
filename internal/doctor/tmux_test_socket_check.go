package doctor

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/tmuxsweep"
)

// TmuxTestSocketCheck reports tmux servers left behind by test runs that died
// before their cleanup, and the socket files every test server leaves behind.
// A leftover server's sessions are test-named, which is the shape the roster
// reads as a phantom polecat: no worktree, no agent bead, and a candidate for
// auto-nuke (gt-2bj).
//
// The sweep itself lives in internal/tmuxsweep so the daemon can run it on a
// cadence; this check is its on-demand presentation.
type TmuxTestSocketCheck struct {
	FixableCheck

	// report holds what Run found, reaped by Fix.
	report tmuxsweep.Report

	socketDirForTest   string                               // override for tmux.SocketDir()
	probeForTest       func(socket string) tmuxsweep.Server // override for the real tmux client
	pidAliveForTest    func(pid int) bool                   // override for os.FindProcess
	socketStateForTest func(path string) tmuxsweep.State    // override for tmuxsweep.DialState
	socketAgeForTest   func(path string) time.Duration      // override for the file's mtime
}

// NewTmuxTestSocketCheck creates a check for abandoned test tmux servers.
func NewTmuxTestSocketCheck() *TmuxTestSocketCheck {
	return &TmuxTestSocketCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "tmux-test-socket",
				CheckDescription: "Detect tmux servers abandoned by killed test runs",
				CheckCategory:    CategoryInfrastructure,
			},
		},
	}
}

// options is the sweep the check's seams configure.
func (c *TmuxTestSocketCheck) options() tmuxsweep.Options {
	return tmuxsweep.Options{
		SocketDir: c.socketDirForTest,
		Server:    c.probeForTest,
		PIDAlive:  c.pidAliveForTest,
		State:     c.socketStateForTest,
		Age:       c.socketAgeForTest,
	}
}

// Run reports every test socket whose owner is gone and whose server still
// answers with only test sessions.
func (c *TmuxTestSocketCheck) Run(ctx *CheckContext) *CheckResult {
	report, err := tmuxsweep.Scan(c.options())
	if err != nil {
		c.report = tmuxsweep.Report{}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read the tmux socket directory",
			Details: []string{err.Error()},
		}
	}
	c.report = report

	if !report.Found() {
		if len(report.Unprobed) > 0 {
			// Nothing was found, but not everything was looked at, and a pass
			// the check did not earn is the report it must not give.
			return &CheckResult{
				Name:    c.Name(),
				Status:  StatusSkipped,
				Message: fmt.Sprintf("unknown: %d socket file(s) could not be probed", len(report.Unprobed)),
				Details: []string{fmt.Sprintf("e.g. %s", strings.Join(firstFew(report.Unprobed, 5), ", "))},
			}
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No abandoned test tmux servers",
		}
	}

	if len(report.Leftovers) == 0 {
		details := []string{fmt.Sprintf("e.g. %s", strings.Join(firstFew(report.StaleFiles, 5), ", "))}
		if len(report.Unprobed) > 0 {
			details = append(details, unprobedDetail(report.Unprobed))
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d socket file(s) left by test runs whose server has exited", len(report.StaleFiles)),
			Details: details,
			FixHint: "Run 'gt doctor fix tmux-test-socket' to remove them",
		}
	}

	details := append([]string(nil), report.Evidence...)
	if len(report.StaleFiles) > 0 {
		details = append(details, fmt.Sprintf("plus %d socket file(s) whose server has exited", len(report.StaleFiles)))
	}
	if len(report.Unprobed) > 0 {
		details = append(details, unprobedDetail(report.Unprobed))
	}
	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: fmt.Sprintf("%d abandoned test tmux server(s) — their sessions read as phantom polecats (gt-2bj)",
			len(report.Leftovers)),
		Details: details,
		FixHint: "Run 'gt doctor fix tmux-test-socket' to kill them",
	}
}

// firstFew returns at most n names, for a detail line that counts in full but
// names a sample.
func firstFew(names []string, n int) []string {
	if len(names) <= n {
		return names
	}
	return names[:n]
}

// unprobedDetail names the sockets the scan could not ask, so a reader of a
// residue report can tell the count it is missing from (gt-ri37).
func unprobedDetail(unprobed []string) string {
	return fmt.Sprintf("plus %d socket file(s) that could not be probed: %s",
		len(unprobed), strings.Join(firstFew(unprobed, 5), ", "))
}

// DestructiveFix marks this repair as destructive (gt-638go.3): it kills leftover tmux servers.
func (c *TmuxTestSocketCheck) DestructiveFix() bool { return true }

// Fix kills each abandoned server and unlinks the socket files. A server
// unlinks its own socket on the way out, so the explicit Remove covers the one
// that died between Run and Fix, and the files whose server had already exited.
//
// Nothing unprobed is touched: those are the sockets Run could not make a
// finding about, and a fix has no more evidence than the scan it follows
// (gt-ri37).
func (c *TmuxTestSocketCheck) Fix(ctx *CheckContext) error {
	err := tmuxsweep.Reap(c.options(), c.report)
	c.report = tmuxsweep.Report{}
	return err
}
