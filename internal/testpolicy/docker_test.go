package testpolicy

import (
	"os"
	"path/filepath"
	"sort"
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

// TestDockerTier keeps docker.txt exact: every package whose unit-tier test
// files call a container entry point is listed, so make test-integration runs
// its container tests (make gate skips them with GT_TEST_DOCKER=0), and every
// listed package still calls one.
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
	var missing []string
	for _, dir := range dirs {
		rel := filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator)))
		calls, err := StartsContainers(dir)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		switch {
		case len(calls) > 0 && !listed[rel]:
			missing = append(missing, rel+" (first call: "+calls[0].String()+")")
		case len(calls) == 0 && listed[rel]:
			t.Errorf("docker.txt lists %s, whose unit-tier tests call no container entry point: delete its line", rel)
		}
		delete(listed, rel)
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("%s starts containers in its unit-tier tests but is not in docker.txt, so make test-integration never runs them: add it", m)
	}
	for rel := range listed {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("docker.txt lists %s, which is not a Go package directory", rel)
		}
	}
}
