package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// answerExec answers every command with out and records each program it runs.
func answerExec(calls *[]string, out string, err error) execFunc {
	return func(_ context.Context, name string, _ ...string) ([]byte, []byte, error) {
		*calls = append(*calls, name)
		return []byte(out), nil, err
	}
}

// answerByProgram answers each program with its own output, so the two reads
// are told apart by what they return, and records each program it runs.
func answerByProgram(calls *[]string, outputs map[string]string) execFunc {
	return func(_ context.Context, name string, _ ...string) ([]byte, []byte, error) {
		*calls = append(*calls, name)
		return []byte(outputs[name]), nil, nil
	}
}

// A sandboxed session's panes and processes are read inside its sandbox: the
// host's list-panes and ps cannot see them (daemon liveness audit, 2026-10-23).
//
//testpolicy:allow parallel — installs the process-wide session router, which another test must not see mid-run
func TestSessionReadsGoToTheClientTheRouterNames(t *testing.T) {
	var hostAsked, sandboxAsked []string
	host := newTmuxForTest("gt-test", answerByProgram(&hostAsked, map[string]string{"tmux": "host-out"}), nil)
	sandbox := newTmuxForTest("gt", answerByProgram(&sandboxAsked, map[string]string{
		"tmux": "ga-box\t143\n",
		"ps":   "  143 1 claude\n",
	}), nil).Routed()
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
	if err != nil || table != "  143 1 claude\n" {
		t.Fatalf("ProcessTable(ga-box) = %q, %v; want the sandbox's answer", table, err)
	}
	if len(hostAsked) != 0 {
		t.Errorf("the host client was asked about a sandbox session: %v", hostAsked)
	}
	if len(sandboxAsked) != 2 {
		t.Errorf("the sandbox answered %d reads, want one per read: %v", len(sandboxAsked), sandboxAsked)
	}

	hostPanes, err := host.SessionPanes("gt-rig-local")
	if err != nil || hostPanes != "host-out" || len(hostAsked) != 1 {
		t.Errorf("host session: panes=%q err=%v hostAsked=%v; want the host's own answer", hostPanes, err, hostAsked)
	}
}

// When the sandbox cannot answer, the read fails. It never falls back to the
// host, where the same session name is a different process table.
//
//testpolicy:allow parallel — installs the process-wide session router, which another test must not see mid-run
func TestSessionReadsFailWhenTheSandboxFails(t *testing.T) {
	var hostAsked, sandboxAsked []string
	host := newTmuxForTest("gt-test", answerByProgram(&hostAsked, map[string]string{"tmux": "host-out", "ps": "  9 1 host\n"}), nil)
	sandbox := newTmuxForTest("gt", answerExec(&sandboxAsked, "", errors.New("exit status 1")), nil).Routed()
	SetSessionRouter(func(session string) *Tmux {
		if session == "ga-box" {
			return sandbox
		}
		return nil
	})
	t.Cleanup(func() { SetSessionRouter(nil) })

	if out, err := host.SessionPanes("ga-box"); err == nil {
		t.Errorf("SessionPanes(ga-box) = %q, nil; want the sandbox's failure, not an answer", out)
	}
	if out, err := host.ProcessTable("ga-box"); err == nil || !strings.Contains(err.Error(), "listing processes") {
		t.Errorf("ProcessTable(ga-box) = %q, %v; want a 'listing processes' error", out, err)
	}
	if len(hostAsked) != 0 {
		t.Errorf("the host answered a sandbox session after the sandbox failed: %v", hostAsked)
	}
	if len(sandboxAsked) != 2 {
		t.Errorf("the sandbox was asked %d reads, want one per read: %v", len(sandboxAsked), sandboxAsked)
	}
}

// A sandbox client answers its own reads and never sends them back through the
// router, which would hand them to the host or loop. ProcessTable relies on this.
//
//testpolicy:allow parallel — installs the process-wide session router, which another test must not see mid-run
func TestRoutedClientReadsNeverConsultTheRouter(t *testing.T) {
	var sandboxAsked []string
	sandbox := newTmuxForTest("gt", answerByProgram(&sandboxAsked, map[string]string{
		"tmux": "ga-box\t143\n",
		"ps":   "  143 1 claude\n",
	}), nil).Routed()
	routerCalls := 0
	SetSessionRouter(func(string) *Tmux {
		routerCalls++
		return nil
	})
	t.Cleanup(func() { SetSessionRouter(nil) })

	if out, err := sandbox.SessionPanes("ga-box"); err != nil || out != "ga-box\t143" {
		t.Fatalf("SessionPanes(ga-box) = %q, %v; want the sandbox's own answer", out, err)
	}
	if out, err := sandbox.ProcessTable("ga-box"); err != nil || out != "  143 1 claude\n" {
		t.Fatalf("ProcessTable(ga-box) = %q, %v; want the sandbox's own answer", out, err)
	}
	if routerCalls != 0 {
		t.Errorf("a routed client asked the router %d times; it must answer its own reads", routerCalls)
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
