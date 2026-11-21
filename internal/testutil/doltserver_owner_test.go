//go:build !windows

package testutil

import (
	"os"
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/procid"
	"github.com/steveyegge/gastown/internal/slot"
)

// Every Dolt test container names the process that started it, so the
// container-gate can remove what a killed or failed run leaves behind by
// ownership rather than by age (gt-ehlga). Reads the request the options
// build; starts nothing.
func TestDoltContainerOpts_LabelsOwnerProcess(t *testing.T) {
	t.Parallel()
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Fatalf("no hostname on this machine: %v", err)
	}
	req := applyOpts(t, newMapEnv())
	if got := req.Labels[slot.OwnerPIDLabel]; got != strconv.Itoa(os.Getpid()) {
		t.Errorf("Labels[%s] = %q, want this test process's pid %d", slot.OwnerPIDLabel, got, os.Getpid())
	}
	if got := req.Labels[slot.OwnerHostLabel]; got != host {
		t.Errorf("Labels[%s] = %q, want %q", slot.OwnerHostLabel, got, host)
	}
	// The start time tells a reused pid apart from the owner, and it is stamped
	// exactly when the same reader the gate uses can read it. Where it cannot,
	// the label must be absent: a made-up value would misjudge ownership.
	if want, ok := procid.StartToken(os.Getpid()); ok {
		if got := req.Labels[slot.OwnerStartLabel]; got != want {
			t.Errorf("Labels[%s] = %q, want this test process's start token %q", slot.OwnerStartLabel, got, want)
		}
	} else if got, present := req.Labels[slot.OwnerStartLabel]; present {
		t.Errorf("Labels[%s] = %q, want it absent: this platform has no start-token reader", slot.OwnerStartLabel, got)
	}
	if req.Env["DOLT_ROOT_HOST"] != "%" {
		t.Errorf("labels displaced the env: DOLT_ROOT_HOST is not the expected value")
	}
}
