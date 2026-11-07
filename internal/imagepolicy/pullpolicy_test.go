package imagepolicy

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// testDigest is a well-formed image digest the policy tests use: a digest names one image, so no
// node's cached copy can be another build's.
const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestPullPolicyFor pins the rule the cache Jobs and the sweep Job both decide by: a moving tag
// pulls, because the node's cached copy is whatever tag it pulled last, while a digest, a commit
// tag and any other fixed tag leave the cluster's default in place.
func TestPullPolicyFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{"dev tag", "gastown-polecat:dev", PullPolicyAlways},
		{"dev tag in a registry", "localhost:5001/gastown-polecat:dev", PullPolicyAlways},
		{"latest tag", "gastown-polecat:latest", PullPolicyAlways},
		{"no tag", "gastown-polecat", PullPolicyAlways},
		{"no tag in a registry with a port", "localhost:5001/gastown-polecat", PullPolicyAlways},
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
