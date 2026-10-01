package cmd

import (
	"errors"
	"testing"
)

// fakeMayorTmux records the destructive calls restartMayorRuntimeIfDead makes.
type fakeMayorTmux struct {
	alive    bool
	aliveErr error
	calls    []string
}

func (f *fakeMayorTmux) IsAgentAliveChecked(string) (bool, error) { return f.alive, f.aliveErr }
func (f *fakeMayorTmux) GetPaneID(string) (string, error) {
	f.calls = append(f.calls, "GetPaneID")
	return "%1", nil
}
func (f *fakeMayorTmux) SetEnvironment(_, _, _ string) error { return nil }
func (f *fakeMayorTmux) SetRemainOnExit(string, bool) error  { return nil }
func (f *fakeMayorTmux) KillPaneProcesses(string) error {
	f.calls = append(f.calls, "KillPaneProcesses")
	return nil
}
func (f *fakeMayorTmux) RespawnPane(_, _ string) error {
	f.calls = append(f.calls, "RespawnPane")
	return nil
}

// gt-fcxe9.1: `gt mayor attach` killed and respawned a live Mayor when the
// liveness query failed. An unknown answer must touch nothing.
func TestRestartMayorRuntimeIfDead_UnknownLivenessTouchesNothing(t *testing.T) {
	t.Parallel()
	f := &fakeMayorTmux{aliveErr: errors.New("tmux show-environment: timed out")}
	if err := restartMayorRuntimeIfDead(f, "hq-mayor", t.TempDir(), nil, ""); err != nil {
		t.Fatalf("restartMayorRuntimeIfDead: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("acted on an unknown liveness answer: %v", f.calls)
	}
}

// A live Mayor is left alone too.
func TestRestartMayorRuntimeIfDead_AliveTouchesNothing(t *testing.T) {
	t.Parallel()
	f := &fakeMayorTmux{alive: true}
	if err := restartMayorRuntimeIfDead(f, "hq-mayor", t.TempDir(), nil, ""); err != nil {
		t.Fatalf("restartMayorRuntimeIfDead: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("acted on a live Mayor: %v", f.calls)
	}
}
