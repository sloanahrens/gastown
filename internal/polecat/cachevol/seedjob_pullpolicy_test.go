package cachevol

import "testing"

// TestSeedJobPullsAMovingTag pins the pull policy of both seed containers: a :dev image pulls, since
// the node's cached copy is whatever it pulled last, and a digest leaves the cluster's default.
func TestSeedJobPullsAMovingTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{"dev tag", "gastown-polecat:dev", pullPolicyAlways},
		{"digest", "gastown-polecat@" + testDigest, ""},
		{"commit tag", "gastown-polecat:0123456789abcdef", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := seedInput()
			in.Image = tc.image
			job, err := BuildSeedJob(in)
			if err != nil {
				t.Fatalf("BuildSeedJob: %v", err)
			}
			pod := job.Spec.Template.Spec
			containers := make([]Container, 0, len(pod.InitContainers)+len(pod.Containers))
			containers = append(containers, pod.InitContainers...)
			containers = append(containers, pod.Containers...)
			if len(containers) != 2 {
				t.Fatalf("the seed Job renders %d containers, want the clone and seed containers", len(containers))
			}
			for _, c := range containers {
				if c.ImagePullPolicy != tc.want {
					t.Errorf("container %q on image %q has imagePullPolicy %q, want %q", c.Name, c.Image, c.ImagePullPolicy, tc.want)
				}
			}
		})
	}
}
