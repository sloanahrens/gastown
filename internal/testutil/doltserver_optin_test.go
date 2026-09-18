package testutil

import (
	"strings"
	"testing"
)

// The container entry points must refuse to start Docker unless the caller
// opted in. EnsureDoltContainerForTestMain is the observable one (it returns
// an error rather than skipping), and it must name the switch so a confused
// reader of a TestMain warning knows what to set.
func TestDockerTestsOptIn(t *testing.T) {
	t.Setenv(DockerTestsEnv, "")
	if DockerTestsEnabled() {
		t.Fatal("DockerTestsEnabled true with the variable unset")
	}
	err := EnsureDoltContainerForTestMain()
	if err == nil || !strings.Contains(err.Error(), DockerTestsEnv) {
		t.Fatalf("EnsureDoltContainerForTestMain with opt-in unset: err=%v, want an error naming %s", err, DockerTestsEnv)
	}

	t.Setenv(DockerTestsEnv, "0")
	if DockerTestsEnabled() {
		t.Fatal("DockerTestsEnabled true for value 0")
	}

	t.Setenv(DockerTestsEnv, "1")
	if !DockerTestsEnabled() {
		t.Fatal("DockerTestsEnabled false for value 1")
	}
}

// RequireDoltContainer skips (not fails) when the opt-in is unset, so a
// filtered run that happens to select a container test still exits 0.
func TestRequireDoltContainerSkipsWithoutOptIn(t *testing.T) {
	t.Setenv(DockerTestsEnv, "")
	skipped := false
	t.Run("probe", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		RequireDoltContainer(t)
		t.Fatal("RequireDoltContainer returned without skipping")
	})
	if !skipped {
		t.Fatal("RequireDoltContainer did not skip with the opt-in unset")
	}
}
