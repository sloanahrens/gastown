package session

import "testing"

// The image sets no Go cache paths. The shared cache claim carries the caches
// the seed built, and the openshell contract points GOCACHE and GOMODCACHE into
// it when the kubernetes profile names the claim (gt-nposw, gt-yvnsa). An ENV
// here would apply wherever the contract does not reach, and a sandbox with no
// claim would compile into a /tmp cache that dies with its container.
func TestDockerfileHasNoGoCacheEnv(t *testing.T) {
	t.Parallel()
	env := dockerfileEnv(t)
	for _, key := range []string{"GOCACHE", "GOMODCACHE"} {
		if v, ok := env[key]; ok {
			t.Errorf("the Dockerfile sets %s=%q: the sandbox contract points the cache into the claim, so the image must leave it unset", key, v)
		}
	}
}
