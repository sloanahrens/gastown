package doctor

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

// sandboxWorld installs a session router and an extra-sessions hook that name
// the sandbox session "ga-bullet", and returns the exec seams: host answers
// list-sessions with hostSessions and records its commands; the sandbox
// records every command it is sent. The clients are real *tmux.Tmux over
// canned output.
type sandboxWorld struct {
	hostCalls    []string
	sandboxCalls []string
}

func (w *sandboxWorld) install(t *testing.T) {
	t.Helper()
	sandbox := tmux.NewTmuxWithExec("gt", func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		w.sandboxCalls = append(w.sandboxCalls, name+" "+strings.Join(args, " "))
		return nil, nil, nil
	}).Routed()
	tmux.SetSessionRouter(func(sess string) *tmux.Tmux {
		if sess == "ga-bullet" {
			return sandbox
		}
		return nil
	})
	tmux.SetExtraSessions(func() []string { return []string{"ga-bullet"} })
	t.Cleanup(func() {
		tmux.SetExtraSessions(nil)
		tmux.SetSessionRouter(nil)
	})
}

func (w *sandboxWorld) host(sessions string) *tmux.Tmux {
	return tmux.NewTmuxWithExec("host", func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		w.hostCalls = append(w.hostCalls, name+" "+strings.Join(args, " "))
		if slices.Contains(args, "list-sessions") {
			return []byte(sessions), nil, nil
		}
		return nil, nil, nil
	})
}

func (w *sandboxWorld) assertSandboxUntouched(t *testing.T) {
	t.Helper()
	if len(w.sandboxCalls) != 0 {
		t.Errorf("the sandbox was sent commands: %v", w.sandboxCalls)
	}
	for _, c := range w.hostCalls {
		if strings.Contains(c, "ga-bullet") {
			t.Errorf("a host socket was asked about the sandbox session: %s", c)
		}
	}
}

// With the town socket not "default", a client that lists the router's extra
// sessions puts every live sandbox session on both sockets, classes it a
// DUPLICATE, and Fix kills it through the router. The check's listers must not
// see sandbox sessions at all.
//
//testpolicy:allow parallel — installs the process-wide session router and extra-sessions hook, which another test must not see mid-run
func TestSocketSplitBrainNeverClassesOrKillsASandboxSession(t *testing.T) {
	var w sandboxWorld
	w.install(t)
	check := NewSocketSplitBrainCheck()
	check.useSocketForTest = true
	check.socketForTest = "gt-abc123"
	check.newTmuxForTest = func(socket string) *tmux.Tmux {
		if socket == "default" {
			return w.host("ga-crew-max\nga-crew-old\n")
		}
		return w.host("ga-crew-max\n")
	}
	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}

	result := check.Run(ctx)
	if result.Status != StatusError || !slices.Equal(check.staleSessions, []string{"ga-crew-max", "ga-crew-old"}) {
		t.Fatalf("Run: status=%v stale=%v details=%v; want exactly the duplicate and the orphan", result.Status, check.staleSessions, result.Details)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	w.assertSandboxUntouched(t)
	if !slices.ContainsFunc(w.hostCalls, func(c string) bool { return strings.Contains(c, "kill-session") && strings.Contains(c, "ga-crew-old") }) {
		t.Errorf("the orphan on the default socket was not killed: %v", w.hostCalls)
	}
}

// The env check writes host-derived values into the sessions it lists; a
// sandbox session's environment holds the sandbox's values.
//
//testpolicy:allow parallel — installs the process-wide session router and extra-sessions hook, which another test must not see mid-run
func TestEnvVarsFixNeverTouchesASandboxSession(t *testing.T) {
	var w sandboxWorld
	w.install(t)
	check := NewEnvVarsCheck()
	check.newTmuxForTest = func() *tmux.Tmux { return w.host("ga-crew-max\n") }
	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	result := check.Run(ctx)

	w.assertSandboxUntouched(t)
	if !slices.ContainsFunc(w.hostCalls, func(c string) bool { return strings.Contains(c, "ga-crew-max") }) {
		t.Errorf("the host session was not examined at all: %v (result %v)", w.hostCalls, result.Message)
	}
}
