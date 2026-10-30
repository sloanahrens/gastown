package main

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// TestProcessDepsAreWired: every seam the command hands a run is set, and the
// environment the sweep's children inherit is the process's own.
func TestProcessDepsAreWired(t *testing.T) {
	t.Parallel()
	d := processDeps()
	if d.Clone == nil || d.Sweep == nil || d.WriteResult == nil {
		t.Errorf("a step seam is nil: clone %v, sweep %v, write %v", d.Clone != nil, d.Sweep != nil, d.WriteResult != nil)
	}
	if d.MkdirTemp == nil || d.MkdirAll == nil || d.RemoveAll == nil || d.Now == nil {
		t.Errorf("a filesystem or clock seam is nil: mkdirTemp %v, mkdirAll %v, removeAll %v, now %v",
			d.MkdirTemp != nil, d.MkdirAll != nil, d.RemoveAll != nil, d.Now != nil)
	}
	if len(d.Environ) != len(os.Environ()) {
		t.Errorf("environment has %d entries, the process has %d", len(d.Environ), len(os.Environ()))
	}
}
