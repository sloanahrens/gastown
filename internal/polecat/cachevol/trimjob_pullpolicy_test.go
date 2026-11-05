package cachevol

import "testing"

// TestTrimJobPullsAMovingTag pins the pull policy of every container the trim renders on either
// route: a :dev image pulls on every container, since the node's cached copy is whatever it pulled
// last, and a digest leaves the cluster's default.
func TestTrimJobPullsAMovingTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		image string
		route TrimRoute
		want  string
	}{
		{"shard route, dev tag", "gastown-polecat:dev", TrimRouteShards, pullPolicyAlways},
		{"shard route, digest", "gastown-polecat@" + testDigest, TrimRouteShards, ""},
		{"claim route, dev tag", "gastown-polecat:dev", TrimRouteClaim, pullPolicyAlways},
		{"claim route, digest", "gastown-polecat@" + testDigest, TrimRouteClaim, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := trimInput()
			in.Image = tc.image
			if tc.route == TrimRouteClaim {
				// Past the shard ceiling, so the Job carries the list in the claim.
				in.CapBytes = 0
				in.SandboxesRunning = 2
				in.Listing = shardPerPathListing(TrimMaxShards + 1)
			}
			m := buildTrim(t, in)
			if got := m.Route(); got != tc.route {
				t.Fatalf("route = %v, want %v", got, tc.route)
			}
			// The shard route's Job has the trim container alone; the claim route's has the
			// writing init container beside it. Both containers run the trim image.
			pod := m.jobs()[0].Spec.Template.Spec
			containers := make([]Container, 0, len(pod.InitContainers)+len(pod.Containers))
			containers = append(containers, pod.InitContainers...)
			containers = append(containers, pod.Containers...)
			if len(containers) == 0 {
				t.Fatal("the trim Job renders no container")
			}
			for _, c := range containers {
				if c.ImagePullPolicy != tc.want {
					t.Errorf("container %q on image %q has imagePullPolicy %q, want %q", c.Name, c.Image, c.ImagePullPolicy, tc.want)
				}
			}
		})
	}
}
