package cmd

import "testing"

// A sling's seat claim names the bead it spawns for, so the roster counts the
// running sling and the seat its claim holds once (gt-75fma5).
func TestPoolClaimNamesTheSpawnsBead(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, hook, want string }{
		{"a hook bead", "gt-a", "gt-a"},
		{"no hook bead", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newTestPoolTown(t, cappedPool(3))
			w.fs = newFakeSeatFS()
			router := w.process()
			s := slingSeatSpawn{
				backpressure: func(string, string, SlingSpawnOptions) error { return nil },
				resolvePool:  func(_, requested string) (string, string, error) { return router.route(requested, true) },
				releaseSeat:  router.seats.store.release,
				prepare: func(_, rigName string, opts SlingSpawnOptions, _ slingStores) (*SpawnedPolecatInfo, error) {
					return &SpawnedPolecatInfo{RigName: rigName, agent: opts.Agent}, nil
				},
				seatClaims: router.seats.store,
			}
			if _, err := s.spawn("/town", "gastown", SlingSpawnOptions{HookBead: tc.hook}); err != nil {
				t.Fatalf("spawn: %v", err)
			}
			claims := w.claims(t)
			if len(claims) != 1 {
				t.Fatalf("want one claim, got %d: %+v", len(claims), claims)
			}
			if claims[0].Bead != tc.want {
				t.Errorf("claim names bead %q, want %q", claims[0].Bead, tc.want)
			}
		})
	}
}
