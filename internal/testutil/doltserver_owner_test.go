//go:build !windows

package testutil

import (
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/slot"
)

// Every Dolt test container names the process that started it, so the
// container-gate can remove what a killed or failed run leaves behind by
// ownership rather than by age (gt-ehlga). Reads the request the options
// build; starts nothing.
func TestDoltContainerOpts_LabelsOwnerProcess(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no hostname on this machine: %v", err)
	}
	req := applyOpts(t)
	if got := req.Labels[slot.OwnerPIDLabel]; got != strconv.Itoa(os.Getpid()) {
		t.Errorf("Labels[%s] = %q, want this test process's pid %d", slot.OwnerPIDLabel, got, os.Getpid())
	}
	if got := req.Labels[slot.OwnerHostLabel]; got != host {
		t.Errorf("Labels[%s] = %q, want %q", slot.OwnerHostLabel, got, host)
	}
	// On darwin and linux the owner's start time is readable, and it is what
	// tells a reused pid apart from the owner.
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if req.Labels[slot.OwnerStartLabel] == "" {
			t.Errorf("Labels[%s] is empty, want the test process's start time", slot.OwnerStartLabel)
		}
	}
	if req.Env["DOLT_ROOT_HOST"] != "%" {
		t.Errorf("labels displaced the env: DOLT_ROOT_HOST = %q", req.Env["DOLT_ROOT_HOST"])
	}
}
