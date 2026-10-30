package main

import (
	"os"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// TestDepsAreWired: every seam the command hands a run is set, and the
// environment it passes on is the one it was given, not the live process's.
func TestDepsAreWired(t *testing.T) {
	t.Parallel()
	env := []string{"PATH=/bin", "GT_SWEEP_RIG=gastown"}
	d := depsWith(env)
	if d.Clone == nil || d.Sweep == nil || d.WriteResult == nil {
		t.Errorf("a step seam is nil: clone %v, sweep %v, write %v", d.Clone != nil, d.Sweep != nil, d.WriteResult != nil)
	}
	if d.MkdirTemp == nil || d.MkdirAll == nil || d.RemoveAll == nil || d.Now == nil {
		t.Errorf("a filesystem or clock seam is nil: mkdirTemp %v, mkdirAll %v, removeAll %v, now %v",
			d.MkdirTemp != nil, d.MkdirAll != nil, d.RemoveAll != nil, d.Now != nil)
	}
	if !slices.Equal(d.Environ, env) {
		t.Errorf("environment = %q, want the %q it was given", d.Environ, env)
	}
}
