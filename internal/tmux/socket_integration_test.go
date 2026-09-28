//go:build integration

package tmux

import "testing"

// These two swap the process-wide default socket, so they cannot run in
// parallel with anything that reads it; the integration tier runs serially.

func TestIntegrationSetGetDefaultSocket(t *testing.T) {
	orig := GetDefaultSocket()
	defer SetDefaultSocket(orig)

	SetDefaultSocket("")
	if got := GetDefaultSocket(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	SetDefaultSocket("mytown")
	if got := GetDefaultSocket(); got != "mytown" {
		t.Errorf("expected %q, got %q", "mytown", got)
	}
}

func TestIntegrationNewTmuxInheritsSocket(t *testing.T) {
	orig := GetDefaultSocket()
	defer SetDefaultSocket(orig)

	SetDefaultSocket("testtown")
	if tmx := NewTmux(); tmx.socketName != "testtown" {
		t.Errorf("NewTmux() socketName = %q, want %q", tmx.socketName, "testtown")
	}
}
