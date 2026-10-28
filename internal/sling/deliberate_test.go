package sling

import (
	"context"
	"testing"
)

// TestRunCarriesTheDeliberateBitToTheSpawn: the classification the cobra layer
// makes has to reach the spawn, because the per-bead respawn budget is charged
// there. A Run that dropped the bit would make every hand sling spend the
// bead's budget again, and hold a rework at the limit (gt-jogkx).
func TestRunCarriesTheDeliberateBitToTheSpawn(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		deliberate bool
	}{
		{name: "a hand dispatch", deliberate: true},
		{name: "an automated dispatch", deliberate: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSlingDeps{}
			opts := holdTestOptions()
			opts.Deliberate = tt.deliberate

			if _, err := Run(context.Background(), f.deps(), opts); err != nil {
				t.Fatalf("dispatch failed: %v", err)
			}
			if len(f.spawnOptions) != 1 {
				t.Fatalf("spawns = %d, want 1", len(f.spawnOptions))
			}
			if got := f.spawnOptions[0].Deliberate; got != tt.deliberate {
				t.Errorf("spawn Deliberate = %v, want %v", got, tt.deliberate)
			}
		})
	}
}
