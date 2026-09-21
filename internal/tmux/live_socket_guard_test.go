//go:build !windows

package tmux

import (
	"os/exec"
	"strings"
	"testing"
)

// liveSocketEnv is a $TMUX value shaped like the real one a test process
// inherits from the agent shell that launched it. It names a socket tmux never
// serves, so a guard that fails to fire leaves nothing behind on this machine.
const liveSocketEnv = "/private/tmp/tmux-501/gt-liveguard,99999,0"

// TestRefuseLiveSessionCreate covers the guard added for gt-2bj: a test binary
// that inherits $TMUX must not create sessions on the server it is running
// inside, because those sessions surface in the live roster as phantom
// polecats.
func TestRefuseLiveSessionCreate(t *testing.T) {
	t.Setenv("TMUX", liveSocketEnv)
	t.Setenv(AllowLiveTmuxEnv, "")

	// An empty socket name is not the default server: tmux honors $TMUX ahead
	// of it, so this is the shape every unharnessed test package has.
	err := (&Tmux{}).NewSession("gt-test-liveguard-refused", "")
	if err == nil {
		t.Fatal("NewSession on the inherited live socket = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "gt-2bj") {
		t.Errorf("error %q does not cite the bead", err)
	}

	// Naming the live socket outright is the same leak with the resolution
	// spelled out.
	if err := NewTmuxWithSocket("gt-liveguard").NewSession("gt-test-liveguard-explicit", ""); err == nil {
		t.Fatal("NewSession on an explicitly named live socket = nil, want refusal")
	}
}

// TestLiveSessionCreateAllowedWhenOptedOut keeps the escape hatch honest: a
// test that deliberately needs the live socket still passes the guard.
func TestLiveSessionCreateAllowedWhenOptedOut(t *testing.T) {
	t.Setenv("TMUX", liveSocketEnv)
	t.Setenv(AllowLiveTmuxEnv, "1")

	if err := (&Tmux{}).refuseLiveSessionCreate(); err != nil {
		t.Errorf("with %s=1: %v", AllowLiveTmuxEnv, err)
	}
}

// TestIsolatedSocketStillCreatesSession is the other half of the guard: a test
// that names its own gt-test-* socket must keep working while $TMUX is set,
// which is how the hermetic harness routes every package.
func TestIsolatedSocketStillCreatesSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("TMUX", liveSocketEnv)
	t.Setenv(AllowLiveTmuxEnv, "")

	socket := uniqueSocketName(t, "gt-test-liveguard")
	// KillServer, not a bare `tmux kill-server`: it unlinks the socket file,
	// which tmux leaves behind when the server exits (gt-20di).
	t.Cleanup(func() { _ = NewTmuxWithSocket(socket).KillServer() })

	if err := NewTmuxWithSocket(socket).NewSession("gt-test-liveguard-isolated", ""); err != nil {
		t.Fatalf("NewSession on %q: %v", socket, err)
	}
}
