//go:build integration

package tmux

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// errClass reduces tmux's stderr to its message family, dropping the target
// and socket path: "can't find pane: nope" -> "can't find pane",
// "error connecting to /private/tmp/..." -> "error connecting to".
func errClass(stderr string) string {
	s := strings.TrimSpace(stderr)
	if i := strings.Index(s, " /"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}

// diffCase is one argv (after -u -L socket) and how its stdout is compared:
// exact, or only whether it is empty (pids, ids and paths differ by nature).
type diffCase struct {
	args      []string
	emptyOnly bool
}

// TestIntegrationFakeServerMatchesTmux replays the argv the package sends,
// including the missing-target and no-server paths the unit tests rely on,
// against real tmux and against fakeServer, and requires the same exit,
// error family and output. fakeServer's canned answers are only as good as
// this table.
func TestIntegrationFakeServerMatchesTmux(t *testing.T) {
	const alive = "gt-diff-alive"
	socket := constants.TestSocketName("gt-test-diff")
	real := NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = real.KillServer() })
	if err := real.NewSessionWithCommand(alive, "", "sleep 300"); err != nil {
		t.Fatalf("real fixture: %v", err)
	}
	if err := real.SetEnvironment(alive, "GT_X", "1"); err != nil {
		t.Fatalf("real fixture env: %v", err)
	}
	fake := newFakeServer()
	fake.addSession(alive, "sleep")
	fake.with(func() { fake.sessions[alive].env["GT_X"] = "1" })

	cases := []diffCase{
		{args: []string{"has-session", "-t", "=" + alive}},
		{args: []string{"has-session", "-t", "=gt-diff-nope"}},
		{args: []string{"new-session", "-d", "-s", alive}},
		{args: []string{"list-sessions", "-F", "#{session_name}"}},
		{args: []string{"list-sessions", "-F", "#{session_name}", "-f", "#{==:#{session_name},gt-diff-nope}"}},
		{args: []string{"display-message", "-t", alive + ":^", "-p", "#{pane_current_command}"}},
		{args: []string{"display-message", "-t", alive + ":^", "-p", "#{pane_pid}"}, emptyOnly: true},
		{args: []string{"display-message", "-t", alive, "-p", "#{session_name}"}},
		{args: []string{"display-message", "-t", "gt-diff-nope:^", "-p", "#{pane_current_command}"}},
		{args: []string{"display-message", "-t", "gt-diff-nope", "-p", "#{pane_pid}"}},
		{args: []string{"display-message", "-t", "%999999", "-p", "#{session_name}"}},
		{args: []string{"display-message", "-p", "-t", "gt-diff-nope", "#{pane_dead}"}},
		{args: []string{"capture-pane", "-p", "-t", "gt-diff-nope", "-S", "-30"}},
		{args: []string{"capture-pane", "-p", "-t", "gt-diff-nope"}},
		{args: []string{"send-keys", "-t", "gt-diff-nope", "-l", "--", "x"}},
		{args: []string{"send-keys", "-t", "gt-diff-nope", "Enter"}},
		{args: []string{"respawn-pane", "-k", "-t", "gt-diff-nope", "sleep 1"}},
		{args: []string{"split-window", "-t", "gt-diff-nope", "-d"}},
		{args: []string{"kill-session", "-t", "gt-diff-nope"}},
		{args: []string{"list-panes", "-s", "-t", "gt-diff-nope", "-F", "#{pane_id}"}},
		{args: []string{"list-panes", "-s", "-t", alive, "-F", "#{pane_current_command}"}},
		{args: []string{"new-window", "-t", "gt-diff-nope"}},
		{args: []string{"resize-window", "-t", "gt-diff-nope", "-x", "80"}},
		{args: []string{"set-option", "-t", "gt-diff-nope", "remain-on-exit", "on"}},
		{args: []string{"set-hook", "-t", "gt-diff-nope", "-u", "pane-died"}},
		{args: []string{"show-options", "-w", "-t", "gt-diff-nope", "window-size"}},
		{args: []string{"show-environment", "-t", "gt-diff-nope", "GT_X"}},
		{args: []string{"show-environment", "-t", alive, "GT_MISSING"}},
		{args: []string{"show-environment", "-t", alive, "GT_X"}},
		{args: []string{"set-environment", "-t", "gt-diff-nope", "GT_X", "1"}},
	}
	compare := func(t *testing.T, c diffCase) {
		t.Helper()
		rOut, rErr, rRunErr := realExec(context.Background(), "tmux", real.tmuxArgs(c.args)...)
		fr := fake.answer(tmuxCall{name: "tmux", socket: socket, args: c.args})
		realOut, fakeOut := strings.TrimSpace(string(rOut)), strings.TrimSpace(fr.stdout)
		if c.emptyOnly {
			realOut, fakeOut = boolStr(realOut != ""), boolStr(fakeOut != "")
		}
		if (rRunErr == nil) != (fr.err == nil) || errClass(string(rErr)) != errClass(fr.stderr) || realOut != fakeOut {
			t.Errorf("%v:\n  tmux: ok=%v stderr=%q stdout=%q\n  fake: ok=%v stderr=%q stdout=%q",
				c.args, rRunErr == nil, strings.TrimSpace(string(rErr)), realOut, fr.err == nil, fr.stderr, fakeOut)
		}
	}
	for _, c := range cases {
		compare(t, c)
	}

	// With no server at all.
	if err := real.KillServer(); err != nil {
		t.Fatal(err)
	}
	fake.with(func() { fake.noServer = true })
	for _, c := range []diffCase{
		{args: []string{"has-session", "-t", "=" + alive}},
		{args: []string{"list-sessions", "-F", "#{session_name}"}},
		{args: []string{"capture-pane", "-p", "-t", alive}},
		{args: []string{"send-keys", "-t", alive, "Enter"}},
		{args: []string{"display-message", "-t", alive, "-p", "#{pane_pid}"}},
		{args: []string{"kill-session", "-t", alive}},
	} {
		compare(t, c)
	}
}

func boolStr(b bool) string {
	if b {
		return "non-empty"
	}
	return "empty"
}
