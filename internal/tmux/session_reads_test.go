package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// answerExec answers every command with out and records "name args…" lines.
func answerExec(calls *[]string, out string, err error) execFunc {
	return func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		return []byte(out), nil, err
	}
}

// A sandboxed session's panes and processes are read inside its sandbox: the
// host's list-panes and ps cannot see them (daemon liveness audit, 2026-10-23).
//
//testpolicy:allow parallel — installs the process-wide session router, which another test must not see mid-run
func TestSessionReadsGoToTheClientTheRouterNames(t *testing.T) {
	var hostCalls, sandboxCalls []string
	host := newTmuxForTest("gt-test", answerExec(&hostCalls, "host-out", nil), nil)
	sandbox := newTmuxForTest("gt", answerExec(&sandboxCalls, "ga-box\t143\n", nil), nil).Routed()
	SetSessionRouter(func(session string) *Tmux {
		if session == "ga-box" {
			return sandbox
		}
		return nil
	})
	t.Cleanup(func() { SetSessionRouter(nil) })

	panes, err := host.SessionPanes("ga-box")
	if err != nil || panes != "ga-box\t143" {
		t.Fatalf("SessionPanes(ga-box) = %q, %v; want the sandbox's answer", panes, err)
	}
	table, err := host.ProcessTable("ga-box")
	if err != nil || table != "ga-box\t143\n" {
		t.Fatalf("ProcessTable(ga-box) = %q, %v; want the sandbox's answer", table, err)
	}
	if len(hostCalls) != 0 {
		t.Errorf("the host client was asked about a sandbox session: %v", hostCalls)
	}
	if len(sandboxCalls) != 2 ||
		!strings.Contains(sandboxCalls[0], "list-panes -t =ga-box -s") ||
		!strings.Contains(sandboxCalls[1], "ps -ww -eo pid,ppid,args") {
		t.Errorf("sandbox calls = %v, want list-panes for =ga-box then ps -ww", sandboxCalls)
	}

	hostPanes, err := host.SessionPanes("gt-rig-local")
	if err != nil || hostPanes != "host-out" || len(hostCalls) != 1 {
		t.Errorf("host session: panes=%q err=%v hostCalls=%v; want the host's own answer", hostPanes, err, hostCalls)
	}
}

func TestSessionReadsReportAFailedQuery(t *testing.T) {
	t.Parallel()
	var calls []string
	c := newTmuxForTest("gt-test", answerExec(&calls, "", errors.New("exit status 1")), nil)

	if out, err := c.SessionPanes("gt-rig-box"); err == nil {
		t.Errorf("SessionPanes = %q, nil; want the query failure, not an empty answer", out)
	}
	if out, err := c.ProcessTable("gt-rig-box"); err == nil || !strings.Contains(err.Error(), "listing processes") {
		t.Errorf("ProcessTable = %q, %v; want a 'listing processes' error", out, err)
	}
}
