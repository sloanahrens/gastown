package testpolicy

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStartsContainersFixtures(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"qualified":        true,  // testutil.OpenTestStore in a unit test
		"unqualified":      true,  // RequireDoltContainer inside package testutil
		"integration_only": false, // only an integration-tagged file calls one
		"none":             false, // a comment or a string is not a call
		"prod_only":        false, // production code is not a test
	}
	for name, want := range cases {
		got, err := StartsContainers(filepath.Join("testdata", "docker", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if (len(got) > 0) != want {
			t.Errorf("%s: StartsContainers = %v, want a call site: %v", name, got, want)
		}
	}
}

// TestCheckDockerTierFixtures drives each finding CheckDockerTier reports
// against a fixture tree, so every red branch is seen to fire.
func TestCheckDockerTierFixtures(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("testdata", "dockertier"))
	if err != nil {
		t.Fatal(err)
	}
	dirs := []string{filepath.Join(root, "calls"), filepath.Join(root, "listedstale"), filepath.Join(root, "wrapper")}
	listed := map[string]bool{"listedstale": true, "gone": true}
	got, err := CheckDockerTier(root, dirs, listed)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"calls (first call: ",                     // calls an entry point, not listed
		"docker.txt lists listedstale, whose",     // listed, calls none
		"docker.txt lists gone, which is not",     // listed, not a package
		"wrapper/w.go:11:6: exported StartServer", // exported wrapper, not an entry point
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "startLocal") {
		t.Errorf("an unexported wrapper was reported; only exported ones can reach another package:\n%s", joined)
	}
	if len(got) != 4 {
		t.Errorf("got %d findings, want 4:\n%s", len(got), joined)
	}
}

// TestDockerTier keeps docker.txt exact: every package whose unit-tier test
// files call a container entry point is listed, so make test-integration runs
// its container tests (make gate skips them with GT_TEST_DOCKER=0), and every
// listed package still calls one. It also fails on an exported production
// function that wraps an entry point without being one, since a package
// calling that wrapper would start containers unseen.
func TestDockerTier(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := ReadList("docker.txt")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := CheckDockerTier(root, dirs, listed)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}
