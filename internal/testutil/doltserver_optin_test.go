package testutil

import (
	"strings"
	"testing"
)

// The container entry points must refuse to start Docker unless the caller
// opted in, and name the switch so a confused reader of a TestMain warning
// knows what to set.
func TestDockerTestsOptIn(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "0", "yes"} {
		env := newMapEnv(DockerTestsEnv + "=" + v)
		if dockerTestsEnabled(env) {
			t.Errorf("dockerTestsEnabled true for %q", v)
		}
		if err := containerOptIn(env); err == nil || !strings.Contains(err.Error(), DockerTestsEnv) {
			t.Errorf("containerOptIn(%q) = %v, want an error naming %s", v, err, DockerTestsEnv)
		}
	}
	if env := newMapEnv(DockerTestsEnv + "=1"); !dockerTestsEnabled(env) || containerOptIn(env) != nil {
		t.Error("opted in with 1, yet refused")
	}
	if dockerTestsEnabled(newMapEnv()) {
		t.Error("dockerTestsEnabled true with the variable unset")
	}
}

// RequireDoltContainer skips (not fails) when the opt-in is unset, so a
// filtered run that happens to select a container test still exits 0.
func TestRequireDoltContainerSkipsWithoutOptIn(t *testing.T) {
	t.Parallel()
	skipped := false
	t.Run("probe", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		requireContainerOptIn(t, newMapEnv())
		t.Fatal("requireContainerOptIn returned without skipping")
	})
	if !skipped {
		t.Fatal("requireContainerOptIn did not skip with the opt-in unset")
	}
}
