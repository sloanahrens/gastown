package tmux

import (
	"strings"
	"testing"
)

// TestRefuseLiveSessionCreate covers the guard added for gt-2bj: a test binary
// that inherits $TMUX must not create sessions on the server it is running
// inside, because those sessions surface in the live roster as phantom
// polecats.
func TestRefuseLiveSessionCreate(t *testing.T) {
	t.Parallel()
	// An empty socket name is not the default server: tmux honors $TMUX ahead
	// of it, so this is the shape every unharnessed test package has.
	err := liveCreateRefusal("", "gt-liveguard", false)
	if err == nil {
		t.Fatal("create on the inherited live socket = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "gt-2bj") {
		t.Errorf("error %q does not cite the bead", err)
	}
	// Naming the live socket outright is the same leak with the resolution
	// spelled out.
	if err := liveCreateRefusal("gt-liveguard", "gt-liveguard", false); err == nil {
		t.Fatal("create on an explicitly named live socket = nil, want refusal")
	}
}

// TestLiveSessionCreateAllowedWhenOptedOut keeps the escape hatch honest: a
// test that deliberately needs the live socket still passes the guard.
func TestLiveSessionCreateAllowedWhenOptedOut(t *testing.T) {
	t.Parallel()
	if err := liveCreateRefusal("", "gt-liveguard", true); err != nil {
		t.Errorf("with %s=1: %v", AllowLiveTmuxEnv, err)
	}
}

// TestIsolatedSocketStillCreatesSession is the other half of the guard: a test
// that names its own gt-test-* socket must keep working while $TMUX is set,
// which is how the hermetic harness routes every package. Outside tmux there
// is nothing to guard.
func TestIsolatedSocketStillCreatesSession(t *testing.T) {
	t.Parallel()
	if err := liveCreateRefusal("gt-test-liveguard", "gt-liveguard", false); err != nil {
		t.Fatalf("isolated socket refused: %v", err)
	}
	if err := liveCreateRefusal("", "", false); err != nil {
		t.Fatalf("no $TMUX refused: %v", err)
	}
}

// TestSocketFromTMUX pins how the live socket is read out of $TMUX.
func TestSocketFromTMUX(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                                   "",
		"/private/tmp/tmux-501/gt-x,99999,0": "gt-x",
		"/tmp/tmux-501/default,1,2":          "default",
		",1,2":                               "",
	} {
		if got := socketFromTMUX(in); got != want {
			t.Errorf("socketFromTMUX(%q) = %q, want %q", in, got, want)
		}
	}
}
