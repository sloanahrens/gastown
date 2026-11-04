package session

import (
	"strings"
	"testing"
)

// The image sets both Go cache paths to writable defaults under /tmp. A pod with
// no cache claim, such as the tier-sweep runner, gets neither variable from the
// sandbox contract, so an unset GOCACHE sends Go to $HOME/go under /sandbox,
// which is not writable there (gt-84ylj). A sandbox with a claim overrides both
// through the contract (gt-nposw).
func TestDockerfileSetsWritableGoCacheDefaults(t *testing.T) {
	t.Parallel()
	env := dockerfileEnv(t)
	for _, key := range []string{"GOCACHE", "GOMODCACHE"} {
		v, ok := env[key]
		if !ok {
			t.Errorf("the Dockerfile leaves %s unset: a pod with no cache claim would build under /sandbox", key)
			continue
		}
		if !strings.HasPrefix(v, "/tmp/") {
			t.Errorf("the Dockerfile sets %s=%q: the unseeded default must be a path under /tmp", key, v)
		}
	}
	for _, in := range dockerfileInstructions(t) {
		if !strings.HasPrefix(in, "ENV ") {
			continue
		}
		for _, kv := range strings.Fields(in)[1:] {
			k, v, _ := strings.Cut(kv, "=")
			if (k == "GOCACHE" || k == "GOMODCACHE") && strings.HasPrefix(v, "/sandbox/") {
				t.Errorf("%q sets %s under /sandbox, which a pod with no cache claim cannot write", in, k)
			}
		}
	}
}
