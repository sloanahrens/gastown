package cachevol

import "testing"

// testDigest is a well-formed image digest the pull policy tests use: the reference names one image,
// so no node's cached copy can be another build's.
const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestPullPolicyFor pins the rule a container's image reference follows: a moving tag pulls, because
// the node's cached copy is whatever tag it pulled last, while a digest, a commit tag, and any other
// fixed tag leave the cluster's default in place.
func TestPullPolicyFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{"dev tag", "gastown-polecat:dev", pullPolicyAlways},
		{"dev tag in a registry", "localhost:5001/gastown-polecat:dev", pullPolicyAlways},
		{"latest tag", "gastown-polecat:latest", pullPolicyAlways},
		{"no tag", "gastown-polecat", pullPolicyAlways},
		{"no tag in a registry with a port", "localhost:5001/gastown-polecat", pullPolicyAlways},
		{"digest", "gastown-polecat@" + testDigest, ""},
		{"digest in a registry", "localhost:5001/gastown-polecat@" + testDigest, ""},
		{"short commit tag", "gastown-polecat:0123abc", ""},
		{"long commit tag", "gastown-polecat:0123456789abcdef", ""},
		{"tag merely containing dev", "gastown-polecat:devel", ""},
		{"release tag", "gastown-polecat:v1.2.3", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PullPolicyFor(tc.image); got != tc.want {
				t.Errorf("PullPolicyFor(%q) = %q, want %q", tc.image, got, tc.want)
			}
		})
	}
}
