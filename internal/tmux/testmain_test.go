package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// testServerShellCandidates are the shells the package's test server prefers,
// in order, each as the pair of options tmux needs: default-shell is the shell
// tmux runs, default-command is the command line it starts it with. Every
// candidate reads no startup file, so a session's first prompt is milliseconds
// away instead of the seconds a heavy interactive rc costs (gt-n0jv).
var testServerShellCandidates = []struct {
	binary  string
	command string
}{
	{"/bin/bash", "/bin/bash --norc --noprofile"},
	{"/bin/sh", "/bin/sh"},
}

// testProbeSession is the throwaway session pinFastTestShell uses to find out
// what tmux calls the shell it was pointed at. The name is one no test uses.
const testProbeSession = "gt-test-shell-probe"

// TestMain sets up a dedicated tmux server for the package's integration tests.
// All tests that call newTestTmux() share this isolated server, which is torn
// down after all tests complete. This prevents test sessions from appearing on
// the user's interactive tmux and avoids socket conflicts with other packages.
func TestMain(m *testing.M) {
	socket := fmt.Sprintf("gt-test-%d", os.Getpid())

	// Set defaultSocket so NewTmux() connects to the test server, not the
	// user's personal server or the sentinel that indicates "no town context".
	SetDefaultSocket(socket)

	// Start a sentinel session to keep the server alive for the entire test run.
	// Without this, tests that kill their last session inadvertently take down
	// the server, leaving a stale socket that prevents subsequent new-session
	// calls from restarting it (tmux sees the socket file but no listener).
	// The sentinel uses a name no individual test touches, so it outlives all
	// per-test sessions. TestMain kills the whole server at the end.
	if _, err := exec.LookPath("tmux"); err == nil {
		_ = exec.Command("tmux", "-u", "-L", socket, "new-session", "-d", "-s", "gt-test-sentinel").Run()
		pinFastTestShell()
	}

	code := m.Run()

	// Kill the test tmux server and restore the original socket state.
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	SetDefaultSocket("")

	os.Exit(code)
}

// pinFastTestShell points the test server's sessions at a shell that reads no
// startup file, so waitForShellPrompt is a barrier rather than a 4.4-5.4s tax.
//
// tmux runs the user's login shell for every new session, and that shell is not
// free to start: on a machine whose ~/.zshrc sources nvm.sh each session shows a
// blank pane for 4.4-5.4s before its first prompt (gt-n0jv). Tests that time an
// operation gated on a prompt (AcceptWorkspaceTrustDialog, AcceptBypass-
// PermissionsWarning) or that diff pane content across an Enter
// (sendEnterVerified) are then measuring shell startup: gt-32pv taught them to
// wait for the prompt first, which stopped the spurious failures but left every
// session in the package paying the login shell's startup. Pinning a shell that
// reads no rc file puts the prompt ~100ms out and removes the tax outright.
//
// The pin is conditional because tmux's callers match on the pane's command
// NAME: WaitForShellReady and WaitForCommand both compare #{pane_current_command}
// against constants.SupportedShells, so a shell tmux names differently — dash
// behind a Debian /bin/sh, say — would turn those waits into timeouts. tmux
// names the pane after the process it execs, which is why the probe session is
// how we find out; when no candidate qualifies, the server keeps the default it
// started with and the tests wait out the login shell exactly as they did
// before, so this can only ever make the package faster.
func pinFastTestShell() {
	tm := NewTmux()
	original, err := tm.run("show-options", "-gv", "default-shell")
	if err != nil {
		return
	}

	for _, candidate := range testServerShellCandidates {
		if _, err := os.Stat(candidate.binary); err != nil {
			continue
		}
		if _, err := tm.run("set-option", "-g", "default-shell", candidate.binary); err != nil {
			continue
		}
		if _, err := tm.run("set-option", "-g", "default-command", candidate.command); err != nil {
			continue
		}
		if paneCommandIsSupportedShell(tm) {
			return
		}
	}

	// No candidate gave a shell the package's waits recognize. Put the server
	// back the way we found it rather than leaving an unsupported name behind.
	_, _ = tm.run("set-option", "-g", "default-shell", original)
	_, _ = tm.run("set-option", "-gu", "default-command")
}

// paneCommandIsSupportedShell reports whether a session on the current server
// runs one of constants.SupportedShells, which is the set WaitForShellReady and
// WaitForCommand match #{pane_current_command} against.
func paneCommandIsSupportedShell(tm *Tmux) bool {
	_ = tm.KillSession(testProbeSession)
	if err := tm.NewSession(testProbeSession, ""); err != nil {
		return false
	}
	defer func() { _ = tm.KillSession(testProbeSession) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cmd, err := tm.GetPaneCommand(testProbeSession); err == nil {
			return slices.Contains(constants.SupportedShells, cmd)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
